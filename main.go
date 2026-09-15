package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"fyne.io/systray"
)

//go:embed static
var staticFS embed.FS

// appDir is the per-user folder for things that are not task data, such as the
// app window's browser profile.
var appDir string

type server struct {
	db      Store
	origin  []string // allowed Origin header values
	quit    chan struct{}
	show    func()        // brings the app window up; see tray.go
	alert   alert         // a deadline close-out waiting for the page; see deadline.go
	recheck chan struct{} // prods the deadline watcher after a settings change

	upd       updater // the update check; see update.go
	chg       changes // the change number pages reload on; see refresh.go
	canNotify bool    // Windows notifications work; see notify.go
	stopping  func()  // tells the window Schedule is quitting
	// windowOpen says whether the app window is on screen, which is what an
	// update waits for before installing itself; see update.go.
	windowOpen func() bool
}

func main() {
	// Checked before anything else, and before the log file is started afresh,
	// so a second start cannot wipe the running copy's log.
	atLogin := startedAtLogin()
	if atLogin {
		if alreadyRunning() {
			return
		}
	} else if showExisting() {
		return
	}

	dir, err := configDir()
	if err != nil {
		fatal("Cannot work out where to keep the app's own files: %v", err)
	}
	appDir = dir
	logToFile(dir)

	// An update downloaded last time and left waiting installs now, before
	// the window opens; Setup starts Schedule again when it is done.
	if installAtStart(atLogin) {
		return
	}

	db, err := openData(atLogin)
	if err != nil {
		if uri := mongoURI(); uri != "" {
			fatal("Cannot reach MongoDB at %s\n\n%v\n\n%s\n"+
				"To keep tasks in a file on this computer instead, start\n"+
				"Schedule without the -mongo option.",
				hostLabel(uri), err, mongoHint(uri))
		}
		fatal("Cannot open the data file\n    %s\n\n%v", dataPath(), err)
	}
	defer db.Close()

	ln, err := listen()
	if err != nil {
		fatal("Cannot start the local server: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	url := "http://127.0.0.1:" + itoa(port)

	s := &server{
		db: db,
		origin: []string{
			"http://127.0.0.1:" + itoa(port),
			"http://localhost:" + itoa(port),
		},
		quit:    make(chan struct{}),
		recheck: make(chan struct{}, 1),
	}
	win := &window{url: url}
	s.show = win.show
	s.stopping = win.stopping
	s.windowOpen = win.open
	// -noui: no window, no tray, no notifications; the server alone, for tests.
	headless := hasArg("noui")
	if headless {
		s.show = func() {}
	} else {
		s.canNotify = setupNotify(dir, win.show)
		win.closedByUser = func() {
			if s.canNotify {
				trayHint(dir)
			}
			s.autoInstall() // an update that waited for the window to close
		}
	}

	httpSrv := &http.Server{Handler: s.routes()}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server stopped: %v", err)
		}
	}()

	go s.watchDeadline()
	go s.watchUpdates()
	go s.watchChanges()

	// Started with Windows: wait quietly in the tray until the icon is clicked.
	if !atLogin && !headless {
		win.show()
	}
	banner(url, db.Label())

	// Closing the window leaves Schedule in the tray. It quits from the tray
	// menu, the power button in the app, or Ctrl+C when run from a terminal.
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
			log.Print("shutting down")
		case <-s.quit:
			log.Print("exit requested from the app")
		}
		win.stopping()
		if !headless {
			systray.Quit()
		}
		close(done)
	}()
	if headless {
		<-done
	} else {
		runTray(win)
	}
	log.Print("quitting")

	win.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// fatal reports a startup failure. The Windows build has no console, so there
// it appears in a message box instead.
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	showError(msg)
	os.Exit(1)
}

// listen prefers the usual port and falls back to whatever the OS hands out.
// "-addr 127.0.0.1:0" asks for a free port outright; tests use that.
func listen() (net.Listener, error) {
	if a := argValue("addr"); a != "" {
		return net.Listen("tcp", a)
	}
	if ln, err := net.Listen("tcp", defaultAddr); err == nil {
		return ln, nil
	}
	log.Printf("%s is taken, asking the OS for a free one", defaultAddr)
	return net.Listen("tcp", "127.0.0.1:0")
}

// mongoHint turns "connection refused" into something a person can act on. On
// Windows it asks the service manager what state MongoDB is actually in, so a
// service that is merely stopped does not read like a missing install.
func mongoHint(uri string) string {
	if !strings.Contains(hostLabel(uri), "localhost") &&
		!strings.Contains(hostLabel(uri), "127.0.0.1") {
		return "That address is not on this machine, so check the server is up\n" +
			"and reachable, and that your IP is allowed to connect.\n" +
			"On MongoDB Atlas that list is under Network Access.\n"
	}
	if runtime.GOOS != "windows" {
		return "Start MongoDB and try again.\n"
	}

	name, state, found := mongoService()
	if !found {
		return "No MongoDB service is installed on this machine, which is why\n" +
			"\"net start MongoDB\" answers \"The service name is invalid\".\n\n" +
			"Install MongoDB Community Server from mongodb.com/try/download/community\n" +
			"and leave \"Install MongoDB as a Service\" ticked. Or, if you already\n" +
			"have the zip build, run mongod yourself:\n" +
			"    mongod --dbpath C:\\data\\db\n\n" +
			"To use MongoDB Atlas instead of a local server, see -mongo below.\n"
	}
	switch state {
	case "4":
		return "The \"" + name + "\" service is running, so it may be listening on a\n" +
			"different port, or bound so that this machine cannot reach it.\n"
	case "1":
		return "MongoDB is installed as \"" + name + "\" but the service is stopped.\n" +
			"Open Terminal as administrator and run:\n" +
			"    net start " + name + "\n"
	}
	return "Make sure the \"" + name + "\" service is started:\n" +
		"    net start " + name + "\n"
}

// stateRe picks the numeric service state out of "sc query" output. The state
// word is translated on localised Windows; the number is not. 1 = stopped,
// 4 = running. Two-digit values such as the TYPE line are not matched.
var stateRe = regexp.MustCompile(`:\s+([1-7])\b`)

// mongoService finds an installed MongoDB service without assuming its name:
// installers have used "MongoDB", "MongoDB Server" and version-suffixed names.
func mongoService() (name, state string, found bool) {
	cmd := exec.Command("sc", "query", "type=", "service", "state=", "all")
	hideConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", false
	}
	var cur string
	var isMongo bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "SERVICE_NAME:"); ok {
			cur = strings.TrimSpace(rest)
			isMongo = strings.Contains(strings.ToLower(cur), "mongo")
			continue
		}
		if rest, ok := strings.CutPrefix(line, "DISPLAY_NAME:"); ok {
			if strings.Contains(strings.ToLower(rest), "mongo") {
				isMongo = true
			}
			continue
		}
		if !isMongo || cur == "" {
			continue
		}
		if m := stateRe.FindStringSubmatch(line); m != nil {
			return cur, m[1], true
		}
	}
	return "", "", false
}

// banner is what someone running Schedule from a terminal sees. The Windows
// build has no console, so there it goes nowhere.
func banner(url, store string) {
	fmt.Printf("\n  Schedule is running.\n\n")
	fmt.Printf("    Address   %s\n", url)
	fmt.Printf("    Database  %s\n\n", store)
	fmt.Printf("  Closing the window leaves it in the notification area. Choose\n")
	fmt.Printf("  Quit from its icon there, or press Ctrl+C here, to stop it.\n\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// startedAtLogin reports whether Windows started Schedule at sign-in, which
// the Run entry marks with -tray.
func startedAtLogin() bool {
	for _, a := range os.Args[1:] {
		if a == "-tray" || a == "--tray" {
			return true
		}
	}
	return false
}

// wantsBrowser reports whether the user asked for a plain browser tab instead
// of the app window, with "schedule.exe -browser".
func wantsBrowser() bool {
	for _, a := range os.Args[1:] {
		if a == "-browser" || a == "--browser" {
			return true
		}
	}
	return false
}

// chromePaths lists where a Chromium-based browser usually lives. Edge ships
// with Windows 11, so on that platform there is nearly always a hit.
func chromePaths() []string {
	if runtime.GOOS != "windows" {
		return []string{"google-chrome-stable", "google-chrome", "chromium",
			"chromium-browser", "microsoft-edge", "brave-browser"}
	}
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LocalAppData"),
	}
	// Chrome first because that is what most people here already use; Edge
	// second because it is always present on Windows 11.
	var out []string
	for _, exe := range []string{
		`Google\Chrome\Application\chrome.exe`,
		`Microsoft\Edge\Application\msedge.exe`,
	} {
		for _, root := range roots {
			if root != "" {
				out = append(out, filepath.Join(root, exe))
			}
		}
	}
	return out
}

// findChrome returns a runnable Chromium-based browser, or "".
func findChrome() string {
	for _, c := range chromePaths() {
		if strings.ContainsAny(c, `/\`) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// openUI puts Schedule on screen. Preferred: a Chromium browser in --app mode,
// which is a plain window with no tabs, no address bar and its own taskbar
// entry, so it reads as a desktop app rather than a web page. The returned
// command is that window, and closing it should stop the program; nil means we
// fell back to the default browser and there is no window to watch.
func openUI(url string) *exec.Cmd {
	if wantsBrowser() {
		openInBrowser(url)
		return nil
	}
	if chrome := findChrome(); chrome != "" {
		profile := filepath.Join(appDir, "window")
		cmd := exec.Command(chrome,
			"--app="+url,
			"--user-data-dir="+profile,
			"--window-size=1280,860",
			"--no-first-run",
			"--no-default-browser-check",
			"--disable-features=Translate,MediaRouter",
		)
		if err := cmd.Start(); err == nil {
			return cmd
		} else {
			log.Printf("could not open the app window (%v), falling back to the browser", err)
		}
	}
	openInBrowser(url)
	return nil
}

// openInBrowser is the fallback: hand the URL to whatever the user has set as
// their default browser. It opens as an ordinary tab.
func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open a browser (%v) - open %s yourself", err, url)
		return
	}
	go cmd.Wait() // reap the child so it does not linger as a zombie
}

/* ---------- routing ---------- */

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	files := http.FileServer(http.FS(sub))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, sub, "schedule.html")
			return
		}
		files.ServeHTTP(w, r)
	})

	// The window's taskbar button uses the page's icon, so match the exe's.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Write(trayIcon)
	})

	mux.HandleFunc("/api/tasks", s.tasksCollection)
	mux.HandleFunc("/api/tasks/", s.taskItem)
	mux.HandleFunc("/api/recategorize", s.recategorizeHandler)
	mux.HandleFunc("/api/prefs", s.prefsHandler)
	mux.HandleFunc("/api/carry", s.carryHandler)
	mux.HandleFunc("/api/quit", s.quitHandler)
	mux.HandleFunc("/api/show", s.showHandler)
	mux.HandleFunc("/api/alert", s.alertHandler)
	mux.HandleFunc("/api/info", s.infoHandler)
	mux.HandleFunc("/api/export", s.exportHandler)
	mux.HandleFunc("/api/import", s.importHandler)
	mux.HandleFunc("/api/update", s.updateHandler)
	mux.HandleFunc("/api/update/install", s.installHandler)
	mux.HandleFunc("/api/reveal", s.revealHandler)

	return s.checkOrigin(s.withRev(mux))
}

// checkOrigin rejects cross-origin requests. A missing Origin header is fine:
// that is a plain navigation or a curl call, not a browser cross-site request.
func (s *server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			ok := false
			for _, allowed := range s.origin {
				if o == allowed {
					ok = true
					break
				}
			}
			if !ok {
				fail(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

/* ---------- helpers ---------- */

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// dbFail reports a store error. A conflict - the data file changed under
// Schedule - is 409, so the page can tell the user what happened rather than
// "could not save".
func dbFail(w http.ResponseWriter, err error) {
	if errors.Is(err, errConflict) {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	fail(w, http.StatusInternalServerError, err.Error())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

var validStatus = map[string]bool{"todo": true, "doing": true, "done": true, "notdone": true}

var validKind = map[string]bool{"note": true, "carry": true, "move": true}

func validate(t Task) string {
	if strings.TrimSpace(t.ID) == "" {
		return "id is required"
	}
	if strings.TrimSpace(t.Title) == "" {
		return "title is required"
	}
	if len(t.Title) > maxTitleLen {
		return "title is too long"
	}
	start, err := time.Parse(day, t.Date)
	if err != nil {
		return "date must be YYYY-MM-DD"
	}
	// A task runs from date to end inclusive. The two are equal for the
	// ordinary one-day task, so there is no separate kind to reason about.
	end, err := time.Parse(day, t.End)
	if err != nil {
		return "end must be YYYY-MM-DD"
	}
	if end.Before(start) {
		return "the last day cannot be before the first day"
	}
	if end.Sub(start) > maxSpanDays*24*time.Hour {
		return fmt.Sprintf("a task cannot run for more than %d days", maxSpanDays+1)
	}
	if t.Category < 0 || t.Category > maxCategoryID {
		return "category is not valid"
	}
	if !validStatus[t.Status] {
		return "status must be one of todo, doing, done, notdone"
	}
	if len(t.Log) > maxLogLen {
		return fmt.Sprintf("a task cannot hold more than %d history entries", maxLogLen)
	}
	if !validRepeat[t.Repeat] {
		return "repeat must be one of daily, weekdays, weekly, monthly, or empty"
	}
	if t.Repeat != "" && t.Series != "" {
		return "a repeating task cannot itself be part of a series"
	}
	if t.RepeatedThrough != "" {
		if _, err := time.Parse(day, t.RepeatedThrough); err != nil {
			return "repeatedThrough must be YYYY-MM-DD"
		}
	}
	for i, e := range t.Log {
		if msg := validateEntry(e); msg != "" {
			return fmt.Sprintf("history entry %d: %s", i+1, msg)
		}
	}
	return ""
}

func validateCategories(cats []Category) string {
	if len(cats) == 0 {
		return "there must be at least one category"
	}
	if len(cats) > maxCategories {
		return fmt.Sprintf("there can be at most %d categories", maxCategories)
	}
	seen := map[int]bool{}
	for _, c := range cats {
		if c.ID < 0 || c.ID > maxCategoryID || seen[c.ID] {
			return "category ids must be unique numbers"
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Name) == "" {
			return "a category needs a name"
		}
		if len(c.Name) > maxCategoryName {
			return "a category name is too long"
		}
	}
	return ""
}

func validateEntry(e Entry) string {
	if !validKind[e.Kind] {
		return "kind must be one of note, carry, move"
	}
	if _, err := time.Parse(day, e.Date); err != nil {
		return "date must be YYYY-MM-DD"
	}
	if e.From != "" {
		if _, err := time.Parse(day, e.From); err != nil {
			return "from must be YYYY-MM-DD"
		}
	}
	if e.Status != "" && !validStatus[e.Status] {
		return "status must be one of todo, doing, done, notdone"
	}
	if len(e.Text) > maxEntryText {
		return "comment is too long"
	}
	// A carry or move is a fact on its own; a note with nothing in it is not.
	if e.Kind == "note" && strings.TrimSpace(e.Text) == "" {
		return "a comment cannot be empty"
	}
	return ""
}

/* ---------- handlers ---------- */

func (s *server) tasksCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.db.ListTasks()
		if err != nil {
			dbFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tasks)

	case http.MethodPost:
		var t Task
		if !decode(w, r, &t) {
			return
		}
		if msg := validate(t); msg != "" {
			fail(w, http.StatusBadRequest, msg)
			return
		}
		if err := s.db.InsertTask(t); errors.Is(err, errDuplicate) {
			fail(w, http.StatusConflict, err.Error())
			return
		} else if err != nil {
			dbFail(w, err)
			return
		}
		stored, err := s.db.GetTask(t.ID)
		if err != nil {
			dbFail(w, err)
			return
		}
		s.changed(w)
		writeJSON(w, http.StatusCreated, stored)

	case http.MethodDelete:
		if err := s.db.DeleteAllTasks(); err != nil {
			dbFail(w, err)
			return
		}
		s.changed(w)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) taskItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusNotFound, "no such task")
		return
	}

	switch r.Method {
	case http.MethodGet:
		t, err := s.db.GetTask(id)
		if errors.Is(err, errNoRow) {
			fail(w, http.StatusNotFound, "no such task")
			return
		} else if err != nil {
			dbFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, t)

	case http.MethodPut:
		var t Task
		if !decode(w, r, &t) {
			return
		}
		if t.ID == "" {
			t.ID = id
		}
		if t.ID != id {
			fail(w, http.StatusBadRequest, "id in the body does not match the URL")
			return
		}
		if msg := validate(t); msg != "" {
			fail(w, http.StatusBadRequest, msg)
			return
		}
		if err := s.db.ReplaceTask(id, t); errors.Is(err, errNoRow) {
			fail(w, http.StatusNotFound, "no such task")
			return
		} else if err != nil {
			dbFail(w, err)
			return
		}
		s.changed(w)
		writeJSON(w, http.StatusOK, t)

	case http.MethodDelete:
		if err := s.db.DeleteTask(id); errors.Is(err, errNoRow) {
			fail(w, http.StatusNotFound, "no such task")
			return
		} else if err != nil {
			dbFail(w, err)
			return
		}
		s.changed(w)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) prefsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p, err := s.db.GetPrefs()
		if err != nil {
			dbFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodPut:
		var p Prefs
		if !decode(w, r, &p) {
			return
		}
		if !validDeadline(p.Deadline) {
			fail(w, http.StatusBadRequest, "deadline must be HH:MM or empty")
			return
		}
		if p.DayStart < 0 || p.DayStart > 23 {
			fail(w, http.StatusBadRequest, "dayStart must be an hour from 0 to 23")
			return
		}
		if msg := validateCategories(p.Categories); msg != "" {
			fail(w, http.StatusBadRequest, msg)
			return
		}
		if err := s.db.SetPrefs(p); err != nil {
			dbFail(w, err)
			return
		}
		s.recheckDeadline()
		s.changed(w)
		writeJSON(w, http.StatusOK, p)

	default:
		w.Header().Set("Allow", "GET, PUT")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// carryHandler rolls unfinished work from earlier days onto today, extends
// every repeating task up to the horizon, and answers with the whole board,
// so the UI needs one call on start-up rather than a carry followed by a
// fetch. Carrying does nothing unless it is switched on in Settings, and the
// whole thing is harmless to call repeatedly: once a task is dated today
// there is nothing left to move, and an occurrence that exists is not made
// again.
func (s *server) carryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	p, err := s.db.GetPrefs()
	if err != nil {
		dbFail(w, err)
		return
	}

	now := time.Now()
	today := logicalDay(now, p.DayStart)
	moved := 0
	if p.Carry {
		if moved, err = s.db.CarryForward(today, now.Format(stamp)); err != nil {
			dbFail(w, err)
			return
		}
	}
	added, err := s.db.AddRepeats(today, repeatHorizon(today), now.Format(stamp))
	if err != nil {
		dbFail(w, err)
		return
	}
	if moved > 0 || added > 0 {
		s.changed(w)
	}

	tasks, err := s.db.ListTasks()
	if err != nil {
		dbFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved, "added": added, "tasks": tasks})
}

func (s *server) quitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.stopping()
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Let the response reach the browser before the listener closes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		select {
		case <-s.quit:
		default:
			close(s.quit)
		}
	}()
}

// recategorizeHandler moves tasks to another category in one go: the tasks of
// a deleted row, or those same tasks back again on Undo.
func (s *server) recategorizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		IDs      []string `json:"ids"`
		Category int      `json:"category"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Category < 0 || body.Category > maxCategoryID {
		fail(w, http.StatusBadRequest, "category is not valid")
		return
	}
	if len(body.IDs) > 0 {
		if err := s.db.SetCategory(body.IDs, body.Category); err != nil {
			dbFail(w, err)
			return
		}
		s.changed(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// infoHandler tells the page where tasks are kept, for Settings.
func (s *server) infoHandler(w http.ResponseWriter, r *http.Request) {
	_, isFile := s.db.(*fileStore)
	writeJSON(w, http.StatusOK, map[string]any{"database": s.db.Label(), "file": isFile})
}

// revealHandler opens the folder the data file is in, with the file selected,
// so a backup is a copy away.
func (s *server) revealHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fs, ok := s.db.(*fileStore)
	if !ok {
		fail(w, http.StatusNotFound, "tasks are not kept in a file")
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,"+fs.path)
	case "darwin":
		cmd = exec.Command("open", "-R", fs.path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(fs.path))
	}
	if err := cmd.Start(); err != nil {
		dbFail(w, err)
		return
	}
	go cmd.Wait()
	w.WriteHeader(http.StatusNoContent)
}

// trayHint says, once ever, that closing the window left Schedule running.
// Without it the app seems to vanish the first time.
func trayHint(dir string) {
	marker := filepath.Join(dir, "tray-hint-shown")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if notify("Schedule is still running",
		"It's in the notification area by the clock. Click its icon to open it again, "+
			"or right-click it and choose Quit.") {
		os.WriteFile(marker, nil, 0o644)
	}
}

// showHandler is how a second start of Schedule hands over to this one: it
// brings the window up here and then exits.
func (s *server) showHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	go s.show()
	writeJSON(w, http.StatusOK, map[string]string{"status": "shown"})
}
