//go:build windows

package scan

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fsctlQueryUSNJournal            = 0x000900f4
	fsctlReadUSNJournal             = 0x000900bb
	fsctlReadUnprivilegedUSNJournal = 0x000903ab
	fsctlGetNTFSVolumeData          = 0x00090064
)

type readUSNJournalDataV1 struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
	MinMajorVersion   uint16
	MaxMajorVersion   uint16
}

func openVolumeProbe(drive string, access, flags uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(`\\.\` + drive)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
}

func isElevated() bool {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	return tok.IsElevated()
}

// TestUSNProbe reports which USN journal operations are available to the
// current process. Run with:
//
//	go test ./internal/scan -run USNProbe -v
func TestUSNProbe(t *testing.T) {
	if os.Getenv("CHONK_PROBE") == "" {
		t.Skip("set CHONK_PROBE=1 to probe USN capabilities")
	}
	t.Logf("elevated: %v", isElevated())

	accesses := []struct {
		name string
		val  uint32
	}{
		{"0", 0},
		{"FILE_READ_ATTRIBUTES", windows.FILE_READ_ATTRIBUTES},
		{"FILE_READ_DATA", windows.FILE_READ_DATA},
		{"FILE_READ_ATTR|SYNCHRONIZE", windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE},
		{"GENERIC_READ", windows.GENERIC_READ},
	}
	flagses := []struct {
		name string
		val  uint32
	}{
		{"none", 0},
		{"BACKUP_SEMANTICS", windows.FILE_FLAG_BACKUP_SEMANTICS},
	}

	for _, fl := range flagses {
		for _, a := range accesses {
			h, err := openVolumeProbe(`C:`, a.val, fl.val)
			if err != nil {
				t.Logf("open[flags=%s access=%s] FAILED: %v", fl.name, a.name, err)
				continue
			}

			// A control code known to work on a volume handle, to prove the
			// handle itself is sound before blaming the USN codes.
			var vol [128]byte
			var ret uint32
			volErr := windows.DeviceIoControl(h, fsctlGetNTFSVolumeData,
				nil, 0, &vol[0], uint32(len(vol)), &ret, nil)

			// USN_JOURNAL_DATA has grown over releases; offer plenty of room.
			var jd [128]byte
			qErr := windows.DeviceIoControl(h, fsctlQueryUSNJournal,
				nil, 0, &jd[0], uint32(len(jd)), &ret, nil)

			status := "QUERY ok"
			if qErr != nil {
				status = "QUERY " + qErr.Error()
			} else {
				id := *(*uint64)(unsafe.Pointer(&jd[0]))
				next := *(*int64)(unsafe.Pointer(&jd[16]))
				status += " id=" + hex(id) + " next=" + dec(next)
			}
			ntfs := "NTFSDATA ok"
			if volErr != nil {
				ntfs = "NTFSDATA " + volErr.Error()
			}
			t.Logf("open[flags=%-16s access=%-26s] -> %s | %s", fl.name, a.name, ntfs, status)

			if qErr == nil {
				probeReads(t, h, jd[:])
			}
			windows.CloseHandle(h)
		}
	}
}

func probeReads(t *testing.T, h windows.Handle, jd []byte) {
	t.Helper()
	id := *(*uint64)(unsafe.Pointer(&jd[0]))
	first := *(*int64)(unsafe.Pointer(&jd[8]))
	probeReadAt(t, h, id, first)
}

// chainRead performs one unprivileged journal read and returns the cursor to
// resume from, which is the first 8 bytes of the output buffer.
func chainRead(t *testing.T, h windows.Handle, id uint64, start int64, label string) int64 {
	t.Helper()
	in := readUSNJournalDataV1{
		StartUsn:        start,
		ReasonMask:      0xFFFFFFFF,
		UsnJournalID:    id,
		MinMajorVersion: 2,
		MaxMajorVersion: 3,
	}
	out := make([]byte, 64<<10)
	var n uint32
	err := windows.DeviceIoControl(h, fsctlReadUnprivilegedUSNJournal,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		&out[0], uint32(len(out)), &n, nil)
	if err != nil {
		t.Logf("        chain[%-24s] start=%d -> FAILED: %v", label, start, err)
		return start
	}
	next := *(*int64)(unsafe.Pointer(&out[0]))
	var recs int
	for off := 8; off+4 <= int(n); {
		rl := int(*(*uint32)(unsafe.Pointer(&out[off])))
		if rl <= 0 {
			break
		}
		recs++
		off += rl
	}
	t.Logf("        chain[%-24s] start=%d -> ok %d bytes, %d records, next=%d",
		label, start, n, recs, next)
	return next
}

func probeReadAt(t *testing.T, h windows.Handle, id uint64, first int64) {
	t.Helper()

	for _, rd := range []struct {
		name string
		code uint32
	}{
		{"READ_USN_JOURNAL", fsctlReadUSNJournal},
		{"READ_UNPRIVILEGED_USN_JOURNAL", fsctlReadUnprivilegedUSNJournal},
	} {
		in := readUSNJournalDataV1{
			StartUsn:        first,
			ReasonMask:      0xFFFFFFFF,
			UsnJournalID:    id,
			MinMajorVersion: 2,
			MaxMajorVersion: 3,
		}
		out := make([]byte, 64<<10)
		var n uint32
		err := windows.DeviceIoControl(h, rd.code,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			&out[0], uint32(len(out)), &n, nil)
		if err != nil {
			t.Logf("        %-32s -> FAILED: %v", rd.name, err)
			continue
		}
		var recs int
		for off := 8; off+4 <= int(n); {
			rl := int(*(*uint32)(unsafe.Pointer(&out[off])))
			if rl <= 0 {
				break
			}
			recs++
			off += rl
		}
		t.Logf("        %-32s -> ok, %d bytes, %d records", rd.name, n, recs)
	}
}

func hex(v uint64) string {
	const d = "0123456789abcdef"
	var b [18]byte
	b[0], b[1] = '0', 'x'
	for i := 0; i < 16; i++ {
		b[17-i] = d[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

func dec(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
