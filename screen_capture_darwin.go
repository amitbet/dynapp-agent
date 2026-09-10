//go:build darwin && cgo

package shellagent

/*
#cgo LDFLAGS: -framework ScreenCaptureKit -framework CoreGraphics
#include <stdlib.h>
int dynapp_screen_supported(void);
char *dynapp_screen_displays(void);
int dynapp_screen_capture(unsigned int display, int x, int y, int width, int height, void **data, size_t *length, char **error);
*/
import "C"
import (
	"encoding/json"
	"errors"
	"strconv"
	"unsafe"
)

func screenCaptureSupported() bool { return C.dynapp_screen_supported() != 0 }
func (macPresentation) listDisplays() ([]screenDisplay, error) {
	value := C.dynapp_screen_displays()
	if value == nil {
		return nil, errors.New("could not list displays")
	}
	defer C.free(unsafe.Pointer(value))
	var displays []screenDisplay
	err := json.Unmarshal([]byte(C.GoString(value)), &displays)
	return displays, err
}
func (macPresentation) captureScreen(display screenDisplay, region screenRegion) ([]byte, error) {
	id, err := strconv.ParseUint(display.ID, 10, 32)
	if err != nil {
		return nil, errors.New("invalid display id")
	}
	var data unsafe.Pointer
	var length C.size_t
	var message *C.char
	result := C.dynapp_screen_capture(C.uint(id), C.int(region.X), C.int(region.Y), C.int(region.Width), C.int(region.Height), &data, &length, &message)
	if message != nil {
		defer C.free(unsafe.Pointer(message))
	}
	if data != nil {
		defer C.free(data)
	}
	if result == 0 {
		if message != nil {
			return nil, errors.New(C.GoString(message))
		}
		return nil, errors.New("screen capture failed")
	}
	if length > maxScreenshotBytes {
		return nil, errors.New("screenshot exceeds 8 MiB; choose a smaller region")
	}
	return C.GoBytes(data, C.int(length)), nil
}
