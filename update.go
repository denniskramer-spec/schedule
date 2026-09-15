package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

/* ---------- updates ----------

   Schedule asks a web address for a small file that names the newest version:

       {"version": "2.8", "file": "Schedule-Setup.exe", "sha256": "...", "notes": "What changed"}

   "file" is resolved against that address, so both files sit in one folder
   on any web host - GitHub Releases, a static site, a shared drive over
   https. ./build.sh installer writes latest.json next to Schedule-Setup.exe
   ready to upload, sha256 included.

   Nothing runs unverified: both addresses must be https (plain http is only
   allowed to this machine, for tests), and the downloaded setup has to hash
   to the sha256 latest.json names, or it is deleted and reported instead of
   started.

   The check runs shortly after start and once a day after that. A newer
   version is downloaded and verified straight away, then installed without
   anyone clicking anything: at once if the window is closed to the tray,
   otherwise the moment the window is closed, or at the next start. Setup
   runs silently, replaces this copy and starts it again. Settings shows what
   is going on, and Update now installs immediately instead of waiting. */

// version is stamped in by the build ("-X main.version=2.8"); a bare
// "go build" gets "dev", which never updates.
var version = "dev"

// defaultUpdateURL is where latest.json is published. The build stamps it in
// from UPDATE_URL ("./build.sh installer" with UPDATE_URL set, or -X
// main.defaultUpdateURL=...), or set it here; SCHEDULE_UPDATE_URL overrides
// it per machine. Empty turns the check off.
var defaultUpdateURL = ""

const (
	updateFirstCheck = 30 * time.Second
	updateEvery      = 24 * time.Hour
)

type updateInfo struct {
	Version string `json:"version"`
	File    string `json:"file"`
	Sha256  string `json:"sha256"` // hex digest of the setup file
	Notes   string `json:"notes"`
	URL     string `json:"url"` // the resolved download address
}

type updater struct {
	mu        sync.Mutex
	latest    *updateInfo
	checkedAt time.Time
	err       string
	status    string  // "", "downloading", "ready", "installing"
	ready     *staged // a verified setup waiting to be run
}

// staged is a downloaded, verified setup. It is also written to a file next
// to the log, so a start-up can finish an install the previous run only
// downloaded.
type staged struct {
	Version string
	Sha256  string
	Path    string
}

func stagedPath() string { return filepath.Join(appDir, "update-ready") }

func (st *staged) save() {
	os.WriteFile(stagedPath(), []byte(st.Version+"\n"+st.Sha256+"\n"+st.Path+"\n"), 0o644)
}

// loadStaged reads the setup a previous run left ready, if it is still
// there, still newer than this version and still hashes as it should.
func loadStaged() *staged {
	b, err := os.ReadFile(stagedPath())
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(parts) != 3 {
		return nil
	}
	st := &staged{Version: parts[0], Sha256: parts[1], Path: parts[2]}
	if !newer(st.Version, version) {
		return nil
	}
	if sum, err := fileSha256(st.Path); err != nil || sum != st.Sha256 {
		return nil
	}
	return st
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func updateURL() string {
	if v := os.Getenv("SCHEDULE_UPDATE_URL"); v != "" {
		return v
	}
	return defaultUpdateURL
}

// newer reports whether a is a higher dotted version than b: "2.10" > "2.9".
func newer(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(strings.TrimSpace(as[i]))
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(strings.TrimSpace(bs[i]))
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// secureURL accepts https anywhere, and http only to this machine, so a
// setup program is never fetched over a link someone on the network could
// tamper with. The loopback exception is for the tests.
func secureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("the update address must use https, not http (%s)", raw)
	}
	return fmt.Errorf("the update address must be an https link (%s)", raw)
}

// fetchLatest reads latest.json and resolves the download address.
func fetchLatest(ctx context.Context) (*updateInfo, error) {
	u := updateURL()
	if u == "" {
		return nil, errors.New("no update address is set")
	}
	if err := secureURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Schedule/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the update address answered %s", resp.Status)
	}
	var info updateInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return nil, fmt.Errorf("latest.json is not readable: %v", err)
	}
	if info.Version == "" {
		return nil, errors.New("latest.json names no version")
	}
	if info.File == "" {
		info.File = "Schedule-Setup.exe"
	}
	base, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(info.File)
	if err != nil {
		return nil, err
	}
	info.URL = base.ResolveReference(ref).String()
	if err := secureURL(info.URL); err != nil {
		return nil, err
	}
	info.Sha256 = strings.ToLower(strings.TrimSpace(info.Sha256))
	return &info, nil
}

// check asks for the latest version and remembers the answer.
func (s *server) checkUpdate() (*updateInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := fetchLatest(ctx)
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	s.upd.checkedAt = time.Now()
	if err != nil {
		s.upd.err = err.Error()
		return nil, err
	}
	s.upd.err = ""
	s.upd.latest = info
	return info, nil
}

// watchUpdates checks on a schedule. A newer version is downloaded and
// verified, then installed on its own: see autoInstall.
func (s *server) watchUpdates() {
	if version == "dev" || updateURL() == "" {
		return
	}
	if st := loadStaged(); st != nil {
		s.upd.mu.Lock()
		s.upd.ready, s.upd.status = st, "ready"
		s.upd.mu.Unlock()
	}
	time.Sleep(updateFirstCheck)
	for {
		if info, err := s.checkUpdate(); err != nil {
			log.Printf("update check: %v", err)
		} else if newer(info.Version, version) {
			log.Printf("Schedule %s is available (this is %s)", info.Version, version)
			if err := s.stage(info); err != nil {
				log.Printf("update: %v", err)
			} else {
				s.autoInstall()
			}
		}
		time.Sleep(updateEvery)
	}
}

// stage downloads and verifies the setup, unless that version is already
// waiting. Only one download runs at a time.
func (s *server) stage(info *updateInfo) error {
	s.upd.mu.Lock()
	if s.upd.ready != nil && s.upd.ready.Version == info.Version {
		s.upd.mu.Unlock()
		return nil
	}
	if s.upd.status == "downloading" || s.upd.status == "installing" {
		s.upd.mu.Unlock()
		return errors.New("an update is already on its way")
	}
	s.upd.status, s.upd.err = "downloading", ""
	s.upd.mu.Unlock()

	path, err := download(info.URL, info.Sha256)
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	if err != nil {
		s.upd.status, s.upd.err = "", "Could not download the update: "+err.Error()
		return err
	}
	st := &staged{Version: info.Version, Sha256: info.Sha256, Path: path}
	st.save()
	s.upd.ready, s.upd.status = st, "ready"
	log.Printf("Schedule %s downloaded and verified: %s", st.Version, path)
	return nil
}

// autoInstall installs the waiting setup if that would not pull the window
// out from under someone: now if the window is closed, otherwise when it is
// (the window's close hook calls this again), or at the next start.
func (s *server) autoInstall() {
	s.upd.mu.Lock()
	st := s.upd.ready
	s.upd.mu.Unlock()
	if st == nil {
		return
	}
	if s.windowOpen != nil && s.windowOpen() {
		if !updateSeen(st.Version) {
			markUpdateSeen(st.Version)
			if s.canNotify {
				notify("Schedule "+st.Version+" is ready",
					"It installs on its own when you close the window.")
			}
		}
		return
	}
	if err := s.installReady(true); err != nil {
		log.Printf("update: %v", err)
	}
}

// installReady runs the waiting setup silently. It closes this copy,
// replaces it and starts it again - in the tray if that is where it was.
func (s *server) installReady(quiet bool) error {
	s.upd.mu.Lock()
	st := s.upd.ready
	if st == nil {
		s.upd.mu.Unlock()
		return errors.New("no update is downloaded")
	}
	if s.upd.status == "installing" {
		s.upd.mu.Unlock()
		return errors.New("the update is already installing")
	}
	if runtime.GOOS != "windows" {
		s.upd.status, s.upd.err = "", "Downloaded to "+st.Path+"; run it yourself on this platform."
		s.upd.mu.Unlock()
		return nil
	}
	s.upd.status = "installing"
	s.upd.mu.Unlock()

	args := []string{"-auto"}
	if s.windowOpen == nil || !s.windowOpen() {
		args = append(args, "-tray")
	}
	if quiet {
		log.Printf("installing Schedule %s on its own", st.Version)
	} else {
		log.Printf("installing Schedule %s", st.Version)
	}
	os.Remove(stagedPath()) // if Setup fails, the next check stages it again
	cmd := exec.Command(st.Path, args...)
	if err := cmd.Start(); err != nil {
		s.upd.mu.Lock()
		s.upd.status, s.upd.err = "ready", "Could not start Setup: "+err.Error()
		s.upd.mu.Unlock()
		return err
	}
	// Get out of Setup's way at once rather than waiting to be asked: Setup
	// still asks, and then waits until this copy has really gone before it
	// starts the new one, so the two never overlap. Setup takes seconds, so
	// one that is gone within the first moment did not get to work - most
	// likely Windows Security stopped it - and that is reported instead.
	died := make(chan error, 1)
	go func() { died <- cmd.Wait() }()
	go func() {
		select {
		case err := <-died:
			msg := "Setup stopped as soon as it started"
			if err != nil {
				msg += " (" + err.Error() + ")"
			}
			msg += ". Windows Security may have blocked it: check Protection history, then try Update now again."
			log.Print("update: " + msg)
			s.upd.mu.Lock()
			s.upd.status, s.upd.err = "ready", msg
			s.upd.mu.Unlock()
			st.save() // keep it staged: the marker was removed above
		case <-time.After(1500 * time.Millisecond):
			log.Print("quitting for the update")
			s.requestQuit()
		}
	}()
	return nil
}

// installAtStart finishes an install the previous run downloaded but could
// not do because the window was open. It runs before anything else is set
// up; true means Setup is taking over and this process should exit.
func installAtStart(tray bool) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	st := loadStaged()
	if st == nil {
		return false
	}
	os.Remove(stagedPath())
	args := []string{"-auto"}
	if tray {
		args = append(args, "-tray")
	}
	log.Printf("installing Schedule %s before starting", st.Version)
	if err := exec.Command(st.Path, args...).Start(); err != nil {
		log.Printf("could not start Setup: %v", err)
		return false
	}
	return true
}

// The version already announced, so the notification comes once.
func updateSeenPath() string { return filepath.Join(appDir, "update-seen") }

func updateSeen(v string) bool {
	b, err := os.ReadFile(updateSeenPath())
	return err == nil && strings.TrimSpace(string(b)) == v
}

func markUpdateSeen(v string) { os.WriteFile(updateSeenPath(), []byte(v), 0o644) }

// installUpdate is Update now: install the waiting setup at once, or
// download it first if the check found one that is not staged yet.
func (s *server) installUpdate() error {
	s.upd.mu.Lock()
	info, st, status := s.upd.latest, s.upd.ready, s.upd.status
	s.upd.mu.Unlock()
	if st == nil && info == nil {
		return errors.New("check for updates first")
	}
	if status == "downloading" || status == "installing" {
		return errors.New("an update is already on its way")
	}
	go func() {
		if st == nil || (info != nil && newer(info.Version, st.Version)) {
			if err := s.stage(info); err != nil {
				log.Printf("update: %v", err)
				return
			}
		}
		if err := s.installReady(false); err != nil {
			log.Printf("update: %v", err)
		}
	}()
	return nil
}

// download fetches a file into the temporary folder, whole, checks that it
// hashes to what latest.json promised, then renames it into place, so a
// half-downloaded or tampered-with setup is never run.
func download(u, wantSha string) (string, error) {
	if len(wantSha) != sha256.Size*2 {
		return "", errors.New("latest.json names no sha256 for the setup, so it cannot be verified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", u, resp.Status)
	}
	name := filepath.Base(u)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	if name == "" || name == "." || name == "/" {
		name = "Schedule-Setup.exe"
	}
	dir := filepath.Join(os.TempDir(), "Schedule-update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, name)
	tmp := final + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSha {
		os.Remove(tmp)
		return "", fmt.Errorf("the downloaded setup does not match latest.json (sha256 %s, expected %s)", got[:12], wantSha[:12])
	}
	os.Remove(final)
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}

/* ---------- handlers ---------- */

func (s *server) updateState() map[string]any {
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	out := map[string]any{
		"version":   version,
		"enabled":   version != "dev" && updateURL() != "",
		"status":    s.upd.status,
		"error":     s.upd.err,
		"checkedAt": "",
	}
	if !s.upd.checkedAt.IsZero() {
		out["checkedAt"] = s.upd.checkedAt.Format(stamp)
	}
	if s.upd.latest != nil {
		out["latest"] = s.upd.latest.Version
		out["notes"] = s.upd.latest.Notes
		out["url"] = s.upd.latest.URL
		out["newer"] = newer(s.upd.latest.Version, version)
	}
	if s.upd.ready != nil {
		out["ready"] = s.upd.ready.Version
		out["windowOpen"] = s.windowOpen != nil && s.windowOpen()
	}
	return out
}

// updateHandler: GET reports what is known; POST checks now.
func (s *server) updateHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if version == "dev" || updateURL() == "" {
			fail(w, http.StatusBadRequest, "updates are not set up for this build")
			return
		}
		s.checkUpdate() // the outcome is in the state either way
	default:
		w.Header().Set("Allow", "GET, POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.updateState())
}

func (s *server) installHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := s.installUpdate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.updateState())
}
