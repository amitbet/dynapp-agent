//go:build windows

package winhost

import (
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Fake COM objects exercise the actual ABI slots and ownership used to read
// WebView2's AdditionalObjects, including duplicate names and folder paths.
type dropCOM struct {
	table *[7]uintptr
	iid   windows.GUID
	child *dropCOM
	files []*dropCOM
	path  string
	refs  int
}

var dropTestTable = [7]uintptr{
	syscall.NewCallback(func(this, iid, result uintptr) uintptr {
		o := (*dropCOM)(unsafe.Pointer(this))
		*(*uintptr)(unsafe.Pointer(result)) = 0
		if *(*windows.GUID)(unsafe.Pointer(iid)) != o.iid {
			return 0x80004002
		}
		o.refs++
		*(*uintptr)(unsafe.Pointer(result)) = this
		return 0
	}),
	syscall.NewCallback(func(this uintptr) uintptr { o := (*dropCOM)(unsafe.Pointer(this)); o.refs++; return uintptr(o.refs) }),
	syscall.NewCallback(func(this uintptr) uintptr { o := (*dropCOM)(unsafe.Pointer(this)); o.refs--; return uintptr(o.refs) }),
	syscall.NewCallback(func(this, result uintptr) uintptr {
		o := (*dropCOM)(unsafe.Pointer(this))
		if o.iid == iidWebViewFile {
			text := windows.StringToUTF16(o.path)
			memory, _, _ := windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemAlloc").Call(uintptr(len(text) * 2))
			copy(unsafe.Slice((*uint16)(unsafe.Pointer(memory)), len(text)), text)
			*(*uintptr)(unsafe.Pointer(result)) = memory
		} else {
			*(*uint32)(unsafe.Pointer(result)) = uint32(len(o.files))
		}
		return 0
	}),
	syscall.NewCallback(func(this, index, result uintptr) uintptr {
		o := (*dropCOM)(unsafe.Pointer(this)).files[index]
		o.refs++
		*(*uintptr)(unsafe.Pointer(result)) = uintptr(unsafe.Pointer(o))
		return 0
	}),
	0,
	syscall.NewCallback(func(this, result uintptr) uintptr {
		o := (*dropCOM)(unsafe.Pointer(this)).child
		o.refs++
		*(*uintptr)(unsafe.Pointer(result)) = uintptr(unsafe.Pointer(o))
		return 0
	}),
}

func TestNativeWindowsDropPaths(t *testing.T) {
	for _, paths := range [][]string{
		{`C:\one\same.mkv`, `C:\two\same.mkv`, `C:\Videos\תיקייה`},
		{`\\server\share\movie.mkv`},
		{},
	} {
		collection := &dropCOM{table: &dropTestTable}
		for _, path := range paths {
			collection.files = append(collection.files, &dropCOM{table: &dropTestTable, iid: iidWebViewFile, path: path})
		}
		args := &dropCOM{table: &dropTestTable, iid: iidMessageArgs2, child: collection}
		got, err := webViewDropPaths(uintptr(unsafe.Pointer(args)))
		runtime.KeepAlive(args)
		if err != nil || !reflect.DeepEqual(got, paths) {
			t.Fatalf("paths = %q, %v; want %q", got, err, paths)
		}
		if args.refs != 0 || collection.refs != 0 {
			t.Fatal("COM references leaked")
		}
		for _, file := range collection.files {
			if file.refs != 0 {
				t.Fatal("file reference leaked")
			}
		}
	}
}

func TestNativeWindowsDropRejectsSyntheticFileAndOldRuntime(t *testing.T) {
	for _, supported := range []bool{false, true} {
		file := &dropCOM{table: &dropTestTable, iid: iidWebViewFile, path: ""}
		collection := &dropCOM{table: &dropTestTable, files: []*dropCOM{file}}
		args := &dropCOM{table: &dropTestTable, child: collection}
		if supported {
			args.iid = iidMessageArgs2
		}
		_, err := webViewDropPaths(uintptr(unsafe.Pointer(args)))
		runtime.KeepAlive(args)
		if err == nil {
			t.Fatal("missing native path was accepted")
		}
		if args.refs != 0 || collection.refs != 0 || file.refs != 0 {
			t.Fatal("COM references leaked on error")
		}
	}
}
