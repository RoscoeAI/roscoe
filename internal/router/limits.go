package router

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RateLimit is the latest thing an upstream said about how much room an
// account has: the anthropic-ratelimit-* and retry-after headers of its most
// recent response. Calibration reads it to size the fleet; the run's closing
// [router] line shows it so nobody has to guess why a worker slowed down.
type RateLimit struct {
	Upstream string
	Seen     time.Time
	Status   int
	Headers  map[string]string // lower-case name -> value, limit-related only
}

// limitHeader reports whether a response header is about rate limits.
func limitHeader(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "anthropic-ratelimit-") || strings.HasPrefix(n, "x-ratelimit-") || n == "retry-after"
}

type limitBook struct {
	mu   sync.Mutex
	last map[string]RateLimit
}

// note records the limit headers of one response for its upstream.
func (b *limitBook) note(upstream string, status int, h http.Header) {
	picked := map[string]string{}
	for k, v := range h {
		if limitHeader(k) && len(v) > 0 {
			picked[strings.ToLower(k)] = v[0]
		}
	}
	if len(picked) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == nil {
		b.last = map[string]RateLimit{}
	}
	b.last[upstream] = RateLimit{Upstream: upstream, Seen: time.Now(), Status: status, Headers: picked}
}

func (b *limitBook) snapshot() map[string]RateLimit {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]RateLimit, len(b.last))
	for k, v := range b.last {
		out[k] = v
	}
	return out
}

// RateLimits is the latest limit headers seen per upstream.
func (rt *Router) RateLimits() map[string]RateLimit { return rt.limits.snapshot() }

// Summary renders the limits on one line, names shortened: the
// anthropic-ratelimit- prefix dropped, sorted, "k=v" joined by spaces.
func (l RateLimit) Summary() string {
	keys := make([]string, 0, len(l.Headers))
	for k := range l.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strings.TrimPrefix(k, "anthropic-ratelimit-")+"="+l.Headers[k])
	}
	return strings.Join(parts, " ")
}

// dumpHeaders writes a response's headers next to the request body dump,
// limit and content headers only, never anything that could carry a secret.
func (rt *Router) dumpHeaders(n int64, upstream string, status int, h http.Header) {
	if rt.dumpDir == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "upstream: %s\nstatus: %d\n", upstream, status)
	keys := make([]string, 0, len(h))
	for k := range h {
		lk := strings.ToLower(k)
		if limitHeader(k) || lk == "content-type" || lk == "request-id" || lk == "x-request-id" || lk == "cf-ray" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", strings.ToLower(k), h.Get(k))
	}
	_ = os.WriteFile(filepath.Join(rt.dumpDir, fmt.Sprintf("%03d-response-headers.txt", n)), []byte(b.String()), 0o600)
}

// Window is a subscription account's usage window as the API reports it in
// the anthropic-ratelimit-unified-* headers: how much of the five-hour and
// seven-day allowances are used, when each resets, and whether requests are
// currently allowed. API-key accounts report request/token limits instead
// and get the raw Summary.
type Window struct {
	Status     string    // allowed, rejected...
	Util5h     float64   // 0..1
	Util7d     float64   // 0..1
	Reset5h    time.Time // zero when absent
	Reset7d    time.Time
	Overage    string // overage status, e.g. rejected
	HasUnified bool
}

// ParseWindow reads the unified headers out of a RateLimit.
func (l RateLimit) ParseWindow() Window {
	h := l.Headers
	w := Window{Status: h["anthropic-ratelimit-unified-status"], Overage: h["anthropic-ratelimit-unified-overage-status"]}
	if w.Status == "" && h["anthropic-ratelimit-unified-5h-utilization"] == "" {
		return w
	}
	w.HasUnified = true
	fmt.Sscanf(h["anthropic-ratelimit-unified-5h-utilization"], "%g", &w.Util5h)
	fmt.Sscanf(h["anthropic-ratelimit-unified-7d-utilization"], "%g", &w.Util7d)
	w.Reset5h = epoch(h["anthropic-ratelimit-unified-5h-reset"])
	w.Reset7d = epoch(h["anthropic-ratelimit-unified-7d-reset"])
	return w
}

func epoch(s string) time.Time {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// Human is the one line a person wants: "5h window 5% used, resets in 2h ·
// 7d window 11% used, resets in 4h · overage off". now sets "resets in".
func (w Window) Human(now time.Time) string {
	if !w.HasUnified {
		return ""
	}
	part := func(name string, util float64, reset time.Time) string {
		s := fmt.Sprintf("%s window %.0f%% used", name, util*100)
		if !reset.IsZero() {
			s += ", resets in " + humanDur(reset.Sub(now))
		}
		return s
	}
	out := part("5h", w.Util5h, w.Reset5h) + " · " + part("7d", w.Util7d, w.Reset7d)
	if w.Status != "" && w.Status != "allowed" {
		out += " · status " + w.Status
	}
	if w.Overage == "rejected" {
		out += " · overage off"
	}
	return out
}

func humanDur(d time.Duration) string {
	if d < 0 {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Line is what the closing [router] line shows: the window sentence for a
// subscription account, else the raw limit headers.
func (l RateLimit) Line(now time.Time) string {
	if h := l.ParseWindow().Human(now); h != "" {
		return h
	}
	return l.Summary()
}
