package shellagent

import (
	"errors"
	"runtime"
)

func (s *Server) handleDragRPC(socket protocolSocket, request message, body []byte) (any, error) {
	switch request.Method {
	case "startPromises":
		if runtime.GOOS != "darwin" {
			return nil, errors.New("native file dragging is available only on macOS")
		}
		return s.publishFilePromises(socket, request, "drag", true)
	case "writePromiseBinary":
		return s.writeClipboardPromiseBinary(request, body)
	case "promiseStatus":
		return s.clipboardPromiseStatus(request)
	case "setPromiseSize":
		return s.setClipboardPromiseSize(request)
	case "finishPromise":
		return s.finishClipboardPromise(request)
	case "cancelPromises":
		return s.cancelFilePromises(request, "drag")
	default:
		return nil, errors.New("unsupported drag method")
	}
}
