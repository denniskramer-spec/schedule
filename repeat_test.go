package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextOccurrence(t *testing.T) {
	cases := []struct{ rule, from, anchor, want string }{
		{"daily", "2026-09-15", "2026-09-15", "2026-09-16"},
		{"weekdays", "2026-09-18", "2026-09-14", "2026-09-21"}, // Friday to Monday
		{"weekdays", "2026-09-16", "2026-09-14", "2026-09-17"},
		{"weekly", "2026-09-15", "2026-09-15", "2026-09-22"},
		{"weekly", "2026-09-17", "2026-09-15", "2026-09-22"}, // the anchor's weekday, not from+7
		{"monthly", "2026-09-15", "2026-09-15", "2026-10-15"},
		{"monthly", "2026-01-31", "2026-01-31", "2026-02-28"}, // clamped to the month's end
		{"monthly", "2026-02-28", "2026-01-31", "2026-03-31"}, // and back to the anchor's day
		{"monthly", "2026-10-15", "2026-09-15", "2026-11-15"},
		{"", "2026-09-15", "2026-09-15", ""},
		{"daily", "nope", "2026-09-15", ""},
	}
	for _, c := range cases {
		if got := nextOccurrence(c.rule, c.from, c.anchor); got != c.want {
			t.Errorf("nextOccurrence(%q, %q, %q) = %q, want %q", c.rule, c.from, c.anchor, got, c.want)
		}
	}
}

func TestNewInstances(t *testing.T) {
	head := task(func(x *Task) {
		x.ID = "h"
		x.Date = "2026-09-14"
		x.End = "2026-09-15" // two days long
		x.Repeat = "weekly"
		x.Category = 2
		x.Agent = true
		x.Status = "doing"
		x.Log = []Entry{{Kind: "note", Date: "2026-09-14", At: "2026-09-14 10:00", Text: "x"}}
	})
	inst, through := newInstances(head, "2026-09-15", "2026-10-15", "2026-09-15 08:00")
	if len(inst) != 4 || through != "2026-10-12" {
		t.Fatalf("got %d instances through %s, want 4 through 2026-10-12", len(inst), through)
	}
	first := inst[0]
	if first.ID != "h.2026-09-21" || first.Date != "2026-09-21" || first.End != "2026-09-22" ||
		first.Series != "h" || first.Repeat != "" || first.Status != "todo" || len(first.Log) != 0 ||
		first.Category != 2 || !first.Agent || first.CreatedAt != "2026-09-15 08:00" || first.Title != head.Title {
		t.Errorf("first instance: %+v", first)
	}
	if validate(first) != "" || validate(head) != "" {
		t.Errorf("instances and heads must validate: %q / %q", validate(first), validate(head))
	}

	// Already extended: only the days after that.
	head.RepeatedThrough = "2026-09-28"
	inst, through = newInstances(head, "2026-09-15", "2026-10-15", "now")
	if len(inst) != 2 || inst[0].Date != "2026-10-05" || through != "2026-10-12" {
		t.Errorf("after 09-28: %d instances from %s through %s", len(inst), inst[0].Date, through)
	}

	// A head dated long ago starts from today: the missed days are not filled in.
	head.RepeatedThrough = ""
	head.Date, head.End, head.Repeat = "2026-01-05", "2026-01-05", "daily"
	inst, _ = newInstances(head, "2026-09-15", "2026-09-17", "now")
	if len(inst) != 3 || inst[0].Date != "2026-09-15" || inst[2].Date != "2026-09-17" {
		t.Errorf("old head: want 15, 16, 17 Sep; got %d from %s", len(inst), inst[0].Date)
	}

	// Nothing to add yet: through still moves to the start, so it is remembered.
	head.Date, head.End = "2026-09-15", "2026-09-15"
	inst, through = newInstances(head, "2026-09-15", "2026-09-15", "now")
	if len(inst) != 0 || through != "2026-09-15" {
		t.Errorf("horizon today: %d instances through %s", len(inst), through)
	}
}

func TestFileStoreAddRepeats(t *testing.T) {
	fs, _ := tempStore(t)
	head := task(func(x *Task) { x.ID = "h"; x.Date = "2026-09-15"; x.End = "2026-09-15"; x.Repeat = "daily" })
	if err := fs.InsertTask(head); err != nil {
		t.Fatal(err)
	}
	n, err := fs.AddRepeats("2026-09-15", "2026-09-18", "now")
	if err != nil || n != 3 {
		t.Fatalf("first extension: %d, %v; want 3", n, err)
	}
	// Again: nothing new. A deleted instance stays deleted.
	if err := fs.DeleteTask("h.2026-09-17"); err != nil {
		t.Fatal(err)
	}
	if n, _ = fs.AddRepeats("2026-09-15", "2026-09-18", "now"); n != 0 {
		t.Errorf("second extension to the same horizon made %d", n)
	}
	// A wider horizon adds only the days beyond.
	if n, _ = fs.AddRepeats("2026-09-15", "2026-09-20", "now"); n != 2 {
		t.Errorf("wider horizon made %d, want 2", n)
	}
	h, _ := fs.GetTask("h")
	if h.RepeatedThrough != "2026-09-20" {
		t.Errorf("head extended through %q, want 2026-09-20", h.RepeatedThrough)
	}
	list, _ := fs.ListTasks()
	if len(list) != 5 { // head, 16, 18, 19, 20
		t.Errorf("want 5 tasks, got %d", len(list))
	}
	// Rule reset: repeatedThrough cleared by the page starts again from today.
	h.Repeat, h.RepeatedThrough = "weekly", ""
	fs.ReplaceTask("h", h)
	if n, _ = fs.AddRepeats("2026-09-16", "2026-09-30", "now"); n != 2 {
		t.Errorf("weekly from 16 Sep to 30 Sep made %d, want 2 (22, 29)", n)
	}
}

func TestFileStoreFingerprintFollowsDisk(t *testing.T) {
	fs, path := tempStore(t)
	fp1, _ := fs.Fingerprint()
	fs.InsertTask(task(nil))
	fp2, _ := fs.Fingerprint()
	if fp1 == fp2 {
		t.Error("a write must change the fingerprint")
	}
	// Someone else writes the file: the store follows it.
	other, _ := openFileStore(filepath.Join(t.TempDir(), "o.json"))
	other.InsertTask(task(func(x *Task) { x.ID = "o"; x.Title = "Theirs" }))
	raw, _ := os.ReadFile(other.path)
	os.WriteFile(path, raw, 0o644)
	fp3, _ := fs.Fingerprint()
	list, _ := fs.ListTasks()
	if fp3 == fp2 || len(list) != 1 || list[0].Title != "Theirs" {
		t.Errorf("after an outside change: fp changed %v, tasks %+v", fp3 != fp2, list)
	}
	// And a write after that goes through, in step with the file.
	if err := fs.InsertTask(task(func(x *Task) { x.ID = "m" })); err != nil {
		t.Errorf("write after following the disk: %v", err)
	}
}
