package main

import (
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/sessions"
)

// Wednesday 2026-09-02 14:00 local: today starts at 00:00 that day, the week
// on Monday 2026-08-31.
var topNow = time.Date(2026, 9, 2, 14, 0, 0, 0, time.Local)

func topSessions() []sessions.Session {
	return []sessions.Session{
		{TaskID: "t-now", ID: "aaaa1111", Started: topNow.Add(-time.Hour), Ended: topNow.Add(-50 * time.Minute), Turns: 3, CostUSD: 0.40, About: "fix the tests", Window: "5h window 5% used, resets in 2h50m"},
		{TaskID: "t-early", ID: "bbbb2222", Started: startOfDay(topNow).Add(time.Minute), Ended: startOfDay(topNow).Add(10 * time.Minute), Turns: 1, CostUSD: 0.10, About: "morning"},
		{TaskID: "t-mon", ID: "cccc3333", Started: startOfWeek(topNow).Add(9 * time.Hour), Ended: startOfWeek(topNow).Add(10 * time.Hour), Turns: 8, CostUSD: 2.00, About: "monday"},
		{TaskID: "t-last-week", ID: "dddd4444", Started: startOfWeek(topNow).Add(-time.Hour), Ended: startOfWeek(topNow).Add(-30 * time.Minute), Turns: 2, CostUSD: 5.00, About: "sunday night"},
	}
}

func TestSpendBoundaries(t *testing.T) {
	list := topSessions()
	today := spendSince(list, startOfDay(topNow))
	if today.Runs != 2 || today.Turns != 4 || today.Cost != 0.50 {
		t.Errorf("today = %+v", today)
	}
	week := spendSince(list, startOfWeek(topNow))
	if week.Runs != 3 || week.Cost != 2.50 {
		t.Errorf("week = %+v; Sunday night must not count toward a Monday-start week", week)
	}
	if startOfWeek(topNow).Weekday() != time.Monday {
		t.Errorf("week starts on %s", startOfWeek(topNow).Weekday())
	}
	if got := (spend{}).String(); got != "nothing" {
		t.Errorf("empty spend = %q", got)
	}
	if got := (spend{Runs: 1, Turns: 1, Cost: 0.1}).String(); got != "$0.10 · 1 run · 1 turn" {
		t.Errorf("singular = %q", got)
	}
}

// The screen leads with the money, says what is running here and on the
// fleet, and shows only as many sessions as asked for.
func TestRenderTop(t *testing.T) {
	probes := fleetProbes()[:2] // one ready, one deployed without login
	probes[0].Busy = 1
	out := renderTop(topData{Now: topNow, Sessions: topSessions(), Here: 2, Probes: probes, Recent: 2})
	for _, want := range []string{
		"today     $0.50 · 2 runs · 4 turns",
		"week      $2.50 · 3 runs · 12 turns",
		"2 workers here · fleet 1/4 slots busy, 1 node ready",
		"account   5h window 5% used, resets in 2h50m  (as of 50m ago)",
		"roscoe-2tb   roscoe-2tb-ts",
		"fix the tests",
		"morning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("top lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "monday") {
		t.Errorf("recent=2 showed a third session:\n%s", out)
	}
	// Without a fleet probe there is no fleet line and no table.
	solo := renderTop(topData{Now: topNow, Sessions: topSessions(), Here: 0, Recent: 5})
	if strings.Contains(solo, "fleet") || strings.Contains(solo, "node ") {
		t.Errorf("no-fleet render mentions the fleet:\n%s", solo)
	}
	if !strings.Contains(solo, "0 workers here") {
		t.Errorf("zero workers not stated:\n%s", solo)
	}
	empty := renderTop(topData{Now: topNow, Recent: 5})
	if !strings.Contains(empty, "today     nothing") || !strings.Contains(empty, "no sessions yet") {
		t.Errorf("empty render:\n%s", empty)
	}
}
