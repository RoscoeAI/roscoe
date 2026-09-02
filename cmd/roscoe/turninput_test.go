package main

import (
	"strings"
	"testing"
	"time"
)

// Steering was urgent enough to interrupt for, so it wins over anything merely
// queued.
func TestPendingNextPrefersSteering(t *testing.T) {
	p := pending{Steer: "stop and do X instead", Queued: []string{"also this", "and this"}}
	if got := p.next(); got != "stop and do X instead" {
		t.Errorf("next = %q", got)
	}
}

// Several queued lines become one message, not one turn each: three turns of
// one line apiece would cost three workers to say one thing.
func TestPendingNextJoinsTheQueue(t *testing.T) {
	p := pending{Queued: []string{"first", "second"}}
	got := p.next()
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("next = %q, want both", got)
	}
	if strings.Count(got, "first") != 1 {
		t.Errorf("next = %q, duplicated", got)
	}
}

func TestPendingEmpty(t *testing.T) {
	if !(pending{}).empty() {
		t.Error("a bare pending should be empty")
	}
	if (pending{Esc: true}).empty() {
		t.Error("esc is something the operator did")
	}
	if got := (pending{}).next(); got != "" {
		t.Errorf("next of nothing = %q", got)
	}
	if got := (pending{Esc: true}).next(); got != "" {
		t.Errorf("esc alone should send nothing, got %q", got)
	}
}

// The heartbeat exists so an hour of silence never looks like a hang, which
// means it has to stay readable at an hour.
func TestElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		3 * time.Second:                "3s",
		59 * time.Second:               "59s",
		90 * time.Second:               "1m30s",
		10*time.Minute + 5*time.Second: "10m05s",
		time.Hour + 16*time.Minute:     "1h16m",
		2*time.Hour + 3*time.Minute:    "2h03m",
	}
	for d, want := range cases {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}
