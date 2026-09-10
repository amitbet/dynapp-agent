//go:build windows

package desktop

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var captureGDI = windows.NewLazySystemDLL("gdi32.dll")

func gdiCall(name string, args ...uintptr) uintptr {
	result, _, _ := captureGDI.NewProc(name).Call(args...)
	return result
}
func screenCaptureSupported() bool { return true }

type captureRect struct{ Left, Top, Right, Bottom int32 }
type captureMonitorInfo struct {
	Size          uint32
	Monitor, Work captureRect
	Flags         uint32
	Device        [32]uint16
}
type captureBitmapInfo struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}

// The callback lives for the helper process. Allocating a Windows callback for
// every screenshot would exhaust Go's fixed callback table.
var captureDisplays *[]screenDisplay
var captureMonitorCallback = syscall.NewCallback(func(monitor, dc, rect, param uintptr) uintptr {
	info := captureMonitorInfo{}
	info.Size = uint32(unsafe.Sizeof(info))
	if uiCall("GetMonitorInfoW", monitor, uintptr(unsafe.Pointer(&info))) != 0 && captureDisplays != nil {
		id := windows.UTF16ToString(info.Device[:])
		*captureDisplays = append(*captureDisplays, screenDisplay{ID: id, Name: id, Width: int(info.Monitor.Right - info.Monitor.Left), Height: int(info.Monitor.Bottom - info.Monitor.Top), ScaleFactor: 1, Primary: info.Flags&1 != 0, left: int(info.Monitor.Left), top: int(info.Monitor.Top)})
	}
	return 1
})

func (w *windowsPresentation) listDisplays() ([]screenDisplay, error) {
	var displays []screenDisplay
	captureDisplays = &displays
	defer func() { captureDisplays = nil }()
	if uiCall("EnumDisplayMonitors", 0, 0, captureMonitorCallback, 0) == 0 {
		return nil, errors.New("Windows could not enumerate displays")
	}
	return displays, nil
}
func (w *windowsPresentation) captureScreen(display screenDisplay, region screenRegion) ([]byte, error) {
	screenDC := uiCall("GetDC", 0)
	if screenDC == 0 {
		return nil, errors.New("Windows could not access the interactive desktop")
	}
	defer uiCall("ReleaseDC", 0, screenDC)
	memoryDC := gdiCall("CreateCompatibleDC", screenDC)
	if memoryDC == 0 {
		return nil, errors.New("could not allocate screen capture context")
	}
	defer gdiCall("DeleteDC", memoryDC)
	info := captureBitmapInfo{Width: int32(region.Width), Height: -int32(region.Height), Planes: 1, BitCount: 32}
	info.Size = uint32(unsafe.Sizeof(info))
	var pixels unsafe.Pointer
	bitmap := gdiCall("CreateDIBSection", screenDC, uintptr(unsafe.Pointer(&info)), 0, uintptr(unsafe.Pointer(&pixels)), 0, 0)
	if bitmap == 0 || pixels == nil {
		return nil, errors.New("could not allocate screen capture bitmap")
	}
	defer gdiCall("DeleteObject", bitmap)
	previous := gdiCall("SelectObject", memoryDC, bitmap)
	defer gdiCall("SelectObject", memoryDC, previous)
	// CAPTUREBLT includes layered application windows. Protected content and
	// the secure desktop remain subject to Windows capture restrictions.
	if gdiCall("BitBlt", memoryDC, 0, 0, uintptr(region.Width), uintptr(region.Height), screenDC, uintptr(display.left+region.X), uintptr(display.top+region.Y), 0x00CC0020|0x40000000) == 0 {
		return nil, errors.New("Windows screen capture failed")
	}
	gdiCall("GdiFlush")
	imageData := image.NewRGBA(image.Rect(0, 0, region.Width, region.Height))
	bgra := unsafe.Slice((*byte)(pixels), region.Width*region.Height*4)
	for i := 0; i < len(bgra); i += 4 {
		imageData.Pix[i] = bgra[i+2]
		imageData.Pix[i+1] = bgra[i+1]
		imageData.Pix[i+2] = bgra[i]
		imageData.Pix[i+3] = 255
	}
	var output bytes.Buffer
	if err := png.Encode(&output, imageData); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
