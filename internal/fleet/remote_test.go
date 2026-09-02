package fleet

import (
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// The remote command is the whole contract of `run --node`: same roscoe run,
// same task id, a directory of its own, and the PATH that finds roscoe.
func TestRemoteRunCommand(t *testing.T) {
	cmd := RemoteRun(`fix the "billing" module; it's slow`, RemoteOpts{TaskID: "t-01"})
	for _, want := range []string{
		`export PATH="$HOME/.local/bin:`,
		`mkdir -p "$HOME/.roscoe/work/"'t-01' && cd "$HOME/.roscoe/work/"'t-01'`,
		`roscoe run 'fix the "billing" module; it'\''s slow' --task-id 't-01'`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %s:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--harness") {
		t.Error("harness passed when none was asked for; the node's config should decide")
	}

	// An existing directory on the node, and a harness override, pass through.
	cmd = RemoteRun("hi", RemoteOpts{TaskID: "t-02", Dir: "/Users/tim/proj", Harness: "codex"})
	if !strings.Contains(cmd, "cd '/Users/tim/proj' && roscoe run 'hi' --task-id 't-02' --harness 'codex'") {
		t.Errorf("explicit dir/harness:\n%s", cmd)
	}
	if strings.Contains(cmd, ".roscoe/work") {
		t.Error("a default work dir was created although --dir was given")
	}
}

func TestDisplayDirIsForPeople(t *testing.T) {
	if got := (RemoteOpts{TaskID: "t-01"}).DisplayDir(); got != "~/.roscoe/work/t-01" {
		t.Errorf("default = %q", got)
	}
	if got := (RemoteOpts{TaskID: "t-01", Dir: "/Users/tim/proj"}).DisplayDir(); got != "/Users/tim/proj" {
		t.Errorf("explicit = %q", got)
	}
}

// Interactive callers get a tty on the node so Esc and redirects work there;
// piped callers get -n so ssh does not eat their stdin.
func TestSSHArgsTTY(t *testing.T) {
	n := config.Node{Name: "roscoe", SSH: "roscoe-ts"}
	// Whole tokens: "-n" is also inside "accept-new" and "-t" inside "roscoe-ts".
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	args := SSHArgs(n, "cmd", true)
	a := strings.Join(args, " ")
	if !strings.HasSuffix(a, " -t roscoe-ts cmd") || has(args, "-n") {
		t.Errorf("tty args = %s", a)
	}
	args = SSHArgs(n, "cmd", false)
	a = strings.Join(args, " ")
	if !strings.HasSuffix(a, " -n roscoe-ts cmd") || has(args, "-t") {
		t.Errorf("piped args = %s", a)
	}
	if !strings.Contains(a, "BatchMode=yes") {
		t.Error("a missing key must fail fast, not prompt under a TUI")
	}
}
