//go:build windows

package scan

// ChildIndex is a precomputed adjacency list built in one pass, replacing the
// linear scan in Tree.Children. Building it costs two passes and 8 bytes per
// entry; a UI that asks for children on every keystroke cannot afford the scan.
type ChildIndex struct {
	starts []uint32 // starts[i]..starts[i+1] is the range of kids for entry i
	kids   []uint32
}

// BuildChildIndex counting-sorts every entry into its parent's bucket. Within a
// bucket children stay in entry order, which is also discovery order.
func (t *Tree) BuildChildIndex() *ChildIndex {
	n := len(t.Entries)
	ci := &ChildIndex{starts: make([]uint32, n+1)}
	if n == 0 {
		return ci
	}

	// Pass 1: count. Entry 0 is the root and is nobody's child. Tombstones from
	// a delta scan are skipped, so nothing downstream has to know they exist.
	counts := make([]uint32, n+1)
	for i := 1; i < n; i++ {
		if t.Entries[i].Flags&FlagDeleted == 0 {
			counts[t.Entries[i].ParentIdx]++
		}
	}

	// Prefix sum into starts.
	var total uint32
	for i := 0; i < n; i++ {
		ci.starts[i] = total
		total += counts[i]
	}
	ci.starts[n] = total

	// Pass 2: place. cursor walks each bucket as it fills.
	ci.kids = make([]uint32, total)
	cursor := counts[:n]
	copy(cursor, ci.starts[:n])
	for i := 1; i < n; i++ {
		if t.Entries[i].Flags&FlagDeleted != 0 {
			continue
		}
		p := t.Entries[i].ParentIdx
		ci.kids[cursor[p]] = uint32(i)
		cursor[p]++
	}
	return ci
}

// Children returns the direct children of dir. The slice aliases the index and
// must not be modified.
func (ci *ChildIndex) Children(dir uint32) []uint32 {
	return ci.kids[ci.starts[dir]:ci.starts[dir+1]]
}

// Stats summarizes a tree. Computed in one pass, before or after Rollup.
type Stats struct {
	Files      int
	Dirs       int
	Unreadable int // directories we could not open or fully enumerate
	Reparse    int // recorded but never descended into
	Deduped    int // files whose bytes were credited to an earlier hardlink
}

// Stats walks the entry array once and counts what the footer needs.
func (t *Tree) Stats() Stats {
	var s Stats
	for i := range t.Entries {
		f := t.Entries[i].Flags
		if f&FlagDeleted != 0 {
			continue // gone since the cached scan
		}
		if f&FlagDir != 0 {
			s.Dirs++
		} else {
			s.Files++
		}
		if f&FlagUnreadable != 0 {
			s.Unreadable++
		}
		if f&FlagReparse != 0 {
			s.Reparse++
		}
		if f&FlagDedup != 0 {
			s.Deduped++
		}
	}
	if s.Dirs > 0 {
		s.Dirs-- // entry 0 is the scan root, not a child directory
	}
	return s
}

// Dedupe zeroes the size of every file whose FileID has already been seen, so a
// hardlinked file is counted once rather than once per link. Without this
// WinSxS looks enormous: it is largely hardlinks into System32.
//
// Must be called before Rollup — it edits leaf sizes, and Rollup is what
// propagates them upward. Returns the number of entries zeroed.
//
// The map costs roughly 48 bytes per distinct file, so ~150MB on a 3M-file
// volume. That is why this is opt-in rather than the default.
func (t *Tree) Dedupe() int {
	seen := make(map[uint64]struct{}, len(t.Entries))
	var n int
	for i := range t.Entries {
		e := &t.Entries[i]
		if e.Flags&FlagDir != 0 {
			continue
		}
		// FileID is zero on filesystems that do not supply one (some network
		// redirectors, FAT). Zero is not a real identity, so never dedupe on it.
		if e.FileID == 0 {
			continue
		}
		if _, dup := seen[e.FileID]; dup {
			e.Size, e.Alloc = 0, 0
			e.Flags |= FlagDedup
			n++
			continue
		}
		seen[e.FileID] = struct{}{}
	}
	return n
}
