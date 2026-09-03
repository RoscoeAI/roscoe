package main

import (
	"strings"
	"testing"
	"time"

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
