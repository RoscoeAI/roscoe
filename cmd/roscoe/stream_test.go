package main

import (
	"io"
	"strings"
	"testing"
)

// An inactive screen still tracks the buffer, so the streaming logic can be
// tested without a terminal. active stays false: nothing is painted.
func bufScreen() *screen {
	s := &screen{rows: 24, cols: 40, liveStart: -1, out: io.Discard}
	s.active = true // buffer bookkeeping runs; every paint is discarded
	return s
}

func TestStreamAppendsInPlace(t *testing.T) {
	s := bufScreen()
	s.Stream("he", "")
	s.Stream("llo", "")
	s.Stream(" world", "")
	if n := len(s.lines); n != 1 {
		t.Fatalf("streaming three chunks made %d lines, want 1", n)
	}
	if !strings.Contains(s.lines[0], "hello world") {
		t.Errorf("line = %q", s.lines[0])
	}
	if !s.Streaming() {
		t.Error("not reported as streaming")
	}
	s.EndStream()
	if s.Streaming() {
		t.Error("still streaming after EndStream")
	}
	s.EndStream() // idempotent
	if len(s.lines) != 1 {
		t.Errorf("EndStream changed the buffer: %d lines", len(s.lines))
	}
}

// A streamed answer must wrap exactly as a printed one would, and keep wrapping
// as it grows rather than running off the edge.
func TestStreamWrapsAsItGrows(t *testing.T) {
	s := bufScreen() // 40 cols
	s.Stream(strings.Repeat("a", 30), "")
	if len(s.lines) != 1 {
		t.Fatalf("30 runes in 40 cols made %d lines", len(s.lines))
	}
	s.Stream(strings.Repeat("b", 30), "")
	if len(s.lines) != 2 {
		t.Fatalf("60 runes in 40 cols made %d lines, want 2", len(s.lines))
	}
	s.EndStream()
	// Same text printed whole wraps to the same number of lines.
	p := bufScreen()
	p.Print(strings.Repeat("a", 30) + strings.Repeat("b", 30))
	if len(p.lines) != len(s.lines) {
		t.Errorf("streamed wrapped to %d lines, printed to %d", len(s.lines), len(p.lines))
	}
}

// A line printed while text is streaming (a tool call, say) must land after
// the streamed text, finishing it, rather than interleaving with it.
func TestPrintDuringStreamFinishesTheLine(t *testing.T) {
	s := bufScreen()
	s.Stream("partial answer", "")
	s.Print("· Read")
	if s.Streaming() {
		t.Error("Print did not end the stream")
	}
	if len(s.lines) != 2 || !strings.Contains(s.lines[0], "partial answer") || !strings.Contains(s.lines[1], "Read") {
		t.Errorf("lines = %q", s.lines)
	}
	// And a new stream after that starts a fresh line, not appended to Read.
	s.Stream("next", "")
	if len(s.lines) != 3 || !strings.Contains(s.lines[2], "next") {
		t.Errorf("lines = %q", s.lines)
	}
}

func TestStreamStyleIsAppliedOnce(t *testing.T) {
	s := bufScreen()
	s.Stream("a", ansiDim)
	s.Stream("b", ansiDim)
	if strings.Count(s.lines[0], ansiDim) != 1 {
		t.Errorf("style repeated per chunk: %q", s.lines[0])
	}
	if !strings.HasSuffix(s.lines[0], ansiReset) {
		t.Errorf("streamed line not reset: %q", s.lines[0])
	}
}
