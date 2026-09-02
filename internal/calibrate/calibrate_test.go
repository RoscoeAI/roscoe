package calibrate

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func report(cores, memGB int, util float64) Report {
	return Report{At: time.Now(), Machine: Machine{Host: "mac", OS: "darwin", Arch: "arm64", Cores: cores, MemGB: memGB, Claude: "2.1.259", Codex: "0.147.0"}, Util5h: util}
}

// The recommendation is deterministic, never zero, and says why.
func TestRecommend(t *testing.T) {
	l := Recommend(report(14, 24, 0.06))
	if l.MaxParallelTasks != 7 || l.PerAccountMaxConcurrent != 4 || l.SubagentsMaxConcurrent != 14 || l.CacheTTL != "1h" {
		t.Errorf("14 cores/24GB/6%% -> %+v", l)
	}
	if len(l.Why) < 4 {
		t.Errorf("reasons missing: %v", l.Why)
	}
	// Memory can be the ceiling; the API cap is 8.
	if got := Recommend(report(32, 8, 0.1)).MaxParallelTasks; got != 5 {
		t.Errorf("8GB machine -> %d workers, want 5", got)
	}
	if got := Recommend(report(64, 256, 0.1)).MaxParallelTasks; got != 8 {
		t.Errorf("big machine -> %d, want the cap 8", got)
	}
	if got := Recommend(report(1, 2, 0.1)).MaxParallelTasks; got != 1 {
		t.Errorf("tiny machine -> %d, want 1", got)
	}
	// The window decides per-account width.
	if got := Recommend(report(14, 24, -1)).PerAccountMaxConcurrent; got != 2 {
		t.Errorf("unknown window -> %d, want 2", got)
	}
	if got := Recommend(report(14, 24, 0.6)).PerAccountMaxConcurrent; got != 2 {
		t.Errorf("60%% used -> %d, want 2", got)
	}
	if got := Recommend(report(14, 24, 0.9)).PerAccountMaxConcurrent; got != 1 {
		t.Errorf("90%% used -> %d, want 1", got)
	}
	// A rate-limited concurrency probe lowers the ceiling to what worked.
	r := report(14, 24, 0.1)
	r.Concurrent = &Probe{Workers: 4, RateLimited: true}
	if got := Recommend(r); got.MaxParallelTasks != 3 || !strings.Contains(strings.Join(got.Why, " "), "rate limited") {
		t.Errorf("rate limited at 4 -> %+v", got)
	}
}

func TestRenderSaveLoadStale(t *testing.T) {
	r := report(14, 24, 0.06)
	r.WorkerStartSeconds = 1.6
	r.Window = "5h window 6% used, resets in 2h"
	r.Warm = &Probe{Workers: 1, Seconds: 3, CostUSD: 0.0146, CacheRead: 52754}
	r.Concurrent = &Probe{Workers: 4, Seconds: 7, CostUSD: 0.06}
	r.Recommend = Recommend(r)
	out := Render(r)
	for _, want := range []string{"14 cores · 24GB", "claude 2.1.259", "1.6s from start", "5h window 6% used", "warm run   $0.0146", "4 at once  $0.0600 · 7.0s · no rate limiting", "limits.max_parallel_tasks 7"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	p := filepath.Join(t.TempDir(), "calibration.json")
	if err := Save(p, r); err != nil {
		t.Fatal(err)
	}
	back, ok := Load(p)
	if !ok || back.Recommend.MaxParallelTasks != 7 || back.Machine.Host != "mac" {
		t.Errorf("round trip = %+v %v", back, ok)
	}
	if _, ok := Load(p + ".missing"); ok {
		t.Error("missing file loaded")
	}
	if Stale(back, back.Machine) != "" {
		t.Error("same machine reported stale")
	}
	other := back.Machine
	other.Claude = "2.2.0"
	if s := Stale(back, other); !strings.Contains(s, "claude was 2.1.259") {
		t.Errorf("claude change: %q", s)
	}
	other = back.Machine
	other.Host = "studio"
	if s := Stale(back, other); !strings.Contains(s, "different machine") {
		t.Errorf("host change: %q", s)
	}
}

// Inspect fills the free facts on any machine the tests run on.
func TestInspect(t *testing.T) {
	m := Inspect()
	if m.Cores < 1 || m.OS == "" || m.Arch == "" || m.Host == "" {
		t.Errorf("inspect = %+v", m)
	}
}
