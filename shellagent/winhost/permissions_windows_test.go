//go:build windows

package winhost

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Exercise the actual modal dialog and common controls on Windows, without
// granting a real app access or needing a person to click through the UI.
func TestNativeWindowsPermissionChecklist(t *testing.T) {
	groups := []permissionGroup{
		{Title: "Files", Danger: "medium", Suggested: true, Permissions: []string{"fs.read", "fs.list"}},
		{Title: "Secrets", Danger: "very-high", Suggested: true, Granted: true, Permissions: []string{"secrets.read"}},
		{Title: "Network", Danger: "high", Permissions: []string{"net.tcp.connect"}},
	}
	for _, tc := range []struct {
		kind        string
		save, empty bool
		want        []string
	}{
		{"delta", true, false, []string{"fs.read", "fs.list", "net.tcp.connect"}},
		{"review", true, false, []string{"secrets.read", "net.tcp.connect"}},
		{"review", true, true, []string{}},
		{"review", false, false, nil},
	} {
		t.Run(fmt.Sprintf("%s-save-%v-empty-%v", tc.kind, tc.save, tc.empty), func(t *testing.T) {
			name := fmt.Sprintf("DynApp permission test %d", time.Now().UnixNano())
			type decision struct {
				caps  []string
				saved bool
			}
			done := make(chan decision, 1)
			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				caps, saved := askPermissions(0, tc.kind, name, "https://example.test", groups)
				done <- decision{caps, saved}
			}()
			var hwnd uintptr
			deadline := time.Now().Add(10 * time.Second)
			for hwnd == 0 && time.Now().Before(deadline) {
				hwnd, _, _ = procFindWindow.Call(0, uintptr(unsafe.Pointer(utf16Ptr(name+" permissions"))))
				if hwnd == 0 {
					select {
					case result := <-done:
						t.Fatalf("dialog exited before opening: %+v", result)
					default:
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			if hwnd == 0 {
				t.Fatal("permission dialog did not open")
			}
			// Post, rather than send: the UI thread can always process cleanup.
			defer user32.NewProc("PostMessageW").Call(hwnd, 0x10, 0, 0)
			list, _, _ := procGetDlgItem.Call(hwnd, permissionListID)
			// FindWindow may see the HWND before WM_INITDIALOG has finished.
			for time.Now().Before(deadline) {
				count, _, _ := procSendMessage.Call(list, lvmFirst+4, 0, 0)
				if count == uintptr(len(groups)) {
					break
				}
				time.Sleep(20 * time.Millisecond)
				list, _, _ = procGetDlgItem.Call(hwnd, permissionListID)
			}
			count, _, _ := procSendMessage.Call(list, lvmFirst+4, 0, 0)
			if count != uintptr(len(groups)) {
				t.Fatalf("rows=%d, want %d", count, len(groups))
			}
			for i, group := range groups {
				state, _, _ := procSendMessage.Call(list, lvmGetItemState, uintptr(i), 0xf000)
				if got := state&0xf000 == 0x2000; got != permissionInitiallyChecked(tc.kind, group) {
					t.Fatalf("row %d checked=%v", i, got)
				}
				// Choose a different subset from the defaults; Save must read the UI.
				item := permissionListItem{StateMask: 0xf000, State: 0x1000}
				if !tc.empty && (i == 2 || i == 1 && tc.kind == "review" || i == 0 && tc.kind != "review") {
					item.State = 0x2000
				}
				procSendMessage.Call(list, lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&item)))
			}
			if tc.save {
				procSendMessage.Call(hwnd, 0x113, 1, 0) // elapsed guard timer
				procSendMessage.Call(hwnd, 0x111, 1, 0)
			} else {
				procSendMessage.Call(hwnd, 0x10, 0, 0)
			}
			select {
			case got := <-done:
				if got.saved != tc.save || !reflect.DeepEqual(got.caps, tc.want) {
					t.Fatalf("decision=%+v, want saved=%v caps=%#v", got, tc.save, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("permission dialog did not finish")
			}
		})
	}
}
