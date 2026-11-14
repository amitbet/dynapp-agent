//go:build windows

package winhost

import (
	"encoding/json"
	webview2 "github.com/jchv/go-webview2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Optional cross-repository check against the authored shared UI, in a real
// DOM. Example: DYNAPP_SHARED_INJECTABLES=C:\tools\dynapp\shared\injectables.
func TestNativeWindowsTitlebarSharedUIWebView2(t *testing.T) {
	dir := os.Getenv("DYNAPP_SHARED_INJECTABLES")
	if os.Getenv("DYNAPP_TEST_WEBVIEW2") != "1" || dir == "" {
		t.Skip("set DYNAPP_TEST_WEBVIEW2 and DYNAPP_SHARED_INJECTABLES")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	badge, err := os.ReadFile(filepath.Join(dir, "versionBadge.js"))
	if err != nil {
		t.Fatal(err)
	}
	titlebar, err := os.ReadFile(filepath.Join(dir, "windowTitlebar.js"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := os.MkdirTemp("", "dynapp-titlebar-shared-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	view := webview2.NewWithOptions(webview2.WebViewOptions{DataPath: profile})
	if view == nil {
		t.Fatal("WebView2 unavailable")
	}
	defer view.Destroy()
	chromium, err := chromiumOf(view)
	if err != nil {
		t.Fatal(err)
	}
	var completed bool
	var failure string
	chromium.MessageCallback = func(raw string) {
		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(raw), &result) != nil {
			return
		}
		completed = result.OK
		failure = result.Error
		view.Terminate()
	}
	timer := time.AfterFunc(20*time.Second, func() { view.Dispatch(func() { failure = "shared UI timed out"; view.Terminate() }) })
	defer timer.Stop()
	source := strings.ReplaceAll(string(titlebar), "export function ", "function ")
	script := string(badge) + "\n" + source + `
 (async () => {
  try {
   const check=(value,message)=>{if(!value)throw new Error(message)};
   const shadows=new WeakMap();
   const attach=Element.prototype.attachShadow;
   Element.prototype.attachShadow=function(options){const root=attach.call(this,options);shadows.set(this,root);return root;};
   let user=null, logins=0, logouts=0;
   const pause=()=>new Promise(resolve=>setTimeout(resolve,30));
   installWindowTitlebar({nativeHost:{platform:'windows',titlebar:{setEnabled(){},drag(){},toggleMaximize(){}}},app:{name:'Test app',version:'1.2.3'},
     account:async()=>user,signIn:async()=>{logins++;user={user:{name:'Amit'}};},signOut:async()=>{logouts++;user=null;},onAccountChange(){}});
   await pause();
   const host=document.getElementById('dynapp-window-titlebar');check(host,'titlebar missing');
   check(document.getElementById('app').getBoundingClientRect().bottom<=innerHeight+1,'full-height app clipped');
   check(document.getElementById('toolbar').getBoundingClientRect().top>=35,'fixed toolbar overlaps titlebar');
   const root=shadows.get(host), badge=root.querySelector('.version'), button=root.querySelector('.user');
   check(badge.dataset.dynappVersionMenu==='true','shared version badge failed to mount');
   check(button.textContent==='Log in','initial login state');
   button.click();await pause();check(logins===1 && button.textContent==='Amit','login failed');
   button.click();check(!root.querySelector('.menu').hidden,'account menu did not open');
   root.querySelector('.menu button').click();await pause();check(logouts===1 && button.textContent==='Log in','logout failed');
   chrome.webview.postMessage(JSON.stringify({ok:true}));
  } catch(error) {chrome.webview.postMessage(JSON.stringify({ok:false,error:String(error.stack||error)}));}
 })();`
	// SetHtml runs the test after DOM ready so the assertions follow installation.
	view.SetHtml("<!doctype html><html><head></head><body style='margin:0'><div id='app' style='height:100vh'><div id='toolbar' style='position:fixed;top:0;left:0;right:0'>Toolbar</div></div><script>window.addEventListener('load',()=>{" + strings.ReplaceAll(script, "</script>", "<\\/script>") + "});</script></body></html>")
	view.Run()
	if !completed {
		t.Fatalf("shared titlebar UI: %s", failure)
	}
}
