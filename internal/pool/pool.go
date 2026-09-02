// Package pool runs several tasks at once inside one roscoe, which is what
// "multi-node" means here: the three tiers on this machine, with many workers
// and their calls in flight together. It is deterministic Go around whatever
// runs a task; no model decides the schedule.
//
// Two rules, both measured before they were written. At most limit tasks run
// at once (limits.max_parallel_tasks, and the per-account ceiling when one
// account serves them all). And the first task starts alone: two workers that
// start together on a cold prompt cache both pay the full prefix write
// ($0.2151 each on sonnet, 52,754 tokens), while a worker that starts after
// the first response has landed reads it for $0.0146. So the rest wait for
// the first worker's first response, or for it to finish, or for a timeout,
// whichever is first, and then run up to the limit.
package pool

import (
	"context"
	"sync"
	"time"
)

// Task is one unit of work by id.
type Task struct {
	ID     string
	Prompt string
}

// Result is what one task produced, in the caller's own result type.
type Result[R any] struct {
	Task    Task
	Value   R
	Err     error
	Elapsed time.Duration
}

// Runner runs one task. It calls warm once the task's first response has
// arrived, which is the moment the shared prompt prefix is cached and the
// other tasks may start without paying for it again. Calling warm more than
// once, or never, is fine.
type Runner[R any] func(ctx context.Context, t Task, warm func()) (R, error)

// Options bound the pool.
type Options struct {
	// Limit is how many tasks run at once; values below 1 mean 1.
	Limit int
	// WarmTimeout releases the waiting tasks if the first one has neither
	// answered nor finished by then. Zero means 20 seconds.
	WarmTimeout time.Duration
}

// Run executes every task and returns results in task order. It returns when
// all tasks have finished or ctx ends; tasks not started by then report
// ctx's error.
func Run[R any](ctx context.Context, tasks []Task, o Options, run Runner[R]) []Result[R] {
	limit := o.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > len(tasks) {
		limit = len(tasks)
	}
	warmTimeout := o.WarmTimeout
	if warmTimeout <= 0 {
		warmTimeout = 20 * time.Second
	}
	results := make([]Result[R], len(tasks))
	if len(tasks) == 0 {
		return results
	}

	// warmed closes when the rest may start: first response, first task
	// done, or the timeout.
	warmed := make(chan struct{})
	var warmOnce sync.Once
	release := func() { warmOnce.Do(func() { close(warmed) }) }

	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, t := range tasks {
		if i > 0 {
			select {
			case <-warmed:
			case <-time.After(warmTimeout):
				release()
			case <-ctx.Done():
				for j := i; j < len(tasks); j++ {
					results[j] = Result[R]{Task: tasks[j], Err: ctx.Err()}
				}
				wg.Wait()
				return results
			}
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			for j := i; j < len(tasks); j++ {
				results[j] = Result[R]{Task: tasks[j], Err: ctx.Err()}
			}
			wg.Wait()
			return results
		}
		wg.Add(1)
		go func(i int, t Task) {
			defer wg.Done()
			defer func() { <-slots }()
			start := time.Now()
			warm := func() {}
			if i == 0 {
				warm = release
				defer release() // finishing counts as warm too
			}
			v, err := run(ctx, t, warm)
			results[i] = Result[R]{Task: t, Value: v, Err: err, Elapsed: time.Since(start)}
		}(i, t)
	}
	wg.Wait()
	return results
}

// EffectiveLimit is how many tasks may run at once given the fleet limit,
// the per-account limit when one account serves every worker (0 when none
// does), and how many tasks there are. Never below 1.
func EffectiveLimit(maxParallel, perAccount, tasks int) int {
	n := maxParallel
	if n < 1 {
		n = 1
	}
	if perAccount > 0 && perAccount < n {
		n = perAccount
	}
	if tasks > 0 && tasks < n {
		n = tasks
	}
	return n
}
