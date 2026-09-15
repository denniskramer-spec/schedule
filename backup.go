package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

/* ---------- backup and restore ----------

   Settings > Data has Save backup, which hands the browser the whole board
   as one JSON file - the same shape as the data file - and Restore, which
   takes such a file and replaces everything with it. Restoring first keeps a
   copy of what is being replaced beside the daily backups, so it can be
   undone by restoring that. */

// maxImport is the largest backup file accepted: far above any real board.
const maxImport = 50 << 20

func (s *server) exportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tasks, err := s.db.ListTasks()
	if err != nil {
		dbFail(w, err)
		return
	}
	p, err := s.db.GetPrefs()
	if err != nil {
		dbFail(w, err)
		return
	}
	doc := fileData{Version: 1, Prefs: p, AlertDay: s.db.GetAlertDay(), Tasks: tasks}
	name := "schedule-backup-" + time.Now().Format("2006-01-02") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(doc)
}

// importHandler replaces the board with the backup in the request body.
func (s *server) importHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var doc fileData
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImport))
	if err := dec.Decode(&doc); err != nil {
		fail(w, http.StatusBadRequest, "that is not a Schedule backup: "+err.Error())
		return
	}
	if doc.Tasks == nil {
		fail(w, http.StatusBadRequest, "that is not a Schedule backup: it has no tasks list")
		return
	}
	doc.Prefs.normalize()
	if !validDeadline(doc.Prefs.Deadline) {
		fail(w, http.StatusBadRequest, "the backup's deadline is not valid")
		return
	}
	if doc.Prefs.DayStart < 0 || doc.Prefs.DayStart > 23 {
		fail(w, http.StatusBadRequest, "the backup's start of day is not valid")
		return
	}
	if msg := validateCategories(doc.Prefs.Categories); msg != "" {
		fail(w, http.StatusBadRequest, "the backup's categories: "+msg)
		return
	}
	seen := map[string]bool{}
	for i, t := range doc.Tasks {
		if t.End == "" {
			doc.Tasks[i].End = t.Date
			t.End = t.Date
		}
		if msg := validate(t); msg != "" {
			fail(w, http.StatusBadRequest, fmt.Sprintf("task %d (%q): %s", i+1, t.Title, msg))
			return
		}
		if seen[t.ID] {
			fail(w, http.StatusBadRequest, fmt.Sprintf("task %d (%q): its id appears twice", i+1, t.Title))
			return
		}
		seen[t.ID] = true
	}
	if err := s.db.ReplaceAll(doc.Tasks, doc.Prefs, doc.AlertDay); err != nil {
		dbFail(w, err)
		return
	}
	s.recheckDeadline()
	s.changed(w)
	writeJSON(w, http.StatusOK, map[string]any{"tasks": len(doc.Tasks)})
}
