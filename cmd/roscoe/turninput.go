package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// turnInput is the operator's input box while a turn is running.
//
// Before this, a running turn put the key reader in Esc-only mode: every other
// key was discarded while the prompt still drew a cursor, so the box looked
// editable and silently ate what you typed. A long turn with no output then
// looks identical to a hang.
//
// Now typing works throughout. Enter queues a message for when the turn ends;
// tab steers, interrupting at the next clean point to deliver it; esc stops
// without sending anything.
type turnInput struct {
	mu      sync.Mutex
	ed      lineEditor
	queued  []string
	steer   string
	stopped bool
	started time.Time
}

// pending is what the operator committed during the turn.
type pending struct {
	Steer  string   // deliver by interrupting now
	Queued []string // deliver after this turn finishes
	Esc    bool     // interrupt with nothing to say
}

func (p pending) empty() bool { return p.Steer == "" && len(p.Queued) == 0 && !p.Esc }

// next is what to send after the turn. Steering wins, since it was urgent
// enough to interrupt for; otherwise the queue goes in the order it was typed,
// as one message rather than several turns of one line each.
func (p pending) next() string {
	if p.Steer != "" {
		return p.Steer
	}
	return strings.Join(p.Queued, "\n\n")
}

// run reads keys until the turn's context ends, keeping the box live. It
// returns what the operator committed. cancel interrupts the worker, which
// esc and tab both do.
func (t *turnInput) run(ctx context.Context, sc *screen, keys *keyReader, cancel func()) pending {
	t.started = time.Now()
	done := make(chan struct{})

	// A heartbeat, because a turn that prints nothing for an hour must not
	// look the same as a turn that died.
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-tick.C:
				t.redraw(sc)
			}
		}
	}()

	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			key := keys.NextKey()
			t.mu.Lock()
			switch key {
			case "eof":
				t.mu.Unlock()
				return
			case "esc":
				if !t.ed.Empty() { // esc clears a draft before it stops the turn
					t.ed.Clear()
					t.mu.Unlock()
					t.redraw(sc)
					continue
				}
				t.stopped = true
				t.mu.Unlock()
				sc.Print(ansiDim + "esc · interrupting this turn…" + ansiReset)
				cancel()
				return
			case "enter":
				line := strings.TrimSpace(t.ed.String())
				t.ed.Clear()
				if line != "" {
					t.queued = append(t.queued, line)
				}
				t.mu.Unlock()
				if line != "" {
					sc.Printf("%squeued · sends when this turn ends%s", ansiFaint, ansiReset)
				}
				t.redraw(sc)
				continue
			case "tab":
				line := strings.TrimSpace(t.ed.String())
				if line == "" {
					t.mu.Unlock()
					continue
				}
				t.steer = line
				t.ed.Clear()
				t.mu.Unlock()
				sc.Printf("%ssteering · stopping at a clean point to say it%s", ansiFaint, ansiReset)
				cancel()
				return
			case "up", "down", "pgup", "pgdn":
				t.mu.Unlock()
				scrollBy(sc, key)
				continue
			default:
				if !t.ed.applyEditKey(key) {
					t.mu.Unlock()
					continue
				}
				t.mu.Unlock()
				t.redraw(sc)
				continue
			}
		}
	}()

	<-ctx.Done()
	<-done

	t.mu.Lock()
	defer t.mu.Unlock()
	return pending{Steer: t.steer, Queued: t.queued, Esc: t.stopped}
}

func scrollBy(sc *screen, key string) {
	switch key {
	case "up":
		sc.Scroll(-1)
	case "down":
		sc.Scroll(1)
	case "pgup":
		sc.Scroll(-10)
	case "pgdn":
		sc.Scroll(10)
	}
}

// redraw shows the turn is alive, how long it has been going, and what is
// waiting to be sent.
func (t *turnInput) redraw(sc *screen) {
	t.mu.Lock()
	buf, cur := t.ed.String(), t.ed.Cursor()
	n := len(t.queued)
	t.mu.Unlock()

	label := fmt.Sprintf("working %s ", elapsed(time.Since(t.started)))
	hint := "  enter queues · tab steers · esc stops"
	if buf != "" {
		hint = "  enter queues it · tab steers with it"
	}
	note := ""
	if n == 1 {
		note = "1 message queued for when this turn ends"
	} else if n > 1 {
		note = fmt.Sprintf("%d messages queued for when this turn ends", n)
	}
	sc.SetPromptCursor(label, buf, cur, hint, note)
}

// elapsed renders a duration compactly: 42s, 3m10s, 1h16m.
func elapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
