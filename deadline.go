package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

/* ---------- the daily deadline ----------

   Settings can name a time of day, the deadline. Once it has passed, Schedule
   sends a notification and the page opens the close-out sheet for that day,
   so every open task gets a verdict and a comment. It fires once per day, the
   first time Schedule sees the deadline behind it - so also when Schedule
   starts late, when the computer wakes up, or right after the deadline is set
   to a time already gone. The day it fired is stored, so a restart does not
   fire it again. */

// deadlineCheck is how often the clock is compared with the deadline.
const deadlineCheck = 15 * time.Second

// After the deadline, while tasks are still open, a reminder follows every
// remindEvery, up to maxReminders of them. Closing the day out - so nothing
// is left open on it - is what stops them.
const maxReminders = 3

var remindEvery = 30 * time.Minute

func init() {
	// For the tests, which cannot wait half an hour.
	if v, err := time.ParseDuration(os.Getenv("SCHEDULE_REMIND_EVERY")); err == nil && v > 0 {
		remindEvery = v
	}
}

// alert is a close-out the page has not picked up yet.
type alert struct {
	mu  sync.Mutex
	day string // the day to close out, "" when nothing is waiting
}

func (a *alert) set(day string) {
	a.mu.Lock()
	a.day = day
	a.mu.Unlock()
}

// take hands over the waiting day, if any, and clears it.
func (a *alert) take() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.day
	a.day = ""
	return d
}

// validDeadline accepts "" (no deadline) or a 24-hour "HH:MM".
func validDeadline(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("15:04", s)
	return err == nil && len(s) == 5
}

/* The Start of day hour moves where one day ends and the next begins, and
   which way it moves depends on the hour. A morning hour lets a day run late:
   with 06:00, the 14th runs until 06:00 on the 15th, so 02:00 still counts as
   the 14th. An afternoon or evening hour ends the day early: with 23:00, the
   14th runs from 23:00 on the 13th to 23:00 on the 14th, so 23:30 already
   counts as the 15th. Either way the day keeps the name of the date most of it
   falls on. */

// dayStartTime is when the named day begins.
func dayStartTime(d string, dayStart int) time.Time {
	v, _ := time.ParseInLocation(day, d, time.Local)
	start := time.Date(v.Year(), v.Month(), v.Day(), dayStart, 0, 0, 0, time.Local)
	if dayStart >= 12 {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

// logicalDay is the day the clock is in.
func logicalDay(now time.Time, dayStart int) string {
	if dayStart >= 12 {
		return now.Add(time.Duration(24-dayStart) * time.Hour).Format(day)
	}
	return now.Add(-time.Duration(dayStart) * time.Hour).Format(day)
}

// deadlineAt is when the deadline falls within the named day: the first time
// the clock shows it after that day has begun.
func deadlineAt(d string, dayStart int, deadline string) (time.Time, bool) {
	hm, err := time.Parse("15:04", deadline)
	if err != nil {
		return time.Time{}, false
	}
	start := dayStartTime(d, dayStart)
	at := time.Date(start.Year(), start.Month(), start.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
	if at.Before(start) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

// openWorkOn reports whether any unfinished task runs through day, which is
// what the close-out sheet would ask about. With nothing open there is no
// reason to interrupt anyone.
func openWorkOn(tasks []Task, day string) bool {
	return countOpenOn(tasks, day) > 0
}

// countOpenOn counts the unfinished tasks that run through day.
func countOpenOn(tasks []Task, day string) int {
	n := 0
	for _, t := range tasks {
		end := t.End
		if end == "" {
			end = t.Date
		}
		if (t.Status == "todo" || t.Status == "doing") && t.Date <= day && day <= end {
			n++
		}
	}
	return n
}

// watchDeadline runs for the life of the program.
func (s *server) watchDeadline() {
	fired := s.db.GetAlertDay()
	var lastFire time.Time // when the deadline or its last reminder went out
	reminders := 0
	tick := time.NewTicker(deadlineCheck)
	defer tick.Stop()
	for {
		now := time.Now()
		if day, ok := s.deadlineDue(now, fired); ok {
			fired, lastFire, reminders = day, now, 0
		} else if fired != "" && !lastFire.IsZero() && reminders < maxReminders &&
			now.Sub(lastFire) >= remindEvery {
			lastFire = now
			reminders++
			s.remind(fired, reminders)
		}
		select {
		case <-tick.C:
		case <-s.recheck:
		}
	}
}

// recheckDeadline makes the watcher look again now, so a deadline set to a
// time already gone fires straight away rather than on the next tick.
func (s *server) recheckDeadline() {
	select {
	case s.recheck <- struct{}{}:
	default:
	}
}

// deadlineDue fires the deadline if it has passed today and has not fired
// for today yet. It reports the day it dealt with.
func (s *server) deadlineDue(now time.Time, fired string) (string, bool) {
	p, err := s.db.GetPrefs()
	if err != nil || p.Deadline == "" {
		return "", false
	}
	today := logicalDay(now, p.DayStart)
	if today == fired {
		return "", false
	}
	at, ok := deadlineAt(today, p.DayStart, p.Deadline)
	if !ok || now.Before(at) {
		return "", false
	}
	tasks, err := s.db.ListTasks()
	if err != nil {
		log.Printf("deadline: %v", err)
		return "", false
	}
	if err := s.db.SetAlertDay(today); err != nil {
		log.Printf("deadline: %v", err)
	}
	s.fireDeadline(p, today, countOpenOn(tasks, today))
	return today, true
}

// remind repeats the deadline's notification if the day still has open
// tasks, and goes quiet once it does not.
func (s *server) remind(day string, n int) {
	p, err := s.db.GetPrefs()
	if err != nil || p.Deadline == "" || logicalDay(time.Now(), p.DayStart) != day {
		return
	}
	tasks, err := s.db.ListTasks()
	if err != nil {
		return
	}
	open := countOpenOn(tasks, day)
	if open == 0 {
		return
	}
	log.Printf("reminder %d of %d: %d task(s) still open on %s", n, maxReminders, open, day)
	s.alert.set(day)
	body := fmt.Sprintf("Still %d open task%s to settle. Click to close out the day.", open, plural(open))
	title := "Reminder: deadline " + p.Deadline
	if n == maxReminders {
		title = "Last reminder: deadline " + p.Deadline
	}
	if !s.canNotify || !notify(title, body) {
		s.show()
	}
}

func (s *server) fireDeadline(p Prefs, today string, open int) {
	if open == 0 {
		log.Printf("deadline %s passed with nothing open", p.Deadline)
		return
	}
	log.Printf("deadline %s passed; asking to close out %s", p.Deadline, today)
	s.alert.set(today)

	// A notification rather than a window in your face: the sheet opens
	// when you click it, or straight away if Schedule is already open.
	body := fmt.Sprintf("%d open task%s to settle. Click to close out the day.", open, plural(open))
	if !s.canNotify || !notify("Deadline "+p.Deadline, body) {
		s.show()
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// alertHandler is polled by the page. It answers with the day to close out,
// or "", and clears it so the sheet opens once. It also carries the change
// number, which is how the page learns the board was changed elsewhere; see
// refresh.go.
func (s *server) alertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"day": s.alert.take(), "rev": s.rev()})
}
