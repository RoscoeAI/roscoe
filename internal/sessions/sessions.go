// Package sessions lists what roscoe has run, so a past conversation can be
// found and resumed without knowing its id. The source is the ledger: every
// run wrote when it started, which session the harness opened, what it cost
// and how many turns it took. The transcript adds what the conversation was
// about, since the ledger does not carry the prompt.
package sessions

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is one run, as reconstructed from its ledger.
type Session struct {
	TaskID  string
	ID      string // the harness session id, what --resume wants
	Started time.Time
	Ended   time.Time
	Dir     string
	Model   string
	Turns   int
	CostUSD float64
	// Kind is what produced the run: chat, loop, run, or "" when unknown.
	Kind string
	// About is the first thing the operator said, filled by an Enricher.
	About string
	// Bytes is the transcript's size on disk, when found.
	Bytes int64
	// Node is the fleet node the run happened on, when its ledger was brought
	// home from one; "" for runs made here.
	Node string
}

// Resumable reports whether --resume has something to resume.
func (s Session) Resumable() bool { return s.ID != "" }

// Enricher adds what the ledger cannot know, given the session id: the first
// prompt and the transcript size. Optional; nil leaves those fields empty.
type Enricher func(s *Session)

// List returns runs under runsDir, newest first, at most limit (0 = all).
// A ledger that cannot be read is skipped rather than failing the listing,
// because one bad file must not hide the other sixty-seven.
func List(runsDir string, limit int, enrich Enricher) ([]Session, error) {
	matches, err := filepath.Glob(filepath.Join(runsDir, "*", "events.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, path := range matches {
		s, ok := readLedger(path)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if enrich != nil {
		for i := range out {
			enrich(&out[i])
		}
	}
	return out, nil
}

// Latest is the most recent resumable session, or false.
func Latest(runsDir string, enrich Enricher) (Session, bool) {
	all, err := List(runsDir, 0, nil)
	if err != nil {
		return Session{}, false
	}
	for _, s := range all {
		if s.Resumable() {
			if enrich != nil {
				enrich(&s)
			}
			return s, true
		}
	}
	return Session{}, false
}

// readLedger folds one events.jsonl into a Session. It reads two record
// shapes: worker events wrapped in {ts, task, event:{...}}, and roscoe's own
// notes as top-level {ts, kind, ...}.
func readLedger(path string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()

	s := Session{TaskID: filepath.Base(filepath.Dir(path))}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var any bool
	for sc.Scan() {
		var rec struct {
			TS    string          `json:"ts"`
			Task  string          `json:"task"`
			Kind  string          `json:"kind"`
			Node  string          `json:"node"`
			Event json.RawMessage `json:"event"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		any = true
		// The home tag is written when the ledger is fetched, which may be
		// long after the run, so it does not count toward when the run was.
		if t, err := time.Parse(time.RFC3339Nano, rec.TS); err == nil && rec.Kind != "fleet.home" {
			if s.Started.IsZero() || t.Before(s.Started) {
				s.Started = t
			}
			if t.After(s.Ended) {
				s.Ended = t
			}
		}
		if rec.Task != "" {
			s.TaskID = rec.Task
		}
		if rec.Kind != "" {
			if strings.HasPrefix(rec.Kind, "loop.") && s.Kind == "" {
				s.Kind = "loop"
			}
			if rec.Kind == "fleet.home" {
				s.Node = rec.Node
			}
			continue
		}
		if len(rec.Event) == 0 || string(rec.Event) == "null" {
			continue
		}
		var ev struct {
			Type      string  `json:"type"`
			Subtype   string  `json:"subtype"`
			SessionID string  `json:"session_id"`
			Model     string  `json:"model"`
			CWD       string  `json:"cwd"`
			NumTurns  int     `json:"num_turns"`
			Cost      float64 `json:"total_cost_usd"`
		}
		if json.Unmarshal(rec.Event, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			if s.ID == "" && ev.SessionID != "" {
				s.ID = ev.SessionID
			}
			if ev.Model != "" {
				s.Model = ev.Model
			}
			if ev.CWD != "" {
				s.Dir = ev.CWD
			}
		case ev.Type == "result":
			// A chat has many result events, one per turn; a loop iteration
			// likewise. Sum them, and let the last session id win, since a
			// trimmed transcript resumes under a new one.
			s.Turns += ev.NumTurns
			s.CostUSD += ev.Cost
			if ev.SessionID != "" {
				s.ID = ev.SessionID
			}
		}
	}
	if !any {
		return Session{}, false
	}
	if s.Kind == "" && s.ID != "" {
		s.Kind = "chat/run"
	}
	return s, true
}

// Age renders how long ago a session ended, for a listing.
func Age(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h ago"
	default:
		return itoa(int(d.Hours()/24)) + "d ago"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
