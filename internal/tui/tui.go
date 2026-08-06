//go:build windows

// Package tui is an interactive drill-down over a scanned tree. It navigates
// the same flat entry array the CLI renders, moving by index rather than by
// path, so descending a level costs a slice of the child index and nothing else.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/matsoken/chonk/internal/cli"
	"github.com/matsoken/chonk/internal/scan"
)

// Run scans root and hands control to the browser. It returns when the user
// quits.
func Run(root string, opts scan.AcquireOptions, dedupe bool) error {
	m := newModel(root, opts, dedupe)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// barW is the width of the proportional bar column.
const barW = 14

type phase int

const (
	phaseScanning phase = iota
	phaseBrowse
	phaseFailed
)

// crumb remembers where the cursor was in a directory we descended out of, so
// going back up lands where you left rather than at the top.
type crumb struct {
	dir    uint32
	cursor int
	offset int
}

type model struct {
	root   string
	opts   scan.AcquireOptions
	dedupe bool

	phase phase
	err   error

	tree  *scan.Tree
	idx   *scan.ChildIndex
	stats scan.Stats

	// Populated before the walk begins so progress can be polled.
	live    *scan.Tree
	started time.Time
	elapsed time.Duration
	spin    int

	total, used uint64

	dir    uint32
	rows   []uint32
	cursor int
	offset int
	trail  []crumb

	sortBy    string // "size", "alloc", "name"
	filter    string
	filtering bool

	w, h int
}

func newModel(root string, opts scan.AcquireOptions, dedupe bool) *model {
	total, used, _ := scan.DiskUsage(root)
	return &model{
		root:    root,
		opts:    opts,
		dedupe:  dedupe,
		phase:   phaseScanning,
		started: time.Now(),
		total:   total,
		used:    used,
		sortBy:  "size",
		w:       80,
		h:       24,
	}
}

// Messages.
type (
	scanDone struct {
		tree *scan.Tree
		res  scan.AcquireResult
		err  error
	}
	scanLive struct{ tree *scan.Tree }
	tick     struct{}
)

func (m *model) Init() tea.Cmd {
	handle := make(chan *scan.Tree, 1)
	o := m.opts
	o.Scan.Handle = handle

	return tea.Batch(
		// The walk, or a cache hit. Its result arrives as a single message.
		func() tea.Msg {
			t, res, err := scan.Acquire(m.root, o)
			return scanDone{t, res, err}
		},
		// The tree pointer, published as soon as the walk starts, so the
		// progress line has something to read.
		func() tea.Msg { return scanLive{<-handle} },
		tickCmd(),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tick{} })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clampScroll()
		return m, nil

	case scanLive:
		m.live = msg.tree
		return m, nil

	case tick:
		if m.phase == phaseScanning {
			m.spin++
			return m, tickCmd()
		}
		return m, nil

	case scanDone:
		m.elapsed = time.Since(m.started)
		if msg.err != nil {
			m.phase, m.err = phaseFailed, msg.err
			return m, nil
		}
		m.tree = msg.tree
		if m.dedupe {
			m.tree.Dedupe() // before Rollup: it edits leaf sizes
		}
		m.tree.Rollup()
		m.stats = m.tree.Stats()
		m.idx = m.tree.BuildChildIndex()
		m.phase = phaseBrowse
		m.rebuild()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	// While typing a filter, most keys are text.
	if m.filtering {
		switch k.String() {
		case "esc":
			m.filtering, m.filter = false, ""
			m.rebuild()
		case "enter":
			m.filtering = false
		case "backspace":
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
				m.rebuild()
			}
		case "ctrl+c":
			return tea.Quit
		default:
			if len(k.Runes) > 0 {
				m.filter += string(k.Runes)
				m.rebuild()
			}
		}
		return nil
	}

	switch k.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.rebuild()
		}
	}

	if m.phase != phaseBrowse {
		return nil
	}

	switch k.String() {
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-m.viewport())
	case "pgdown":
		m.move(m.viewport())
	case "home", "g":
		m.cursor = 0
		m.clampScroll()
	case "end", "G":
		m.cursor = len(m.rows) - 1
		m.clampScroll()
	case "enter", "right", "l":
		m.descend()
	case "backspace", "left", "h":
		m.ascend()
	case "s":
		switch m.sortBy {
		case "size":
			m.sortBy = "alloc"
		case "alloc":
			m.sortBy = "name"
		default:
			m.sortBy = "size"
		}
		m.rebuild()
	case "/":
		m.filtering = true
	}
	return nil
}

// ---------------------------------------------------------------------------
// Navigation
// ---------------------------------------------------------------------------

func (m *model) viewport() int {
	// header (3 lines) + footer (2 lines)
	if n := m.h - 5; n > 0 {
		return n
	}
	return 1
}

func (m *model) move(d int) {
	m.cursor += d
	m.clampScroll()
}

func (m *model) clampScroll() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	vp := m.viewport()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+vp {
		m.offset = m.cursor - vp + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *model) descend() {
	if m.cursor >= len(m.rows) {
		return
	}
	i := m.rows[m.cursor]
	e := &m.tree.Entries[i]
	// Files have no children, and reparse points were never followed, so both
	// would open an empty pane.
	if e.Flags&scan.FlagDir == 0 || len(m.idx.Children(i)) == 0 {
		return
	}
	m.trail = append(m.trail, crumb{m.dir, m.cursor, m.offset})
	m.dir, m.cursor, m.offset = i, 0, 0
	m.rebuild()
}

func (m *model) ascend() {
	if len(m.trail) == 0 {
		return
	}
	c := m.trail[len(m.trail)-1]
	m.trail = m.trail[:len(m.trail)-1]
	m.dir, m.cursor, m.offset = c.dir, c.cursor, c.offset
	m.rebuild()
	m.clampScroll()
}

func (m *model) key(i uint32) int64 {
	if m.sortBy == "alloc" {
		return m.tree.Entries[i].Alloc
	}
	return m.tree.Entries[i].Size
}

// rebuild recomputes the visible row set for the current directory, sort and
// filter. Sorting a copy keeps the shared child index untouched.
func (m *model) rebuild() {
	kids := m.idx.Children(m.dir)
	m.rows = make([]uint32, 0, len(kids))

	needle := strings.ToLower(m.filter)
	for _, i := range kids {
		if needle != "" && !strings.Contains(strings.ToLower(m.tree.Name(i)), needle) {
			continue
		}
		m.rows = append(m.rows, i)
	}

	if m.sortBy == "name" {
		names := make(map[uint32]string, len(m.rows))
		for _, i := range m.rows {
			names[i] = strings.ToLower(m.tree.Name(i))
		}
		sort.Slice(m.rows, func(a, b int) bool { return names[m.rows[a]] < names[m.rows[b]] })
	} else {
		sort.Slice(m.rows, func(a, b int) bool {
			ka, kb := m.key(m.rows[a]), m.key(m.rows[b])
			if ka != kb {
				return ka > kb
			}
			return m.rows[a] < m.rows[b]
		})
	}
	m.clampScroll()
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

var (
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("80"))
	stDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	stDir    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	stFile   = lipgloss.NewStyle()
	stLink   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	stSel    = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236"))
	stSize   = lipgloss.NewStyle().Bold(true)
	stWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	stKey    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("215"))
)

func heat(frac float64) lipgloss.Style {
	switch {
	case frac >= 0.30:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	case frac >= 0.10:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
	case frac >= 0.03:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	}
}

func (m *model) View() string {
	switch m.phase {
	case phaseFailed:
		return fmt.Sprintf("\n  %s\n\n  %s\n",
			stWarn.Render("scan failed"), m.err.Error())
	case phaseScanning:
		return m.scanView()
	default:
		return m.browseView()
	}
}

func (m *model) scanView() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	var n int64
	if m.live != nil {
		n = m.live.Progress()
	}
	return fmt.Sprintf("\n  %s  scanning %s\n\n  %s entries in %.1fs\n\n  %s\n",
		stHeader.Render(frames[m.spin%len(frames)]),
		stHeader.Render(m.root),
		stSize.Render(cli.Comma(int(n))),
		time.Since(m.started).Seconds(),
		stDim.Render("q to cancel"))
}

func (m *model) browseView() string {
	var b strings.Builder

	// Header: where we are, and what it holds.
	path := m.root
	if m.dir != 0 {
		path = m.tree.Path(m.dir)
	}
	b.WriteString(" " + stHeader.Render(truncLeft(path, m.w-2)) + "\n")

	label := "logical"
	if m.sortBy == "alloc" {
		label = "on disk"
	}
	b.WriteString(fmt.Sprintf(" %s %s · %s of %s used · %s %s\n",
		stSize.Render(cli.Human(m.key(m.dir))),
		stDim.Render(label),
		cli.Human(int64(m.used)), cli.Human(int64(m.total)),
		stSize.Render(cli.Comma(len(m.rows))),
		stDim.Render("entries here")))
	b.WriteString(stDim.Render(strings.Repeat("─", max(1, m.w))) + "\n")

	// Rows.
	vp := m.viewport()
	total := m.key(m.dir)
	var maxKey int64
	for _, i := range m.rows {
		if k := m.key(i); k > maxKey {
			maxKey = k
		}
	}

	end := min(m.offset+vp, len(m.rows))
	for n := m.offset; n < end; n++ {
		b.WriteString(m.rowLine(m.rows[n], n == m.cursor, total, maxKey) + "\n")
	}
	for n := end - m.offset; n < vp; n++ {
		b.WriteString("\n")
	}

	// Footer: filter prompt or key hints.
	b.WriteString(stDim.Render(strings.Repeat("─", max(1, m.w))) + "\n")
	if m.filtering || m.filter != "" {
		cur := ""
		if m.filtering {
			cur = "█"
		}
		b.WriteString(fmt.Sprintf(" %s %s%s   %s",
			stKey.Render("filter:"), m.filter, cur,
			stDim.Render("esc clears · enter accepts")))
	} else {
		b.WriteString(" " + stDim.Render(strings.Join([]string{
			stKey.Render("↑↓") + " move",
			stKey.Render("⏎") + " open",
			stKey.Render("⌫") + " up",
			stKey.Render("s") + " sort:" + m.sortBy,
			stKey.Render("/") + " filter",
			stKey.Render("q") + " quit",
		}, stDim.Render(" · "))))
	}
	return b.String()
}

func (m *model) rowLine(i uint32, selected bool, total, maxKey int64) string {
	e := &m.tree.Entries[i]
	k := m.key(i)

	var frac, share float64
	if maxKey > 0 {
		frac = float64(k) / float64(maxKey)
	}
	if total > 0 {
		share = float64(k) / float64(total)
	}

	filled := int(frac*barW + 0.5)
	if filled == 0 && frac > 0 {
		filled = 1
	}
	bar := heat(share).Render(strings.Repeat("█", filled)) +
		stDim.Render(strings.Repeat("░", barW-filled))

	name := m.tree.Name(i)
	nameStyle := stFile
	switch {
	case e.Flags&scan.FlagReparse != 0:
		name += "\\ →"
		nameStyle = stLink
	case e.Flags&scan.FlagDir != 0:
		name += "\\"
		nameStyle = stDir
	}
	if e.Flags&scan.FlagUnreadable != 0 {
		name += " " + stWarn.Render("!")
	}

	// The size, percentage and bar columns are fixed width; the name takes
	// whatever is left. Leading space + 8 + space + 6 + space + bar + 2 spaces.
	avail := max(8, m.w-(1+8+1+6+1+barW+2))

	line := fmt.Sprintf(" %s %s %s  %s",
		stSize.Render(padLeft(cli.Human(k), 8)),
		stDim.Render(padLeft(fmt.Sprintf("%.1f%%", share*100), 6)),
		bar,
		nameStyle.Render(truncRight(name, avail)))

	if selected {
		return stSel.Render(padRight(line, m.w))
	}
	return line
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func padLeft(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncRight keeps the head of a string, which is what identifies a file name.
func truncRight(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// truncLeft keeps the tail, which is the informative end of a path.
func truncLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w <= 1 {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}
