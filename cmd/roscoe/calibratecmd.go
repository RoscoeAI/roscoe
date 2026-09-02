package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/accounts"
	"roscoe.sh/roscoe/internal/calibrate"
	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/pool"
	"roscoe.sh/roscoe/internal/router"
	"roscoe.sh/roscoe/internal/sessions"
	"roscoe.sh/roscoe/internal/streamjson"
	"roscoe.sh/roscoe/internal/worker"
)

// calibrationPath is where the report lives, beside the runs.
func calibrationPath(cfg *config.Config) string {
	return filepath.Join(config.ExpandPath(cfg.StateDir), "calibration.json")
}

// utilFromWindow reads the 5h utilisation back out of the window sentence a
// run recorded, -1 when there is none.
var util5hRe = regexp.MustCompile(`5h window (\d+)% used`)

func utilFromWindow(window string) float64 {
	m := util5hRe.FindStringSubmatch(window)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return float64(n) / 100
}

// cmdCalibrate measures this machine and recommends the fleet's limits. The
// free probes always run; --spend adds a warm worker and a burst of workers
// at once; --apply writes the recommendation into the config.
func cmdCalibrate(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	spend := fs.Bool("spend", false, "also run real workers: one warm, then a burst, to measure cost and rate limiting (about $0.10)")
	apply := fs.Bool("apply", false, "write the recommended limits into roscoe.json")
	burst := fs.Int("workers", 0, "how many workers the --spend burst runs at once (default: the recommendation, at most 4)")
	_ = fs.Parse(args)

	cfg, env, cfgPath, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: %v\n", err)
		return 1
	}
	r := calibrate.Report{At: time.Now(), Util5h: -1}
	fmt.Fprintln(os.Stderr, "[calibrate] machine")
	r.Machine = calibrate.Inspect()
	fmt.Fprintln(os.Stderr, "[calibrate] worker start (a refused local request, no tokens)")
	if d, err := calibrate.WorkerStart(ctx, ""); err == nil {
		r.WorkerStartSeconds = d.Seconds()
	} else {
		fmt.Fprintf(os.Stderr, "[calibrate] worker start: %v\n", err)
	}
	// The account window, from the newest run that recorded one.
	if list, err := sessions.List(runsDir(cfg), 0, nil); err == nil {
		for _, s := range list {
			if s.Window != "" {
				r.Window = s.Window + " (as of " + sessions.Age(s.Ended, time.Now()) + ")"
				r.Util5h = utilFromWindow(s.Window)
				break
			}
		}
	}

	if *spend {
		if code := spendProbes(ctx, cfg, env, &r, *burst); code != 0 {
			return code
		}
	}

	r.Recommend = calibrate.Recommend(r)
	fmt.Print(calibrate.Render(r))
	path := calibrationPath(cfg)
	if err := calibrate.Save(path, r); err != nil {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: save: %v\n", err)
		return 1
	}
	fmt.Printf("\nsaved to %s\n", shortDir(path))
	if !*apply {
		fmt.Println("apply it:  roscoe calibrate --apply     measure cost and rate limits too:  roscoe calibrate --spend")
		return 0
	}
	sets := map[string]string{
		"limits.max_parallel_tasks":         strconv.Itoa(r.Recommend.MaxParallelTasks),
		"limits.per_account_max_concurrent": strconv.Itoa(r.Recommend.PerAccountMaxConcurrent),
		"tiers.subagents.max_concurrent":    strconv.Itoa(r.Recommend.SubagentsMaxConcurrent),
		"tiers.middle.cache_ttl":            r.Recommend.CacheTTL,
	}
	for k, v := range sets {
		if err := cfg.SetPath(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe calibrate: set %s: %v\n", k, err)
			return 1
		}
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: recommendation does not validate: %v\n", errs[0])
		return 1
	}
	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: save config: %v\n", err)
		return 1
	}
	fmt.Printf("applied to %s\n", shortDir(cfgPath))
	return 0
}

// spendProbes runs real workers through a router: one warm task for the cost
// of a start, then a burst to see whether the API pushes back. Trivial
// prompts keep it to cents.
func spendProbes(ctx context.Context, cfg *config.Config, env map[string]string, r *calibrate.Report, burst int) int {
	rt, err := router.New(router.Options{Cfg: cfg, Env: env})
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: %v\n", err)
		return 1
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	errCh := make(chan error, 1)
	go func() { errCh <- rt.ListenAndServe(rctx) }()
	addr, err := waitHealthz(ctx, rt.Addr, errCh, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe calibrate: %v\n", err)
		return 1
	}
	creds, _ := accounts.ResolveAll(cfg, cfg.Tiers.Middle.Accounts, env, os.Getenv, accounts.MacKeychain{})
	accts := newAccountPool(creds, 0)
	base := "calib-" + time.Now().UTC().Format("20060102-150405")

	run := func(ctx context.Context, t pool.Task, warm func()) (*streamjson.ResultEvent, error) {
		cred, release := accts.acquire(ctx)
		defer release()
		return worker.Run(ctx, worker.Task{ID: t.ID, Prompt: t.Prompt, Dir: os.TempDir(), Account: cred.Name, Token: cred.Token},
			worker.Opts{Cfg: cfg, RouterAddr: addr, OnEvent: func(ev *streamjson.Event) {
				if ev.Type == "assistant" {
					warm()
				}
			}})
	}
	probe := func(n int) *calibrate.Probe {
		before := rt.Totals()[cfg.Tiers.Middle.Provider]
		tasks := make([]pool.Task, n)
		for i := range tasks {
			tasks[i] = pool.Task{ID: fmt.Sprintf("%s-%d-%d", base, n, i+1), Prompt: "Reply with exactly: ok"}
		}
		start := time.Now()
		results := pool.Run(ctx, tasks, pool.Options{Limit: n}, run)
		p := &calibrate.Probe{Workers: n, Seconds: time.Since(start).Seconds()}
		for _, res := range results {
			switch {
			case res.Err != nil:
				p.Err = res.Err.Error()
			case res.Value == nil:
				p.Err = "no result"
			default:
				p.CostUSD += res.Value.TotalCostUSD
				if res.Value.IsError && strings.Contains(strings.ToLower(res.Value.Result), "rate") {
					p.RateLimited = true
				}
			}
		}
		after := rt.Totals()[cfg.Tiers.Middle.Provider]
		p.CacheRead, p.CacheWrite = after.CacheRead-before.CacheRead, after.CacheWrite-before.CacheWrite
		if l, ok := rt.RateLimits()[cfg.Tiers.Middle.Provider]; ok {
			if h := l.Headers["retry-after"]; h != "" || l.Status == 429 {
				p.RateLimited = true
			}
			r.Window = l.Line(time.Now())
			r.Util5h = l.ParseWindow().Util5h
			if !l.ParseWindow().HasUnified {
				r.Util5h = -1
			}
		}
		return p
	}
	fmt.Fprintln(os.Stderr, "[calibrate] one warm worker")
	r.Warm = probe(1)
	if burst <= 0 {
		burst = calibrate.Recommend(*r).MaxParallelTasks
		if burst > 4 {
			burst = 4
		}
	}
	if burst > 1 {
		fmt.Fprintf(os.Stderr, "[calibrate] %d workers at once\n", burst)
		r.Concurrent = probe(burst)
	}
	return 0
}
