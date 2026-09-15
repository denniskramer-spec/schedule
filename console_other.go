//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Elsewhere the program runs from a terminal, so these are plain.

func hideConsole(cmd *exec.Cmd) {}

func logToFile(dir string) {}

// Bringing a window forward is left to the desktop; opening a new one will do.
func focusWindow(title string) bool { return false }

func allowForeground() {}

func showError(msg string) {
	fmt.Fprintf(os.Stderr, "\nSchedule could not start.\n\n%s\n", msg)
}
