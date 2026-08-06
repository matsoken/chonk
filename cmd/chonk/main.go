//go:build windows

// Command chonk reports disk usage for a Windows directory tree.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matsoken/chonk/internal/cli"
	"github.com/matsoken/chonk/internal/scan"
	"github.com/matsoken/chonk/internal/tui"
)

const version = "0.1.0"

func main() {
	var (
		sortBy   = flag.String("sort", "size", "sort rows by size, alloc or name")
		depth    = flag.Int("depth", 1, "levels of directory nesting to print")
		top      = flag.Int("top", 20, "rows per level (0 for all)")
		noColor  = flag.Bool("no-color", false, "disable ANSI color")
		jsonOut  = flag.Bool("json", false, "emit JSON instead of a table")
		dedupe   = flag.Bool("dedupe", false, "count hardlinked files once (slower, uses more memory)")
		workers  = flag.Int("workers", 0, "concurrent directory readers (0 picks a default)")
		fallback = flag.Bool("fallback", false, "force the compatible enumeration class (no hardlink dedupe)")
		showVer  = flag.Bool("version", false, "print version and exit")
		quiet    = flag.Bool("quiet", false, "suppress the live progress line")
		useTUI   = flag.Bool("tui", false, "interactive drill-down instead of a printed table")
		noCache  = flag.Bool("no-cache", false, "always do a full walk; do not read or write the cache")
		refresh  = flag.Bool("refresh", false, "ignore the cached tree but write a fresh one")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("chonk", version)
		return
	}

	switch *sortBy {
	case "size", "alloc", "name":
	default:
		fatalf("invalid -sort %q: want size, alloc or name", *sortBy)
	}

	target := "."
	if flag.NArg() > 0 {
		target = flag.Arg(0)
	}
	root, err := filepath.Abs(target)
	if err != nil {
		fatalf("cannot resolve %q: %v", target, err)
	}
	if fi, err := os.Stat(root); err != nil {
		fatalf("cannot read %s: %v", root, err)
	} else if !fi.IsDir() {
		fatalf("%s is not a directory", root)
	}

	acquireOpts := scan.AcquireOptions{
		Scan:     scan.Options{Workers: *workers, ForceFallback: *fallback},
		UseCache: !*noCache,
		Refresh:  *refresh,
	}

	// The TUI owns the whole terminal, including its own progress display, so
	// it drives the scan itself rather than reusing the CLI path.
	if *useTUI {
		if err := tui.Run(root, acquireOpts, *dedupe); err != nil {
			fatalf("tui: %v", err)
		}
		return
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	opt := cli.Options{
		Sort:  *sortBy,
		Depth: *depth,
		Top:   *top,
		Alloc: *sortBy == "alloc",
		Color: !*noColor && !*jsonOut && cli.EnableColor(os.Stdout),
	}

	// The headline needs no scan at all: one syscall for the whole volume.
	rep := cli.Report{Root: root}
	rep.Total, rep.Used, rep.DiskErr = scan.DiskUsage(root)

	start := time.Now()
	t, err := acquireWithProgress(root, acquireOpts,
		!*quiet && !*jsonOut && cli.IsTerminal(os.Stderr), &rep)
	if err != nil {
		fatalf("scan failed: %v", err)
	}
	rep.Fallback = t.FallbackDirs()

	// Dedupe zeroes the sizes of hardlinked files, so it has to happen after
	// the cache is written or the next run would inherit those zeros.
	if *dedupe {
		rep.Deduped = t.Dedupe() // must precede Rollup: it edits leaf sizes
	}
	t.Rollup()
	rep.Stats = t.Stats()
	rep.Elapsed = time.Since(start)

	idx := t.BuildChildIndex()

	if *jsonOut {
		if err := cli.RenderJSON(out, t, idx, rep, opt); err != nil {
			fatalf("writing json: %v", err)
		}
		return
	}
	cli.Render(out, t, idx, rep, opt)
}

// acquireWithProgress gets the tree, printing a live entry count to stderr so a
// multi-second walk of a full volume does not look like a hang. Progress goes to
// stderr so that stdout stays clean for redirection.
//
// A cache hit returns almost immediately and never publishes a tree handle, so
// no progress line appears — which is the point.
func acquireWithProgress(root string, ao scan.AcquireOptions, live bool, rep *cli.Report) (*scan.Tree, error) {
	apply := func(res scan.AcquireResult) {
		rep.Delta, rep.CachedAt, rep.CacheNote = res.Delta, res.CachedAt, res.Note
	}

	if !live {
		t, res, err := scan.Acquire(root, ao)
		apply(res)
		return t, err
	}

	type result struct {
		t   *scan.Tree
		res scan.AcquireResult
		err error
	}
	done := make(chan result, 1)
	handle := make(chan *scan.Tree, 1)
	ao.Scan.Handle = handle

	go func() {
		t, res, err := scan.Acquire(root, ao)
		done <- result{t, res, err}
	}()

	var t *scan.Tree
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case t = <-handle:
		case <-tick.C:
			if t != nil {
				fmt.Fprintf(os.Stderr, "\r\033[K  scanning… %s entries", cli.Comma(int(t.Progress())))
			}
		case r := <-done:
			fmt.Fprint(os.Stderr, "\r\033[K")
			apply(r.res)
			return r.t, r.err
		}
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `chonk %s — disk usage for Windows

usage: chonk [flags] [path]

Prints the volume's capacity and usage, then a proportional breakdown of what
is underneath [path] (default: the current directory).

flags:
`, version)
	flag.PrintDefaults()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "chonk: "+format+"\n", args...)
	os.Exit(1)
}
