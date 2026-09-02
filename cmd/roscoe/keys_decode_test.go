package main

import "testing"

// feed builds a keyReader whose stdin is the given bytes, no tty required.
func feed(b ...byte) *keyReader {
	k := &keyReader{events: make(chan byte, len(b)+1)}
	for _, c := range b {
		k.events <- c
	}
	return k
}

// Every sequence a real terminal sends for the editing keys must decode to a
// name the editor acts on; anything unknown must be swallowed as "", never
// misread as esc, which would abandon the line.
func TestEscapeKeyDecodes(t *testing.T) {
	cases := []struct {
		seq  string
		want string
	}{
		{"[A", "up"}, {"[B", "down"}, {"[C", "right"}, {"[D", "left"},
		{"[H", "home"}, {"[F", "end"}, {"OH", "home"}, {"OF", "end"},
		{"[1~", "home"}, {"[7~", "home"}, {"[4~", "end"}, {"[8~", "end"},
		{"[3~", "delete"}, {"[5~", "pgup"}, {"[6~", "pgdn"},
		{"[1;5D", "word-left"}, {"[1;5C", "word-right"}, // ctrl-arrows
		{"[1;3D", "word-left"}, {"[1;3C", "word-right"}, // alt-arrows
		{"b", "word-left"}, {"f", "word-right"}, // alt-b / alt-f
		{"\x7f", "kill-word"},               // alt-backspace
		{"[99~", ""}, {"[Z", ""}, {"x", ""}, // unknown: swallowed
	}
	for _, c := range cases {
		k := feed([]byte(c.seq)...)
		if got := k.escapeKey(); got != c.want {
			t.Errorf("ESC %q -> %q, want %q", c.seq, got, c.want)
		}
	}
}

// A lone escape with nothing after it is the esc key.
func TestBareEscape(t *testing.T) {
	k := feed()
	if got := k.escapeKey(); got != "esc" {
		t.Errorf("bare ESC -> %q, want esc", got)
	}
}

func TestNextKeyNamesControlKeys(t *testing.T) {
	cases := map[byte]string{'\r': "enter", '\n': "enter", '\t': "tab", 0x03: "ctrl-c", 'a': "a"}
	for b, want := range cases {
		if got := feed(b).NextKey(); got != want {
			t.Errorf("byte %#x -> %q, want %q", b, got, want)
		}
	}
	// Raw control bytes the editor understands pass through as themselves.
	for _, b := range []byte{0x01, 0x05, 0x0b, 0x15, 0x17, 0x7f} {
		if got := feed(b).NextKey(); got != string(rune(b)) {
			t.Errorf("byte %#x -> %q, want it passed through", b, got)
		}
	}
	if got := feed(0x1b, '[', 'H').NextKey(); got != "home" {
		t.Errorf("ESC [ H via NextKey -> %q", got)
	}
}

// The box must scroll a long line so the cursor stays visible, and a line
// that fits must never scroll at all.
func TestInputWindow(t *testing.T) {
	cases := []struct{ n, cur, avail, want int }{
		{5, 5, 10, 0},    // fits: never scroll
		{5, 0, 10, 0},    //
		{20, 20, 10, 10}, // cursor at end of a long line: show the tail
		{20, 0, 10, 0},   // cursor at start of a long line: show the head
		{20, 15, 10, 6},  // cursor mid-line: window ends just after it
		{20, 9, 10, 0},   // cursor within the first window: no scroll
		{20, 10, 10, 1},  // one past the first window: scroll by one
		{20, 19, 10, 10}, // near the end: clamp so the tail fills the box
		{3, 1, 0, 0},     // degenerate width
	}
	for _, c := range cases {
		if got := inputWindow(c.n, c.cur, c.avail); got != c.want {
			t.Errorf("inputWindow(n=%d cur=%d avail=%d) = %d, want %d", c.n, c.cur, c.avail, got, c.want)
		}
	}
	// The cursor is always inside the chosen window.
	for n := 0; n < 40; n++ {
		for cur := 0; cur <= n; cur++ {
			ws := inputWindow(n, cur, 12)
			if cur < ws || cur > ws+12 {
				t.Fatalf("cursor %d outside window [%d,%d] for n=%d", cur, ws, ws+12, n)
			}
		}
	}
}
