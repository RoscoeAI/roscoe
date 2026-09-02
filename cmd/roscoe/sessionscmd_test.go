package main

import (
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/sessions"
)

func TestSessionsTable(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	list := []sessions.Session{
		{ID: "abcdef12-3456", Ended: now.Add(-2 * time.Hour), CostUSD: 3.2376, Turns: 23, About: "can you continue?"},
		{ID: "", Ended: now.Add(-3 * 24 * time.Hour), Dir: "/Users/x/Projects/node/orch"},
		{ID: "ffff0000-9999", Ended: now.Add(-30 * time.Second), CostUSD: 0, Turns: 0,
			About: strings.Repeat("a very long first prompt that keeps going ", 5)},
	}
	out := sessionsTable(list, now)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[1], "2h ago") || !strings.Contains(lines[1], "abcdef12") || !strings.Contains(lines[1], "$3.24") || !strings.Contains(lines[1], "can you continue?") {
		t.Errorf("row 1 = %q", lines[1])
	}
	if !strings.Contains(lines[2], "(none)") || !strings.Contains(lines[2], "orch") {
		t.Errorf("a session with no id should say so and fall back to its dir: %q", lines[2])
	}
	if !strings.Contains(lines[3], "just now") || !strings.Contains(lines[3], "…") {
		t.Errorf("a long prompt should be cut with an ellipsis: %q", lines[3])
	}
	// Zero cost reads as a dash, not as a free run that cost money.
	if strings.Contains(lines[3], "$0.00") {
		t.Errorf("zero cost rendered as money: %q", lines[3])
	}
}

// A session with nothing to show must say so, not print colour codes around
// an empty string, which come out as literal garbage when piped.
func TestSessionsTableEmptyRow(t *testing.T) {
	now := time.Now()
	out := sessionsTable([]sessions.Session{{Ended: now}}, now)
	if !strings.Contains(out, "(no transcript)") {
		t.Errorf("empty session row = %q", out)
	}
	if strings.Contains(out, ansiFaint+ansiReset) {
		t.Errorf("colour codes wrapped nothing: %q", out)
	}
}

// Every loop iteration starts with the same kernel prompt, so the charter at
// its end is what identifies the run.
func TestAboutFromPrompt(t *testing.T) {
	kernel := "Read loop.md in the working directory. It is your memory...\n\nDo the next useful piece of work.\n\nCharter: Finish roscoe's autonomy stack: the quorum, then memory."
	if got := aboutFromPrompt(kernel); got != "Finish roscoe's autonomy stack: the quorum, then memory." {
		t.Errorf("about = %q", got)
	}
	if got := aboutFromPrompt("fix the billing module"); got != "fix the billing module" {
		t.Errorf("a plain prompt should pass through, got %q", got)
	}
}

// A fleet run says which node it ran on, first, because everything else on
// the row (resume, transcript) means something different for it.
func TestSessionsTableNamesTheNode(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	rows := []sessions.Session{
		{TaskID: "t1", ID: "bf4cbb82", Node: "roscoe", Dir: "/x/.roscoe/work/t1", Ended: now.Add(-time.Hour)},
		{TaskID: "t2", ID: "aaaabbbb", About: "fix the tests", Ended: now.Add(-time.Hour)},
	}
	out := sessionsTable(rows, now)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.Contains(lines[1], "on roscoe") || !strings.Contains(lines[1], ".roscoe/work/t1") {
		t.Errorf("fleet row = %q", lines[1])
	}
	if strings.Contains(lines[2], "on ") {
		t.Errorf("local row claims a node: %q", lines[2])
	}
}

// A run whose only cost was the harness's guess shows no cost, a marker, and
// one footnote for the table; priced runs carry no marker.
func TestSessionsTableMarksUnpriced(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	rows := []sessions.Session{
		{TaskID: "a", ID: "aaaa1111", Unpriced: 60797, About: "old tier-3 probe", Ended: now.Add(-time.Hour)},
		{TaskID: "b", ID: "bbbb2222", CostUSD: 0.25, Unpriced: 60003, About: "mixed", Ended: now.Add(-time.Hour)},
		{TaskID: "c", ID: "cccc3333", CostUSD: 0.0046, RoutedCostUSD: 0.0046, About: "priced by the router", Ended: now.Add(-time.Hour)},
	}
	out := sessionsTable(rows, now)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("want header, 3 rows, footnote:\n%s", out)
	}
	if !strings.Contains(lines[1], " -*") || !strings.Contains(lines[2], "$0.25*") || strings.Contains(lines[3], "*") {
		t.Errorf("markers wrong:\n%s", out)
	}
	if !strings.Contains(lines[4], "120K tokens") {
		t.Errorf("footnote = %q", lines[4])
	}
	if strings.Contains(sessionsTable(rows[2:], now), "*") {
		t.Error("a fully priced listing has a footnote")
	}
}

func TestShortDirCutsLongPaths(t *testing.T) {
	long := "/private/tmp/claude-501/-Users-t-Projects-node-orch/a1c55021-68ad-424c-92cd-7ffaf6026668/scratchpad/mcptest"
	if got := shortDir(long); got != "…/scratchpad/mcptest" {
		t.Errorf("shortDir(long) = %q", got)
	}
	if got := shortDir("/Users/x/proj"); got != "/Users/x/proj" {
		t.Errorf("short path changed: %q", got)
	}
	if got := shortDir(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}
