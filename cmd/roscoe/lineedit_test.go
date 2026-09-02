package main

import "testing"

func typed(s string) *lineEditor {
	e := &lineEditor{}
	for _, r := range s {
		e.Insert(r)
	}
	return e
}

func want(t *testing.T, e *lineEditor, text string, cur int) {
	t.Helper()
	if e.String() != text || e.Cursor() != cur {
		t.Errorf("got %q@%d, want %q@%d", e.String(), e.Cursor(), text, cur)
	}
}

func TestInsertAtCursor(t *testing.T) {
	e := typed("hllo")
	e.Home()
	e.Right()
	e.Insert('e')
	want(t, e, "hello", 2)
	e.End()
	e.Insert('!')
	want(t, e, "hello!", 6)
}

func TestBackspaceAndDelete(t *testing.T) {
	e := typed("abc")
	e.Left() // cursor between b and c
	if !e.Backspace() {
		t.Error("backspace with text before cursor returned false")
	}
	want(t, e, "ac", 1)
	if !e.Delete() {
		t.Error("delete with text under cursor returned false")
	}
	want(t, e, "a", 1)
	if e.Delete() {
		t.Error("delete at end returned true")
	}
	e.Home()
	if e.Backspace() {
		t.Error("backspace at start returned true")
	}
	want(t, e, "a", 0)
}

func TestMovementClamps(t *testing.T) {
	e := typed("ab")
	e.Left()
	e.Left()
	e.Left()
	want(t, e, "ab", 0)
	e.Right()
	e.Right()
	e.Right()
	want(t, e, "ab", 2)
	e.Home()
	want(t, e, "ab", 0)
	e.End()
	want(t, e, "ab", 2)
}

func TestWordJumps(t *testing.T) {
	e := typed("fix the   billing  module")
	e.WordLeft()
	want(t, e, "fix the   billing  module", 19) // start of "module"
	e.WordLeft()
	want(t, e, "fix the   billing  module", 10) // start of "billing"
	e.Home()
	e.WordRight()
	want(t, e, "fix the   billing  module", 3) // end of "fix"
	e.WordRight()
	want(t, e, "fix the   billing  module", 7) // end of "the"
	e.WordRight()
	want(t, e, "fix the   billing  module", 17) // end of "billing", spaces skipped first
}

func TestKillKeys(t *testing.T) {
	e := typed("one two three")
	if !e.KillWordBack() {
		t.Error("kill-word-back returned false")
	}
	want(t, e, "one two ", 8)
	e.KillWordBack()
	want(t, e, "one ", 4)

	e = typed("keep this drop this")
	e.Home()
	for i := 0; i < 9; i++ {
		e.Right()
	}
	if !e.KillToEnd() {
		t.Error("kill-to-end returned false")
	}
	want(t, e, "keep this", 9)
	if e.KillToEnd() {
		t.Error("kill-to-end at end returned true")
	}

	e = typed("drop this keep this")
	e.Home()
	for i := 0; i < 10; i++ {
		e.Right()
	}
	if !e.KillToStart() {
		t.Error("kill-to-start returned false")
	}
	want(t, e, "keep this", 0)
	if e.KillToStart() {
		t.Error("kill-to-start at start returned true")
	}
}

func TestSetAndClear(t *testing.T) {
	e := typed("x")
	e.Set("recalled prompt")
	want(t, e, "recalled prompt", 15)
	e.Clear()
	want(t, e, "", 0)
	if !e.Empty() {
		t.Error("cleared editor is not empty")
	}
}

// The prompt and the during-turn box both route keys through this, so it is
// the one place that decides what a key does.
func TestApplyEditKey(t *testing.T) {
	e := typed("ab cd")
	cases := []struct {
		key  string
		text string
		cur  int
	}{
		{"left", "ab cd", 4},
		{"\x01", "ab cd", 0}, // ctrl-a
		{"word-right", "ab cd", 2},
		{"x", "abx cd", 3},
		{"\x7f", "ab cd", 2}, // backspace
		{"\x05", "ab cd", 5}, // ctrl-e
		{"\x17", "ab ", 3},   // ctrl-w
		{"home", "ab ", 0},
		{"delete", "b ", 0},
		{"\x0b", "", 0}, // ctrl-k
	}
	for _, c := range cases {
		if !e.applyEditKey(c.key) {
			t.Errorf("key %q not treated as an edit", c.key)
		}
		want(t, e, c.text, c.cur)
	}
	for _, k := range []string{"enter", "esc", "tab", "up", "eof", "", "ctrl-c", "\x03"} {
		if e.applyEditKey(k) {
			t.Errorf("key %q wrongly treated as an edit", k)
		}
	}
}

// Multi-rune input is inserted as runes, so a later Left steps over one
// character, not one byte.
func TestRunesNotBytes(t *testing.T) {
	e := &lineEditor{}
	for _, r := range "héllo" {
		e.Insert(r)
	}
	want(t, e, "héllo", 5)
	e.Left()
	e.Left()
	e.Left()
	e.Left()
	e.Delete()
	want(t, e, "hllo", 1)
}
