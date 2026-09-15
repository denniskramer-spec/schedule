package main

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// Setup is linked with -H windowsgui, so it has no console. Every question and
// message is a standard Windows message box instead.

var (
	user32         = syscall.NewLazyDLL("user32.dll")
	messageBox     = user32.NewProc("MessageBoxW")
	setDPIAware    = user32.NewProc("SetProcessDPIAware")
	dpiAwareCalled bool
)

const (
	mbOK          = 0x00000000
	mbYesNo       = 0x00000004
	mbIconError   = 0x00000010
	mbIconQuest   = 0x00000020
	mbIconWarning = 0x00000030
	mbIconInfo    = 0x00000040
	mbDefButton2  = 0x00000100
	mbSetFg       = 0x00010000
	idYes         = 6
)

func box(text string, flags uintptr) int {
	if !dpiAwareCalled {
		// Without this the dialogs are drawn blurry on scaled displays.
		setDPIAware.Call()
		dpiAwareCalled = true
	}
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(appName + " Setup")
	r, _, _ := messageBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags|mbSetFg)
	return int(r)
}

// ask shows a Yes/No question; dflt picks which button Enter presses.
func ask(q string, dflt bool) bool {
	flags := uintptr(mbYesNo | mbIconQuest)
	if !dflt {
		flags |= mbDefButton2
	}
	return box(q, flags) == idYes
}

func info(msg string)   { box(msg, mbOK|mbIconInfo) }
func notice(msg string) { box(msg, mbOK|mbIconWarning) }
func alert(msg string)  { box(msg, mbOK|mbIconError) }

const createNoWindow = 0x08000000

// hideConsole keeps the console tools Setup relies on - powershell, reg, cmd -
// from flashing up windows of their own.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// The same Run value Schedule's own "Start with Windows" menu item uses.
const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "Schedule"
)

// turnedOffStartAtLogin reports whether the user unticked Start with Windows
// in Schedule's tray menu, which records the choice under Software\Schedule.
func turnedOffStartAtLogin() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Schedule`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("StartWithWindows")
	return err == nil && v == 0
}

// forgetStartAtLoginChoice removes that record when Schedule is uninstalled.
func forgetStartAtLoginChoice() {
	registry.DeleteKey(registry.CURRENT_USER, `Software\Schedule`)
}

// setStartAtLogin makes Schedule start in the notification area at sign-in, or
// stops it doing so.
func setStartAtLogin(exe string, on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	return k.SetStringValue(runValue, `"`+exe+`" -tray`)
}
