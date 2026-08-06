//go:build windows

package scan

import (
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestUSNRaw calls DeviceIoControl through syscall directly so the raw
// GetLastError value is visible, rather than whatever the wrapper maps it to.
func TestUSNRaw(t *testing.T) {
	if os.Getenv("CHONK_PROBE") == "" {
		t.Skip("set CHONK_PROBE=1")
	}

	t.Logf("syscall.EINVAL formats as %q (value %d)", syscall.EINVAL.Error(), uintptr(syscall.EINVAL))

	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	devIoCtl := kernel32.NewProc("DeviceIoControl")
	getFileType := kernel32.NewProc("GetFileType")

	cases := []struct {
		path   string
		access uint32
		flags  uint32
	}{
		{`\\.\C:`, 0, 0},
		{`\\?\C:`, 0, 0},
		// A handle to the volume's root directory. Opening a directory needs
		// BACKUP_SEMANTICS, and FILE_READ_ATTRIBUTES is granted to everyone.
		{`C:\`, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS},
		{`\\?\C:\`, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS},
		{`C:\`, windows.FILE_LIST_DIRECTORY, windows.FILE_FLAG_BACKUP_SEMANTICS},
		{`C:\`, 0, windows.FILE_FLAG_BACKUP_SEMANTICS},
	}

	for _, c := range cases {
		path := c.path
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, c.access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, c.flags, 0)
		if err != nil {
			t.Logf("%-10s access=%#-10x open failed: %v", path, c.access, err)
			continue
		}
		t.Logf("--- %s access=%#x flags=%#x", path, c.access, c.flags)

		ft, _, _ := getFileType.Call(uintptr(h))
		t.Logf("%-24s opened, handle=%#x GetFileType=%d (1=disk)", path, uintptr(h), ft)

		var out [128]byte
		var ret uint32
		r1, _, lastErr := devIoCtl.Call(
			uintptr(h),
			uintptr(fsctlQueryUSNJournal),
			0, 0,
			uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)),
			uintptr(unsafe.Pointer(&ret)),
			0,
		)
		t.Logf("%-24s QUERY_USN r1=%d lastErr=%d (%v) bytes=%d",
			path, r1, uintptr(lastErr.(syscall.Errno)), lastErr, ret)

		if r1 != 0 {
			id := *(*uint64)(unsafe.Pointer(&out[0]))
			first := *(*int64)(unsafe.Pointer(&out[8]))
			next := *(*int64)(unsafe.Pointer(&out[16]))
			t.Logf("%-24s journal id=%#x first=%d next=%d", path, id, first, next)
			probeReads(t, h, out[:])

			// The delta pattern: read once, remember the returned cursor, then
			// resume from it. That cursor is a real record boundary, which an
			// arbitrary byte offset into the journal is not.
			cursor := chainRead(t, h, id, first, "first read")
			cursor = chainRead(t, h, id, cursor, "resume from cursor")
			// Resuming at the head should be valid and simply return nothing new.
			chainRead(t, h, id, next, "resume from journal head")
			_ = cursor
		}
		windows.CloseHandle(h)
	}
}
