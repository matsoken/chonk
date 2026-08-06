//go:build windows

// Package cli renders a scanned tree as a terminal report: a headline the scan
// is not required for, then a proportional breakdown of what is underneath.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matsoken/chonk/internal/scan"
)

// Options controls what Render prints.
type Options struct {
	Sort  string // "size", "alloc" or "name"
	Depth int    // levels of directory nesting to print
	Top   int    // rows per level, 0 for all
	Color bool
	Alloc bool // rank and display allocated size instead of logical
}

const (
	barWidth  = 22
	sizeWidth = 8
)

// ---------------------------------------------------------------------------
// Sizes and numbers
// ---------------------------------------------------------------------------

var units = [...]string{"B", "K", "M", "G", "T", "P"}

// Human formats a byte count in binary units, the way du -h does: 1K is 1024
// bytes. Output is at most 6 characters so columns stay aligned.
func Human(n int64) string {
	if n < 0 {
		return "-" + Human(-n)
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64) + units[i]
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64) + units[i]
	default:
		return strconv.FormatFloat(v, 'f', 2, 64) + units[i]
	}
}

// Comma groups digits for the footer counts, which are large enough that raw
// digit strings are hard to read at a glance.
func Comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ago renders a duration the way a footer wants it: coarse and short.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// Color
// ---------------------------------------------------------------------------

type style string

const (
	reset  style = "\x1b[0m"
	bold   style = "\x1b[1m"
	dim    style = "\x1b[2m"
	red    style = "\x1b[38;5;203m"
	orange style = "\x1b[38;5;215m"
	green  style = "\x1b[38;5;114m"
	blue   style = "\x1b[38;5;75m"
	cyan   style = "\x1b[38;5;80m"
	gray   style = "\x1b[38;5;244m"
)

type renderer struct {
	w     io.Writer
	t     *scan.Tree
	idx   *scan.ChildIndex
	opt   Options
	color bool
}

// paint wraps s in the given styles, or returns it unchanged when color is off.
func (r *renderer) paint(s string, sty ...style) string {
	if !r.color || len(sty) == 0 {
		return s
	}
	var b strings.Builder
	for _, x := range sty {
		b.WriteString(string(x))
	}
	b.WriteString(s)
	b.WriteString(string(reset))
	return b.String()
}

// heatOf picks a bar color by share of the parent. Big things should be
// immediately findable without reading any numbers.
func heatOf(frac float64) style {
	switch {
	case frac >= 0.30:
		return red
	case frac >= 0.10:
		return orange
	case frac >= 0.03:
		return green
	default:
		return blue
	}
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

// Report is everything Render needs that is not the tree itself.
type Report struct {
	Root     string
	Total    uint64 // volume capacity
	Used     uint64 // volume bytes in use
	DiskErr  error  // volume query failed; headline is omitted
	Stats    scan.Stats
	Elapsed  time.Duration
	Deduped  int
	Fallback int // directories enumerated via the M3 fallback class

	// Delta is set when the tree came from the cache rather than a full walk.
	Delta    *scan.DeltaStats
	CachedAt time.Time
	// CacheNote explains why the cache was not used, when it was not.
	CacheNote string
}

// Render writes the full report: headline, breakdown, footer.
func Render(w io.Writer, t *scan.Tree, idx *scan.ChildIndex, rep Report, opt Options) {
	if opt.Depth < 1 {
		opt.Depth = 1
	}
	r := &renderer{w: w, t: t, idx: idx, opt: opt, color: opt.Color}

	r.headline(rep)
	fmt.Fprintln(w)
	r.rows(0, 1, r.key(0))
	fmt.Fprintln(w)
	r.footer(rep)
}

func (r *renderer) headline(rep Report) {
	w := r.w
	fmt.Fprintf(w, "%s\n", r.paint(rep.Root, bold, cyan))

	if rep.DiskErr == nil && rep.Total > 0 {
		free := rep.Total - rep.Used
		pct := float64(rep.Used) / float64(rep.Total)
		fmt.Fprintf(w, "  %s used of %s  %s   %s free\n",
			r.paint(Human(int64(rep.Used)), bold),
			Human(int64(rep.Total)),
			r.paint(fmt.Sprintf("(%.1f%%)", pct*100), heatOf(pct)),
			Human(int64(free)))
		fmt.Fprintf(w, "  %s\n", r.bar(pct, 52, heatOf(pct)))
	} else if rep.DiskErr != nil {
		fmt.Fprintf(w, "  %s\n", r.paint("volume info unavailable: "+rep.DiskErr.Error(), dim))
	}

	// Both totals, always. Logical is EndOfFile and allocated is AllocationSize;
	// they diverge sharply on compressed and sparse files, and "why doesn't this
	// match Explorer" is nearly always one of the two. The active sort key is
	// the emphasized one.
	root := &r.t.Entries[0]
	logical, disk := dim, dim
	if r.opt.Alloc {
		disk = bold
	} else {
		logical = bold
	}

	// The scanned total is not the volume's used bytes: it excludes everything
	// outside the scan root, plus filesystem metadata such as $MFT.
	fmt.Fprintf(w, "  %s %s   %s %s   in %s %s, %s %s\n",
		r.paint(Human(root.Size), logical), r.paint("logical", dim),
		r.paint(Human(root.Alloc), disk), r.paint("on disk", dim),
		r.paint(Comma(rep.Stats.Files), bold),
		plural(rep.Stats.Files, "file", "files"),
		r.paint(Comma(rep.Stats.Dirs), bold),
		plural(rep.Stats.Dirs, "directory", "directories"))
}

// key is the sort and display value for an entry under the current options.
func (r *renderer) key(i uint32) int64 {
	if r.opt.Alloc {
		return r.t.Entries[i].Alloc
	}
	return r.t.Entries[i].Size
}

func (r *renderer) bar(frac float64, width int, sty style) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	// Anything with a nonzero share gets at least one cell, or a long tail of
	// small-but-real directories renders as an empty row.
	if filled == 0 && frac > 0 {
		filled = 1
	}
	return r.paint(strings.Repeat("█", filled), sty) +
		r.paint(strings.Repeat("░", width-filled), dim)
}

// rows prints one directory's children, then recurses into them while there is
// depth budget left. parentTotal is what percentages are taken against.
func (r *renderer) rows(dir uint32, level int, parentTotal int64) {
	kids := r.idx.Children(dir)
	if len(kids) == 0 {
		return
	}

	sorted := make([]uint32, len(kids))
	copy(sorted, kids)
	r.sortEntries(sorted)

	shown := sorted
	var hidden []uint32
	if r.opt.Top > 0 && len(sorted) > r.opt.Top {
		shown, hidden = sorted[:r.opt.Top], sorted[r.opt.Top:]
	}

	// Bars are scaled to the largest sibling, not to the parent total. Scaling
	// to the parent makes every row invisible whenever one child dominates.
	var maxKey int64
	for _, i := range shown {
		if k := r.key(i); k > maxKey {
			maxKey = k
		}
	}

	for _, i := range shown {
		r.row(i, level, parentTotal, maxKey)
		e := &r.t.Entries[i]
		if level < r.opt.Depth && e.Flags&scan.FlagDir != 0 && e.Flags&scan.FlagReparse == 0 {
			r.rows(i, level+1, r.key(i))
		}
	}

	if len(hidden) > 0 {
		var sum int64
		for _, i := range hidden {
			sum += r.key(i)
		}
		indent := strings.Repeat("  ", level-1)
		fmt.Fprintf(r.w, "%*s  %*s  %s%s\n",
			sizeWidth, Human(sum), barWidth+7, "",
			indent,
			r.paint(fmt.Sprintf("(%s more)", Comma(len(hidden))), dim))
	}
}

func (r *renderer) row(i uint32, level int, parentTotal, maxKey int64) {
	e := &r.t.Entries[i]
	k := r.key(i)

	var frac, share float64
	if maxKey > 0 {
		frac = float64(k) / float64(maxKey)
	}
	if parentTotal > 0 {
		share = float64(k) / float64(parentTotal)
	}

	name := r.t.Name(i)
	var sty []style
	switch {
	case e.Flags&scan.FlagReparse != 0:
		name += "\\ ->"
		sty = []style{gray}
	case e.Flags&scan.FlagDir != 0:
		name += "\\"
		sty = []style{bold, blue}
	}

	var marks string
	if e.Flags&scan.FlagUnreadable != 0 {
		marks += r.paint(" !", red)
	}
	if e.Flags&scan.FlagCompressed != 0 {
		marks += r.paint(" c", dim)
	}
	if e.Flags&scan.FlagSparse != 0 {
		marks += r.paint(" s", dim)
	}

	fmt.Fprintf(r.w, "%s  %s %s  %s%s%s\n",
		r.padLeft(Human(k), sizeWidth, bold),
		r.padLeft(fmt.Sprintf("%.1f%%", share*100), 6, dim),
		r.bar(frac, barWidth, heatOf(share)),
		strings.Repeat("  ", level-1),
		r.paint(name, sty...),
		marks)
}

// padLeft right-aligns before painting, since escape codes are not printable
// width and would break %*s.
func (r *renderer) padLeft(s string, width int, sty ...style) string {
	if n := width - len(s); n > 0 {
		return strings.Repeat(" ", n) + r.paint(s, sty...)
	}
	return r.paint(s, sty...)
}

func (r *renderer) sortEntries(ids []uint32) {
	switch r.opt.Sort {
	case "name":
		names := make(map[uint32]string, len(ids))
		for _, i := range ids {
			names[i] = strings.ToLower(r.t.Name(i))
		}
		sort.Slice(ids, func(a, b int) bool { return names[ids[a]] < names[ids[b]] })
	default: // size, alloc
		sort.Slice(ids, func(a, b int) bool {
			ka, kb := r.key(ids[a]), r.key(ids[b])
			if ka != kb {
				return ka > kb
			}
			return ids[a] < ids[b]
		})
	}
}

func (r *renderer) footer(rep Report) {
	var parts []string

	if rep.Stats.Unreadable > 0 {
		parts = append(parts, r.paint(fmt.Sprintf("%s unreadable %s",
			Comma(rep.Stats.Unreadable),
			plural(rep.Stats.Unreadable, "directory", "directories")), red))
	}
	if rep.Stats.Reparse > 0 {
		parts = append(parts, fmt.Sprintf("%s reparse %s not followed",
			Comma(rep.Stats.Reparse), plural(rep.Stats.Reparse, "point", "points")))
	}
	if rep.Deduped > 0 {
		parts = append(parts, fmt.Sprintf("%s hardlinked %s counted once",
			Comma(rep.Deduped), plural(rep.Deduped, "file", "files")))
	}
	if rep.Fallback > 0 {
		parts = append(parts, fmt.Sprintf("%s %s via fallback enumeration",
			Comma(rep.Fallback), plural(rep.Fallback, "directory", "directories")))
	}
	if d := rep.Delta; d != nil {
		what := fmt.Sprintf("%s %s updated from the journal",
			Comma(d.DirsTouched), plural(d.DirsTouched, "directory", "directories"))
		if d.DirsTouched == 0 {
			what = "nothing changed since the last scan"
		}
		parts = append(parts, r.paint(what, green))
		if !rep.CachedAt.IsZero() {
			parts = append(parts, "cached "+ago(time.Since(rep.CachedAt)))
		}
	} else if rep.CacheNote != "" {
		parts = append(parts, "full walk ("+rep.CacheNote+")")
	}
	parts = append(parts, fmt.Sprintf("%.1fs", rep.Elapsed.Seconds()))

	fmt.Fprintln(r.w, r.paint(strings.Join(parts, " · "), dim))
}
