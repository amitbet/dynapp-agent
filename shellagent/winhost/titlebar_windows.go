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
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procFillRect         = user32.NewProc("FillRect")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procGetStockObject   = gdi.NewProc("GetStockObject")
)

type titlebarPaint struct {
	HDC       uintptr
	Erase     int32
	Paint     titlebarRect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

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
	procInvalidateRect.Call(h.hwnd, 0, 1)
}

// captionButtonsRect is the client-area rectangle the DWM caption buttons
// occupy once the frame extends into the client area.
func (h *host) captionButtonsRect() titlebarRect {
	var bounds titlebarRect
	procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&bounds)))
	dpi := h.titlebarScale()
	return titlebarRect{Left: max(0, bounds.Right-148*dpi/96), Top: 0, Right: bounds.Right, Bottom: 36 * dpi / 96}
}

// paintCaptionButtons fills the caption-button rectangle with black. DWM only
// shows its buttons over extended-frame pixels the window leaves black
// (transparent); the window class has no background brush, so without this
// the area keeps stale opaque pixels and the buttons work but are invisible.
func (h *host) paintCaptionButtons(dc uintptr) {
	rect := h.captionButtonsRect()
	black, _, _ := procGetStockObject.Call(4) // BLACK_BRUSH
	procFillRect.Call(dc, uintptr(unsafe.Pointer(&rect)), black)
}

func (h *host) clipTitlebarButtons() {
	if h.titlebarEnabled {
		procInvalidateRect.Call(h.hwnd, 0, 1)
	}
	var bounds titlebarRect
	procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&bounds)))
	// WebView2's controller owns a child window. Clip every immediate child,
	// rather than depending on Chromium's private window class name.
	child, _, _ := procGetWindow.Call(h.hwnd, 5) // GW_CHILD
	for child != 0 {
		if h.titlebarEnabled {
			region, _, _ := procCreateRectRgn.Call(0, 0, uintptr(bounds.Right), uintptr(bounds.Bottom))
			buttons := h.captionButtonsRect()
			cutout, _, _ := procCreateRectRgn.Call(uintptr(buttons.Left), uintptr(buttons.Top), uintptr(buttons.Right), uintptr(buttons.Bottom))
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
	case 0x0014: // WM_ERASEBKGND
		h.paintCaptionButtons(wparam)
		return 1, true
	case 0x000F: // WM_PAINT
		var paint titlebarPaint
		dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&paint)))
		if dc != 0 {
			h.paintCaptionButtons(dc)
			procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&paint)))
		}
		return 0, true
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
