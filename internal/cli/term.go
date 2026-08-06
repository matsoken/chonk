//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableColor reports whether f can render ANSI sequences, turning on virtual
// terminal processing if the console needs it. Windows Terminal enables VT by
// default; the legacy conhost does not, and prints the escape codes literally
// unless asked.
//
// Honors NO_COLOR (https://no-color.org) and returns false when output is
// redirected to a file or pipe.
func EnableColor(f *os.File) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}

	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false // not a console: redirected to a file or pipe
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}

// IsTerminal reports whether f is attached to a console, used to decide whether
// live progress is worth printing.
func IsTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
