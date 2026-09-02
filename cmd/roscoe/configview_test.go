package main

import (
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// The card must answer the four questions before someone changes a setting:
// what it is now, what it does, what it can be, and how to change it, with
// the one-word command when there is one.
func TestLeafCardForEffort(t *testing.T) {
	cfg := config.Default()
	card := buildLeafCard(cfg, "tiers.middle.effort", "/config", shortcutFor("tiers.middle.effort"))
	lines := strings.Join(card.lines(), "\n")
	for _, want := range []string{
		"tiers.middle.effort = " + cfg.Tiers.Middle.Effort,
		"options  low · medium · high · xhigh · max · ultracode",
		"means    ultracode",
		"set it   /config tiers.middle.effort <value>",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("card lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "   or   ") {
		t.Errorf("a tier's knob must not advertise a one-word command:\n%s", lines)
	}
	// The fleet-wide knob keeps its shortcut, and the card says so.
	auto := strings.Join(buildLeafCard(cfg, "autonomy.level", "/config", shortcutFor("autonomy.level")).lines(), "\n")
	if !strings.Contains(auto, "   or   /autonomy <value>") {
		t.Errorf("autonomy card lacks its shortcut:\n%s", auto)
	}
	// A free-form setting shows its format; one absent from the file shows
	// what the fleet runs with, marked as the default.
	card = buildLeafCard(cfg, "reporting.git_remote", "roscoe config set", "")
	text := strings.Join(card.lines(), "\n")
	if !strings.Contains(text, "options  a git remote") {
		t.Errorf("free-form card = %+v", card)
	}
	if got := buildLeafCard(cfg, "tiers.middle.harness", "/config", "").Value; got != "claude (default)" {
		t.Errorf("harness absent from the file shows %q, want the implied default", got)
	}
	cfg.Reporting.GitRemote = ""
	if got := buildLeafCard(cfg, "reporting.git_remote", "/config", "").Value; got != "local branches only (default)" {
		t.Errorf("cleared git_remote shows %q", got)
	}
	if strings.Contains(text, "   or   ") {
		t.Error("a setting with no shortcut claims one")
	}
}

// Every one-word command points at a real leaf, and the reverse lookup finds it.
func TestShortcutsAreRealSettings(t *testing.T) {
	cfg := config.Default()
	for cmd, path := range shortcuts {
		if _, err := cfg.Get(path); err != nil {
			if _, ok := config.Implied(path); !ok {
				t.Errorf("%s points at %q: %v", cmd, path, err)
			}
		}
		if shortcutFor(path) != cmd {
			t.Errorf("shortcutFor(%q) = %q", path, shortcutFor(path))
		}
		if !contains(commands, cmd) {
			t.Errorf("%s is not a chat command", cmd)
		}
	}
}

// The top level leads with what people change, sets one-time setup apart,
// and loses nothing; a branch shows leaves with their values.
func TestTopLevelListingIsGrouped(t *testing.T) {
	cfg := config.Default()
	lines := levelLines(cfg, "")
	text := strings.Join(lines, "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "tiers") {
		t.Errorf("first row is %q, want tiers", lines[1])
	}
	setup := strings.Index(text, "set up once")
	if setup < 0 {
		t.Fatal("no setup section")
	}
	for _, k := range []string{"project", "state_dir", "env_file", "version"} {
		if i := strings.Index(text, "\n  "+k); i < setup {
			t.Errorf("%s is above the setup heading", k)
		}
	}
	for _, k := range cfg.ChildPaths("") {
		if !strings.Contains(text, "\n  "+k) {
			t.Errorf("%s missing from the listing", k)
		}
	}
	mid := strings.Join(levelLines(cfg, "tiers.middle"), "\n")
	if !strings.Contains(mid, "effort") || !strings.Contains(mid, cfg.Tiers.Middle.Effort) {
		t.Errorf("tiers.middle listing:\n%s", mid)
	}
	if !strings.Contains(mid, "claude (default)") {
		t.Errorf("a setting absent from the file should show its implied default:\n%s", mid)
	}
}

// /help puts every command in exactly one group, and every command has a line.
func TestHelpGroupsCoverEveryCommand(t *testing.T) {
	seen := map[string]int{}
	for _, g := range helpGroups {
		for _, c := range g.cmds {
			seen[c]++
			if commandHelp[c] == "" {
				t.Errorf("%s has no help line", c)
			}
		}
	}
	for _, c := range commands {
		if seen[c] != 1 {
			t.Errorf("%s appears %d times in /help groups", c, seen[c])
		}
	}
	if len(seen) != len(commands) {
		t.Errorf("groups list %d commands, chat has %d", len(seen), len(commands))
	}
}
