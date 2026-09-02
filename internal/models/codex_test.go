package models

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexConfigModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("# codex\nmodel_provider = \"openai\"\nmodel = \"gpt-5.6-sol\"\nmodel_reasoning_effort = \"max\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codexConfigModelAt(p); got != "gpt-5.6-sol" {
		t.Errorf("model = %q", got)
	}
	// A key that merely starts with "model" must not match.
	if err := os.WriteFile(p, []byte("model_provider = \"openai\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codexConfigModelAt(p); got != "" {
		t.Errorf("matched model_provider as model: %q", got)
	}
	if got := codexConfigModelAt(filepath.Join(dir, "missing.toml")); got != "" {
		t.Errorf("missing file gave %q", got)
	}
}

func TestIsClaudeName(t *testing.T) {
	for _, c := range []string{"sonnet", "Opus", "haiku", "claude-sonnet-5", "claude-opus-4-1-20250805"} {
		if !IsClaudeName(c) {
			t.Errorf("%q should read as a claude name", c)
		}
	}
	for _, c := range []string{"gpt-5.6-sol", "o3", "zai-org/GLM-5.3-Flash", ""} {
		if IsClaudeName(c) {
			t.Errorf("%q should not read as a claude name", c)
		}
	}
}

// The panel must never show "sonnet" against the codex harness, and roscoe must
// never pass a claude name to codex.
func TestCodexModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex/config.toml
	if m, pass := CodexModel("gpt-5.6-sol"); m != "gpt-5.6-sol" || !pass {
		t.Errorf("explicit codex model = (%q, %v), want passed through", m, pass)
	}
	if m, pass := CodexModel("sonnet"); pass || m != CodexDefault {
		t.Errorf("claude leftover = (%q, %v), want codex default and no flag", m, pass)
	}
	if m, pass := CodexModel(""); pass || m != CodexDefault {
		t.Errorf("empty = (%q, %v)", m, pass)
	}

	// With a config.toml present, the leftover falls back to codex's own model.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"gpt-5.6-sol\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, pass := CodexModel("sonnet"); pass || m != "gpt-5.6-sol" {
		t.Errorf("leftover with config = (%q, %v), want config model unpassed", m, pass)
	}
}
