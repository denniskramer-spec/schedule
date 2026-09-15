package main

import (
	"time"
)

/* ---------- repeating tasks ----------

   A task with Repeat set is the head of a series: every day, weekdays, every
   week or every month from its first day. Its later occurrences are made
   ahead of time as ordinary tasks - each with its own column, comments and
   carries - marked with Series, the head's id, so the board, the week and
   the month show them like anything else planned.

   Instances are made up to repeatHorizonDays ahead, at start and at midnight
   (the same sync that carries unfinished work). The head remembers the last
   day it has been extended to, so a deleted instance is not put back and a
   changed rule starts afresh from today. The first extension of a head dated
   in the past starts from today: missed days are not filled in. */

const repeatHorizonDays = 62

var validRepeat = map[string]bool{"": true, "daily": true, "weekdays": true, "weekly": true, "monthly": true}

// instanceID is the id of the series' occurrence on a day, so making the
// same day twice is impossible.
func instanceID(head, day string) string { return head + "." + day }

// nextOccurrence is the first day after d on which the series falls.
// anchor is the head's first day, which fixes the weekday and the day of the
// month.
func nextOccurrence(rule, d, anchor string) string {
	from, err := time.ParseInLocation(day, d, time.Local)
	if err != nil {
		return ""
	}
	switch rule {
	case "daily":
		return from.AddDate(0, 0, 1).Format(day)
	case "weekdays":
		n := from.AddDate(0, 0, 1)
		for n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
			n = n.AddDate(0, 0, 1)
		}
		return n.Format(day)
	case "weekly":
		a, err := time.ParseInLocation(day, anchor, time.Local)
		if err != nil {
			return from.AddDate(0, 0, 7).Format(day)
		}
		// The anchor's weekday, strictly after d.
		n := from.AddDate(0, 0, 1)
		for n.Weekday() != a.Weekday() {
			n = n.AddDate(0, 0, 1)
		}
		return n.Format(day)
	case "monthly":
		a, err := time.ParseInLocation(day, anchor, time.Local)
		if err != nil {
			a = from
		}
		// The anchor's day of the month, or the month's last day when it is
		// shorter: a task on the 31st falls on 30 April.
		cur := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.Local)
		for i := 0; i < 2; i++ {
			last := time.Date(cur.Year(), cur.Month()+1, 0, 0, 0, 0, 0, time.Local).Day()
			dd := a.Day()
			if dd > last {
				dd = last
			}
			cand := time.Date(cur.Year(), cur.Month(), dd, 0, 0, 0, 0, time.Local)
			if cand.After(from) {
				return cand.Format(day)
			}
			cur = cur.AddDate(0, 1, 0)
		}
	}
	return ""
}

// newInstances is what extending a head up to horizon adds: the occurrences
// after the last day it was extended to (or after its own first day, but
// never before today), and the day it is now extended to. now stamps the
// instances' createdAt.
func newInstances(head Task, today, horizon, now string) ([]Task, string) {
	if !validRepeat[head.Repeat] || head.Repeat == "" {
		return nil, head.RepeatedThrough
	}
	from := head.RepeatedThrough
	if from == "" {
		from = head.Date
		if yesterday := prevDay(today); from < yesterday {
			from = yesterday
		}
	}
	length := 0
	if s, err := time.ParseInLocation(day, head.Date, time.Local); err == nil {
		if e, err := time.ParseInLocation(day, head.End, time.Local); err == nil && e.After(s) {
			length = int(e.Sub(s).Hours() / 24)
		}
	}
	var out []Task
	through := from
	for occ := nextOccurrence(head.Repeat, from, head.Date); occ != "" && occ <= horizon; occ = nextOccurrence(head.Repeat, occ, head.Date) {
		t := head
		t.ID = instanceID(head.ID, occ)
		t.Date = occ
		t.End = occ
		if length > 0 {
			if d, err := time.ParseInLocation(day, occ, time.Local); err == nil {
				t.End = d.AddDate(0, 0, length).Format(day)
			}
		}
		t.Status = "todo"
		t.Log = []Entry{}
		t.CreatedAt = now
		t.FinishedAt = ""
		t.Repeat = ""
		t.Series = head.ID
		t.RepeatedThrough = ""
		out = append(out, t)
		through = occ
	}
	if through < from {
		through = from
	}
	return out, through
}

func prevDay(d string) string {
	v, err := time.ParseInLocation(day, d, time.Local)
	if err != nil {
		return d
	}
	return v.AddDate(0, 0, -1).Format(day)
}

// repeatHorizon is how far ahead instances are made, counted from today.
func repeatHorizon(today string) string {
	v, err := time.ParseInLocation(day, today, time.Local)
	if err != nil {
		return today
	}
	return v.AddDate(0, 0, repeatHorizonDays).Format(day)
}
