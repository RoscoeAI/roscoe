// Package ledger appends run events to an append-only events.jsonl file.
// Single writer, mutex-guarded; durability is fsync-on-Close only.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/streamjson"
)

// Ledger is a mutex-guarded appender to <dir>/events.jsonl.
type Ledger struct {
	mu     sync.Mutex
	f      *os.File
	seq    uint64
	closed bool
}

// Open creates dir if missing and opens events.jsonl for append.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	return &Ledger{f: f}, nil
}

// envelope is the wire shape of one worker event line.
type envelope struct {
	TS     string          `json:"ts"`
	Node   string          `json:"node"`
	Worker string          `json:"worker"`
	Task   string          `json:"task"`
	Seq    uint64          `json:"seq"`
	Event  json.RawMessage `json:"event"`
}

// Event appends a worker stream-json event wrapped in the node envelope.
// Seq is a per-Ledger monotonic counter. Safe on a nil receiver (no-op).
func (l *Ledger) Event(node, worker, task string, ev *streamjson.Event) error {
	if l == nil {
		return nil
	}
	raw := json.RawMessage("null")
	if ev != nil && len(ev.Raw) > 0 {
		raw = ev.Raw
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	line, err := json.Marshal(envelope{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Node:   node,
		Worker: worker,
		Task:   task,
		Seq:    l.seq,
		Event:  raw,
	})
	if err != nil {
		return fmt.Errorf("ledger: marshal event: %w", err)
	}
	return l.writeLocked(line)
}

// Note appends an orchestrator-side record: {ts, kind, ...v}. When v is a JSON
// object its fields are spread into the line (ts/kind win on collision);
// otherwise v lands under "value". Safe on a nil receiver (no-op).
func (l *Ledger) Note(kind string, v any) error {
	if l == nil {
		return nil
	}
	obj := map[string]any{}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("ledger: marshal note %q: %w", kind, err)
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			// Not a JSON object (scalar, array, ...): keep it whole.
			obj = map[string]any{"value": json.RawMessage(b)}
		}
	}
	obj["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	obj["kind"] = kind

	line, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("ledger: marshal note %q: %w", kind, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeLocked(line)
}

// writeLocked appends line + "\n"; caller holds l.mu.
func (l *Ledger) writeLocked(line []byte) error {
	if l.closed {
		return fmt.Errorf("ledger: write after close")
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("ledger: append: %w", err)
	}
	return nil
}

// Close fsyncs and closes the file. Idempotent; safe on a nil receiver.
func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if err := l.f.Sync(); err != nil {
		l.f.Close()
		return fmt.Errorf("ledger: fsync: %w", err)
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("ledger: close: %w", err)
	}
	return nil
}
