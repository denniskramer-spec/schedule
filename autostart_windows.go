package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// Starting with Windows is a value under the current user's Run key, so it
// needs no administrator rights. Setup writes the same value on install.
const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "Schedule"

	// choiceKey remembers that the user picked on or off themselves, so Setup
	// leaves the setting alone when it updates Schedule.
	choiceKey   = `Software\Schedule`
	choiceValue = "StartWithWindows"
)

// autostartEnabled reports whether Schedule is set to start at sign-in.
func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

// setAutostart turns starting at sign-in on or off. It points at this exe,
// with -tray so it waits in the notification area instead of opening a window.
func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && err != registry.ErrNotExist {
			return err
		}
	} else {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := k.SetStringValue(runValue, `"`+exe+`" -tray`); err != nil {
			return err
		}
	}
	if c, _, err := registry.CreateKey(registry.CURRENT_USER, choiceKey, registry.SET_VALUE); err == nil {
		var v uint32
		if on {
			v = 1
		}
		c.SetDWordValue(choiceValue, v)
		c.Close()
	}
	return nil
}
