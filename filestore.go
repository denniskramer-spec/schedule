package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

/* ---------- the data file ----------

   Everything - tasks, settings, the day the deadline last fired - is one JSON
   document, kept in memory and written whole after every change. A board has
   hundreds of tasks at most, so that is instant, and it means the file on
   disk is always a complete, readable copy that can be backed up by copying.

   Writes go to a temporary file that is then renamed over the real one, so a
   crash mid-write leaves the old file intact rather than a half-written new
   one. The first write of each day first copies the current file into
   backups/, and the newest keepBackups copies are kept.

   Only one copy of Schedule should use a file, but nothing stops a second
   machine reaching it through a synced folder. So before each write the file
   is read back and compared with what was last read or written: if someone
   else has changed it, the write is refused with errConflict, the file is
   re-read, and the caller's change is dropped rather than overwriting theirs.
   The board then shows what is really in the file. */

const keepBackups = 14

type fileData struct {
	Version  int    `json:"version"`
	Prefs    Prefs  `json:"prefs"`
	AlertDay string `json:"alertDay,omitempty"`
	Tasks    []Task `json:"tasks"`
}

type fileStore struct {
	path string

	mu       sync.Mutex
	data     fileData
	backedUp string   // the day a backup was last taken
	onDisk   [32]byte // sha256 of the file as last read or written
}

func openFileStore(path string) (*fileStore, error) {
	fs := &fileStore{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		fs.data = fileData{Version: 1, Prefs: defaultPrefs(), Tasks: []Task{}}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := fs.save(); err != nil {
			return nil, err
		}
		return fs, nil
	case err != nil:
		return nil, err
	}
	if err := fs.load(raw); err != nil {
		return nil, err
	}
	return fs, nil
}

// load takes the file's bytes as the store's contents.
func (fs *fileStore) load(raw []byte) error {
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("%s is not a Schedule data file: %v", filepath.Base(fs.path), err)
	}
	data.Prefs.normalize()
	if data.Tasks == nil {
		data.Tasks = []Task{}
	}
	for i := range data.Tasks {
		t := &data.Tasks[i]
		t.Log = trimLog(t.Log)
		if t.End == "" {
			t.End = t.Date
		}
	}
	fs.data = data
	fs.onDisk = sha256.Sum256(raw)
	return nil
}

// save writes the whole document. Callers hold fs.mu.
//
// It first reads the file back: if it is no longer what this store last
// read or wrote, someone else has changed it. Then the in-memory change is
// abandoned, the file is re-read so the board shows the other side's work,
// and errConflict says so.
func (fs *fileStore) save() error {
	src, err := os.ReadFile(fs.path)
	if err == nil {
		if fs.onDisk != [32]byte{} && sha256.Sum256(src) != fs.onDisk {
			if lerr := fs.load(src); lerr != nil {
				return fmt.Errorf("%w. Re-reading it failed too: %v", errConflict, lerr)
			}
			return errConflict
		}
		fs.backup(src)
	}
	raw, err := json.MarshalIndent(fs.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := fs.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, fs.path); err != nil {
		os.Remove(tmp)
		return err
	}
	fs.onDisk = sha256.Sum256(raw)
	return nil
}

// backup keeps one copy of the file per day, from before that day's first
// change, so a mistake can be undone by copying a backup back. src is the
// file as it is now.
func (fs *fileStore) backup(src []byte) {
	today := time.Now().Format(day)
	if fs.backedUp == today {
		return
	}
	fs.backedUp = today
	dir := filepath.Join(filepath.Dir(fs.path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := strings.TrimSuffix(filepath.Base(fs.path), filepath.Ext(fs.path))
	dst := filepath.Join(dir, name+"-"+today+".json")
	if _, err := os.Stat(dst); err == nil {
		return // already taken today, by an earlier run
	}
	os.WriteFile(dst, src, 0o644)

	old, _ := filepath.Glob(filepath.Join(dir, name+"-*.json"))
	sort.Strings(old) // dated names sort oldest first
	for len(old) > keepBackups {
		os.Remove(old[0])
		old = old[1:]
	}
}

// snapshot copies the file as it is now into backups/, under a label.
func (fs *fileStore) snapshot(label string) {
	src, err := os.ReadFile(fs.path)
	if err != nil {
		return
	}
	dir := filepath.Join(filepath.Dir(fs.path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := strings.TrimSuffix(filepath.Base(fs.path), filepath.Ext(fs.path))
	os.WriteFile(filepath.Join(dir, name+"-"+label+".json"), src, 0o644)
}

// ReplaceAll swaps in a whole board. What it replaces is kept first, as
// backups/<name>-before-restore-<time>.json, so a restore can be undone.
func (fs *fileStore) ReplaceAll(tasks []Task, p Prefs, alertDay string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.data.Tasks) > 0 {
		fs.snapshot("before-restore-" + time.Now().Format("2006-01-02-1504"))
	}
	p.normalize()
	for i := range tasks {
		tasks[i].Log = trimLog(tasks[i].Log)
	}
	fs.data = fileData{Version: 1, Prefs: p, AlertDay: alertDay, Tasks: tasks}
	return fs.save()
}

func (fs *fileStore) Close() {}

func (fs *fileStore) Label() string { return fs.path }

func (fs *fileStore) find(id string) int {
	for i := range fs.data.Tasks {
		if fs.data.Tasks[i].ID == id {
			return i
		}
	}
	return -1
}

// copyTask hands out a task the caller may change without touching the store.
func copyTask(t Task) Task {
	t.Log = append([]Entry{}, t.Log...)
	return t
}

func (fs *fileStore) ListTasks() ([]Task, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]Task, 0, len(fs.data.Tasks))
	for _, t := range fs.data.Tasks {
		out = append(out, copyTask(t))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

func (fs *fileStore) GetTask(id string) (Task, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	i := fs.find(id)
	if i < 0 {
		return Task{}, errNoRow
	}
	return copyTask(fs.data.Tasks[i]), nil
}

func (fs *fileStore) InsertTask(t Task) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.find(t.ID) >= 0 {
		return errDuplicate
	}
	t.Log = trimLog(t.Log)
	fs.data.Tasks = append(fs.data.Tasks, copyTask(t))
	return fs.save()
}

func (fs *fileStore) ReplaceTask(id string, t Task) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	i := fs.find(id)
	if i < 0 {
		return errNoRow
	}
	t.ID = id
	t.Log = trimLog(t.Log)
	fs.data.Tasks[i] = copyTask(t)
	return fs.save()
}

func (fs *fileStore) DeleteTask(id string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	i := fs.find(id)
	if i < 0 {
		return errNoRow
	}
	fs.data.Tasks = append(fs.data.Tasks[:i], fs.data.Tasks[i+1:]...)
	return fs.save()
}

func (fs *fileStore) DeleteAllTasks() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.Tasks = []Task{}
	return fs.save()
}

func (fs *fileStore) GetPrefs() (Prefs, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p := fs.data.Prefs
	p.Categories = append([]Category{}, p.Categories...)
	return p, nil
}

func (fs *fileStore) SetPrefs(p Prefs) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p.normalize()
	fs.data.Prefs = p
	return fs.save()
}

func (fs *fileStore) GetAlertDay() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.data.AlertDay
}

func (fs *fileStore) SetAlertDay(d string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.data.AlertDay = d
	return fs.save()
}

func (fs *fileStore) SetCategory(ids []string, category int) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	changed := false
	for i := range fs.data.Tasks {
		if want[fs.data.Tasks[i].ID] && fs.data.Tasks[i].Category != category {
			fs.data.Tasks[i].Category = category
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return fs.save()
}

// AddRepeats extends every repeating task up to horizon; see repeat.go.
func (fs *fileStore) AddRepeats(today, horizon, now string) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	have := map[string]bool{}
	for _, t := range fs.data.Tasks {
		have[t.ID] = true
	}
	n, changed := 0, false
	var made []Task
	for i := range fs.data.Tasks {
		head := &fs.data.Tasks[i]
		if head.Repeat == "" {
			continue
		}
		inst, through := newInstances(*head, today, horizon, now)
		for _, t := range inst {
			if have[t.ID] {
				continue
			}
			made = append(made, t)
			have[t.ID] = true
			n++
		}
		if through != head.RepeatedThrough {
			head.RepeatedThrough = through
			changed = true
		}
	}
	if n == 0 && !changed {
		return 0, nil
	}
	fs.data.Tasks = append(fs.data.Tasks, made...)
	return n, fs.save()
}

// Fingerprint is the file's hash. If the file on disk has changed since it
// was last read or written - a synced folder delivering another machine's
// work - it is re-read first, so the board follows the file.
func (fs *fileStore) Fingerprint() (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	src, err := os.ReadFile(fs.path)
	if err != nil {
		return hex.EncodeToString(fs.onDisk[:]), nil
	}
	if sha256.Sum256(src) != fs.onDisk {
		if err := fs.load(src); err != nil {
			return "", err
		}
		log.Printf("%s changed on disk; re-read it", filepath.Base(fs.path))
	}
	return hex.EncodeToString(fs.onDisk[:]), nil
}

// CarryForward moves every unfinished task that has run out of days onto
// today and records the move in its history. A task stuck for a week lands on
// today with the carries behind it, next to whatever was written on those
// days.
//
// What counts as run out is the last day, not the first: a task planned to run
// Monday to Friday is not late on Tuesday, so it is left alone until Saturday.
// A task several days stale moves straight to today rather than one day at a
// time: it logs the one move that actually happened instead of inventing days
// nothing was said on.
func (fs *fileStore) CarryForward(today, at string) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for i := range fs.data.Tasks {
		if needsCarry(fs.data.Tasks[i], today) {
			carried(&fs.data.Tasks[i], today, at)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, fs.save()
}
