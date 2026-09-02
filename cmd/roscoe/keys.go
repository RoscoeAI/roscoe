package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// keyReader gives roscoe run its interactive controls: Esc interrupts the
// running worker, then a redirect line is read with manual echo (the tty's
// canonical mode and echo are switched off via stty for the duration).
type keyReader struct {
	events chan byte
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func stty(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// newKeyReader switches the tty to byte-at-a-time input and starts the
// single goroutine that owns stdin. The returned restore func reinstates
// canonical mode and echo; call it on every exit path.
func newKeyReader() (*keyReader, func(), error) {
	if err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return nil, nil, fmt.Errorf("stty: %w", err)
	}
	restore := func() { _ = stty("icanon", "echo") }
	k := &keyReader{events: make(chan byte, 64)}
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(k.events)
				return
			}
			if n == 1 {
				select {
				case k.events <- buf[0]:
				default: // never block the reader on a full buffer
				}
			}
		}
	}()
	return k, restore, nil
}

// WaitEsc blocks until Esc is pressed (true) or ctx ends (false). Other
// keys pressed while a task runs are discarded.
func (k *keyReader) WaitEsc(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case b, ok := <-k.events:
			if !ok {
				return false
			}
			if b == 0x1b {
				return true
			}
		}
	}
}

// ReadLine collects a redirect line with manual echo. Enter submits;
// backspace edits; Esc or Ctrl+C abort (ok=false).
func (k *keyReader) ReadLine(promptStr string) (string, bool) {
	fmt.Fprint(os.Stderr, promptStr)
	var b []byte
	for c := range k.events {
		switch {
		case c == '\r' || c == '\n':
			fmt.Fprintln(os.Stderr)
			return string(b), true
		case c == 0x7f || c == 0x08:
			if len(b) > 0 {
				b = b[:len(b)-1]
				fmt.Fprint(os.Stderr, "\b \b")
			}
		case c == 0x1b || c == 0x03:
			fmt.Fprintln(os.Stderr)
			return "", false
		default:
			b = append(b, c)
			fmt.Fprintf(os.Stderr, "%c", c)
		}
	}
	return "", false
}

// ReadLineOn collects a line into the pinned prompt of a screen, starting
// from initial (empty for a fresh line). Enter submits; up/down scroll the
// conversation; tab completes a slash command; Esc or Ctrl+C abandon the line
// (ok=false). history is the previous inputs, oldest first, walked with
// up/down once the line is empty.
func (k *keyReader) ReadLineOn(sc *screen, promptStr, initial string, history []string, comp *completer) (string, bool) {
	var ed lineEditor
	ed.Set(initial)
	hist := len(history) // index into history; len == "current, unsaved line"
	redraw := func() {
		line := ed.String()
		sc.SetPromptCursor(promptStr, line, ed.Cursor(), comp.hintFor(line), comp.noteFor(line))
	}
	redraw()

	for c := range k.events {
		switch {
		case c == '\r' || c == '\n':
			line := ed.String()
			sc.SetPrompt(promptStr, "", "", "")
			return line, true

		case c == '\t':
			if done := comp.completeOn(ed.String()); done != "" {
				ed.Set(done)
			}

		case c == 0x1b:
			key := k.escapeKey()
			switch key {
			case "esc":
				return "", false
			case "up":
				if ed.Empty() && hist > 0 { // recall a previous prompt
					hist--
					ed.Set(history[hist])
				} else {
					sc.Scroll(-1)
				}
			case "down":
				if ed.Empty() || hist < len(history) {
					if hist < len(history)-1 {
						hist++
						ed.Set(history[hist])
					} else if hist == len(history)-1 {
						hist++
						ed.Clear()
					} else {
						sc.Scroll(1)
					}
				} else {
					sc.Scroll(1)
				}
			case "pgup":
				sc.Scroll(-10)
			case "pgdn":
				sc.Scroll(10)
			default:
				if !ed.applyEditKey(key) {
					continue
				}
			}

		case c == 0x03:
			return "", false

		default:
			if !ed.applyEditKey(string(rune(c))) {
				continue
			}
		}
		redraw()
	}
	return "", false
}

// NextKey blocks for one keypress and names it: "up", "down", "left",
// "right", "pgup", "pgdn", "enter", "esc", "tab", "ctrl-c", "eof", or the
// character itself. Unrecognised escape sequences come back as "".
func (k *keyReader) NextKey() string {
	b, ok := <-k.events
	if !ok {
		return "eof"
	}
	switch {
	case b == 0x1b:
		return k.escapeKey()
	case b == '\r' || b == '\n':
		return "enter"
	case b == '\t':
		return "tab"
	case b == 0x03:
		return "ctrl-c"
	default:
		return string(rune(b))
	}
}

// completer supplies candidate completions for the token being typed.
// Chat owns the knowledge (commands, config paths, provider values); the
// line editor only renders and applies them.
type completer struct {
	candidates func(input string) []string
	// note returns one line of help for what is being typed, shown above the
	// input box. Optional.
	note func(input string) string
	// descends reports that a candidate has a level below it, so completing
	// onto it should open that level rather than end the token.
	descends func(candidate string) bool
}

// suggestions returns the candidates for the current token, plus that token.
func (c *completer) suggestions(input string) ([]string, string) {
	if c == nil || c.candidates == nil {
		return nil, ""
	}
	token := currentToken(input)
	return c.candidates(input), token
}

// currentToken is the word being typed: empty when the input ends in a space
// (a new argument is starting).
func currentToken(input string) string {
	if input == "" || strings.HasSuffix(input, " ") {
		return ""
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// hintFor renders ghost text after the cursor: the completion of a single
// match, or a sample of the alternatives.
func (c *completer) hintFor(input string) string {
	cands, token := c.suggestions(input)
	if len(cands) == 0 {
		return ""
	}
	if len(cands) == 1 {
		// Just the completion. A trailing tab glyph reads as part of what you
		// typed, which is worse than not advertising the key at all.
		tail := ""
		if c.descends != nil && c.descends(cands[0]) {
			tail = "."
		}
		return strings.TrimPrefix(cands[0], token) + tail
	}
	// Show candidates relative to the level being walked: under
	// "tiers.middle." the choices read "model provider effort", not the
	// whole dotted path repeated for each.
	parent := ""
	if i := strings.LastIndex(token, "."); i >= 0 {
		parent = token[:i+1]
	}
	shown := make([]string, 0, len(cands))
	for _, c := range cands {
		shown = append(shown, strings.TrimPrefix(c, parent))
	}
	const max = 6
	more := ""
	if len(shown) > max {
		more = fmt.Sprintf(" +%d", len(shown)-max)
		shown = shown[:max]
	}
	return "  " + strings.Join(shown, " ") + more
}

// completeOn applies a tab press: the single match, or the longest common
// prefix shared by all matches.
func (c *completer) completeOn(input string) string {
	cands, token := c.suggestions(input)
	if len(cands) == 0 {
		return ""
	}
	replacement := cands[0]
	if len(cands) > 1 {
		replacement = commonPrefix(cands)
		if replacement == token {
			return ""
		}
	}
	base := strings.TrimSuffix(input, token)
	out := base + replacement
	if len(cands) == 1 {
		if c.descends != nil && c.descends(replacement) {
			out += "." // open the next level instead of ending the token
		} else {
			out += " "
		}
	}
	return out
}

// noteFor is the help line for the current input, or "".
func (c *completer) noteFor(input string) string {
	if c == nil || c.note == nil {
		return ""
	}
	return c.note(input)
}

func commonPrefix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	prefix := items[0]
	for _, it := range items[1:] {
		for !strings.HasPrefix(it, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}

// escapeKey resolves the Esc just read into a named key. A bare Esc — nothing
// following within a beat — returns "esc"; recognised sequences return "up",
// "down", "pgup", "pgdn", "left", "right"; anything else returns "" after
// being swallowed, so stray sequences never reach the line.
func (k *keyReader) escapeKey() string {
	var intro byte
	select {
	case c, ok := <-k.events:
		if !ok {
			return "esc"
		}
		intro = c
	case <-time.After(50 * time.Millisecond):
		return "esc"
	}
	switch intro {
	case 'b':
		return "word-left" // alt-b
	case 'f':
		return "word-right" // alt-f
	case 0x7f, 0x08:
		return "kill-word" // alt-backspace
	}
	if intro != '[' && intro != 'O' {
		return ""
	}
	var seq []byte
	for {
		select {
		case f, ok := <-k.events:
			if !ok {
				return ""
			}
			if f >= 0x40 && f <= 0x7e { // final byte of the sequence
				// A ";5" or ";3" parameter is ctrl or alt held down.
				mod := strings.Contains(string(seq), ";5") || strings.Contains(string(seq), ";3")
				switch f {
				case 'A':
					return "up"
				case 'B':
					return "down"
				case 'C':
					if mod {
						return "word-right"
					}
					return "right"
				case 'D':
					if mod {
						return "word-left"
					}
					return "left"
				case 'H':
					return "home"
				case 'F':
					return "end"
				case '~':
					switch string(seq) {
					case "1", "7":
						return "home"
					case "4", "8":
						return "end"
					case "3":
						return "delete"
					case "5":
						return "pgup"
					case "6":
						return "pgdn"
					}
				}
				return ""
			}
			seq = append(seq, f)
		case <-time.After(50 * time.Millisecond):
			return ""
		}
	}
}
