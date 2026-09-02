// Package calibrate measures a machine before roscoe assumes what it can
// carry: cores and memory, the harnesses installed, how long a worker takes
// to start, what the account's usage window says, and, when the operator is
// willing to spend, what a warm worker costs and how many run side by side
// without the API pushing back. From those it recommends the fleet's limits,
// deterministically, with the reason for each number.
package calibrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Machine is what the hardware and installed tools are.
type Machine struct {
	Host   string `json:"host"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Cores  int    `json:"cores"`
	MemGB  int    `json:"mem_gb"`
	Claude string `json:"claude"` // version, or "missing"
	Codex  string `json:"codex"`
}

// Probe is one paid measurement: a worker, or several, run for real.
type Probe struct {
	Workers     int     `json:"workers"`
	Seconds     float64 `json:"seconds"`
	CostUSD     float64 `json:"cost_usd"`
	CacheRead   int     `json:"cache_read"`
	CacheWrite  int     `json:"cache_write"`
	RateLimited bool    `json:"rate_limited"`
	Err         string  `json:"err,omitempty"`
}

// Limits are the recommended settings, with the reason for each.
type Limits struct {
	MaxParallelTasks        int      `json:"max_parallel_tasks"`
	PerAccountMaxConcurrent int      `json:"per_account_max_concurrent"`
	SubagentsMaxConcurrent  int      `json:"subagents_max_concurrent"`
	CacheTTL                string   `json:"cache_ttl"`
	Why                     []string `json:"why"`
}

// Report is everything one calibration learned.
type Report struct {
	At                 time.Time `json:"at"`
	Machine            Machine   `json:"machine"`
	WorkerStartSeconds float64   `json:"worker_start_seconds"` // exec to first request, refused locally, zero tokens
	Window             string    `json:"window,omitempty"`     // account window sentence, from the newest ledger
	Util5h             float64   `json:"util_5h"`              // 0..1, -1 when unknown
	Warm               *Probe    `json:"warm,omitempty"`
	Concurrent         *Probe    `json:"concurrent,omitempty"`
	Recommend          Limits    `json:"recommend"`
}

// Inspect gathers the free facts about this machine.
func Inspect() Machine {
	m := Machine{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	m.Host, _ = os.Hostname()
	m.MemGB = memGB()
	m.Claude = version("claude", "--version")
	m.Codex = version("codex", "--version")
	return m
}

func version(bin string, args ...string) string {
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return "missing"
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if i := strings.IndexByte(line, ' '); i > 0 && strings.HasPrefix(line, "codex-cli") {
		line = strings.TrimSpace(line[i+1:])
	}
	if i := strings.Index(line, " ("); i > 0 {
		line = line[:i]
	}
	return line
}

func memGB() int {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(n >> 30)
			}
		}
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "MemTotal:") {
					f := strings.Fields(l)
					if len(f) >= 2 {
						if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
							return int(kb >> 20)
						}
					}
				}
			}
		}
	}
	return 0
}

// WorkerStart measures how long the claude binary takes from exec to its
// first API request, by pointing it at a local endpoint that refuses the
// request with a 400 (which it does not retry). No tokens are spent and no
// login is needed: the request is composed before credentials are checked.
func WorkerStart(ctx context.Context, bin string) (time.Duration, error) {
	if bin == "" {
		bin = "claude"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	first := make(chan time.Time, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case first <- time.Now():
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"roscoe calibration"}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin, "-p", "ok", "--model", "sonnet", "--max-turns", "1",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "project")
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=http://"+ln.Addr().String())
	start := time.Now()
	_ = cmd.Run() // non-zero exit expected: we refused its only request
	select {
	case t := <-first:
		return t.Sub(start), nil
	default:
		return 0, fmt.Errorf("%s made no request within 30s", bin)
	}
}

// Recommend turns a report into limits, each with its reason. It never
// returns zero for a count: a machine that can run roscoe can run one worker.
func Recommend(r Report) Limits {
	var l Limits
	why := func(f string, a ...any) { l.Why = append(l.Why, fmt.Sprintf(f, a...)) }

	// Parallel workers: a claude process is a node runtime around 300-500MB
	// with a model call in flight; half the cores and ~1.5GB each is a
	// ceiling that leaves the machine usable, capped at 8 because the API,
	// not the box, is the limit past that.
	byCores := r.Machine.Cores / 2
	byMem := 0
	if r.Machine.MemGB > 0 {
		byMem = int(float64(r.Machine.MemGB) / 1.5)
	}
	n := byCores
	if byMem > 0 && byMem < n {
		n = byMem
	}
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	why("max_parallel_tasks %d: %d cores allow %d, %dGB allows %d, capped at 8", n, r.Machine.Cores, byCores, r.Machine.MemGB, byMem)
	if r.Concurrent != nil && r.Concurrent.RateLimited && r.Concurrent.Workers > 1 {
		n = r.Concurrent.Workers - 1
		if n < 1 {
			n = 1
		}
		why("max_parallel_tasks lowered to %d: %d workers at once were rate limited", n, r.Concurrent.Workers)
	}
	l.MaxParallelTasks = n

	// Per account: the window says how much room there is. Above half used,
	// or not allowed, go gently; unknown means the fleet default of 2.
	switch {
	case r.Util5h < 0:
		l.PerAccountMaxConcurrent = 2
		why("per_account_max_concurrent 2: no window reading yet (run one task, then calibrate again)")
	case r.Util5h >= 0.8:
		l.PerAccountMaxConcurrent = 1
		why("per_account_max_concurrent 1: the 5h window is %.0f%% used", r.Util5h*100)
	case r.Util5h >= 0.5:
		l.PerAccountMaxConcurrent = 2
		why("per_account_max_concurrent 2: the 5h window is %.0f%% used", r.Util5h*100)
	default:
		l.PerAccountMaxConcurrent = min(4, n)
		why("per_account_max_concurrent %d: the 5h window is %.0f%% used, room to run wide", l.PerAccountMaxConcurrent, r.Util5h*100)
	}

	// Subagents: the swarm is cheap and its provider has no window headers;
	// two per worker slot keeps a full fleet under most providers' request
	// rates.
	l.SubagentsMaxConcurrent = max(4, 2*n)
	if l.SubagentsMaxConcurrent > 16 {
		l.SubagentsMaxConcurrent = 16
	}
	why("subagents max_concurrent %d: two per worker slot, 4 to 16", l.SubagentsMaxConcurrent)

	// Cache TTL: a worker that starts in ~1.6s and a prefix that costs ~$0.21
	// to write make the hour worth paying for whenever runs are less than an
	// hour apart, which is every working session.
	l.CacheTTL = "1h"
	why("cache_ttl 1h: the prefix costs more to write than an hour of reads; 5m only if runs are rarer than that")
	return l
}

// Render is the report as a person reads it.
func Render(r Report) string {
	var b strings.Builder
	m := r.Machine
	fmt.Fprintf(&b, "machine    %s · %s/%s · %d cores · %dGB\n", m.Host, m.OS, m.Arch, m.Cores, m.MemGB)
	fmt.Fprintf(&b, "harnesses  claude %s · codex %s\n", m.Claude, m.Codex)
	if r.WorkerStartSeconds > 0 {
		fmt.Fprintf(&b, "worker     %.1fs from start to first request (measured, no tokens)\n", r.WorkerStartSeconds)
	}
	if r.Window != "" {
		fmt.Fprintf(&b, "account    %s\n", r.Window)
	} else {
		fmt.Fprintf(&b, "account    no window reading yet; run one task and calibrate again\n")
	}
	if r.Warm != nil {
		if r.Warm.Err != "" {
			fmt.Fprintf(&b, "warm run   failed: %s\n", r.Warm.Err)
		} else {
			fmt.Fprintf(&b, "warm run   $%.4f · %.1fs · cache %d read / %d write\n", r.Warm.CostUSD, r.Warm.Seconds, r.Warm.CacheRead, r.Warm.CacheWrite)
		}
	}
	if r.Concurrent != nil {
		state := "no rate limiting"
		if r.Concurrent.RateLimited {
			state = "RATE LIMITED"
		}
		if r.Concurrent.Err != "" {
			state = "failed: " + r.Concurrent.Err
		}
		fmt.Fprintf(&b, "%d at once  $%.4f · %.1fs · %s\n", r.Concurrent.Workers, r.Concurrent.CostUSD, r.Concurrent.Seconds, state)
	}
	fmt.Fprintf(&b, "\nrecommend  limits.max_parallel_tasks %d · limits.per_account_max_concurrent %d · tiers.subagents.max_concurrent %d · tiers.middle.cache_ttl %s\n",
		r.Recommend.MaxParallelTasks, r.Recommend.PerAccountMaxConcurrent, r.Recommend.SubagentsMaxConcurrent, r.Recommend.CacheTTL)
	for _, w := range r.Recommend.Why {
		fmt.Fprintf(&b, "  · %s\n", w)
	}
	return b.String()
}

// Save writes the report where roscoe top and later calibrations find it.
func Save(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load reads a saved report; ok is false when there is none.
func Load(path string) (Report, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, false
	}
	var r Report
	if json.Unmarshal(b, &r) != nil {
		return Report{}, false
	}
	return r, true
}

// Stale says why a saved calibration no longer describes this machine, or
// "" when it still does.
func Stale(saved Report, now Machine) string {
	switch {
	case saved.Machine.Host != now.Host:
		return "different machine (" + saved.Machine.Host + " then, " + now.Host + " now)"
	case saved.Machine.Cores != now.Cores || saved.Machine.MemGB != now.MemGB:
		return "hardware changed"
	case saved.Machine.Claude != now.Claude:
		return "claude was " + saved.Machine.Claude + ", now " + now.Claude
	}
	return ""
}
