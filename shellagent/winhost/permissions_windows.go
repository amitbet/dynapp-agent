//go:build windows

package winhost

import (
	"encoding/binary"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	unicode16 "unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procDialogBox          = user32.NewProc("DialogBoxIndirectParamW")
	procEndDialog          = user32.NewProc("EndDialog")
	procGetDlgItem         = user32.NewProc("GetDlgItem")
	procSetWindowText      = user32.NewProc("SetWindowTextW")
	procEnableWindow       = user32.NewProc("EnableWindow")
	procSetTimer           = user32.NewProc("SetTimer")
	procKillTimer          = user32.NewProc("KillTimer")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procInitCommonControls = windows.NewLazySystemDLL("comctl32.dll").NewProc("InitCommonControlsEx")
)

const (
	permissionListID    = 100
	permissionDetailsID = 101
	lvmFirst            = 0x1000
	lvmSetItemState     = lvmFirst + 43
	lvmGetItemState     = lvmFirst + 44
)

// The pointer-bearing fields use Go's native alignment on both Win32 and Win64.
type permissionListItem struct {
	Mask             uint32
	Item, SubItem    int32
	State, StateMask uint32
	Text             *uint16
	TextMax, Image   int32
	Param            uintptr
	Indent, GroupID  int32
	Columns          uint32
	ColumnIndices    *uint32
	ColumnFormats    *int32
	Group            int32
}

type permissionListColumn struct {
	Mask                                                               uint32
	Format, Width                                                      int32
	Text                                                               *uint16
	TextMax, SubItem, Image, Order, MinWidth, DefaultWidth, IdealWidth int32
}

type permissionNotification struct {
	Window, ID uintptr
	Code       uint32
}

// A DLGTEMPLATE owns the controls, keyboard navigation and modal lifetime.
// All strings are UTF-16, and each DLGITEMTEMPLATE starts on a DWORD boundary.
type permissionTemplate []byte

func (t *permissionTemplate) word(value uint16)  { *t = binary.LittleEndian.AppendUint16(*t, value) }
func (t *permissionTemplate) dword(value uint32) { *t = binary.LittleEndian.AppendUint32(*t, value) }
func (t *permissionTemplate) text(value string) {
	for _, unit := range unicode16.Encode([]rune(strings.ReplaceAll(value, "\x00", ""))) {
		t.word(unit)
	}
	t.word(0)
}
func (t *permissionTemplate) control(id uint16, class, title string, x, y, w, h uint16, style uint32) {
	for len(*t)%4 != 0 {
		*t = append(*t, 0)
	}
	t.dword(style | 0x50000000) // WS_CHILD | WS_VISIBLE
	t.dword(0)
	for _, n := range []uint16{x, y, w, h, id} {
		t.word(n)
	}
	t.text(class)
	t.text(title)
	t.word(0)
}

func makePermissionTemplate(kind, appName, origin string) permissionTemplate {
	var t permissionTemplate
	t.dword(0x80c808c0) // popup, caption, system menu, modal frame, center, font
	t.dword(0)
	for _, n := range []uint16{7, 0, 0, 400, 290, 0, 0} {
		t.word(n)
	}
	t.text(appName + " permissions")
	t.word(9)
	t.text("Segoe UI")
	heading := "Choose what " + appName + " may do on this computer."
	action, cancel := "Allow selected", "Don't Allow"
	if kind == "delta" {
		heading = appName + " wants more access. Choose the permissions to allow."
	}
	if kind == "review" {
		heading = "Review permissions for " + appName + "."
		action, cancel = "Save", "Cancel"
	}
	t.control(102, "STATIC", heading, 12, 10, 376, 25, 0x80) // SS_NOPREFIX
	t.control(103, "STATIC", origin, 12, 38, 376, 18, 0x80)
	t.control(permissionListID, "SysListView32", "", 12, 60, 376, 135, 0x00a1000d)                                                                         // report, single select, show selection, border, tab stop
	t.control(permissionDetailsID, "EDIT", "Select a permission to read its description. Use Space to toggle its checkbox.", 12, 201, 376, 43, 0x00210844) // read-only, multiline, scroll, no tab stop
	t.control(104, "STATIC", "You can change this later from the window menu > Permissions…", 12, 249, 376, 12, 0x80)
	t.control(2, "BUTTON", cancel, 208, 267, 85, 16, 0x00010001) // default is deny/cancel
	t.control(1, "BUTTON", action, 299, 267, 89, 16, 0x00010000)
	return t
}

// Windows callbacks cannot be freed. Allocate one for all permission dialogs,
// and retain per-dialog state only for the modal call's lifetime.
var permissionDialogs sync.Map
var permissionDialogSequence atomic.Uint64
var permissionDialogProc = syscall.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	if message == 0x110 {
		if handler, ok := permissionDialogs.Load(lparam); ok {
			permissionDialogs.Store(hwnd, handler)
		}
	}
	if handler, ok := permissionDialogs.Load(hwnd); ok {
		return handler.(func(uintptr, uint32, uintptr, uintptr) uintptr)(hwnd, message, wparam, lparam)
	}
	return 0
})

func askPermissions(owner uintptr, kind, appName, origin string, groups []permissionGroup) ([]string, bool) {
	// This window contains native controls only. It never loads app HTML or
	// exposes its decision callback to the app's JavaScript bridge.
	controls := struct{ Size, Classes uint32 }{8, 1} // ICC_LISTVIEW_CLASSES
	initialized, _, err := procInitCommonControls.Call(uintptr(unsafe.Pointer(&controls)))
	if initialized == 0 {
		log.Printf("permission controls: %v", err)
		return nil, false
	}
	template := makePermissionTemplate(kind, appName, origin)
	var capabilities []string
	var list uintptr
	ready := false
	handler := func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
		switch message {
		case 0x110: // WM_INITDIALOG
			list, _, _ = procGetDlgItem.Call(hwnd, permissionListID)
			procSendMessage.Call(list, lvmFirst+54, 0, 0x00010024) // checkboxes, full row selection, double buffering
			var rect struct{ Left, Top, Right, Bottom int32 }
			procGetClientRect.Call(list, uintptr(unsafe.Pointer(&rect)))
			width := rect.Right - rect.Left - 24
			for i, title := range []string{"Permission", "Risk"} {
				columnWidth := width * 3 / 4
				if i == 1 {
					columnWidth = width - columnWidth
				}
				column := permissionListColumn{Mask: 6, Width: columnWidth, Text: utf16Ptr(title)}
				procSendMessage.Call(list, lvmFirst+97, uintptr(i), uintptr(unsafe.Pointer(&column)))
			}
			for i, group := range groups {
				item := permissionListItem{Mask: 1, Item: int32(i), Text: utf16Ptr(group.Title)}
				procSendMessage.Call(list, lvmFirst+77, 0, uintptr(unsafe.Pointer(&item)))
				item.SubItem = 1
				item.Text = utf16Ptr(group.Danger)
				procSendMessage.Call(list, lvmFirst+116, uintptr(i), uintptr(unsafe.Pointer(&item)))
				item.StateMask = 0xf000
				item.State = 0x1000
				if permissionInitiallyChecked(kind, group) {
					item.State = 0x2000
				}
				procSendMessage.Call(list, lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&item)))
			}
			allow, _, _ := procGetDlgItem.Call(hwnd, 1)
			procEnableWindow.Call(allow, 0)
			procSetTimer.Call(hwnd, 1, 1000, 0)
			bringToFront(hwnd, swShowNormal)
			return 1
		case 0x113: // WM_TIMER: guard against an accidental click-through
			if wparam == 1 {
				ready = true
				allow, _, _ := procGetDlgItem.Call(hwnd, 1)
				procEnableWindow.Call(allow, 1)
				procKillTimer.Call(hwnd, 1)
			}
			return 1
		case 0x4e: // WM_NOTIFY
			// Copy the native WM_NOTIFY header without retaining an OS pointer.
			var notification permissionNotification
			if err := windows.ReadProcessMemory(windows.CurrentProcess(), lparam, (*byte)(unsafe.Pointer(&notification)), unsafe.Sizeof(notification), nil); err != nil {
				return 0
			}
			if notification.ID == permissionListID && notification.Code == 0xffffff9b { // LVN_ITEMCHANGED (-101)
				index, _, _ := procSendMessage.Call(list, lvmFirst+12, ^uintptr(0), 2) // LVNI_SELECTED
				if index < uintptr(len(groups)) {
					group := groups[index]
					lines := []string{group.Summary}
					for _, reason := range group.Reasons {
						lines = append(lines, "“"+reason+"”")
					}
					details, _, _ := procGetDlgItem.Call(hwnd, permissionDetailsID)
					procSetWindowText.Call(details, uintptr(unsafe.Pointer(utf16Ptr(strings.Join(lines, "\r\n")))))
				}
			}
		case 0x111: // WM_COMMAND
			switch wparam & 0xffff {
			case 1:
				if !ready {
					return 1
				}
				capabilities = selectedCapabilities(groups, func(i int) bool {
					state, _, _ := procSendMessage.Call(list, lvmGetItemState, uintptr(i), 0xf000)
					return state&0xf000 == 0x2000
				})
				procEndDialog.Call(hwnd, 1)
				return 1
			case 2:
				procEndDialog.Call(hwnd, 2)
				return 1
			}
		case 0x10: // WM_CLOSE is cancellation, never an empty saved grant
			procEndDialog.Call(hwnd, 2)
			return 1
		}
		return 0
	}
	// Tokens use odd values; Windows HWNDs are aligned, so the two keys cannot
	// collide in the registry. Capture the HWND for cleanup even on cancellation.
	token := uintptr(permissionDialogSequence.Add(1))*2 + 1
	var dialogWindow uintptr
	permissionDialogs.Store(token, func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
		dialogWindow = hwnd
		return handler(hwnd, message, wparam, lparam)
	})
	defer func() { permissionDialogs.Delete(token); permissionDialogs.Delete(dialogWindow) }()
	result, _, err := procDialogBox.Call(0, uintptr(unsafe.Pointer(&template[0])), owner, permissionDialogProc, token)
	runtime.KeepAlive(template)
	if result == ^uintptr(0) {
		log.Printf("permission dialog: %v", err)
	}
	return capabilities, result == 1
}

func utf16Ptr(value string) *uint16 {
	pointer, _ := windows.UTF16PtrFromString(strings.ReplaceAll(value, "\x00", ""))
	return pointer
}
