package main

import (
	"errors"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/fleet"
)

func fleetProbes() []fleet.Probe {
	return []fleet.Probe{
		{Node: config.Node{Name: "roscoe", SSH: "roscoe-ts", Workers: 2, Enabled: true}, Reachable: true, Cores: 28,
			Claude: "2.1.251 (Claude Code)", LoggedIn: true, Roscoe: "roscoe v0.28.0 (go1.26.7)", HasConfig: true, HasEnv: true},
		{Node: config.Node{Name: "roscoe-2tb", SSH: "roscoe-2tb-ts", Workers: 2, Enabled: true}, Reachable: true, Cores: 28,
			Claude: "2.1.251 (Claude Code)", Roscoe: "roscoe v0.28.0 (go1.26.7)", HasConfig: true, HasEnv: true},
		{Node: config.Node{Name: "blank", SSH: "blank-ts", Enabled: true}, Reachable: true, Cores: 8, Claude: "missing", Roscoe: "missing"},
		{Node: config.Node{Name: "off", SSH: "off-ts", Enabled: true}, Err: errors.New("ssh: connect to host off-ts port 22: No route to host")},
		{Node: config.Node{Name: "laptop", Enabled: false}},
	}
}

// One row per configured node, and every row says the one thing that matters
// about it: ready, what it needs, why it could not be reached, or that it is
// not in play.
func TestNodesTableSaysWhatEachNodeNeeds(t *testing.T) {
	out := nodesTable(fleetProbes())
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	want := []struct{ node, has, hasNot string }{
		{"roscoe", "ready", "no login"},
		{"roscoe-2tb", "2.1.251, no login", "ready"},
		{"blank", "needs roscoe, config, claude, env", "2.1.251"},
		{"off", "No route to host", "needs"},
		{"laptop", "(this machine)", "needs"},
	}
	for i, w := range want {
		line := lines[i+1]
		if !strings.HasPrefix(strings.TrimSpace(line), w.node) {
			t.Errorf("line %d is not %s: %q", i+1, w.node, line)
		}
		if !strings.Contains(line, w.has) {
			t.Errorf("%s row lacks %q: %q", w.node, w.has, line)
		}
		if strings.Contains(line, w.hasNot) {
			t.Errorf("%s row wrongly says %q: %q", w.node, w.hasNot, line)
		}
	}
	// Full versions are noise in a table; the number alone is the fact.
	if strings.Contains(out, "(Claude Code)") || strings.Contains(out, "go1.") {
		t.Errorf("table shows unshortened versions:\n%s", out)
	}
	if strings.Contains(lines[4], "disabled") {
		t.Error("an unreachable node was called disabled")
	}
}

// After deploy the only thing left is login, and the hint has to switch from
// "deploy" to the login command, or it sends you round in a circle.
func TestNextStepPointsAtTheRealGap(t *testing.T) {
	ps := fleetProbes()
	if got := nextStep(ps); !strings.Contains(got, "roscoe deploy") {
		t.Errorf("with a blank node the hint is %q, want deploy", got)
	}
	logged := ps[:2] // ready + deployed-but-not-logged-in
	got := nextStep(logged)
	if !strings.Contains(got, "ssh -t roscoe-2tb-ts claude auth login") {
		t.Errorf("with only login missing the hint is %q", got)
	}
	if got := nextStep(ps[:1]); got != "" {
		t.Errorf("an all-ready fleet still gets a hint: %q", got)
	}
	// Ready but keyless: the last thing worth saying, and only when nothing
	// else is missing, because env is optional and login is not.
	keyless := ps[0]
	keyless.HasEnv = false
	if got := nextStep([]fleet.Probe{keyless}); !strings.Contains(got, "roscoe deploy --env") {
		t.Errorf("keyless ready node hint = %q", got)
	}
	if got := nextStep([]fleet.Probe{keyless, ps[1]}); !strings.Contains(got, "claude auth login") {
		t.Errorf("login must outrank env: %q", got)
	}
	// A disabled node needs nothing, so it must not trigger deploy.
	if got := nextStep([]fleet.Probe{ps[0], ps[4]}); got != "" {
		t.Errorf("a disabled node produced the hint %q", got)
	}
}

// Refusing to dispatch must say what is missing and how to fix it, on one
// line, because it is the whole of what the operator sees.
func TestNotReadySaysWhyAndHow(t *testing.T) {
	ps := fleetProbes()
	got := notReady(ps[1]) // deployed, no login
	if !strings.Contains(got, "roscoe-2tb is not ready: needs login") || !strings.Contains(got, "claude auth login") {
		t.Errorf("no-login node: %q", got)
	}
	got = notReady(ps[2]) // blank
	if !strings.Contains(got, "needs roscoe, config, claude, env") || !strings.Contains(got, "roscoe deploy") {
		t.Errorf("blank node: %q", got)
	}
	got = notReady(ps[3]) // unreachable
	if !strings.Contains(got, "needs unreachable") {
		t.Errorf("down node: %q", got)
	}
}

// A refused dispatch shows the whole table, so the reason for every node is
// on screen, then the one command that helps.
func TestNoNodeFreeShowsEveryReason(t *testing.T) {
	ps := fleetProbes()
	ps[0].Busy = 2 // the ready node is full
	ps[0].Node.Workers = 2
	got := noNodeFree(ps)
	for _, want := range []string{"no node can take work", "roscoe       roscoe-ts", "0/2", "no login", "needs roscoe, config, claude, env", "roscoe deploy"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal lacks %q:\n%s", want, got)
		}
	}
	// Everything ready but full: the hint is about capacity, not deploy.
	got = noNodeFree(ps[:1])
	if !strings.Contains(got, "worker limit") || strings.Contains(got, "roscoe deploy") {
		t.Errorf("full fleet refusal: %s", got)
	}
}

func TestFreeCell(t *testing.T) {
	p := fleetProbes()[0]
	p.Node.Workers, p.Busy = 3, 1
	if got := freeCell(p); got != "2/3" {
		t.Errorf("freeCell = %q", got)
	}
}

func TestShortVersionAndPin(t *testing.T) {
	cases := map[string]string{
		"roscoe v0.28.0 (go1.26.7)": "v0.28.0",
		"2.1.251 (Claude Code)":     "2.1.251",
		"missing":                   "missing",
		"":                          "missing",
		"  v1 \n":                   "v1",
	}
	for in, want := range cases {
		if got := shortVersion(in); got != want {
			t.Errorf("shortVersion(%q) = %q, want %q", in, got, want)
		}
	}
	old := Version
	defer func() { Version = old }()
	for in, want := range map[string]string{"dev": "", "": "", "v0.29.0": "v0.29.0"} {
		Version = in
		if got := pinnedVersion(); got != want {
			t.Errorf("pinnedVersion with Version=%q = %q, want %q", in, got, want)
		}
	}
}

// The default config's only node is this machine, with no ssh alias. It is
// never probed, so the table must not call it unreachable.
func TestNodesTableLocalRow(t *testing.T) {
	out := nodesTable([]fleet.Probe{{Node: config.Node{Name: "local", Enabled: true, Workers: 2}}})
	if strings.Contains(out, "unreachable") {
		t.Errorf("the local node reads as unreachable:\n%s", out)
	}
	if !strings.Contains(out, "here") || !strings.Contains(out, "roscoe run") {
		t.Errorf("the local row should say it is this machine and how it is used:\n%s", out)
	}
}
