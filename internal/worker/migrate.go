package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FindSession locates sessionID's transcript under configDir/projects/*/,
// returning its absolute path. configDir "" means the user's default
// claude home (~/.claude), which is where interactive sessions live.
func FindSession(configDir, sessionID string) (string, error) {
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("worker: resolve home: %w", err)
		}
		configDir = filepath.Join(home, ".claude")
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("worker: search sessions: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("worker: session %s not found under %s/projects", sessionID, configDir)
	}
	return matches[0], nil
}

// maxResumeBytes caps an imported transcript. A live claude session keeps a
// compacted window in memory, but `claude -p --resume` rebuilds the whole
// on-disk log into one request, so a long conversation blows the context
// window before compaction can run. Trimming keeps resume working.
const maxResumeBytes = 400 << 10

// conversationRecord reports whether a transcript record is part of the
// conversation rather than the surrounding bookkeeping (attachments, file
// history, UI state), which is what makes these logs enormous.
func conversationRecord(line []byte) bool {
	var rec struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return false
	}
	switch rec.Type {
	case "user", "assistant", "system", "summary":
		return true
	}
	return false
}

// trimTranscript returns the conversation records from r, newest-first
// truncated to fit maxResumeBytes, in original order. Whole records are kept
// or dropped: a half-record would not parse.
func trimTranscript(r io.Reader) ([][]byte, bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	var kept [][]byte
	dropped := false
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !conversationRecord(line) {
			dropped = true
			continue
		}
		kept = append(kept, line)
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("worker: read transcript: %w", err)
	}
	budget := maxResumeBytes
	cut := len(kept)
	for i := len(kept) - 1; i >= 0; i-- {
		if budget-len(kept[i]) < 0 {
			cut = i + 1
			dropped = true
			break
		}
		budget -= len(kept[i])
		cut = i
	}
	return kept[cut:], dropped, nil
}

// importSession copies a session transcript into destConfigDir, preserving
// the project-directory name (it encodes the session's original cwd, which
// claude uses to associate the transcript). Oversized transcripts are trimmed
// to the recent conversation so headless resume stays within the context
// window.
// importSession returns the session id to resume: the original when the
// transcript is usable as-is, or a new id naming a trimmed copy.
func importSession(srcPath, destConfigDir, sessionID string, notify func(string)) (string, error) {
	projectDir := filepath.Base(filepath.Dir(srcPath))
	destDir := filepath.Join(destConfigDir, "projects", projectDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("worker: create project dir: %w", err)
	}
	dest := filepath.Join(destDir, filepath.Base(srcPath))

	if fi, err := os.Stat(srcPath); err == nil && fi.Size() > maxResumeBytes {
		return trimIntoNewSession(srcPath, destDir, sessionID, notify)
	}
	if _, err := os.Stat(dest); err == nil {
		return sessionID, nil // already imported (re-resume)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("worker: open source transcript: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("worker: create transcript copy: %w", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return "", fmt.Errorf("worker: copy transcript: %w", err)
	}
	return sessionID, out.Close()
}

// trimIntoNewSession writes the recent conversation of an oversized
// transcript under a fresh session id, leaving the original log untouched.
// The ids inside each record are rewritten so claude reads a coherent
// session.
func trimIntoNewSession(srcPath, destDir, sessionID string, notify func(string)) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("worker: open source transcript: %w", err)
	}
	defer src.Close()

	lines, _, err := trimTranscript(src)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("worker: transcript %s has no conversation records", srcPath)
	}

	newID, err := uuidV4()
	if err != nil {
		return "", fmt.Errorf("worker: new session id: %w", err)
	}
	dest := filepath.Join(destDir, newID+".jsonl")
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("worker: create trimmed transcript: %w", err)
	}
	old, replacement := []byte(sessionID), []byte(newID)
	var bytesWritten int
	for _, line := range lines {
		if sessionID != "" {
			line = bytes.ReplaceAll(line, old, replacement)
		}
		n, err := out.Write(append(line, '\n'))
		if err != nil {
			out.Close()
			return "", fmt.Errorf("worker: write trimmed transcript: %w", err)
		}
		bytesWritten += n
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("worker: close trimmed transcript: %w", err)
	}
	// A nil callback is a caller with nowhere to render, not a bug.
	if notify == nil {
		notify = func(string) {}
	}
	notify(fmt.Sprintf("this conversation is too large to reload whole; resuming its most recent %d messages (%dKB)",
		len(lines), bytesWritten/1024))
	return newID, nil
}

// Message is one conversational turn recovered from a transcript.
type Message struct {
	Role string // "user" or "assistant"
	Text string
}

// RecentMessages returns up to max trailing user/assistant messages from a
// transcript, so a resumed session can be shown to the operator rather than
// only handed to the model. Tool calls, attachments, and bookkeeping are
// skipped; empty messages are dropped.
func RecentMessages(path string, max int) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("worker: open transcript: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	var msgs []Message
	for sc.Scan() {
		line := sc.Bytes()
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		text := extractText(rec.Message.Content)
		if text == "" || !operatorVisible(text) {
			continue
		}
		msgs = append(msgs, Message{Role: rec.Type, Text: text})
		if len(msgs) > max*4 { // keep the tail cheap on huge logs
			msgs = msgs[len(msgs)-max*2:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("worker: read transcript: %w", err)
	}
	if len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}
	return msgs, nil
}

// operatorVisible filters out harness plumbing that was never part of the
// conversation the operator had: injected reminders, task notifications, and
// bare image placeholders.
func operatorVisible(text string) bool {
	switch {
	case strings.HasPrefix(text, "<task-notification"),
		strings.HasPrefix(text, "<system-reminder"),
		strings.HasPrefix(text, "<cross-session-message"),
		strings.HasPrefix(text, "[Image:"),
		strings.HasPrefix(text, "[Request interrupted"):
		return false
	}
	return true
}

// extractText pulls plain text out of a content field that may be a string or
// a block array.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
