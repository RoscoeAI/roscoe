package router

import (
	"bytes"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

func TestParseUsageFromAJSONReply(t *testing.T) {
	body := `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":12,"output_tokens":34,"cache_creation_input_tokens":56,"cache_read_input_tokens":78}}`
	u, ok := parseUsage(body)
	if !ok {
		t.Fatal("no usage found")
	}
	if u.Input != 12 || u.Output != 34 || u.CacheWrite != 56 || u.CacheRead != 78 {
		t.Errorf("usage = %+v", u)
	}
}

// A streamed reply reports usage twice: message_start with the input counts
// and output still zero, then message_delta with the totals. Taking either one
// alone loses half the picture.
func TestParseUsageCombinesStreamedFrames(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1000,"output_tokens":1,"cache_read_input_tokens":900}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"hello"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":250}}

event: message_stop
data: {"type":"message_stop"}
`
	u, ok := parseUsage(sse)
	if !ok {
		t.Fatal("no usage found in the stream")
	}
	if u.Input != 1000 {
		t.Errorf("input = %d; message_delta must not erase message_start's counts", u.Input)
	}
	if u.Output != 250 {
		t.Errorf("output = %d, want the final total", u.Output)
	}
	if u.CacheRead != 900 {
		t.Errorf("cache read = %d", u.CacheRead)
	}
}

func TestParseUsageFindsNothing(t *testing.T) {
	for name, in := range map[string]string{
		"empty":       "",
		"no usage":    `{"id":"m","content":[]}`,
		"all zero":    `{"usage":{"input_tokens":0,"output_tokens":0}}`,
		"unbalanced":  `{"usage":{"input_tokens":1`,
		"in a string": `{"text":"the word \"usage\" appears here"}`,
	} {
		if _, ok := parseUsage(in); ok {
			t.Errorf("%s: reported usage where there is none", name)
		}
	}
}

// The tap must be transparent: bytes through unchanged, and memory bounded no
// matter how long the response is, because every tier-3 request goes through
// it and some of them stream for minutes.
func TestUsageTapIsTransparentAndBounded(t *testing.T) {
	var sink bytes.Buffer
	tap := &usageTap{next: &sink}

	big := strings.Repeat("x", 200<<10)
	if _, err := tap.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	usage := `{"usage":{"input_tokens":5,"output_tokens":6}}`
	if _, err := tap.Write([]byte(usage)); err != nil {
		t.Fatal(err)
	}

	if sink.Len() != len(big)+len(usage) {
		t.Errorf("wrote %d bytes downstream, want %d", sink.Len(), len(big)+len(usage))
	}
	if !strings.HasSuffix(sink.String(), usage) {
		t.Error("the tap altered what it passed through")
	}
	if len(tap.tail) > tailBytes {
		t.Errorf("tail grew to %d bytes, past the %d cap", len(tap.tail), tailBytes)
	}
	u, ok := tap.Usage()
	if !ok || u.Input != 5 || u.Output != 6 {
		t.Errorf("usage = %+v ok=%v; the tail must still catch what is at the end", u, ok)
	}
}

// Usage split across write boundaries is the normal case for SSE.
func TestUsageAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	tap := &usageTap{next: &sink}
	for _, chunk := range []string{`data: {"type":"message_delta","usa`, `ge":{"output_tokens":`, `42}}`} {
		if _, err := tap.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	u, ok := tap.Usage()
	if !ok || u.Output != 42 {
		t.Errorf("usage = %+v ok=%v", u, ok)
	}
}

func TestCost(t *testing.T) {
	p := &config.Pricing{Input: 0.075, Output: 0.25, CachedInput: 0.015}
	u := Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	got := u.Cost(p)
	want := 0.075 + 0.25 + 0.015
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}

	// A cache write is billed at the input rate, not the cached rate.
	if c := (Usage{CacheWrite: 1_000_000}).Cost(p); c != p.Input {
		t.Errorf("cache write cost = %v, want the input rate %v", c, p.Input)
	}
	// Unpriced providers report zero rather than guessing.
	if c := u.Cost(nil); c != 0 {
		t.Errorf("cost without pricing = %v, want 0", c)
	}
	// A provider that priced input but not cached input falls back to input.
	if c := (Usage{CacheRead: 1_000_000}).Cost(&config.Pricing{Input: 2}); c != 2 {
		t.Errorf("cache read without a cached rate = %v, want the input rate", c)
	}
}

func TestTotalsAccumulatePerUpstream(t *testing.T) {
	var ts Totals
	ts.add("deepinfra", Usage{Input: 10, Output: 1, CacheRead: 90}, 0.5)
	ts.add("deepinfra", Usage{Input: 10, Output: 1, CacheRead: 90}, 0.5)
	ts.add("anthropic", Usage{Input: 5}, 0.25)

	snap := ts.Snapshot()
	di := snap["deepinfra"]
	if di.Requests != 2 || di.Input != 20 || di.CacheRead != 180 {
		t.Errorf("deepinfra = %+v", di)
	}
	if di.CostUSD != 1.0 {
		t.Errorf("cost = %v, want 1.0", di.CostUSD)
	}
	if an := snap["anthropic"]; an.Requests != 1 || an.Input != 5 {
		t.Errorf("anthropic = %+v; upstreams must not be mixed", an)
	}

	// The snapshot is a copy: mutating it must not corrupt the router.
	di.Requests = 999
	if ts.Snapshot()["deepinfra"].Requests != 2 {
		t.Error("Snapshot handed out a live pointer")
	}
}

// The number that answers whether caching works on a leg at all.
func TestCacheHitRate(t *testing.T) {
	if got := (Total{Input: 10, CacheRead: 90}).CacheHitRate(); got != 0.9 {
		t.Errorf("hit rate = %v, want 0.9", got)
	}
	if got := (Total{Input: 100}).CacheHitRate(); got != 0 {
		t.Errorf("hit rate with no cache = %v, want 0", got)
	}
	if got := (Total{}).CacheHitRate(); got != 0 {
		t.Errorf("hit rate of nothing = %v, want 0 rather than NaN", got)
	}
	// A cache write counts as prompt tokens, not as a hit.
	if got := (Total{CacheWrite: 100}).CacheHitRate(); got != 0 {
		t.Errorf("a write-only request reported a %v hit rate", got)
	}
}
