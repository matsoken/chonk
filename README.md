# chonk

[![ci](https://github.com/matsoken/chonk/actions/workflows/ci.yml/badge.svg)](https://github.com/matsoken/chonk/actions/workflows/ci.yml)

A fast disk usage tool for Windows. No admin prompt, no GUI, works on network
shares and USB sticks.

```
chonk C:\
```

```
C:\
  371G used of 458G  (81.0%)   87.3G free
  ██████████████████████████████████████████░░░░░░░░░░
  319G logical   326G on disk   in 774,496 files, 241,718 directories

   85.9G   26.9% ██████████████████████  Users\
   61.7G   19.4% ████████████████░░░░░░  Program Files (x86)\
   53.9G   16.9% ██████████████░░░░░░░░  Windows\
   44.7G   14.0% ███████████░░░░░░░░░░░  temp\
   25.8G    8.1% ███████░░░░░░░░░░░░░░░  pagefile.sys
   23.4G    7.3% ██████░░░░░░░░░░░░░░░░  Program Files\
   23.3G                                 (25 more)

190 unreadable directories · 134 reparse points not followed · 12.0s
```

Run it again and it is effectively instant, because the second run reads the
NTFS change journal instead of the disk:

```
190 unreadable directories · 134 reparse points not followed · 6 directories updated from the journal · cached just now · 0.4s
```

`chonk --tui C:\` opens an interactive drill-down over the same tree.

## Install

Download the setup .exe from [Releases](https://github.com/matsoken/chonk/releases)
and run it. It installs to `%LOCALAPPDATA%\Programs\chonk`, adds that to your user
PATH, and never asks for admin — same as the tool itself. Uninstall from Settings →
Apps.

Or, with a Go toolchain:

```
go install github.com/matsoken/chonk/cmd/chonk@latest
```

winget support is [pending review](https://github.com/microsoft/winget-pkgs) in the
community repository; once it merges, `winget install matsoken.chonk`.

## Measured on a 458 GB volume

| | files | time |
|---|---|---|
| first run (cold cache) | 774,496 | 32 s |
| first run (warm OS cache) | 774,496 | 8–12 s |
| subsequent runs (journal delta) | 774,496 | **0.4 s** |

The on-disk cache is about 93 MB for a million entries: 40 bytes per entry plus
the UTF-16 name arena. It lives in `%LOCALAPPDATA%\chonk\`, one file per scan
root.

### How long the cache stays warm

There is no time-based expiry. The cache is valid for exactly as long as the USN
journal still contains the position it was saved at, and that window is shorter
than people expect.

Windows gives the journal a **32 MB** default budget. Measured on this machine
while close to idle, the journal advances about **10.5 MB/hour**, so it retains
roughly **3 hours** of history. During a build, a large checkout, or Windows
Update it is considerably less.

So repeated runs inside one working session hit the fast path. Come back
tomorrow and chonk silently does a full walk and writes a fresh cache:

```
90 reparse points not followed · full walk (journal wrapped) · 2.2s
90 reparse points not followed · 2 directories updated from the journal · cached just now · 0.2s
```

This is a self-healing fallback, never an error, and never a wrong number. The
footer always says which path was taken.

To widen the window, enlarge the journal (needs admin, once):

```
fsutil usn createjournal m=536870912 a=8388608 C:
```

512 MB buys roughly two days at the rate above. Note this reallocates the
journal and assigns it a **new journal ID**, which invalidates any existing
chonk cache — the next run does one full walk.

A cache is also discarded, and a full walk done instead, when the journal was
reset or disabled, the volume serial does not match, more than 200,000 journal
records are pending (a full walk is cheaper by then), or the `Entry` struct
layout changed between builds.

## Flags

```
-tui              interactive drill-down instead of a printed table
-depth N          levels of directory nesting to print (default 1)
-top N            rows per level, 0 for all (default 20)
-sort size|alloc|name
-json             machine-readable output, honors -depth and -top
-dedupe           count hardlinked files once
-no-color         disable ANSI color (also honors NO_COLOR)
-refresh          ignore the cached tree but write a fresh one
-no-cache         always do a full walk
-fallback         force the compatible enumeration class
-workers N        concurrent directory readers
-quiet            suppress the live progress line
```

## TUI keys

`?` shows this list in the app.

```
↑ ↓ · k j     move                   o        show this folder in Explorer
pgup pgdn     move a page            !        shell in this folder
g · G         first · last row       c · y    copy the selected path
⏎ · → · l     open directory         C        copy this directory's path
⌫ · ← · h     go back up             r        rescan
s             cycle sort             /        filter (esc clears)
q             quit                   ?        key list
```

chonk never deletes anything — see the non-goals in `plan.md`. `o`, `!` and `c`
exist so it can hand a path to something that does.

`o` and `!` both act on the folder named in the header, not on the highlighted
row; press `⏎` first to act inside a directory. `o` does highlight the row you
were on inside that folder, which is free — Explorer's `/select,` opens an
item's parent, and that parent is the folder `!` would drop you into. `c` and
`C` are where the row and the folder are addressed separately.

`!` runs a shell with its working directory set to the folder you are browsing;
typing `exit` returns to chonk, which then marks the tree **stale**, because the
sizes on screen may be describing files you just removed. `r` rescans, and on a
volume with a working USN journal that is close to instant — it patches the
cached tree from the journal rather than walking again. A rescan starts back at
the root, since a fresh tree invalidates every entry index.

The shell is `cmd` by default. Set `CHONK_SHELL` to an executable — for example
`pwsh.exe` — to use another one. It names a program, not a command line, so
arguments in that variable are not parsed.

Note that no program can change the working directory of the shell it was
launched from, chonk included. `!` gives you a child shell in that folder; when
you quit chonk you are back where you started.

## How it works

**The headline never requires a scan.** `GetDiskFreeSpaceEx` is one syscall. The
walk only exists to fill in the breakdown.

**Enumeration is tier-2.** `GetFileInformationByHandleEx` with
`FileIdBothDirectoryInfo` fills a 64 KB buffer per syscall rather than returning
one entry per call like `FindNextFile`. No admin, any filesystem. Some network
redirectors reject that info class, so `FileFullDirectoryInfo` is used as a
per-directory fallback; it carries no `FileId`, which costs hardlink dedupe on
that path only.

**The tree is a flat, pointer-free array.** `Entry` has no strings, slices or
maps, so a 3M-element `[]Entry` is one allocation the GC never scans. Names live
in a single UTF-16 arena and are converted to Go strings only for the few
hundred rows actually rendered. A directory is always appended before its
children, so `ParentIdx < index` always holds — which is what lets the size
rollup be a single reverse loop instead of a graph traversal.

Those three properties are load-bearing. Breaking any one costs a rewrite; see
the invariants in `plan.md`.

## Two things that were not obvious

### Reading the USN journal does not require admin

Nearly every write-up says it does. What actually requires admin is opening the
*volume device*, `\\.\C:`, which needs `FILE_READ_DATA`. Two changes make the
whole thing work as a normal user:

- Open the volume's **root directory** (`C:\` with `FILE_FLAG_BACKUP_SEMANTICS`)
  instead of the device. This works with *zero* requested access rights, and
  `FSCTL_QUERY_USN_JOURNAL` succeeds on it.
- Read with `FSCTL_READ_UNPRIVILEGED_USN_JOURNAL`. The classic
  `FSCTL_READ_USN_JOURNAL` returns `ERROR_ACCESS_DENIED` to a normal user. The
  unprivileged variant filters out records for files the caller cannot see,
  which for a disk usage tool is the correct behavior anyway.

Measured on the development machine, unelevated:

```
\\.\C:  access=0                 -> QUERY: Incorrect function
C:\     access=0 + BACKUP_SEM.   -> QUERY: ok, id=0x1d7d1bf28e427a2
        FSCTL_READ_USN_JOURNAL               -> Access is denied
        FSCTL_READ_UNPRIVILEGED_USN_JOURNAL  -> ok, 819 records
```

Journal positions must be record boundaries the journal itself handed back. An
arbitrary byte offset is rejected with `ERROR_INVALID_PARAMETER`, so the cache
stores the cursor returned by the previous read.

### The enumeration buffer must be 8-byte aligned

`GetFileInformationByHandleEx` fails with `ERROR_NOACCESS` ("Invalid access to
memory location") if its output buffer is misaligned, and Go guarantees no
particular alignment for `make([]byte, n)` — one landed at 4 mod 8 in practice.
Buffers are allocated as `[]uint64` and reinterpreted; see `newDirBuf`.
`TestDirBufIsAligned` and `TestEnumerateRejectsMisalignedBuffer` keep this from
regressing silently, because the symptom (every directory reports as unreadable)
looks nothing like the cause.

## Correctness

Logical totals reconcile **exactly** with
`Get-ChildItem -Recurse | Measure-Object -Sum Length` on
`C:\Windows\System32\drivers`, `C:\Windows\Fonts`, `C:\Windows\INF`,
`C:\Windows\Boot` and `C:\Program Files\Common Files`.

- **Logical vs allocated** are both reported. `Size` is `EndOfFile`, `Alloc` is
  `AllocationSize`; they diverge sharply on compressed and sparse files, and
  "why doesn't this match Explorer" is nearly always one of the two.
- **Reparse points** are recorded but never followed, so `C:\Users\All Users`
  does not send the walk into a loop and OneDrive placeholders are not counted
  at full size. The footer says how many were skipped.
- **Hardlinks** are counted once under `-dedupe`. Across a whole volume that is
  59,002 files and 16.9 GiB, at no measurable time cost. It stays opt-in because
  Explorer double-counts them too, so the default matches what people expect.
- **Unreadable directories** are counted in the footer. Silently
  under-reporting is worse than saying so.

The delta path is checked against a full walk after every kind of change —
create, delete, resize, new nested subtree — and over repeated cycles, since an
incremental cache that is merely almost right degrades invisibly.

## Testing

```
go test ./...
```

The race detector needs a C toolchain that is not installed here; in its place
`TestDeterministicAcrossWorkerCounts` runs the same tree at 1, 2, 3, 8 and 32
workers and requires identical totals, entry counts, and the `ParentIdx`
ordering invariant.

## Non-goals

Treemaps (needs a real GUI), deleting or moving files (read-only, reporting
only), and cross-platform support (`//go:build windows` throughout; `gdu`
already covers Linux).

## License

[MIT](LICENSE).
