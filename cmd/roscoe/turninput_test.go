package main

import (
	"context"
	"io"
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

// The watcher must return as soon as the turn ends, with no keypress, and
// must not have swallowed a key that arrives afterwards: that key belongs
// to the prompt that comes back.
func TestTurnInputReturnsWhenTheTurnEnds(t *testing.T) {
	keys := &keyReader{events: make(chan byte, 8)}
	sc := &screen{rows: 24, cols: 80, liveStart: -1, out: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pending, 1)
	go func() { done <- (&turnInput{}).run(ctx, sc, keys, func() {}) }()
	time.Sleep(30 * time.Millisecond)
	cancel() // the worker finished; nobody has typed
	select {
	case p := <-done:
		if !p.empty() {
			t.Errorf("pending = %+v, want nothing", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return when the turn ended; it is waiting for a keypress")
	}
	keys.events <- 'x'
	if got := keys.NextKey(); got != "x" {
		t.Errorf("the next key after the turn = %q, want x", got)
	}
}

// Typing during the turn still queues, and enter still commits.
func TestTurnInputQueuesWhileRunning(t *testing.T) {
	keys := &keyReader{events: make(chan byte, 16)}
	sc := &screen{rows: 24, cols: 80, liveStart: -1, out: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pending, 1)
	go func() { done <- (&turnInput{}).run(ctx, sc, keys, func() {}) }()
	for _, b := range []byte("later\r") {
		keys.events <- b
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	p := <-done
	if p.next() != "later" {
		t.Errorf("queued = %q", p.next())
	}
}

// Esc with a draft clears the draft; esc on an empty box stops the turn.
// Tab with a draft steers: the worker is interrupted and the line is what
// gets sent next.
func TestTurnInputEscAndTab(t *testing.T) {
	sc := &screen{rows: 24, cols: 80, liveStart: -1, out: io.Discard}

	keys := &keyReader{events: make(chan byte, 32)}
	cancelled := 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pending, 1)
	go func() { done <- (&turnInput{}).run(ctx, sc, keys, func() { cancelled++; cancel() }) }()
	feedKeys(keys, "draft")
	time.Sleep(60 * time.Millisecond)
	feedKeys(keys, "\x1b") // clears the draft, turn keeps running
	time.Sleep(120 * time.Millisecond)
	feedKeys(keys, "\x1b") // nothing to clear: stop
	p := <-done
	if !p.Esc || p.next() != "" || cancelled != 1 {
		t.Errorf("esc twice = %+v, cancelled %d", p, cancelled)
	}

	keys = &keyReader{events: make(chan byte, 32)}
	cancelled = 0
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan pending, 1)
	go func() { done <- (&turnInput{}).run(ctx, sc, keys, func() { cancelled++; cancel() }) }()
	feedKeys(keys, "\t") // an empty tab is ignored
	feedKeys(keys, "go left instead\t")
	p = <-done
	if p.Steer != "go left instead" || p.next() != "go left instead" || cancelled != 1 {
		t.Errorf("tab steer = %+v, cancelled %d", p, cancelled)
	}
}

// A bracketed paste's newlines are content, not sends; up and down scroll
// the screen rather than editing the line.
func TestTurnInputPasteAndScroll(t *testing.T) {
	sc := &screen{rows: 10, cols: 80, liveStart: -1, out: io.Discard, active: true}
	for i := 0; i < 40; i++ {
		sc.lines = append(sc.lines, "old")
	}
	keys := &keyReader{events: make(chan byte, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pending, 1)
	go func() { done <- (&turnInput{}).run(ctx, sc, keys, func() {}) }()
	feedKeys(keys, "\x1b[200~two\rlines\x1b[201~\r") // paste start, text with a newline, paste end, then enter
	feedKeys(keys, "\x1b[A")                         // up: scroll
	time.Sleep(150 * time.Millisecond)
	cancel()
	p := <-done
	if p.next() != "two\nlines" {
		t.Errorf("pasted queue = %q", p.next())
	}
	if sc.offset == 0 {
		t.Error("up during a turn should scroll the screen")
	}
}

func feedKeys(keys *keyReader, s string) {
	for _, b := range []byte(s) {
		keys.events <- b
	}
}
