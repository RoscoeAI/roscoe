package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRun lays down one ledger the way roscoe writes it: worker events in
// the envelope, notes at top level.
func writeRun(t *testing.T, dir, task string, lines ...string) {
	t.Helper()
	d := filepath.Join(dir, task)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ev(ts, task, event string) string {
	return fmt.Sprintf(`{"ts":%q,"node":"local","worker":"w","task":%q,"seq":1,"event":%s}`, ts, task, event)
}

func note(ts, kind string) string {
	return fmt.Sprintf(`{"ts":%q,"kind":%q,"task":"t"}`, ts, kind)
}

func TestListReadsLedgersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "task-old",
		ev("2026-09-01T10:00:00Z", "task-old", `{"type":"system","subtype":"init","session_id":"aaaa-1111","model":"claude-sonnet-5","cwd":"/p/old"}`),
		ev("2026-09-01T10:01:00Z", "task-old", `{"type":"result","num_turns":3,"total_cost_usd":0.5,"session_id":"aaaa-1111"}`),
	)
	writeRun(t, dir, "task-new",
		ev("2026-09-02T10:00:00Z", "task-new", `{"type":"system","subtype":"init","session_id":"bbbb-2222","model":"claude-opus-5","cwd":"/p/new"}`),
		ev("2026-09-02T10:05:00Z", "task-new", `{"type":"result","num_turns":2,"total_cost_usd":0.25,"session_id":"bbbb-2222"}`),
		ev("2026-09-02T10:09:00Z", "task-new", `{"type":"result","num_turns":4,"total_cost_usd":0.75,"session_id":"bbbb-2222"}`),
	)
	list, err := List(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sessions", len(list))
	}
	n := list[0]
	if n.TaskID != "task-new" || n.ID != "bbbb-2222" || n.Model != "claude-opus-5" || n.Dir != "/p/new" {
		t.Errorf("newest = %+v", n)
	}
	// Several result events (one per chat turn) sum, they do not replace.
	if n.Turns != 6 || n.CostUSD != 1.0 {
		t.Errorf("turns=%d cost=%v, want 6 and 1.0", n.Turns, n.CostUSD)
	}
	if !n.Started.Equal(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)) || !n.Ended.Equal(time.Date(2026, 9, 2, 10, 9, 0, 0, time.UTC)) {
		t.Errorf("started=%v ended=%v", n.Started, n.Ended)
	}
	if list[1].TaskID != "task-old" {
		t.Errorf("order wrong: %v", list[1].TaskID)
	}
	if n.Kind != "chat/run" {
		t.Errorf("kind = %q", n.Kind)
	}
}

// A trimmed transcript resumes under a new id, and the result carries it; that
// later id is the one --resume must be given.
func TestLastSessionIDWins(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "t",
		ev("2026-09-02T10:00:00Z", "t", `{"type":"system","subtype":"init","session_id":"first"}`),
		ev("2026-09-02T10:01:00Z", "t", `{"type":"result","session_id":"second-after-trim","num_turns":1}`),
	)
	list, _ := List(dir, 0, nil)
	if list[0].ID != "second-after-trim" {
		t.Errorf("id = %q", list[0].ID)
	}
}

func TestLoopRunsAreLabelled(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "t",
		note("2026-09-02T10:00:00Z", "loop.seeded"),
		ev("2026-09-02T10:00:01Z", "t", `{"type":"system","subtype":"init","session_id":"s"}`),
		note("2026-09-02T10:00:02Z", "loop.iteration"),
	)
	list, _ := List(dir, 0, nil)
	if list[0].Kind != "loop" {
		t.Errorf("kind = %q, want loop", list[0].Kind)
	}
}

func TestLimitAndBadFilesAndEnricher(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		ts := fmt.Sprintf("2026-09-02T10:0%d:00Z", i)
		writeRun(t, dir, fmt.Sprintf("t%d", i), ev(ts, "t", fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"s%d"}`, i)))
	}
	// One unreadable ledger must not hide the rest.
	writeRun(t, dir, "broken", "this is not json", "{neither")
	enriched := 0
	list, err := List(dir, 3, func(s *Session) { enriched++; s.About = "hello " + s.ID })
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("limit gave %d", len(list))
	}
	if list[0].ID != "s4" {
		t.Errorf("newest = %q", list[0].ID)
	}
	if enriched != 3 || list[0].About != "hello s4" {
		t.Errorf("enricher ran %d times, about=%q", enriched, list[0].About)
	}
}

func TestLatestSkipsUnresumable(t *testing.T) {
	dir := t.TempDir()
	// Newest run never got an init event (crashed before the harness spoke).
	writeRun(t, dir, "newest", note("2026-09-03T10:00:00Z", "loop.seeded"))
	writeRun(t, dir, "older", ev("2026-09-02T10:00:00Z", "older", `{"type":"system","subtype":"init","session_id":"good"}`))
	s, ok := Latest(dir, nil)
	if !ok || s.ID != "good" {
		t.Errorf("latest = %+v ok=%v, want the older resumable one", s, ok)
	}
	if _, ok := Latest(t.TempDir(), nil); ok {
		t.Error("an empty runs dir reported a latest session")
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cases := map[time.Time]string{
		now.Add(-10 * time.Second): "just now",
		now.Add(-5 * time.Minute):  "5m ago",
		now.Add(-3 * time.Hour):    "3h ago",
		now.Add(-49 * time.Hour):   "2d ago",
		{}:                         "?",
	}
	for tm, want := range cases {
		if got := Age(tm, now); got != want {
			t.Errorf("Age(%v) = %q, want %q", tm, got, want)
		}
	}
}

// A ledger brought home from a node carries a fleet.home tag at the end. The
// session names the node, and the tag's time (when it was fetched, maybe days
// later) does not count as when the run was.
func TestReadLedgerHomeTag(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "task-remote")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := `{"ts":"2026-09-02T03:54:26Z","node":"local","task":"task-remote","seq":1,"event":{"type":"system","subtype":"init","session_id":"bf4c","model":"claude-sonnet-5","cwd":"/Users/t/.roscoe/work/task-remote"}}
{"ts":"2026-09-02T03:54:29Z","node":"local","task":"task-remote","seq":2,"event":{"type":"result","is_error":true,"num_turns":1,"total_cost_usd":0,"session_id":"bf4c"}}
{"ts":"2026-09-05T10:00:00Z","kind":"fleet.home","node":"roscoe","ssh":"roscoe-ts"}
`
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir, 0, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	s := list[0]
	if s.Node != "roscoe" {
		t.Errorf("node = %q", s.Node)
	}
	if s.Ended.Day() != 2 {
		t.Errorf("ended = %s; the home tag's time leaked into the run", s.Ended)
	}
	if s.ID != "bf4c" || s.Turns != 1 {
		t.Errorf("session = %+v", s)
	}
}

// The harness prices a model it does not know at a made-up rate and says so
// with costBasis "unknown". That guess must not reach the listing; the
// router's own note is the real price, and without one the tokens are
// reported as unpriced rather than as opus money.
func TestReadLedgerIgnoresGuessedCosts(t *testing.T) {
	dir := t.TempDir()
	write := func(task, ledger string) {
		if err := os.MkdirAll(filepath.Join(dir, task), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, task, "events.jsonl"), []byte(ledger), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result := `{"ts":"2026-08-31T22:44:52Z","task":"%s","seq":2,"event":{"type":"result","num_turns":1,"total_cost_usd":0.304245,"session_id":"s1","modelUsage":{"roscoe/tier3":{"inputTokens":60784,"outputTokens":13,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.304245,"costBasis":"unknown"}}}}
`
	// Old run: no router record. Cost is not $0.30; the tokens are unpriced.
	write("old", fmt.Sprintf(result, "old"))
	// New run: the router priced the same requests.
	write("new", fmt.Sprintf(result, "new")+`{"ts":"2026-08-31T22:44:53Z","kind":"router.totals","task":"new","upstream":"deepinfra","priced_here":true,"total":{"requests":1,"input_tokens":60784,"output_tokens":13,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cost_usd":0.0046}}
{"ts":"2026-08-31T22:44:53Z","kind":"router.totals","task":"new","upstream":"anthropic","priced_here":false,"total":{"requests":4,"input_tokens":9000,"output_tokens":300,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cost_usd":0.09}}
`)
	// Mixed run: sonnet's own cost is real and kept; the routed part is not.
	write("mixed", `{"ts":"2026-08-31T22:44:52Z","task":"mixed","seq":2,"event":{"type":"result","num_turns":3,"total_cost_usd":0.55,"session_id":"s3","modelUsage":{"claude-sonnet-5":{"inputTokens":1000,"outputTokens":200,"costUSD":0.25,"costBasis":"known"},"roscoe/tier3":{"inputTokens":60000,"outputTokens":3,"costUSD":0.30,"costBasis":"unknown"}}}}
`)
	list, err := List(dir, 0, nil)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %v, %v", list, err)
	}
	by := map[string]Session{}
	for _, s := range list {
		by[s.TaskID] = s
	}
	if o := by["old"]; o.CostUSD != 0 || o.Unpriced != 60797 {
		t.Errorf("old = cost %.4f unpriced %d, want 0 and 60797", o.CostUSD, o.Unpriced)
	}
	if n := by["new"]; n.CostUSD < 0.0045 || n.CostUSD > 0.0047 || n.RoutedCostUSD != 0.0046 || n.Unpriced != 0 {
		t.Errorf("new = %+v; want the router's $0.0046 only (the anthropic leg is on the harness's bill) and nothing unpriced", n)
	}
	if m := by["mixed"]; m.CostUSD < 0.2499 || m.CostUSD > 0.2501 || m.Unpriced != 60003 {
		t.Errorf("mixed = cost %.4f unpriced %d, want sonnet's $0.25 kept", m.CostUSD, m.Unpriced)
	}
}

// The account window recorded during a run is read back for roscoe top.
func TestReadLedgerWindow(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "task-w")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := `{"ts":"2026-09-02T22:00:00Z","task":"task-w","seq":1,"event":{"type":"result","num_turns":1,"total_cost_usd":0.01,"session_id":"s"}}
{"ts":"2026-09-02T22:00:01Z","kind":"router.limits","task":"task-w","upstream":"anthropic","window":"5h window 5% used, resets in 2h50m","headers":{}}
`
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir, 0, nil)
	if err != nil || len(list) != 1 || list[0].Window != "5h window 5% used, resets in 2h50m" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}
