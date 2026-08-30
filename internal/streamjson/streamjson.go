// Package streamjson parses Claude Code `--output-format stream-json --verbose`
// output: newline-delimited JSON events on stdout.
package streamjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// maxLine is the largest NDJSON line accepted; assistant events carrying big
// tool results can run to megabytes.
const maxLine = 10 * 1024 * 1024

// Event is one stream-json line. Unknown event types are passed through
// untouched: Type/Subtype hold whatever the line declared (possibly ""), and
// Raw always keeps the full line.
type Event struct {
	Type    string          // "system" | "assistant" | "user" | "result"
	Subtype string          // "init", "api_retry", "success", ...
	Raw     json.RawMessage // full line, always kept
}

// Scanner reads events from an NDJSON stream, skipping blank lines.
type Scanner struct {
	sc *bufio.Scanner
}

// NewScanner wraps r in a line scanner accepting lines up to 10MB.
func NewScanner(r io.Reader) *Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	return &Scanner{sc: sc}
}

// Next returns the next event, or io.EOF at end of stream. Blank lines are
// skipped; a non-JSON line is returned as a generic Event (empty Type) rather
// than an error, so one garbled line never kills the stream.
func (s *Scanner) Next() (*Event, error) {
	for s.sc.Scan() {
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// Scanner reuses its buffer; copy before handing the line out.
		raw := make(json.RawMessage, len(line))
		copy(raw, line)

		var hdr struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		// Best effort: on unmarshal failure Type/Subtype stay "".
		_ = json.Unmarshal(raw, &hdr)
		return &Event{Type: hdr.Type, Subtype: hdr.Subtype, Raw: raw}, nil
	}
	if err := s.sc.Err(); err != nil {
		return nil, fmt.Errorf("streamjson: scan: %w", err)
	}
	return nil, io.EOF
}

// ResultEvent is the terminal "result" event of a claude -p run.
type ResultEvent struct {
	Result            string          `json:"result"`
	SessionID         string          `json:"session_id"`
	TotalCostUSD      float64         `json:"total_cost_usd"`
	IsError           bool            `json:"is_error"`
	Usage             json.RawMessage `json:"usage"`
	PermissionDenials json.RawMessage `json:"permission_denials"`
}

// AsResult decodes e as a ResultEvent when Type == "result".
func (e *Event) AsResult() (*ResultEvent, bool) {
	if e == nil || e.Type != "result" {
		return nil, false
	}
	var re ResultEvent
	if err := json.Unmarshal(e.Raw, &re); err != nil {
		return nil, false
	}
	return &re, true
}

// InitEvent is the "system"/"init" event announcing the session.
type InitEvent struct {
	SessionID    string   `json:"session_id"`
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`
	Capabilities []string `json:"capabilities"`
}

// AsInit decodes e as an InitEvent when Type == "system" and Subtype == "init".
func (e *Event) AsInit() (*InitEvent, bool) {
	if e == nil || e.Type != "system" || e.Subtype != "init" {
		return nil, false
	}
	var ie InitEvent
	if err := json.Unmarshal(e.Raw, &ie); err != nil {
		return nil, false
	}
	return &ie, true
}
