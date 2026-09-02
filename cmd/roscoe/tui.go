package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// boxRows is how many bottom rows the input box occupies: border, input, border.
const boxRows = 3

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

// screen owns the terminal: a scrollback buffer painted into the region above
// a pinned input box. Output appends to the buffer; the viewport shows the
// tail unless the operator has scrolled back.
type screen struct {
	mu     sync.Mutex
	rows   int
	cols   int
	prompt string
	input  string
	hint   string // completion hint shown after the input
	note   string // one-line help for what is being typed, shown above the box
	cursor int    // insertion point within input, in runes
	active bool

	lines  []string // display lines, pre-wrapped to the terminal width
	offset int      // rows scrolled back; 0 follows new output

	// overlay, when non-nil, is painted over the viewport instead of the
	// scrollback: a panel that survives resizes and prompt redraws without
	// disturbing the conversation underneath.
	overlay []string
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
			if r, err := strconv.Atoi(parts[0]); err == nil && r > 6 {
				s.rows = r
			}
			if c, err := strconv.Atoi(parts[1]); err == nil && c > 20 {
				s.cols = c
			}
		}
	}
}

// boxHeight is the bottom region the prompt owns: the box, plus a row for the
// help note when there is one.
func (s *screen) boxHeight() int {
	if s.note != "" {
		return boxRows + 1
	}
	return boxRows
}

func (s *screen) viewHeight() int {
	if h := s.rows - s.boxHeight(); h > 0 {
		return h
	}
	return 1
}

// Enter takes over the screen.
func (s *screen) Enter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	fmt.Fprint(os.Stdout, "\x1b[2J")
	s.repaintLocked()
}

// Leave restores a normal terminal.
func (s *screen) Leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s\n", s.rows, ansiClrEOL, ansiShow)
}

// Print appends output. Long lines are wrapped here so scroll math matches
// what the terminal shows.
func (s *screen) Print(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		fmt.Fprintln(os.Stdout, line)
		return
	}
	s.lines = append(s.lines, wrapVisible(line, s.cols)...)
	const maxScrollback = 5000
	if len(s.lines) > maxScrollback {
		s.lines = s.lines[len(s.lines)-maxScrollback:]
	}
	if s.offset == 0 { // following the tail
		s.repaintLocked()
	}
}

func (s *screen) Printf(format string, args ...any) {
	s.Print(strings.TrimRight(fmt.Sprintf(format, args...), "\n"))
}

// Scroll moves the viewport; negative delta goes back in time.
func (s *screen) Scroll(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	max := len(s.lines) - s.viewHeight()
	if max < 0 {
		max = 0
	}
	s.offset -= delta
	if s.offset < 0 {
		s.offset = 0
	}
	if s.offset > max {
		s.offset = max
	}
	s.repaintLocked()
}

// SetPrompt updates the input line, its completion hint, and the help note
// above it. A note appearing or disappearing changes the viewport height, so
// that case repaints everything rather than just the box.
func (s *screen) SetPrompt(prompt, input, hint, note string) {
	s.SetPromptCursor(prompt, input, len([]rune(input)), hint, note)
}

// SetPromptCursor is SetPrompt with the cursor somewhere other than the end.
func (s *screen) SetPromptCursor(prompt, input string, cursor int, hint, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resized := (s.note == "") != (note == "")
	s.prompt, s.input, s.hint, s.note = prompt, input, hint, note
	s.cursor = cursor
	if resized {
		s.repaintLocked()
		return
	}
	s.drawBoxLocked()
}

// Overlay paints lines over the viewport until called with nil, which
// restores the conversation. Output arriving underneath is buffered, not lost.
func (s *screen) Overlay(lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlay = lines
	s.repaintLocked()
}

// ViewHeight is how many rows an overlay may use.
func (s *screen) ViewHeight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewHeight()
}

// Cols is the terminal width.
func (s *screen) Cols() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols
}

func (s *screen) repaintLocked() {
	if !s.active {
		return
	}
	h := s.viewHeight()
	view := s.overlay
	if view == nil {
		end := len(s.lines) - s.offset
		if end < 0 {
			end = 0
		}
		start := end - h
		if start < 0 {
			start = 0
		}
		view = s.lines[start:end]
	} else if len(view) > h {
		view = view[:h]
	}

	var b strings.Builder
	b.WriteString(ansiHide)
	for i := 0; i < h; i++ {
		fmt.Fprintf(&b, "\x1b[%d;1H%s", i+1, ansiClrEOL)
		if i < len(view) {
			b.WriteString(view[i])
		}
	}
	fmt.Fprint(os.Stdout, b.String())
	s.drawBoxLocked()
}

// drawBoxLocked paints the three-row input box across the bottom.
func (s *screen) drawBoxLocked() {
	if !s.active {
		return
	}
	inner := s.cols - 2
	if inner < 10 {
		inner = 10
	}
	label := s.prompt
	if s.offset > 0 {
		label = fmt.Sprintf("%d↑ %s", s.offset, s.prompt)
	}

	// Window the input around the cursor when it is wider than the box, so
	// editing the middle of a long line shows the part being edited rather
	// than always the tail.
	in := []rune(s.input)
	cur := s.cursor
	if cur < 0 {
		cur = 0
	}
	if cur > len(in) {
		cur = len(in)
	}
	avail := inner - 2 - len([]rune(label))
	if avail < 1 {
		avail = 1
	}
	ws := inputWindow(len(in), cur, avail)
	we := ws + avail
	if we > len(in) {
		we = len(in)
	}
	shown := string(in[ws:we])
	rest := inner - 1 - len([]rune(label)) - (we - ws)
	if rest < 0 {
		rest = 0
	}
	hint := s.hint
	if ws > 0 || we < len(in) { // a scrolled line has no room for ghost text
		hint = ""
	}
	if len([]rune(hint)) > rest {
		hint = string([]rune(hint)[:rest])
	}
	pad := rest - len([]rune(hint))

	if s.note != "" {
		note := s.note
		if len([]rune(note)) > s.cols-2 {
			note = string([]rune(note)[:s.cols-2])
		}
		fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s %s%s%s",
			ansiHide, s.rows-3, ansiClrEOL, ansiDim, note, ansiReset)
	}
	fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s%s╭%s╮%s",
		ansiHide, s.rows-2, ansiClrEOL, ansiFaint, strings.Repeat("─", inner), ansiReset)
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s│%s %s%s%s%s%s%s%s│%s",
		s.rows-1, ansiClrEOL, ansiFaint, ansiReset,
		ansiGreen+label+ansiReset, shown,
		ansiFaint, hint, ansiReset, strings.Repeat(" ", pad), ansiFaint, ansiReset)
	fmt.Fprintf(os.Stdout, "\x1b[%d;1H%s%s╰%s╯%s",
		s.rows, ansiClrEOL, ansiFaint, strings.Repeat("─", inner), ansiReset)
	// Put the terminal cursor where the insertion point is.
	fmt.Fprintf(os.Stdout, "\x1b[%d;%dH%s", s.rows-1, 3+len([]rune(label))+(cur-ws), ansiShow)
}

// Resize re-measures and repaints; wrapped lines keep their old width, which
// self-corrects as new output arrives.
func (s *screen) Resize() {
	s.measure()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repaintLocked()
}

// Banner prints the chat header.
func (s *screen) Banner(model, harness, dir string) {
	home, _ := os.UserHomeDir()
	shown := dir
	if home != "" && strings.HasPrefix(dir, home) {
		shown = "~" + dir[len(home):]
	}
	s.Print(ansiGreen + ansiBold + "  roscoe" + ansiReset + ansiFaint + "  " + model + " · " + harness + " · " + shown + ansiReset)
}

// wrapVisible splits a styled line into chunks occupying at most width
// columns, counting printable runes only so ANSI sequences do not consume
// the budget.
func wrapVisible(line string, width int) []string {
	if width < 8 {
		width = 8
	}
	var (
		out     []string
		cur     strings.Builder
		visible int
		inEsc   bool
	)
	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
		visible = 0
	}
	for _, r := range line {
		if inEsc {
			cur.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			cur.WriteRune(r)
			inEsc = true
			continue
		}
		if visible == width {
			flush()
		}
		cur.WriteRune(r)
		visible++
	}
	flush()
	return out
}

// inputWindow picks the first visible rune of a line n runes long so that the
// cursor at cur fits within avail columns. A line that fits starts at 0; a
// longer one scrolls so the cursor is visible, preferring to keep the window
// as far left as the cursor allows.
func inputWindow(n, cur, avail int) int {
	if avail <= 0 || n <= avail {
		return 0
	}
	ws := cur - avail + 1
	if ws < 0 {
		ws = 0
	}
	if ws > n-avail {
		ws = n - avail
	}
	return ws
}
