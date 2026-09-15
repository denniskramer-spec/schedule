package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"git.sr.ht/~jackmordaunt/go-toast/v2"
)

/* ---------- Windows notifications ----------

   Things Schedule wants to tell you while you are busy elsewhere - a deadline
   has passed, the window closed but the app is still in the tray - arrive as
   ordinary Windows notifications instead of a window jumping in front of
   whatever you are doing. Clicking one opens Schedule. */

//go:embed icon.png
var iconPNG []byte

const notifyAppID = "Schedule"

// setupNotify registers Schedule as a notification sender and makes a click on
// any of its notifications bring the window up. It reports whether
// notifications can be used at all.
func setupNotify(dir string, open func()) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// Windows reads the sender's icon from a file, so keep a copy beside the log.
	icon := filepath.Join(dir, "icon.png")
	if err := os.WriteFile(icon, iconPNG, 0o644); err != nil {
		icon = ""
	}
	if err := toast.SetAppData(toast.AppData{AppID: notifyAppID, IconPath: icon}); err != nil {
		log.Printf("notifications unavailable: %v", err)
		return false
	}
	toast.SetActivationCallback(func(args string, data []toast.UserData) { open() })
	return true
}

// notify shows a notification and reports whether it could.
func notify(title, body string) bool {
	n := toast.Notification{AppID: notifyAppID, Title: title, Body: body}
	if err := n.Push(); err != nil {
		log.Printf("notification failed: %v", err)
		return false
	}
	return true
}
