//go:build windows

package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	sizeA      = 1000
	sizeB      = 2000
	sizeC      = 3000
	sparseSize = 1 << 20 // logical; allocates nothing
)

// fixture builds a tree with known sizes plus the three things that make disk
// accounting wrong when mishandled: a hardlink pair, a junction, and a sparse
// file.
//
//	root/
//	  a.bin           1000 bytes
//	  b.bin           2000 bytes
//	  hard.bin        hardlink to a.bin (same FileID, same 1000 bytes)
//	  sparse.bin      1 MiB logical, ~0 on disk
//	  sub/
//	    c.bin         3000 bytes
//	  junc/           junction to sub/ — must not be followed
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write(t, filepath.Join(root, "a.bin"), sizeA)
	write(t, filepath.Join(root, "b.bin"), sizeB)

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "c.bin"), sizeC)

	mklink(t, "/H", filepath.Join(root, "hard.bin"), filepath.Join(root, "a.bin"))
	mklink(t, "/J", filepath.Join(root, "junc"), sub)
	sparse(t, filepath.Join(root, "sparse.bin"), sparseSize)

	return root
}

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mklink shells out because junctions and hardlinks both work without admin
// this way, whereas CreateSymbolicLink needs a privilege we may not have.
func mklink(t *testing.T, flag, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", flag, link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create %s link (%v): %s", flag, err, out)
	}
}

// sparse marks a file sparse and extends it without writing, so EndOfFile is
// large while AllocationSize stays near zero.
func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const fsctlSetSparse = 0x000900C4
	var ret uint32
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse,
		nil, 0, nil, 0, &ret, nil); err != nil {
		t.Skipf("cannot mark file sparse: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// find returns the index of the entry with the given name, or 0 if absent.
func find(t *Tree, name string) uint32 {
	for i := range t.Entries {
		if t.Name(uint32(i)) == name {
			return uint32(i)
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestScanExactTotals(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	tr.Rollup()

	// Every byte counted once, except the hardlink which is counted twice
	// without --dedupe, and the junction which is never followed.
	want := int64(sizeA + sizeB + sizeC + sizeA + sparseSize)
	if got := tr.Entries[0].Size; got != want {
		t.Errorf("logical total = %d, want %d", got, want)
	}

	st := tr.Stats()
	// a, b, hard, sparse, sub/c = 5 files. junc is a directory entry.
	if st.Files != 5 {
		t.Errorf("files = %d, want 5", st.Files)
	}
	if st.Dirs != 2 { // sub and junc
		t.Errorf("dirs = %d, want 2", st.Dirs)
	}
	if st.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0", st.Unreadable)
	}
}

func TestJunctionNotFollowed(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	junc := find(tr, "junc")
	if junc == 0 {
		t.Fatal("junction entry not found")
	}
	if tr.Entries[junc].Flags&FlagReparse == 0 {
		t.Error("junction is not flagged as a reparse point")
	}

	// The junction points at sub/, so following it would produce a second copy
	// of c.bin and inflate the total.
	var cCount int
	for i := range tr.Entries {
		if tr.Name(uint32(i)) == "c.bin" {
			cCount++
		}
	}
	if cCount != 1 {
		t.Errorf("c.bin appears %d times, want 1: the junction was followed", cCount)
	}
	if kids := tr.BuildChildIndex().Children(junc); len(kids) != 0 {
		t.Errorf("junction has %d children, want 0", len(kids))
	}
}

func TestSparseFileLogicalVsAllocated(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	i := find(tr, "sparse.bin")
	if i == 0 {
		t.Fatal("sparse.bin not found")
	}
	e := tr.Entries[i]
	if e.Flags&FlagSparse == 0 {
		t.Error("sparse.bin is not flagged sparse")
	}
	if e.Size != sparseSize {
		t.Errorf("logical size = %d, want %d", e.Size, sparseSize)
	}
	// The whole point: a sparse file occupies far less than it reports.
	if e.Alloc >= e.Size {
		t.Errorf("allocated %d >= logical %d; sparseness not reflected", e.Alloc, e.Size)
	}
}

func TestDedupeCountsHardlinkOnce(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	a, hard := find(tr, "a.bin"), find(tr, "hard.bin")
	if a == 0 || hard == 0 {
		t.Fatal("hardlink pair not found")
	}
	if tr.Entries[a].FileID != tr.Entries[hard].FileID {
		t.Fatalf("hardlink pair has different FileIDs (%d vs %d)",
			tr.Entries[a].FileID, tr.Entries[hard].FileID)
	}

	n := tr.Dedupe()
	if n != 1 {
		t.Errorf("deduped %d entries, want 1", n)
	}
	tr.Rollup()

	want := int64(sizeA + sizeB + sizeC + sparseSize) // one copy of a.bin
	if got := tr.Entries[0].Size; got != want {
		t.Errorf("deduped total = %d, want %d", got, want)
	}
}

// TestParentIdxInvariant checks the property the whole design rests on: a
// parent is always committed before its children, so Rollup can be a single
// reverse loop rather than a graph traversal.
func TestParentIdxInvariant(t *testing.T) {
	tr, err := Scan(os.Getenv("SystemRoot")+`\System32\drivers`, 0)
	if err != nil {
		t.Skipf("cannot scan drivers directory: %v", err)
	}
	if len(tr.Entries) < 2 {
		t.Skip("nothing enumerated")
	}
	for i := 1; i < len(tr.Entries); i++ {
		if p := tr.Entries[i].ParentIdx; uint32(i) <= p {
			t.Fatalf("entry %d has ParentIdx %d: parent must precede child", i, p)
		}
	}
}

// TestChildIndexMatchesLinearScan pins the fast adjacency list to the obvious
// slow implementation it replaces.
func TestChildIndexMatchesLinearScan(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	idx := tr.BuildChildIndex()

	for i := range tr.Entries {
		want := tr.Children(uint32(i))
		got := idx.Children(uint32(i))
		if len(want) != len(got) {
			t.Fatalf("entry %d: index has %d children, linear scan found %d",
				i, len(got), len(want))
		}
		for k := range want {
			if want[k] != got[k] {
				t.Fatalf("entry %d child %d: index %d, linear scan %d",
					i, k, got[k], want[k])
			}
		}
	}
}

// TestRollupMatchesRecursiveSum verifies the reverse-loop rollup against a
// straightforward recursive sum over the same tree.
func TestRollupMatchesRecursiveSum(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	idx := tr.BuildChildIndex()

	leaf := make([]int64, len(tr.Entries))
	for i := range tr.Entries {
		leaf[i] = tr.Entries[i].Size
	}
	var sum func(uint32) int64
	sum = func(i uint32) int64 {
		total := leaf[i]
		for _, c := range idx.Children(i) {
			total += sum(c)
		}
		return total
	}
	want := sum(0)

	tr.Rollup()
	if got := tr.Entries[0].Size; got != want {
		t.Errorf("Rollup total = %d, recursive sum = %d", got, want)
	}
}

func TestPathReconstruction(t *testing.T) {
	root := fixture(t)
	tr, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	c := find(tr, "c.bin")
	if c == 0 {
		t.Fatal("c.bin not found")
	}
	want := filepath.Join(root, "sub", "c.bin")
	got := tr.Path(c)
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	// The \\?\ prefix is a syscall detail and must not reach the user.
	if len(got) > 4 && got[:4] == `\\?\` {
		t.Errorf("Path leaked the long-path prefix: %q", got)
	}
}

// TestDeterministicAcrossWorkerCounts runs the same tree at several
// concurrency levels. Totals and entry counts must not depend on how the work
// happened to be scheduled. The race detector needs a C toolchain that is not
// always present, so this stands in as the check on the parallel commit path.
func TestDeterministicAcrossWorkerCounts(t *testing.T) {
	root := fixture(t)

	var wantSize, wantAlloc int64
	var wantEntries int
	for _, workers := range []int{1, 2, 3, 8, 32} {
		for rep := 0; rep < 5; rep++ {
			tr, err := Scan(root, workers)
			if err != nil {
				t.Fatal(err)
			}
			tr.Rollup()

			// Invariant 2 must hold no matter the interleaving.
			for i := 1; i < len(tr.Entries); i++ {
				if uint32(i) <= tr.Entries[i].ParentIdx {
					t.Fatalf("workers=%d: entry %d has ParentIdx %d",
						workers, i, tr.Entries[i].ParentIdx)
				}
			}

			if wantEntries == 0 {
				wantSize, wantAlloc = tr.Entries[0].Size, tr.Entries[0].Alloc
				wantEntries = len(tr.Entries)
				continue
			}
			if got := tr.Entries[0].Size; got != wantSize {
				t.Errorf("workers=%d: size = %d, want %d", workers, got, wantSize)
			}
			if got := tr.Entries[0].Alloc; got != wantAlloc {
				t.Errorf("workers=%d: alloc = %d, want %d", workers, got, wantAlloc)
			}
			if got := len(tr.Entries); got != wantEntries {
				t.Errorf("workers=%d: %d entries, want %d", workers, got, wantEntries)
			}
		}
	}
}

// TestFallbackClassAgreesWithPrimary reads the same directory through both
// enumeration classes and compares them field by field. The two structs have
// different layouts, so a wrong offset in the fallback would otherwise show up
// only on the network shares that need it — where it would be least noticed.
func TestFallbackClassAgreesWithPrimary(t *testing.T) {
	dir := longPath(os.Getenv("SystemRoot") + `\System32\drivers`)

	read := func(c dirClass) dirResult {
		t.Helper()
		h, err := openDir(dir)
		if err != nil {
			t.Skipf("cannot open %s: %v", dir, err)
		}
		defer windows.CloseHandle(h)

		buf := newDirBuf()
		r, complete, unsupported := enumerate(h, buf, c)
		if unsupported {
			t.Skipf("%s not supported here", c.name)
		}
		if !complete {
			t.Fatalf("%s enumeration did not complete", c.name)
		}
		return r
	}

	primary, alt := read(classIDBoth), read(classFull)

	if len(primary.items) != len(alt.items) {
		t.Fatalf("%s returned %d entries, %s returned %d",
			classIDBoth.name, len(primary.items), classFull.name, len(alt.items))
	}
	if len(primary.items) == 0 {
		t.Skip("directory is empty")
	}

	for i := range primary.items {
		p, a := primary.items[i], alt.items[i]
		pn := string(windows.UTF16ToString(primary.names[p.nameOff : p.nameOff+uint32(p.nameLen)]))
		an := string(windows.UTF16ToString(alt.names[a.nameOff : a.nameOff+uint32(a.nameLen)]))
		if pn != an {
			t.Fatalf("entry %d: name %q vs %q", i, pn, an)
		}
		if p.size != a.size {
			t.Errorf("%s: size %d vs %d", pn, p.size, a.size)
		}
		if p.alloc != a.alloc {
			t.Errorf("%s: alloc %d vs %d", pn, p.alloc, a.alloc)
		}
		if p.flags != a.flags {
			t.Errorf("%s: flags %#x vs %#x", pn, p.flags, a.flags)
		}
	}

	// The distinguishing feature: only the primary class carries a FileId.
	if primary.items[0].fileID == 0 {
		t.Error("primary class returned no FileId")
	}
	if alt.items[0].fileID != 0 {
		t.Error("fallback class returned a FileId; it has no such field")
	}
}

// TestForceFallbackWholeTree checks that a full scan down the fallback path
// produces the same sizes as the normal one.
func TestForceFallbackWholeTree(t *testing.T) {
	root := fixture(t)

	normal, err := Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	normal.Rollup()

	alt, err := ScanOpts(root, Options{Workers: 4, ForceFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	alt.Rollup()

	if normal.Entries[0].Size != alt.Entries[0].Size {
		t.Errorf("logical total: primary %d, fallback %d",
			normal.Entries[0].Size, alt.Entries[0].Size)
	}
	if normal.Entries[0].Alloc != alt.Entries[0].Alloc {
		t.Errorf("allocated total: primary %d, fallback %d",
			normal.Entries[0].Alloc, alt.Entries[0].Alloc)
	}
	if len(normal.Entries) != len(alt.Entries) {
		t.Errorf("entry count: primary %d, fallback %d",
			len(normal.Entries), len(alt.Entries))
	}
	if alt.FallbackDirs() == 0 {
		t.Error("FallbackDirs is 0 despite ForceFallback")
	}

	// No FileIds means dedupe cannot work, and must not silently pretend to.
	if n := alt.Dedupe(); n != 0 {
		t.Errorf("fallback scan deduped %d entries; it has no FileIds", n)
	}
}

func TestDiskUsage(t *testing.T) {
	total, used, err := DiskUsage(os.Getenv("SystemDrive") + `\`)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Error("total capacity is 0")
	}
	if used > total {
		t.Errorf("used %d exceeds total %d", used, total)
	}
}

// TestDirBufIsAligned guards a bug that cost real debugging time:
// GetFileInformationByHandleEx rejects a misaligned output buffer with
// ERROR_NOACCESS, and Go hands out []byte allocations that are not 8-byte
// aligned. Every enumeration buffer must come from newDirBuf.
func TestDirBufIsAligned(t *testing.T) {
	for i := 0; i < 64; i++ {
		buf := newDirBuf()
		if len(buf) != bufSize {
			t.Fatalf("buffer is %d bytes, want %d", len(buf), bufSize)
		}
		if addr := uintptr(unsafe.Pointer(&buf[0])); addr%8 != 0 {
			t.Fatalf("buffer at %#x is %d bytes off an 8-byte boundary", addr, addr%8)
		}
	}
}

// TestEnumerateRejectsMisalignedBuffer documents *why* newDirBuf exists, by
// showing the failure it prevents.
func TestEnumerateRejectsMisalignedBuffer(t *testing.T) {
	dir := longPath(os.Getenv("SystemRoot") + `\System32\drivers`)
	h, err := openDir(dir)
	if err != nil {
		t.Skipf("cannot open %s: %v", dir, err)
	}
	defer windows.CloseHandle(h)

	// Deliberately offset a buffer by 4 bytes to reproduce the misalignment.
	backing := newDirBuf()
	skewed := backing[4:]
	err = windows.GetFileInformationByHandleEx(h,
		classFileIDBothDirectoryRestartInfo, &skewed[0], uint32(len(skewed)))
	if err == nil {
		t.Skip("this system tolerates a misaligned buffer")
	}
	t.Logf("misaligned buffer correctly rejected: %v", err)

	// The aligned one must still work on the same handle.
	if err := windows.GetFileInformationByHandleEx(h,
		classFileIDBothDirectoryRestartInfo, &backing[0], uint32(len(backing))); err != nil {
		t.Errorf("aligned buffer failed: %v", err)
	}
}
