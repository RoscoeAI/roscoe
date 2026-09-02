package main

import (
	"strings"
	"testing"
)

// A screen that is not active paints nothing, which is what a test wants.
func quietScreen() *screen { return &screen{rows: 24, cols: 80} }

func TestEditorNewlinesAndLineCol(t *testing.T) {
	e := typed("ab")
	e.Newline()
	e.Insert('c')
	if got := e.String(); got != "ab\nc" {
		t.Errorf("text = %q", got)
	}
	if l, c := e.LineCol(); l != 1 || c != 1 {
		t.Errorf("linecol = %d,%d want 1,1", l, c)
	}
	e.Home() // start of the whole buffer, not the line
	if l, c := e.LineCol(); l != 0 || c != 0 {
		t.Errorf("linecol at home = %d,%d", l, c)
	}
	if got := e.Lines(); len(got) != 2 || got[0] != "ab" || got[1] != "c" {
		t.Errorf("lines = %q", got)
	}
	if !e.applyEditKey("alt-enter") || strings.Count(e.String(), "\n") != 2 {
		t.Errorf("alt-enter did not insert a newline: %q", e.String())
	}
}

func TestDecoderPasteAndAltEnter(t *testing.T) {
	cases := map[string]string{"[200~": "paste-start", "[201~": "paste-end", "\r": "alt-enter", "\n": "alt-enter"}
	for seq, want := range cases {
		if got := feed([]byte(seq)...).escapeKey(); got != want {
			t.Errorf("ESC %q -> %q, want %q", seq, got, want)
		}
	}
}

// The defect this fixes: a pasted block's first line must not go out on its
// own. Inside the paste brackets, newlines insert; the enter after the paste
// submits everything at once.
func TestPasteSubmitsWholeBlock(t *testing.T) {
	var keys []byte
	keys = append(keys, 0x1b, '[', '2', '0', '0', '~')
	keys = append(keys, []byte("line one\nline two\n\tindented")...)
	keys = append(keys, 0x1b, '[', '2', '0', '1', '~')
	keys = append(keys, '\r')
	k := feed(keys...)
	got, ok := k.ReadLineOn(quietScreen(), "› ", "", nil, nil)
	if !ok {
		t.Fatal("line was abandoned")
	}
	if got != "line one\nline two\n\tindented" {
		t.Errorf("submitted %q; the paste was not kept whole", got)
	}
}

// Without brackets, a newline is still enter: the terminal is not in paste
// mode, so a real keypress must still submit.
func TestBareNewlineStillSubmits(t *testing.T) {
	k := feed([]byte("first\nsecond\r")...)
	got, ok := k.ReadLineOn(quietScreen(), "› ", "", nil, nil)
	if !ok || got != "first" {
		t.Errorf("got %q ok=%v, want the first line submitted on its own", got, ok)
	}
}

func TestBackslashContinuesTheLine(t *testing.T) {
	k := feed([]byte("one \\\rtwo\r")...)
	got, ok := k.ReadLineOn(quietScreen(), "› ", "", nil, nil)
	if !ok || got != "one \ntwo" {
		t.Errorf("got %q ok=%v; a trailing backslash should become a newline", got, ok)
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	k := feed(append(append([]byte("a"), 0x1b, '\r'), []byte("b\r")...)...)
	got, ok := k.ReadLineOn(quietScreen(), "› ", "", nil, nil)
	if !ok || got != "a\nb" {
		t.Errorf("got %q ok=%v", got, ok)
	}
}

// The box grows one row per line up to the cap, then windows.
func TestBoxHeightTracksLines(t *testing.T) {
	s := quietScreen()
	s.input = "one"
	if s.boxHeight() != 3 {
		t.Errorf("single line box height = %d, want 3", s.boxHeight())
	}
	s.input = "a\nb\nc"
	if s.boxHeight() != 5 {
		t.Errorf("three-line box height = %d, want 5", s.boxHeight())
	}
	s.input = strings.Repeat("x\n", 20) + "x"
	if s.boxHeight() != boxRows-1+maxInputRows {
		t.Errorf("21-line box height = %d, want capped at %d", s.boxHeight(), boxRows-1+maxInputRows)
	}
	s.note = "help"
	if s.boxHeight() != boxRows+maxInputRows {
		t.Errorf("with a note = %d", s.boxHeight())
	}
	// And the viewport shrinks to match, never below one row.
	if s.viewHeight() != s.rows-s.boxHeight() {
		t.Errorf("viewHeight = %d, want rows - box", s.viewHeight())
	}
}

func TestCursorLineCol(t *testing.T) {
	cases := []struct {
		text      string
		cur       int
		line, col int
	}{
		{"abc", 0, 0, 0}, {"abc", 3, 0, 3}, {"abc", 99, 0, 3},
		{"ab\ncd", 2, 0, 2}, {"ab\ncd", 3, 1, 0}, {"ab\ncd", 5, 1, 2},
		{"a\n\nb", 2, 1, 0}, {"a\n\nb", 3, 2, 0}, {"", 0, 0, 0}, {"x", -4, 0, 0},
	}
	for _, c := range cases {
		l, col := cursorLineCol(c.text, c.cur)
		if l != c.line || col != c.col {
			t.Errorf("cursorLineCol(%q,%d) = %d,%d want %d,%d", c.text, c.cur, l, col, c.line, c.col)
		}
	}
}

// Paste during a running turn queues one multi-line message, not one per line.
func TestPendingKeepsMultilineWhole(t *testing.T) {
	p := pending{Queued: []string{"stack trace line 1\n  at foo()\n  at bar()"}}
	if got := p.next(); strings.Count(got, "\n") != 2 {
		t.Errorf("queued block was split: %q", got)
	}
}
