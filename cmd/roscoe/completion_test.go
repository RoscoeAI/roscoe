package main

import (
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// Typing /config should walk the settings a level at a time, with a line of
// help for whatever is under the cursor.
func TestConfigCompletionWalksLevels(t *testing.T) {
	comp := newChatCompleter(config.Default())

	top := comp.candidates("/config ")
	if len(top) == 0 {
		t.Fatal("/config with nothing typed offered no keys")
	}
	for _, p := range top {
		if strings.Contains(p, ".") {
			t.Errorf("/config offered the nested path %q; it should start at the top level", p)
		}
	}

	if got := comp.candidates("/config tiers."); len(got) != 3 {
		t.Errorf("/config tiers. offered %v, want the three tiers", got)
	}
	for _, p := range comp.candidates("/config tiers.middle.") {
		if strings.Count(p, ".") != 2 {
			t.Errorf("/config tiers.middle. offered %q, deeper than one level", p)
		}
	}

	// Once the path is complete, the argument is the value: no path candidates.
	if got := comp.candidates("/config autonomy.level "); got != nil {
		t.Errorf("past the path, candidates should be empty, got %v", got)
	}
	if got := comp.candidates("/config autonomy.level 80"); got != nil {
		t.Errorf("while typing a value, candidates should be empty, got %v", got)
	}
}

func TestConfigCompletionNotes(t *testing.T) {
	comp := newChatCompleter(config.Default())
	cases := []struct{ input, want string }{
		{"/config accounts", "accounts: the Claude credentials workers may run under"},
		{"/config autonomy.level", "autonomy.level: 0 asks you about everything; 100 interrupts you only when credits run out"},
		{"/config autonomy.lev", "autonomy.level: 0 asks you about everything; 100 interrupts you only when credits run out"}, // unambiguous partial
		{"/config autonomy.level 90", "autonomy.level: 0 asks you about everything; 100 interrupts you only when credits run out"},
		{"/config tiers.", "tiers: the three tiers of the fleet: your session, the workers that do the work, the swarm each worker fans out to"}, // mid-walk
		{"/auto", "/autonomy: 0 to 100: how much roscoe decides without asking you; fleet-wide, no tier"},
		{"/settings", "/settings: every tier's model and effort on one screen, under its tier; arrows change them"},
		{"hello", ""},
	}
	for _, tc := range cases {
		if got := comp.noteFor(tc.input); got != tc.want {
			t.Errorf("noteFor(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Completing onto a branch should open it rather than end the token, so tab
// keeps walking down instead of stranding the user on "tiers ".
func TestCompleteOnDescendsIntoBranches(t *testing.T) {
	comp := newChatCompleter(config.Default())
	if got := comp.completeOn("/config auton"); got != "/config autonomy." {
		t.Errorf("completing a branch = %q, want %q", got, "/config autonomy.")
	}
	if got := comp.completeOn("/config autonomy.lev"); got != "/config autonomy.level " {
		t.Errorf("completing a leaf = %q, want %q", got, "/config autonomy.level ")
	}
	if got := comp.completeOn("/auto"); got != "/autonomy " {
		t.Errorf("completing a command = %q, want %q", got, "/autonomy ")
	}
}

// The hint shows choices relative to the level being walked; repeating the
// full dotted path for each sibling does not fit and does not read.
func TestHintIsRelativeToLevel(t *testing.T) {
	comp := newChatCompleter(config.Default())
	hint := comp.hintFor("/config tiers.")
	for _, want := range []string{"main", "middle", "subagents"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q is missing %q", hint, want)
		}
	}
	if strings.Contains(hint, "tiers.main") {
		t.Errorf("hint %q repeats the parent path", hint)
	}
	// The ghost text is the completion itself. A trailing tab glyph reads as
	// part of what you typed, so a single match shows only what tab would add
	// (plus a dot when there is a level below).
	if got := comp.hintFor("/config auton"); got != "omy." {
		t.Errorf("single match hint = %q, want just the completion", got)
	}
	if strings.ContainsAny(comp.hintFor("/setti"), "⇥") {
		t.Error("the hint still carries a tab glyph")
	}
}

// Model, effort, harness and swarm width belong to one tier each, so they have
// no one-word command: /effort silently meant the workers and was misread as
// the session's. They are reached under their tier in /settings or by a path
// that names the tier in /config.
func TestNoTierBoundShortcuts(t *testing.T) {
	for _, gone := range []string{"/effort", "/model", "/harness", "/subagents"} {
		if contains(commands, gone) {
			t.Errorf("%s is back; a tier's knob must not have a command that hides the tier", gone)
		}
		if nearestCommand(gone) == gone {
			t.Errorf("nearestCommand still knows %s", gone)
		}
	}
	comp := newChatCompleter(config.Default())
	if got := comp.candidates("/autonomy "); strings.Join(got, " ") != "0 25 50 75 90 100" {
		t.Errorf("/autonomy offered %v", got)
	}
	// The tier-named path still completes to the levels' owner.
	if got := comp.completeOn("/config tiers.middle.eff"); got != "/config tiers.middle.effort " {
		t.Errorf("effort by path completes to %q", got)
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range commands {
		if commandHelp[c] == "" {
			t.Errorf("command %s has no one-line help", c)
		}
	}
	for _, c := range commands {
		if _, ok := commandHelp[c]; !ok {
			t.Errorf("command %s missing from commandHelp", c)
		}
	}
	for c := range commandArgs {
		if !contains(commands, c) {
			t.Errorf("args for %s, which is not a command", c)
		}
	}
	for c := range commandHelp {
		if !contains(commands, c) {
			t.Errorf("help for %s, which is not a command", c)
		}
	}
}

// A mistyped slash command must never become a prompt. Falling through spawns
// a worker, costs a turn, and answers with Claude Code's own "unknown command"
// text, which reads as though roscoe said it.
func TestNearestCommand(t *testing.T) {
	cases := map[string]string{
		"/setting":  "/settings", // the real one, hit in a live session
		"/settigns": "/settings",
		"/autonmy":  "/autonomy",
		"/conifg":   "/config",
		"/halp":     "/help",
		"/set":      "/settings", // prefix match
		"/hel":      "/help",
	}
	for typed, want := range cases {
		if got := nearestCommand(typed); got != want {
			t.Errorf("nearestCommand(%q) = %q, want %q", typed, got, want)
		}
	}
	// A wrong suggestion is worse than none.
	for _, typed := range []string{"/xyzzy", "/deploy-everything-now"} {
		if got := nearestCommand(typed); got != "" {
			t.Errorf("nearestCommand(%q) = %q, want no guess", typed, got)
		}
	}
	// Every real command suggests itself.
	for _, c := range commands {
		if got := nearestCommand(c); got != c {
			t.Errorf("nearestCommand(%q) = %q; a real command must match itself", c, got)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"a", "", 1}, {"", "abc", 3},
		{"kitten", "sitting", 3}, {"same", "same", 0}, {"ab", "ba", 2},
	}
	for _, tc := range cases {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
