package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// boxRows is how many bottom rows a single-line input box occupies: border,
// input, border. The box grows by one row per extra input line, up to
// maxInputRows, then windows vertically around the cursor's line.
const boxRows = 3

const maxInputRows = 8

// bracketedPasteOn asks the terminal to wrap pasted text in ESC[200~ and
// ESC[201~, so a pasted stack trace's newlines insert instead of submitting
// its first line early. bracketedPasteOff restores the default on exit; a
// terminal left in paste mode confuses the next program.
const (
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
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
func (s *screen) inputRows() int {
	n := strings.Count(s.input, "\n") + 1
	if n > maxInputRows {
		n = maxInputRows
	}
	return n
}

func (s *screen) boxHeight() int {
	h := boxRows - 1 + s.inputRows()
	if s.note != "" {
		h++
	}
	return h
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
	fmt.Fprint(os.Stdout, "\x1b[2J"+bracketedPasteOn)
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
	fmt.Fprintf(os.Stdout, "%s\x1b[%d;1H%s%s\n", bracketedPasteOff, s.rows, ansiClrEOL, ansiShow)
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
	before := s.boxHeight()
	s.prompt, s.input, s.hint, s.note = prompt, input, hint, note
	s.cursor = cursor
	resized := s.boxHeight() != before
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

// drawBoxLocked paints the input box across the bottom: a border, one row per
// input line (windowed around the cursor's line past maxInputRows), a border.
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
	lw := len([]rune(label))
	lines := strings.Split(s.input, "\n")
	curLine, curCol := cursorLineCol(s.input, s.cursor)

	rowsShown := s.inputRows()
	vstart := inputWindow(len(lines), curLine, rowsShown)
	avail := inner - 2 - lw
	if avail < 1 {
		avail = 1
	}
	// Horizontal window follows the cursor on its own line; other lines are
	// simply cut to fit.
	ws := inputWindow(len([]rune(lines[curLine])), curCol, avail)

	top := s.rows - rowsShown - 1
	var b strings.Builder
	b.WriteString(ansiHide)
	if s.note != "" {
		note := s.note
		if len([]rune(note)) > s.cols-2 {
			note = string([]rune(note)[:s.cols-2])
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s %s%s%s", top-1, ansiClrEOL, ansiDim, note, ansiReset)
	}
	fmt.Fprintf(&b, "\x1b[%d;1H%s%s╭%s╮%s", top, ansiClrEOL, ansiFaint, strings.Repeat("─", inner), ansiReset)

	for j := 0; j < rowsShown; j++ {
		li := vstart + j
		row := top + 1 + j
		text := []rune(strings.ReplaceAll(lines[li], "\t", "    "))
		start := 0
		if li == curLine {
			start = ws
		}
		end := start + avail
		if end > len(text) {
			end = len(text)
		}
		if start > len(text) {
			start = len(text)
		}
		shown := string(text[start:end])
		lead := ansiGreen + label + ansiReset
		if j > 0 || vstart > 0 {
			lead = strings.Repeat(" ", lw) // continuation rows align under the text
		}
		used := lw + (end - start)
		rest := inner - 1 - used
		if rest < 0 {
			rest = 0
		}
		hint := ""
		if len(lines) == 1 && ws == 0 && end == len(text) { // ghost text only fits a single, unscrolled line
			hint = s.hint
			if len([]rune(hint)) > rest {
				hint = string([]rune(hint)[:rest])
			}
		}
		pad := rest - len([]rune(hint))
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s│%s %s%s%s%s%s%s%s│%s",
			row, ansiClrEOL, ansiFaint, ansiReset,
			lead, shown, ansiFaint, hint, ansiReset, strings.Repeat(" ", pad), ansiFaint, ansiReset)
	}
	fmt.Fprintf(&b, "\x1b[%d;1H%s%s╰%s╯%s", s.rows, ansiClrEOL, ansiFaint, strings.Repeat("─", inner), ansiReset)
	// Put the terminal cursor at the insertion point.
	fmt.Fprintf(&b, "\x1b[%d;%dH%s", top+1+(curLine-vstart), 3+lw+(curCol-ws), ansiShow)
	fmt.Fprint(os.Stdout, b.String())
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

// cursorLineCol maps a rune offset into a multi-line string to a line index
// and rune column, clamping an out-of-range offset to the end.
func cursorLineCol(text string, cursor int) (line, col int) {
	r := []rune(text)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(r) {
		cursor = len(r)
	}
	for i := 0; i < cursor; i++ {
		if r[i] == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}
	return line, col
}
