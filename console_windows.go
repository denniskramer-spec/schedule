package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// The Windows build is linked with -H windowsgui, so there is no console
// window. These helpers stand in for the things the console used to do.

const createNoWindow = 0x08000000

// hideConsole stops a console program we run, such as sc, from flashing up a
// window of its own now that we have no console for it to share.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// logToFile sends log output to %APPDATA%\Schedule\schedule.log, since there
// is no console to print it to. The previous run's log is kept alongside as
// schedule.old.log, so the reason for a crash survives restarting.
func logToFile(dir string) {
	path := filepath.Join(dir, "schedule.log")
	os.Rename(path, filepath.Join(dir, "schedule.old.log"))
	f, err := os.Create(path)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	findWindow          = user32.NewProc("FindWindowW")
	isIconic            = user32.NewProc("IsIconic")
	showWindow          = user32.NewProc("ShowWindow")
	setForegroundWindow = user32.NewProc("SetForegroundWindow")
	allowSetForeground  = user32.NewProc("AllowSetForegroundWindow")
)

// focusWindow brings the app window to the front and reports whether it found
// one. Chrome and Edge both use this window class, and the title is the page
// title, so a folder that happens to be called Schedule is not matched.
func focusWindow(title string) bool {
	class, _ := syscall.UTF16PtrFromString("Chrome_WidgetWin_1")
	name, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := findWindow.Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)))
	if hwnd == 0 {
		return false
	}
	if minimised, _, _ := isIconic.Call(hwnd); minimised != 0 {
		const swRestore = 9
		showWindow.Call(hwnd, swRestore)
	}
	setForegroundWindow.Call(hwnd)
	return true
}

// allowForeground lets the copy already running take the foreground when this
// one asks it to show its window. Otherwise Windows only flashes its taskbar
// button, because the user started this process, not that one.
func allowForeground() {
	const asfwAny = ^uintptr(0)
	allowSetForeground.Call(asfwAny)
}

// showError puts a startup failure in a message box, where a user who
// double-clicked the exe can read it.
func showError(msg string) {
	box := user32.NewProc("MessageBoxW")
	text, _ := syscall.UTF16PtrFromString(msg)
	title, _ := syscall.UTF16PtrFromString("Schedule could not start")
	const mbOK, mbIconError = 0x0, 0x10
	box.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbOK|mbIconError)
}
