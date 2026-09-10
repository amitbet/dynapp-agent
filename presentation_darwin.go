//go:build darwin && cgo

package shellagent

/*
#cgo LDFLAGS: -framework AppKit -framework Carbon
#include <stdlib.h>
void dynapp_presentation_run(void);
int dynapp_presentation_tray(const char *json);
void dynapp_presentation_destroy(void);
void dynapp_presentation_stop(void);
int dynapp_presentation_register(unsigned int id, unsigned int key, unsigned int modifiers);
void dynapp_presentation_unregister(unsigned int id);
*/
import "C"
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"time"
	"unsafe"
)

type macPresentationEvent struct {
	kind, value string
	id          uint32
}

var macPresentationEvents = make(chan macPresentationEvent, 128)

func init()                       { runtime.LockOSThread() }
func presentationSupported() bool { return true }
func launchPresentation(exe string, args []string, token string) (func(), error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("native UI requires the per-user macOS LaunchAgent; install and run the agent as the signed-in user")
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "DYNAPP_PRESENTATION_TOKEN="+token)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	return func() { _ = cmd.Process.Kill() }, nil
}

//export dynappGoPresentationEvent
func dynappGoPresentationEvent(kind, value *C.char, id C.uint) {
	select {
	case macPresentationEvents <- macPresentationEvent{C.GoString(kind), C.GoString(value), uint32(id)}:
	default:
	}
}

type macPresentation struct{}

func (macPresentation) setTray(options trayOptions) error {
	data, _ := json.Marshal(options)
	value := C.CString(string(data))
	defer C.free(unsafe.Pointer(value))
	if C.dynapp_presentation_tray(value) == 0 {
		return errors.New("could not create macOS status item")
	}
	return nil
}
func (macPresentation) destroyTray() { C.dynapp_presentation_destroy() }
func (macPresentation) balloon(title, body string) bool {
	// The agent is a signed command-line binary, not an application bundle.
	// Pass notification text as argv, never as executable AppleScript source.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "on run argv\n display notification (item 2 of argv) with title (item 1 of argv)\nend run", "--", title, body).Run() == nil
}
func (macPresentation) register(id uint32, key presentationKey) bool {
	// Carbon uses physical ANSI virtual keys. These are the same named keys
	// regardless of the currently selected text input method.
	codes := map[uint32]uint32{'A': 0, 'S': 1, 'D': 2, 'F': 3, 'H': 4, 'G': 5, 'Z': 6, 'X': 7, 'C': 8, 'V': 9, 'B': 11, 'Q': 12, 'W': 13, 'E': 14, 'R': 15, 'Y': 16, 'T': 17, '1': 18, '2': 19, '3': 20, '4': 21, '6': 22, '5': 23, 187: 24, '9': 25, '7': 26, 189: 27, '8': 28, '0': 29, 'O': 31, 'U': 32, 'I': 34, 'P': 35, 13: 36, 'L': 37, 'J': 38, 'K': 40, 188: 43, 190: 47, 'N': 45, 'M': 46, 9: 48, 32: 49, 8: 51, 27: 53, 112: 122, 113: 120, 114: 99, 115: 118, 116: 96, 117: 97, 118: 98, 119: 100, 120: 101, 121: 109, 122: 103, 123: 111, 124: 105, 125: 107, 126: 113, 127: 106, 128: 64, 129: 79, 130: 80, 131: 90, 45: 114, 36: 115, 33: 116, 46: 117, 35: 119, 34: 121, 37: 123, 39: 124, 40: 125, 38: 126}
	code, ok := codes[key.Code]
	if !ok {
		return false
	}
	return C.dynapp_presentation_register(C.uint(id), C.uint(code), C.uint(key.Modifiers)) != 0
}
func (macPresentation) unregister(id uint32) { C.dynapp_presentation_unregister(C.uint(id)) }
func runNativePresentation(commands <-chan presentationCommand, emit func(presentationReply)) error {
	controller := &presentationController{native: macPresentation{}, platform: "darwin"}
	go func() {
		for {
			select {
			case command, ok := <-commands:
				if !ok {
					controller.close()
					C.dynapp_presentation_stop()
					return
				}
				result, err := controller.handle(command)
				reply := presentationReply{ID: command.ID, Result: result}
				if err != nil {
					reply.Error = err.Error()
				}
				emit(reply)
			case event := <-macPresentationEvents:
				reply := presentationReply{Service: "tray"}
				switch event.kind {
				case "hotkey":
					reply.Service = "globalShortcut"
					reply.Event = controller.hotkey(event.id)
				case "menu":
					reply.Event = controller.menuClick(event.value)
				case "click":
					reply.Event = map[string]any{"kind": event.value}
				}
				if reply.Event != nil {
					emit(reply)
				}
			}
		}
	}()
	C.dynapp_presentation_run()
	return nil
}
