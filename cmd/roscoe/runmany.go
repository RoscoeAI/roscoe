package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/accounts"
	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/pool"
	"roscoe.sh/roscoe/internal/streamjson"
	"roscoe.sh/roscoe/internal/worker"
)

// oneTask runs a single task and reports warm when its first response has
// landed. cmdRun supplies the real worker; tests supply a fake.
type oneTask func(ctx context.Context, t pool.Task, warm func()) (*streamjson.ResultEvent, error)

// taskIDs names the tasks of one multi-prompt run after their base id, so
// they sort together in roscoe sessions.
func taskIDs(base string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", base, i+1)
	}
	return ids
}

// runMany runs several prompts at once: at most limit together, the first
// alone until its first response so the others read the prompt prefix it
// wrote instead of writing their own. Each task gets its own worker and its
// own ledger. Streams are not interleaved; each task gets a start line, a
// done line, and its answer printed under its id at the end.
func runMany(ctx context.Context, out io.Writer, prompts []string, base string, limit int, who string, run oneTask) int {
	w := &lockedWriter{w: out} // tasks report from their own goroutines
	ids := taskIDs(base, len(prompts))
	tasks := make([]pool.Task, len(prompts))
	for i, p := range prompts {
		tasks[i] = pool.Task{ID: ids[i], Prompt: p}
	}
	fmt.Fprintf(w, "[tasks] %d prompts · %d at a time · %s · the first warms the prompt cache for the rest\n", len(tasks), limit, who)
	start := time.Now()
	n := len(tasks)
	results := pool.Run(ctx, tasks, pool.Options{Limit: limit}, func(ctx context.Context, t pool.Task, warm func()) (*streamjson.ResultEvent, error) {
		idx := indexOf(ids, t.ID) + 1
		fmt.Fprintf(w, "[%d/%d %s] started\n", idx, n, t.ID)
		res, err := run(ctx, t, warm)
		switch {
		case err != nil:
			fmt.Fprintf(w, "[%d/%d %s] failed · %v\n", idx, n, t.ID, err)
		case res == nil:
			fmt.Fprintf(w, "[%d/%d %s] no result\n", idx, n, t.ID)
		case res.IsError:
			fmt.Fprintf(w, "[%d/%d %s] error · $%.4f · %s\n", idx, n, t.ID, res.TotalCostUSD, oneLineOf(res.Result, 80))
		default:
			fmt.Fprintf(w, "[%d/%d %s] done · $%.4f · %s\n", idx, n, t.ID, res.TotalCostUSD, oneLineOf(res.Result, 80))
		}
		return res, err
	})

	// The answers, in prompt order, then the bill.
	failed := 0
	var total float64
	for i, r := range results {
		fmt.Fprintf(w, "\n%s · %s\n", r.Task.ID, oneLineOf(prompts[i], 70))
		switch {
		case r.Err != nil:
			failed++
			fmt.Fprintf(w, "  error: %v\n", r.Err)
		case r.Value == nil:
			failed++
			fmt.Fprintln(w, "  no result")
		default:
			if r.Value.IsError {
				failed++
			}
			total += r.Value.TotalCostUSD
			for _, line := range strings.Split(strings.TrimSpace(r.Value.Result), "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
	fmt.Fprintf(w, "\n[tasks] %d done, %d failed · $%.4f · %s\n", len(results)-failed, failed, total, time.Since(start).Round(time.Second))
	if failed > 0 {
		return 1
	}
	return 0
}

// lockedWriter serialises the per-task lines, which arrive from as many
// goroutines as there are running tasks.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// accountPool hands parallel workers their accounts: the least-loaded one
// with a free slot, so no account carries more than
// limits.per_account_max_concurrent at once, and a fleet with two accounts
// runs twice as wide as one. With no credentials every worker runs on
// claude's own login and nothing is limited here.
type accountPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	creds    []accounts.Credential
	inFlight []int
	cap      int // per account; <= 0 means unlimited
}

func newAccountPool(creds []accounts.Credential, perAccount int) *accountPool {
	p := &accountPool{creds: creds, inFlight: make([]int, len(creds)), cap: perAccount}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// acquire blocks until an account has room and returns it with a release.
// The zero credential (no accounts) is returned at once.
func (p *accountPool) acquire(ctx context.Context) (accounts.Credential, func()) {
	if len(p.creds) == 0 {
		return accounts.Credential{}, func() {}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		best := -1
		for i := range p.creds {
			if p.cap > 0 && p.inFlight[i] >= p.cap {
				continue
			}
			if best < 0 || p.inFlight[i] < p.inFlight[best] {
				best = i
			}
		}
		if best >= 0 {
			p.inFlight[best]++
			return p.creds[best], func() {
				p.mu.Lock()
				p.inFlight[best]--
				p.mu.Unlock()
				p.cond.Broadcast()
			}
		}
		if ctx.Err() != nil {
			return accounts.Credential{}, func() {}
		}
		// Wake when a slot frees, or poll so a cancelled context is noticed.
		go func() { time.Sleep(50 * time.Millisecond); p.cond.Broadcast() }()
		p.cond.Wait()
	}
}

// describe says what the workers run under and how wide that lets them go,
// for the run's header: the reason the parallelism is what it is.
func (p *accountPool) describe() string {
	if len(p.creds) == 0 {
		return "claude's own login"
	}
	names := make([]string, len(p.creds))
	for i, c := range p.creds {
		names[i] = c.Name
	}
	if p.cap <= 0 {
		return fmt.Sprintf("%d account(s): %s", len(p.creds), strings.Join(names, ", "))
	}
	return fmt.Sprintf("%d account(s): %s · %d each at once", len(p.creds), strings.Join(names, ", "), p.cap)
}

// slots is how many workers the accounts can carry at once; 0 means no
// account-imposed limit.
func (p *accountPool) slots() int {
	if len(p.creds) == 0 || p.cap <= 0 {
		return 0
	}
	return len(p.creds) * p.cap
}

// workerTask is the real oneTask: a worker per task with its own ledger and
// an account from the pool, warming the pool on the first assistant event,
// which is the first response the API returned and so the moment the shared
// prefix is cached.
func workerTask(cfg *config.Config, addr string, accts *accountPool, dir string) oneTask {
	return func(ctx context.Context, t pool.Task, warm func()) (*streamjson.ResultEvent, error) {
		cred, release := accts.acquire(ctx)
		defer release()
		account, token := cred.Name, cred.Token
		var led *ledger.Ledger
		if cfg.Reporting.Ledger != "" {
			p := config.ExpandPath(strings.ReplaceAll(cfg.Reporting.Ledger, "{run_id}", t.ID))
			if l, err := ledger.Open(filepath.Dir(p)); err == nil {
				led = l
				defer led.Close()
			}
		}
		return worker.Run(ctx,
			worker.Task{ID: t.ID, Prompt: t.Prompt, Dir: dir, Account: account, Token: token},
			worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led,
				OnNotice: func(m string) { fmt.Fprintf(os.Stderr, "[%s] %s\n", t.ID, m) },
				OnEvent: func(ev *streamjson.Event) {
					if ev.Type == "assistant" {
						warm()
					}
					if ie, ok := ev.AsInit(); ok {
						learnResolvedModel(cfg, cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model, ie.Model)
					}
				}})
	}
}
