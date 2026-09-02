package router

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only limit headers are kept, per upstream, latest wins; the summary drops
// the long prefix so a line of it reads.
func TestLimitBookKeepsOnlyLimitHeaders(t *testing.T) {
	var b limitBook
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Requests-Limit", "4000")
	h.Set("Anthropic-Ratelimit-Requests-Remaining", "3999")
	h.Set("Retry-After", "7")
	h.Set("Authorization", "Bearer nope")
	h.Set("Content-Type", "application/json")
	b.note("anthropic", 200, h)
	got := b.snapshot()["anthropic"]
	if got.Status != 200 || len(got.Headers) != 3 {
		t.Fatalf("kept %+v", got)
	}
	if _, leaked := got.Headers["authorization"]; leaked {
		t.Fatal("a credential header was kept")
	}
	if s := got.Summary(); s != "requests-limit=4000 requests-remaining=3999 retry-after=7" {
		t.Errorf("summary = %q", s)
	}
	// A response with no limit headers leaves the last reading alone.
	b.note("anthropic", 200, http.Header{"Content-Type": {"text/plain"}})
	if b.snapshot()["anthropic"].Headers["requests-remaining"] == "" && b.snapshot()["anthropic"].Headers["anthropic-ratelimit-requests-remaining"] == "" {
		t.Error("a limit-less response erased the last reading")
	}
	if len((&limitBook{}).snapshot()) != 0 {
		t.Error("empty book not empty")
	}
}

// The header dump pairs with the body dump by number and carries no secrets.
func TestDumpHeadersPairsWithBody(t *testing.T) {
	dir := t.TempDir()
	rt := &Router{dumpDir: dir}
	n := rt.dump("/v1/messages", []byte("{}"))
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Set-Cookie", "secret")
	h.Set("Request-Id", "req_1")
	rt.dumpHeaders(n, "anthropic", 200, h)
	b, err := os.ReadFile(filepath.Join(dir, "001-response-headers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "upstream: anthropic") || !strings.Contains(s, "anthropic-ratelimit-unified-status: allowed") || !strings.Contains(s, "request-id: req_1") {
		t.Errorf("dump = %s", s)
	}
	if strings.Contains(s, "secret") {
		t.Error("a cookie was dumped")
	}
	(&Router{}).dumpHeaders(0, "x", 200, h) // off: no-op
}

// A subscription account's unified headers become one sentence; an API-key
// account's request/token limits fall back to the raw summary.
func TestWindowHuman(t *testing.T) {
	now := time.Unix(1788390000, 0)
	l := RateLimit{Headers: map[string]string{
		"anthropic-ratelimit-unified-status":         "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "0.05",
		"anthropic-ratelimit-unified-5h-reset":       "1788400200",
		"anthropic-ratelimit-unified-7d-utilization": "0.11",
		"anthropic-ratelimit-unified-7d-reset":       "1788408000",
		"anthropic-ratelimit-unified-overage-status": "rejected",
	}}
	got := l.Line(now)
	want := "5h window 5% used, resets in 2h50m · 7d window 11% used, resets in 5h00m · overage off"
	if got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	w := l.ParseWindow()
	if !w.HasUnified || w.Util7d != 0.11 || w.Reset5h.Unix() != 1788400200 {
		t.Errorf("window = %+v", w)
	}
	// Not allowed: the status shows.
	l.Headers["anthropic-ratelimit-unified-status"] = "rejected"
	if !strings.Contains(l.Line(now), "status rejected") {
		t.Errorf("rejected status hidden: %q", l.Line(now))
	}
	// API-key style: raw summary.
	k := RateLimit{Headers: map[string]string{"anthropic-ratelimit-requests-remaining": "3999"}}
	if k.Line(now) != "requests-remaining=3999" {
		t.Errorf("api-key line = %q", k.Line(now))
	}
	if humanDur(-time.Minute) != "now" || humanDur(30*time.Minute) != "30m" || humanDur(72*time.Hour) != "3d" {
		t.Error("humanDur")
	}
}
