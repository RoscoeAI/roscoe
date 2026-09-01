package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/loop"
	"roscoe.sh/roscoe/internal/memory"
	"roscoe.sh/roscoe/internal/quorum"
	"roscoe.sh/roscoe/internal/router"
	"roscoe.sh/roscoe/internal/streamjson"
	"roscoe.sh/roscoe/internal/worker"
)

// cmdLoop runs a charter to completion instead of a prompt to an answer: the
// supervisor dispatches an iteration, reads what the worker left in loop.md,
// judges it, and dispatches again. Esc stops at a clean point.
func cmdLoop(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("loop", flag.ExitOnError)
	dir := fl.String("dir", "", "working directory (default: current directory)")
	taskID := fl.String("task-id", "", "task id (default: generated)")
	harness := fl.String("harness", "", `worker harness: "claude" (default) or "codex"`)
	maxIter := fl.Int("max-iterations", loop.DefaultMaxIterations, "stop after this many iterations; -1 for no ceiling")
	budget := fl.Float64("budget", 0, "stop once the run has spent this many dollars (0: no ceiling)")
	once := fl.Bool("once", false, "run a single iteration and stop, whatever the status says")
	noQuorum := fl.Bool("no-quorum", false, "judge with the worker's own status line instead of the model quorum")
	_ = fl.Parse(args)

	rest := fl.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, `usage: roscoe loop "<charter>" [--max-iterations N] [--budget USD] [--dir D]`)
		return 2
	}
	charter := rest[0]
	if len(rest) > 1 {
		_ = fl.Parse(rest[1:])
	}

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe loop: %v\n", err)
		return 1
	}
	if *harness != "" {
		cfg.Tiers.Middle.Harness = *harness
	}
	if *taskID == "" {
		*taskID = newTaskID()
	}
	if *dir == "" {
		if *dir, err = os.Getwd(); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe loop: getwd: %v\n", err)
			return 1
		}
	}

	var led *ledger.Ledger
	if cfg.Reporting.Ledger != "" {
		p := config.ExpandPath(strings.ReplaceAll(cfg.Reporting.Ledger, "{run_id}", *taskID))
		if led, err = ledger.Open(filepath.Dir(p)); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe loop: open ledger: %v\n", err)
			return 1
		}
		defer led.Close()
	}

	r, err := router.New(router.Options{Cfg: cfg, Env: env})
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe loop: %v\n", err)
		return 1
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.ListenAndServe(rctx) }()
	addr, err := waitHealthz(ctx, r.Addr, errCh, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe loop: %v\n", err)
		return 1
	}

	account, token := resolveMiddleAccount(cfg, env)
	fmt.Fprintf(os.Stderr, "[router] listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "[loop] %s dir=%s · %s\n", *taskID, *dir, loop.Path(*dir))

	// Esc stops the loop at the end of the current iteration, so the worker
	// finishes writing loop.md rather than being cut off mid-thought.
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	var stopping atomic.Bool
	if isTTY(os.Stdin) {
		if keys, restoreTTY, kerr := newKeyReader(); kerr == nil {
			defer restoreTTY()
			fmt.Fprintln(os.Stderr, "[keys] esc stops after the current iteration")
			go func() {
				if keys.WaitEsc(loopCtx) {
					stopping.Store(true)
					fmt.Fprintln(os.Stderr, "\n[esc] stopping after this iteration…")
				}
			}()
		}
	}

	// The codex harness cannot resume a session yet, so every iteration there
	// starts cold and loop.md is the only continuity.
	canResume := cfg.Tiers.Middle.Harness != "codex"

	dispatch := func(ctx context.Context, it loop.Iteration, prompt, resume string) (*streamjson.ResultEvent, string, error) {
		fmt.Fprintf(os.Stderr, "\n[iteration %d]\n", it.N)
		session := ""
		onEvent := func(ev *streamjson.Event) {
			if ie, ok := ev.AsInit(); ok && ie.SessionID != "" {
				session = ie.SessionID
			}
			narrate(ev)
		}
		if !canResume {
			resume = ""
		}
		res, err := worker.Run(ctx,
			worker.Task{ID: *taskID, Prompt: prompt, Dir: *dir, Account: account, Token: token, Resume: resume},
			worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led, OnEvent: onEvent},
		)
		if res != nil && res.SessionID != "" {
			session = res.SessionID
		}
		return res, session, err
	}

	// The quorum replaces the worker grading its own work. Without voters
	// configured there is nothing to ask, so fall back rather than fail.
	judge := loop.Judge(loop.StatusJudge{})
	switch {
	case *once:
		judge = loop.FixedJudge{D: loop.Decision{Action: loop.Done, Reason: "--once"}}
	case *noQuorum || !cfg.Quorum.Enabled || len(cfg.Quorum.Voters) == 0:
		fmt.Fprintln(os.Stderr, "[judge] the worker's own status line (no quorum)")
	default:
		q := quorum.New(cfg, env)
		q.OnVerdicts = func(it loop.Iteration, vs []quorum.Verdict, d loop.Decision) {
			for _, v := range vs {
				if v.Err != nil {
					fmt.Fprintf(os.Stderr, "  [vote] %s: unavailable (%v)\n", v.Voter, v.Err)
					continue
				}
				fmt.Fprintf(os.Stderr, "  [vote] %s: %s %.2f · %s\n", v.Voter, v.Action, v.Confidence, v.Reason)
			}
			if led != nil {
				ballots := make([]map[string]any, 0, len(vs))
				for _, v := range vs {
					b := map[string]any{"voter": v.Voter, "action": string(v.Action), "confidence": v.Confidence, "reason": v.Reason}
					if len(v.Kinds) > 0 {
						b["kinds"] = v.Kinds
					}
					if v.Err != nil {
						b["error"] = v.Err.Error()
					}
					ballots = append(ballots, b)
				}
				_ = led.Note("quorum.vote", map[string]any{
					"task": *taskID, "iteration": it.N, "ballots": ballots,
					"action": string(d.Action), "reason": d.Reason, "confidence": d.Confidence,
				})
			}
		}
		judge = q
		fmt.Fprintf(os.Stderr, "[judge] quorum of %d · %s · min confidence %.2f · autonomy %d\n",
			len(cfg.Quorum.Voters), cfg.Quorum.Decide, cfg.Quorum.MinConfidence, cfg.Autonomy.Level)
	}

	// Cross-run memory. Recall is written into loop.md before each dispatch,
	// so the worker just reads its memory file and never learns a graph
	// exists; the codex path works identically. Every failure here is
	// swallowed on purpose: memory is never allowed to break a loop.
	mem := memory.New(cfg, *dir)
	var recall func(context.Context, loop.Iteration) string
	var signal func(context.Context, loop.Iteration, loop.Decision)
	if mem.Ready() {
		fmt.Fprintf(os.Stderr, "[memory] %s\n", mem.GraphPath())
		var lastRecall string
		recall = func(ctx context.Context, it loop.Iteration) string {
			out, err := mem.Recall(ctx, charter, 1200)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [memory] recall unavailable: %v\n", err)
				return ""
			}
			lastRecall = out
			return out
		}
		signal = func(ctx context.Context, it loop.Iteration, d loop.Decision) {
			if lastRecall == "" {
				return
			}
			outcome := memory.Useful
			if it.Err != nil || d.Action == loop.Abort {
				outcome = memory.DeadEnd
			}
			if err := mem.Record(ctx, charter, lastRecall, outcome); err != nil {
				fmt.Fprintf(os.Stderr, "  [memory] signal not recorded: %v\n", err)
			}
		}
	} else if mem.Enabled && mem.Installed() {
		fmt.Fprintf(os.Stderr, "[memory] no graph yet for this project; build one with: roscoe memory build\n")
	}

	sum, err := loop.Run(loopCtx, loop.Options{
		Charter:       charter,
		Dir:           *dir,
		TaskID:        *taskID,
		Dispatch:      dispatch,
		Judge:         judge,
		Ledger:        led,
		MaxIterations: *maxIter,
		BudgetUSD:     *budget,
		Recall:        recall,
		Signal:        signal,
		OnIteration: func(it loop.Iteration, d loop.Decision) {
			fmt.Fprintf(os.Stderr, "[iteration %d] %s · %s · %.4f USD · %s\n",
				it.N, it.Status, d.Action, it.SpentUSD, d.Reason)
			// Esc asked to stop; let this iteration's decision be recorded,
			// then end the run.
			if stopping.Load() {
				stopLoop()
			}
		},
	})

	if sum != nil {
		fmt.Fprintf(os.Stderr, "\n[loop] %s after %d iterations · %.4f USD · %s\n",
			sum.Action, sum.Iterations, sum.SpentUSD, sum.Reason)
		if sum.Session != "" {
			fmt.Fprintf(os.Stderr, "[loop] pick it up with: roscoe run --resume %s \"...\"\n", sum.Session)
		}
		fmt.Fprintf(os.Stderr, "[loop] working memory: %s\n", loop.Path(*dir))
		// What a worker charges itself covers tier 2 only; everything its
		// subagents spend goes to another provider and is otherwise invisible.
		for up, t := range r.Totals() {
			line := fmt.Sprintf("[router] %s · %d requests · %d in / %d out", up, t.Requests, t.Input, t.Output)
			if t.CacheRead+t.CacheWrite > 0 {
				line += fmt.Sprintf(" · cache %d read / %d write (%.0f%% of prompt)",
					t.CacheRead, t.CacheWrite, t.CacheHitRate()*100)
			} else {
				line += " · no prompt caching reported"
			}
			if t.CostUSD > 0 {
				line += fmt.Sprintf(" · $%.4f", t.CostUSD)
			}
			fmt.Fprintln(os.Stderr, line)
			if led != nil {
				_ = led.Note("router.totals", map[string]any{"task": *taskID, "upstream": up, "total": t})
			}
		}
		// Deterministic and model-free, so it is cheap to distil what this
		// run learned into the cross-run lessons before exiting.
		if path, rerr := mem.Reflect(context.WithoutCancel(ctx)); rerr == nil && path != "" {
			fmt.Fprintf(os.Stderr, "[memory] lessons: %s\n", path)
		}
	}
	if err != nil && loopCtx.Err() == nil {
		fmt.Fprintf(os.Stderr, "roscoe loop: %v\n", err)
		return 1
	}
	if sum != nil && (sum.Action == loop.Abort || sum.Action == loop.Escalate) {
		return 3 // ended without finishing the charter
	}
	return 0
}
