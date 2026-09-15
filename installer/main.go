// Schedule Setup - a single self-contained installer for Windows.
//
// It carries schedule.exe inside itself and puts it under the current user's
// own folders, which means no administrator rights and no UAC prompt. It adds
// a Start menu entry and registers with Apps & features so Windows can offer
// to remove it later; -uninstall does the same job from the command line.
//
// It is built with -H windowsgui: there is no console window, and every
// question is a standard Windows message box (see ui_windows.go). With -auto
// there are no questions at all: that is how Schedule updates itself, and
// anything worth saying goes to setup.log next to the app's own log, under
// %APPDATA%\Schedule, instead.
// -tray with it starts the new copy in the notification area, where the old
// one was.
//
// Nothing here needs a packaging tool: the file copy is Go, and the two things
// Go cannot do directly - a .lnk shortcut and a registry value - are handed to
// powershell and reg, both of which ship with Windows.
package main

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed payload/schedule.exe
var payload []byte

const (
	appName   = "Schedule"
	appExe    = "schedule.exe"
	uninstExe = "uninstall.exe"
	regKey    = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\Schedule`
)

// version is stamped in by the build; the default is what you get from a bare
// "go build".
var version = "1.0"

// auto: no questions, no dialogs; tray: start the new copy in the tray.
var auto, tray bool

func main() {
	for _, a := range os.Args[1:] {
		switch strings.TrimLeft(a, "-/") {
		case "uninstall":
			uninstall()
			return
		case "auto":
			auto = true
		case "tray":
			tray = true
		}
	}
	if auto {
		// The first thing written, so a Setup that got this far leaves a trace
		// even if nothing after it works.
		setupLog("Setup %s started with %v, from %s", version, os.Args[1:], selfPath())
	}
	install()
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "?"
	}
	return p
}

// setupLog is where -auto reports, since it shows nothing: setup.log in the
// app's own folder, which exists on any machine Schedule has run on.
func setupLog(format string, args ...any) {
	dir := profileDir()
	if dir == "" {
		return
	}
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "setup.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format("2006-01-02 15:04:05")+" "+format+"\n", args...)
}

/* ---------- where things go ---------- */

// installDir is under LocalAppData rather than Program Files on purpose: the
// user already owns it, so nothing has to be elevated.
func installDir() string {
	base := os.Getenv("LocalAppData")
	if base == "" {
		// Only reachable on a very odd profile, but a relative path here would
		// scatter the install into whatever folder setup was double-clicked in.
		if home := os.Getenv("UserProfile"); home != "" {
			base = filepath.Join(home, "AppData", "Local")
		} else {
			die("Windows did not say where your local app data folder is,\n" +
				"so there is nowhere safe to install to.")
		}
	}
	return filepath.Join(base, "Programs", appName)
}

func startMenuLink() string {
	return filepath.Join(os.Getenv("AppData"),
		`Microsoft\Windows\Start Menu\Programs`, appName+".lnk")
}

func desktopLink() string {
	home := os.Getenv("UserProfile")
	if home == "" {
		return ""
	}
	return filepath.Join(home, "Desktop", appName+".lnk")
}

// profileDir is the app window's browser profile, which Schedule itself makes.
// It is not ours to delete without asking.
func profileDir() string {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cfg, appName)
}

/* ---------- install ---------- */

func install() {
	dir := installDir()
	exe := filepath.Join(dir, appExe)

	q := fmt.Sprintf("Install %s %s?\n\nInstall to:  %s\nSize:  %.1f MB",
		appName, version, dir, float64(len(payload))/(1<<20))
	if _, err := os.Stat(exe); err == nil {
		q += "\n\n" + appName + " is already installed there and will be replaced.\n" +
			"If it is open, it will be closed first."
	}
	if auto {
		setupLog("installing %s %s on its own", appName, version)
	} else if !ask(q, true) {
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		die("Could not create %s\n\n%v", dir, err)
	}

	// A running copy holds a lock on its own file. Ask it to quit, then write
	// the new exe in a way that still works if it did not.
	closeRunning()
	if auto {
		setupLog("replacing %s", exe)
	}
	if err := writeFile(exe, payload); err != nil {
		die("Could not write %s\n\n%v\n\n"+
			"If %s is running, close its window and run Setup again.", exe, err, appName)
	}

	var warnings []string

	// The uninstaller is this same program: it already knows every path it put
	// something in, so there is nothing to keep in step.
	if self, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(self); err == nil {
			if err := writeFile(filepath.Join(dir, uninstExe), data); err != nil {
				warnings = append(warnings, "Could not install the uninstaller: "+err.Error())
			}
		}
	}

	if err := shortcut(startMenuLink(), exe, dir); err != nil {
		warnings = append(warnings, "Could not add the Start menu entry: "+err.Error())
	}

	if link := desktopLink(); link != "" {
		// Keep an existing Desktop shortcut up to date without asking again.
		_, had := os.Stat(link)
		if had == nil || (!auto && ask("Put a "+appName+" shortcut on the Desktop?", false)) {
			if err := shortcut(link, exe, dir); err != nil {
				warnings = append(warnings, "Could not add the Desktop shortcut: "+err.Error())
			}
		}
	}

	// Someone who unticked Start with Windows should not find it switched
	// back on by an update.
	if !turnedOffStartAtLogin() {
		if err := setStartAtLogin(exe, true); err != nil {
			warnings = append(warnings, "Could not set it to start with Windows: "+err.Error())
		}
	}

	if err := register(dir, exe); err != nil {
		warnings = append(warnings, "Could not register with Apps & features: "+err.Error())
	}

	if len(warnings) > 0 {
		if auto {
			setupLog("installed, but: %s", strings.Join(warnings, "; "))
		} else {
			notice(appName + " is installed, but not everything went to plan:\n\n" +
				strings.Join(warnings, "\n\n"))
		}
	}

	done := appName + " is installed.\n\n" +
		"Your tasks are kept in a file of your own, schedule.json, under\n" +
		"your AppData folder. Settings > Show file opens it; copy it to\n" +
		"back it up. A daily copy is kept beside it for two weeks.\n\n" +
		"It starts with Windows and waits in the notification area. To turn\n" +
		"that off, right-click its icon and untick Start with Windows.\n\n" +
		"To remove " + appName + " later, use Apps & features.\n\n" +
		"Start " + appName + " now?"
	if auto || ask(done, true) {
		// schedule.exe is a GUI program with no console, so starting it
		// directly opens only its app window, and it outlives Setup. An
		// update puts it back where it was: in the tray, or on screen.
		var args []string
		if tray {
			args = append(args, "-tray")
		}
		c := exec.Command(exe, args...)
		c.Dir = dir
		if err := c.Start(); err != nil {
			if auto {
				setupLog("could not start %s: %v", appName, err)
			} else {
				alert("Could not start " + appName + ": " + err.Error())
			}
		} else if auto {
			setupLog("installed %s and started it", version)
		}
	}
}

// closeRunning asks a running Schedule to exit through its own quit endpoint,
// the same one the power button in the app uses, then waits until it has
// really gone: as long as the old copy answers on its port, a new copy
// started now would take it for a running Schedule and step aside, and
// nothing would be left running. If nothing is listening this returns
// straight away.
func closeRunning() {
	const addr = "http://127.0.0.1:8765"
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Post(addr+"/api/quit", "application/json", nil); err == nil {
		resp.Body.Close()
		if auto {
			setupLog("asked the running copy to quit")
		}
	}
	probe := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := probe.Get(addr + "/api/prefs")
		if err != nil {
			return // nothing answering: it has gone
		}
		resp.Body.Close()
		time.Sleep(250 * time.Millisecond)
	}
	if auto {
		setupLog("the old copy is still answering after 30s; going ahead anyway")
	}
}

/* ---------- uninstall ---------- */

func uninstall() {
	dir := installDir()
	if !ask(fmt.Sprintf("Remove %s from this computer?\n\n%s\n\n"+
		"Your tasks stay where they are unless you say otherwise.", appName, dir), false) {
		return
	}

	closeRunning()

	for _, link := range []string{startMenuLink(), desktopLink()} {
		if link != "" {
			os.Remove(link)
		}
	}

	setStartAtLogin("", false)
	forgetStartAtLoginChoice()

	// The Apps & features entry, and what Schedule registered to send
	// notifications.
	for _, key := range []string{regKey, `HKCU\Software\Classes\AppUserModelId\Schedule`} {
		reg := exec.Command("reg", "delete", key, "/f")
		hideConsole(reg)
		reg.Run()
	}

	if p := profileDir(); p != "" {
		// The app window's browser profile is disposable; the data file and
		// its backups beside it are not, so those go only on request.
		os.RemoveAll(filepath.Join(p, "window"))
		if _, err := os.Stat(filepath.Join(p, "schedule.json")); err == nil {
			if ask("Also delete your tasks and settings?\n\n"+p+"\n\n"+
				"This removes schedule.json and the backups folder. Choose No to keep them.", false) {
				if err := os.RemoveAll(p); err != nil {
					notice("Could not delete it: " + err.Error())
				}
			}
		}
	}

	// Windows will not let a running program delete itself, so the last step
	// goes to a hidden cmd that waits for this one to exit first.
	cmd := exec.Command("cmd", "/c",
		"ping 127.0.0.1 -n 3 >nul & rd /s /q \""+dir+"\"")
	hideConsole(cmd)
	if err := cmd.Start(); err != nil {
		notice(fmt.Sprintf("Could not remove the folder: %v\n\nDelete it by hand:\n%s", err, dir))
		return
	}
	info(appName + " has been removed.")
}

/* ---------- the two things Go cannot do on its own ---------- */

// shortcut writes a .lnk. Windows shortcuts are COM objects, so this is the
// one place worth shelling out to powershell.
func shortcut(link, target, workdir string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	ps := strings.Join([]string{
		"$w = New-Object -ComObject WScript.Shell",
		"$s = $w.CreateShortcut(" + psQuote(link) + ")",
		"$s.TargetPath = " + psQuote(target),
		"$s.WorkingDirectory = " + psQuote(workdir),
		"$s.Description = 'A task board for your day'",
		"$s.Save()",
	}, "; ")
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-Command", ps)
	hideConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// psQuote makes a PowerShell single-quoted string, where the only escape is a
// doubled quote.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// register puts Schedule in Apps & features so it can be removed the usual way.
func register(dir, exe string) error {
	vals := [][]string{
		{"DisplayName", "REG_SZ", appName},
		{"DisplayVersion", "REG_SZ", version},
		{"Publisher", "REG_SZ", appName},
		{"InstallLocation", "REG_SZ", dir},
		{"DisplayIcon", "REG_SZ", exe},
		{"UninstallString", "REG_SZ", "\"" + filepath.Join(dir, uninstExe) + "\" -uninstall"},
		{"NoModify", "REG_DWORD", "1"},
		{"NoRepair", "REG_DWORD", "1"},
		{"EstimatedSize", "REG_DWORD", fmt.Sprint(len(payload) / 1024)},
	}
	for _, v := range vals {
		cmd := exec.Command("reg", "add", regKey,
			"/v", v[0], "/t", v[1], "/d", v[2], "/f")
		hideConsole(cmd)
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

/* ---------- helpers ---------- */

// writeFile replaces a file that may be in use. Windows refuses to overwrite or
// delete a running exe but does allow renaming it, so the old file is moved
// aside first and cleaned up on a later run once nothing holds it.
func writeFile(path string, data []byte) error {
	tmp := path + ".new"
	old := path + ".old"
	os.Remove(old) // left by an earlier run; fails harmlessly if still in use

	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		if err := os.Rename(path, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func die(format string, args ...any) {
	if auto {
		setupLog("failed: "+format, args...)
		os.Exit(1)
	}
	alert(fmt.Sprintf("Setup could not finish.\n\n"+format, args...))
	os.Exit(1)
}
