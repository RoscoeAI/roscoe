package main

import (
	"io"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"5551234567":        "+15551234567",
		"(555) 123-4567":    "+15551234567",
		"15551234567":       "+15551234567",
		"+1 555 123 4567":   "+15551234567",
		"+44 20 7946 0958":  "+442079460958",
		"  +15551234567  ":  "+15551234567",
		"123":               "+123",
		"":                  "",
		"no digits at all!": "",
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Errorf("normalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"/settings"}, "/settings"},
		{[]string{"/session", "/settings"}, "/se"},
		{[]string{"tiers.main", "tiers.middle", "tiers.subagents"}, "tiers."},
		{[]string{"abc", "xyz"}, ""},
	}
	for _, tc := range cases {
		if got := commonPrefix(tc.in); got != tc.want {
			t.Errorf("commonPrefix(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Scrolling moves the viewport back through the lines and clamps at both
// ends; it does nothing on a screen that has not taken over the terminal.
func TestScreenScrollClamps(t *testing.T) {
	s := &screen{rows: 10, cols: 40, liveStart: -1, out: io.Discard}
	for i := 0; i < 50; i++ {
		s.lines = append(s.lines, "line")
	}
	s.Scroll(-5) // inactive: ignored
	if s.offset != 0 {
		t.Fatalf("scrolled an inactive screen to %d", s.offset)
	}
	s.active = true
	scrollBy(s, "up")
	if s.offset != 1 {
		t.Errorf("up = offset %d, want 1", s.offset)
	}
	scrollBy(s, "pgup")
	if s.offset != 11 {
		t.Errorf("pgup = offset %d, want 11", s.offset)
	}
	s.Scroll(-1000)
	max := len(s.lines) - s.viewHeight()
	if s.offset != max {
		t.Errorf("offset %d past the top, want the max %d", s.offset, max)
	}
	scrollBy(s, "pgdn")
	scrollBy(s, "down")
	if s.offset != max-11 {
		t.Errorf("after pgdn+down offset %d, want %d", s.offset, max-11)
	}
	s.Scroll(1000)
	if s.offset != 0 {
		t.Errorf("offset %d below the bottom, want 0", s.offset)
	}
	if s.Cols() != 40 {
		t.Errorf("Cols = %d", s.Cols())
	}
	// Resize re-measures and repaints without a terminal to ask.
	s.Resize()
	if s.cols <= 0 || s.rows <= 0 {
		t.Errorf("resize left %dx%d", s.cols, s.rows)
	}
}
