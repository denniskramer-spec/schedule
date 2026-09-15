package main

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

/* ---------- live refresh ----------

   The page draws from its own copy of the board and writes through. To see
   changes it did not make - a second window, another machine on the same
   MongoDB, a synced folder delivering a changed data file - it needs telling.
   The server keeps a change number: every write through a handler bumps it,
   and a watcher compares the store's fingerprint every few seconds and bumps
   it when someone else has changed things. Every response carries the number
   in X-Rev, and the page's regular alert poll brings it too; when the poll's
   number is ahead of the last one the page saw, it reloads. A page's own
   writes never make it reload, because their responses carry the new number
   already. */

const fingerprintEvery = 5 * time.Second

type changes struct {
	mu  sync.Mutex
	rev uint64
	fp  string
}

// rev is the current change number.
func (s *server) rev() uint64 {
	s.chg.mu.Lock()
	defer s.chg.mu.Unlock()
	return s.chg.rev
}

// changed records a write made through this server: the number moves on and
// the fingerprint is brought up to date so the watcher does not count the
// same change twice. It also stamps the response, so the page that made the
// change learns the new number and does not reload for its own work.
func (s *server) changed(w http.ResponseWriter) {
	fp, err := s.db.Fingerprint()
	s.chg.mu.Lock()
	s.chg.rev++
	if err == nil {
		s.chg.fp = fp
	}
	rev := s.chg.rev
	s.chg.mu.Unlock()
	w.Header().Set("X-Rev", strconv.FormatUint(rev, 10))
}

// watchChanges notices writes that did not come through this server.
func (s *server) watchChanges() {
	fp, err := s.db.Fingerprint()
	s.chg.mu.Lock()
	if err == nil && s.chg.fp == "" {
		s.chg.fp = fp
	}
	s.chg.mu.Unlock()
	tick := time.NewTicker(fingerprintEvery)
	defer tick.Stop()
	for range tick.C {
		fp, err := s.db.Fingerprint()
		if err != nil {
			continue
		}
		s.chg.mu.Lock()
		if fp != s.chg.fp {
			s.chg.fp = fp
			s.chg.rev++
			log.Printf("the board was changed elsewhere; pages will reload (rev %d)", s.chg.rev)
		}
		s.chg.mu.Unlock()
	}
}

// withRev stamps every response with the change number, so the page can
// keep in step from any request it makes. Handlers that write call changed,
// which overwrites it with the number after their write.
func (s *server) withRev(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rev", strconv.FormatUint(s.rev(), 10))
		next.ServeHTTP(w, r)
	})
}
