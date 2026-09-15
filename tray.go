package main

import (
	_ "embed"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
)

//go:embed icon.ico
var trayIcon []byte

// defaultAddr is where Schedule listens unless told otherwise, and where a
// second start looks for the first.
const defaultAddr = "127.0.0.1:8765"

// window keeps track of the app window so the tray can bring it back. Closing
// the window no longer quits Schedule: it carries on in the notification area
// until Quit is chosen there, or the power button in the app is pressed.
type window struct {
	url string
	// closedByUser runs when the window is closed while Schedule carries on
	// in the tray, rather than as part of quitting.
	closedByUser func()

	mu       sync.Mutex
	cmd      *exec.Cmd // the window we last opened, nil if it is closed
	quitting bool
}

// stopping marks that Schedule is quitting, so the window going away is not
// mistaken for the user closing it to the tray.
func (w *window) stopping() {
	w.mu.Lock()
	w.quitting = true
	w.mu.Unlock()
}

// show brings the window to the front, or opens a new one if there is none.
func (w *window) show() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cmd != nil && focusWindow("Schedule") {
		return
	}
	cmd := openUI(w.url)
	w.cmd = cmd
	if cmd == nil {
		return // opened in the default browser; nothing to watch
	}
	go func() {
		cmd.Wait()
		w.mu.Lock()
		mine := w.cmd == cmd
		if mine {
			w.cmd = nil
		}
		w.mu.Unlock()
		if !mine || w.closedByUser == nil {
			return
		}
		// The power button closes the window a moment before the quit reaches
		// us, so wait briefly before deciding this was a close to the tray.
		time.Sleep(time.Second)
		w.mu.Lock()
		quitting := w.quitting
		w.mu.Unlock()
		if !quitting {
			w.closedByUser()
		}
	}()
}

// open reports whether the app window is on screen.
func (w *window) open() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cmd != nil
}

// close shuts the window, if one is open: as Schedule quits, or to leave it
// in the tray (the hide button in the app). Its watcher in show tells the
// two apart by the quitting flag.
func (w *window) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd != nil && w.cmd.Process != nil {
		w.cmd.Process.Kill()
	}
}

// runTray puts Schedule's icon in the notification area and blocks until
// systray.Quit is called. It has to run on the main goroutine.
func runTray(win *window) {
	systray.Run(func() {
		if runtime.GOOS == "windows" {
			systray.SetIcon(trayIcon)
		} else {
			systray.SetIcon(iconPNG) // the Linux tray does not read .ico
		}
		systray.SetTitle("Schedule")
		systray.SetTooltip("Schedule")
		systray.SetOnTapped(func() { go win.show() })

		open := systray.AddMenuItem("Open Schedule", "Show the Schedule window")
		systray.AddSeparator()
		login := systray.AddMenuItemCheckbox("Start with Windows",
			"Start Schedule in the notification area when you sign in", autostartEnabled())
		if runtime.GOOS != "windows" {
			login.Hide()
		}
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Close Schedule completely")
		// Without a secondary-tap handler, right-click shows this menu.

		go func() {
			for {
				select {
				case <-open.ClickedCh:
					win.show()
				case <-login.ClickedCh:
					on := !login.Checked()
					if err := setAutostart(on); err != nil {
						log.Printf("start with Windows: %v", err)
						continue
					}
					if on {
						login.Check()
					} else {
						login.Uncheck()
					}
				case <-quit.ClickedCh:
					win.stopping()
					systray.Quit()
					return
				}
			}
		}()
	}, nil)
}

// alreadyRunning reports whether a copy of Schedule is already answering on
// its usual port, without asking it to do anything.
func alreadyRunning() bool {
	if argValue("addr") != "" {
		return false // a custom address is not the shared one
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + defaultAddr + "/api/prefs")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// showExisting asks a copy of Schedule that is already running to show its
// window. Starting Schedule again from the Start menu while it sits in the
// tray should bring it up, not start a second copy on another port.
func showExisting() bool {
	if argValue("addr") != "" {
		return false
	}
	allowForeground()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post("http://"+defaultAddr+"/api/show", "application/json", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	log.Print("already running; asked it to show its window")
	return true
}
