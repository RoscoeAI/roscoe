package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/sessions"
)

// The picker leaves out the session you are in and anything that cannot be
// resumed, and says what each row was about.
func TestSessionRows(t *testing.T) {
	now := time.Now()
	list := []sessions.Session{
		{ID: "cur-session", Ended: now, About: "the one we are in"},
		{ID: "aaaaaaaa-1", Ended: now.Add(-time.Hour), CostUSD: 0.5, About: "fix the auth bug"},
		{ID: "", Ended: now.Add(-2 * time.Hour), About: "a run with no session"},
		{ID: "bbbbbbbb-2", Ended: now.Add(-48 * time.Hour), Dir: "/Users/x/proj", About: ""},
	}
	rows, lines := sessionRows(list, "cur-session", now)
	if len(rows) != 2 || len(lines) != 2 {
		t.Fatalf("rows %d lines %d", len(rows), len(lines))
	}
	if rows[0].ID != "aaaaaaaa-1" || !strings.Contains(lines[0], "fix the auth bug") || !strings.Contains(lines[0], "$0.50") {
		t.Errorf("row 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "proj") {
		t.Errorf("a row with nothing said should fall back to the directory: %q", lines[1])
	}
	for _, l := range lines {
		if strings.Contains(l, "the one we are in") {
			t.Error("the current session is offered for resume")
		}
	}
}

// Every chat command has help, a group, and a place in the completion list.
func TestResumeIsAFirstClassCommand(t *testing.T) {
	if !contains(commands, "/resume") {
		t.Error("/resume is not offered for completion")
	}
	if commandHelp["/resume"] == "" {
		t.Error("/resume has no help line")
	}
	found := false
	for _, g := range helpGroups {
		if contains(g.cmds, "/resume") && g.title == "this conversation" {
			found = true
		}
	}
	if !found {
		t.Error("/resume is not under \"this conversation\" in /help")
	}
}

// A /config value that parses but fails validation must not reach the file
// or stay in memory: the file is what every later command loads.
func TestSetConfigInChatValidatesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roscoe.json")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	sc := &screen{rows: 24, cols: 80, liveStart: -1, out: io.Discard}

	if setConfigInChat(sc, cfg, path, "tiers.middle.effort", "turbo") {
		t.Error("an effort that is not a level was accepted")
	}
	if got, _ := cfg.Get("tiers.middle.effort"); got == "turbo" {
		t.Error("the bad value stayed in memory")
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
	if !setConfigInChat(sc, cfg, path, "tiers.middle.effort", "high") {
		t.Error("a valid effort was refused")
	}
	saved, err := config.Load(path)
	if err != nil || saved.Tiers.Middle.Effort != "high" {
		t.Errorf("file has effort %q, err %v", saved.Tiers.Middle.Effort, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "turbo") {
		t.Error("turbo reached the file")
	}
}
