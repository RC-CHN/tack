package protocol

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// File data is flushed before rename. Windows filesystem durability is verified
// separately; directory FlushFileBuffers is not available through os.File.Sync.
func syncDirectory(path string) error { return nil }

func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var overlap syscall.Overlapped
	if ok, _, err := proc.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlap))); ok == 0 {
		f.Close()
		return nil, fmt.Errorf("state lock unavailable: %w", err)
	}
	return func() { f.Close() }, nil
}
