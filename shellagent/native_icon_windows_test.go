//go:build windows

package shellagent

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestNativeWindowsIconLoads(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 80, G: 120, B: 220, A: 255})
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "icon.ico")
	if err := os.WriteFile(path, pngToICO(data.Bytes()), 0600); err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(path)
	dll := windows.NewLazySystemDLL("user32.dll")
	icon, _, err := dll.NewProc("LoadImageW").Call(0, uintptr(unsafe.Pointer(name)), 1, 32, 32, 0x10)
	if icon == 0 {
		t.Fatalf("Windows could not load the installed icon: %v", err)
	}
	dll.NewProc("DestroyIcon").Call(icon)
}
