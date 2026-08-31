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
// boxRows is how many bottom rows the input box occupies.
const boxRows = 3

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
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
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
	// Scroll region = everything above the input box; park the cursor there.
	fmt.Fprintf(os.Stdout, "\x1b[1;%dr\x1b[%d;1H", s.rows-boxRows, s.rows-boxRows)
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
	for i := 0; i < boxRows; i++ {
		fmt.Fprint(os.Stdout, ansiClrEOL+"\n")
	}
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
	fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s\n", ansiHide, s.rows-boxRows, ansiClrEOL+line)
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

// drawPromptLocked paints the three-row input box across the bottom.
func (s *screen) drawPromptLocked() {
	if !s.active {
		return
	}
	inner := s.cols - 2
	if inner < 10 {
		inner = 10
	}
	top := "╭" + strings.Repeat("─", inner) + "╮"
	bottom := "╰" + strings.Repeat("─", inner) + "╯"

	// The visible input scrolls with the cursor when it outgrows the box.
	body := s.prompt + s.input
	visible := body
	if len([]rune(visible)) > inner-2 {
		r := []rune(visible)
		visible = string(r[len(r)-(inner-2):])
	}
	pad := inner - 1 - len([]rune(visible))
	if pad < 0 {
		pad = 0
	}

	fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s%s%s%s",
		ansiHide, s.rows-2, ansiClrEOL, ansiFaint, top, ansiReset)
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s│%s %s%s%s%s│%s",
		s.rows-1, ansiClrEOL, ansiFaint, ansiReset,
		ansiGreen+s.prompt+ansiReset, s.input, strings.Repeat(" ", pad), ansiFaint, ansiReset)
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s%s%s",
		s.rows, ansiClrEOL, ansiFaint, bottom, ansiReset)
	// Park the cursor just after the typed text, inside the box.
	fmt.Fprintf(os.Stdout, "\x1b[%d;%dH%s", s.rows-1, 3+len([]rune(body)), ansiShow)
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
	home, _ := os.UserHomeDir()
	shown := dir
	if home != "" && strings.HasPrefix(dir, home) {
		shown = "~" + dir[len(home):]
	}
	s.Print("")
	s.Print(ansiGreen + ansiBold + "  roscoe" + ansiReset + ansiFaint + "  " + model + " · " + harness + " · " + shown + ansiReset)
}
