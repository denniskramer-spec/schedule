//go:build !windows

package main

import "errors"

// Starting at sign-in is only wired up for Windows.

func autostartEnabled() bool { return false }

func setAutostart(on bool) error { return errors.New("only supported on Windows") }
