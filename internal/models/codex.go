package models

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Codex needs no alias resolution: you hand it a concrete id. What it needs is
// the opposite, a way to say which model it will actually run, because
// `codex exec --json` emits no model field in any event (verified: the stream
// is thread.started, turn.started, item.completed, turn.completed and nothing
// names the model). The answer is deterministic instead: the -m flag roscoe
// passes, else the `model` key in ~/.codex/config.toml, else codex's own
// built-in default, which it does not disclose.

// CodexDefault is what to report when neither roscoe nor config.toml names a
// model. Honest, rather than a guess at codex's current built-in.
const CodexDefault = "codex default (unset)"

var tomlModelRE = regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)

// CodexConfigModel reads the model codex is configured to run from its own
// config file. Empty when there is none.
func CodexConfigModel() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return codexConfigModelAt(filepath.Join(home, ".codex", "config.toml"))
}

func codexConfigModelAt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := tomlModelRE.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// IsClaudeName reports whether a model name belongs to the claude harness: an
// alias like "sonnet" or an id like "claude-opus-5". Under the codex harness
// such a value in tiers.middle.model is a leftover from the claude default and
// must not be passed to codex, which would reject it.
func IsClaudeName(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch m {
	case "opus", "sonnet", "haiku":
		return true
	}
	return strings.HasPrefix(m, "claude-")
}

// CodexModel is the model a codex worker will run given what roscoe has
// configured. It returns the model and whether roscoe should pass it as -m.
// A claude name is never passed: codex falls back to its own config, and the
// panel says so instead of showing "sonnet" against a harness that has never
// heard of it.
func CodexModel(configured string) (model string, passFlag bool) {
	c := strings.TrimSpace(configured)
	if c != "" && !IsClaudeName(c) {
		return c, true
	}
	if m := CodexConfigModel(); m != "" {
		return m, false
	}
	return CodexDefault, false
}
