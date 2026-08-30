package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// codexStub writes a stub codex binary that records its argv, honors -o by
// writing the final message there, and emits one codex-style JSONL event.
func codexStub(t *testing.T, dir string) (bin, argvFile string) {
	t.Helper()
	argvFile = filepath.Join(dir, "argv.txt")
	bin = filepath.Join(dir, "codex")
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  printf '%s\n' "$a" >> "` + argvFile + `"
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf '{"type":"thread.started","thread_id":"th_123"}\n'
if [ -n "$out" ]; then printf 'pong' > "$out"; fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvFile
}

func codexCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Tiers.Middle.Harness = "codex"
	cfg.Tiers.Middle.PermissionMode = "bypassPermissions"
	return cfg
}

func TestRunCodexHarness(t *testing.T) {
	dir := t.TempDir()
	bin, argvFile := codexStub(t, dir)
	cfg := codexCfg(t)
	taskDir := t.TempDir()

	res, err := Run(context.Background(), Task{ID: "codex-1", Prompt: "say pong", Dir: taskDir}, Opts{
		Cfg:        cfg,
		RouterAddr: "127.0.0.1:1", // unused by codex; must not be required
		CodexBin:   bin,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Result != "pong" {
		t.Fatalf("Result = %q, want pong", res.Result)
	}
	if res.IsError {
		t.Fatal("IsError = true, want false")
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(argv)), "\n")
	joined := strings.Join(got, " ")
	for _, want := range []string{"exec", "--json", "--skip-git-repo-check", "-o", "--dangerously-bypass-approvals-and-sandbox", "-C", taskDir} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, got)
		}
	}
	if got[len(got)-1] != "say pong" {
		t.Errorf("prompt should be the final arg, got %q", got[len(got)-1])
	}
	for _, forbidden := range []string{"--session-id", "--agents", "--model"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("claude-only flag %q leaked into codex argv", forbidden)
		}
	}
}

func TestRunCodexResumeUnsupported(t *testing.T) {
	dir := t.TempDir()
	bin, _ := codexStub(t, dir)
	cfg := codexCfg(t)

	_, err := Run(context.Background(), Task{ID: "codex-2", Prompt: "x", Resume: "abc"}, Opts{Cfg: cfg, CodexBin: bin})
	if err == nil || !strings.Contains(err.Error(), "not supported for the codex harness") {
		t.Fatalf("want codex resume error, got %v", err)
	}
}

func TestRunCodexNonBypassSandbox(t *testing.T) {
	dir := t.TempDir()
	bin, argvFile := codexStub(t, dir)
	cfg := codexCfg(t)
	cfg.Tiers.Middle.PermissionMode = "acceptEdits"

	if _, err := Run(context.Background(), Task{ID: "codex-3", Prompt: "x"}, Opts{Cfg: cfg, CodexBin: bin}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	joined := strings.ReplaceAll(strings.TrimSpace(string(argv)), "\n", " ")
	if !strings.Contains(joined, "-s workspace-write") || !strings.Contains(joined, "--approve-for-me") {
		t.Errorf("non-bypass mode should map to sandboxed approvals, got %s", joined)
	}
	if strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("bypass flag present in non-bypass mode: %s", joined)
	}
}
