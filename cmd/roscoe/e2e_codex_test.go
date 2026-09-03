package main

// The codex harness: a worker that is `codex exec`, with its own auth and
// model, no router and no swarm, and its answer in the -o file.

import (
	"path/filepath"
	"strings"
	"testing"
)

func (w *world) installFakeCodex() string {
	log := filepath.Join(filepath.Dir(w.bin), "codex-argv")
	w.fake("codex", `#!/bin/sh
printf '%s ' "$@" | tr '\n' ' ' >> "`+log+`"; echo >> "`+log+`"
if [ "$FAKE_CODEX_MODE" = fail ]; then echo "codex: something broke" >&2; exit 1; fi
out=""; last=""; prompt=""
for a in "$@"; do
  case "$last" in -o) out="$a";; esac
  last="$a"; prompt="$a"
done
echo '{"type":"thread.started","thread_id":"th_1"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"working on it"}}'
[ -n "$out" ] && printf 'codex answer to: %s\n' "$prompt" > "$out"
exit 0
`)
	return log
}

func TestE2ERunCodexHarness(t *testing.T) {
	w := newWorld(t)
	w.init()
	log := w.installFakeCodex()

	r := w.run("", "run", "--harness", "codex", "say hi", "--task-id", "c-1")
	expect(t, r, 0, "codex answer to: say hi")
	if strings.TrimSpace(r.stdout) != "codex answer to: say hi" {
		t.Errorf("stdout should be the answer alone:\n%q", r.stdout)
	}
	if n := len(w.claudeStarts()); n != 0 {
		t.Errorf("claude started %d times under the codex harness", n)
	}
	argv := strings.TrimSpace(readFile(log))
	for _, want := range []string{"exec --json --skip-git-repo-check -o ", "-C " + w.cwd, "--dangerously-bypass-approvals-and-sandbox", " say hi"} {
		if !strings.Contains(argv, want) {
			t.Errorf("codex argv lacks %q:\n%s", want, argv)
		}
	}
	// tiers.middle.model is a claude alias by default: never handed to codex.
	if strings.Contains(argv, " -m ") {
		t.Errorf("a claude model name was passed to codex:\n%s", argv)
	}

	// A codex model in the config is passed through, and models says so.
	expect(t, w.run("", "config", "set", "tiers.middle.harness", "codex"), 0)
	expect(t, w.run("", "config", "set", "tiers.middle.model", "gpt-5.6-sol"), 0)
	expect(t, w.run("", "models"), 0, "tier 2  gpt-5.6-sol  (codex)")
	expect(t, w.run("", "run", "again"), 0, "codex answer to: again")
	lines := strings.Split(strings.TrimSpace(readFile(log)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], " -m gpt-5.6-sol ") {
		t.Errorf("configured codex model not passed:\n%s", strings.Join(lines, "\n"))
	}

	// Resume is a claude feature; codex says so up front, without starting.
	r = w.run("", "run", "--resume", "abc", "go on")
	expect(t, r, 2, "--resume is not supported for the codex harness")
	if strings.Contains(r.all(), "session abc not found") {
		t.Errorf("the session was looked up before the harness was checked:\n%s", r.stderr)
	}
	if n := len(strings.Split(strings.TrimSpace(readFile(log)), "\n")); n != 2 {
		t.Errorf("codex was started for an unsupported resume (%d starts)", n)
	}

	// A codex that dies is reported as such.
	w.claudeMode = "fail" // unused by codex; the codex fake has its own switch
	r = w.runEnv([]string{"FAKE_CODEX_MODE=fail"}, "run", "doomed")
	if r.code == 0 || !strings.Contains(r.all(), "codex failed without result") || !strings.Contains(r.all(), "something broke") {
		t.Errorf("a dead codex: %s", r)
	}
	if strings.Contains(r.stdout, "codex answer to") {
		t.Errorf("a dead codex handed back an earlier run's answer:\n%s", r.stdout)
	}
}
