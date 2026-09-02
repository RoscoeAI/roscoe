package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	code := runMany(context.Background(), &out, []string{"one", "two", "boom", "refused", "five"}, "task-x", 2, fake)
	text := out.String()
	if code != 1 {
		t.Errorf("exit %d with two failures", code)
	}
	if maxSeen > 2 {
		t.Errorf("%d ran at once, limit 2", maxSeen)
	}
	for _, want := range []string{
		"[tasks] 5 prompts · 2 at a time",
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
