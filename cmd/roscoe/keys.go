package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// ReadLineOn collects a line into the pinned prompt of a screen, repainting
// as the operator types. Enter submits; Esc or Ctrl+C abort (ok=false).
func (k *keyReader) ReadLineOn(sc *screen, promptStr string) (string, bool) {
	var b []byte
	sc.SetPrompt(promptStr, "")
	for c := range k.events {
		switch {
		case c == '\r' || c == '\n':
			line := string(b)
			sc.SetPrompt(promptStr, "")
			return line, true
		case c == 0x7f || c == 0x08:
			if len(b) > 0 {
				b = b[:len(b)-1]
			}
		case c == 0x1b:
			// Arrow keys and friends arrive as ESC [ … ; only a bare Esc means
			// "abandon this line".
			if k.consumeEscapeSequence() {
				continue
			}
			return "", false
		case c == 0x03:
			return "", false
		case c >= 0x20 && c < 0x7f:
			b = append(b, c)
		default:
			continue
		}
		sc.SetPrompt(promptStr, string(b))
	}
	return "", false
}

// consumeEscapeSequence reports whether the Esc just read began a terminal
// escape sequence (arrow keys, home/end, mouse), swallowing the rest of it.
// A bare Esc — nothing following within a beat — returns false.
func (k *keyReader) consumeEscapeSequence() bool {
	select {
	case c, ok := <-k.events:
		if !ok {
			return false
		}
		if c != '[' && c != 'O' {
			return false
		}
		for {
			select {
			case f, ok := <-k.events:
				if !ok {
					return true
				}
				if f >= 0x40 && f <= 0x7e { // final byte of a CSI sequence
					return true
				}
			case <-time.After(50 * time.Millisecond):
				return true
			}
		}
	case <-time.After(50 * time.Millisecond):
		return false
	}
}
