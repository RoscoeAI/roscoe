package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"roscoe.sh/roscoe/internal/streamjson"
	"runtime"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// writeTranscript creates configDir/projects/<project>/<sessionID>.jsonl with
// the given content and returns its path.
func writeTranscript(t *testing.T, configDir, project, sessionID, content string) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFindSessionFound(t *testing.T) {
	cfgDir := t.TempDir()
	const sid = "aaaa1111-bbbb-4ccc-8ddd-eeeeffff0000"
	want := writeTranscript(t, cfgDir, "-Users-tim-code-app", sid, "{}\n")

	got, err := FindSession(cfgDir, sid)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if got != want {
		t.Errorf("FindSession = %q, want %q", got, want)
	}
}

func TestFindSessionNotFound(t *testing.T) {
	cfgDir := t.TempDir()
	// A projects tree exists, with transcripts for other sessions only.
	writeTranscript(t, cfgDir, "-Users-tim-other", "other-session", "{}\n")

	const sid = "does-not-exist"
	_, err := FindSession(cfgDir, sid)
	if err == nil {
		t.Fatal("FindSession = nil error, want not-found")
	}
	msg := err.Error()
	if !strings.Contains(msg, sid) {
		t.Errorf("err %q does not name the session id", msg)
	}
	if !strings.Contains(msg, filepath.Join(cfgDir, "projects")) {
		t.Errorf("err %q does not name the searched dir %s/projects", msg, cfgDir)
	}
}

func TestFindSessionEmptyConfigDirUsesHomeClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based UserHomeDir override is unix-only")
	}
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	const sid = "cccc1111-dddd-4eee-9fff-000011112222"
	want := writeTranscript(t, filepath.Join(fakeHome, ".claude"), "-Users-tim-proj", sid, "{}\n")

	got, err := FindSession("", sid)
	if err != nil {
		t.Fatalf("FindSession with empty configDir: %v", err)
	}
	if got != want {
		t.Errorf("FindSession = %q, want %q", got, want)
	}
}

func TestFindSessionMultipleMatchesReturnsFirst(t *testing.T) {
	cfgDir := t.TempDir()
	const sid = "dddd1111-eeee-4fff-8000-111122223333"
	first := writeTranscript(t, cfgDir, "aaa-project", sid, "first\n")
	writeTranscript(t, cfgDir, "zzz-project", sid, "second\n")

	got, err := FindSession(cfgDir, sid)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	// filepath.Glob returns sorted matches; the lexically first project wins.
	if got != first {
		t.Errorf("FindSession = %q, want first sorted match %q", got, first)
	}
}

func TestImportSessionCopiesPreservingProjectDir(t *testing.T) {
	srcCfg := t.TempDir()
	destCfg := t.TempDir()
	const sid = "eeee1111-aaaa-4bbb-8ccc-444455556666"
	const content = "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	src := writeTranscript(t, srcCfg, "-Users-tim-code-app", sid, content)

	if _, err := importSession(src, destCfg, "sess-1", nil); err != nil {
		t.Fatalf("importSession: %v", err)
	}

	dest := filepath.Join(destCfg, "projects", "-Users-tim-code-app", sid+".jsonl")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("copy missing at project-preserving path: %v", err)
	}
	if string(got) != content {
		t.Errorf("copied content = %q, want %q", got, content)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("copy mode = %o, want 0600", perm)
	}
}

func TestImportSessionIdempotent(t *testing.T) {
	srcCfg := t.TempDir()
	destCfg := t.TempDir()
	const sid = "ffff1111-aaaa-4bbb-8ccc-777788889999"
	src := writeTranscript(t, srcCfg, "proj", sid, "original\n")

	if _, err := importSession(src, destCfg, "sess-1", nil); err != nil {
		t.Fatalf("first importSession: %v", err)
	}
	// Change the source; a re-import must be a no-op, not an overwrite.
	if err := os.WriteFile(src, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importSession(src, destCfg, "sess-1", nil); err != nil {
		t.Fatalf("second importSession: %v", err)
	}

	dest := filepath.Join(destCfg, "projects", "proj", sid+".jsonl")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original\n" {
		t.Errorf("re-import overwrote the copy: got %q, want %q", got, "original\n")
	}
}

func TestImportSessionMissingSource(t *testing.T) {
	destCfg := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects", "proj", "nope.jsonl")

	_, err := importSession(src, destCfg, "sess-1", nil)
	if err == nil {
		t.Fatal("importSession = nil error, want open failure")
	}
	if !strings.Contains(err.Error(), "open source transcript") {
		t.Errorf("err = %q, want 'open source transcript'", err)
	}
	// No half-written destination file.
	if _, statErr := os.Stat(filepath.Join(destCfg, "projects", "proj", "nope.jsonl")); !os.IsNotExist(statErr) {
		t.Errorf("destination file exists after failed import (stat err = %v)", statErr)
	}
}

func TestImportSessionTrimsOversizedTranscript(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "projects", "-Users-tim-code-app")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	src := filepath.Join(srcDir, sessionID+".jsonl")

	// A log dominated by bookkeeping: only the conversation should survive,
	// and only as much of it as fits.
	var b strings.Builder
	blob := strings.Repeat("x", 50_000)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, `{"type":"attachment","sessionId":%q,"data":%q}`+"\n", sessionID, blob)
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"n":%d,"text":%q}`+"\n", sessionID, i, blob)
		fmt.Fprintf(&b, `{"type":"file-history-snapshot","sessionId":%q}`+"\n", sessionID)
	}
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	destCfg := t.TempDir()
	newID, err := importSession(src, destCfg, sessionID, nil)
	if err != nil {
		t.Fatalf("importSession: %v", err)
	}
	if newID == sessionID {
		t.Fatal("an oversized transcript must resume under a new session id")
	}

	out, err := os.ReadFile(filepath.Join(destCfg, "projects", "-Users-tim-code-app", newID+".jsonl"))
	if err != nil {
		t.Fatalf("trimmed transcript missing: %v", err)
	}
	if len(out) > maxResumeBytes+50_000 {
		t.Errorf("trimmed transcript is %d bytes, want <= budget", len(out))
	}
	if strings.Contains(string(out), `"attachment"`) || strings.Contains(string(out), "file-history-snapshot") {
		t.Error("bookkeeping records should be dropped")
	}
	if !strings.Contains(string(out), newID) || strings.Contains(string(out), sessionID) {
		t.Error("session ids inside the trimmed transcript should be rewritten")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the original log must be left alone: %v", err)
	}
}

// oversizedTranscript writes a transcript far past the resume cap and returns
// its path plus a fresh destination config dir.
func oversizedTranscript(t *testing.T) (src, destCfg string) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "projects", "-Users-tim-code-app")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	src = filepath.Join(srcDir, sessionID+".jsonl")
	var b strings.Builder
	blob := strings.Repeat("x", 50_000)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"n":%d,"text":%q}`+"\n", sessionID, i, blob)
	}
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return src, t.TempDir()
}

// The trim notice must reach whoever owns the screen. Writing it to stderr
// under a full-screen TUI paints it into the viewport, where the next repaint
// wipes it: the operator sees a message flash and vanish.
func TestTrimNoticeGoesToTheCaller(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src, destCfg := oversizedTranscript(t)
	var got []string
	if _, err := importSession(src, destCfg, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", func(m string) { got = append(got, m) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d notices, want 1: %v", len(got), got)
	}
	for _, want := range []string{"too large", "most recent"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("notice %q is missing %q", got[0], want)
		}
	}
}

// A caller with nowhere to render is not a bug, and must not crash the run.
func TestNilNoticeIsSafe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src, destCfg := oversizedTranscript(t)
	if _, err := importSession(src, destCfg, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", nil); err != nil {
		t.Fatal(err)
	}
}

// Chat re-trims a session from its own config dir between turns. The trimmed
// copy must land beside the source under a new id, and the source must be left
// alone, or a mid-conversation trim destroys the very history it is compacting.
func TestReimportFromSameConfigDirTrimsIntoNewSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgDir := t.TempDir()
	projDir := filepath.Join(cfgDir, "projects", "-Users-tim-code-app")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	src := filepath.Join(projDir, sessionID+".jsonl")
	var b strings.Builder
	blob := strings.Repeat("x", 50_000)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"n":%d,"text":%q}`+"\n", sessionID, i, blob)
	}
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(src)
	if !Oversized(src) {
		t.Fatal("fixture is not oversized")
	}

	// dest config dir == the dir the source already lives in.
	newID, err := importSession(src, cfgDir, sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newID == sessionID {
		t.Fatal("re-import of an oversized session kept the old id")
	}
	after, err := os.Stat(src)
	if err != nil || after.Size() != before.Size() {
		t.Errorf("source transcript was modified (%v -> %v)", before.Size(), after.Size())
	}
	trimmed := filepath.Join(projDir, newID+".jsonl")
	fi, err := os.Stat(trimmed)
	if err != nil {
		t.Fatalf("trimmed copy missing beside the source: %v", err)
	}
	if Oversized(trimmed) {
		t.Errorf("trimmed copy is still oversized: %d bytes", fi.Size())
	}
	// And it is findable where chat will look next turn.
	if p, err := FindSession(cfgDir, newID); err != nil || p != trimmed {
		t.Errorf("FindSession(%q) = %q, %v", newID, p, err)
	}
}

func TestOversizedAndSessionConfigDir(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(small, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Oversized(small) {
		t.Error("a tiny file reported oversized")
	}
	if Oversized(filepath.Join(dir, "missing.jsonl")) {
		t.Error("a missing file reported oversized")
	}

	cfg := config.Default()
	cfg.StateDir = dir
	own, err := SessionConfigDir(cfg, "task-1", "")
	if err != nil || !strings.HasSuffix(own, ".claude") {
		t.Errorf("own-auth dir = %q, %v; want ~/.claude", own, err)
	}
	fleet, err := SessionConfigDir(cfg, "task-1", "tok")
	if err != nil || fleet != filepath.Join(dir, "workers", "task-1", "ccfg") {
		t.Errorf("fleet dir = %q, %v", fleet, err)
	}
}

// A listing shows a conversation by its first prompt, so FirstMessage must
// skip the harness plumbing that precedes it in a resumed transcript.
func TestFirstMessageSkipsPlumbing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"user","message":{"role":"user","content":"<system-reminder>injected</system-reminder>"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"fix the billing module"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"on it"}}`,
		`{"type":"user","message":{"role":"user","content":"a later message"}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := FirstMessage(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "fix the billing module" || m.Role != "user" {
		t.Errorf("first = %+v", m)
	}
	if _, err := FirstMessage(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Error("missing file gave no error")
	}
	empty := filepath.Join(dir, "empty.jsonl")
	_ = os.WriteFile(empty, []byte(`{"type":"system"}`+"\n"), 0o600)
	if _, err := FirstMessage(empty); err == nil {
		t.Error("a transcript with no operator message gave no error")
	}
}

// The window is in bytes but the limit is tokens: a 399KB window was refused
// as too long on a real session. The default must leave room for the ~52K
// token worker prefix on a 200K context.
func TestResumeBudgetIsTokenSafe(t *testing.T) {
	if maxResumeBytes > 256<<10 {
		t.Errorf("maxResumeBytes = %d; over 256KB the API refused the prompt", maxResumeBytes)
	}
	if DefaultResumeBudget() != maxResumeBytes {
		t.Error("DefaultResumeBudget disagrees with the constant")
	}
	cases := map[int]int{0: maxResumeBytes / 2, maxResumeBytes: maxResumeBytes / 2, 100 << 10: 50 << 10, 40 << 10: 32 << 10, 10: 32 << 10}
	for in, want := range cases {
		if got := HalveBudget(in); got != want {
			t.Errorf("HalveBudget(%d) = %d, want %d", in, got, want)
		}
	}
	for text, want := range map[string]bool{"Prompt is too long": true, "prompt is too long for this model": true, "ok": false, "": false} {
		if PromptTooLong(text) != want {
			t.Errorf("PromptTooLong(%q) = %v", text, !want)
		}
	}
}

// Chat and run share one retry decision: an error result that says the
// prompt is too long, while retries remain. Anything else is not retried.
func TestRetryTooLong(t *testing.T) {
	tooLong := &streamjson.ResultEvent{IsError: true, Result: "Prompt is too long"}
	if !RetryTooLong(tooLong, 0) || !RetryTooLong(tooLong, MaxTooLongRetries-1) {
		t.Error("a too-long error with retries left must retry")
	}
	if RetryTooLong(tooLong, MaxTooLongRetries) {
		t.Error("retries must stop at the cap")
	}
	if RetryTooLong(&streamjson.ResultEvent{IsError: true, Result: "rate limited"}, 0) {
		t.Error("an unrelated error must not shrink the window")
	}
	if RetryTooLong(&streamjson.ResultEvent{IsError: false, Result: "Prompt is too long"}, 0) {
		t.Error("a successful turn that happens to say the phrase is not an error")
	}
	if RetryTooLong(nil, 0) {
		t.Error("nil result")
	}
}

func TestTrimTranscriptToHonoursBudget(t *testing.T) {
	var b strings.Builder
	line := strings.Repeat("y", 1000)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, `{"type":"user","n":%d,"text":%q}`+"\n", i, line)
	}
	kept, dropped, err := trimTranscriptTo(strings.NewReader(b.String()), 10_500)
	if err != nil {
		t.Fatal(err)
	}
	if !dropped || len(kept) != 10 {
		t.Errorf("kept %d records (dropped=%v), want the 10 most recent", len(kept), dropped)
	}
	if !strings.Contains(string(kept[len(kept)-1]), `"n":99`) || !strings.Contains(string(kept[0]), `"n":90`) {
		t.Errorf("wrong records kept: first %s", kept[0][:30])
	}
	// A zero budget means the default.
	kept, _, _ = trimTranscriptTo(strings.NewReader(b.String()), 0)
	if len(kept) != 100 {
		t.Errorf("default budget kept %d of 100 small records", len(kept))
	}
}

// One 200KB tool result must not eat the resume window: clip it inside the
// record and keep everything else in the record as it was.
func TestClipRecordShrinksHugeToolResults(t *testing.T) {
	huge := strings.Repeat("z", 200_000)
	rec := fmt.Sprintf(`{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":%q},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":%q}]},{"type":"text","text":"keep me"}]}}`, huge, huge)
	out := clipRecord([]byte(rec))
	if len(out) > maxRecordBytes {
		t.Fatalf("clipped record is still %d bytes", len(out))
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("clipped record does not parse: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "keep me") || !strings.Contains(s, `"tool_use_id":"t1"`) || !strings.Contains(s, "clipped by roscoe") {
		t.Errorf("clip lost structure: %s", s[:200])
	}
	if strings.Count(s, "clipped by roscoe") != 2 {
		t.Errorf("both oversized results should be clipped, got %d markers", strings.Count(s, "clipped by roscoe"))
	}
	// Small records and unparseable ones are returned as they were.
	small := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"tiny"}]}}`)
	if string(clipRecord(small)) != string(small) {
		t.Error("a small record was rewritten")
	}
	bad := []byte(`{"type":"user", not json`)
	if string(clipRecord(bad)) != string(bad) {
		t.Error("an unparseable record was altered")
	}
	// And the trimmer applies it: a transcript of huge tool results fits many
	// more messages than whole-record trimming would keep.
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, `{"type":"user","n":%d,"message":{"role":"user","content":[{"type":"tool_result","content":%q}]}}`+"\n", i, huge)
	}
	kept, _, err := trimTranscriptTo(strings.NewReader(b.String()), maxResumeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 20 {
		t.Errorf("kept %d of 20 clipped records; clipping should make them all fit", len(kept))
	}
}

// A smaller budget flows through the import: the retry path depends on it.
func TestImportSessionWithSmallerBudget(t *testing.T) {
	src, destCfg := oversizedTranscript(t)
	newID, err := importSessionWithBudget(src, destCfg, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", nil, 120<<10)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(destCfg, "projects", "-Users-tim-code-app", newID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 120<<10+1024 {
		t.Errorf("trimmed to %d bytes, want within the 120KB budget", len(out))
	}
	if !OversizedBy(src, 120<<10) || OversizedBy(src, 100<<20) {
		t.Error("OversizedBy does not honour its budget")
	}
}

// The new session id is 36 characters where the old one may be 7, and it is
// substituted into every record. The window on disk must still fit the
// budget, or the cap is not a cap.
func TestTrimmedTranscriptFitsTheBudgetAfterIDRewrite(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src", "projects", "p")
	os.MkdirAll(srcDir, 0o755)
	src := filepath.Join(srcDir, "abc.jsonl")
	var b strings.Builder
	pad := strings.Repeat("x", 500)
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, `{"type":"user","sessionId":"abc","message":{"role":"user","content":"%d %s"}}`+"\n", i, pad)
	}
	os.WriteFile(src, []byte(b.String()), 0o600)

	destCfg := t.TempDir()
	const budget = 64 << 10
	newID, err := importSessionWithBudget(src, destCfg, "abc", nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	if newID == "abc" {
		t.Fatal("an oversized import kept the old id")
	}
	matches, _ := filepath.Glob(filepath.Join(destCfg, "projects", "*", newID+".jsonl"))
	if len(matches) != 1 {
		t.Fatalf("trimmed transcript not found: %v", matches)
	}
	fi, _ := os.Stat(matches[0])
	if fi.Size() > budget {
		t.Errorf("trimmed transcript is %d bytes, budget %d", fi.Size(), budget)
	}
	if fi.Size() < budget*9/10 {
		t.Errorf("trimmed transcript is %d bytes; the trimmer left most of a %d budget unused", fi.Size(), budget)
	}
	data, _ := os.ReadFile(matches[0])
	if strings.Contains(string(data), `"abc"`) {
		t.Error("old session id survived the rewrite")
	}
}
