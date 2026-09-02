package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tasks(n int) []Task {
	out := make([]Task, n)
	for i := range out {
		out[i] = Task{ID: string(rune('a' + i)), Prompt: "p"}
	}
	return out
}

// The first task runs alone until it reports its first response; then the
// rest run, never more than limit at once.
func TestFirstWarmsThenLimitHolds(t *testing.T) {
	var running, maxSeen int32
	var mu sync.Mutex
	started := map[string]time.Time{}
	firstWarmed := make(chan struct{})
	run := func(ctx context.Context, tk Task, warm func()) (string, error) {
		mu.Lock()
		started[tk.ID] = time.Now()
		mu.Unlock()
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxSeen)
			if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
				break
			}
		}
		if tk.ID == "a" {
			time.Sleep(60 * time.Millisecond) // "the first response"
			warm()
			close(firstWarmed)
			time.Sleep(60 * time.Millisecond) // keeps running after warming
		} else {
			time.Sleep(40 * time.Millisecond)
		}
		atomic.AddInt32(&running, -1)
		return "ok:" + tk.ID, nil
	}
	res := Run(context.Background(), tasks(6), Options{Limit: 3}, run)
	if len(res) != 6 {
		t.Fatalf("%d results", len(res))
	}
	for i, r := range res {
		if r.Err != nil || r.Value != "ok:"+tasks(6)[i].ID {
			t.Errorf("result %d = %+v", i, r)
		}
	}
	if maxSeen > 3 {
		t.Errorf("%d tasks ran at once, limit 3", maxSeen)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, at := range started {
		if id != "a" && at.Sub(started["a"]) < 55*time.Millisecond {
			t.Errorf("task %s started %s after a, before a's first response", id, at.Sub(started["a"]))
		}
	}
}

// A first task that never reports warm still releases the rest when it
// finishes, and one that hangs releases them at the timeout.
func TestReleaseOnFinishOrTimeout(t *testing.T) {
	run := func(ctx context.Context, tk Task, warm func()) (int, error) {
		if tk.ID == "a" {
			time.Sleep(30 * time.Millisecond) // finishes, never calls warm
		}
		return 1, nil
	}
	start := time.Now()
	Run(context.Background(), tasks(3), Options{Limit: 3, WarmTimeout: 5 * time.Second}, run)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("finish did not release the rest: %s", el)
	}

	hang := func(ctx context.Context, tk Task, warm func()) (int, error) {
		if tk.ID == "a" {
			time.Sleep(300 * time.Millisecond) // neither answers nor finishes in time
		}
		return 1, nil
	}
	start = time.Now()
	res := Run(context.Background(), tasks(3), Options{Limit: 3, WarmTimeout: 50 * time.Millisecond}, hang)
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Errorf("timeout did not release the rest: %s", el)
	}
	for _, r := range res {
		if r.Err != nil {
			t.Errorf("hang run errored: %v", r.Err)
		}
	}
}

// Cancelling stops scheduling; unstarted tasks report the context error and
// results stay in task order.
func TestCancelStopsScheduling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := func(ctx context.Context, tk Task, warm func()) (int, error) {
		if tk.ID == "a" {
			warm()
			cancel()
			return 1, nil
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	res := Run(ctx, tasks(4), Options{Limit: 2}, run)
	if res[0].Err != nil || res[0].Value != 1 {
		t.Errorf("first = %+v", res[0])
	}
	unstarted := 0
	for _, r := range res[1:] {
		if errors.Is(r.Err, context.Canceled) {
			unstarted++
		}
	}
	if unstarted == 0 {
		t.Error("no task reported the cancellation")
	}
}

func TestEffectiveLimit(t *testing.T) {
	cases := []struct{ max, acct, n, want int }{
		{4, 2, 10, 2}, // the account ceiling wins
		{4, 0, 10, 4}, // no single account: fleet limit
		{4, 2, 1, 1},  // one task
		{0, 0, 5, 1},  // nonsense config still runs one
		{8, 16, 3, 3}, // fewer tasks than either limit
	}
	for _, c := range cases {
		if got := EffectiveLimit(c.max, c.acct, c.n); got != c.want {
			t.Errorf("EffectiveLimit(%d,%d,%d) = %d, want %d", c.max, c.acct, c.n, got, c.want)
		}
	}
	if len(Run[int](context.Background(), nil, Options{}, nil)) != 0 {
		t.Error("no tasks should give no results")
	}
}
