# chonk — plan

A fast disk usage tool for Windows. CLI first, TUI second.

`chonk C:\` prints the drive's total, used, and percentage, then a breakdown of
top-level directories with proportional bars. `chonk --tui C:\` drops into an
interactive drill-down of the same tree.

The gap being filled is not raw speed — WizTree already owns that — but a tool
that is fast *and* pleasant: no admin prompt, no GUI, works on network shares
and USB sticks, good-looking terminal output.

## Non-goals

- Treemap visualization. That needs a real GUI; out of scope.
- Deleting or moving files. Read-only tool. Reporting only. The TUI's `o`, `!`
  and `c` keys are not a softening of this: they hand a path to File Explorer,
  a child shell, or the clipboard, and the user does the deleting somewhere
  else. chonk still writes nothing but its own cache. `os/exec` in
  `internal/tui/actions.go` is that handoff, not a retreat from the invariant.
- Cross-platform. `//go:build windows` throughout. `gdu` already covers Linux.

## Settled decisions

These were worked through already. Don't relitigate them mid-session.

**Go, not Rust/C#/C++.** The workload is syscall- and memory-bound, so language
choice is worth maybe 1.5x. Bubbletea is worth more than that on the UI side.

**Tier-2 enumeration, not MFT parsing.** `GetFileInformationByHandleEx` with
`FileIdBothDirectoryInfo` fills a 64KB buffer per syscall rather than one entry
per call like `FindNextFile`. Roughly 5-10x faster than naive recursion, needs
no admin, works on any filesystem. Raw `$MFT` parsing is another ~5x but costs
admin rights, NTFS-only, and a full NTFS attribute parser including fixups and
data-run decoding. Deferred indefinitely (see Later).

**USN journal is for deltas, not sizes.** `USN_RECORD_V2` has no size field, so
it cannot build the tree on its own. Its value is telling us which file
references changed since the last scan, so run 2 onward can be near-instant.

**Headline numbers never require a scan.** `GetDiskFreeSpaceEx` is one syscall.
The scan only exists to fill in the breakdown.

## Layout

```
chonk/
  go.mod                  module github.com/YOU/chonk
  cmd/chonk/main.go       flag parsing, mode dispatch
  internal/scan/
    walker.go             DONE — parallel enumerator, flat tree, rollup
    fallback.go           M3 — FileFullDirectoryInfo path
    cache.go              M5 — USN delta cache
  internal/cli/render.go  table, bars, human-readable sizes
  internal/tui/           M4 — bubbletea model
```

`walker.go` is already written and drops in at `internal/scan/`.

## Data model invariants

The whole design leans on these three. Breaking any one costs a rewrite.

1. **`Entry` is pointer-free.** No strings, no slices, no maps in the struct.
   A `[]Entry` of 3M elements is then a single allocation the GC never scans.
   Adding a `Name string` field is the single easiest way to make this tool
   feel like WinDirStat.
2. **`ParentIdx < index` for every entry.** A directory is committed to the
   array before it is ever queued for scanning, so parents always precede
   children even under parallel enumeration. This is what makes `Rollup` a
   single reverse loop instead of a graph traversal.
3. **Names stay UTF-16 until display.** Windows hands us UTF-16; Go strings are
   UTF-8. Converting 3M names eagerly is millions of allocations for data we
   will never look at. Convert the few hundred rows actually rendered.

## Milestones

### M0 — scaffold
`go mod init`, drop in `walker.go`, get it compiling.

The `unsafe.Sizeof` assertion at the top of `walker.go` verifies the
`FILE_ID_BOTH_DIR_INFO` layout comes out at 104 bytes. It was reasoned out by
hand, not compiled. If the build fails there, print `unsafe.Offsetof` for each
field and correct `nameFieldOffset`.

*Done when:* `chonk C:\Users\you` prints a raw entry count that roughly matches
Explorer's file count for that folder.

### M1 — CLI output
The actual daily-use deliverable. `DiskUsage()` for the headline, `Scan()` +
`Rollup()` + `Children(0)` for the breakdown.

Steal from `dust` and `gdu`: proportional bar per row, right-aligned
human-readable sizes, sorted descending, color. The point is that the default
invocation answers the question without entering a TUI at all.

Flags: `--sort size|alloc|name`, `--depth N`, `--top N`, `--no-color`,
`--json`.

*Done when:* `chonk C:\` is visibly faster than `du -sh` equivalents and the
output is worth screenshotting.

### M2 — correctness
Numbers that look plausible but are wrong are the main risk in this category of
tool. Work through:

- **Hardlinks.** `FileID` is captured but unused. Without dedupe, WinSxS looks
  enormous — it is mostly hardlinks into System32. A `map[uint64]struct{}` over
  3M files costs ~150MB and real hashing time, so put it behind `--dedupe` and
  measure before making it default.
- **Logical vs allocated.** Report both. `Size` is `EndOfFile`, `Alloc` is
  `AllocationSize`. They diverge sharply on compressed and sparse files, and
  "why doesn't this match Explorer" is always one of these two.
- **Reparse points.** Recorded, never followed. Verify `C:\Users\All Users`
  does not loop and OneDrive placeholders don't get counted at full size.
- **Unreadable directories.** `FlagUnreadable` is set but nothing surfaces it.
  Show a count in the footer; silently under-reporting is worse than the error.

*Done when:* totals reconcile with Explorer's folder properties on three real
directories, including one under `C:\Windows`.

### M3 — fallback enumeration
`FileIdBothDirectoryInfo` is not universal — some network redirectors return
`ERROR_INVALID_PARAMETER` on the class. Add `FileFullDirectoryInfo` (class 14,
same buffer-walking shape, no `FileId` field, so no dedupe on that path).
Detect at runtime, fall back per-directory.

### M4 — TUI
Bubbletea. Drill-down tree over the same flat array, navigating by index.
`Children()` is currently a linear scan — build a child index once at startup
before wiring it to a UI that calls it per keystroke.

Minimum: arrows to navigate, enter to descend, backspace to ascend, `s` to
toggle sort, `/` to filter, `q` to quit. Live progress during the scan.

### M5 — USN delta cache
Where the repeat-run speed comes from. Persist the tree plus the USN journal
position after each scan. On the next run, read the journal forward from that
position, stat only the changed file references, patch the tree, re-roll sums.

Second run onward is effectively instant, which beats WizTree's cold start
without touching `$MFT` or requiring admin.

Handle: journal disabled, journal wrapped (entries aged out — detect and fall
back to full rescan), cache older than some threshold.

## Later, maybe

Raw `$MFT` parsing for a fast cold start on a fresh volume. Needs
`SeBackupPrivilege`, admin, NTFS-only, and a real attribute parser: update
sequence array fixups, data-run decoding, resident vs non-resident `$DATA`,
`$FILE_NAME` DOS-namespace dedupe. A genuinely fun weekend, and a want rather
than a need once M5 lands.

If it happens, keep the tier-2 walker permanently as both the fallback and the
correctness oracle — diff the two trees against each other. A subtly wrong
data-run parser produces confident, wrong numbers.

## Testing

- Build a fixture tree with known sizes, sparse files, a junction, and a
  hardlink pair. Assert exact totals.
- Cross-check against `Get-ChildItem -Recurse | Measure-Object -Sum Length`
  on a mid-size directory. Slow, but authoritative for logical size.
- Benchmark on a real volume, cold and warm cache. File *count* dominates
  runtime far more than byte count, so report both in benchmarks.
