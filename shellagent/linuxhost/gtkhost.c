//go:build linux && cgo && webkitgtk

#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#ifdef GDK_WINDOWING_X11
#include <gdk/gdkx.h>
#endif
#include <stdlib.h>
#include <string.h>

#include "gtkhost.h"
#include "_cgo_export.h"

static GtkApplication *application;
static GtkWidget *main_window;
static WebKitWebView *main_view;
static gboolean has_bridge;
static GtkWidget *fallback_header;
static GtkWidget *caption_controls;
static GtkWidget *caption_maximize;
static GdkEvent *caption_press;
static gboolean caption_drag;

// Navigation verdicts from Go (policy.go).
enum { POLICY_ALLOW = 0, POLICY_IGNORE = 1 };

// WebKitGTK draws the page through a GPU path (accelerated compositing and,
// since 2.42, the DMA-BUF renderer) and copies frames into the window later.
// An X11 desktop without a compositing manager, such as a VNC or Xvfb session,
// keeps no copy of covered windows, so an uncovered area stays stale until the
// page paints again: a window dragged over a DynApp leaves a smear. On such a
// display, fall back to WebKit's plain software painting. A variable the user
// set already wins.
static void choose_webkit_rendering(void) {
#ifdef GDK_WINDOWING_X11
  GdkDisplay *display = gdk_display_get_default();
  if (!display || !GDK_IS_X11_DISPLAY(display)) return;
  if (gdk_screen_is_composited(gdk_display_get_default_screen(display))) return;
  if (getenv("WEBKIT_DISABLE_COMPOSITING_MODE") || getenv("WEBKIT_DISABLE_DMABUF_RENDERER")) return;
  setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1", 0);
  setenv("WEBKIT_DISABLE_COMPOSITING_MODE", "1", 0);
  g_message("X11 display without a compositor: using WebKit software rendering");
#endif
}

// Without the DMA-BUF renderer, WebKitGTK (seen on 2.54) never creates the
// web process's shared display, yet every WebCodecs video decoder asks it for
// a GL context (GStreamer setGstElementGLContext -> sharedDisplay()), which
// aborts the web process. noVNC probes VideoDecoder.isConfigSupported on
// load, so such a page crashed on every load. Turning the DMA-BUF renderer
// back on brings the smear back, so hide WebCodecs video instead: pages see no
// VideoDecoder and use their other codecs.
static void disable_webcodecs_without_dmabuf(WebKitSettings *settings) {
  const char *dmabuf = getenv("WEBKIT_DISABLE_DMABUF_RENDERER");
  if (!dmabuf || !*dmabuf || strcmp(dmabuf, "0") == 0) return;
#if WEBKIT_CHECK_VERSION(2, 42, 0)
  WebKitFeatureList *features = webkit_settings_get_all_features();
  for (gsize index = 0; index < webkit_feature_list_get_length(features); index++) {
    WebKitFeature *feature = webkit_feature_list_get(features, index);
    if (g_strcmp0(webkit_feature_get_identifier(feature), "WebCodecsVideo") == 0) {
      webkit_settings_set_feature_enabled(settings, feature, FALSE);
      g_message("DMA-BUF renderer off: WebCodecs video disabled");
    }
  }
  webkit_feature_list_unref(features);
#else
  (void)settings;
#endif
}

const char *host_check(void) {
  if (!gtk_init_check(NULL, NULL)) return "cannot open the display";
  return NULL;
}

// ---- Dispatch from Go goroutines ------------------------------------------

static gboolean run_dispatch(gpointer data) {
  goDispatch((uintptr_t)data);
  return G_SOURCE_REMOVE;
}

void host_dispatch(uintptr_t handle) { g_idle_add(run_dispatch, (gpointer)handle); }

void host_quit(void) {
  if (application) g_application_quit(G_APPLICATION(application));
}

// ---- Small helpers -------------------------------------------------------

void host_open_external(const char *uri) {
  GError *error = NULL;
  if (!g_app_info_launch_default_for_uri(uri, NULL, &error)) {
    g_warning("open %s: %s", uri, error ? error->message : "failed");
    g_clear_error(&error);
  }
}

void host_show_error(const char *title, const char *message) {
  GtkWidget *dialog = gtk_message_dialog_new(main_window ? GTK_WINDOW(main_window) : NULL, GTK_DIALOG_MODAL,
                                             GTK_MESSAGE_WARNING, GTK_BUTTONS_OK, "%s", title);
  gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(dialog), "%s", message);
  gtk_dialog_run(GTK_DIALOG(dialog));
  gtk_widget_destroy(dialog);
}

static char *message_json(WebKitJavascriptResult *result) {
  JSCValue *value = webkit_javascript_result_get_js_value(result);
  if (jsc_value_is_string(value)) return jsc_value_to_string(value);
  return jsc_value_to_json(value, 0);
}

// ---- Page bridge ---------------------------------------------------------

static void on_bridge_message(WebKitUserContentManager *manager, WebKitJavascriptResult *result, gpointer data) {
  (void)manager;
  (void)data;
  if (!main_view) return;
  char *json = message_json(result);
  if (!json) return;
  const char *uri = webkit_web_view_get_uri(main_view);
  goBridgeMessage(json, (char *)(uri ? uri : ""));
  g_free(json);
}

void host_eval(const char *script) {
  if (main_view) webkit_web_view_evaluate_javascript(main_view, script, -1, NULL, NULL, NULL, NULL, NULL);
}

static gboolean is_dark_color(const GdkRGBA *color) {
  return 0.2126 * color->red + 0.7152 * color->green + 0.0722 * color->blue < 0.5;
}

static void update_caption_palette(void) {
  GtkStyleContext *style = gtk_widget_get_style_context(caption_controls);
  GdkRGBA foreground, background;
  if (!gtk_style_context_lookup_color(style, "theme_fg_color", &foreground))
    gtk_style_context_get_color(style, GTK_STATE_FLAG_NORMAL, &foreground);
  if (!gtk_style_context_lookup_color(style, "theme_bg_color", &background) || background.alpha < 1)
    gdk_rgba_parse(&background, is_dark_color(&foreground) ? "#ffffff" : "#2e2e2e");

  char *fg = gdk_rgba_to_string(&foreground);
  char *bg = gdk_rgba_to_string(&background);
  char *script = g_strdup_printf(
      "document.documentElement?.style.setProperty('--dynapp-linux-caption-bg', '%s');"
      "document.documentElement?.style.setProperty('--dynapp-linux-caption-fg', '%s');"
      "document.documentElement?.style.setProperty('--dynapp-linux-caption-scheme', '%s');",
      bg, fg, is_dark_color(&background) ? "dark" : "light");
  host_eval(script);
  g_free(script);
  g_free(bg);
  g_free(fg);
}

static void on_caption_style_updated(GtkWidget *widget, gpointer data) {
  (void)data;
  if (main_view && gtk_widget_get_visible(widget)) update_caption_palette();
}

static void layout_caption_controls(void) {
  double zoom = webkit_web_view_get_zoom_level(main_view);
  gtk_widget_set_size_request(caption_controls, -1, MAX(36, (int)(36 * zoom)));
  gint width;
  gtk_widget_get_preferred_width(caption_controls, NULL, &width);
  char pixels[G_ASCII_DTOSTR_BUF_SIZE];
  char height[G_ASCII_DTOSTR_BUF_SIZE];
  g_ascii_dtostr(pixels, sizeof pixels, (width + 10) / zoom);
  g_ascii_dtostr(height, sizeof height, MAX(36, 36 / zoom));
  char *script = g_strdup_printf(
      "document.documentElement?.style.setProperty('--dynapp-linux-caption-width', '%spx');"
      "document.documentElement?.style.setProperty('--dynapp-linux-caption-height', '%spx');", pixels, height);
  host_eval(script);
  g_free(script);
}

static void clear_caption_press(void) {
  g_clear_pointer(&caption_press, gdk_event_free);
  caption_drag = FALSE;
}

void host_titlebar(int enabled) {
  if (!main_window || !caption_controls) return;
  if (gtk_widget_get_visible(caption_controls) == (enabled != 0)) return;
  gtk_widget_set_visible(fallback_header, !enabled);
  gtk_widget_set_visible(caption_controls, enabled);
  if (enabled) {
    layout_caption_controls();
    update_caption_palette();
  } else clear_caption_press();
}

static gboolean on_caption_press(GtkWidget *widget, GdkEventButton *event, gpointer data) {
  (void)widget;
  (void)data;
  if (event->type == GDK_BUTTON_PRESS && event->button == GDK_BUTTON_PRIMARY) {
    clear_caption_press();
    if (gtk_widget_get_visible(caption_controls)) caption_press = gdk_event_copy((GdkEvent *)event);
  }
  return FALSE;
}

static gboolean on_caption_release(GtkWidget *widget, GdkEventButton *event, gpointer data) {
  (void)widget;
  (void)data;
  if (event->button == GDK_BUTTON_PRIMARY) clear_caption_press();
  return FALSE;
}

static gboolean on_caption_motion(GtkWidget *widget, GdkEventMotion *event, gpointer data) {
  (void)data;
  if (!caption_drag || !caption_press || !(event->state & GDK_BUTTON1_MASK)) return FALSE;
  const GdkEventButton *pressed = &caption_press->button;
  if (!gtk_drag_check_threshold(widget, pressed->x_root, pressed->y_root, event->x_root, event->y_root)) return FALSE;
  // Take the press over before synthesizing a release: on_caption_release
  // clears caption_press.
  GdkEvent *press = caption_press;
  caption_press = NULL;
  caption_drag = FALSE;
  GdkWindow *window = gtk_widget_get_window(main_window);
  GdkDevice *device = gdk_event_get_device(press);
  gboolean started = window && device;
  if (started) {
    // The window manager grabs the pointer for the move, so WebKit never sees
    // this button's release and keeps extending a selection (or a page drag)
    // with every later motion. End the press inside the page first.
    GdkEvent *release = gdk_event_copy(press);
    release->button.type = GDK_BUTTON_RELEASE;
    release->button.time = event->time;
    release->button.state |= GDK_BUTTON1_MASK;
    gtk_widget_event(widget, release);
    gdk_event_free(release);
    gdk_window_begin_move_drag_for_device(window, device, press->button.button, press->button.x_root,
                                          press->button.y_root, press->button.time);
  }
  gdk_event_free(press);
  return started;
}

void host_titlebar_drag(void) {
  caption_drag = caption_press != NULL;
}

void host_titlebar_zoom(void) {
  if (!main_window || !gtk_widget_get_visible(caption_controls)) return;
  GtkWindow *window = GTK_WINDOW(main_window);
  if (gtk_window_is_maximized(window)) gtk_window_unmaximize(window);
  else gtk_window_maximize(window);
}

static void on_main_load(WebKitWebView *view, WebKitLoadEvent load_event, gpointer data) {
  (void)view;
  (void)data;
  if (load_event == WEBKIT_LOAD_STARTED) host_titlebar(FALSE);
}

void host_load(const char *url) {
  if (main_view) webkit_web_view_load_uri(main_view, url);
}

void host_reload(void) {
  if (main_view) webkit_web_view_reload(main_view);
}

void host_present(void) {
  if (main_window) gtk_window_present(GTK_WINDOW(main_window));
}

int host_has_window(void) { return main_window != NULL; }

// ---- Navigation policy ---------------------------------------------------

static gboolean on_decide_policy(WebKitWebView *view, WebKitPolicyDecision *decision, WebKitPolicyDecisionType type,
                                 gpointer data) {
  (void)data;
  int main = view == main_view;
  if (type == WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION) {
    WebKitNavigationAction *action =
        webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(decision));
    const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(action));
    if (goNavigationPolicy((char *)(uri ? uri : ""), main, webkit_navigation_action_is_user_gesture(action)) ==
        POLICY_IGNORE) {
      webkit_policy_decision_ignore(decision);
      return TRUE;
    }
    return FALSE;
  }
  if (type == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) {
    WebKitResponsePolicyDecision *response = WEBKIT_RESPONSE_POLICY_DECISION(decision);
    if (!webkit_response_policy_decision_is_mime_type_supported(response)) {
      webkit_policy_decision_download(decision);
      return TRUE;
    }
    if (main && webkit_response_policy_decision_is_main_frame_main_resource(response)) {
      const char *uri = webkit_uri_request_get_uri(webkit_response_policy_decision_get_request(response));
      if (goMainFrameResponse((char *)(uri ? uri : "")) == POLICY_IGNORE) {
        webkit_policy_decision_ignore(decision);
        return TRUE;
      }
    }
  }
  return FALSE;
}

// ---- Popups --------------------------------------------------------------

static void on_popup_close(WebKitWebView *view, gpointer window) {
  (void)view;
  gtk_widget_destroy(GTK_WIDGET(window));
}

static void on_popup_ready(WebKitWebView *view, gpointer data) {
  (void)data;
  GtkWidget *window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
  gtk_window_set_title(GTK_WINDOW(window), main_window ? gtk_window_get_title(GTK_WINDOW(main_window)) : "");
  gtk_window_set_default_size(GTK_WINDOW(window), 520, 680);
  if (main_window) gtk_window_set_transient_for(GTK_WINDOW(window), GTK_WINDOW(main_window));
  gtk_container_add(GTK_CONTAINER(window), GTK_WIDGET(view));
  g_signal_connect(view, "close", G_CALLBACK(on_popup_close), window);
  gtk_widget_show_all(window);
}

static GtkWidget *on_create(WebKitWebView *view, WebKitNavigationAction *action, gpointer data);

static GtkWidget *on_create(WebKitWebView *view, WebKitNavigationAction *action, gpointer data) {
  (void)data;
  const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(action));
  if (goNewWindowPolicy((char *)(uri ? uri : "")) == POLICY_IGNORE) return NULL;
  // Popups (sign-in windows of the app's own origin) get no bridge: a fresh
  // content manager has no script and no message handler.
  WebKitUserContentManager *empty = webkit_user_content_manager_new();
  WebKitWebView *popup = WEBKIT_WEB_VIEW(
      g_object_new(WEBKIT_TYPE_WEB_VIEW, "related-view", view, "user-content-manager", empty, NULL));
  g_object_unref(empty);
  g_signal_connect(popup, "decide-policy", G_CALLBACK(on_decide_policy), NULL);
  g_signal_connect(popup, "create", G_CALLBACK(on_create), NULL);
  g_signal_connect(popup, "ready-to-show", G_CALLBACK(on_popup_ready), NULL);
  return GTK_WIDGET(popup);
}

// ---- Device and notification permissions ----------------------------------

static int media_answer = -1;        // -1 ask, 0 denied, 1 allowed for this run
static int notification_answer = -1;

typedef struct {
  WebKitPermissionRequest *request;
  int *answer;
} PermissionAsk;

static void on_permission_response(GtkDialog *dialog, gint response, gpointer data) {
  PermissionAsk *ask = data;
  int allowed = response == GTK_RESPONSE_ACCEPT;
  *ask->answer = allowed;
  if (allowed) webkit_permission_request_allow(ask->request);
  else webkit_permission_request_deny(ask->request);
  g_object_unref(ask->request);
  g_free(ask);
  gtk_widget_destroy(GTK_WIDGET(dialog));
}

static void ask_permission(WebKitPermissionRequest *request, int *answer, const char *what) {
  if (*answer == 1) {
    webkit_permission_request_allow(request);
    return;
  }
  if (*answer == 0) {
    webkit_permission_request_deny(request);
    return;
  }
  const char *title = main_window ? gtk_window_get_title(GTK_WINDOW(main_window)) : "This app";
  GtkWidget *dialog = gtk_message_dialog_new(GTK_WINDOW(main_window), GTK_DIALOG_MODAL | GTK_DIALOG_DESTROY_WITH_PARENT,
                                             GTK_MESSAGE_QUESTION, GTK_BUTTONS_NONE, "Allow %s to use %s?", title, what);
  gtk_dialog_add_buttons(GTK_DIALOG(dialog), "Don't Allow", GTK_RESPONSE_REJECT, "Allow", GTK_RESPONSE_ACCEPT, NULL);
  gtk_dialog_set_default_response(GTK_DIALOG(dialog), GTK_RESPONSE_REJECT);
  PermissionAsk *ask = g_new0(PermissionAsk, 1);
  ask->request = g_object_ref(request);
  ask->answer = answer;
  g_signal_connect(dialog, "response", G_CALLBACK(on_permission_response), ask);
  gtk_widget_show(dialog);
}

static gboolean on_permission_request(WebKitWebView *view, WebKitPermissionRequest *request, gpointer data) {
  (void)data;
  const char *uri = webkit_web_view_get_uri(view);
  if (view != main_view || goIsAppOrigin((char *)(uri ? uri : "")) == 0) {
    webkit_permission_request_deny(request);
    return TRUE;
  }
  if (WEBKIT_IS_USER_MEDIA_PERMISSION_REQUEST(request)) {
    WebKitUserMediaPermissionRequest *media = WEBKIT_USER_MEDIA_PERMISSION_REQUEST(request);
    gboolean audio = webkit_user_media_permission_is_for_audio_device(media);
    gboolean video = webkit_user_media_permission_is_for_video_device(media);
    if (webkit_user_media_permission_is_for_display_device(media)) {
      ask_permission(request, &media_answer, "your screen");
      return TRUE;
    }
    ask_permission(request, &media_answer,
                   audio && video ? "your camera and microphone" : video ? "your camera" : "your microphone");
    return TRUE;
  }
  if (WEBKIT_IS_DEVICE_INFO_PERMISSION_REQUEST(request)) {
    if (media_answer == 1) webkit_permission_request_allow(request);
    else webkit_permission_request_deny(request);
    return TRUE;
  }
  if (WEBKIT_IS_NOTIFICATION_PERMISSION_REQUEST(request)) {
    ask_permission(request, &notification_answer, "notifications");
    return TRUE;
  }
  if (WEBKIT_IS_CLIPBOARD_PERMISSION_REQUEST(request) || WEBKIT_IS_POINTER_LOCK_PERMISSION_REQUEST(request)) {
    webkit_permission_request_allow(request);
    return TRUE;
  }
  webkit_permission_request_deny(request);
  return TRUE;
}

// ---- Downloads -----------------------------------------------------------

static gboolean on_decide_destination(WebKitDownload *download, gchar *suggested, gpointer data) {
  (void)data;
  GtkFileChooserNative *chooser =
      gtk_file_chooser_native_new("Save File", GTK_WINDOW(main_window), GTK_FILE_CHOOSER_ACTION_SAVE, "_Save", "_Cancel");
  GtkFileChooser *files = GTK_FILE_CHOOSER(chooser);
  gtk_file_chooser_set_do_overwrite_confirmation(files, TRUE);
  const char *downloads = g_get_user_special_dir(G_USER_DIRECTORY_DOWNLOAD);
  if (downloads) gtk_file_chooser_set_current_folder(files, downloads);
  gtk_file_chooser_set_current_name(files, suggested && *suggested ? suggested : "download");
  if (gtk_native_dialog_run(GTK_NATIVE_DIALOG(chooser)) == GTK_RESPONSE_ACCEPT) {
    char *uri = gtk_file_chooser_get_uri(files);
    webkit_download_set_destination(download, uri);
    g_free(uri);
  } else {
    webkit_download_cancel(download);
  }
  g_object_unref(chooser);
  return TRUE;
}

static void on_download_started(WebKitWebContext *context, WebKitDownload *download, gpointer data) {
  (void)context;
  (void)data;
  g_signal_connect(download, "decide-destination", G_CALLBACK(on_decide_destination), NULL);
}

// ---- File drops ----------------------------------------------------------
//
// WebKit gives the page File objects without paths. The drag's text/uri-list
// has them; report them like the macOS host so appShell.drop gets real paths.

static GdkDragContext *drop_context;
static gchar **drop_uris;
static gboolean drop_pending;
static int drop_x, drop_y;

static void deliver_drop(void) {
  if (!drop_uris) return;
  int count = 0;
  char **paths = g_new0(char *, g_strv_length(drop_uris) + 1);
  for (int index = 0; drop_uris[index]; index++) {
    char *path = g_filename_from_uri(drop_uris[index], NULL, NULL);
    if (path) paths[count++] = path;
  }
  if (count > 0) goDrop(paths, count, drop_x, drop_y);
  g_strfreev(paths);
  g_strfreev(drop_uris);
  drop_uris = NULL;
  drop_pending = FALSE;
  drop_context = NULL;
}

static void on_drag_data_received(GtkWidget *widget, GdkDragContext *context, gint x, gint y, GtkSelectionData *data,
                                  guint info, guint time, gpointer user) {
  (void)widget;
  (void)info;
  (void)time;
  (void)user;
  gchar **uris = gtk_selection_data_get_uris(data);
  if (!uris) return;
  g_strfreev(drop_uris);
  drop_uris = uris;
  drop_context = context;
  if (!drop_pending) {
    drop_x = x;
    drop_y = y;
  }
  if (drop_pending) deliver_drop();
}

static gboolean on_drag_drop(GtkWidget *widget, GdkDragContext *context, gint x, gint y, guint time, gpointer user) {
  (void)widget;
  (void)time;
  (void)user;
  drop_x = x;
  drop_y = y;
  drop_pending = TRUE;
  if (drop_uris && drop_context == context) deliver_drop();
  return FALSE;  // WebKit still runs the DOM drop.
}

// ---- Permission prompt window --------------------------------------------

typedef struct {
  char *id;
  GtkWidget *window;
  WebKitUserContentManager *manager;
  gboolean shown;
  gboolean finished;
} PromptPage;

static GHashTable *prompt_pages;  // id → PromptPage*

// The prompt window is mapped from the start so WebKit lays the page out in
// a real viewport, but stays transparent (where a compositor allows) until
// the page reports its height.
static void prompt_show(PromptPage *page) {
  if (page->shown || page->finished) return;
  page->shown = TRUE;
  gtk_widget_set_opacity(page->window, 1.0);
  gtk_window_present(GTK_WINDOW(page->window));
}

static gboolean prompt_show_later(gpointer data) {
  PromptPage *page = g_hash_table_lookup(prompt_pages, data);
  if (page) prompt_show(page);
  g_free(data);
  return G_SOURCE_REMOVE;
}

static void prompt_free(gpointer data) {
  PromptPage *page = data;
  g_free(page->id);
  g_free(page);
}

static void on_prompt_message(WebKitUserContentManager *manager, WebKitJavascriptResult *result, gpointer data) {
  (void)manager;
  PromptPage *page = g_hash_table_lookup(prompt_pages, data);
  if (!page || page->finished) return;
  char *json = message_json(result);
  if (!json) return;
  goPromptMessage(page->id, json);
  g_free(json);
}

static gboolean on_prompt_delete(GtkWidget *widget, GdkEvent *event, gpointer data) {
  (void)widget;
  (void)event;
  PromptPage *page = g_hash_table_lookup(prompt_pages, data);
  if (page && !page->finished) goPromptMessage(page->id, "{\"decision\":\"dismissed\"}");
  return FALSE;
}

static gboolean on_prompt_policy(WebKitWebView *view, WebKitPolicyDecision *decision, WebKitPolicyDecisionType type,
                                 gpointer data) {
  (void)view;
  (void)data;
  if (type == WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION) {
    WebKitNavigationAction *action =
        webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(decision));
    const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(action));
    if (uri && strcmp(uri, "about:blank") == 0) return FALSE;
  }
  if (type == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) return FALSE;
  webkit_policy_decision_ignore(decision);
  return TRUE;
}

static GtkWidget *on_prompt_create(WebKitWebView *view, WebKitNavigationAction *action, gpointer data) {
  (void)view;
  (void)action;
  (void)data;
  return NULL;
}

static void on_prompt_crashed(WebKitWebView *view, WebKitWebProcessTerminationReason reason, gpointer data) {
  (void)view;
  (void)reason;
  PromptPage *page = g_hash_table_lookup(prompt_pages, data);
  if (page && !page->finished) goPromptMessage(page->id, "{\"decision\":\"dismissed\"}");
}

void host_show_permission_page(const char *id, const char *html) {
  if (!prompt_pages) prompt_pages = g_hash_table_new_full(g_str_hash, g_str_equal, NULL, prompt_free);
  PromptPage *page = g_new0(PromptPage, 1);
  page->id = g_strdup(id);
  g_hash_table_insert(prompt_pages, page->id, page);

  page->window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
  gtk_window_set_title(GTK_WINDOW(page->window), "Permissions");
  gtk_window_set_type_hint(GTK_WINDOW(page->window), GDK_WINDOW_TYPE_HINT_DIALOG);
  gtk_window_set_default_size(GTK_WINDOW(page->window), 500, 420);
  gtk_window_set_resizable(GTK_WINDOW(page->window), FALSE);
  if (main_window) {
    gtk_window_set_transient_for(GTK_WINDOW(page->window), GTK_WINDOW(main_window));
    gtk_window_set_modal(GTK_WINDOW(page->window), TRUE);
    gtk_window_set_position(GTK_WINDOW(page->window), GTK_WIN_POS_CENTER_ON_PARENT);
  }
  // Its own ephemeral context and content manager: the app's page has no
  // path to this web view or its message handler.
  WebKitWebContext *context = webkit_web_context_new_ephemeral();
  page->manager = webkit_user_content_manager_new();
  webkit_user_content_manager_register_script_message_handler(page->manager, "dynappPermission");
  g_signal_connect(page->manager, "script-message-received::dynappPermission", G_CALLBACK(on_prompt_message),
                   page->id);
  WebKitWebView *view = WEBKIT_WEB_VIEW(
      g_object_new(WEBKIT_TYPE_WEB_VIEW, "web-context", context, "user-content-manager", page->manager, NULL));
  g_object_unref(context);
  g_object_unref(page->manager);
  WebKitSettings *settings = webkit_web_view_get_settings(view);
  webkit_settings_set_javascript_can_open_windows_automatically(settings, FALSE);
  g_signal_connect(view, "decide-policy", G_CALLBACK(on_prompt_policy), NULL);
  g_signal_connect(view, "create", G_CALLBACK(on_prompt_create), NULL);
  g_signal_connect(view, "web-process-terminated", G_CALLBACK(on_prompt_crashed), page->id);
  g_signal_connect(page->window, "delete-event", G_CALLBACK(on_prompt_delete), page->id);
  gtk_container_add(GTK_CONTAINER(page->window), GTK_WIDGET(view));
  gtk_widget_set_opacity(page->window, 0.0);
  gtk_widget_show_all(page->window);
  webkit_web_view_load_html(view, html, NULL);
  // The page reports its height once laid out; show it then, or after a
  // second if that report never comes.
  g_timeout_add(1000, prompt_show_later, g_strdup(id));
}

void host_resize_permission_page(const char *id, int height) {
  if (!prompt_pages) return;
  PromptPage *page = g_hash_table_lookup(prompt_pages, id);
  if (!page) return;
  int available = 700;
  if (main_window) {
    int width = 0, parent = 0;
    gtk_window_get_size(GTK_WINDOW(main_window), &width, &parent);
    if (parent > 0) available = parent - 40;
  }
  if (available < 320) available = 320;
  int target = height < 220 ? 220 : height;
  if (target > available) target = available;
  if (target > 680) target = 680;
  gtk_window_resize(GTK_WINDOW(page->window), 500, target);
  prompt_show(page);
}

void host_close_permission_page(const char *id) {
  if (!prompt_pages) return;
  PromptPage *page = g_hash_table_lookup(prompt_pages, id);
  if (!page) return;
  page->finished = TRUE;
  webkit_user_content_manager_unregister_script_message_handler(page->manager, "dynappPermission");
  gtk_widget_destroy(page->window);
  g_hash_table_remove(prompt_pages, id);
}

// ---- Main window ---------------------------------------------------------

// A page that crashes WebKit on every load would otherwise reload forever,
// flashing between the page and a blank view. Reload after a crash only while
// crashes stay rare; after three within a minute, show what happened and let
// the user reload by hand (menu Reload, Ctrl+R or F5).
static gint64 crash_times[3];

static void on_web_process_terminated(WebKitWebView *view, WebKitWebProcessTerminationReason reason, gpointer data) {
  (void)data;
  if (reason == WEBKIT_WEB_PROCESS_TERMINATED_BY_API) return;
  gint64 now = g_get_monotonic_time();
  memmove(crash_times, crash_times + 1, sizeof(crash_times) - sizeof(crash_times[0]));
  crash_times[G_N_ELEMENTS(crash_times) - 1] = now;
  if (crash_times[0] == 0 || now - crash_times[0] > 60 * G_USEC_PER_SEC) {
    webkit_web_view_reload(view);
    return;
  }
  memset(crash_times, 0, sizeof(crash_times));
  const char *what = reason == WEBKIT_WEB_PROCESS_EXCEEDED_MEMORY_LIMIT ? "ran out of memory" : "crashed";
  char *html = g_strdup_printf(
      "<!doctype html><meta charset=utf-8><body style=\"font:15px system-ui,sans-serif;margin:48px;color:#222\">"
      "<h2 style=\"font-weight:600\">This app %s</h2>"
      "<p>WebKit stopped the page several times in a row, so it was not reloaded again.</p>"
      "<p>Choose Reload in the window menu, or press Ctrl+R, to try again.</p>",
      what);
  g_warning("web process %s three times within a minute; not reloading", what);
  // The failed page's URI as base keeps Reload pointed at the app.
  webkit_web_view_load_alternate_html(view, html, webkit_web_view_get_uri(view), NULL);
  g_free(html);
}

static void on_view_close(WebKitWebView *view, gpointer data) {
  (void)view;
  (void)data;
  if (main_window) gtk_window_close(GTK_WINDOW(main_window));
}

static void on_title_changed(WebKitWebView *view, GParamSpec *spec, gpointer header) {
  (void)spec;
  const char *title = webkit_web_view_get_title(view);
  const char *name = gtk_window_get_title(GTK_WINDOW(main_window));
  gtk_header_bar_set_subtitle(GTK_HEADER_BAR(header), title && *title && g_strcmp0(title, name) != 0 ? title : NULL);
}

static void on_main_destroy(GtkWidget *widget, gpointer data) {
  (void)widget;
  (void)data;
  main_window = NULL;
  main_view = NULL;
  fallback_header = NULL;
  caption_controls = NULL;
  caption_maximize = NULL;
  clear_caption_press();
  goWindowClosed();
}

static void action_permissions(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  (void)data;
  goMenuPermissions();
}

// GtkWindow runs accelerators before the focused widget sees a key, so F5 or
// Ctrl+R reloaded the page even when the app uses them (Commander's F5 is
// Copy). Give the page the key first, like WKWebView and WebView2 do.
// WebKitGTK consumes the key, asks the page, and re-dispatches it to this
// window only when the page did not handle it; that second pass reaches the
// accelerators. Always stop here: the default handler would hand the
// re-dispatched key back to WebKit as a new event.
static gboolean on_main_key_press(GtkWidget *widget, GdkEventKey *event, gpointer data) {
  (void)data;
  GtkWindow *window = GTK_WINDOW(widget);
  if (!main_view || gtk_window_get_focus(window) != GTK_WIDGET(main_view)) return FALSE;
  if (gtk_window_propagate_key_event(window, event)) return TRUE;
  gtk_window_activate_key(window, event);
  return TRUE;
}

static void action_reload(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  (void)data;
  host_reload();
}

static void action_zoom(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  if (!main_view) return;
  double step = GPOINTER_TO_INT(data) / 10.0;
  double level = step == 0 ? 1.0 : webkit_web_view_get_zoom_level(main_view) + step;
  if (level < 0.3) level = 0.3;
  if (level > 3.0) level = 3.0;
  webkit_web_view_set_zoom_level(main_view, level);
  if (gtk_widget_get_visible(caption_controls)) layout_caption_controls();
}

static void action_fullscreen(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  (void)data;
  if (!main_window) return;
  GdkWindow *window = gtk_widget_get_window(main_window);
  if (window && (gdk_window_get_state(window) & GDK_WINDOW_STATE_FULLSCREEN)) gtk_window_unfullscreen(GTK_WINDOW(main_window));
  else gtk_window_fullscreen(GTK_WINDOW(main_window));
}

static void action_inspector(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  (void)data;
  if (main_view) webkit_web_inspector_show(webkit_web_view_get_inspector(main_view));
}

static void add_action(const char *name, GCallback callback, gpointer data, const char *accel1, const char *accel2) {
  GSimpleAction *action = g_simple_action_new(name, NULL);
  g_signal_connect(action, "activate", callback, data);
  g_action_map_add_action(G_ACTION_MAP(application), G_ACTION(action));
  g_object_unref(action);
  char detailed[64];
  g_snprintf(detailed, sizeof detailed, "app.%s", name);
  const char *accels[] = {accel1, accel2, NULL};
  if (accel1) gtk_application_set_accels_for_action(application, detailed, accels);
}

// GTK leaves the button tooltip up over its own open menu; suppress it
// while the menu is showing.
static void on_caption_menu_active(GObject *button, GParamSpec *pspec, gpointer data) {
  (void)pspec; (void)data;
  gboolean active = gtk_toggle_button_get_active(GTK_TOGGLE_BUTTON(button));
  gtk_widget_set_has_tooltip(GTK_WIDGET(button), !active);
  // Re-query so a tooltip that is already visible is hidden now.
  gtk_widget_trigger_tooltip_query(GTK_WIDGET(button));
}

static GtkWidget *caption_menu(GMenuModel *menu) {
  GtkWidget *button = gtk_menu_button_new();
  gtk_menu_button_set_menu_model(GTK_MENU_BUTTON(button), menu);
  gtk_button_set_image(GTK_BUTTON(button), gtk_image_new_from_icon_name("open-menu-symbolic", GTK_ICON_SIZE_BUTTON));
  gtk_widget_set_tooltip_text(button, "Menu");
  g_signal_connect(button, "notify::active", G_CALLBACK(on_caption_menu_active), NULL);
  return button;
}

static GtkWidget *caption_button(const char *icon, const char *label, GCallback callback) {
  GtkWidget *button = gtk_button_new_from_icon_name(icon, GTK_ICON_SIZE_BUTTON);
  gtk_widget_set_tooltip_text(button, label);
  g_signal_connect(button, "clicked", callback, NULL);
  return button;
}

static void minimize_caption(GtkButton *button, gpointer data) {
  (void)button;
  (void)data;
  gtk_window_iconify(GTK_WINDOW(main_window));
}

static void maximize_caption(GtkButton *button, gpointer data) {
  (void)button;
  (void)data;
  host_titlebar_zoom();
}

static void close_caption(GtkButton *button, gpointer data) {
  (void)button;
  (void)data;
  gtk_window_close(GTK_WINDOW(main_window));
}

static void update_maximize_caption(GtkWindow *window, GParamSpec *spec, gpointer data) {
  (void)spec;
  (void)data;
  gboolean maximized = gtk_window_is_maximized(window);
  gtk_image_set_from_icon_name(GTK_IMAGE(gtk_button_get_image(GTK_BUTTON(caption_maximize))),
                               maximized ? "window-restore-symbolic" : "window-maximize-symbolic", GTK_ICON_SIZE_BUTTON);
  gtk_widget_set_tooltip_text(caption_maximize, maximized ? "Restore" : "Maximize");
}

void host_create_window(const char *title, const char *icon, const char *data_dir, const char *cache_dir,
                        const char *user_script, int debug) {
  WebKitWebsiteDataManager *manager =
      webkit_website_data_manager_new("base-data-directory", data_dir, "base-cache-directory", cache_dir, NULL);
  WebKitWebContext *context = webkit_web_context_new_with_website_data_manager(manager);
  char *cookies = g_build_filename(data_dir, "cookies.sqlite", NULL);
  webkit_cookie_manager_set_persistent_storage(webkit_website_data_manager_get_cookie_manager(manager), cookies,
                                               WEBKIT_COOKIE_PERSISTENT_STORAGE_SQLITE);
  g_free(cookies);
  g_object_unref(manager);
  g_signal_connect(context, "download-started", G_CALLBACK(on_download_started), NULL);

  WebKitUserContentManager *content = webkit_user_content_manager_new();
  has_bridge = user_script != NULL;
  if (has_bridge) {
    char *source = g_strconcat("Object.defineProperty(window, '__DYNAPP_NATIVE_TITLEBAR__', { value: true });\n",
                               user_script, NULL);
    WebKitUserScript *script = webkit_user_script_new(source, WEBKIT_USER_CONTENT_INJECT_TOP_FRAME,
                                                      WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL);
    g_free(source);
    webkit_user_content_manager_add_script(content, script);
    webkit_user_script_unref(script);
    webkit_user_content_manager_register_script_message_handler(content, "dynappNativeHost");
    g_signal_connect(content, "script-message-received::dynappNativeHost", G_CALLBACK(on_bridge_message), NULL);
  }
  main_view = WEBKIT_WEB_VIEW(
      g_object_new(WEBKIT_TYPE_WEB_VIEW, "web-context", context, "user-content-manager", content, NULL));
  g_object_unref(context);
  g_object_unref(content);

  WebKitSettings *settings = webkit_web_view_get_settings(main_view);
  webkit_settings_set_user_agent_with_application_details(settings, "DynApp", "1");
  webkit_settings_set_javascript_can_open_windows_automatically(settings, TRUE);
  webkit_settings_set_javascript_can_access_clipboard(settings, TRUE);
  webkit_settings_set_enable_fullscreen(settings, TRUE);
  webkit_settings_set_media_playback_requires_user_gesture(settings, FALSE);
  webkit_settings_set_enable_developer_extras(settings, debug ? TRUE : FALSE);
  GObjectClass *settings_class = G_OBJECT_GET_CLASS(settings);
  const char *enabled[] = {"enable-media-stream", "enable-mediasource", "enable-webrtc", "enable-encrypted-media",
                           "enable-back-forward-navigation-gestures", NULL};
  for (int index = 0; enabled[index]; index++) {
    if (g_object_class_find_property(settings_class, enabled[index])) g_object_set(settings, enabled[index], TRUE, NULL);
  }
  disable_webcodecs_without_dmabuf(settings);

  g_signal_connect(main_view, "decide-policy", G_CALLBACK(on_decide_policy), NULL);
  g_signal_connect(main_view, "create", G_CALLBACK(on_create), NULL);
  g_signal_connect(main_view, "permission-request", G_CALLBACK(on_permission_request), NULL);
  g_signal_connect(main_view, "web-process-terminated", G_CALLBACK(on_web_process_terminated), NULL);
  g_signal_connect(main_view, "close", G_CALLBACK(on_view_close), NULL);
  g_signal_connect(main_view, "drag-data-received", G_CALLBACK(on_drag_data_received), NULL);
  g_signal_connect(main_view, "drag-drop", G_CALLBACK(on_drag_drop), NULL);

  main_window = gtk_application_window_new(application);
  gtk_window_set_title(GTK_WINDOW(main_window), title);
  gtk_window_set_default_size(GTK_WINDOW(main_window), 1280, 820);
  gtk_window_set_position(GTK_WINDOW(main_window), GTK_WIN_POS_CENTER);
  if (icon && *icon) {
    gtk_window_set_icon_from_file(GTK_WINDOW(main_window), icon, NULL);
    gtk_window_set_default_icon_from_file(icon, NULL);
  }

  add_action("permissions", G_CALLBACK(action_permissions), NULL, "<Control>comma", NULL);
  add_action("reload", G_CALLBACK(action_reload), NULL, "<Control>r", "F5");
  add_action("zoom-in", G_CALLBACK(action_zoom), GINT_TO_POINTER(1), "<Control>plus", "<Control>equal");
  add_action("zoom-out", G_CALLBACK(action_zoom), GINT_TO_POINTER(-1), "<Control>minus", NULL);
  add_action("zoom-reset", G_CALLBACK(action_zoom), GINT_TO_POINTER(0), "<Control>0", NULL);
  add_action("fullscreen", G_CALLBACK(action_fullscreen), NULL, "F11", NULL);
  if (debug) add_action("inspector", G_CALLBACK(action_inspector), NULL, "<Control><Shift>i", "F12");

  GMenu *menu = g_menu_new();
  GMenu *top = g_menu_new();
  if (has_bridge) g_menu_append(top, "Permissions…", "app.permissions");
  g_menu_append(top, "Reload", "app.reload");
  g_menu_append_section(menu, NULL, G_MENU_MODEL(top));
  GMenu *view = g_menu_new();
  g_menu_append(view, "Zoom In", "app.zoom-in");
  g_menu_append(view, "Zoom Out", "app.zoom-out");
  g_menu_append(view, "Actual Size", "app.zoom-reset");
  g_menu_append(view, "Full Screen", "app.fullscreen");
  if (debug) g_menu_append(view, "Web Inspector", "app.inspector");
  g_menu_append_section(menu, NULL, G_MENU_MODEL(view));
  GtkWidget *button = caption_menu(G_MENU_MODEL(menu));
  g_object_unref(top);
  g_object_unref(view);

  GtkWidget *header = gtk_header_bar_new();
  gtk_header_bar_set_show_close_button(GTK_HEADER_BAR(header), TRUE);
  gtk_header_bar_set_title(GTK_HEADER_BAR(header), title);
  gtk_header_bar_pack_end(GTK_HEADER_BAR(header), button);
  gtk_window_set_titlebar(GTK_WINDOW(main_window), header);
  fallback_header = header;
  g_signal_connect(main_view, "notify::title", G_CALLBACK(on_title_changed), header);

  GtkWidget *overlay = gtk_overlay_new();
  gtk_container_add(GTK_CONTAINER(overlay), GTK_WIDGET(main_view));
  caption_controls = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 0);
  gtk_widget_set_halign(caption_controls, GTK_ALIGN_END);
  gtk_widget_set_valign(caption_controls, GTK_ALIGN_START);
  gtk_widget_set_margin_end(caption_controls, 4);
  gtk_widget_set_no_show_all(caption_controls, TRUE);
  caption_maximize = caption_button("window-maximize-symbolic", "Maximize", G_CALLBACK(maximize_caption));
  GtkWidget *buttons[] = {
      caption_menu(G_MENU_MODEL(menu)),
      caption_button("window-minimize-symbolic", "Minimize", G_CALLBACK(minimize_caption)),
      caption_maximize,
      caption_button("window-close-symbolic", "Close", G_CALLBACK(close_caption)),
  };
  g_object_unref(menu);
  GtkCssProvider *caption_style = gtk_css_provider_new();
  gtk_css_provider_load_from_data(caption_style,
      "button:not(:hover):not(:active):not(:checked) {"
      "  background-color: transparent; background-image: none; border-color: transparent; box-shadow: none;"
      "}", -1, NULL);
  for (guint index = 0; index < G_N_ELEMENTS(buttons); index++) {
    gtk_style_context_add_provider(gtk_widget_get_style_context(buttons[index]),
                                   GTK_STYLE_PROVIDER(caption_style), GTK_STYLE_PROVIDER_PRIORITY_APPLICATION);
    gtk_button_set_relief(GTK_BUTTON(buttons[index]), GTK_RELIEF_NONE);
    gtk_box_pack_start(GTK_BOX(caption_controls), buttons[index], FALSE, FALSE, 0);
    gtk_widget_show_all(buttons[index]);
  }
  g_object_unref(caption_style);
  gtk_overlay_add_overlay(GTK_OVERLAY(overlay), caption_controls);
  g_signal_connect(caption_controls, "style-updated", G_CALLBACK(on_caption_style_updated), NULL);
  g_signal_connect(main_window, "notify::is-maximized", G_CALLBACK(update_maximize_caption), NULL);
  g_signal_connect(main_view, "load-changed", G_CALLBACK(on_main_load), NULL);
  gtk_widget_add_events(GTK_WIDGET(main_view), GDK_BUTTON_PRESS_MASK | GDK_BUTTON_RELEASE_MASK | GDK_POINTER_MOTION_MASK);
  g_signal_connect(main_view, "button-press-event", G_CALLBACK(on_caption_press), NULL);
  g_signal_connect(main_view, "button-release-event", G_CALLBACK(on_caption_release), NULL);
  g_signal_connect(main_view, "motion-notify-event", G_CALLBACK(on_caption_motion), NULL);
  gtk_container_add(GTK_CONTAINER(main_window), overlay);
  g_signal_connect(main_window, "destroy", G_CALLBACK(on_main_destroy), NULL);
  g_signal_connect(main_window, "key-press-event", G_CALLBACK(on_main_key_press), NULL);
  gtk_widget_show_all(main_window);
  gtk_widget_grab_focus(GTK_WIDGET(main_view));
}

// ---- Application ---------------------------------------------------------

static void on_activate(GApplication *app, gpointer data) {
  (void)app;
  (void)data;
  if (main_window) {
    gtk_window_present(GTK_WINDOW(main_window));
    return;
  }
  goStart(NULL, 0);
}

static void on_open(GApplication *app, GFile **files, gint count, const gchar *hint, gpointer data) {
  (void)app;
  (void)hint;
  (void)data;
  char **paths = g_new0(char *, count + 1);
  int found = 0;
  for (int index = 0; index < count; index++) {
    char *path = g_file_get_path(files[index]);
    if (path) paths[found++] = path;
  }
  if (main_window) goOpenFiles(paths, found);
  else goStart(paths, found);
  g_strfreev(paths);
}

// startup runs once, in the primary instance, after GTK has opened the
// display and before any web view exists.
static void on_startup(GApplication *app, gpointer data) {
  (void)app;
  (void)data;
  choose_webkit_rendering();
}

int host_run(const char *app_id, int argc, char **argv) {
  g_set_prgname(app_id);
  gdk_set_program_class(app_id);
  if (!g_application_id_is_valid(app_id)) app_id = NULL;
  application = gtk_application_new(app_id, G_APPLICATION_HANDLES_OPEN);
  g_signal_connect(application, "startup", G_CALLBACK(on_startup), NULL);
  g_signal_connect(application, "activate", G_CALLBACK(on_activate), NULL);
  g_signal_connect(application, "open", G_CALLBACK(on_open), NULL);
  int status = g_application_run(G_APPLICATION(application), argc, argv);
  g_object_unref(application);
  application = NULL;
  return status;
}
