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

// ReadLineOn collects a line into the pinned prompt of a screen. Enter
// submits; up/down scroll the conversation; tab completes a slash command;
// Esc or Ctrl+C abandon the line (ok=false). history is the previous inputs,
// oldest first, walked with up/down once the line is empty.
func (k *keyReader) ReadLineOn(sc *screen, promptStr string, history []string) (string, bool) {
	var b []byte
	hist := len(history) // index into history; len == "current, unsaved line"
	redraw := func() { sc.SetPrompt(promptStr, string(b), completionHint(string(b))) }
	redraw()

	for c := range k.events {
		switch {
		case c == '\r' || c == '\n':
			line := string(b)
			sc.SetPrompt(promptStr, "", "")
			return line, true

		case c == 0x7f || c == 0x08:
			if len(b) > 0 {
				b = b[:len(b)-1]
			}

		case c == '\t':
			if done := completeCommand(string(b)); done != "" {
				b = []byte(done)
			}

		case c == 0x1b:
			switch k.escapeKey() {
			case "esc":
				return "", false
			case "up":
				if len(b) == 0 && hist > 0 { // recall a previous prompt
					hist--
					b = []byte(history[hist])
				} else {
					sc.Scroll(-1)
				}
			case "down":
				if len(b) == 0 || hist < len(history) {
					if hist < len(history)-1 {
						hist++
						b = []byte(history[hist])
					} else if hist == len(history)-1 {
						hist++
						b = nil
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
				continue
			}

		case c == 0x03:
			return "", false

		case c >= 0x20 && c < 0x7f:
			b = append(b, c)

		default:
			continue
		}
		redraw()
	}
	return "", false
}

// chatCommands is the slash-command surface, used for hints and completion.
var chatCommands = []string{
	"/autonomy", "/config", "/cost", "/exit", "/harness",
	"/help", "/model", "/new", "/session", "/subagents",
}

// completionHint suggests the rest of a slash command as ghost text, or lists
// the commands once "/" is typed.
func completionHint(input string) string {
	if input == "" || !strings.HasPrefix(input, "/") || strings.Contains(input, " ") {
		return ""
	}
	if input == "/" {
		return strings.Join(trimPrefixes(chatCommands), " ")
	}
	var matches []string
	for _, c := range chatCommands {
		if strings.HasPrefix(c, input) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return ""
	case 1:
		return matches[0][len(input):] + "  ⇥"
	default:
		return "  " + strings.Join(matches, " ")
	}
}

// completeCommand returns the completed command for a tab press, or "" when
// there is nothing unambiguous to complete.
func completeCommand(input string) string {
	if !strings.HasPrefix(input, "/") || strings.Contains(input, " ") {
		return ""
	}
	var matches []string
	for _, c := range chatCommands {
		if strings.HasPrefix(c, input) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		return matches[0] + " "
	}
	return ""
}

func trimPrefixes(cmds []string) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, strings.TrimPrefix(c, "/"))
	}
	return out
}

// escapeKey resolves the Esc just read into a named key. A bare Esc — nothing
// following within a beat — returns "esc"; recognised sequences return "up",
// "down", "pgup", "pgdn", "left", "right", "home", "end"; anything else
// returns "" after being swallowed.
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
			if f >= 0x40 && f <= 0x7e { // final byte
				switch f {
				case 'A':
					return "up"
				case 'B':
					return "down"
				case 'C':
					return "right"
				case 'D':
					return "left"
				case 'H':
					return "home"
				case 'F':
					return "end"
				case '~':
					switch string(seq) {
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
