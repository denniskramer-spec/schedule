package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func task(mut func(*Task)) Task {
	t := Task{ID: "t1", Title: "Ship it", Date: "2026-09-07", End: "2026-09-07",
		Status: "todo", Log: []Entry{}}
	if mut != nil {
		mut(&t)
	}
	return t
}

func TestValidateSpan(t *testing.T) {
	cases := []struct {
		name string
		task Task
		ok   bool
	}{
		{"one day", task(nil), true},
		{"runs a week", task(func(x *Task) { x.End = "2026-09-11" }), true},
		{"end missing", task(func(x *Task) { x.End = "" }), false},
		{"end before start", task(func(x *Task) { x.End = "2026-09-06" }), false},
		{"end malformed", task(func(x *Task) { x.End = "11-09-2026" }), false},
		{"absurd span", task(func(x *Task) { x.End = "2030-09-07" }), false},
		{"no title", task(func(x *Task) { x.Title = " " }), false},
		{"bad status", task(func(x *Task) { x.Status = "maybe" }), false},
	}
	for _, c := range cases {
		got := validate(c.task) == ""
		if got != c.ok {
			t.Errorf("%s: valid=%v, want %v (%q)", c.name, got, c.ok, validate(c.task))
		}
	}
}

func TestValidateLog(t *testing.T) {
	with := func(e Entry) Task { return task(func(x *Task) { x.Log = []Entry{e} }) }
	cases := []struct {
		name  string
		entry Entry
		ok    bool
	}{
		{"a comment", Entry{Kind: "note", Date: "2026-09-07", Text: "blocked"}, true},
		{"a carry needs no text", Entry{Kind: "carry", Date: "2026-09-07", From: "2026-09-06"}, true},
		{"a move", Entry{Kind: "move", Date: "2026-09-07", From: "2026-09-01"}, true},
		{"empty comment", Entry{Kind: "note", Date: "2026-09-07", Text: "  "}, false},
		{"unknown kind", Entry{Kind: "shrug", Date: "2026-09-07"}, false},
		{"bad date", Entry{Kind: "carry", Date: "septemberish"}, false},
		{"bad from", Entry{Kind: "carry", Date: "2026-09-07", From: "nope"}, false},
		{"bad status", Entry{Kind: "carry", Date: "2026-09-07", Status: "maybe"}, false},
	}
	for _, c := range cases {
		if got := validate(with(c.entry)) == ""; got != c.ok {
			t.Errorf("%s: valid=%v, want %v (%q)", c.name, got, c.ok, validate(with(c.entry)))
		}
	}
}

func TestTrimLog(t *testing.T) {
	if got := trimLog(nil); got == nil || len(got) != 0 {
		t.Errorf("nil log must become an empty array, got %v", got)
	}
	long := make([]Entry, maxLogLen+40)
	for i := range long {
		long[i] = Entry{Kind: "note", Date: "2026-09-07", Text: string(rune('a' + i%26))}
	}
	got := trimLog(long)
	if len(got) != maxLogLen {
		t.Errorf("kept %d entries, want %d", len(got), maxLogLen)
	}
	if got[len(got)-1].Text != long[len(long)-1].Text {
		t.Error("trimLog dropped the newest entries instead of the oldest")
	}
}

func TestDBNameFromURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://localhost:27017":                       "",
		"mongodb://localhost:27017/":                      "",
		"mongodb://localhost:27017/mydata":                "mydata",
		"mongodb+srv://u:p@c0.abc.mongodb.net/schedule":   "schedule",
		"mongodb://localhost:27017/mydata?retryWrites=on": "mydata",
	}
	for uri, want := range cases {
		if got := dbNameFromURI(uri); got != want {
			t.Errorf("%s -> %q, want %q", uri, got, want)
		}
	}
}

func TestHostLabelHidesCredentials(t *testing.T) {
	cases := map[string]string{
		"mongodb://localhost:27017":                     "localhost:27017",
		"mongodb://bob:hunter2@db.example:27017/sched":  "db.example:27017",
		"mongodb+srv://u:p@c0.abc.mongodb.net/schedule": "c0.abc.mongodb.net",
	}
	for uri, want := range cases {
		if got := hostLabel(uri); got != want {
			t.Errorf("%s -> %q, want %q; a password must never be printable", uri, got, want)
		}
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.1", "2.0", true}, {"2.0", "2.1", false}, {"2.0", "2.0", false},
		{"2.10", "2.9", true}, {"3", "2.9", true}, {"2.0.1", "2.0", true}, {"2.1", "dev", true},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestValidDeadline(t *testing.T) {
	for s, ok := range map[string]bool{
		"": true, "05:30": true, "17:45": true, "23:59": true,
		"5:30": false, "24:00": false, "05:60": false, "noon": false,
	} {
		if validDeadline(s) != ok {
			t.Errorf("validDeadline(%q) = %v, want %v", s, !ok, ok)
		}
	}
}

func TestOpenWorkOn(t *testing.T) {
	week := task(func(x *Task) { x.End = "2026-09-11" })
	done := task(func(x *Task) { x.Status = "done" })
	if !openWorkOn([]Task{week}, "2026-09-09") {
		t.Error("a running task should count on a day in its range")
	}
	if openWorkOn([]Task{week}, "2026-09-12") {
		t.Error("a task should not count after its last day")
	}
	if openWorkOn([]Task{done}, "2026-09-07") {
		t.Error("a finished task should not count")
	}
}

func TestLogicalDay(t *testing.T) {
	at := func(s string) time.Time {
		v, _ := time.ParseInLocation(stamp, s, time.Local)
		return v
	}
	cases := []struct {
		now   string
		start int
		want  string
	}{
		{"2026-09-15 02:00", 0, "2026-09-15"},
		// a morning start lets the day run late
		{"2026-09-15 02:00", 6, "2026-09-14"},
		{"2026-09-15 05:59", 6, "2026-09-14"},
		{"2026-09-15 06:00", 6, "2026-09-15"},
		// an evening start ends the day early
		{"2026-09-14 22:30", 23, "2026-09-14"},
		{"2026-09-14 23:00", 23, "2026-09-15"},
		{"2026-09-14 12:00", 12, "2026-09-15"},
		{"2026-09-14 11:59", 12, "2026-09-14"},
	}
	for _, c := range cases {
		if got := logicalDay(at(c.now), c.start); got != c.want {
			t.Errorf("logicalDay(%s, %d) = %s, want %s", c.now, c.start, got, c.want)
		}
	}
}

func TestDeadlineAt(t *testing.T) {
	cases := []struct {
		day      string
		start    int
		deadline string
		want     string
	}{
		{"2026-09-14", 0, "17:30", "2026-09-14 17:30"},
		// the case that did not fire: start 23:00, deadline 22:00
		{"2026-09-14", 23, "22:00", "2026-09-14 22:00"},
		{"2026-09-14", 23, "23:30", "2026-09-13 23:30"},
		// a morning start: 05:30 is the end of the night, on the next date
		{"2026-09-14", 6, "05:30", "2026-09-15 05:30"},
		{"2026-09-14", 6, "18:00", "2026-09-14 18:00"},
	}
	for _, c := range cases {
		got, ok := deadlineAt(c.day, c.start, c.deadline)
		if !ok || got.Format(stamp) != c.want {
			t.Errorf("deadlineAt(%s, %d, %s) = %s, want %s", c.day, c.start, c.deadline, got.Format(stamp), c.want)
		}
		// and the moment itself belongs to that day
		if ok && logicalDay(got, c.start) != c.day {
			t.Errorf("deadline %s for %s lands in %s", c.want, c.day, logicalDay(got, c.start))
		}
	}
}

func TestValidateCategories(t *testing.T) {
	if msg := validate(task(func(x *Task) { x.Category = 7 })); msg != "" {
		t.Errorf("category 7 should be valid: %s", msg)
	}
	if validate(task(func(x *Task) { x.Category = -1 })) == "" {
		t.Error("a negative category should be rejected")
	}
	if validateCategories(defaultCategories()) != "" {
		t.Error("the defaults should be valid")
	}
	one := []Category{{4, "Only"}}
	if validateCategories(one) != "" {
		t.Error("a single category should be valid")
	}
	if validateCategories(nil) == "" {
		t.Error("no categories should be rejected")
	}
	if validateCategories([]Category{{0, "Job"}, {1, " "}}) == "" {
		t.Error("a blank name should be rejected")
	}
	if validateCategories([]Category{{0, "Job"}, {0, "Again"}}) == "" {
		t.Error("a repeated id should be rejected")
	}
	many := make([]Category, maxCategories+1)
	for i := range many {
		many[i] = Category{i, "Row"}
	}
	if validateCategories(many) == "" {
		t.Error("too many categories should be rejected")
	}
}

func TestSecureURL(t *testing.T) {
	ok := []string{"https://example.com/latest.json", "http://127.0.0.1:8080/latest.json",
		"http://localhost/latest.json", "http://[::1]:9/x"}
	for _, u := range ok {
		if err := secureURL(u); err != nil {
			t.Errorf("secureURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{"http://example.com/latest.json", "ftp://example.com/x", "file:///c:/x", "example.com/x"}
	for _, u := range bad {
		if err := secureURL(u); err == nil {
			t.Errorf("secureURL(%q) = nil, want an error", u)
		}
	}
}

// The setup only runs if it hashes to what latest.json promised.
func TestDownloadVerifiesSha256(t *testing.T) {
	body := []byte("not really an exe")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	t.Setenv("TEMP", os.Getenv("TMPDIR"))

	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	path, err := download(srv.URL+"/Schedule-Setup.exe", good)
	if err != nil {
		t.Fatalf("download with the right sha256: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Errorf("downloaded file differs: %q", got)
	}

	wrong := strings.Repeat("0", 64)
	if _, err := download(srv.URL+"/Schedule-Setup.exe", wrong); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Errorf("download with the wrong sha256: got %v, want a mismatch error", err)
	}
	if _, err := download(srv.URL+"/Schedule-Setup.exe", ""); err == nil ||
		!strings.Contains(err.Error(), "no sha256") {
		t.Errorf("download with no sha256: got %v, want a refusal", err)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "Schedule-update", "*.part")); len(left) != 0 {
		t.Errorf("a refused download left %v behind", left)
	}
}

// A staged setup survives a restart only while it is still newer than this
// build and still hashes as it did when it was verified.
func TestStagedUpdate(t *testing.T) {
	appDir = t.TempDir()
	body := []byte("setup bytes")
	path := filepath.Join(appDir, "Schedule-Setup.exe")
	os.WriteFile(path, body, 0o644)
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])

	old := version
	version = "2.4"
	defer func() { version = old }()

	(&staged{Version: "2.5", Sha256: good, Path: path}).save()
	if st := loadStaged(); st == nil || st.Version != "2.5" || st.Path != path {
		t.Fatalf("a good staged setup should load: %+v", st)
	}
	(&staged{Version: "2.4", Sha256: good, Path: path}).save()
	if loadStaged() != nil {
		t.Error("a staged setup no newer than this build should be ignored")
	}
	(&staged{Version: "2.5", Sha256: strings.Repeat("0", 64), Path: path}).save()
	if loadStaged() != nil {
		t.Error("a staged setup whose file no longer hashes right should be ignored")
	}
	(&staged{Version: "2.5", Sha256: good, Path: path}).save()
	os.Remove(path)
	if loadStaged() != nil {
		t.Error("a staged setup whose file is gone should be ignored")
	}
}
