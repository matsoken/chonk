//go:build windows

package tui

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
