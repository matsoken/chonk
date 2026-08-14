//go:build windows

package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpKeys is the full binding list. The footer only ever had room for six of
// these, which is why hjkl, g/G and pgup/pgdn went undocumented for so long.
// An empty pair is a blank separator line.
var helpKeys = [][2]string{
	{"↑ ↓ · k j", "move"},
	{"pgup pgdn", "move a page"},
	{"g · G", "first · last row"},
	{"⏎ · → · l", "open directory"},
	{"⌫ · ← · h", "go back up"},
	{"", ""},
	{"o", "show this folder in File Explorer"},
	{"!", "shell in this folder — exit returns"},
	{"c · y", "copy the selected path"},
	{"C", "copy this directory's path"},
	{"r", "rescan"},
	{"", ""},
	{"s", "cycle sort — size, on disk, name"},
	{"/", "filter · esc clears it"},
	{"?", "this list"},
	{"q", "quit"},
}

// footerHints renders the key hints, dropping the least useful ones until the
// line fits. The footer is a single row with no truncation, so an overlong one
// wraps and shoves the whole layout up — and it is easy to overflow, since the
// six default hints already fill 60 columns exactly. The pointer to the full
// list is the last thing to go: as long as `?` is visible, nothing is lost.
func (m *model) footerHints() string {
	// In display order, each with the width pressure at which it is dropped.
	items := []struct {
		keepAt int
		text   string
	}{
		{3, stKey.Render("↑↓") + " move"},
		{4, stKey.Render("⏎") + " open"},
		{5, stKey.Render("s") + " sort:" + m.sortBy},
		{6, stKey.Render("/") + " filter"},
		{1, stKey.Render("?") + " keys"},
		{2, stKey.Render("q") + " quit"},
	}

	sep := stDim.Render(" · ")
	for cutoff := 6; cutoff >= 1; cutoff-- {
		keep := make([]string, 0, len(items))
		for _, it := range items {
			if it.keepAt <= cutoff {
				keep = append(keep, it.text)
			}
		}
		line := " " + stDim.Render(strings.Join(keep, sep))
		if lipgloss.Width(line) <= m.w || cutoff == 1 {
			return line
		}
	}
	return ""
}

// helpView is a full-screen key list. chonk never deletes anything itself; o,
// ! and c are how you hand a path to something that can.
func (m *model) helpView() string {
	var b strings.Builder
	b.WriteString(" " + stHeader.Render("keys") + "\n")
	b.WriteString(stDim.Render(strings.Repeat("─", max(1, m.w))) + "\n")

	keyW := 0
	for _, kv := range helpKeys {
		if w := lipgloss.Width(kv[0]); w > keyW {
			keyW = w
		}
	}

	// Two lines are spent on the header and three on the note below, so a short
	// terminal drops the tail of the list rather than scrolling the alt screen
	// out from under itself.
	room := m.h - 5
	for n, kv := range helpKeys {
		if n >= room {
			break
		}
		if kv[0] == "" {
			b.WriteString("\n")
			continue
		}
		// Truncate the description before styling it. Cutting a rendered line
		// would slice through an ANSI escape and leave the terminal colored.
		desc := truncRight(kv[1], max(1, m.w-(1+keyW+2)))
		b.WriteString(" " + stKey.Render(padRight(kv[0], keyW)) + "  " + stDim.Render(desc) + "\n")
	}

	b.WriteString("\n " + stDim.Render(truncRight(
		"o and ! act on the folder in the header, not the highlighted row.",
		max(1, m.w-2))))
	b.WriteString("\n " + stDim.Render("any key returns"))
	return b.String()
}
