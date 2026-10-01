//go:build darwin && cgo

package shellagent

/*
#include <libproc.h>
*/
import "C"

import (
	"errors"
	"unsafe"
)

// processExecutablePath returns the executable image of a running process.
func processExecutablePath(pid int) (string, error) {
	buffer := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	length := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buffer[0]), C.uint32_t(len(buffer)))
	if length <= 0 {
		return "", errors.New("could not read the peer process path")
	}
	return string(buffer[:length]), nil
}
