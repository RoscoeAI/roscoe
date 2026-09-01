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
		{"/config accounts", "accounts: Claude credentials the fleet may use"},
		{"/config autonomy.level", "autonomy.level: 0-100; at 100 only exhausted credits interrupt you"},
		{"/config autonomy.lev", "autonomy.level: 0-100; at 100 only exhausted credits interrupt you"}, // unambiguous partial
		{"/config autonomy.level 90", "autonomy.level: 0-100; at 100 only exhausted credits interrupt you"},
		{"/config tiers.", "tiers: the three tiers: your session, the workers, the swarm"}, // mid-walk
		{"/mod", "/model: the model your workers run"},
		{"/harness", "/harness: which CLI the workers run: claude or codex"},
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
	if got := comp.completeOn("/harn"); got != "/harness " {
		t.Errorf("completing a command = %q, want %q", got, "/harness ")
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
	if got := comp.hintFor("/config auton"); !strings.HasSuffix(got, "⇥") {
		t.Errorf("a single match should show the tab affordance, got %q", got)
	}
}

func TestEffortCompletion(t *testing.T) {
	comp := newChatCompleter(config.Default())
	got := comp.candidates("/effort ")
	if strings.Join(got, " ") != strings.Join(config.EffortLevels(), " ") {
		t.Errorf("/effort offered %v, want %v", got, config.EffortLevels())
	}
	if !contains(got, "ultracode") {
		t.Error("ultracode must be offered; it is the default and claude --help omits it")
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
