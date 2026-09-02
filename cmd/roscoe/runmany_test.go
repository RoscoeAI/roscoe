package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/accounts"
	"roscoe.sh/roscoe/internal/pool"
	"roscoe.sh/roscoe/internal/streamjson"
)

// Several prompts run through the pool: each named after the base id, each
// reported as it starts and finishes, answers printed in prompt order with the
// bill, and the exit code says whether every task succeeded.
func TestRunManyReportsEveryTask(t *testing.T) {
	var running, maxSeen int32
	fake := func(ctx context.Context, tk pool.Task, warm func()) (*streamjson.ResultEvent, error) {
		n := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		for {
			m := atomic.LoadInt32(&maxSeen)
			if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
				break
			}
		}
		warm()
		time.Sleep(20 * time.Millisecond)
		switch tk.Prompt {
		case "boom":
			return nil, errors.New("worker exploded")
		case "refused":
			return &streamjson.ResultEvent{IsError: true, Result: "Prompt is too long", TotalCostUSD: 0}, nil
		}
		return &streamjson.ResultEvent{Result: "answer to " + tk.Prompt, TotalCostUSD: 0.01}, nil
	}
	var out bytes.Buffer
	code := runMany(context.Background(), &out, []string{"one", "two", "boom", "refused", "five"}, "task-x", 2, "2 account(s): a, b · 1 each at once", fake)
	text := out.String()
	if code != 1 {
		t.Errorf("exit %d with two failures", code)
	}
	if maxSeen > 2 {
		t.Errorf("%d ran at once, limit 2", maxSeen)
	}
	for _, want := range []string{
		"[tasks] 5 prompts · 2 at a time · 2 account(s): a, b · 1 each at once",
		"task-x-1] started", "task-x-5] started",
		"task-x-1] done · $0.0100 · answer to one",
		"task-x-3] failed · worker exploded",
		"task-x-4] error · $0.0000 · Prompt is too long",
		"\ntask-x-2 · two\n  answer to two",
		"[tasks] 3 done, 2 failed · $0.0300",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	// Answers come out in prompt order, whatever order they finished in.
	if strings.Index(text, "\ntask-x-1 · one") > strings.Index(text, "\ntask-x-2 · two") {
		t.Error("answers are not in prompt order")
	}
	if ids := taskIDs("t", 3); strings.Join(ids, ",") != "t-1,t-2,t-3" {
		t.Errorf("taskIDs = %v", ids)
	}
}

// Two accounts with room for one each carry two workers at once, spread
// across both; a third waits for a release. No accounts means no limit.
func TestAccountPoolSpreadsAndCaps(t *testing.T) {
	creds := []accounts.Credential{{Name: "a", Token: "ta"}, {Name: "b", Token: "tb"}}
	p := newAccountPool(creds, 1)
	if p.slots() != 2 {
		t.Fatalf("slots = %d", p.slots())
	}
	c1, r1 := p.acquire(context.Background())
	c2, r2 := p.acquire(context.Background())
	if c1.Name == c2.Name {
		t.Errorf("both workers got %s; the second should take the other account", c1.Name)
	}
	got := make(chan string, 1)
	go func() { c3, r3 := p.acquire(context.Background()); got <- c3.Name; r3() }()
	select {
	case n := <-got:
		t.Fatalf("third worker got %s while both accounts were full", n)
	case <-time.After(80 * time.Millisecond):
	}
	r1()
	select {
	case n := <-got:
		if n != c1.Name {
			t.Errorf("third worker got %s, want the freed %s", n, c1.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third worker never got the freed account")
	}
	r2()
	// Cancelled while waiting: returns rather than hanging.
	c4, r4 := p.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, r5 := p.acquire(ctx) // b is free, so this one succeeds regardless
	r5()
	r4()
	_ = c4
	if got := p.describe(); got != "2 account(s): a, b · 1 each at once" {
		t.Errorf("describe = %q", got)
	}
	// No credentials: everything runs on claude's own login, unlimited.
	none := newAccountPool(nil, 2)
	if got := none.describe(); got != "claude's own login" {
		t.Errorf("no-account describe = %q", got)
	}
	if none.slots() != 0 {
		t.Errorf("no accounts should impose no limit, got %d", none.slots())
	}
	if c, r := none.acquire(context.Background()); c.Name != "" {
		t.Errorf("no-account pool returned %q", c.Name)
	} else {
		r()
	}
}
