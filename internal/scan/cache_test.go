//go:build windows

package scan

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"testing"
)

// roundTrip saves and reloads a tree, returning the reloaded copy.
func roundTrip(t *testing.T, tr *Tree, root string, id uint64, usn int64) *Tree {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.chonk")
	if err := tr.Save(path, root, id, usn); err != nil {
		t.Fatal(err)
	}
	got, info, err := LoadCache(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if info.JournalID != id {
		t.Errorf("journal id = %#x, want %#x", info.JournalID, id)
	}
	if info.NextUsn != usn {
		t.Errorf("next usn = %d, want %d", info.NextUsn, usn)
	}
	return got
}

func TestCacheRoundTripPreservesTree(t *testing.T) {
	root := fixture(t)
	orig, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	got := roundTrip(t, orig, root, 0xabcd, 12345)

	if len(got.Entries) != len(orig.Entries) {
		t.Fatalf("reloaded %d entries, saved %d", len(got.Entries), len(orig.Entries))
	}
	for i := range orig.Entries {
		if got.Entries[i] != orig.Entries[i] {
			t.Fatalf("entry %d differs:\n saved %+v\n load  %+v",
				i, orig.Entries[i], got.Entries[i])
		}
		if a, b := got.Name(uint32(i)), orig.Name(uint32(i)); a != b {
			t.Fatalf("entry %d name = %q, want %q", i, a, b)
		}
	}

	// Sizes must survive, which means rolling up the reloaded copy matches.
	orig.Rollup()
	got.Rollup()
	if got.Entries[0].Size != orig.Entries[0].Size {
		t.Errorf("reloaded total = %d, want %d", got.Entries[0].Size, orig.Entries[0].Size)
	}
}

func TestCacheRejectsWrongRoot(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.chonk")
	if err := tr.Save(path, root, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCache(path, filepath.Dir(root)); err == nil {
		t.Error("a cache for a different root was accepted")
	}
}

func TestCacheRejectsTruncatedFile(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.chonk")
	if err := tr.Save(path, root, 1, 1); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCache(path, root); err == nil {
		t.Error("a truncated cache was accepted")
	}
}

func TestCacheNameIsStableAndDistinct(t *testing.T) {
	a := cacheName(`C:\Users\me`)
	if a != cacheName(`C:\Users\me\`) || a != cacheName(`c:\users\ME`) {
		t.Error("equivalent roots produced different cache names")
	}
	if a == cacheName(`C:\Users\you`) {
		t.Error("different roots collided")
	}
}

// TestRootFileIDIsCaptured guards the delta path: without the root's own file
// ID, a change to the top level of the scan root cannot be matched.
func TestRootFileIDIsCaptured(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Entries[0].FileID == 0 {
		t.Fatal("root entry has no FileID")
	}

	// It must be the same identifier the children report for their parent,
	// which is what the journal will hand us.
	sub := find(tr, "sub")
	if sub == 0 {
		t.Fatal("sub not found")
	}
	h, err := openDir(longPath(filepath.Join(root, "sub")))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	id, err := fileIDOf(h)
	if err != nil {
		t.Fatal(err)
	}
	if id != tr.Entries[sub].FileID {
		t.Errorf("GetFileInformationByHandle id %#x != enumerated FileId %#x",
			id, tr.Entries[sub].FileID)
	}
}

func TestCompactRemovesTombstonesAndKeepsParents(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	before := len(tr.Entries)
	idx := tr.BuildChildIndex()
	var ds DeltaStats

	// Tombstone the whole "sub" subtree.
	sub := find(tr, "sub")
	if sub == 0 {
		t.Fatal("sub not found")
	}
	tr.tombstone(sub, idx, &ds)
	if ds.Removed < 2 {
		t.Fatalf("tombstoned %d entries, want the directory and its child", ds.Removed)
	}

	tr.compact()
	if len(tr.Entries) != before-ds.Removed {
		t.Errorf("after compact %d entries, want %d", len(tr.Entries), before-ds.Removed)
	}
	if find(tr, "sub") != 0 || find(tr, "c.bin") != 0 {
		t.Error("compact left tombstoned entries behind")
	}

	// The invariant everything depends on must survive renumbering.
	for i := 1; i < len(tr.Entries); i++ {
		if uint32(i) <= tr.Entries[i].ParentIdx {
			t.Fatalf("entry %d has ParentIdx %d after compact", i, tr.Entries[i].ParentIdx)
		}
	}
	// Surviving entries keep their names, so ParentIdx was remapped correctly.
	tr.Rollup()
	want := int64(sizeA + sizeB + sizeA + sparseSize) // sub/c.bin is gone
	if got := tr.Entries[0].Size; got != want {
		t.Errorf("total after compact = %d, want %d", got, want)
	}
}
