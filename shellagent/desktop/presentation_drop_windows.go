//go:build windows

package desktop

import (
	"errors"
	"math"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfHDrop         = 15
	tymedHGlobal    = 1
	dvAspectContent = 1
	dropEffectNone  = 0
	dropEffectCopy  = 1
	wsPopup         = 0x80000000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExLayered     = 0x00080000
	wsExNoActivate  = 0x08000000
	swpNoActivate   = 0x0010
	swpShowWindow   = 0x0040
	swHide          = 0
	lwaAlpha        = 0x00000002
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	sOK             = 0
	eNoInterface    = 0x80004002
)

var (
	dropOle32     = windows.NewLazySystemDLL("ole32.dll")
	dropShell32   = windows.NewLazySystemDLL("shell32.dll")
	dropGdi32     = windows.NewLazySystemDLL("gdi32.dll")
	iidIUnknown   = windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidDropTarget = windows.GUID{Data1: 0x00000122, Data2: 0, Data3: 0, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
)

type dropFormatEtc struct {
	Format uint16
	Target uintptr
	Aspect uint32
	Index  int32
	Medium uint32
}

type dropStorageMedium struct {
	Medium       uint32
	Handle       uintptr
	ReleaseOwner uintptr
}

type windowsDropTargetVTable struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	DragEnter      uintptr
	DragOver       uintptr
	DragLeave      uintptr
	Drop           uintptr
}

type windowsDropTarget struct {
	vtable *windowsDropTargetVTable
	refs   int32
	owner  *windowsPresentation
}

func sameGUID(pointer uintptr, wanted windows.GUID) bool {
	if pointer == 0 {
		return false
	}
	return *(*windows.GUID)(unsafe.Pointer(pointer)) == wanted
}

func setDropEffect(pointer uintptr, effect uint32) {
	if pointer != 0 {
		*(*uint32)(unsafe.Pointer(pointer)) = effect
	}
}

func dropTargetFrom(pointer uintptr) *windowsDropTarget {
	return (*windowsDropTarget)(unsafe.Pointer(pointer))
}

func newWindowsDropTarget(owner *windowsPresentation) *windowsDropTarget {
	target := &windowsDropTarget{refs: 1, owner: owner}
	target.vtable = &windowsDropTargetVTable{
		QueryInterface: syscall.NewCallback(func(this, iid, result uintptr) uintptr {
			if result == 0 {
				return eNoInterface
			}
			if sameGUID(iid, iidIUnknown) || sameGUID(iid, iidDropTarget) {
				*(*uintptr)(unsafe.Pointer(result)) = this
				atomic.AddInt32(&dropTargetFrom(this).refs, 1)
				return sOK
			}
			*(*uintptr)(unsafe.Pointer(result)) = 0
			return eNoInterface
		}),
		AddRef: syscall.NewCallback(func(this uintptr) uintptr {
			return uintptr(atomic.AddInt32(&dropTargetFrom(this).refs, 1))
		}),
		Release: syscall.NewCallback(func(this uintptr) uintptr {
			return uintptr(atomic.AddInt32(&dropTargetFrom(this).refs, -1))
		}),
		DragEnter: syscall.NewCallback(func(this, dataObject, keyState, point, effect uintptr) uintptr {
			value := dropTargetFrom(this)
			paths := windowsDropPaths(dataObject)
			value.owner.dropAllowed = len(paths) > 0 && value.owner.dropID != ""
			if value.owner.dropAllowed {
				value.owner.dropDeadline = time.Now().Add(time.Second)
				value.owner.emit(presentationReply{Service: "dropTarget", Event: map[string]any{"type": "entered", "targetId": value.owner.dropID, "count": len(paths)}})
				setDropEffect(effect, dropEffectCopy)
			} else {
				setDropEffect(effect, dropEffectNone)
			}
			return sOK
		}),
		DragOver: syscall.NewCallback(func(this, keyState, point, effect uintptr) uintptr {
			value := dropTargetFrom(this).owner
			if value.dropAllowed && value.dropID != "" {
				value.dropDeadline = time.Now().Add(time.Second)
				setDropEffect(effect, dropEffectCopy)
			} else {
				setDropEffect(effect, dropEffectNone)
			}
			return sOK
		}),
		DragLeave: syscall.NewCallback(func(this uintptr) uintptr {
			value := dropTargetFrom(this).owner
			if value.dropID != "" {
				value.emit(presentationReply{Service: "dropTarget", Event: map[string]any{"type": "left", "targetId": value.dropID}})
			}
			value.hideDrop()
			return sOK
		}),
		Drop: syscall.NewCallback(func(this, dataObject, keyState, point, effect uintptr) uintptr {
			value := dropTargetFrom(this).owner
			paths := windowsDropPaths(dataObject)
			identifier := value.dropID
			if identifier != "" && len(paths) > 0 {
				value.emit(presentationReply{Service: "dropTarget", Event: map[string]any{"type": "dropped", "targetId": identifier, "paths": paths}})
				setDropEffect(effect, dropEffectCopy)
			} else {
				setDropEffect(effect, dropEffectNone)
			}
			value.hideDrop()
			return sOK
		}),
	}
	return target
}

func windowsDropPaths(dataObject uintptr) []string {
	if dataObject == 0 {
		return nil
	}
	format := dropFormatEtc{Format: cfHDrop, Aspect: dvAspectContent, Index: -1, Medium: tymedHGlobal}
	var medium dropStorageMedium
	vtable := *(*uintptr)(unsafe.Pointer(dataObject))
	getData := *(*uintptr)(unsafe.Pointer(vtable + 3*unsafe.Sizeof(uintptr(0))))
	result, _, _ := syscall.SyscallN(getData, dataObject, uintptr(unsafe.Pointer(&format)), uintptr(unsafe.Pointer(&medium)))
	if result != sOK || medium.Medium != tymedHGlobal || medium.Handle == 0 {
		return nil
	}
	defer dropOle32.NewProc("ReleaseStgMedium").Call(uintptr(unsafe.Pointer(&medium)))
	dragQuery := dropShell32.NewProc("DragQueryFileW")
	count, _, _ := dragQuery.Call(medium.Handle, 0xffffffff, 0, 0)
	if count > 128 {
		count = 128
	}
	paths := make([]string, 0, count)
	for index := uintptr(0); index < count; index++ {
		length, _, _ := dragQuery.Call(medium.Handle, index, 0, 0)
		if length == 0 || length > 32767 {
			continue
		}
		buffer := make([]uint16, length+1)
		if written, _, _ := dragQuery.Call(medium.Handle, index, uintptr(unsafe.Pointer(&buffer[0])), length+1); written > 0 {
			paths = append(paths, windows.UTF16ToString(buffer))
		}
	}
	return paths
}

func (w *windowsPresentation) initializeDropTarget() error {
	result, _, _ := dropOle32.NewProc("OleInitialize").Call(0)
	if uint32(result) != 0 && uint32(result) != 1 {
		return errors.New("Windows could not initialize OLE drag and drop")
	}
	name := uiString("DynAppDropTargetHelper")
	instance, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	brush, _, _ := dropGdi32.NewProc("CreateSolidBrush").Call(0x00d77a1a)
	callback := syscall.NewCallback(func(hwnd uintptr, message uint32, wp, lp uintptr) uintptr {
		switch message {
		case wmClose:
			uiCall("ShowWindow", hwnd, swHide)
			return 0
		case wmDestroy:
			return 0
		}
		return uiCall("DefWindowProcW", hwnd, uintptr(message), wp, lp)
	})
	class := presentationWindowClass{Proc: callback, ClassName: name, Instance: instance, Background: brush}
	class.Size = uint32(unsafe.Sizeof(class))
	if uiCall("RegisterClassExW", uintptr(unsafe.Pointer(&class))) == 0 {
		return errors.New("could not register Windows drop-target window")
	}
	w.dropWindow = uiCall("CreateWindowExW", wsExTopmost|wsExToolWindow|wsExLayered|wsExNoActivate,
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), wsPopup, 0, 0, 0, 0, 0, 0, instance, 0)
	if w.dropWindow == 0 {
		return errors.New("could not create Windows drop-target window")
	}
	uiCall("SetLayeredWindowAttributes", w.dropWindow, 0, 48, lwaAlpha)
	w.dropTarget = newWindowsDropTarget(w)
	registerResult, _, _ := dropOle32.NewProc("RegisterDragDrop").Call(w.dropWindow, uintptr(unsafe.Pointer(w.dropTarget)))
	if registerResult != sOK {
		uiCall("DestroyWindow", w.dropWindow)
		w.dropWindow = 0
		return errors.New("Windows could not register the native drop target")
	}
	return nil
}

func (w *windowsPresentation) destroyDropTarget() {
	w.hideDrop()
	if w.dropWindow != 0 {
		dropOle32.NewProc("RevokeDragDrop").Call(w.dropWindow)
		uiCall("DestroyWindow", w.dropWindow)
		w.dropWindow = 0
	}
	w.dropTarget = nil
	dropOle32.NewProc("OleUninitialize").Call()
}

func (w *windowsPresentation) armDrop(bounds dropTargetBounds) error {
	if w.dropWindow == 0 {
		return errors.New("Windows drop target is unavailable")
	}
	scale := bounds.Scale
	x := int32(math.Round(bounds.X * scale))
	y := int32(math.Round(bounds.Y * scale))
	width := int32(math.Max(1, math.Round(bounds.Width*scale)))
	height := int32(math.Max(1, math.Round(bounds.Height*scale)))
	w.dropID = bounds.ID
	w.dropAllowed = false
	w.dropDeadline = time.Now().Add(time.Second)
	if uiCall("SetWindowPos", w.dropWindow, ^uintptr(0), uintptr(int64(x)), uintptr(int64(y)), uintptr(width), uintptr(height), swpNoActivate|swpShowWindow) == 0 {
		w.hideDrop()
		return errors.New("Windows could not position the native drop target")
	}
	return nil
}

func (w *windowsPresentation) hideDrop() {
	if w.dropWindow != 0 {
		uiCall("ShowWindow", w.dropWindow, swHide)
	}
	w.dropID = ""
	w.dropAllowed = false
	w.dropDeadline = time.Time{}
}
