//go:build windows

package scan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// openTestJournal opens the journal for the volume holding dir, skipping the
// test if this machine cannot provide one.
func openTestJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Skipf("no usable USN journal: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// fullScan is the oracle: what the tree should look like if we had just walked
// it from scratch.
func fullScan(t *testing.T, root string) *Tree {
	t.Helper()
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	tr.Rollup()
	return tr
}

// listing flattens a rolled-up tree to path -> size, so two trees can be
// compared without depending on entry order.
func listing(tr *Tree) map[string]int64 {
	out := make(map[string]int64, len(tr.Entries))
	for i := range tr.Entries {
		if tr.Entries[i].Flags&FlagDeleted != 0 {
			continue
		}
		out[tr.Path(uint32(i))] = tr.Entries[i].Size
	}
	return out
}

func compareTrees(t *testing.T, got, want *Tree, what string) {
	t.Helper()
	g, w := listing(got), listing(want)

	for path, size := range w {
		gs, ok := g[path]
		if !ok {
			t.Errorf("%s: missing %s", what, path)
			continue
		}
		if gs != size {
			t.Errorf("%s: %s size = %d, want %d", what, path, gs, size)
		}
	}
	for path := range g {
		if _, ok := w[path]; !ok {
			t.Errorf("%s: unexpected %s", what, path)
		}
	}
	if got.Entries[0].Size != want.Entries[0].Size {
		t.Errorf("%s: total = %d, want %d", what, got.Entries[0].Size, want.Entries[0].Size)
	}
}

// TestDeltaMatchesFullScan is the core M5 test: apply a set of filesystem
// changes, patch a cached tree from the journal, and require the result to be
// identical to a fresh full walk.
func TestDeltaMatchesFullScan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.bin"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "doomed.bin"), make([]byte, 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "grow.bin"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}

	j := openTestJournal(t, root)
	head := j.Head

	cached, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "c.chonk")
	if err := cached.Save(cachePath, root, j.ID, head); err != nil {
		t.Fatal(err)
	}

	// Now change things in every way that matters.
	if err := os.Remove(filepath.Join(root, "doomed.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "grow.bin"), make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fresh.bin"), make([]byte, 700), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "newdir", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "deep.bin"), make([]byte, 4242), 0o644); err != nil {
		t.Fatal(err)
	}

	// Patch the cached tree from the journal.
	loaded, info, err := LoadCache(cachePath, root)
	if err != nil {
		t.Fatal(err)
	}
	j2 := openTestJournal(t, root)
	if j2.ID != info.JournalID {
		t.Skip("journal was recreated mid-test")
	}

	var ds DeltaStats
	if _, err := loaded.Delta(root, j2, info.NextUsn, &ds); err != nil {
		if errors.Is(err, ErrJournalWrapped) {
			t.Skip("journal wrapped during the test")
		}
		t.Fatal(err)
	}
	t.Logf("delta: %d records, %d dirs touched, %d added, %d removed, compacted=%v",
		ds.Records, ds.DirsTouched, ds.Added, ds.Removed, ds.Compacted)

	if ds.DirsTouched == 0 {
		t.Fatal("journal reported no changed directories in this tree")
	}
	loaded.Rollup()

	compareTrees(t, loaded, fullScan(t, root), "after delta")
}

// TestDeltaSurvivesRepeatedCycles is the drift test. An incremental cache that
// is merely almost right degrades a little on every run, and the error is
// invisible until the numbers are badly wrong. Each cycle must land exactly on
// what a full walk would have produced.
func TestDeltaSurvivesRepeatedCycles(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "c.chonk")

	j := openTestJournal(t, root)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(cachePath, root, j.ID, j.Head); err != nil {
		t.Fatal(err)
	}

	for cycle := 0; cycle < 6; cycle++ {
		// A different kind of change each time round.
		switch cycle % 3 {
		case 0:
			d := filepath.Join(root, "dir"+string(rune('a'+cycle)))
			if err := os.MkdirAll(filepath.Join(d, "inner"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "inner", "x.bin"),
				make([]byte, 100*(cycle+1)), 0o644); err != nil {
				t.Fatal(err)
			}
		case 1:
			// Rewrite an existing file at a new size.
			p := filepath.Join(root, "dir"+string(rune('a'+cycle-1)), "inner", "x.bin")
			if err := os.WriteFile(p, make([]byte, 7000+cycle), 0o644); err != nil {
				t.Fatal(err)
			}
		case 2:
			// Delete a whole subtree.
			if err := os.RemoveAll(filepath.Join(root, "dir"+string(rune('a'+cycle-2)))); err != nil {
				t.Fatal(err)
			}
		}

		loaded, info, err := LoadCache(cachePath, root)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		jj := openTestJournal(t, root)
		if jj.ID != info.JournalID {
			t.Skip("journal recreated mid-test")
		}

		var ds DeltaStats
		nextUsn, err := loaded.Delta(root, jj, info.NextUsn, &ds)
		if err != nil {
			if errors.Is(err, ErrJournalWrapped) {
				t.Skip("journal wrapped")
			}
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if err := loaded.Save(cachePath, root, jj.ID, nextUsn); err != nil {
			t.Fatal(err)
		}

		// Compare a rolled-up copy against a fresh full walk.
		check, _, err := LoadCache(cachePath, root)
		if err != nil {
			t.Fatal(err)
		}
		check.Rollup()
		compareTrees(t, check, fullScan(t, root), fmt.Sprintf("cycle %d", cycle))
		if t.Failed() {
			t.Fatalf("cycle %d diverged (%d dirs, +%d -%d)",
				cycle, ds.DirsTouched, ds.Added, ds.Removed)
		}
	}
}

// TestDeltaNoChangesIsAStableNoop checks the common case: nothing under the
// root changed, so the tree should come back untouched.
func TestDeltaNoChangesIsAStableNoop(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}

	j := openTestJournal(t, root)
	cached, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "c.chonk")
	if err := cached.Save(cachePath, root, j.ID, j.Head); err != nil {
		t.Fatal(err)
	}

	loaded, info, err := LoadCache(cachePath, root)
	if err != nil {
		t.Fatal(err)
	}
	j2 := openTestJournal(t, root)

	var ds DeltaStats
	if _, err := loaded.Delta(root, j2, info.NextUsn, &ds); err != nil {
		if errors.Is(err, ErrJournalWrapped) {
			t.Skip("journal wrapped")
		}
		t.Fatal(err)
	}
	if ds.Added != 0 || ds.Removed != 0 {
		t.Errorf("an unchanged tree was patched: +%d -%d", ds.Added, ds.Removed)
	}
	loaded.Rollup()
	compareTrees(t, loaded, fullScan(t, root), "unchanged tree")
}

// TestDeltaRejectsWrappedJournal covers the case the plan calls out: the cached
// position has aged out, so a full rescan is the only correct answer.
func TestDeltaRejectsWrappedJournal(t *testing.T) {
	root := t.TempDir()
	j := openTestJournal(t, root)

	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	// A position older than anything the journal still holds.
	var ds DeltaStats
	_, err = tr.Delta(root, j, j.Low-1, &ds)
	if !errors.Is(err, ErrJournalWrapped) {
		t.Errorf("err = %v, want ErrJournalWrapped", err)
	}

	// A position past the head is equally unusable.
	_, err = tr.Delta(root, j, j.Head+1<<20, &ds)
	if !errors.Is(err, ErrJournalWrapped) {
		t.Errorf("err = %v, want ErrJournalWrapped for a future position", err)
	}
}

func TestJournalOpensWithoutAdmin(t *testing.T) {
	j := openTestJournal(t, os.Getenv("SystemDrive")+`\`)
	if j.ID == 0 {
		t.Error("journal id is 0")
	}
	if j.Head <= 0 {
		t.Error("journal head is not positive")
	}
	if j.Low > j.Head {
		t.Errorf("oldest usn %d exceeds head %d", j.Low, j.Head)
	}
	t.Logf("journal id=%#x low=%d head=%d (elevated=%v)", j.ID, j.Low, j.Head, isElevated())
}

// TestJournalReadsRecords confirms the unprivileged read path returns records
// and that the cursor advances.
func TestJournalReadsRecords(t *testing.T) {
	root := t.TempDir()
	j := openTestJournal(t, root)
	start := j.Head

	// Generate some journal traffic of our own.
	for i := 0; i < 20; i++ {
		p := filepath.Join(root, "f"+string(rune('a'+i))+".bin")
		if err := os.WriteFile(p, make([]byte, 64), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	j2 := openTestJournal(t, root)
	var n int
	next, err := j2.Changes(start, 1_000_000, func(Change) { n++ })
	if err != nil {
		if errors.Is(err, ErrJournalWrapped) {
			t.Skip("journal wrapped")
		}
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no journal records after creating 20 files")
	}
	if next < start {
		t.Errorf("cursor went backwards: %d -> %d", start, next)
	}
	t.Logf("%d records between %d and %d", n, start, next)
}
