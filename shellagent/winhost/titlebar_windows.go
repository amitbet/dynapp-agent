//go:build windows

package winhost

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

// Keep the real DWM caption buttons. The web view occupies the extended frame
// except for their rectangle; clipping its child HWND lets DWM paint and hit
// test the original controls (including Windows 11's snap flyout).
var (
	dwm                  = windows.NewLazySystemDLL("dwmapi.dll")
	gdi                  = windows.NewLazySystemDLL("gdi32.dll")
	procDwmDefWindowProc = dwm.NewProc("DwmDefWindowProc")
	procDwmExtendFrame   = dwm.NewProc("DwmExtendFrameIntoClientArea")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procGetWindow        = user32.NewProc("GetWindow")
	procSetWindowRgn     = user32.NewProc("SetWindowRgn")
	procCreateRectRgn    = gdi.NewProc("CreateRectRgn")
	procCombineRgn       = gdi.NewProc("CombineRgn")
	procDeleteObject     = gdi.NewProc("DeleteObject")
	procReleaseCapture   = user32.NewProc("ReleaseCapture")
	procIsZoomed         = user32.NewProc("IsZoomed")
	procGetDpiForWindow  = user32.NewProc("GetDpiForWindow")
)

type titlebarRect struct{ Left, Top, Right, Bottom int32 }

func (h *host) titlebarScale() int32 {
	dpi, _, _ := procGetDpiForWindow.Call(h.hwnd)
	if dpi == 0 {
		dpi = 96
	}
	return int32(dpi)
}

func (h *host) setTitlebar(enabled bool) {
	if h.titlebarEnabled == enabled {
		return
	}
	h.titlebarEnabled = enabled
	height := int32(0)
	if enabled {
		height = 36 * h.titlebarScale() / 96
	}
	margins := [4]int32{0, 0, height, 0}
	procDwmExtendFrame.Call(h.hwnd, uintptr(unsafe.Pointer(&margins)))
	procSetWindowPos.Call(h.hwnd, 0, 0, 0, 0, 0, 0x0037) // FRAMECHANGED | NOMOVE | NOSIZE | NOZORDER | NOACTIVATE
	h.clipTitlebarButtons()
}

func (h *host) clipTitlebarButtons() {
	var bounds titlebarRect
	procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&bounds)))
	// WebView2's controller owns a child window. Clip every immediate child,
	// rather than depending on Chromium's private window class name.
	child, _, _ := procGetWindow.Call(h.hwnd, 5) // GW_CHILD
	for child != 0 {
		if h.titlebarEnabled {
			region, _, _ := procCreateRectRgn.Call(0, 0, uintptr(bounds.Right), uintptr(bounds.Bottom))
			dpi := h.titlebarScale()
			cutout, _, _ := procCreateRectRgn.Call(uintptr(max(0, bounds.Right-148*dpi/96)), 0, uintptr(bounds.Right), uintptr(36*dpi/96))
			procCombineRgn.Call(region, region, cutout, 4) // RGN_DIFF
			procDeleteObject.Call(cutout)
			if ok, _, _ := procSetWindowRgn.Call(child, region, 1); ok == 0 {
				procDeleteObject.Call(region)
			}
		} else {
			procSetWindowRgn.Call(child, 0, 1)
		}
		child, _, _ = procGetWindow.Call(child, 2) // GW_HWNDNEXT
	}
}

func (h *host) titlebarMessage(hwnd, message, wparam, lparam uintptr) (uintptr, bool) {
	if !h.titlebarEnabled {
		return 0, false
	}
	switch message {
	case 0x0083: // WM_NCCALCSIZE: keep resize borders; extend only the caption.
		if wparam != 0 {
			rect := (*titlebarRect)(unsafe.Pointer(lparam))
			top := rect.Top
			procCallWindowProc.Call(h.oldProc, hwnd, message, wparam, lparam)
			zoomed, _, _ := procIsZoomed.Call(hwnd)
			if zoomed == 0 {
				rect.Top = top
			} else {
				// Preserve the invisible maximized resize border outside the work area.
				border, _, _ := procGetSystemMetrics.Call(33)  // SM_CYSIZEFRAME
				padding, _, _ := procGetSystemMetrics.Call(92) // SM_CXPADDEDBORDER
				rect.Top = top + int32(border+padding)
			}
			return 0, true
		}
	case 0x0084, 0x00A0, 0x00A1, 0x00A2, 0x02A2: // non-client hit/mouse messages
		var result uintptr
		if handled, _, _ := procDwmDefWindowProc.Call(hwnd, message, wparam, lparam, uintptr(unsafe.Pointer(&result))); handled != 0 {
			return result, true
		}
		if message == 0x0084 {
			var rect titlebarRect
			procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
			y := int32(int16(lparam>>16)) - rect.Top
			if y >= 0 && y < 5*h.titlebarScale()/96 {
				return 12, true
			} // HTTOP
		}
	case 0x0005, 0x02E0: // WM_SIZE / WM_DPICHANGED
		result, _, _ := procCallWindowProc.Call(h.oldProc, hwnd, message, wparam, lparam)
		h.clipTitlebarButtons()
		return result, true
	}
	return 0, false
}
