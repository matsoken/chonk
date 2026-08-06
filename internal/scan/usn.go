//go:build windows

package scan

import (
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The USN change journal records every modification to every file on an NTFS
// volume. It cannot build a size tree on its own — USN_RECORD_V2 has no size
// field — but it can say precisely which directories changed since a given
// point, which is what makes a second scan nearly free.
//
// Reading it does not require administrator, contrary to most write-ups. Two
// details make the difference:
//
//   - The handle must be to the volume's *root directory* (`C:\` opened with
//     FILE_FLAG_BACKUP_SEMANTICS), not to the volume device (`\\.\C:`). The
//     device handle needs FILE_READ_DATA, which is admin-only; the root
//     directory handle works with no access rights at all.
//   - Reads must use FSCTL_READ_UNPRIVILEGED_USN_JOURNAL. The classic
//     FSCTL_READ_USN_JOURNAL returns ERROR_ACCESS_DENIED to a normal user.
//     The unprivileged variant filters out records for files the caller cannot
//     see, which for a disk usage tool is exactly the right behavior anyway.
const (
	fsctlQueryUSNJournal2            = 0x000900f4
	fsctlReadUnprivilegedUSNJournal2 = 0x000903ab
)

// ErrNoJournal means the volume has no usable change journal, so a delta scan
// is impossible and the caller should fall back to a full walk.
var ErrNoJournal = errors.New("usn journal unavailable")

// ErrJournalWrapped means the records we needed have aged out of the journal.
var ErrJournalWrapped = errors.New("usn journal wrapped past the cached position")

// Journal is an open handle to one volume's change journal.
type Journal struct {
	h    windows.Handle
	ID   uint64 // recreated journals get a new ID, invalidating any cache
	Head int64  // the next USN that will be written
	Low  int64  // oldest USN still present
}

// usnJournalData mirrors USN_JOURNAL_DATA_V0. Later versions append fields, so
// a larger output buffer is fine.
type usnJournalData struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

// readUSNJournalData mirrors READ_USN_JOURNAL_DATA_V1.
type readUSNJournalData struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
	MinMajorVersion   uint16
	MaxMajorVersion   uint16
}

// usnRecordV2 mirrors USN_RECORD_V2. Version 3 widens the file references to
// 128 bits, which would not match the 64-bit FileId the walker records, so the
// read below pins the record version to 2.
type usnRecordV2 struct {
	RecordLength              uint32
	MajorVersion              uint16
	MinorVersion              uint16
	FileReferenceNumber       uint64
	ParentFileReferenceNumber uint64
	Usn                       int64
	TimeStamp                 int64
	Reason                    uint32
	SourceInfo                uint32
	SecurityID                uint32
	FileAttributes            uint32
	FileNameLength            uint16
	FileNameOffset            uint16
	// FileName [1]uint16 follows.
}

var _ = [1]struct{}{}[unsafe.Offsetof(usnRecordV2{}.FileNameOffset)-58]

// OpenJournal opens the change journal for the volume containing path.
func OpenJournal(path string) (*Journal, error) {
	vol := filepath.VolumeName(path)
	if len(vol) != 2 || vol[1] != ':' {
		// A UNC share has no journal we can read from this side.
		return nil, fmt.Errorf("%w: %q is not a local volume", ErrNoJournal, path)
	}

	p, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return nil, err
	}
	// Zero desired access is deliberate and is what keeps this unprivileged.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: opening %s: %v", ErrNoJournal, vol, err)
	}

	var data [128]byte // room for whatever version this OS returns
	var ret uint32
	if err := windows.DeviceIoControl(h, fsctlQueryUSNJournal2,
		nil, 0, &data[0], uint32(len(data)), &ret, nil); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%w: querying %s: %v", ErrNoJournal, vol, err)
	}

	jd := (*usnJournalData)(unsafe.Pointer(&data[0]))
	return &Journal{h: h, ID: jd.UsnJournalID, Head: jd.NextUsn, Low: jd.FirstUsn}, nil
}

func (j *Journal) Close() error { return windows.CloseHandle(j.h) }

// Change is one journal record, reduced to the parts a size tree cares about.
type Change struct {
	File   uint64 // FileReferenceNumber, comparable with Entry.FileID
	Parent uint64 // the directory whose contents changed
	Reason uint32
	IsDir  bool
}

// USN reason bits.
const (
	usnReasonFileCreate = 0x00000100
	usnReasonFileDelete = 0x00000200
	usnReasonRenameNew  = 0x00002000
	usnReasonRenameOld  = 0x00001000
)

// Changes walks journal records from `from` up to the current head, calling fn
// for each. It returns the USN to resume from next time.
//
// `from` must be a USN the journal handed back previously (a record boundary),
// not an arbitrary offset — the driver rejects those with ERROR_INVALID_PARAMETER.
//
// If more than limit records are pending, it gives up and returns
// ErrJournalWrapped, since at that point a full rescan is cheaper anyway.
func (j *Journal) Changes(from int64, limit int, fn func(Change)) (int64, error) {
	if from < j.Low {
		return 0, fmt.Errorf("%w: cached position %d, oldest available %d",
			ErrJournalWrapped, from, j.Low)
	}
	if from > j.Head {
		// The journal was reset or truncated under us.
		return 0, fmt.Errorf("%w: cached position %d is past the head %d",
			ErrJournalWrapped, from, j.Head)
	}

	buf := make([]byte, 64<<10)
	cursor := from
	var seen int

	for cursor < j.Head {
		in := readUSNJournalData{
			StartUsn:        cursor,
			ReasonMask:      0xFFFFFFFF,
			UsnJournalID:    j.ID,
			MinMajorVersion: 2,
			MaxMajorVersion: 2,
		}
		var n uint32
		err := windows.DeviceIoControl(j.h, fsctlReadUnprivilegedUSNJournal2,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			&buf[0], uint32(len(buf)), &n, nil)
		if err != nil {
			return 0, fmt.Errorf("reading journal at %d: %w", cursor, err)
		}
		if n < 8 {
			break
		}

		next := *(*int64)(unsafe.Pointer(&buf[0]))
		off := 8
		for off+int(unsafe.Sizeof(usnRecordV2{})) <= int(n) {
			r := (*usnRecordV2)(unsafe.Pointer(&buf[off]))
			if r.RecordLength == 0 {
				break
			}
			fn(Change{
				File:   r.FileReferenceNumber,
				Parent: r.ParentFileReferenceNumber,
				Reason: r.Reason,
				IsDir:  r.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
			})
			seen++
			off += int(r.RecordLength)
		}

		if seen > limit {
			return 0, fmt.Errorf("%w: more than %d records pending", ErrJournalWrapped, limit)
		}
		// No forward progress means the journal has nothing further for us.
		if next <= cursor {
			cursor = next
			break
		}
		cursor = next
	}
	return cursor, nil
}

// VolumeSerial identifies the volume a cache belongs to, so a cache is not
// applied to a different disk that happens to be mounted at the same letter.
func VolumeSerial(path string) (uint32, error) {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return 0, fmt.Errorf("no volume in %q", path)
	}
	p, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return 0, err
	}
	var serial, maxComp, flags uint32
	err = windows.GetVolumeInformation(p, nil, 0, &serial, &maxComp, &flags, nil, 0)
	return serial, err
}

// fileIDOf returns the 64-bit file reference for an open handle, which is what
// the journal reports and what Entry.FileID holds.
func fileIDOf(h windows.Handle) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, err
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}
