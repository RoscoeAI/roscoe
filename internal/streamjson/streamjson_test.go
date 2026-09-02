package streamjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func mustNext(t *testing.T, s *Scanner) *Event {
	t.Helper()
	ev, err := s.Next()
	if err != nil {
		t.Fatalf("Next: unexpected error: %v", err)
	}
	return ev
}

func TestNextParsesEventHeaders(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantType    string
		wantSubtype string
	}{
		{
			name:        "system init",
			line:        `{"type":"system","subtype":"init","session_id":"s-1"}`,
			wantType:    "system",
			wantSubtype: "init",
		},
		{
			name:     "assistant",
			line:     `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
			wantType: "assistant",
		},
		{
			name:        "result",
			line:        `{"type":"result","subtype":"success","result":"done"}`,
			wantType:    "result",
			wantSubtype: "success",
		},
		{
			name:     "user",
			line:     `{"type":"user"}`,
			wantType: "user",
		},
		{
			name:        "unknown type passes through",
			line:        `{"type":"totally_new_event","subtype":"v2","payload":{"x":1}}`,
			wantType:    "totally_new_event",
			wantSubtype: "v2",
		},
		{
			name:     "no type field",
			line:     `{"foo":"bar"}`,
			wantType: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScanner(strings.NewReader(tt.line + "\n"))
			ev := mustNext(t, s)
			if ev.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tt.wantType)
			}
			if ev.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", ev.Subtype, tt.wantSubtype)
			}
			if string(ev.Raw) != tt.line {
				t.Errorf("Raw = %q, want %q", ev.Raw, tt.line)
			}
			if _, err := s.Next(); err != io.EOF {
				t.Errorf("second Next err = %v, want io.EOF", err)
			}
		})
	}
}

func TestNextMalformedLineNoError(t *testing.T) {
	line := `this is {{{ not json`
	s := NewScanner(strings.NewReader(line + "\n" + `{"type":"assistant"}` + "\n"))

	ev := mustNext(t, s)
	if ev.Type != "" || ev.Subtype != "" {
		t.Errorf("malformed line: Type=%q Subtype=%q, want both empty", ev.Type, ev.Subtype)
	}
	if string(ev.Raw) != line {
		t.Errorf("malformed line Raw = %q, want %q", ev.Raw, line)
	}

	// The stream survives a garbled line.
	ev2 := mustNext(t, s)
	if ev2.Type != "assistant" {
		t.Errorf("after malformed line, Type = %q, want %q", ev2.Type, "assistant")
	}
}

func TestNextSkipsBlankLines(t *testing.T) {
	input := "\n\n   \n\t\n" + `{"type":"assistant"}` + "\n \n\n"
	s := NewScanner(strings.NewReader(input))

	ev := mustNext(t, s)
	if ev.Type != "assistant" {
		t.Fatalf("Type = %q, want %q", ev.Type, "assistant")
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("after blanks, err = %v, want io.EOF", err)
	}
}

func TestNextTrimsSurroundingWhitespace(t *testing.T) {
	line := `{"type":"assistant"}`
	s := NewScanner(strings.NewReader("   " + line + "  \r\n"))
	ev := mustNext(t, s)
	if string(ev.Raw) != line {
		t.Errorf("Raw = %q, want trimmed %q", ev.Raw, line)
	}
}

func TestNextEOFOnEmptyInput(t *testing.T) {
	s := NewScanner(strings.NewReader(""))
	ev, err := s.Next()
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if ev != nil {
		t.Fatalf("ev = %+v, want nil", ev)
	}
	// io.EOF is sticky.
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("repeat Next err = %v, want io.EOF", err)
	}
}

func TestNextCopiesRawOutOfScannerBuffer(t *testing.T) {
	// bufio.Scanner reuses its internal buffer between Scan calls; Next must
	// hand out a copy or an earlier event's Raw gets clobbered.
	line1 := `{"type":"system","subtype":"init"}`
	line2 := `{"type":"assistant","message":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`
	s := NewScanner(strings.NewReader(line1 + "\n" + line2 + "\n"))

	ev1 := mustNext(t, s)
	_ = mustNext(t, s)

	if string(ev1.Raw) != line1 {
		t.Errorf("first event Raw corrupted after second Scan: %q", ev1.Raw)
	}
}

func TestNextNearMaxLineOK(t *testing.T) {
	// A line just under the 10MB cap must parse.
	pad := strings.Repeat("a", maxLine-1024)
	line := `{"type":"assistant","pad":"` + pad + `"}`
	if len(line) >= maxLine {
		t.Fatalf("test bug: line length %d >= maxLine %d", len(line), maxLine)
	}
	s := NewScanner(strings.NewReader(line + "\n"))
	ev := mustNext(t, s)
	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want %q", ev.Type, "assistant")
	}
	if len(ev.Raw) != len(line) {
		t.Errorf("Raw length = %d, want %d", len(ev.Raw), len(line))
	}
}

func TestNextOverMaxLineErrTooLong(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"type":"assistant","pad":"`)
	b.WriteString(strings.Repeat("a", maxLine))
	b.WriteString("\"}\n")

	s := NewScanner(&b)
	ev, err := s.Next()
	if err == nil {
		t.Fatalf("Next = %+v, want error", ev)
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("err = %v, want wrapped bufio.ErrTooLong", err)
	}
	if !strings.Contains(err.Error(), "streamjson: scan") {
		t.Errorf("err = %v, want %q prefix wrap", err, "streamjson: scan")
	}
}

func TestAsResult(t *testing.T) {
	line := `{"type":"result","subtype":"success","result":"all good","session_id":"sess-42","total_cost_usd":0.125,"is_error":true,"usage":{"input_tokens":10},"permission_denials":[{"tool":"Bash"}]}`
	s := NewScanner(strings.NewReader(line + "\n"))
	ev := mustNext(t, s)

	re, ok := ev.AsResult()
	if !ok {
		t.Fatal("AsResult = false, want true")
	}
	if re.Result != "all good" {
		t.Errorf("Result = %q, want %q", re.Result, "all good")
	}
	if re.SessionID != "sess-42" {
		t.Errorf("SessionID = %q, want %q", re.SessionID, "sess-42")
	}
	if re.TotalCostUSD != 0.125 {
		t.Errorf("TotalCostUSD = %v, want 0.125", re.TotalCostUSD)
	}
	if !re.IsError {
		t.Error("IsError = false, want true")
	}
	if string(re.Usage) != `{"input_tokens":10}` {
		t.Errorf("Usage = %s", re.Usage)
	}
	if string(re.PermissionDenials) != `[{"tool":"Bash"}]` {
		t.Errorf("PermissionDenials = %s", re.PermissionDenials)
	}
}

func TestAsResultRejects(t *testing.T) {
	tests := []struct {
		name string
		ev   *Event
	}{
		{"nil event", nil},
		{"wrong type", &Event{Type: "assistant", Raw: json.RawMessage(`{"type":"assistant"}`)}},
		{"result with mismatched field types", &Event{Type: "result", Raw: json.RawMessage(`{"type":"result","result":42}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, ok := tt.ev.AsResult()
			if ok || re != nil {
				t.Errorf("AsResult = (%+v, %v), want (nil, false)", re, ok)
			}
		})
	}
}

func TestAsInit(t *testing.T) {
	line := `{"type":"system","subtype":"init","session_id":"sess-7","model":"claude-fable-5","tools":["Bash","Read"],"capabilities":["mcp"]}`
	s := NewScanner(strings.NewReader(line + "\n"))
	ev := mustNext(t, s)

	ie, ok := ev.AsInit()
	if !ok {
		t.Fatal("AsInit = false, want true")
	}
	if ie.SessionID != "sess-7" {
		t.Errorf("SessionID = %q, want %q", ie.SessionID, "sess-7")
	}
	if ie.Model != "claude-fable-5" {
		t.Errorf("Model = %q, want %q", ie.Model, "claude-fable-5")
	}
	if len(ie.Tools) != 2 || ie.Tools[0] != "Bash" || ie.Tools[1] != "Read" {
		t.Errorf("Tools = %v, want [Bash Read]", ie.Tools)
	}
	if len(ie.Capabilities) != 1 || ie.Capabilities[0] != "mcp" {
		t.Errorf("Capabilities = %v, want [mcp]", ie.Capabilities)
	}
}

func TestAsInitRejects(t *testing.T) {
	tests := []struct {
		name string
		ev   *Event
	}{
		{"nil event", nil},
		{"system but not init", &Event{Type: "system", Subtype: "api_retry", Raw: json.RawMessage(`{"type":"system","subtype":"api_retry"}`)}},
		{"init subtype wrong type", &Event{Type: "assistant", Subtype: "init", Raw: json.RawMessage(`{}`)}},
		{"init with mismatched field types", &Event{Type: "system", Subtype: "init", Raw: json.RawMessage(`{"type":"system","subtype":"init","model":123}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ie, ok := tt.ev.AsInit()
			if ok || ie != nil {
				t.Errorf("AsInit = (%+v, %v), want (nil, false)", ie, ok)
			}
		})
	}
}

func TestNextMultipleEventsInOrder(t *testing.T) {
	input := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant"}

{"type":"result","subtype":"success","result":"ok","session_id":"s1"}
`
	s := NewScanner(strings.NewReader(input))

	ev := mustNext(t, s)
	if _, ok := ev.AsInit(); !ok {
		t.Fatalf("event 1: AsInit failed on %s", ev.Raw)
	}
	ev = mustNext(t, s)
	if ev.Type != "assistant" {
		t.Fatalf("event 2: Type = %q, want assistant", ev.Type)
	}
	ev = mustNext(t, s)
	re, ok := ev.AsResult()
	if !ok || re.Result != "ok" {
		t.Fatalf("event 3: AsResult = (%+v, %v)", re, ok)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("trailing Next err = %v, want io.EOF", err)
	}
}

// Shapes recorded from a real `--include-partial-messages` run.
func TestAsTextDelta(t *testing.T) {
	delta := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"alpha "}},"session_id":"s","parent_tool_use_id":null,"uuid":"u"}`
	ev := &Event{Type: "stream_event", Raw: []byte(delta)}
	d, ok := ev.AsTextDelta()
	if !ok || d.Text != "alpha " || d.ParentToolUseID != "" {
		t.Errorf("delta = %+v ok=%v", d, ok)
	}

	sub := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}},"parent_tool_use_id":"toolu_123"}`
	d, ok = (&Event{Type: "stream_event", Raw: []byte(sub)}).AsTextDelta()
	if !ok || d.ParentToolUseID != "toolu_123" {
		t.Errorf("subagent delta = %+v ok=%v; the parent id must survive so it can be filtered", d, ok)
	}

	for name, raw := range map[string]string{
		"block start":   `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text","text":""}}}`,
		"input delta":   `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}}`,
		"message start": `{"type":"stream_event","event":{"type":"message_start"}}`,
		"assistant":     `{"type":"assistant","message":{"content":[{"type":"text","text":"whole"}]}}`,
	} {
		if _, ok := (&Event{Type: typeOf(raw), Raw: []byte(raw)}).AsTextDelta(); ok {
			t.Errorf("%s was read as a text delta", name)
		}
	}
	if (*Event)(nil).IsStreamEnd() {
		t.Error("nil event is a stream end")
	}
}

func TestIsStreamEnd(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`: true,
		`{"type":"stream_event","event":{"type":"message_stop"}}`:                 true,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`:          false,
		`{"type":"assistant"}`: false,
	} {
		if got := (&Event{Type: typeOf(raw), Raw: []byte(raw)}).IsStreamEnd(); got != want {
			t.Errorf("IsStreamEnd(%s) = %v, want %v", raw, got, want)
		}
	}
}

func typeOf(raw string) string {
	var r struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &r)
	return r.Type
}
