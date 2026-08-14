//go:build windows

// Package clip puts text on the Windows clipboard.
//
// The obvious alternatives are both worse here. Piping to clip.exe spawns a
// process and encodes through the console codepage, which mangles the accented
// and CJK path components that turn up constantly under Users and Downloads.
// OSC 52 escape sequences travel over SSH but legacy conhost ignores them and
// Windows Terminal gates them behind a setting, so a copy can silently do
// nothing with no way to detect it. The Win32 API is a few dozen lines and is
// always right locally, which is where a disk usage tool runs.
package clip

import (
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// open takes the clipboard, retrying briefly. Another application holding it is
// ordinary and transient — Explorer and clipboard managers both do it — so a
// single attempt fails often enough to be annoying.
func open() error {
	var err error
	for range 5 {
		r, _, e := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		err = e
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("clipboard is busy: %w", err)
}

// lock pins a global block and returns a pointer to it, or nil.
//
// GlobalLock hands back a uintptr addressing memory the Windows heap manager
// owns and will not move while the block is locked. That makes the conversion
// safe, but the unsafeptr vet check cannot know it and flags every uintptr to
// unsafe.Pointer conversion on principle, so the value is laundered through its
// own address here rather than converted directly.
func lock(h uintptr) unsafe.Pointer {
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

// Write replaces the clipboard contents with s, as CF_UNICODETEXT.
func Write(s string) error {
	u16, err := windows.UTF16FromString(s)
	if err != nil {
		return err
	}

	// Clipboard ownership is per-thread, and a goroutine may be rescheduled
	// onto a different OS thread at any call. Every step below has to run on
	// the thread that opened the clipboard.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := open(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	if r, _, e := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", e)
	}

	// UTF16FromString includes the terminating NUL, which the clipboard format
	// requires.
	size := uintptr(len(u16)) * unsafe.Sizeof(u16[0])
	h, _, e := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %w", e)
	}

	p := lock(h)
	if p == nil {
		procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %w", windows.GetLastError())
	}
	copy(unsafe.Slice((*uint16)(p), len(u16)), u16)
	procGlobalUnlock.Call(h)

	if r, _, e := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %w", e)
	}
	// The system owns the block now. Freeing it here would be a double free.
	return nil
}
