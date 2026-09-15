package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tempStore(t *testing.T) (*fileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.json")
	fs, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return fs, path
}

func TestFileStoreRoundTrip(t *testing.T) {
	fs, path := tempStore(t)

	if err := fs.InsertTask(task(nil)); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertTask(task(nil)); !errors.Is(err, errDuplicate) {
		t.Errorf("second insert of the same id: got %v, want errDuplicate", err)
	}
	later := task(func(x *Task) { x.ID = "t0"; x.Date = "2026-09-01"; x.End = "2026-09-01" })
	if err := fs.InsertTask(later); err != nil {
		t.Fatal(err)
	}

	got, err := fs.GetTask("t1")
	if err != nil || got.Title != "Ship it" {
		t.Fatalf("GetTask: %v, %+v", err, got)
	}
	got.Title = "Shipped"
	got.Log = append(got.Log, Entry{Kind: "note", Date: "2026-09-07", At: "2026-09-07 10:00", Text: "done"})
	if err := fs.ReplaceTask("t1", got); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReplaceTask("nope", got); !errors.Is(err, errNoRow) {
		t.Errorf("replace of a missing task: got %v, want errNoRow", err)
	}

	p, _ := fs.GetPrefs()
	p.Deadline = "17:30"
	p.Categories = append(p.Categories, Category{7, "Personal"})
	if err := fs.SetPrefs(p); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAlertDay("2026-09-07"); err != nil {
		t.Fatal(err)
	}

	// Everything must come back from the file, not just from memory.
	again, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := again.ListTasks()
	if len(list) != 2 || list[0].ID != "t0" || list[1].ID != "t1" {
		t.Fatalf("ListTasks after reopen: %+v", list)
	}
	if list[1].Title != "Shipped" || len(list[1].Log) != 1 || list[1].Log[0].Text != "done" {
		t.Errorf("edit did not survive reopen: %+v", list[1])
	}
	p2, _ := again.GetPrefs()
	if p2.Deadline != "17:30" || len(p2.Categories) != 4 || p2.Categories[3].Name != "Personal" {
		t.Errorf("prefs did not survive reopen: %+v", p2)
	}
	if again.GetAlertDay() != "2026-09-07" {
		t.Errorf("alert day did not survive reopen: %q", again.GetAlertDay())
	}

	if err := again.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if err := again.DeleteTask("t1"); !errors.Is(err, errNoRow) {
		t.Errorf("second delete: got %v, want errNoRow", err)
	}
	if _, err := again.GetTask("t1"); !errors.Is(err, errNoRow) {
		t.Errorf("deleted task still found")
	}
}

func TestFileStoreHandsOutCopies(t *testing.T) {
	fs, _ := tempStore(t)
	fs.InsertTask(task(func(x *Task) { x.Log = []Entry{{Kind: "note", Date: "2026-09-07", Text: "a"}} }))
	got, _ := fs.GetTask("t1")
	got.Log[0].Text = "changed outside the store"
	got.Title = "changed outside the store"
	back, _ := fs.GetTask("t1")
	if back.Title != "Ship it" || back.Log[0].Text != "a" {
		t.Errorf("a caller's edits leaked into the store: %+v", back)
	}
}

func TestFileStoreCarryForward(t *testing.T) {
	fs, _ := tempStore(t)
	fs.InsertTask(task(func(x *Task) { x.ID = "one"; x.Date = "2026-09-05"; x.End = "2026-09-05" }))
	fs.InsertTask(task(func(x *Task) { x.ID = "span"; x.Date = "2026-09-01"; x.End = "2026-09-05"; x.Status = "doing" }))
	fs.InsertTask(task(func(x *Task) { x.ID = "done"; x.Date = "2026-09-05"; x.End = "2026-09-05"; x.Status = "done" }))
	fs.InsertTask(task(func(x *Task) { x.ID = "fine"; x.Date = "2026-09-07"; x.End = "2026-09-09" }))

	n, err := fs.CarryForward("2026-09-07", "2026-09-07 06:00")
	if err != nil || n != 2 {
		t.Fatalf("CarryForward: n=%d err=%v, want 2", n, err)
	}
	one, _ := fs.GetTask("one")
	if one.Date != "2026-09-07" || one.End != "2026-09-07" {
		t.Errorf("a one-day task should move whole: %s to %s", one.Date, one.End)
	}
	span, _ := fs.GetTask("span")
	if span.Date != "2026-09-01" || span.End != "2026-09-07" {
		t.Errorf("only the last day of a run should move: %s to %s", span.Date, span.End)
	}
	if len(span.Log) != 1 || span.Log[0].Kind != "carry" || span.Log[0].From != "2026-09-05" {
		t.Errorf("the move should be in the history: %+v", span.Log)
	}
	for _, id := range []string{"done", "fine"} {
		x, _ := fs.GetTask(id)
		if len(x.Log) != 0 {
			t.Errorf("%s should have been left alone", id)
		}
	}
	// Nothing left to carry: no write, no count.
	if n, _ := fs.CarryForward("2026-09-07", "2026-09-07 06:01"); n != 0 {
		t.Errorf("second carry moved %d, want 0", n)
	}
}

func TestFileStoreSetCategory(t *testing.T) {
	fs, _ := tempStore(t)
	for _, id := range []string{"a", "b", "c"} {
		fs.InsertTask(task(func(x *Task) { x.ID = id; x.Category = 2 }))
	}
	if err := fs.SetCategory([]string{"a", "c"}, 0); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"a": 0, "b": 2, "c": 0} {
		x, _ := fs.GetTask(id)
		if x.Category != want {
			t.Errorf("%s: category %d, want %d", id, x.Category, want)
		}
	}
}

func TestFileStoreBackup(t *testing.T) {
	fs, path := tempStore(t)
	fs.InsertTask(task(nil))
	fs.InsertTask(task(func(x *Task) { x.ID = "t2" }))
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "schedule-*.json"))
	if len(backups) != 1 {
		t.Fatalf("want one backup for today, got %v", backups)
	}
	// The backup is the file as it was before today's first change: the
	// empty board the store was created with.
	b, err := openFileStore(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := b.ListTasks(); len(list) != 0 {
		t.Errorf("backup should hold the board from before the first change, has %d tasks", len(list))
	}
}

func TestFileStoreRejectsOtherFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	os.WriteFile(path, []byte("this is not json"), 0o644)
	if _, err := openFileStore(path); err == nil {
		t.Error("a file that is not a Schedule data file should be refused, not overwritten")
	}
}

// A second copy of Schedule reaching the file through a synced folder must
// not have its work overwritten: the write that notices is refused, the file
// re-read, and the board shows what the other side put there.
func TestFileStoreRefusesToOverwriteOutsideChange(t *testing.T) {
	fs, path := tempStore(t)
	if err := fs.InsertTask(task(nil)); err != nil {
		t.Fatal(err)
	}

	// Someone else writes the file: a different board, one task called Theirs.
	other, _ := openFileStore(filepath.Join(t.TempDir(), "other.json"))
	other.InsertTask(task(func(x *Task) { x.ID = "o1"; x.Title = "Theirs" }))
	theirs, _ := os.ReadFile(other.path)
	if err := os.WriteFile(path, theirs, 0o644); err != nil {
		t.Fatal(err)
	}

	mine := task(func(x *Task) { x.ID = "t2"; x.Title = "Mine" })
	if err := fs.InsertTask(mine); !errors.Is(err, errConflict) {
		t.Fatalf("insert over an outside change: got %v, want errConflict", err)
	}
	list, _ := fs.ListTasks()
	if len(list) != 1 || list[0].Title != "Theirs" {
		t.Errorf("after the conflict the store should hold the outside board, got %+v", list)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(theirs) {
		t.Error("the outside change was overwritten")
	}

	// Once re-read, the store is in step with the file again and writes go through.
	if err := fs.InsertTask(mine); err != nil {
		t.Fatalf("insert after re-reading: %v", err)
	}
	again, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ = again.ListTasks(); len(list) != 2 {
		t.Errorf("want Theirs and Mine in the file, got %+v", list)
	}
}
