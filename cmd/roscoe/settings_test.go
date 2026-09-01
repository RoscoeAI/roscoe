package main

import (
	"fmt"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// The panel's whole purpose is that all three tiers are on one surface, so a
// missing tier is a real defect rather than a cosmetic one.
func TestSettingsCoversEveryTier(t *testing.T) {
	rows := settingsRows(config.Default())

	var headings []string
	for _, r := range rows {
		if r.heading != "" {
			headings = append(headings, r.heading)
		}
	}
	if len(headings) != 4 {
		t.Fatalf("want tier 1, tier 2, tier 3 and fleet, got %v", headings)
	}
	for i, want := range []string{"tier 1", "tier 2", "tier 3", "fleet"} {
		if !strings.HasPrefix(headings[i], want) {
			t.Errorf("heading %d = %q, want it to start with %q", i, headings[i], want)
		}
	}

	// Each tier states its model, provider and effort, even where effort is
	// not settable: silence would read as an oversight.
	var tier, labels = "", map[string][]string{}
	for _, r := range rows {
		if r.heading != "" {
			f := strings.Fields(r.heading)
			tier = f[0]
			if len(f) > 1 { // "tier 2", but plain "fleet"
				tier += " " + f[1]
			}
			continue
		}
		labels[tier] = append(labels[tier], r.label)
	}
	for _, tr := range []string{"tier 1", "tier 2", "tier 3"} {
		for _, want := range []string{"model", "provider", "effort"} {
			if !contains(labels[tr], want) {
				t.Errorf("%s is missing a %s row; it has %v", tr, want, labels[tr])
			}
		}
	}
}

// A typo in a path would make a row silently unsettable, so every path has to
// resolve against the config it came from.
func TestSettingsPathsResolve(t *testing.T) {
	cfg := config.Default()
	for _, r := range settingsRows(cfg) {
		if r.heading != "" {
			continue
		}
		if !r.editable() {
			if r.why == "" {
				t.Errorf("row %q is not editable and does not say why", r.label)
			}
			continue
		}
		if len(r.choices) == 0 {
			t.Errorf("row %q offers no choices, so left/right does nothing", r.label)
			continue
		}
		// Settable is the claim the row makes, so prove it by setting it.
		// Get is not enough: a field left at its zero value is omitted from
		// the marshalled config and reads as missing.
		if err := cfg.SetPath(r.path, r.choices[0]); err != nil {
			t.Errorf("row %q points at %q, which cannot be set: %v", r.label, r.path, err)
			continue
		}
		if got, err := cfg.Get(r.path); err != nil {
			t.Errorf("row %q: %q did not read back after being set: %v", r.label, r.path, err)
		} else if fmt.Sprintf("%v", got) != r.choices[0] {
			t.Errorf("row %q: set %q to %q, read back %v", r.label, r.path, r.choices[0], got)
		}
		if config.Describe(r.path) == "" {
			t.Errorf("row %q (%s) has no description for the help line", r.label, r.path)
		}
	}
}

func TestSettingsChoicesAreReal(t *testing.T) {
	cfg := config.Default()
	for _, r := range settingsRows(cfg) {
		switch r.path {
		case "tiers.middle.effort":
			if strings.Join(r.choices, " ") != strings.Join(config.EffortLevels(), " ") {
				t.Errorf("effort choices %v, want %v", r.choices, config.EffortLevels())
			}
		case "tiers.main.provider", "tiers.middle.provider", "tiers.subagents.provider":
			for _, c := range r.choices {
				if _, ok := cfg.Providers[c]; !ok {
					t.Errorf("%s offers %q, which is not a configured provider", r.path, c)
				}
			}
		}
	}
}

func TestNextSelectableSkipsUnsettable(t *testing.T) {
	rows := settingsRows(config.Default())
	first := firstSelectable(rows)
	if rows[first].heading != "" || !rows[first].editable() {
		t.Fatalf("first selection landed on %+v", rows[first])
	}

	// Walking the whole panel must only ever land on editable rows.
	seen := map[int]bool{}
	at := first
	for i := 0; i < len(rows)+2; i++ {
		next := nextSelectable(rows, at, 1)
		if !rows[next].editable() {
			t.Fatalf("moved onto a non-editable row: %+v", rows[next])
		}
		if next == at { // clamped at the end
			break
		}
		seen[next] = true
		at = next
	}
	if at == first {
		t.Fatal("down never moved")
	}
	// And back up to the top, never past it.
	for i := 0; i < len(rows)+2; i++ {
		prev := nextSelectable(rows, at, -1)
		if prev == at {
			break
		}
		at = prev
	}
	if at != first {
		t.Errorf("walking up ended at %d, want the first selectable row %d", at, first)
	}
}

func TestCycleChoice(t *testing.T) {
	levels := config.EffortLevels() // low medium high xhigh max ultracode
	cases := []struct {
		current string
		step    int
		want    string
	}{
		{"high", 1, "xhigh"},
		{"high", -1, "medium"},
		{"low", -1, ""},                  // clamped at the start
		{"ultracode", 1, ""},             // clamped at the end
		{"ultracode", -1, "max"},         //
		{"not-a-level", 1, "low"},        // an off-list value enters at the start
		{"not-a-level", -1, "ultracode"}, // or the end, going the other way
	}
	for _, tc := range cases {
		if got := cycleChoice(levels, tc.current, tc.step); got != tc.want {
			t.Errorf("cycleChoice(%q, %d) = %q, want %q", tc.current, tc.step, got, tc.want)
		}
	}
	if got := cycleChoice(nil, "x", 1); got != "" {
		t.Errorf("cycling an empty list = %q, want empty", got)
	}
}

// On a short terminal the panel must window rather than clip, or the rows you
// are trying to reach are the ones that vanish.
func TestRenderSettingsKeepsSelectionVisible(t *testing.T) {
	rows := settingsRows(config.Default())
	last := 0
	for i, r := range rows {
		if r.editable() {
			last = i
		}
	}
	const height = 8
	out := renderSettings(rows, last, height)
	if len(out) != height {
		t.Fatalf("rendered %d lines into a height of %d", len(out), height)
	}
	found := false
	for _, line := range out {
		if strings.Contains(line, rows[last].label) && strings.Contains(line, "›") {
			found = true
		}
	}
	if !found {
		t.Errorf("the selected row %q scrolled out of a %d-row window:\n%s",
			rows[last].label, height, strings.Join(out, "\n"))
	}

	// With room to spare, nothing is dropped.
	full := renderSettings(rows, 1, 100)
	if len(full) < len(rows) {
		t.Errorf("full render has %d lines for %d rows", len(full), len(rows))
	}
}

func TestNoteForRowAlwaysSaysSomething(t *testing.T) {
	for _, r := range settingsRows(config.Default()) {
		if r.heading != "" {
			continue
		}
		if strings.TrimSpace(noteForRow(r)) == "" {
			t.Errorf("row %q has an empty help line", r.label)
		}
	}
}

// /settings is sold as the one surface for how the fleet is configured, so a
// knob that ships without a row here quietly makes that claim false. These two
// are the largest cost levers in the config.
func TestSettingsCoversTheCostKnobs(t *testing.T) {
	rows := settingsRows(config.Default())
	var paths []string
	for _, r := range rows {
		if r.editable() {
			paths = append(paths, r.path)
		}
	}
	for _, want := range []string{"tiers.middle.lean_context", "tiers.middle.cache_ttl"} {
		if !contains(paths, want) {
			t.Errorf("%s has no row; /settings has drifted from the config", want)
		}
	}
}

// A bare "true" says nothing about what it does, and this panel is the only
// place most people will ever read about it.
func TestLeanRowExplainsItself(t *testing.T) {
	for _, r := range settingsRows(config.Default()) {
		if r.path != "tiers.middle.lean_context" {
			continue
		}
		if r.raw != "true" {
			t.Errorf("lean raw = %q, want the default true", r.raw)
		}
		if !strings.Contains(r.shown(), "MCP") {
			t.Errorf("the lean row shows %q, which does not say what it strips", r.shown())
		}
		return
	}
	t.Fatal("no lean row")
}
