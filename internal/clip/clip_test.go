//go:build windows

package clip

import (
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

var procGetClipboardData = user32.NewProc("GetClipboardData")

// read returns the clipboard's CF_UNICODETEXT contents. It exists only to prove
// Write round-trips; nothing in chonk reads the clipboard.
func read(t *testing.T) string {
	t.Helper()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := open(); err != nil {
		t.Fatalf("reopening the clipboard: %v", err)
	}
	defer procCloseClipboard.Call()

	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		t.Fatalf("GetClipboardData: %v", err)
	}
	p := lock(h)
	if p == nil {
		t.Fatal("GlobalLock returned nothing")
	}
	defer procGlobalUnlock.Call(h)

	return windows.UTF16PtrToString((*uint16)(p))
}

// TestWriteRoundTrips is the only check that matters for this package: that a
// path with non-ASCII in it survives, which is the whole reason for going
// native instead of piping to clip.exe.
func TestWriteRoundTrips(t *testing.T) {
	const want = `C:\Users\kristján\Zajęcia\日本語\big file.bin`

	if err := Write(want); err != nil {
		// A session with no usable window station — some CI runners — cannot
		// reach the clipboard at all. That is not a failure of this code.
		t.Skipf("clipboard unavailable: %v", err)
	}
	if got := read(t); got != want {
		t.Errorf("clipboard = %q, want %q", got, want)
	}
}

func TestWriteEmptyString(t *testing.T) {
	if err := Write(""); err != nil {
		t.Skipf("clipboard unavailable: %v", err)
	}
	if got := read(t); got != "" {
		t.Errorf("clipboard = %q, want empty", got)
	}
}
