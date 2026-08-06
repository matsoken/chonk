//go:build windows

package scan

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// FileIdBothDirectoryInfo is not universal. Some network redirectors, and a few
// third-party filesystem drivers, reject the class outright with
// ERROR_INVALID_PARAMETER. FileFullDirectoryInfo (class 14) has the same
// buffer-walking shape and is far more widely implemented; it just has no
// FileId field, so hardlink dedupe is unavailable on that path.
const (
	classFileFullDirectoryInfo        = 14
	classFileFullDirectoryRestartInfo = 15
)

// dirInfoHeader is the prefix that FILE_ID_BOTH_DIR_INFO and FILE_FULL_DIR_INFO
// share. The two structs are byte-for-byte identical from NextEntryOffset
// through EaSize; they diverge only in what follows. That is what lets one
// decoder serve both classes.
//
// Go pads this to 72 bytes because of the int64 alignment, so the size is not
// the field span. EaSize ending at 68 is the fact that matters, since that is
// where FILE_FULL_DIR_INFO's FileName begins.
type dirInfoHeader struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    int64
	LastAccessTime  int64
	LastWriteTime   int64
	ChangeTime      int64
	EndOfFile       int64
	AllocationSize  int64
	FileAttributes  uint32
	FileNameLength  uint32 // bytes, not code units
	EaSize          uint32
}

var (
	hdrSample dirInfoHeader
	idSample  fileIDBothDirInfo
)

const (
	// Where FILE_FULL_DIR_INFO's variable-length FileName starts.
	fullNameOffset = unsafe.Offsetof(hdrSample.EaSize) + 4
	// Where FILE_ID_BOTH_DIR_INFO keeps its FileId.
	fileIDOffset = unsafe.Offsetof(idSample.FileID)
)

// Compile-time assertions on both layouts. If either fails, print
// unsafe.Offsetof for each field and correct the offset it names.
var (
	_ = [1]struct{}{}[fullNameOffset-68]
	_ = [1]struct{}{}[fileIDOffset-96]
	// The shared prefix really is shared: same offset for the last common field.
	_ = [1]struct{}{}[unsafe.Offsetof(hdrSample.EaSize)-unsafe.Offsetof(idSample.EaSize)]
)

// dirClass describes one enumeration class: the pair of restart/continue values
// and where the variable-length name sits relative to each entry.
type dirClass struct {
	restart  uint32
	continu  uint32
	nameOff  uintptr
	hasFilID bool
	name     string
}

var (
	classIDBoth = dirClass{
		restart: classFileIDBothDirectoryRestartInfo,
		continu: classFileIDBothDirectoryInfo,
		nameOff: fullNameOffset + 36, // 104
		name:    "FileIdBothDirectoryInfo",
	}
	classFull = dirClass{
		restart: classFileFullDirectoryRestartInfo,
		continu: classFileFullDirectoryInfo,
		nameOff: fullNameOffset, // 68
		name:    "FileFullDirectoryInfo",
	}
)

func init() {
	classIDBoth.hasFilID = true
	if classIDBoth.nameOff != nameFieldOffset {
		panic("FileIdBothDirectoryInfo name offset disagrees with walker.go")
	}
}

// enumerate walks one open directory handle with the given class.
//
// complete reports whether the directory was read to the end; unsupported
// distinguishes "this filesystem rejects this class" from an ordinary error, so
// the caller knows a retry with a different class is worth attempting.
func enumerate(h windows.Handle, buf []byte, c dirClass) (r dirResult, complete, unsupported bool) {
	class := c.restart
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		if err != nil {
			switch err {
			case windows.ERROR_NO_MORE_FILES:
				return r, true, false
			case windows.ERROR_INVALID_PARAMETER, windows.ERROR_NOT_SUPPORTED,
				windows.ERROR_INVALID_FUNCTION, windows.ERROR_CALL_NOT_IMPLEMENTED:
				// Only meaningful on the first call. Once entries have come
				// back the class is plainly supported and this is a real error.
				return r, false, class == c.restart
			default:
				return r, false, false
			}
		}
		class = c.continu

		off := 0
		for {
			info := (*dirInfoHeader)(unsafe.Pointer(&buf[off]))
			nlen := int(info.FileNameLength) / 2
			name := unsafe.Slice(
				(*uint16)(unsafe.Pointer(&buf[uintptr(off)+c.nameOff])), nlen)

			if !isDotDir(name) {
				var fileID uint64
				if c.hasFilID {
					fileID = *(*uint64)(unsafe.Pointer(&buf[uintptr(off)+fileIDOffset]))
				}

				attr := info.FileAttributes
				var flags uint16
				isDir := attr&windows.FILE_ATTRIBUTE_DIRECTORY != 0
				if isDir {
					flags |= FlagDir
				}
				if attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
					flags |= FlagReparse
				}
				if attr&windows.FILE_ATTRIBUTE_COMPRESSED != 0 {
					flags |= FlagCompressed
				}
				if attr&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
					flags |= FlagSparse
				}

				var size, alloc int64
				if !isDir {
					size, alloc = info.EndOfFile, info.AllocationSize
				}

				nameOff := uint32(len(r.names))
				r.names = append(r.names, name...) // copy out; buf gets reused
				r.items = append(r.items, item{
					fileID:  fileID,
					size:    size,
					alloc:   alloc,
					nameOff: nameOff,
					nameLen: uint16(nlen),
					flags:   flags,
				})
			}

			if info.NextEntryOffset == 0 {
				break
			}
			off += int(info.NextEntryOffset)
		}
	}
}
