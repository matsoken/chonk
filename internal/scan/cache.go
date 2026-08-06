//go:build windows

package scan

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A cache stores the tree exactly as the walker produced it — before Rollup, so
// directories still hold zero and only files carry bytes. Rollup is not
// reversible, and a patched tree has to be rolled up from scratch.
//
// Entry is pointer-free and fixed size, so the entry array and the UTF-16 name
// arena are written as raw memory. That is the whole reason loading a 3M entry
// tree is a single read rather than three million allocations.

const (
	cacheMagic   = "chonkcac"
	cacheVersion = 1

	// Past this many pending journal records, a full rescan is cheaper than
	// patching directory by directory.
	maxPendingRecords = 200_000
)

// ErrCacheMiss means no usable cache was found. It is not a failure; it just
// means a full scan is needed.
var ErrCacheMiss = errors.New("no usable cache")

type cacheHeader struct {
	Version      uint32
	EntrySize    uint32 // guards against a struct layout change
	VolumeSerial uint32
	Flags        uint32
	JournalID    uint64
	NextUsn      int64
	ScannedAt    int64
	EntryCount   uint64
	NamesLen     uint64
	RootLen      uint32
	_            uint32
}

// CacheInfo describes a loaded cache.
type CacheInfo struct {
	Root      string
	ScannedAt time.Time
	NextUsn   int64
	JournalID uint64
}

// CachePath returns where the cache for root lives: one file per scan root,
// under the user's local app data.
func CachePath(root string) (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "chonk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheName(root)+".chonk"), nil
}

// cacheName turns a path into a stable, filesystem-safe file name.
func cacheName(root string) string {
	clean := strings.ToLower(strings.TrimSuffix(filepath.Clean(root), `\`))
	var b strings.Builder
	for _, r := range clean {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	// A short hash keeps distinct roots distinct after the sanitizing above.
	var h uint64 = 1469598103934665603
	for i := 0; i < len(clean); i++ {
		h ^= uint64(clean[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%s-%016x", b.String(), h)
}

// ---------------------------------------------------------------------------
// Save / load
// ---------------------------------------------------------------------------

// Save writes the tree and the journal position it is current as of. The tree
// must not have been rolled up.
func (t *Tree) Save(path, root string, journalID uint64, nextUsn int64) error {
	serial, err := VolumeSerial(root)
	if err != nil {
		return err
	}

	// Write to a temporary file and rename, so an interrupted save cannot leave
	// a half-written cache that the next run would try to trust.
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.WriteString(cacheMagic); err != nil {
		f.Close()
		return err
	}
	hdr := cacheHeader{
		Version:      cacheVersion,
		EntrySize:    uint32(unsafe.Sizeof(Entry{})),
		VolumeSerial: serial,
		JournalID:    journalID,
		NextUsn:      nextUsn,
		ScannedAt:    time.Now().Unix(),
		EntryCount:   uint64(len(t.Entries)),
		NamesLen:     uint64(len(t.names)),
		RootLen:      uint32(len(root)),
	}
	if err := binary.Write(w, binary.LittleEndian, &hdr); err != nil {
		f.Close()
		return err
	}
	if _, err := w.WriteString(root); err != nil {
		f.Close()
		return err
	}
	if _, err := w.Write(asBytes(t.Entries)); err != nil {
		f.Close()
		return err
	}
	if _, err := w.Write(asBytes(t.names)); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadCache reads a cache written by Save. It validates the volume and struct
// layout, but not the journal: that is Delta's job.
func LoadCache(path, root string) (*Tree, CacheInfo, error) {
	var info CacheInfo

	f, err := os.Open(path)
	if err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	magic := make([]byte, len(cacheMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != cacheMagic {
		return nil, info, fmt.Errorf("%w: bad magic", ErrCacheMiss)
	}

	var hdr cacheHeader
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}
	if hdr.Version != cacheVersion {
		return nil, info, fmt.Errorf("%w: version %d", ErrCacheMiss, hdr.Version)
	}
	if hdr.EntrySize != uint32(unsafe.Sizeof(Entry{})) {
		return nil, info, fmt.Errorf("%w: entry layout changed", ErrCacheMiss)
	}

	serial, err := VolumeSerial(root)
	if err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}
	if serial != hdr.VolumeSerial {
		return nil, info, fmt.Errorf("%w: different volume", ErrCacheMiss)
	}

	rootBuf := make([]byte, hdr.RootLen)
	if _, err := io.ReadFull(r, rootBuf); err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}
	if !strings.EqualFold(string(rootBuf), root) {
		return nil, info, fmt.Errorf("%w: cache is for %q", ErrCacheMiss, rootBuf)
	}

	// Sanity-check the sizes before allocating from them.
	const maxEntries = 1 << 30
	if hdr.EntryCount == 0 || hdr.EntryCount > maxEntries || hdr.NamesLen > maxEntries {
		return nil, info, fmt.Errorf("%w: implausible header", ErrCacheMiss)
	}

	t := &Tree{
		Entries: make([]Entry, hdr.EntryCount),
		names:   make([]uint16, hdr.NamesLen),
	}
	if _, err := io.ReadFull(r, asBytes(t.Entries)); err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}
	if _, err := io.ReadFull(r, asBytes(t.names)); err != nil {
		return nil, info, fmt.Errorf("%w: %v", ErrCacheMiss, err)
	}

	// A corrupt ParentIdx would break the reverse-loop rollup, so verify the
	// invariant rather than trusting bytes off disk.
	for i := 1; i < len(t.Entries); i++ {
		e := &t.Entries[i]
		if e.ParentIdx >= uint32(i) {
			return nil, info, fmt.Errorf("%w: parent order violated at %d", ErrCacheMiss, i)
		}
		if uint64(e.NameOff)+uint64(e.NameLen) > hdr.NamesLen {
			return nil, info, fmt.Errorf("%w: name out of range at %d", ErrCacheMiss, i)
		}
	}

	info = CacheInfo{
		Root:      string(rootBuf),
		ScannedAt: time.Unix(hdr.ScannedAt, 0),
		NextUsn:   hdr.NextUsn,
		JournalID: hdr.JournalID,
	}
	return t, info, nil
}

// asBytes views a slice of fixed-size, pointer-free values as raw bytes. Safe
// only because Entry contains no pointers, which is invariant 1.
func asBytes[T any](s []T) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*int(unsafe.Sizeof(s[0])))
}

// ---------------------------------------------------------------------------
// Acquire
// ---------------------------------------------------------------------------

// AcquireOptions controls how a tree is obtained.
type AcquireOptions struct {
	Scan     Options
	UseCache bool // consult and update the on-disk cache
	Refresh  bool // ignore any cached tree, but write a fresh one
}

// AcquireResult describes where the tree came from.
type AcquireResult struct {
	Delta    *DeltaStats // non-nil when the tree was patched rather than walked
	CachedAt time.Time
	Note     string // why the cache was not used, when it was not
}

// Acquire returns the tree for root, preferring a cached one patched from the
// USN journal and falling back to a full walk whenever that is not possible.
//
// The returned tree is not rolled up: the cache stores leaf sizes, and Rollup is
// not reversible. Callers roll up after any dedupe.
func Acquire(root string, ao AcquireOptions) (*Tree, AcquireResult, error) {
	var res AcquireResult

	if !ao.UseCache {
		t, err := ScanOpts(root, ao.Scan)
		return t, res, err
	}

	path, err := CachePath(root)
	if err != nil {
		res.Note = "no cache directory"
		t, err := ScanOpts(root, ao.Scan)
		return t, res, err
	}

	// Without a journal there is no way to know what changed, so a cached tree
	// could not be trusted even if one exists.
	j, err := OpenJournal(root)
	if err != nil {
		res.Note = "journal unavailable"
		t, err := ScanOpts(root, ao.Scan)
		return t, res, err
	}
	defer j.Close()

	// Capture the head before walking, so anything that changes during the scan
	// is picked up next run instead of being missed.
	head := j.Head

	if !ao.Refresh {
		t, info, err := LoadCache(path, root)
		switch {
		case err != nil:
			res.Note = "no cache"
		case info.JournalID != j.ID:
			res.Note = "journal was reset"
		default:
			var ds DeltaStats
			next, err := t.Delta(root, j, info.NextUsn, &ds)
			if err == nil {
				res.Delta, res.CachedAt = &ds, info.ScannedAt
				if err := t.Save(path, root, j.ID, next); err != nil {
					res.Note = "cache not written: " + err.Error()
				}
				return t, res, nil
			}
			if errors.Is(err, ErrJournalWrapped) {
				res.Note = "journal wrapped"
			} else {
				res.Note = "delta failed: " + err.Error()
			}
		}
	}

	t, err := ScanOpts(root, ao.Scan)
	if err != nil {
		return nil, res, err
	}
	// A tree built with the fallback class carries no FileIDs, so the journal
	// could never be matched against it. Caching it would waste a read.
	if t.FallbackDirs() == 0 {
		if err := t.Save(path, root, j.ID, head); err != nil {
			res.Note = "cache not written: " + err.Error()
		}
	}
	return t, res, nil
}

// ---------------------------------------------------------------------------
// Delta
// ---------------------------------------------------------------------------

// DeltaStats reports what a delta scan had to do.
type DeltaStats struct {
	Records     int // journal records examined
	DirsTouched int // directories re-enumerated
	Added       int // entries appended
	Removed     int // entries tombstoned
	Compacted   bool
}

// Delta brings a cached tree up to date using the journal, re-reading only the
// directories whose contents actually changed. It returns the USN to record for
// next time.
//
// The tree must be as loaded (not rolled up). On return it is still not rolled
// up, so the caller can Save it and then Rollup.
func (t *Tree) Delta(root string, j *Journal, from int64, ds *DeltaStats) (int64, error) {
	if j.ID == 0 {
		return 0, ErrNoJournal
	}

	// Which directories had their contents change? The journal names the parent
	// of every changed file, which is exactly that set.
	changed := make(map[uint64]struct{})
	var records int
	next, err := j.Changes(from, maxPendingRecords, func(c Change) {
		records++
		changed[c.Parent] = struct{}{}
		// A directory that was created, deleted or renamed also changes its own
		// listing's validity, so re-read it too if we know it.
		if c.IsDir {
			changed[c.File] = struct{}{}
		}
	})
	if err != nil {
		return 0, err
	}
	ds.Records = records
	if len(changed) == 0 {
		return next, nil
	}

	// Map file IDs to entry indices so journal references become tree positions.
	byID := make(map[uint64]uint32, len(t.Entries))
	for i := range t.Entries {
		if id := t.Entries[i].FileID; id != 0 {
			byID[id] = uint32(i)
		}
	}

	// Keep only the changed directories that are inside this tree.
	var dirs []uint32
	for id := range changed {
		if i, ok := byID[id]; ok && t.Entries[i].Flags&FlagDir != 0 &&
			t.Entries[i].Flags&FlagDeleted == 0 {
			dirs = append(dirs, i)
		}
	}
	if len(dirs) == 0 {
		return next, nil
	}

	idx := t.BuildChildIndex()
	for _, d := range dirs {
		t.repatch(d, root, idx, ds)
	}
	ds.DirsTouched = len(dirs)

	// Compact whenever anything was removed, so no consumer of the tree ever
	// has to reason about tombstones. It is one linear rebuild.
	if ds.Removed > 0 {
		t.compact()
		ds.Compacted = true
	}
	return next, nil
}

// repatch re-reads one directory and reconciles it with what the cache holds.
// Subdirectories that still exist keep their entire cached subtree; only
// genuinely new directories are walked.
func (t *Tree) repatch(dir uint32, root string, idx *ChildIndex, ds *DeltaStats) {
	path := t.pathUnder(dir, root)

	buf := newDirBuf()
	res, complete := scanDir(longPath(path), buf, t)
	if !complete {
		t.Entries[dir].Flags |= FlagUnreadable
		return
	}

	// Index the fresh listing by file ID.
	fresh := make(map[uint64]int, len(res.items))
	for k := range res.items {
		if id := res.items[k].fileID; id != 0 {
			fresh[id] = k
		}
	}

	// Reconcile the cached children against it.
	matched := make(map[int]bool, len(res.items))
	for _, c := range idx.Children(dir) {
		e := &t.Entries[c]
		if e.Flags&FlagDeleted != 0 {
			continue
		}
		k, still := fresh[e.FileID]
		if !still || e.FileID == 0 {
			t.tombstone(c, idx, ds)
			continue
		}
		matched[k] = true

		it := res.items[k]
		// A file's size may have changed; a directory's total is recomputed by
		// Rollup, so leave directory entries holding zero.
		if it.flags&FlagDir == 0 {
			e.Size, e.Alloc = it.size, it.alloc
		}
		e.Flags = it.flags
	}

	// Whatever the fresh listing has that the cache did not is new.
	for k, it := range res.items {
		if matched[k] {
			continue
		}
		i := t.appendItem(dir, res.names, it)
		ds.Added++
		if it.flags&FlagDir != 0 && it.flags&FlagReparse == 0 {
			// A new directory needs a full walk; it has no cached subtree.
			name := windows.UTF16ToString(res.names[it.nameOff : it.nameOff+uint32(it.nameLen)])
			ds.Added += t.walkInto(i, joinPath(path, name))
		}
	}
}

// appendItem adds one enumerated item as a child of parent. Parents are always
// appended before children, so appending at the end preserves invariant 2.
func (t *Tree) appendItem(parent uint32, names []uint16, it item) uint32 {
	nameBase := uint32(len(t.names))
	t.names = append(t.names, names[it.nameOff:it.nameOff+uint32(it.nameLen)]...)
	t.Entries = append(t.Entries, Entry{
		FileID:    it.fileID,
		Size:      it.size,
		Alloc:     it.alloc,
		ParentIdx: parent,
		NameOff:   nameBase,
		NameLen:   it.nameLen,
		Flags:     it.flags,
	})
	return uint32(len(t.Entries) - 1)
}

// walkInto recursively enumerates a newly discovered directory. It is
// sequential: new subtrees are usually small, and this runs while the rest of
// the tree is already in hand.
func (t *Tree) walkInto(dir uint32, path string) int {
	buf := newDirBuf()
	type pending struct {
		idx  uint32
		path string
	}
	stack := []pending{{dir, path}}
	var added int

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		res, complete := scanDir(longPath(cur.path), buf, t)
		if !complete {
			t.Entries[cur.idx].Flags |= FlagUnreadable
		}
		for _, it := range res.items {
			i := t.appendItem(cur.idx, res.names, it)
			added++
			if it.flags&FlagDir != 0 && it.flags&FlagReparse == 0 {
				name := windows.UTF16ToString(res.names[it.nameOff : it.nameOff+uint32(it.nameLen)])
				stack = append(stack, pending{i, joinPath(cur.path, name)})
			}
		}
	}
	return added
}

// tombstone marks an entry and everything under it as gone. Removing entries
// outright would renumber the array and break every ParentIdx in it, so they
// are zeroed in place and swept up later by compact.
func (t *Tree) tombstone(i uint32, idx *ChildIndex, ds *DeltaStats) {
	stack := []uint32{i}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		e := &t.Entries[c]
		if e.Flags&FlagDeleted != 0 {
			continue
		}
		e.Flags |= FlagDeleted
		e.Size, e.Alloc = 0, 0
		ds.Removed++

		// Children appended after the index was built are not in it, but a
		// tombstoned directory cannot have any: it existed in the cache.
		if c < uint32(len(idx.starts))-1 {
			stack = append(stack, idx.Children(c)...)
		}
	}
}

func (t *Tree) tombstones() int {
	var n int
	for i := range t.Entries {
		if t.Entries[i].Flags&FlagDeleted != 0 {
			n++
		}
	}
	return n
}

// compact rebuilds the array without tombstones, remapping ParentIdx. Because
// parents precede children, a single forward pass can assign new indices and
// look up each parent's new index as it goes.
func (t *Tree) compact() {
	remap := make([]uint32, len(t.Entries))
	entries := make([]Entry, 0, len(t.Entries))
	names := make([]uint16, 0, len(t.names))

	for i := range t.Entries {
		e := t.Entries[i]
		if i > 0 && (e.Flags&FlagDeleted != 0 || t.Entries[e.ParentIdx].Flags&FlagDeleted != 0) {
			// Mark the whole line dead so descendants drop too.
			t.Entries[i].Flags |= FlagDeleted
			continue
		}
		if i > 0 {
			e.ParentIdx = remap[e.ParentIdx]
		}
		nameOff := uint32(len(names))
		names = append(names, t.names[e.NameOff:e.NameOff+uint32(e.NameLen)]...)
		e.NameOff = nameOff

		remap[i] = uint32(len(entries))
		entries = append(entries, e)
	}
	t.Entries, t.names = entries, names
}

// pathUnder rebuilds an entry's path with root substituted for the stored root
// name, so a cache written for `C:\Users\me` still resolves correctly.
func (t *Tree) pathUnder(i uint32, root string) string {
	if i == 0 {
		return root
	}
	var parts []string
	for i != 0 {
		parts = append(parts, t.Name(i))
		i = t.Entries[i].ParentIdx
	}
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(root, `\`))
	for k := len(parts) - 1; k >= 0; k-- {
		b.WriteByte('\\')
		b.WriteString(parts[k])
	}
	return b.String()
}
