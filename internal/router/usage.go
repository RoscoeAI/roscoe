package router

import (
	"encoding/json"
	"strings"
	"sync"

	"roscoe.sh/roscoe/internal/config"
)

// Usage is what an upstream reported for one request.
type Usage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
}

func (u Usage) empty() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheWrite == 0 && u.CacheRead == 0
}

// Cost prices usage against a provider's configured rates. A provider with no
// pricing returns 0, which is honest: roscoe does not know what it cost.
func (u Usage) Cost(p *config.Pricing) float64 {
	if p == nil {
		return 0
	}
	const perM = 1_000_000.0
	cachedRate := p.CachedInput
	if cachedRate == 0 {
		cachedRate = p.Input
	}
	return (float64(u.Input)*p.Input +
		float64(u.Output)*p.Output +
		float64(u.CacheWrite)*p.Input +
		float64(u.CacheRead)*cachedRate) / perM
}

// tailBytes is how much of a response the scanner keeps. Usage sits at the end
// of both shapes: the closing object of a JSON reply, and the final
// message_delta frame of an SSE stream. Keeping a bounded tail means streaming
// is never buffered and memory never depends on response size.
const tailBytes = 16 << 10

// usageTap passes bytes straight through while keeping the tail, so usage can
// be read after the response has already been delivered. It adds no latency
// and no buffering to the hot path, which matters because every tier-3
// subagent request goes through here.
type usageTap struct {
	next interface{ Write([]byte) (int, error) }
	tail []byte
}

func (t *usageTap) Write(p []byte) (int, error) {
	n, err := t.next.Write(p)
	if n > 0 {
		t.tail = append(t.tail, p[:n]...)
		if len(t.tail) > tailBytes {
			t.tail = t.tail[len(t.tail)-tailBytes:]
		}
	}
	return n, err
}

// Usage parses whatever the upstream reported, or false when it said nothing.
func (t *usageTap) Usage() (Usage, bool) { return parseUsage(string(t.tail)) }

// parseUsage reads the last "usage" object in a response tail. Last, because a
// streamed reply reports usage twice: message_start carries the input counts
// with output still zero, and the closing message_delta carries the totals.
func parseUsage(s string) (Usage, bool) {
	best, ok := Usage{}, false
	for i := 0; ; {
		j := strings.Index(s[i:], `"usage"`)
		if j < 0 {
			break
		}
		i += j + len(`"usage"`)
		obj, found := balancedObject(s[i:])
		if !found {
			continue
		}
		var u Usage
		if err := json.Unmarshal([]byte(obj), &u); err != nil || u.empty() {
			continue
		}
		// Keep the richest report: a message_delta omits the input counts it
		// already sent, so the two frames have to be combined rather than
		// replaced.
		if u.Input > best.Input {
			best.Input = u.Input
		}
		if u.Output > best.Output {
			best.Output = u.Output
		}
		if u.CacheWrite > best.CacheWrite {
			best.CacheWrite = u.CacheWrite
		}
		if u.CacheRead > best.CacheRead {
			best.CacheRead = u.CacheRead
		}
		ok = true
	}
	return best, ok
}

// balancedObject returns the first {...} run after a colon, ignoring braces
// inside strings.
func balancedObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// Totals accumulates what has crossed the router, per upstream. Tier-3 spend
// is otherwise invisible: a worker reports only its own cost, and everything
// its subagents spend goes straight to another provider.
type Totals struct {
	mu   sync.Mutex
	byUp map[string]*Total
}

// Total is one upstream's running account.
type Total struct {
	Requests   int     `json:"requests"`
	Input      int     `json:"input_tokens"`
	Output     int     `json:"output_tokens"`
	CacheWrite int     `json:"cache_creation_input_tokens"`
	CacheRead  int     `json:"cache_read_input_tokens"`
	CostUSD    float64 `json:"cost_usd"`
}

// CacheHitRate is the share of prompt tokens served from cache. It is the
// number that answers "is prompt caching working on this leg at all".
func (t Total) CacheHitRate() float64 {
	prompt := t.Input + t.CacheWrite + t.CacheRead
	if prompt == 0 {
		return 0
	}
	return float64(t.CacheRead) / float64(prompt)
}

func (ts *Totals) add(upstream string, u Usage, cost float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.byUp == nil {
		ts.byUp = map[string]*Total{}
	}
	t := ts.byUp[upstream]
	if t == nil {
		t = &Total{}
		ts.byUp[upstream] = t
	}
	t.Requests++
	t.Input += u.Input
	t.Output += u.Output
	t.CacheWrite += u.CacheWrite
	t.CacheRead += u.CacheRead
	t.CostUSD += cost
}

// Snapshot returns a copy safe to read while the router is still serving.
func (ts *Totals) Snapshot() map[string]Total {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make(map[string]Total, len(ts.byUp))
	for k, v := range ts.byUp {
		out[k] = *v
	}
	return out
}
