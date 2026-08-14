//go:build windows

package tui

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// shellDone arrives when the shell spawned by `!` exits and chonk has the
// terminal back.
type shellDone struct{ err error }

// explorerCmd builds the command that shows path in File Explorer. With sel,
// explorer opens path's *parent* and highlights path inside it — which works
// for directories just as well as files. Without it, path opens as a folder.
//
// The /select, form cannot go through the argument list. explorer.exe does not
// parse its command line the way CommandLineToArgvW does: it wants the quotes
// around the path alone.
//
//	explorer.exe /select,"C:\dir with a space\f.bin"   works
//	explorer.exe "/select,C:\dir with a space\f.bin"   opens Documents
//
// The second is what Go produces, because it escapes each argument as a unit
// and this one contains a space. Explorer does not report the problem; it just
// silently opens the default folder. SysProcAttr.CmdLine is the only way to set
// the line verbatim, and it has to lead with the program name: CreateProcess
// passes the whole string to the child, which takes the first token to be its
// own name and skips it. Paths cannot contain a quote character on Windows, so
// wrapping in quotes needs no escaping of its own.
func explorerCmd(path string, sel bool) *exec.Cmd {
	c := exec.Command("explorer.exe", path)
	if sel {
		c.SysProcAttr = &syscall.SysProcAttr{
			CmdLine: `explorer.exe /select,"` + path + `"`,
		}
	}
	return c
}

// explorerTarget picks what `o` shows. Like `!`, it acts on the folder being
// browsed rather than on the highlighted row — descend first to act inside a
// directory. The row is not ignored, though: selecting it inside that folder
// costs nothing and says which one you were on.
func (m *model) explorerTarget() (path string, sel bool) {
	if i, ok := m.selected(); ok {
		return m.pathOf(i), true
	}
	return m.pathOf(m.dir), false
}

// openExplorer shows the folder being browsed in File Explorer.
func (m *model) openExplorer() {
	path, sel := m.explorerTarget()

	c := explorerCmd(path, sel)
	// Start rather than Run: explorer detaches, and it exits with code 1 even
	// when it did exactly what was asked. Only a failure to launch is real.
	if err := c.Start(); err != nil {
		m.status = "explorer: " + err.Error()
		return
	}
	go c.Wait() // release the process handle once it is gone
	m.status = "opened " + path
}

// shellExe is the shell `!` drops into. CHONK_SHELL wins so PowerShell users
// are not stuck with cmd; it names an executable, not a command line, since
// splitting one correctly on Windows is its own project.
func shellExe() string {
	if s := os.Getenv("CHONK_SHELL"); s != "" {
		return s
	}
	if s := os.Getenv("ComSpec"); s != "" {
		return s
	}
	return "cmd.exe"
}

// shellHere suspends the TUI and runs a shell in dir. chonk cannot change the
// cwd of the shell it was launched from — no process can — so this spawns a
// child instead: you clean up, type exit, and land back in the browser.
func (m *model) shellHere(dir string) tea.Cmd {
	c := exec.Command(shellExe())
	c.Dir = dir
	return tea.ExecProcess(c, func(err error) tea.Msg { return shellDone{err} })
}

// launchFailed reports whether err means the shell never started — a missing
// CHONK_SHELL, or a directory that has since been deleted.
//
// A nonzero exit is not that. cmd.exe's bare `exit` returns the current
// ERRORLEVEL, so anyone whose last command failed would otherwise come back to
// chonk complaining about an exit status that is none of its business.
func launchFailed(err error) bool {
	if err == nil {
		return false
	}
	var exit *exec.ExitError
	return !errors.As(err, &exit)
}

// copyPath puts p on the clipboard and reports what happened in the footer.
func (m *model) copyPath(p string) {
	if err := m.copyFn(p); err != nil {
		m.status = "copy failed: " + err.Error()
		return
	}
	m.status = "copied " + p
}
