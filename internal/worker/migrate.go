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

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/streamjson"
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
//
// The cap is in bytes because that is what the file has, but the limit that
// matters is tokens. Tool-heavy transcript JSON packs at roughly three
// characters per token, and the worker's own prefix is ~52K tokens on a 200K
// window, so the transcript gets about 80K tokens: ~240KB. The previous cap
// of 400KB produced a 399KB window that the API rejected with "Prompt is too
// long" on the first turn. When a window still does not fit, chat halves it
// and retries (see HalveBudget); this is the starting point, not a promise.
const maxResumeBytes = 240 << 10

// maxRecordBytes is the size past which a single kept record has its tool
// results clipped. Records are kept or dropped whole, so one 200KB tool
// result would otherwise eat most of the window on its own.
const maxRecordBytes = 24 << 10

// clipToBytes is how much of an oversized tool result survives.
const clipToBytes = 4 << 10

// DefaultResumeBudget is the transcript window a resume starts with.
func DefaultResumeBudget() int { return maxResumeBytes }

// HalveBudget is the next window to try after the model refused the current
// one as too long. It never goes below 32KB: past that, resuming is pointless
// and starting fresh is the honest answer.
func HalveBudget(current int) int {
	if current <= 0 {
		current = maxResumeBytes
	}
	next := current / 2
	if next < 32<<10 {
		next = 32 << 10
	}
	return next
}

// PromptTooLong reports whether a turn's result text is the API refusing the
// request for size, which is what an oversized resume window produces.
func PromptTooLong(result string) bool {
	return strings.Contains(strings.ToLower(result), "prompt is too long")
}

// MaxTooLongRetries bounds how many times a caller halves the window and
// resends before giving the error to the operator.
const MaxTooLongRetries = 3

// RetryTooLong is the one decision chat and run share: this result is the
// model refusing the resume window as too long, and we have retries left.
func RetryTooLong(res *streamjson.ResultEvent, attempt int) bool {
	return res != nil && res.IsError && PromptTooLong(res.Result) && attempt < MaxTooLongRetries
}

// Oversized reports whether a session transcript has grown past the point
// where resuming it whole is a bad idea. Chat asks this after every turn: the
// import trims once, but `claude -p --resume` appends every turn after that,
// and one substantive turn was measured to grow a 400KB import to 689KB.
func Oversized(path string) bool { return OversizedBy(path, maxResumeBytes) }

// OversizedBy is Oversized against a specific window (0 means the default),
// for a chat that has already had to shrink its window once.
func OversizedBy(path string, budget int) bool {
	if budget <= 0 {
		budget = maxResumeBytes
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > int64(budget)
}

// SessionConfigDir is where a task's transcripts live: the operator's own
// ~/.claude on the own-auth path, or the task's isolated dir under a fleet
// token. Callers that need to find a session after a turn must look in the
// same place the worker wrote it.
func SessionConfigDir(cfg *config.Config, taskID, token string) (string, error) {
	if token == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("worker: resolve home: %w", err)
		}
		return filepath.Join(home, ".claude"), nil
	}
	return filepath.Join(config.ExpandPath(cfg.StateDir), "workers", taskID, "ccfg"), nil
}

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
	return trimTranscriptTo(r, maxResumeBytes)
}

// trimTranscriptTo is trimTranscript with an explicit byte budget.
func trimTranscriptTo(r io.Reader, budget int) ([][]byte, bool, error) {
	return trimTranscriptRewriting(r, budget, nil)
}

// trimTranscriptRewriting applies rewrite to every kept record BEFORE it is
// measured against the budget, so what is written is what was budgeted. The
// session-id substitution grows every record (a 7-char id becomes a 36-char
// uuid), and measuring first once produced a 246KB window under a 240KB cap.
func trimTranscriptRewriting(r io.Reader, budget int, rewrite func([]byte) []byte) ([][]byte, bool, error) {
	if budget <= 0 {
		budget = maxResumeBytes
	}
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
		if len(line) > maxRecordBytes {
			line = clipRecord(line)
		}
		if rewrite != nil {
			line = rewrite(line)
		}
		kept = append(kept, line)
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("worker: read transcript: %w", err)
	}
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

// clipRecord shortens the tool results inside one transcript record so a
// single huge tool output cannot dominate the resume window. Everything else
// in the record is left as it was; anything that does not parse is returned
// untouched, because a record we cannot understand is one we must not edit.
func clipRecord(line []byte) []byte {
	var rec map[string]any
	if json.Unmarshal(line, &rec) != nil {
		return line
	}
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return line
	}
	items, ok := msg["content"].([]any)
	if !ok {
		return line
	}
	changed := false
	for _, it := range items {
		block, ok := it.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		switch c := block["content"].(type) {
		case string:
			if len(c) > clipToBytes {
				block["content"] = clipText(c)
				changed = true
			}
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := pm["text"].(string); ok && len(t) > clipToBytes {
					pm["text"] = clipText(t)
					changed = true
				}
			}
		}
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return line
	}
	return out
}

func clipText(s string) string {
	// Cut on a rune boundary, then say what happened where the rest was.
	cut := clipToBytes
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[… %d more bytes clipped by roscoe so this conversation could be resumed]", len(s)-cut)
}

// importSession copies a session transcript into destConfigDir, preserving
// the project-directory name (it encodes the session's original cwd, which
// claude uses to associate the transcript). Oversized transcripts are trimmed
// to the recent conversation so headless resume stays within the context
// window.
// importSession returns the session id to resume: the original when the
// transcript is usable as-is, or a new id naming a trimmed copy.
func importSession(srcPath, destConfigDir, sessionID string, notify func(string)) (string, error) {
	return importSessionWithBudget(srcPath, destConfigDir, sessionID, notify, maxResumeBytes)
}

// importSessionWithBudget is importSession with an explicit resume window.
func importSessionWithBudget(srcPath, destConfigDir, sessionID string, notify func(string), budget int) (string, error) {
	if budget <= 0 {
		budget = maxResumeBytes
	}
	projectDir := filepath.Base(filepath.Dir(srcPath))
	destDir := filepath.Join(destConfigDir, "projects", projectDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("worker: create project dir: %w", err)
	}
	dest := filepath.Join(destDir, filepath.Base(srcPath))

	if fi, err := os.Stat(srcPath); err == nil && fi.Size() > int64(budget) {
		return trimIntoNewSession(srcPath, destDir, sessionID, notify, budget)
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
func trimIntoNewSession(srcPath, destDir, sessionID string, notify func(string), budget int) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("worker: open source transcript: %w", err)
	}
	defer src.Close()

	newID, err := uuidV4()
	if err != nil {
		return "", fmt.Errorf("worker: new session id: %w", err)
	}
	// The records are rewritten to the new id before they are budgeted, so
	// the window on disk is the window that was measured.
	var rewrite func([]byte) []byte
	if sessionID != "" {
		old, replacement := []byte(sessionID), []byte(newID)
		rewrite = func(line []byte) []byte { return bytes.ReplaceAll(line, old, replacement) }
	}
	lines, _, err := trimTranscriptRewriting(src, budget, rewrite)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("worker: transcript %s has no conversation records", srcPath)
	}

	dest := filepath.Join(destDir, newID+".jsonl")
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("worker: create trimmed transcript: %w", err)
	}
	var bytesWritten int
	for _, line := range lines {
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

// FirstMessage returns the first thing the operator said in a transcript,
// which is what people recognise a conversation by in a listing. Harness
// plumbing (reminders, notifications) is skipped the same way RecentMessages
// skips it, so a resumed session's injected preamble is never mistaken for
// the prompt.
func FirstMessage(path string) (Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return Message{}, fmt.Errorf("worker: open transcript: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "user" {
			continue
		}
		text := extractText(rec.Message.Content)
		if text == "" || !operatorVisible(text) {
			continue
		}
		return Message{Role: "user", Text: text}, nil
	}
	if err := sc.Err(); err != nil {
		return Message{}, fmt.Errorf("worker: read transcript: %w", err)
	}
	return Message{}, fmt.Errorf("worker: no operator message in %s", filepath.Base(path))
}
