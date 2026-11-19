// GTK 3 / WebKitGTK 4.1 side of the Linux app host. Every function here runs
// on the GTK main thread; Go reaches it from other goroutines through
// host_dispatch.
#ifndef DYNAPP_GTKHOST_H
#define DYNAPP_GTKHOST_H

#include <stdint.h>

// host_check opens the display; it returns NULL on success or an error.
const char *host_check(void);
// host_run starts the GApplication. argv holds the program name and the
// files to open; the call returns when the application quits.
int host_run(const char *app_id, int argc, char **argv);
// host_create_window builds the window and web view. user_script is the
// bridge to inject into the top frame, or NULL to run without the bridge.
void host_create_window(const char *title, const char *icon, const char *data_dir, const char *cache_dir,
                        const char *user_script, int debug);
void host_load(const char *url);
void host_reload(void);
void host_eval(const char *script);
void host_present(void);
int host_has_window(void);
void host_show_error(const char *title, const char *message);
void host_open_external(const char *uri);
// host_show_permission_page shows the agent's prompt in its own web view.
void host_show_permission_page(const char *id, const char *html);
void host_resize_permission_page(const char *id, int height);
void host_close_permission_page(const char *id);
void host_dispatch(uintptr_t handle);
void host_quit(void);

#endif
