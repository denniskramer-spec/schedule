//go:build uitest

// Package uitest drives the real Schedule program in a headless Chrome, the
// way a person would: it builds the app, starts it against a throwaway data
// file, and clicks, drags and types on the page.
//
//	./build.sh uitest        (or: go test -tags uitest ./uitest/)
//
// It needs a Chromium-based browser on the machine; set CHROME to point at
// one if it is not found. Set UITEST_SHOTS to a folder to keep screenshots.
package uitest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

/* ---------- the app under test ---------- */

var (
	buildOnce sync.Once
	appPath   string
	buildErr  error
)

// build compiles the program once for all tests.
func build(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "schedule-uitest-")
		if err != nil {
			buildErr = err
			return
		}
		appPath = filepath.Join(dir, "schedule")
		if runtime.GOOS == "windows" {
			appPath += ".exe"
		}
		// A real version number, so the update check is live in the tests.
		cmd := exec.Command("go", "build", "-ldflags", "-X main.version=1.0", "-o", appPath, "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return appPath
}

type app struct {
	base string // http://127.0.0.1:port
	data string // the data file
	home string // its config folder's parent
	cmd  *exec.Cmd
}

// start runs the app with no window on a free port, against the given data
// file (a fresh one if empty), and waits until it answers.
func start(t *testing.T, data string, env ...string) *app {
	t.Helper()
	exe := build(t)
	if data == "" {
		data = filepath.Join(t.TempDir(), "schedule.json")
	}
	cmd := exec.Command(exe, "-noui", "-addr", "127.0.0.1:0", "-data", data)
	// Keep the app's own folder (log, profile) out of the real one.
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+home, "LocalAppData="+home, "AppData="+home, "HOME="+home,
		// Never the real release address: a test must not fetch, let alone
		// install, a real update. Tests that want a check point this elsewhere.
		"SCHEDULE_UPDATE_URL=http://127.0.0.1:1/latest.json")
	cmd.Env = append(cmd.Env, env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &app{data: data, home: home, cmd: cmd}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("app stderr:\n%s", stderr.String())
		}
	})

	// The banner names the address it actually got.
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if rest, ok := strings.CutPrefix(line, "Address"); ok {
				found <- strings.TrimSpace(rest)
			}
		}
		go io.Copy(io.Discard, stdout)
	}()
	select {
	case a.base = <-found:
	case <-time.After(15 * time.Second):
		t.Fatalf("the app did not start:\n%s", stderr.String())
	}
	return a
}

func (a *app) call(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// alertDay polls the alert endpoint: the day waiting to be closed out, or "".
func (a *app) alertDay(t *testing.T) string {
	t.Helper()
	var r struct {
		Day string `json:"day"`
	}
	json.Unmarshal([]byte(a.must(t, "POST", "/api/alert", nil)), &r)
	return r.Day
}

func (a *app) must(t *testing.T, method, path string, body any) string {
	t.Helper()
	code, out := a.call(t, method, path, body)
	if code >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

type task map[string]any

func today(offset int) string {
	return time.Now().AddDate(0, 0, offset).Format("2006-01-02")
}

func mk(id, title, status string, cat int, from, to int) task {
	return task{"id": id, "title": title, "date": today(from), "end": today(to), "status": status,
		"agent": false, "category": cat, "log": []any{}, "createdAt": today(from) + " 09:00", "finishedAt": ""}
}

func (a *app) seed(t *testing.T, tasks ...task) {
	t.Helper()
	for _, x := range tasks {
		a.must(t, "POST", "/api/tasks", x)
	}
}

/* ---------- the browser ---------- */

func findChrome() string {
	if v := os.Getenv("CHROME"); v != "" {
		return v
	}
	names := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "msedge"}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			for _, rel := range []string{`Google\Chrome\Application\chrome.exe`, `Microsoft\Edge\Application\msedge.exe`} {
				if p := filepath.Join(root, rel); root != "" && exists(p) {
					return p
				}
			}
		}
	}
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// browser opens a headless Chrome on the app's page and hands back a context
// to drive it with.
func browser(t *testing.T, a *app) context.Context {
	t.Helper()
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chromium-based browser found; set CHROME to point at one")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome), chromedp.WindowSize(1400, 900))
	actx, cancel1 := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel2 := chromedp.NewContext(actx)
	ctx, cancel3 := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(func() { cancel3(); cancel2(); cancel1() })
	if err := chromedp.Run(ctx,
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "light"}}),
		chromedp.Navigate(a.base),
	); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func run(t *testing.T, ctx context.Context, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}
}

func pause(ms int) chromedp.Action { return chromedp.Sleep(time.Duration(ms) * time.Millisecond) }

// shot saves a screenshot when UITEST_SHOTS names a folder.
func shot(name string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		dir := os.Getenv("UITEST_SHOTS")
		if dir == "" {
			return nil
		}
		var b []byte
		if err := chromedp.CaptureScreenshot(&b).Do(ctx); err != nil {
			return err
		}
		os.MkdirAll(dir, 0o755)
		return os.WriteFile(filepath.Join(dir, name+".png"), b, 0o644)
	})
}

// rightClick opens the card menu the way a mouse does.
func rightClick(sel string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var xy []float64
		js := `(()=>{const r=document.querySelector(` + fmt.Sprintf("%q", sel) + `).getBoundingClientRect();return [r.left+20,r.top+10]})()`
		if err := chromedp.Evaluate(js, &xy).Do(ctx); err != nil {
			return err
		}
		input.DispatchMouseEvent(input.MouseMoved, xy[0], xy[1]).Do(ctx)
		if err := input.DispatchMouseEvent(input.MousePressed, xy[0], xy[1]).WithButton(input.Right).WithButtons(2).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		return input.DispatchMouseEvent(input.MouseReleased, xy[0], xy[1]).WithButton(input.Right).WithClickCount(1).Do(ctx)
	})
}

// drag moves a card from one selector to a point inside another.
func drag(from, to string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var a, b []float64
		q := func(sel string, out *[]float64) error {
			js := `(()=>{const r=document.querySelector(` + fmt.Sprintf("%q", sel) + `).getBoundingClientRect();return [r.left+30,r.top+12]})()`
			return chromedp.Evaluate(js, out).Do(ctx)
		}
		if err := q(from, &a); err != nil {
			return err
		}
		if err := q(to, &b); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MousePressed, a[0], a[1]).WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		for i := 1; i <= 10; i++ {
			f := float64(i) / 10
			if err := input.DispatchMouseEvent(input.MouseMoved, a[0]+(b[0]-a[0])*f, a[1]+(b[1]-a[1])*f).WithButton(input.Left).WithButtons(1).Do(ctx); err != nil {
				return err
			}
		}
		return input.DispatchMouseEvent(input.MouseReleased, b[0], b[1]).WithButton(input.Left).WithClickCount(1).Do(ctx)
	})
}

func setInput(sel, value string) chromedp.Action {
	js := `(()=>{const b=document.querySelector(` + fmt.Sprintf("%q", sel) + `);b.value=` + fmt.Sprintf("%q", value) +
		`;b.dispatchEvent(new Event("change",{bubbles:true}));return 1})()`
	return chromedp.Evaluate(js, nil)
}

/* ---------- the tests ---------- */

func TestBoard(t *testing.T) {
	a := start(t, "")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": false, "carry": false, "deadline": "", "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}, {"id": 1, "name": "AI training"}, {"id": 2, "name": "Assets"}}})
	a.seed(t,
		mk("b", "Client call", "todo", 0, 0, 0),
		mk("c", "Label dataset batch 4", "doing", 1, -1, 3),
		mk("d", "Review model output", "todo", 1, 0, 0),
		mk("e", "Export logo files", "notdone", 2, 0, 0),
		mk("f", "Yesterday's invoice", "todo", 0, -1, -1),
		mk("x1", "Asset item 1", "todo", 2, 0, 0),
		mk("x2", "Asset item 2", "todo", 2, 0, 0),
	)
	ctx := browser(t, a)

	// A day left open comes up for closing out first. What is typed is kept
	// on Later: the running task's line is saved, the due task's is held.
	var s string
	run(t, ctx,
		chromedp.WaitVisible("#wrap.on", chromedp.ByQuery),
		chromedp.SendKeys(`[data-run="c"]`, "Half of batch 4 labelled", chromedp.ByQuery),
		chromedp.SendKeys(`[data-note="f"]`, "Draft about the invoice", chromedp.ByQuery),
		shot("closeout"),
		chromedp.Click("#wLater", chromedp.ByQuery),
		pause(600),
		shot("board"),
	)
	if s = a.must(t, "GET", "/api/tasks/c", nil); !strings.Contains(s, "Half of batch 4 labelled") {
		t.Errorf("Later did not save the running comment: %s", s)
	}

	// Category rows fold and show what they hold.
	var cells, lanes int
	run(t, ctx,
		chromedp.Click(`[data-lane="0"]`, chromedp.ByQuery), pause(300),
		chromedp.Evaluate(`document.querySelectorAll('[data-cat="0"]').length`, &cells),
		chromedp.Evaluate(`document.querySelectorAll('.lane').length`, &lanes),
		shot("row-folded"),
		chromedp.Click(`[data-lane="0"]`, chromedp.ByQuery), pause(300),
	)
	if cells != 0 || lanes != 3 {
		t.Errorf("folded row: %d cells shown (want 0), %d lanes (want 3)", cells, lanes)
	}

	// The right-click menu sets the column and the category.
	run(t, ctx,
		rightClick(`.card[data-id="b"]`), pause(300),
		shot("menu"),
		chromedp.Click(`#menu [data-mstatus="done"]`, chromedp.ByQuery), pause(400),
		rightClick(`.card[data-id="d"]`), pause(300),
		chromedp.Click(`#menu [data-mcat="2"]`, chromedp.ByQuery), pause(400),
	)
	if s = a.must(t, "GET", "/api/tasks/b", nil); !strings.Contains(s, `"status":"done"`) {
		t.Errorf("menu did not set Done: %s", s)
	}
	if s = a.must(t, "GET", "/api/tasks/d", nil); !strings.Contains(s, `"category":2`) {
		t.Errorf("menu did not move the category: %s", s)
	}

	// Dragging a card changes both its column and its row.
	run(t, ctx, drag(`.card[data-id="x1"]`, `[data-col="doing"][data-cat="0"]`), pause(500))
	if s = a.must(t, "GET", "/api/tasks/x1", nil); !strings.Contains(s, `"status":"doing"`) || !strings.Contains(s, `"category":0`) {
		t.Errorf("drag did not move the card: %s", s)
	}

	// Keys on a focused card: 2 = In progress, Delete removes with Undo.
	var undoText string
	run(t, ctx,
		chromedp.Focus(`.card[data-id="d"]`, chromedp.ByQuery), chromedp.KeyEvent("2"), pause(400),
		chromedp.Focus(`.card[data-id="x2"]`, chromedp.ByQuery), chromedp.KeyEvent("\x7f"), pause(400),
		chromedp.Text("#toastText", &undoText, chromedp.ByQuery),
		shot("undo"),
	)
	if s = a.must(t, "GET", "/api/tasks/d", nil); !strings.Contains(s, `"status":"doing"`) {
		t.Errorf("key 2 did not set In progress: %s", s)
	}
	if code, _ := a.call(t, "GET", "/api/tasks/x2", nil); code != 404 {
		t.Errorf("Delete key did not remove the task (got %d)", code)
	}
	if !strings.Contains(undoText, "Asset item 2") {
		t.Errorf("undo bar text: %q", undoText)
	}
	run(t, ctx, chromedp.Click("#toastUndo", chromedp.ByQuery), pause(500))
	if code, _ := a.call(t, "GET", "/api/tasks/x2", nil); code != 200 {
		t.Errorf("Undo did not bring the task back (got %d)", code)
	}

	// A task sheet with unsaved typing stays open on an outside click.
	var open bool
	run(t, ctx,
		chromedp.Click(`.card[data-id="e"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#detail.on", chromedp.ByQuery),
		chromedp.SendKeys("#dNote", "not lost", chromedp.ByQuery),
		chromedp.MouseClickXY(20, 20), pause(300),
		chromedp.Evaluate(`document.getElementById("detail").classList.contains("on")`, &open),
		chromedp.Click("#dCancel", chromedp.ByQuery),
	)
	if !open {
		t.Error("the task sheet closed and dropped an unsaved comment")
	}

	// Week and Month draw; dark mode changes the page's ground.
	var light, dark string
	run(t, ctx,
		chromedp.KeyEvent("w"), pause(300), chromedp.WaitVisible(".weekgrid .lane", chromedp.ByQuery), shot("week"),
		chromedp.KeyEvent("m"), pause(300), chromedp.WaitVisible(".monthgrid", chromedp.ByQuery), shot("month"),
		chromedp.KeyEvent("d"), pause(300),
		chromedp.Evaluate(`getComputedStyle(document.body).backgroundColor`, &light),
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}}),
		pause(400),
		chromedp.Evaluate(`getComputedStyle(document.body).backgroundColor`, &dark),
		shot("dark"),
	)
	if light == dark {
		t.Errorf("dark mode did not change the page: %s", dark)
	}
}

func TestCategories(t *testing.T) {
	a := start(t, "")
	a.seed(t,
		mk("j1", "Client call", "todo", 0, 0, 0),
		mk("a1", "Label batch 4", "doing", 1, 0, 0),
		mk("a2", "Review output", "todo", 1, 0, 0),
		mk("s1", "Logo files", "todo", 2, 0, 0),
	)
	ctx := browser(t, a)

	var lanes int
	run(t, ctx,
		chromedp.WaitVisible(".lane", chromedp.ByQuery),
		chromedp.Click("#openSettings", chromedp.ByQuery), pause(300),
		chromedp.Click("#catAdd", chromedp.ByQuery), pause(300),
		chromedp.Evaluate(`(()=>{const b=document.activeElement;b.value="Personal";b.dispatchEvent(new Event("change",{bubbles:true}));return 1})()`, nil),
		pause(400),
		chromedp.Evaluate(`document.querySelectorAll('.lane').length`, &lanes),
	)
	p := a.must(t, "GET", "/api/prefs", nil)
	if !strings.Contains(p, `{"id":3,"name":"Personal"}`) || lanes != 4 {
		t.Errorf("add: prefs %s, %d lanes", p, lanes)
	}

	var toast string
	run(t, ctx,
		setInput(`[data-catname="2"]`, "Design"), pause(400),
		chromedp.Click(`[data-catdel="1"]`, chromedp.ByQuery), pause(600),
		chromedp.Text("#toastText", &toast, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('.lane').length`, &lanes),
		shot("category-deleted"),
	)
	p = a.must(t, "GET", "/api/prefs", nil)
	if strings.Contains(p, "AI training") || !strings.Contains(p, `"name":"Design"`) || lanes != 3 {
		t.Errorf("rename/delete: prefs %s, %d lanes", p, lanes)
	}
	if s := a.must(t, "GET", "/api/tasks/a1", nil); !strings.Contains(s, `"category":0`) {
		t.Errorf("a deleted row's task should move to the first row: %s", s)
	}
	if !strings.Contains(toast, "2 tasks moved to Job") {
		t.Errorf("undo bar text: %q", toast)
	}

	run(t, ctx, chromedp.Click("#toastUndo", chromedp.ByQuery), pause(700),
		chromedp.Evaluate(`document.querySelectorAll('.lane').length`, &lanes),
		chromedp.Evaluate(`(()=>{const s=document.querySelector('.sheet.settings');s.scrollTop=s.scrollHeight;return 1})()`, nil),
		pause(300), shot("settings-bottom"))
	p = a.must(t, "GET", "/api/prefs", nil)
	if !strings.Contains(p, `{"id":0,"name":"Job"},{"id":1,"name":"AI training"},{"id":2,"name":"Design"}`) || lanes != 4 {
		t.Errorf("undo: prefs %s, %d lanes", p, lanes)
	}
	if s := a.must(t, "GET", "/api/tasks/a1", nil); !strings.Contains(s, `"category":1`) {
		t.Errorf("undo should move the tasks back: %s", s)
	}
}

// The deadline needs no browser: it fires when set to a time already gone,
// once, and not again after a restart on the same data file.
func TestDeadline(t *testing.T) {
	a := start(t, "")
	a.seed(t, mk("q1", "Open job", "doing", 0, 0, 0))
	past := time.Now().Add(-3 * time.Minute).Format("15:04")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": true, "carry": false, "deadline": past, "dayStart": 23,
		"categories": []map[string]any{{"id": 0, "name": "Job"}}})

	var got string
	for i := 0; i < 40; i++ {
		got = a.alertDay(t)
		if got != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got != today(0) {
		t.Fatalf("alert after setting a past deadline: %q, want %q", got, today(0))
	}
	if got = a.alertDay(t); got != "" {
		t.Errorf("the alert should be handed over once: %s", got)
	}

	a.cmd.Process.Kill()
	a.cmd.Wait()
	b := start(t, a.data)
	time.Sleep(2 * time.Second)
	if got = b.alertDay(t); got != "" {
		t.Errorf("the deadline fired again after a restart: %s", got)
	}
}

// Save backup hands out the whole board; Restore takes it back, keeping a
// copy of what it replaced.
func TestBackupRestore(t *testing.T) {
	a := start(t, "")
	a.seed(t, mk("k1", "Kept", "todo", 0, 0, 0), mk("k2", "Also kept", "done", 1, 0, 0))
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": true, "carry": true, "deadline": "18:00", "dayStart": 6,
		"categories": []map[string]any{{"id": 0, "name": "Job"}, {"id": 1, "name": "Side"}}})

	code, backup := a.call(t, "GET", "/api/export", nil)
	if code != 200 || !strings.Contains(backup, `"Also kept"`) || !strings.Contains(backup, `"deadline": "18:00"`) {
		t.Fatalf("export: %d %s", code, backup)
	}

	// Change the board, then put the backup back.
	a.must(t, "DELETE", "/api/tasks", nil)
	a.seed(t, mk("z9", "Newer task", "todo", 0, 0, 0))
	var doc map[string]any
	json.Unmarshal([]byte(backup), &doc)
	out := a.must(t, "POST", "/api/import", doc)
	if !strings.Contains(out, `"tasks":2`) {
		t.Errorf("import answered %s", out)
	}
	list := a.must(t, "GET", "/api/tasks", nil)
	if !strings.Contains(list, `"k1"`) || !strings.Contains(list, `"k2"`) || strings.Contains(list, `"z9"`) {
		t.Errorf("after restore: %s", list)
	}
	if p := a.must(t, "GET", "/api/prefs", nil); !strings.Contains(p, `"deadline":"18:00"`) || !strings.Contains(p, `"name":"Side"`) {
		t.Errorf("settings not restored: %s", p)
	}
	before, _ := filepath.Glob(filepath.Join(filepath.Dir(a.data), "backups", "*before-restore*.json"))
	if len(before) != 1 {
		t.Errorf("want a before-restore copy, got %v", before)
	}

	// Rubbish is refused and changes nothing.
	if code, out := a.call(t, "POST", "/api/import", map[string]any{"hello": "world"}); code != 400 {
		t.Errorf("a non-backup should be refused: %d %s", code, out)
	}
	if code, out := a.call(t, "POST", "/api/import", map[string]any{"tasks": []map[string]any{{"id": "x", "title": "", "date": "2026-01-01", "end": "2026-01-01", "status": "todo"}}}); code != 400 {
		t.Errorf("a backup with an invalid task should be refused: %d %s", code, out)
	}
	if list := a.must(t, "GET", "/api/tasks", nil); !strings.Contains(list, `"k1"`) {
		t.Errorf("a refused restore changed the board: %s", list)
	}
}

// The update check reads latest.json from wherever SCHEDULE_UPDATE_URL points.
func TestUpdateCheck(t *testing.T) {
	setup := []byte("not really an exe")
	sum := sha256.Sum256(setup)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			fmt.Fprintf(w, `{"version": "9.5", "file": "Schedule-Setup.exe", "sha256": "%x", "notes": "Big news"}`, sum)
		case "/bad/latest.json": // the hash does not match the file
			fmt.Fprint(w, `{"version": "9.5", "file": "../Schedule-Setup.exe", "sha256": "`+strings.Repeat("0", 64)+`"}`)
		case "/Schedule-Setup.exe":
			w.Write(setup)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := start(t, "", "SCHEDULE_UPDATE_URL="+srv.URL+"/latest.json")
	st := a.must(t, "GET", "/api/update", nil)
	if !strings.Contains(st, `"version":"1.0"`) || !strings.Contains(st, `"enabled":true`) {
		t.Fatalf("update state before checking: %s", st)
	}
	st = a.must(t, "POST", "/api/update", nil)
	if !strings.Contains(st, `"latest":"9.5"`) || !strings.Contains(st, `"newer":true`) ||
		!strings.Contains(st, `"notes":"Big news"`) || !strings.Contains(st, srv.URL+"/Schedule-Setup.exe") {
		t.Errorf("after checking: %s", st)
	}

	// Install downloads the file; off Windows it stops there and says where.
	a.must(t, "POST", "/api/update/install", nil)
	var last string
	for i := 0; i < 40; i++ {
		last = a.must(t, "GET", "/api/update", nil)
		if !strings.Contains(last, `"status":"downloading"`) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if runtime.GOOS != "windows" && !strings.Contains(last, "Downloaded to") {
		t.Errorf("after install on this platform: %s", last)
	}
	// The verified download is left staged, so a restart can finish the job.
	if !strings.Contains(last, `"ready":"9.5"`) {
		t.Errorf("the downloaded update should be reported as ready: %s", last)
	}
	if _, err := os.Stat(filepath.Join(a.home, "Schedule", "update-ready")); err != nil {
		t.Errorf("no staged-update marker was written: %v", err)
	}

	// A setup that does not hash to what latest.json says is not run.
	c := start(t, "", "SCHEDULE_UPDATE_URL="+srv.URL+"/bad/latest.json")
	c.must(t, "POST", "/api/update", nil)
	c.must(t, "POST", "/api/update/install", nil)
	for i := 0; i < 40; i++ {
		last = c.must(t, "GET", "/api/update", nil)
		if !strings.Contains(last, `"status":"downloading"`) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !strings.Contains(last, "does not match") || strings.Contains(last, `"status":"installing"`) {
		t.Errorf("a setup with the wrong sha256 should be refused: %s", last)
	}

	// An unreachable address is reported, not fatal.
	b := start(t, "", "SCHEDULE_UPDATE_URL=http://127.0.0.1:1/latest.json")
	if st := b.must(t, "POST", "/api/update", nil); !strings.Contains(st, `"error":"Get`) {
		t.Errorf("a failed check should carry an error: %s", st)
	}

	// Plain http off this machine is refused before anything is fetched.
	d := start(t, "", "SCHEDULE_UPDATE_URL=http://example.invalid/latest.json")
	if st := d.must(t, "POST", "/api/update", nil); !strings.Contains(st, "must use https") {
		t.Errorf("an http address should be refused: %s", st)
	}
}

// After the deadline, reminders repeat while tasks stay open and stop once
// the day is closed out.
func TestDeadlineReminders(t *testing.T) {
	a := start(t, "", "SCHEDULE_REMIND_EVERY=2s")
	a.seed(t, mk("r1", "Open job", "todo", 0, 0, 0))
	past := time.Now().Add(-1 * time.Minute).Format("15:04")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": true, "carry": false, "deadline": past, "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}}})

	count := func(window time.Duration) int {
		n := 0
		end := time.Now().Add(window)
		for time.Now().Before(end) {
			if a.alertDay(t) != "" {
				n++
			}
			time.Sleep(200 * time.Millisecond)
		}
		return n
	}
	// The deadline itself, then reminders 2 s apart (the watcher looks every
	// 15 s, so allow for that).
	if n := count(20 * time.Second); n < 2 {
		t.Fatalf("want the deadline and at least one reminder within 20 s, got %d alerts", n)
	}
	// Close the task out: no more reminders.
	tk := map[string]any{}
	json.Unmarshal([]byte(a.must(t, "GET", "/api/tasks/r1", nil)), &tk)
	tk["status"] = "done"
	a.must(t, "PUT", "/api/tasks/r1", tk)
	count(1 * time.Second) // drain anything already queued
	if n := count(17 * time.Second); n != 0 {
		t.Errorf("reminders should stop once nothing is open, got %d", n)
	}
}

// Everything the page wrote is in the data file, readable, with a backup.
func TestDataFile(t *testing.T) {
	a := start(t, "")
	a.seed(t, mk("k1", "Kept", "todo", 0, 0, 0))
	raw, err := os.ReadFile(a.data)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
		Prefs map[string]any   `json:"prefs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the data file is not JSON: %v", err)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0]["title"] != "Kept" {
		t.Errorf("task not in the file: %s", raw)
	}
	if info := a.must(t, "GET", "/api/info", nil); !strings.Contains(info, `"file":true`) {
		t.Errorf("info should say tasks are in a file: %s", info)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(a.data), "backups", "*.json"))
	if len(backups) != 1 {
		t.Errorf("want one backup after the first change of the day, got %v", backups)
	}
}

// The search box narrows the board to matching titles, categories and
// comments; Escape clears it.
func TestSearch(t *testing.T) {
	a := start(t, "")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": false, "carry": false, "deadline": "", "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}, {"id": 1, "name": "AI training"}}})
	withNote := mk("s3", "Export logo files", "todo", 0, 0, 0)
	withNote["log"] = []map[string]any{{"kind": "note", "date": today(0), "at": today(0) + " 09:00",
		"text": "waiting on the vector source"}}
	a.seed(t,
		mk("s1", "Client call", "todo", 0, 0, 0),
		mk("s2", "Label dataset batch 4", "doing", 1, 0, 0),
		withNote,
	)
	ctx := browser(t, a)

	var all, byTitle, byCat, byNote, two, cleared int
	var sub string
	run(t, ctx,
		chromedp.WaitVisible("#stage .card", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &all),
		chromedp.Click("#search", chromedp.ByQuery),
		chromedp.SendKeys("#search", "client", chromedp.ByQuery), pause(150),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &byTitle),
		chromedp.Evaluate(`document.getElementById("subtitle").textContent`, &sub),
		shot("search"),
		setSearch("training"), pause(150),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &byCat),
		setSearch("vector source"), pause(150),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &byNote),
		setSearch("client label"), pause(150),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &two),
		// Escape in the box clears it.
		chromedp.Evaluate(`(()=>{const b=document.getElementById("search");b.focus();`+
			`b.dispatchEvent(new KeyboardEvent("keydown",{key:"Escape",bubbles:true}));return 1})()`, nil),
		pause(150),
		chromedp.Evaluate(`document.querySelectorAll('.card').length`, &cleared),
	)
	if all != 3 || byTitle != 1 || byCat != 1 || byNote != 1 || two != 0 || cleared != 3 {
		t.Errorf("cards: all %d, 'client' %d, 'training' %d, 'vector source' %d, 'client label' %d, cleared %d; want 3 1 1 1 0 3",
			all, byTitle, byCat, byNote, two, cleared)
	}
	if !strings.Contains(sub, "matching") {
		t.Errorf("the heading should say what is being matched: %q", sub)
	}
}

// setSearch types a fresh query into the search box.
func setSearch(q string) chromedp.Action {
	js := `(()=>{const b=document.getElementById("search");b.value=` + fmt.Sprintf("%q", q) +
		`;b.dispatchEvent(new Event("input",{bubbles:true}));return 1})()`
	return chromedp.Evaluate(js, nil)
}

// Save as .txt writes the day as "M/D Report" and "M/D  Todo" blocks.
func TestReport(t *testing.T) {
	a := start(t, "")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": false, "carry": false, "deadline": "", "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}}})
	call := mk("r1", "Call status", "done", 0, 0, 0)
	call["log"] = []map[string]any{{"kind": "note", "date": today(0), "at": today(0) + " 16:00",
		"text": "1 final call done, 1 rescheduled"}}
	a.seed(t,
		mk("r0", "Bid 221 Done", "done", 0, 0, 0),
		call,
		mk("r2", "Export logo files", "notdone", 0, 0, 0),
		mk("r3", "Continue 300 Bid", "doing", 0, 0, 0), // open on its last day: carries onto tomorrow
		mk("r4", "Find Citizen", "todo", 0, 1, 1),      // planned for tomorrow
		mk("r5", "Untouched", "todo", 0, 3, 3),         // another day: in neither block
	)
	ctx := browser(t, a)

	// Catch the file the button hands the browser instead of downloading it.
	var txt, jsErr string
	run(t, ctx,
		chromedp.WaitVisible("#stage .card", chromedp.ByQuery),
		chromedp.Evaluate(`(()=>{window.__txt="";window.__err="";HTMLAnchorElement.prototype.click=function(){};`+
			`window.onerror=(m)=>{window.__err=String(m)};`+
			`URL.createObjectURL=b=>{b.text().then(s=>{window.__txt=s});return "blob:x"};return 1})()`, nil),
		chromedp.Click("#saveTxt", chromedp.ByQuery),
		pause(600),
		chromedp.Evaluate(`window.__txt`, &txt),
		chromedp.Evaluate(`window.__err`, &jsErr),
	)
	if jsErr != "" {
		t.Fatalf("the page threw: %s", jsErr)
	}
	d := func(off int) string {
		v, _ := time.Parse("2006-01-02", today(off))
		return fmt.Sprintf("%d/%d", int(v.Month()), v.Day())
	}
	want := d(0) + " Report\r\n\r\n" +
		" -Bid 221 Done\r\n" +
		" -Call status(1 final call done, 1 rescheduled)\r\n" +
		" -Export logo files (not done)\r\n" +
		"=================================================\r\n" +
		d(1) + "  Todo\r\n\r\n" +
		" -Continue 300 Bid\r\n" +
		" -Find Citizen\r\n"
	if txt != want {
		t.Errorf("report:\n%q\nwant:\n%q", txt, want)
	}
}

// waitCards waits until the board shows n cards, or gives up.
func waitCards(t *testing.T, ctx context.Context, n int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var got int
		run(t, ctx, chromedp.Evaluate(`document.querySelectorAll('.card').length`, &got))
		if got == n {
			return true
		}
		if time.Now().After(deadline) {
			t.Errorf("board shows %d cards, want %d", got, n)
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// countTasks is how many tasks the server holds.
func (a *app) countTasks(t *testing.T) int {
	var list []map[string]any
	json.Unmarshal([]byte(a.must(t, "GET", "/api/tasks", nil)), &list)
	return len(list)
}

func (a *app) waitTasks(t *testing.T, n int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := a.countTasks(t)
		if got == n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A repeating task is planned ahead by the server; the page keeps the
// planned days in step with the head and takes them along when it goes.
func TestRepeat(t *testing.T) {
	a := start(t, "")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": false, "carry": false, "deadline": "", "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}}})
	head := mk("h", "Standup", "todo", 0, 0, 0)
	head["repeat"] = "daily"
	a.seed(t, head)

	// Sync plans 62 days ahead, once.
	var r struct {
		Added int              `json:"added"`
		Tasks []map[string]any `json:"tasks"`
	}
	json.Unmarshal([]byte(a.must(t, "POST", "/api/carry", nil)), &r)
	if r.Added != 62 || len(r.Tasks) != 63 {
		t.Fatalf("first sync planned %d (want 62), board has %d", r.Added, len(r.Tasks))
	}
	json.Unmarshal([]byte(a.must(t, "POST", "/api/carry", nil)), &r)
	if r.Added != 0 {
		t.Errorf("second sync planned %d more", r.Added)
	}
	inst := a.must(t, "GET", "/api/tasks/h."+today(1), nil)
	if !strings.Contains(inst, `"series":"h"`) || !strings.Contains(inst, `"title":"Standup"`) ||
		!strings.Contains(inst, `"repeat":""`) {
		t.Errorf("tomorrow's instance: %s", inst)
	}

	ctx := browser(t, a)
	var badge string
	run(t, ctx,
		chromedp.WaitVisible("#stage .card", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('.card[data-id="h"] .badge').textContent`, &badge),
		chromedp.Click(`[data-view="week"]`, chromedp.ByQuery), pause(300),
		shot("repeat-week"),
	)
	if badge != "daily" {
		t.Errorf("the head's badge reads %q, want daily", badge)
	}
	// This week: the head today and a planned day for each day left of it.
	now, _ := time.Parse("2006-01-02", today(0))
	left := 6 - (int(now.Weekday())+6)%7
	waitCards(t, ctx, 1+left, 3*time.Second)

	// The sheet on the head: a new rule drops the planned days and plans afresh.
	run(t, ctx,
		chromedp.Click(`[data-view="day"]`, chromedp.ByQuery), pause(200),
		chromedp.Click(`.card[data-id="h"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#detail.on", chromedp.ByQuery),
		setInput("#dRepeat", "weekly"), pause(100),
		chromedp.Click("#dSave", chromedp.ByQuery),
	)
	if n := a.waitTasks(t, 9, 6*time.Second); n != 9 { // head + 8 weekly days within 62
		t.Errorf("after switching to weekly the board has %d tasks, want 9", n)
	}
	if !strings.Contains(a.must(t, "GET", "/api/tasks/h", nil), `"repeat":"weekly"`) {
		t.Error("the head did not take the new rule")
	}

	// Renaming the head renames its planned days.
	run(t, ctx,
		chromedp.Click(`.card[data-id="h"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#detail.on", chromedp.ByQuery),
		chromedp.Evaluate(`(()=>{document.getElementById("dTitle").value="Weekly standup";return 1})()`, nil),
		chromedp.Click("#dSave", chromedp.ByQuery), pause(800),
	)
	if s := a.must(t, "GET", "/api/tasks/h."+today(7), nil); !strings.Contains(s, "Weekly standup") {
		t.Errorf("next week's day did not follow the rename: %s", s)
	}

	// Deleting the head takes the untouched planned days with it; Undo brings all back.
	run(t, ctx,
		chromedp.Click(`.card[data-id="h"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#detail.on", chromedp.ByQuery),
		chromedp.Click("#dDelete", chromedp.ByQuery),
	)
	if n := a.waitTasks(t, 0, 6*time.Second); n != 0 {
		t.Errorf("after deleting the head %d tasks remain", n)
	}
	run(t, ctx, chromedp.Click("#toastUndo", chromedp.ByQuery))
	if n := a.waitTasks(t, 9, 6*time.Second); n != 9 {
		t.Errorf("after Undo the board has %d tasks, want 9", n)
	}
}

// The page follows changes it did not make: through the server, and to the
// data file itself.
func TestLiveRefresh(t *testing.T) {
	a := start(t, "")
	a.must(t, "PUT", "/api/prefs", map[string]any{"ask": false, "carry": false, "deadline": "", "dayStart": 0,
		"categories": []map[string]any{{"id": 0, "name": "Job"}}})
	a.seed(t, mk("l1", "First", "todo", 0, 0, 0))
	ctx := browser(t, a)
	run(t, ctx, chromedp.WaitVisible("#stage .card", chromedp.ByQuery))

	// Another client of the same server.
	a.seed(t, mk("l2", "Second", "todo", 0, 0, 0))
	waitCards(t, ctx, 2, 10*time.Second)

	// Another program writing the data file: the server follows the file,
	// and the page follows the server.
	raw, err := os.ReadFile(a.data)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	list := doc["tasks"].([]any)
	doc["tasks"] = append(list, map[string]any(mk("l3", "Third", "todo", 0, 0, 0)))
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(a.data, out, 0o644); err != nil {
		t.Fatal(err)
	}
	waitCards(t, ctx, 3, 15*time.Second)
	var note string
	run(t, ctx, chromedp.Evaluate(`document.getElementById("err").textContent`, &note))
	if !strings.Contains(note, "changed elsewhere") {
		t.Errorf("the page should say the board was reloaded, got %q", note)
	}
	if list := a.must(t, "GET", "/api/tasks", nil); !strings.Contains(list, `"l3"`) {
		t.Errorf("the server did not pick up the file: %s", list)
	}
}
