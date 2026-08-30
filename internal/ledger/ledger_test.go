package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/streamjson"
)

// readLines reads <dir>/events.jsonl and returns its non-terminal lines,
// verifying the file ends with a newline (tail-ability contract).
func readLines(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	s := string(b)
	if len(s) == 0 {
		return nil
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("events.jsonl does not end with newline: %q", s)
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func TestEventEnvelope(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	ev := &streamjson.Event{
		Type: "assistant",
		Raw:  json.RawMessage(`{"type":"assistant","message":"hi"}`),
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := l.Event("node-1", "worker-a", "task-9", ev); err != nil {
		t.Fatalf("Event: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var env struct {
		TS     string          `json:"ts"`
		Node   string          `json:"node"`
		Worker string          `json:"worker"`
		Task   string          `json:"task"`
		Seq    uint64          `json:"seq"`
		Event  json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (line %q)", err, lines[0])
	}
	if env.Node != "node-1" || env.Worker != "worker-a" || env.Task != "task-9" {
		t.Errorf("node/worker/task = %q/%q/%q, want node-1/worker-a/task-9", env.Node, env.Worker, env.Task)
	}
	if env.Seq != 1 {
		t.Errorf("Seq = %d, want 1", env.Seq)
	}
	if string(env.Event) != `{"type":"assistant","message":"hi"}` {
		t.Errorf("Event = %s, want original raw line", env.Event)
	}
	ts, err := time.Parse(time.RFC3339Nano, env.TS)
	if err != nil {
		t.Fatalf("ts %q not RFC3339Nano: %v", env.TS, err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("ts %v outside window [%v, %v]", ts, before, after)
	}
}

func TestEventSeqIncrements(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	ev := &streamjson.Event{Raw: json.RawMessage(`{"type":"user"}`)}
	for i := 0; i < 3; i++ {
		if err := l.Event("n", "w", "t", ev); err != nil {
			t.Fatalf("Event %d: %v", i, err)
		}
	}

	lines := readLines(t, dir)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var env struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if want := uint64(i + 1); env.Seq != want {
			t.Errorf("line %d: Seq = %d, want %d", i, env.Seq, want)
		}
	}
}

func TestEventNilAndEmptyRaw(t *testing.T) {
	tests := []struct {
		name string
		ev   *streamjson.Event
	}{
		{"nil event", nil},
		{"empty raw", &streamjson.Event{Type: "assistant"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()

			if err := l.Event("n", "w", "t", tt.ev); err != nil {
				t.Fatalf("Event: %v", err)
			}
			lines := readLines(t, dir)
			var env struct {
				Event json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if string(env.Event) != "null" {
				t.Errorf("event = %s, want null", env.Event)
			}
		})
	}
}

func TestNoteObjectSpreadsFields(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if err := l.Note("worker_started", map[string]any{"worker": "w-1", "pid": 4321}); err != nil {
		t.Fatalf("Note: %v", err)
	}

	lines := readLines(t, dir)
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "worker_started" {
		t.Errorf("kind = %v, want worker_started", got["kind"])
	}
	if got["worker"] != "w-1" {
		t.Errorf("worker = %v, want w-1", got["worker"])
	}
	if got["pid"] != float64(4321) {
		t.Errorf("pid = %v, want 4321", got["pid"])
	}
	if _, ok := got["value"]; ok {
		t.Error("object note must spread fields, not nest under value")
	}
	if _, err := time.Parse(time.RFC3339Nano, got["ts"].(string)); err != nil {
		t.Errorf("ts %v not RFC3339Nano: %v", got["ts"], err)
	}
}

func TestNoteTSAndKindWinOnCollision(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if err := l.Note("real_kind", map[string]any{"kind": "spoofed", "ts": "1999-01-01", "x": 1}); err != nil {
		t.Fatalf("Note: %v", err)
	}

	lines := readLines(t, dir)
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "real_kind" {
		t.Errorf("kind = %v, want real_kind (envelope must win)", got["kind"])
	}
	if got["ts"] == "1999-01-01" {
		t.Error("ts kept the spoofed value; envelope must win")
	}
	if _, err := time.Parse(time.RFC3339Nano, got["ts"].(string)); err != nil {
		t.Errorf("ts %v not RFC3339Nano: %v", got["ts"], err)
	}
	if got["x"] != float64(1) {
		t.Errorf("x = %v, want 1", got["x"])
	}
}

func TestNoteNonObjectWrappedUnderValue(t *testing.T) {
	tests := []struct {
		name      string
		v         any
		wantValue string // raw JSON of the "value" field
	}{
		{"string scalar", "hello", `"hello"`},
		{"number scalar", 42, `42`},
		{"bool scalar", true, `true`},
		{"array", []int{1, 2, 3}, `[1,2,3]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()

			if err := l.Note("k", tt.v); err != nil {
				t.Fatalf("Note: %v", err)
			}
			lines := readLines(t, dir)
			var got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if string(got["value"]) != tt.wantValue {
				t.Errorf("value = %s, want %s", got["value"], tt.wantValue)
			}
			if string(got["kind"]) != `"k"` {
				t.Errorf("kind = %s, want \"k\"", got["kind"])
			}
			if _, ok := got["ts"]; !ok {
				t.Error("ts missing")
			}
		})
	}
}

func TestNoteNilValue(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if err := l.Note("bare", nil); err != nil {
		t.Fatalf("Note: %v", err)
	}
	lines := readLines(t, dir)
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("nil-value note has fields %v, want only ts and kind", got)
	}
	if got["kind"] != "bare" {
		t.Errorf("kind = %v, want bare", got["kind"])
	}
}

func TestNoteUnmarshalableValueErrors(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	err = l.Note("bad", make(chan int))
	if err == nil {
		t.Fatal("Note(chan) = nil, want error")
	}
	if !strings.Contains(err.Error(), `marshal note "bad"`) {
		t.Errorf("err = %v, want marshal note wrap", err)
	}
	// Nothing was written.
	if lines := readLines(t, dir); len(lines) != 0 {
		t.Errorf("failed Note wrote %d lines: %v", len(lines), lines)
	}
}

func TestNilReceiverNoOps(t *testing.T) {
	var l *Ledger
	if err := l.Event("n", "w", "t", &streamjson.Event{Raw: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("nil.Event = %v, want nil", err)
	}
	if err := l.Note("k", map[string]any{"a": 1}); err != nil {
		t.Errorf("nil.Note = %v, want nil", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("nil.Close = %v, want nil", err)
	}
}

func TestWritesAreTailableBeforeClose(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// Each call must land a complete newline-terminated line without waiting
	// for Close, so `tail -f` style readers see whole JSON lines.
	for i := 1; i <= 3; i++ {
		if err := l.Note("tick", i); err != nil {
			t.Fatalf("Note %d: %v", i, err)
		}
		lines := readLines(t, dir)
		if len(lines) != i {
			t.Fatalf("after write %d: file has %d lines, want %d", i, len(lines), i)
		}
		for j, line := range lines {
			if !json.Valid([]byte(line)) {
				t.Errorf("line %d is not valid JSON: %q", j, line)
			}
		}
	}
}

func TestCloseIdempotentAndWriteAfterCloseErrors(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Note("before", nil); err != nil {
		t.Fatalf("Note: %v", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil (idempotent)", err)
	}

	if err := l.Note("after", nil); err == nil {
		t.Error("Note after Close = nil, want error")
	} else if !strings.Contains(err.Error(), "write after close") {
		t.Errorf("Note after Close err = %v, want write-after-close", err)
	}
	if err := l.Event("n", "w", "t", nil); err == nil {
		t.Error("Event after Close = nil, want error")
	}

	// The failed writes must not have landed.
	if lines := readLines(t, dir); len(lines) != 1 {
		t.Errorf("file has %d lines after close, want 1", len(lines))
	}
}

func TestOpenCreatesDirAndAppendsAcrossOpens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")

	l1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (creates dir): %v", err)
	}
	if err := l1.Note("first", nil); err != nil {
		t.Fatalf("Note: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := l2.Event("n", "w", "t", &streamjson.Event{Raw: json.RawMessage(`{"type":"user"}`)}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("got %d lines after reopen, want 2 (append, not truncate)", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0: %v", err)
	}
	if first["kind"] != "first" {
		t.Errorf("line 0 kind = %v; reopen clobbered earlier data", first["kind"])
	}
	// Seq is per-Ledger, so a fresh Open restarts at 1.
	var second struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if second.Seq != 1 {
		t.Errorf("seq after reopen = %d, want 1 (per-Ledger counter)", second.Seq)
	}
}
