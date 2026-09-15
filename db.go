package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is one dated line in a task's history. A task that keeps slipping
// accumulates one entry per day, so the detail sheet can show what was said
// about it day by day rather than a single comment that gets overwritten.
//
// Kinds:
//
//	note   something the user wrote on that day
//	carry  the task rolled over to a later day unfinished
//	move   the user dragged it to a different day on purpose
type Entry struct {
	Kind   string `json:"kind"   bson:"kind"`
	Date   string `json:"date"   bson:"date"` // the day it belongs to
	At     string `json:"at"     bson:"at"`   // when it was written
	Text   string `json:"text"   bson:"text,omitempty"`
	From   string `json:"from"   bson:"from,omitempty"`   // carry/move: the day it left
	Status string `json:"status" bson:"status,omitempty"` // status at the time
}

// Task mirrors the JavaScript task object used by the UI. The JSON tags keep
// the API in camelCase; the bson tags are the field names in MongoDB. The
// task's own id is the document _id, so there is no second identifier.
type Task struct {
	ID     string `json:"id"         bson:"_id"`
	Title  string `json:"title"      bson:"title"`
	Date   string `json:"date"       bson:"date"` // first day
	End    string `json:"end"        bson:"end"`  // last day; equal to date for a one-day task
	Status string `json:"status"     bson:"status"`
	Agent  bool   `json:"agent"      bson:"agent"`
	// Category is the ID of the board row the task sits in, one of
	// Prefs.Categories. Tasks from before categories decode as 0, the first.
	Category   int     `json:"category"   bson:"category"`
	Log        []Entry `json:"log"        bson:"log"`
	CreatedAt  string  `json:"createdAt"  bson:"createdAt"`
	FinishedAt string  `json:"finishedAt" bson:"finishedAt"`

	// Repeat makes the task the head of a series - "daily", "weekdays",
	// "weekly" or "monthly" - whose later occurrences are made ahead of time
	// as tasks of their own, each carrying Series, the head's id. The head
	// keeps RepeatedThrough, the last day it has been extended to. See
	// repeat.go.
	Repeat          string `json:"repeat"          bson:"repeat,omitempty"`
	Series          string `json:"series"          bson:"series,omitempty"`
	RepeatedThrough string `json:"repeatedThrough" bson:"repeatedThrough,omitempty"`

	// Note and NoteAt are the single comment kept by versions before the
	// history log. They are decoded so old documents still load; migrate
	// folds them into Log at startup and nothing writes them again.
	Note   string `json:"-" bson:"note,omitempty"`
	NoteAt string `json:"-" bson:"noteAt,omitempty"`
}

type Category struct {
	ID   int    `json:"id"   bson:"id"`
	Name string `json:"name" bson:"name"`
}

type Prefs struct {
	Ask   bool `json:"ask"   bson:"ask"`
	Carry bool `json:"carry" bson:"carry"`
	// Deadline is the time of day, "HH:MM", when the close-out sheet opens
	// by itself. Empty means never; see deadline.go.
	Deadline string `json:"deadline" bson:"deadline"`
	// Categories are the rows the board is divided into, in order. Each has a
	// stable ID that tasks refer to, so rows can be renamed, added, removed
	// and the rest keep their tasks.
	Categories []Category `json:"categories" bson:"cats"`
	// LegacyCategories is how 1.4-1.6 stored the three names; getPrefs turns
	// it into Categories, with IDs 0-2 matching the tasks already filed.
	LegacyCategories []string `json:"-" bson:"categories,omitempty"`
	// DayStart is the hour, 0-23, a day begins at. Before it, the clock still
	// counts as the previous day for "today", the day's lists and carrying.
	DayStart int `json:"dayStart" bson:"dayStart"`
}

/* ---------- where tasks live ----------

   By default in one JSON file under the user's own app-data folder, which
   needs nothing installed and backs up with a copy. A MongoDB server is the
   alternative, for people who want the board shared between machines:
   "schedule.exe -mongo mongodb://host:27017". Both sit behind Store. */

type Store interface {
	Close()
	Label() string // what to show the user: the file's path, or the server
	ListTasks() ([]Task, error)
	GetTask(id string) (Task, error)
	InsertTask(t Task) error
	ReplaceTask(id string, t Task) error
	DeleteTask(id string) error
	DeleteAllTasks() error
	GetPrefs() (Prefs, error)
	SetPrefs(p Prefs) error
	// The day the deadline last fired, so it fires once a day across restarts.
	GetAlertDay() string
	SetAlertDay(d string) error
	// SetCategory files the given tasks under one category: deleting a row,
	// and undoing that.
	SetCategory(ids []string, category int) error
	// ReplaceAll swaps in a whole board: restoring a backup, and the import
	// from MongoDB.
	ReplaceAll(tasks []Task, p Prefs, alertDay string) error
	// CarryForward moves every unfinished task that has run out of days onto
	// today and records the move in its history; see fileStore.CarryForward.
	CarryForward(today, at string) (int, error)
	// AddRepeats makes the occurrences of every repeating task up to horizon
	// that do not exist yet; see repeat.go. It reports how many it made.
	AddRepeats(today, horizon, now string) (int, error)
	// Fingerprint identifies the board's contents, so a change made elsewhere
	// - another machine on the same MongoDB, a synced folder delivering a
	// changed data file - can be noticed; see refresh.go. The file store
	// re-reads its file when the file has changed.
	Fingerprint() (string, error)
}

var (
	errNoRow     = errors.New("task not found")
	errDuplicate = errors.New("a task with that id already exists")
	// errConflict: the data file changed on disk behind Schedule's back - a
	// second copy on another machine through a synced folder, say. The
	// change that found it out is dropped and the file re-read, so the
	// board shows what is really there; see fileStore.save.
	errConflict = errors.New("the data file was changed by another program since Schedule read it. " +
		"The board has been reloaded from it, and this change was not saved")
)

const (
	dataFile  = "schedule.json"
	opTimeout = 10 * time.Second

	// stamp is how every timestamp in the database is written: local time, to
	// the minute, sortable as a string.
	stamp = "2006-01-02 15:04"
	day   = "2006-01-02"

	// A task carried for years would otherwise grow an unbounded history, so
	// the log keeps the most recent maxLogLen entries.
	maxLogLen    = 500
	maxEntryText = 4000
	maxTitleLen  = 500

	// The board has up to this many category rows, named in Settings.
	maxCategories   = 12
	maxCategoryID   = 999
	maxCategoryName = 40

	// A task may run over a range of days. The cap is there to catch a
	// mistyped year, not to be a real limit.
	maxSpanDays = 365
)

func defaultCategories() []Category {
	return []Category{{0, "Job"}, {1, "AI training"}, {2, "Assets"}}
}

// defaultPrefs: carrying is on unless the user has turned it off, because an
// unfinished task belongs on the next day, not stranded on a date nobody
// looks at again.
func defaultPrefs() Prefs {
	return Prefs{Ask: true, Carry: true, Categories: defaultCategories()}
}

// normalize fills in what an older or empty settings record lacks.
func (p *Prefs) normalize() {
	if len(p.Categories) == 0 {
		for i, name := range p.LegacyCategories {
			p.Categories = append(p.Categories, Category{ID: i, Name: name})
		}
	}
	if len(p.Categories) == 0 {
		p.Categories = defaultCategories()
	}
	p.LegacyCategories = nil
}

// trimLog makes a log safe to store: never nil, and never longer than the
// cap, so a task carried for years cannot grow without bound.
func trimLog(l []Entry) []Entry {
	if l == nil {
		return []Entry{}
	}
	if len(l) > maxLogLen {
		return l[len(l)-maxLogLen:]
	}
	return l
}

// carried is the change carrying makes to one task that has run out of days:
// only the last day moves, so the range still says when the work started; a
// one-day task has both on the same date and so moves whole.
func carried(t *Task, today, at string) {
	if t.Date == t.End {
		t.Date = today
	}
	e := Entry{Kind: "carry", Date: today, At: at, From: t.End, Status: t.Status}
	t.End = today
	t.Log = trimLog(append(t.Log, e))
}

func needsCarry(t Task, today string) bool {
	return t.End < today && (t.Status == "todo" || t.Status == "doing")
}

// argValue finds "-name value" or "-name=value" on the command line, with one
// or two dashes. Empty if absent.
func argValue(name string) string {
	args := os.Args[1:]
	for i, a := range args {
		for _, dash := range []string{"-", "--"} {
			if a == dash+name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, dash+name+"="); ok {
				return v
			}
		}
	}
	return ""
}

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == "-"+name || a == "--"+name {
			return true
		}
	}
	return false
}

// dataPath is where the JSON file lives: -data, then SCHEDULE_DATA, then the
// app's own folder.
func dataPath() string {
	if v := argValue("data"); v != "" {
		return v
	}
	if v := os.Getenv("SCHEDULE_DATA"); v != "" {
		return v
	}
	return filepath.Join(appDir, dataFile)
}

// openData picks the store for this run. The MongoDB path is only taken when
// asked for. On the first run with a file, tasks in a local MongoDB - where
// versions up to 1.7 kept them - are brought across, so upgrading loses
// nothing.
func openData(atLogin bool) (Store, error) {
	if uri := mongoURI(); uri != "" {
		db, err := openMongo(uri, opTimeout)
		if err != nil && atLogin {
			// At sign-in the MongoDB service is often still starting, so give
			// it time before reporting anything.
			db, err = waitForMongo(uri, 3*time.Minute)
		}
		if err != nil {
			return nil, err
		}
		return db, nil
	}

	path := dataPath()
	_, statErr := os.Stat(path)
	fs, err := openFileStore(path)
	if err != nil {
		return nil, err
	}
	if os.IsNotExist(statErr) {
		if n := importFromMongo(fs); n > 0 {
			log.Printf("brought %d task(s) across from MongoDB into %s", n, path)
		}
	}
	return fs, nil
}

// importFromMongo copies tasks and settings out of a MongoDB on this machine,
// if there is one answering, into a fresh file store.
func importFromMongo(fs *fileStore) int {
	m, err := openMongo(localMongoURI, 2*time.Second)
	if err != nil {
		return 0
	}
	defer m.Close()
	tasks, err := m.ListTasks()
	if err != nil || len(tasks) == 0 {
		return 0
	}
	p, err := m.GetPrefs()
	if err != nil {
		p = defaultPrefs()
	}
	if err := fs.ReplaceAll(tasks, p, m.GetAlertDay()); err != nil {
		log.Printf("could not import from MongoDB: %v", err)
		return 0
	}
	return len(tasks)
}

// configDir is where per-user files that are not task data belong: the app
// window keeps its browser profile there, and by default the data file too.
func configDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cfg, "Schedule")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
