package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

	if err := importSession(src, destCfg); err != nil {
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

	if err := importSession(src, destCfg); err != nil {
		t.Fatalf("first importSession: %v", err)
	}
	// Change the source; a re-import must be a no-op, not an overwrite.
	if err := os.WriteFile(src, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importSession(src, destCfg); err != nil {
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

	err := importSession(src, destCfg)
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
