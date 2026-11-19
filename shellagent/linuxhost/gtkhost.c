//go:build linux && cgo && webkitgtk

#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <string.h>

#include "gtkhost.h"
#include "_cgo_export.h"

static GtkApplication *application;
static GtkWidget *main_window;
static WebKitWebView *main_view;
static gboolean has_bridge;

// Navigation verdicts from Go (policy.go).
enum { POLICY_ALLOW = 0, POLICY_IGNORE = 1 };

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

static void on_web_process_terminated(WebKitWebView *view, WebKitWebProcessTerminationReason reason, gpointer data) {
  (void)data;
  if (reason != WEBKIT_WEB_PROCESS_TERMINATED_BY_API) webkit_web_view_reload(view);
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
  goWindowClosed();
}

static void action_permissions(GSimpleAction *action, GVariant *parameter, gpointer data) {
  (void)action;
  (void)parameter;
  (void)data;
  goMenuPermissions();
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
    WebKitUserScript *script = webkit_user_script_new(user_script, WEBKIT_USER_CONTENT_INJECT_TOP_FRAME,
                                                      WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL);
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
  GtkWidget *button = gtk_menu_button_new();
  gtk_menu_button_set_menu_model(GTK_MENU_BUTTON(button), G_MENU_MODEL(menu));
  gtk_button_set_image(GTK_BUTTON(button), gtk_image_new_from_icon_name("open-menu-symbolic", GTK_ICON_SIZE_BUTTON));
  gtk_widget_set_tooltip_text(button, "Menu");
  g_object_unref(top);
  g_object_unref(view);
  g_object_unref(menu);

  GtkWidget *header = gtk_header_bar_new();
  gtk_header_bar_set_show_close_button(GTK_HEADER_BAR(header), TRUE);
  gtk_header_bar_set_title(GTK_HEADER_BAR(header), title);
  gtk_header_bar_pack_end(GTK_HEADER_BAR(header), button);
  gtk_window_set_titlebar(GTK_WINDOW(main_window), header);
  g_signal_connect(main_view, "notify::title", G_CALLBACK(on_title_changed), header);

  gtk_container_add(GTK_CONTAINER(main_window), GTK_WIDGET(main_view));
  g_signal_connect(main_window, "destroy", G_CALLBACK(on_main_destroy), NULL);
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

int host_run(const char *app_id, int argc, char **argv) {
  g_set_prgname(app_id);
  gdk_set_program_class(app_id);
  if (!g_application_id_is_valid(app_id)) app_id = NULL;
  application = gtk_application_new(app_id, G_APPLICATION_HANDLES_OPEN);
  g_signal_connect(application, "activate", G_CALLBACK(on_activate), NULL);
  g_signal_connect(application, "open", G_CALLBACK(on_open), NULL);
  int status = g_application_run(G_APPLICATION(application), argc, argv);
  g_object_unref(application);
  application = NULL;
  return status;
}
