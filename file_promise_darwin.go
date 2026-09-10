//go:build darwin

package shellagent

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>
void dynapp_file_promise_run(void);
void dynapp_file_promise_publish(const char *json);
void dynapp_file_promise_complete(const char *identifier, const char *errorMessage);
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

// AppKit must stay on the process main thread. Package initialization runs on
// that thread, so pin it before main has any opportunity to migrate.
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
				C.dynapp_file_promise_publish(value)
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
	return errors.New(fmt.Sprint("macOS file promise helper stopped"))
}
