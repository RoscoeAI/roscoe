package main

import "unicode"

// lineEditor is a single line with a cursor. Both the prompt and the
// during-turn box use it, so left and right, home and end, word jumps and
// the readline kill keys behave identically everywhere you can type.
//
// Before this the buffer was append-only with backspace at the end, and the
// arrow keys were decoded and then dropped. Retyping a long prompt to fix one
// word is the most common annoyance a line editor exists to remove.
type lineEditor struct {
	buf []rune
	cur int // insertion point, 0..len(buf)
}

func (e *lineEditor) String() string { return string(e.buf) }
func (e *lineEditor) Cursor() int    { return e.cur }
func (e *lineEditor) Empty() bool    { return len(e.buf) == 0 }

// Set replaces the line and puts the cursor at the end, which is what history
// recall and tab completion want.
func (e *lineEditor) Set(s string) {
	e.buf = []rune(s)
	e.cur = len(e.buf)
}

func (e *lineEditor) Clear() { e.buf, e.cur = nil, 0 }

func (e *lineEditor) Insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

// Backspace deletes the rune before the cursor.
func (e *lineEditor) Backspace() bool {
	if e.cur == 0 {
		return false
	}
	e.buf = append(e.buf[:e.cur-1], e.buf[e.cur:]...)
	e.cur--
	return true
}

// Delete removes the rune under the cursor.
func (e *lineEditor) Delete() bool {
	if e.cur >= len(e.buf) {
		return false
	}
	e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
	return true
}

func (e *lineEditor) Left() {
	if e.cur > 0 {
		e.cur--
	}
}

func (e *lineEditor) Right() {
	if e.cur < len(e.buf) {
		e.cur++
	}
}

func (e *lineEditor) Home() { e.cur = 0 }
func (e *lineEditor) End()  { e.cur = len(e.buf) }

// wordStart is the index where the word before pos begins: skip spaces
// backwards, then letters, the way readline's backward-word does.
func (e *lineEditor) wordStart(pos int) int {
	for pos > 0 && unicode.IsSpace(e.buf[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(e.buf[pos-1]) {
		pos--
	}
	return pos
}

// wordEnd is the index just past the word after pos.
func (e *lineEditor) wordEnd(pos int) int {
	n := len(e.buf)
	for pos < n && unicode.IsSpace(e.buf[pos]) {
		pos++
	}
	for pos < n && !unicode.IsSpace(e.buf[pos]) {
		pos++
	}
	return pos
}

func (e *lineEditor) WordLeft()  { e.cur = e.wordStart(e.cur) }
func (e *lineEditor) WordRight() { e.cur = e.wordEnd(e.cur) }

// KillWordBack is ctrl-w / alt-backspace: delete the word before the cursor.
func (e *lineEditor) KillWordBack() bool {
	start := e.wordStart(e.cur)
	if start == e.cur {
		return false
	}
	e.buf = append(e.buf[:start], e.buf[e.cur:]...)
	e.cur = start
	return true
}

// KillToEnd is ctrl-k.
func (e *lineEditor) KillToEnd() bool {
	if e.cur >= len(e.buf) {
		return false
	}
	e.buf = e.buf[:e.cur]
	return true
}

// KillToStart is ctrl-u.
func (e *lineEditor) KillToStart() bool {
	if e.cur == 0 {
		return false
	}
	e.buf = append([]rune(nil), e.buf[e.cur:]...)
	e.cur = 0
	return true
}

// applyEditKey performs a named key on the editor and reports whether it was
// an editing key at all. Callers handle enter, tab, esc and history first;
// everything that only moves or mutates the line lands here, so the prompt
// and the during-turn box cannot drift apart in what they accept.
func (e *lineEditor) applyEditKey(key string) bool {
	switch key {
	case "left":
		e.Left()
	case "right":
		e.Right()
	case "home", "\x01":
		e.Home()
	case "end", "\x05":
		e.End()
	case "word-left":
		e.WordLeft()
	case "word-right":
		e.WordRight()
	case "delete":
		e.Delete()
	case "\x7f", "\b":
		e.Backspace()
	case "kill-word", "\x17":
		e.KillWordBack()
	case "\x0b":
		e.KillToEnd()
	case "\x15":
		e.KillToStart()
	default:
		if r := []rune(key); len(r) == 1 && r[0] >= 0x20 && r[0] != 0x7f {
			e.Insert(r[0])
			return true
		}
		return false
	}
	return true
}
