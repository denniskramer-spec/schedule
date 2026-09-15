//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Setup only does real work on Windows. This terminal version exists so the
// package still builds, vets and tests on the Linux machine it is made on.

var stdin = bufio.NewReader(os.Stdin)

func ask(q string, dflt bool) bool {
	fmt.Print(q + " [y/n] ")
	line, err := stdin.ReadString('\n')
	if err != nil {
		return dflt
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return dflt
}

func info(msg string)   { fmt.Println(msg) }
func notice(msg string) { fmt.Println(msg) }
func alert(msg string)  { fmt.Fprintln(os.Stderr, msg) }

func hideConsole(cmd *exec.Cmd) {}

func setStartAtLogin(exe string, on bool) error { return nil }

func turnedOffStartAtLogin() bool { return false }

func forgetStartAtLoginChoice() {}
