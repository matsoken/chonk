//go:build windows

package tui

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/matsoken/chonk/internal/scan"
)

// browsing returns a model already past the scan, sitting in the browser over
// a small known tree:
//
//	root/
//	  big/    (3000 bytes in one file)
//	  mid/    (2000)
//	  small/  (1000)
//	  loose.txt (10)
func browsing(t *testing.T) *model {
	t.Helper()
	root := t.TempDir()
	for name, size := range map[string]int{"big": 3000, "mid": 2000, "small": 1000} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.bin"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}

	tr, err := scan.Scan(root, 4)
	if err != nil {
		t.Fatal(err)
	}

	m := newModel(root, scan.AcquireOptions{}, false)
	m.w, m.h = 100, 20
	// Nothing in the suite should reach the real clipboard by accident; the
	// copy tests install their own recorder over this.
	m.copyFn = func(string) error { return nil }
	m.Update(scanDone{tree: tr})
	if m.phase != phaseBrowse {
		t.Fatalf("model is in phase %v, want browse", m.phase)
	}
	return m
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func (m *model) press(s string) { m.Update(key(s)) }

// names returns the current visible rows, in order.
func (m *model) names() []string {
	out := make([]string, 0, len(m.rows))
	for _, i := range m.rows {
		out = append(out, m.tree.Name(i))
	}
	return out
}

func eq(t *testing.T, got, want []string, what string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestRowsSortedBySizeDescending(t *testing.T) {
	m := browsing(t)
	eq(t, m.names(), []string{"big", "mid", "small", "loose.txt"}, "rows")
}

func TestSortToggleCyclesAndReorders(t *testing.T) {
	m := browsing(t)

	m.press("s")
	if m.sortBy != "alloc" {
		t.Fatalf("sortBy = %q, want alloc", m.sortBy)
	}
	m.press("s")
	if m.sortBy != "name" {
		t.Fatalf("sortBy = %q, want name", m.sortBy)
	}
	eq(t, m.names(), []string{"big", "loose.txt", "mid", "small"}, "name-sorted rows")

	m.press("s")
	if m.sortBy != "size" {
		t.Fatalf("sortBy = %q, want size", m.sortBy)
	}
}

func TestDescendAndAscendRestoresCursor(t *testing.T) {
	m := browsing(t)

	m.press("down") // onto "mid"
	if got := m.cursor; got != 1 {
		t.Fatalf("cursor = %d, want 1", got)
	}
	m.press("enter")

	if m.dir == 0 {
		t.Fatal("did not descend")
	}
	eq(t, m.names(), []string{"f.bin"}, "children of mid")

	m.press("backspace")
	if m.dir != 0 {
		t.Fatal("did not return to the root")
	}
	if m.cursor != 1 {
		t.Errorf("cursor = %d after ascending, want the 1 it left from", m.cursor)
	}
}

func TestDescendIntoFileIsANoop(t *testing.T) {
	m := browsing(t)
	m.cursor = len(m.rows) - 1 // loose.txt
	m.press("enter")
	if m.dir != 0 {
		t.Error("descended into a file")
	}
}

func TestAscendAtRootIsANoop(t *testing.T) {
	m := browsing(t)
	m.press("backspace")
	if m.dir != 0 || len(m.trail) != 0 {
		t.Error("ascending from the root changed state")
	}
}

func TestFilterNarrowsRows(t *testing.T) {
	m := browsing(t)

	m.press("/")
	if !m.filtering {
		t.Fatal("filter mode did not engage")
	}
	for _, r := range "m" {
		m.press(string(r))
	}
	eq(t, m.names(), []string{"mid", "small"}, "rows matching 'm'")

	m.press("esc")
	if m.filtering || m.filter != "" {
		t.Error("esc did not clear the filter")
	}
	eq(t, m.names(), []string{"big", "mid", "small", "loose.txt"}, "rows after clearing")
}

func TestFilterKeysAreNotCommands(t *testing.T) {
	m := browsing(t)
	m.press("/")
	m.press("s") // must be text, not the sort toggle
	if m.sortBy != "size" {
		t.Errorf("typing 's' in filter mode changed sort to %q", m.sortBy)
	}
	if m.filter != "s" {
		t.Errorf("filter = %q, want %q", m.filter, "s")
	}
}

func TestCursorStaysInBounds(t *testing.T) {
	m := browsing(t)
	for i := 0; i < 50; i++ {
		m.press("up")
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d after many ups, want 0", m.cursor)
	}
	for i := 0; i < 50; i++ {
		m.press("down")
	}
	if want := len(m.rows) - 1; m.cursor != want {
		t.Errorf("cursor = %d after many downs, want %d", m.cursor, want)
	}
}

// TestViewSurvivesHostileSizes exercises the truncation and padding paths. A
// panic here would take down the whole terminal session.
func TestViewSurvivesHostileSizes(t *testing.T) {
	m := browsing(t)
	for _, size := range [][2]int{{0, 0}, {1, 1}, {3, 2}, {20, 5}, {200, 60}, {40, 6}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if out := m.View(); out == "" && size[0] > 0 {
			t.Errorf("empty view at %dx%d", size[0], size[1])
		}
		m.press("/")
		m.View()
		m.press("esc")

		// The help overlay, the stale marker and a long status message all have
		// their own width arithmetic.
		m.press("?")
		m.View()
		m.press("esc")

		m.stale = true
		m.status = strings.Repeat("a very long status message ", 8)
		m.View()
		m.stale, m.status = false, ""
	}
}

// TestHeaderAndFooterFitTheTerminal guards the two lines the renderer does not
// truncate for itself. Overflowing either one wraps it and shoves the whole
// layout up a row, and the six default hints fill 60 columns exactly — so the
// margin here is nil and worth pinning.
func TestHeaderAndFooterFitTheTerminal(t *testing.T) {
	m := browsing(t)
	for _, stale := range []bool{false, true} {
		for _, sort := range []string{"size", "alloc", "name"} {
			for _, w := range []int{20, 30, 40, 50, 55, 60, 80, 120} {
				m.stale, m.sortBy, m.w, m.h = stale, sort, w, 20
				lines := strings.Split(m.View(), "\n")

				if got := lipgloss.Width(lines[0]); got > w {
					t.Errorf("stale=%v w=%d: header is %d wide: %q",
						stale, w, got, lines[0])
				}
				foot := lines[len(lines)-1]
				if got := lipgloss.Width(foot); got > w {
					t.Errorf("sort=%s w=%d: footer is %d wide: %q",
						sort, w, got, foot)
				}
			}
		}
	}
}

// TestFooterAlwaysOffersTheKeyList is what makes dropping hints acceptable: no
// matter how narrow the terminal, the way to find every other binding stays on
// screen.
func TestFooterAlwaysOffersTheKeyList(t *testing.T) {
	m := browsing(t)
	for _, w := range []int{1, 5, 10, 20, 40, 80} {
		m.w, m.h = w, 20
		if hints := m.footerHints(); !strings.Contains(hints, "keys") {
			t.Errorf("w=%d: footer %q dropped the ? hint", w, hints)
		}
	}
}

// ---------------------------------------------------------------------------
// Handoff keys
// ---------------------------------------------------------------------------

// TestPathOfRootUsesScanRoot pins the drive-root case. The root entry's name is
// stored with its trailing separator trimmed, so Tree.Path(0) on a `C:\` scan
// yields "C:" — the current directory on that drive rather than its root.
func TestPathOfRootUsesScanRoot(t *testing.T) {
	m := browsing(t)

	if got := m.pathOf(0); got != m.root {
		t.Errorf("pathOf(0) = %q, want the scan root %q", got, m.root)
	}
	want := filepath.Join(m.root, "big")
	if got := m.pathOf(m.rows[0]); got != want {
		t.Errorf("pathOf(big) = %q, want %q", got, want)
	}

	m.root = `C:\`
	if got := m.pathOf(0); got != `C:\` {
		t.Errorf("pathOf(0) at a drive root = %q, want %q", got, `C:\`)
	}
}

// TestExplorerQuotesOnlyThePath pins the real bug: explorer.exe wants the
// quotes around the path alone. Go's argument escaping wraps the whole
// /select,<path> token instead, because it contains a space, and explorer
// answers that by silently opening Documents. Only the raw command line can
// express what it wants, so that is what has to be asserted — the argument
// list is not the layer explorer reads.
func TestExplorerQuotesOnlyThePath(t *testing.T) {
	const p = `C:\Users\krist\AppData\Roaming\Microsoft\Teams\Service Worker`

	got := explorerCmd(p, true).SysProcAttr.CmdLine
	if want := `explorer.exe /select,"` + p + `"`; got != want {
		t.Errorf("command line = %s, want %s", got, want)
	}
	if strings.HasPrefix(got, `"`) || strings.Contains(got, `"/select`) {
		t.Errorf("the switch ended up inside the quotes: %s", got)
	}
	// CreateProcess hands the whole line to the child, which discards the first
	// token as its own name. Without it, /select, would be eaten instead.
	if !strings.HasPrefix(got, "explorer.exe ") {
		t.Errorf("command line does not lead with the program name: %s", got)
	}

	// Opening a folder outright needs no such trick, and must not get one:
	// Go's escaping handles a plain path correctly, trailing backslash and all.
	open := explorerCmd(`C:\`, false)
	if open.SysProcAttr != nil {
		t.Error("the plain open path should use ordinary argument escaping")
	}
	if len(open.Args) != 2 || open.Args[1] != `C:\` {
		t.Errorf("args = %#v, want the bare path", open.Args)
	}
}

// TestExplorerAndShellTargetTheSameFolder is the consistency rule: o and ! both
// act on the folder being browsed. o additionally highlights the row you were
// on, which costs nothing because /select, opens that row's parent — the very
// folder ! would drop a shell into.
func TestExplorerAndShellTargetTheSameFolder(t *testing.T) {
	m := browsing(t)
	m.press("down") // onto "mid", a directory

	path, sel := m.explorerTarget()
	if !sel {
		t.Fatal("explorerTarget did not select the highlighted row")
	}
	if want := filepath.Join(m.root, "mid"); path != want {
		t.Errorf("explorer target = %q, want %q", path, want)
	}
	// The window that opens is the row's parent, which must be where ! goes.
	if got, want := filepath.Dir(path), m.pathOf(m.dir); got != want {
		t.Errorf("explorer opens %q but the shell would land in %q", got, want)
	}

	// One level down, the two must still agree.
	m.press("enter")
	path, _ = m.explorerTarget()
	if got, want := filepath.Dir(path), m.pathOf(m.dir); got != want {
		t.Errorf("after descending: explorer %q, shell %q", got, want)
	}
	if want := filepath.Join(m.root, "mid"); m.pathOf(m.dir) != want {
		t.Errorf("browsing %q, want %q", m.pathOf(m.dir), want)
	}
}

func TestExplorerFallsBackToTheFolderWithNoRow(t *testing.T) {
	m := browsing(t)

	m.press("/")
	for _, r := range "zzz" { // matches nothing
		m.press(string(r))
	}
	m.press("enter")

	path, sel := m.explorerTarget()
	if sel {
		t.Error("selected a row when the filter matched none")
	}
	if path != m.root {
		t.Errorf("explorer target = %q, want the browsed folder %q", path, m.root)
	}
}

func TestCopyKeysCopyRowAndDirectory(t *testing.T) {
	m := browsing(t)

	var got []string
	m.copyFn = func(s string) error { got = append(got, s); return nil }

	m.press("c") // the selected row: "big"
	m.press("C") // the directory being browsed: the root
	m.press("y") // alias for c

	want := []string{filepath.Join(m.root, "big"), m.root, filepath.Join(m.root, "big")}
	eq(t, got, want, "copied paths")
}

func TestCopyFailureIsReportedNotSwallowed(t *testing.T) {
	m := browsing(t)
	m.copyFn = func(string) error { return errors.New("clipboard is busy") }

	m.press("c")
	if !strings.Contains(m.status, "clipboard is busy") {
		t.Errorf("status = %q, want it to carry the copy error", m.status)
	}
	if !strings.Contains(m.View(), "clipboard is busy") {
		t.Error("the copy failure never reached the footer")
	}
}

// TestStatusClearsOnNextKey covers the whole expiry mechanism: there is no
// timer, so a message that outlived its keypress would be a stuck footer.
func TestStatusClearsOnNextKey(t *testing.T) {
	m := browsing(t)
	m.press("c")
	if m.status == "" {
		t.Fatal("copying set no status")
	}
	m.press("down")
	if m.status != "" {
		t.Errorf("status = %q after a later keypress, want it cleared", m.status)
	}
}

func TestCopyWithNoRowsIsANoop(t *testing.T) {
	m := browsing(t)
	m.copyFn = func(string) error { t.Error("copied with no row selected"); return nil }

	m.press("/")
	for _, r := range "zzz" { // matches nothing
		m.press(string(r))
	}
	if len(m.rows) != 0 {
		t.Fatalf("filter left %d rows, want none", len(m.rows))
	}
	m.press("enter") // leave filter entry, keeping the filter
	m.press("c")
}

func TestHelpOverlayIsModal(t *testing.T) {
	m := browsing(t)

	m.press("?")
	if !m.showHelp {
		t.Fatal("? did not open the help")
	}
	out := m.View()
	for _, want := range []string{"File Explorer", "shell", "copy", "rescan", "pgup"} {
		if !strings.Contains(out, want) {
			t.Errorf("the help view never mentions %q", want)
		}
	}

	m.press("s") // must dismiss the overlay, not cycle the sort behind it
	if m.showHelp {
		t.Error("a keypress did not dismiss the help")
	}
	if m.sortBy != "size" {
		t.Errorf("sortBy = %q, want the keypress to have been swallowed", m.sortBy)
	}
}

func TestRescanRestartsTheScan(t *testing.T) {
	m := browsing(t)
	m.press("down")
	m.press("enter") // into "mid"
	m.stale = true

	if cmd := m.onKey(key("r")); cmd == nil {
		t.Fatal("r returned no command, so no scan was started")
	}
	if m.phase != phaseScanning {
		t.Errorf("phase = %v, want scanning", m.phase)
	}
	if m.dir != 0 || m.cursor != 0 || len(m.trail) != 0 {
		t.Error("rescan did not return to the root")
	}
	if m.stale {
		t.Error("rescan left the stale marker set")
	}
	if out := m.View(); !strings.Contains(out, "scanning") {
		t.Errorf("view = %q, want the scanning view", out)
	}
}

func TestShellDoneMarksTheTreeStale(t *testing.T) {
	m := browsing(t)
	if m.stale {
		t.Fatal("the tree started out stale")
	}

	m.Update(shellDone{})
	if !m.stale {
		t.Fatal("returning from a shell did not mark the tree stale")
	}
	if !strings.Contains(m.View(), "stale") {
		t.Error("the stale marker never reached the header")
	}
}

// TestNonzeroShellExitIsNotAnError covers the common case: cmd.exe's bare
// `exit` hands back the current ERRORLEVEL, so a failed command before leaving
// the shell must not come back as a chonk error.
func TestNonzeroShellExitIsNotAnError(t *testing.T) {
	m := browsing(t)

	exit := exec.Command("cmd", "/c", "exit 1").Run()
	if exit == nil {
		t.Fatal("expected a nonzero exit to produce an error")
	}
	m.Update(shellDone{exit})

	if !m.stale {
		t.Error("the tree should still be marked stale")
	}
	if m.status != "" {
		t.Errorf("status = %q, want a nonzero exit to pass silently", m.status)
	}

	// A shell that never started is a real failure and must be reported.
	m.Update(shellDone{errors.New("file does not exist")})
	if !strings.Contains(m.status, "does not exist") {
		t.Errorf("status = %q, want the launch failure reported", m.status)
	}
}

func TestShellExePrefersChonkShell(t *testing.T) {
	t.Setenv("CHONK_SHELL", `C:\pwsh.exe`)
	if got := shellExe(); got != `C:\pwsh.exe` {
		t.Errorf("shellExe() = %q, want CHONK_SHELL to win", got)
	}

	t.Setenv("CHONK_SHELL", "")
	t.Setenv("ComSpec", `C:\Windows\System32\cmd.exe`)
	if got := shellExe(); got != `C:\Windows\System32\cmd.exe` {
		t.Errorf("shellExe() = %q, want ComSpec", got)
	}

	t.Setenv("ComSpec", "")
	if got := shellExe(); got != "cmd.exe" {
		t.Errorf("shellExe() = %q, want the cmd.exe fallback", got)
	}
}

func TestScanningViewRendersBeforeTreeArrives(t *testing.T) {
	m := newModel(os.Getenv("SystemRoot"), scan.AcquireOptions{}, false)
	m.w, m.h = 80, 24
	// live is nil at this point: the walk has not published its tree yet.
	if out := m.View(); !strings.Contains(out, "scanning") {
		t.Errorf("scanning view = %q, want it to mention scanning", out)
	}
	m.Update(tick{})
	m.View()
}

func TestScanFailureIsReported(t *testing.T) {
	m := newModel(`Z:\definitely\not\here`, scan.AcquireOptions{}, false)
	m.w, m.h = 80, 24
	m.Update(scanDone{err: os.ErrNotExist})
	if m.phase != phaseFailed {
		t.Fatalf("phase = %v, want failed", m.phase)
	}
	if out := m.View(); !strings.Contains(out, "scan failed") {
		t.Errorf("view = %q, want it to report the failure", out)
	}
}

// TestProgramQuits drives the real bubbletea runtime with scripted input and
// no TTY, which is the closest we can get to launching the app in a test.
func TestProgramQuits(t *testing.T) {
	m := newModel(t.TempDir(), scan.AcquireOptions{}, false)

	p := tea.NewProgram(m,
		tea.WithInput(strings.NewReader("q")),
		tea.WithOutput(io.Discard))

	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("program exited with %v", err)
		}
	case <-time.After(10 * time.Second):
		p.Kill()
		t.Fatal("program did not quit on 'q'")
	}
}
