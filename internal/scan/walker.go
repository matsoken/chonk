//go:build windows

// Package scan enumerates a Windows directory tree into a flat, pointer-free
// array of entries. It uses GetFileInformationByHandleEx with the
// FileIdBothDirectoryInfo class, which fills a large buffer with many entries
// per syscall instead of one entry per call like FindNextFile.
//
// Design notes:
//   - Entry contains no pointers, so the GC never scans the entry slice.
//   - Names live in one big []uint16 arena; they stay UTF-16 (what the OS gave
//     us) and are converted to Go strings only on demand for display.
//   - A directory is always appended before its children, so ParentIdx < index
//     for every entry. That makes the size rollup a single reverse loop.
package scan

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Entry flags.
const (
	FlagDir uint16 = 1 << iota
	FlagReparse
	FlagCompressed
	FlagSparse
	FlagUnreadable // directory we could not open or fully enumerate
	FlagDedup      // file whose bytes were credited to an earlier hardlink
	FlagDeleted    // gone since the cached scan; kept to preserve entry indices
)

// Entry is 40 bytes and contains no pointers.
type Entry struct {
	FileID    uint64 // volume-unique file ID; use for hardlink dedupe
	Size      int64  // logical bytes. After Rollup, dirs hold subtree totals.
	Alloc     int64  // bytes on disk. After Rollup, dirs hold subtree totals.
	ParentIdx uint32
	NameOff   uint32 // offset into Tree.names
	NameLen   uint16 // in UTF-16 code units
	Flags     uint16
}

// Tree is the result of a scan. Entries[0] is the root.
type Tree struct {
	Entries []Entry
	names   []uint16
	mu      sync.Mutex

	// Read without the lock by progress reporters while Scan is running.
	seen atomic.Int64

	// Set once a directory rejects the FileId enumeration class, so the rest of
	// the scan goes straight to the fallback. See scanDir.
	preferFallback atomic.Bool
	fallbackDirs   atomic.Int64
}

// Progress reports how many entries have been committed so far. Safe to call
// from another goroutine while Scan is in flight.
func (t *Tree) Progress() int64 { return t.seen.Load() }

// FallbackDirs reports how many directories were enumerated with the fallback
// info class. Nonzero means hardlink dedupe is incomplete, because that class
// carries no FileId.
func (t *Tree) FallbackDirs() int { return int(t.fallbackDirs.Load()) }

// Name returns the entry's own name (not its full path).
func (t *Tree) Name(i uint32) string {
	e := &t.Entries[i]
	return string(windows.UTF16ToString(t.names[e.NameOff : e.NameOff+uint32(e.NameLen)]))
}

// Path reconstructs the full path by walking the parent chain.
func (t *Tree) Path(i uint32) string {
	var parts []string
	for {
		parts = append(parts, t.Name(i))
		if i == 0 {
			break
		}
		i = t.Entries[i].ParentIdx
	}
	for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
		parts[l], parts[r] = parts[r], parts[l]
	}
	return strings.Join(parts, `\`)
}

// Children returns the direct children of dir. This is a linear scan; if you
// need it repeatedly, build a child index once instead.
func (t *Tree) Children(dir uint32) []uint32 {
	var out []uint32
	for i := dir + 1; i < uint32(len(t.Entries)); i++ {
		if t.Entries[i].ParentIdx == dir {
			out = append(out, i)
		}
	}
	return out
}

// Rollup propagates sizes bottom-up so every directory holds its subtree total.
// Because a parent is always appended before its children, iterating in reverse
// visits every node after all of its descendants.
func (t *Tree) Rollup() {
	for i := len(t.Entries) - 1; i >= 1; i-- {
		e := &t.Entries[i]
		p := &t.Entries[e.ParentIdx]
		p.Size += e.Size
		p.Alloc += e.Alloc
	}
}

// ---------------------------------------------------------------------------
// FILE_ID_BOTH_DIR_INFO
// ---------------------------------------------------------------------------

// FILE_INFO_BY_HANDLE_CLASS values.
const (
	classFileIDBothDirectoryInfo        = 10
	classFileIDBothDirectoryRestartInfo = 11
)

// fileIDBothDirInfo mirrors FILE_ID_BOTH_DIR_INFO. The explicit padding byte
// after ShortNameLength makes Go's natural alignment reproduce the C layout:
// ShortName lands at 70, FileId at 96 (after 2 bytes of tail padding), and the
// variable-length FileName array begins at 104.
type fileIDBothDirInfo struct {
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
	ShortNameLength uint8
	_               uint8
	ShortName       [12]uint16
	FileID          uint64
	// FileName [1]uint16 follows here.
}

const nameFieldOffset = 104

// Compile-time assertion that the struct layout matches nameFieldOffset.
// If this fails to build, the layout assumption above is wrong.
var _ = [1]struct{}{}[unsafe.Sizeof(fileIDBothDirInfo{})-nameFieldOffset]

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

const bufSize = 64 << 10

// newDirBuf allocates an enumeration buffer that is guaranteed 8-byte aligned.
//
// GetFileInformationByHandleEx fails with ERROR_NOACCESS ("Invalid access to
// memory location") if its output buffer is not aligned for the 64-bit fields
// in the structures it writes. Go guarantees no particular alignment for a
// plain []byte — in practice one can land at 4 mod 8 — so the buffer is
// allocated as []uint64 and reinterpreted. Every caller must use this rather
// than make([]byte, bufSize).
func newDirBuf() []byte {
	raw := make([]uint64, bufSize/8)
	return unsafe.Slice((*byte)(unsafe.Pointer(&raw[0])), bufSize)
}

type item struct {
	fileID  uint64
	size    int64
	alloc   int64
	nameOff uint32 // offset into dirResult.names
	nameLen uint16
	flags   uint16
}

type dirResult struct {
	names []uint16
	items []item
}

type job struct {
	idx  uint32
	path string
}

type pool struct {
	mu     sync.Mutex
	cond   *sync.Cond
	stack  []job
	active int
}

func (p *pool) get() (job, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.stack) == 0 {
		if p.active == 0 {
			p.cond.Broadcast() // wake the other idle workers so they exit too
			return job{}, false
		}
		p.cond.Wait()
	}
	j := p.stack[len(p.stack)-1]
	p.stack = p.stack[:len(p.stack)-1]
	p.active++
	return j, true
}

func (p *pool) done(next []job) {
	p.mu.Lock()
	p.stack = append(p.stack, next...)
	p.active--
	// Most directories have no subdirectories, so broadcasting on every
	// completion wakes every idle worker to find an empty stack. Wake exactly
	// as many as there is new work for, and broadcast only when the count of
	// running workers hits zero, which is what termination hinges on.
	if p.active == 0 {
		p.cond.Broadcast()
	} else {
		for range next {
			p.cond.Signal()
		}
	}
	p.mu.Unlock()
}

// Options tunes a scan. The zero value is the default: automatic worker count,
// preferred enumeration class, no progress reporting.
type Options struct {
	// Workers is the number of concurrent directory readers. Zero or less
	// picks a default based on CPU count.
	Workers int

	// ForceFallback skips the FileId enumeration class entirely and uses the
	// more widely supported one. Useful on network shares that accept the
	// FileId class but return junk, and for exercising the fallback path.
	// Costs hardlink dedupe, since that class carries no FileId.
	ForceFallback bool

	// Handle, if non-nil, receives the Tree before the walk begins, so a caller
	// in another goroutine can poll Progress for a live count. Only Progress is
	// safe to call before the scan returns; the entry array is still growing.
	Handle chan<- *Tree
}

// Scan walks the tree rooted at root. Pass workers <= 0 for a sensible default.
// root should be an absolute path such as `C:\` or `C:\Users\me`.
func Scan(root string, workers int) (*Tree, error) {
	return ScanOpts(root, Options{Workers: workers})
}

// ScanOpts is Scan with explicit options.
func ScanOpts(root string, o Options) (*Tree, error) {
	workers := o.Workers
	if workers <= 0 {
		workers = runtime.NumCPU() * 2
	}
	// The \\?\ prefix is needed on the paths we hand to CreateFile, but it must
	// not leak into the stored name or every displayed path carries it.
	walkRoot := longPath(root)

	t := &Tree{}
	rootName, err := windows.UTF16FromString(strings.TrimSuffix(root, `\`))
	if err != nil {
		return nil, fmt.Errorf("bad root path: %w", err)
	}
	rootName = rootName[:len(rootName)-1] // drop trailing NUL
	t.names = append(t.names, rootName...)
	t.Entries = append(t.Entries, Entry{
		ParentIdx: 0,
		NameOff:   0,
		NameLen:   uint16(len(rootName)),
		Flags:     FlagDir,
	})

	// The root's own FileID is never seen by enumeration, which only reports
	// children. The delta cache needs it to recognize changes to the root's own
	// contents, so fetch it directly. A failure here is not fatal: it only
	// costs the cache the ability to patch the top level.
	if h, err := openDir(walkRoot); err == nil {
		if id, err := fileIDOf(h); err == nil {
			t.Entries[0].FileID = id
		}
		windows.CloseHandle(h)
	}

	if o.ForceFallback {
		t.preferFallback.Store(true)
	}
	if o.Handle != nil {
		o.Handle <- t
	}

	p := &pool{stack: []job{{idx: 0, path: walkRoot}}}
	p.cond = sync.NewCond(&p.mu)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// One aligned buffer per worker, reused for every directory.
			buf := newDirBuf()

			for {
				j, ok := p.get()
				if !ok {
					return
				}
				res, complete := scanDir(j.path, buf, t)
				next := t.commit(j.idx, j.path, res, complete)
				p.done(next)
			}
		}()
	}
	wg.Wait()
	return t, nil
}

// commit appends one directory's worth of entries under the global lock and
// returns jobs for the subdirectories it found. One lock acquisition per
// directory, not per file.
func (t *Tree) commit(parent uint32, parentPath string, r dirResult, complete bool) []job {
	t.mu.Lock()
	base := uint32(len(t.Entries))
	nameBase := uint32(len(t.names))
	t.names = append(t.names, r.names...)
	for _, it := range r.items {
		t.Entries = append(t.Entries, Entry{
			FileID:    it.fileID,
			Size:      it.size,
			Alloc:     it.alloc,
			ParentIdx: parent,
			NameOff:   nameBase + it.nameOff,
			NameLen:   it.nameLen,
			Flags:     it.flags,
		})
	}
	if !complete {
		t.Entries[parent].Flags |= FlagUnreadable
	}
	t.mu.Unlock()
	t.seen.Add(int64(len(r.items)))

	var next []job
	for k, it := range r.items {
		// Descend into real directories only. Reparse points (junctions,
		// symlinks, OneDrive placeholders) are recorded but never followed,
		// or C:\Users\All Users sends us into a loop.
		if it.flags&FlagDir == 0 || it.flags&FlagReparse != 0 {
			continue
		}
		name := windows.UTF16ToString(r.names[it.nameOff : it.nameOff+uint32(it.nameLen)])
		next = append(next, job{
			idx:  base + uint32(k),
			path: joinPath(parentPath, name),
		})
	}
	return next
}

// scanDir enumerates one directory, falling back to a more widely supported
// info class if the preferred one is rejected. The bool reports whether
// enumeration completed; false means the directory was skipped or truncated.
func scanDir(path string, buf []byte, t *Tree) (dirResult, bool) {
	h, err := openDir(path)
	if err != nil {
		return dirResult{}, false
	}
	defer windows.CloseHandle(h)

	// Once a volume has rejected the FileId class, stop paying a failed syscall
	// per directory to rediscover that. The flag only ever goes one way; the
	// cost of being wrong is losing FileIds (so no hardlink dedupe) on a
	// subtree that would have supported them, never a wrong size.
	if t.preferFallback.Load() {
		t.fallbackDirs.Add(1)
		r, complete, _ := enumerate(h, buf, classFull)
		return r, complete
	}

	r, complete, unsupported := enumerate(h, buf, classIDBoth)
	if !unsupported {
		return r, complete
	}

	// The restart class resets enumeration state, so the same handle can be
	// reused for the second attempt.
	t.preferFallback.Store(true)
	t.fallbackDirs.Add(1)
	r, complete, _ = enumerate(h, buf, classFull)
	return r, complete
}

func openDir(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to get a handle to a directory.
	// Share everything, or busy directories fail to open.
	return windows.CreateFile(p,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0)
}

func isDotDir(name []uint16) bool {
	switch len(name) {
	case 1:
		return name[0] == '.'
	case 2:
		return name[0] == '.' && name[1] == '.'
	}
	return false
}

// longPath adds the \\?\ prefix so MAX_PATH does not apply. The prefix disables
// path normalization, so the input must already be absolute and use backslashes.
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return `\\?\` + trimSep(p[4:])
	}
	if strings.HasPrefix(p, `\\`) { // UNC
		return `\\?\UNC\` + trimSep(p[2:])
	}
	return `\\?\` + trimSep(p)
}

// trimSep drops a trailing separator, except after a bare drive specifier where
// it is load-bearing: `\\?\C:\` is the root directory, while `\\?\C:` names the
// volume itself and cannot be opened as a directory.
func trimSep(p string) string {
	s := strings.TrimSuffix(p, `\`)
	if len(s) == 2 && s[1] == ':' {
		return s + `\`
	}
	return s
}

// joinPath appends a child name, tolerating a parent that already ends in a
// separator. Because \\?\ suppresses normalization, a doubled backslash is not
// cleaned up for us — it is simply an invalid path.
func joinPath(parent, name string) string {
	if strings.HasSuffix(parent, `\`) {
		return parent + name
	}
	return parent + `\` + name
}

// ---------------------------------------------------------------------------
// Headline numbers, no scan required
// ---------------------------------------------------------------------------

// DiskUsage returns total and used bytes for the volume containing path.
// This is a single syscall and does not depend on Scan.
func DiskUsage(path string) (total, used uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return totalBytes, totalBytes - totalFree, nil
}
