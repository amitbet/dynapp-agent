//go:build windows

package desktop

/*
#cgo windows CXXFLAGS: -std=c++17
#cgo windows LDFLAGS: -lole32 -lshell32 -luuid
#include <stdlib.h>
int dynapp_file_promise_publish(const char *json);
void dynapp_file_promise_complete(const char *identifier, const char *errorMessage);
void dynapp_file_promise_run(void);
void dynappGoFilePromiseRequested(char *identifier, char *path);
*/
import "C"

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"
)

var promiseHelperOutput sync.Mutex

// The Windows clipboard provider is an OLE STA object. Keep the helper's
// message pump on one OS thread, just as the macOS AppKit helper stays on its
// main thread.
func init() { runtime.LockOSThread() }

func platformFilePromisesSupported() bool { return true }

//export dynappGoFilePromiseRequested
func dynappGoFilePromiseRequested(identifier, path *C.char) {
	event := filePromiseEvent{Type: "request", ID: C.GoString(identifier), Path: C.GoString(path)}
	promiseHelperOutput.Lock()
	_ = json.NewEncoder(os.Stdout).Encode(event)
	promiseHelperOutput.Unlock()
}

func RunFilePromiseHelper() error {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var command filePromiseCommand
			if json.Unmarshal(scanner.Bytes(), &command) != nil {
				continue
			}
			switch command.Type {
			case "publish":
				data, _ := json.Marshal(command.Files)
				value := C.CString(string(data))
				_ = C.dynapp_file_promise_publish(value)
				C.free(unsafe.Pointer(value))
			case "complete":
				identifier, message := C.CString(command.ID), C.CString(command.Error)
				C.dynapp_file_promise_complete(identifier, message)
				C.free(unsafe.Pointer(identifier))
				C.free(unsafe.Pointer(message))
			}
		}
		os.Exit(0)
	}()
	C.dynapp_file_promise_run()
	return errors.New(fmt.Sprint("Windows file promise helper stopped"))
}
