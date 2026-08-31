package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// ANSI palette, matched to roscoe.sh: phosphor green accent on the terminal's
// own ground, dim parchment for secondary text.
const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[38;5;114m"
	ansiDim    = "\x1b[38;5;245m"
	ansiBold   = "\x1b[1m"
	ansiFaint  = "\x1b[2m"
	ansiHide   = "\x1b[?25l"
	ansiShow   = "\x1b[?25h"
	ansiClrEOL = "\x1b[K"
)

// screen pins a prompt line to the bottom row and scrolls output above it,
// using a DECSTBM scroll region. Everything roscoe prints goes through
// Print/Printf so the prompt is redrawn afterwards.
type screen struct {
	mu     sync.Mutex
	rows   int
	cols   int
	prompt string
	input  string
	active bool
}

func newScreen() *screen {
	s := &screen{}
	s.measure()
	return s
}

func (s *screen) measure() {
	s.rows, s.cols = 24, 80
	out, err := exec.Command("stty", "size").Output()
	if err == nil {
		if parts := strings.Fields(string(out)); len(parts) == 2 {
			if r, err := strconv.Atoi(parts[0]); err == nil && r > 4 {
				s.rows = r
			}
			if c, err := strconv.Atoi(parts[1]); err == nil && c > 20 {
				s.cols = c
			}
		}
	}
}

// Enter reserves the bottom row for the prompt.
func (s *screen) Enter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	// Scroll region = rows 1..rows-1, then park the cursor inside it.
	fmt.Fprintf(os.Stdout, "\x1b[1;%dr\x1b[%d;1H", s.rows-1, s.rows-1)
}

// Leave restores the full-screen scroll region.
func (s *screen) Leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	fmt.Fprintf(os.Stdout, "\x1b[r\x1b[%d;1H%s%s\n", s.rows, ansiClrEOL, ansiShow)
}

// Print writes a line into the scrolling region above the prompt.
func (s *screen) Print(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		fmt.Fprintln(os.Stdout, line)
		return
	}
	// Park at the last scrolling row, emit the line (which scrolls the region),
	// then repaint the pinned prompt.
	fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s\n", ansiHide, s.rows-1, ansiClrEOL+line)
	s.drawPromptLocked()
}

func (s *screen) Printf(format string, args ...any) {
	s.Print(strings.TrimRight(fmt.Sprintf(format, args...), "\n"))
}

// SetPrompt changes the label and current input, then repaints.
func (s *screen) SetPrompt(prompt, input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompt, s.input = prompt, input
	s.drawPromptLocked()
}

func (s *screen) drawPromptLocked() {
	if !s.active {
		return
	}
	line := ansiGreen + s.prompt + ansiReset + s.input
	// Move to the pinned row, clear it, draw, leave the cursor after the input.
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s%s", s.rows, ansiClrEOL, line, ansiShow)
}

// Resize re-measures the terminal and re-establishes the region.
func (s *screen) Resize() {
	s.mu.Lock()
	wasActive := s.active
	s.mu.Unlock()
	s.measure()
	if wasActive {
		s.Enter()
		s.SetPrompt(s.prompt, s.input)
	}
}

// Banner prints the chat header in the site's voice.
func (s *screen) Banner(model, harness, dir string) {
	s.Print(ansiGreen + ansiBold + "roscoe" + ansiReset + ansiDim + "  " + model + " · " + harness + " · " + dir + ansiReset)
	s.Print(ansiFaint + "enter to send · esc interrupts a turn · /help for commands" + ansiReset)
}
