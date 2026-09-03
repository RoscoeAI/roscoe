package main

// The fleet commands against a simulated node. The fake ssh and scp keep a
// node's state in a directory: which tools are installed, whether claude is
// logged in, the config and env files, the runs on it. Every command roscoe
// would send over ssh is logged, so a test can assert exactly what would
// have run on a real machine, and nothing ever leaves this one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNode is one simulated machine reachable by the fake ssh.
type fakeNode struct {
	w    *world
	ssh  string
	dir  string
	runs string
}

func (w *world) installFakeFleetTools() {
	w.t.Helper()
	w.fake("ssh", `#!/bin/sh
# Options first, then the host, then the one command.
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2;;
    -n|-t|-q) shift;;
    *) break;;
  esac
done
host="$1"; shift; cmd="$*"
echo "$host: $cmd" >> "$ROSCOE_E2E_SSH_LOG"
node="$FAKE_NODES/$host"
[ -d "$node" ] || { echo "ssh: Could not resolve hostname $host: nodename nor servname provided" >&2; exit 255; }
val() { [ -f "$node/$1" ] && cat "$node/$1" || echo "$2"; }
case "$cmd" in
  *'echo "host='*)
    echo "host=$host.local"; echo "arch=arm64"; echo "cores=$(val cores 8)"
    echo "claude=$(val claude missing)"; echo "roscoe=$(val roscoe missing)"
    echo "login=$(val login false)"; echo "busy=$(val busy 0)"
    echo "config=$(val config no)"; echo "env=$(val env no)"; exit 0;;
  *roscoe.sh/install*)
    ver=latest
    case "$cmd" in ROSCOE_VERSION=*) ver=$(printf '%s' "$cmd" | sed "s/^ROSCOE_VERSION='\([^']*\)'.*/\1/");; esac
    echo "roscoe $ver ($(uname -s))" > "$node/roscoe"; exit 0;;
  *claude.ai/install.sh*) echo "2.1.259 (Claude Code)" > "$node/claude"; exit 0;;
  *'mkdir -p "$HOME/.roscoe"'*) exit 0;;
  *'chmod 600'*) exit 0;;
  *'echo "roscoe=$('*) echo "roscoe=$(val roscoe missing)"; echo "claude=$(val claude missing)"; exit 0;;
  *'ls -1 "$HOME/.roscoe/runs"'*) ls -1 "$node/runs" 2>/dev/null; exit 0;;
  *'roscoe run '*)
    id=$(printf '%s' "$cmd" | sed "s/.*--task-id '\([^']*\)'.*/\1/")
    mkdir -p "$node/runs/$id" && cp "$FAKE_NODE_LEDGER" "$node/runs/$id/events.jsonl"
    echo "[remote $host] ran task $id"; exit 0;;
esac
echo "fake ssh: unhandled command: $cmd" >&2; exit 1
`)
	w.fake("scp", `#!/bin/sh
fetch=0
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2;;
    -q) shift;;
    -r) fetch=1; shift;;
    *) break;;
  esac
done
echo "scp: $*" >> "$ROSCOE_E2E_SSH_LOG"
if [ "$fetch" = 1 ]; then
  src="$1"; dst="$2"; host="${src%%:*}"; path="${src#*:}"
  [ -d "$FAKE_NODES/$host/${path#.roscoe/}" ] || { echo "scp: $path: No such file or directory" >&2; exit 1; }
  mkdir -p "$dst" && cp -R "$FAKE_NODES/$host/${path#.roscoe/}/." "$dst/"; exit 0
fi
src="$1"; dst="$2"; host="${dst%%:*}"; path="${dst#*:}"
[ -d "$FAKE_NODES/$host" ] || { echo "ssh: Could not resolve hostname $host" >&2; exit 1; }
case "$path" in
  *roscoe.json) cp "$src" "$FAKE_NODES/$host/roscoe.json" && echo yes > "$FAKE_NODES/$host/config";;
  *.env) cp "$src" "$FAKE_NODES/$host/dotenv" && echo yes > "$FAKE_NODES/$host/env";;
esac
exit 0
`)
}

// addNode configures nodes[0] as an ssh-reachable machine and creates its
// simulated state: claude installed and logged in, roscoe absent.
func (w *world) addNode(name, ssh string, workers int) *fakeNode {
	w.t.Helper()
	expect(w.t, w.run("", "config", "set", "nodes.0.name", name), 0)
	expect(w.t, w.run("", "config", "set", "nodes.0.ssh", ssh), 0)
	expect(w.t, w.run("", "config", "set", "nodes.0.workers", itoa(workers)), 0)
	n := &fakeNode{w: w, ssh: ssh, dir: filepath.Join(w.nodes, ssh)}
	n.runs = filepath.Join(n.dir, "runs")
	os.MkdirAll(n.runs, 0o755)
	n.set("claude", "2.1.259 (Claude Code)")
	n.set("login", "true")
	return n
}

func (n *fakeNode) set(key, val string) {
	if err := os.WriteFile(filepath.Join(n.dir, key), []byte(val+"\n"), 0o644); err != nil {
		n.w.t.Fatal(err)
	}
}

func (n *fakeNode) get(key string) string {
	return strings.TrimSpace(readFile(filepath.Join(n.dir, key)))
}

// sshLog is what roscoe asked the fleet to run, in order.
func (w *world) sshCommands() []string {
	var out []string
	for _, l := range strings.Split(readFile(w.sshLog), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func containsCmd(cmds []string, needle string) bool {
	for _, c := range cmds {
		if strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

func TestE2EFleetProbeAndDeploy(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.installFakeFleetTools()
	n := w.addNode("studio", "studio-ts", 2)

	// Before deploy: reachable, missing roscoe and its config.
	r := w.run("", "node")
	expect(t, r, 0, "studio", "studio-ts", "needs roscoe, config", "0 of 1 ready", "roscoe deploy")
	if !containsCmd(w.sshCommands(), `studio-ts: `) || !containsCmd(w.sshCommands(), `echo "host=`) {
		t.Fatalf("no probe over ssh:\n%s", strings.Join(w.sshCommands(), "\n"))
	}

	expect(t, w.run("", "deploy", "--node", "nobody"), 2, `no enabled node named "nobody"`)

	// Deploy pins this binary's version and pushes the config.
	r = w.run("", "deploy")
	expect(t, r, 0, "[deploy] roscoe v0.0.0-e2e + ", "to 1 node(s)", "studio (studio-ts)", " ok in ", "roscoe v0.0.0-e2e", "claude 2.1.259", "claude auth login")
	cmds := w.sshCommands()
	if !containsCmd(cmds, "ROSCOE_VERSION='v0.0.0-e2e' curl -fsSL https://roscoe.sh/install") {
		t.Errorf("install was not pinned to this version:\n%s", strings.Join(cmds, "\n"))
	}
	if !containsCmd(cmds, "scp: ") || !strings.Contains(readFile(filepath.Join(n.dir, "roscoe.json")), `"tiers"`) {
		t.Error("roscoe.json was not pushed to the node")
	}
	if containsCmd(cmds, "claude.ai/install.sh") || containsCmd(cmds, "chmod 600") {
		t.Errorf("deploy touched claude or the env file without being asked:\n%s", strings.Join(cmds, "\n"))
	}
	if n.get("roscoe") != "roscoe v0.0.0-e2e (Linux)" && n.get("roscoe") != "roscoe v0.0.0-e2e (Darwin)" {
		t.Errorf("node now runs %q", n.get("roscoe"))
	}

	// After deploy: ready.
	r = w.run("", "node")
	expect(t, r, 0, "ready", "1 of 1 ready")
	if strings.Contains(r.stdout, "needs ") {
		t.Errorf("still needs something:\n%s", r.stdout)
	}

	// --env pushes the secrets file and locks it down; --claude installs claude.
	os.WriteFile(filepath.Join(w.home, ".roscoe", ".env"), []byte("DEEP_INFRA_API_KEY=dk\n"), 0o600)
	n.set("claude", "missing")
	expect(t, w.run("", "up", "--env", "--claude"), 0, "[deploy] done", "1 of 1 ready")
	cmds = w.sshCommands()
	if !containsCmd(cmds, "chmod 600") || n.get("env") != "yes" || !containsCmd(cmds, "claude.ai/install.sh") {
		t.Errorf("--env/--claude steps missing:\n%s", strings.Join(cmds, "\n"))
	}

	// An unreachable node says why, a disabled one says so, neither is ready.
	expect(t, w.run("", "config", "set", "nodes.0.ssh", "ghost"), 0)
	expect(t, w.run("", "node"), 0, "ghost", "0 of 1 ready")
	expect(t, w.run("", "config", "set", "nodes.0.enabled", "false"), 0)
	expect(t, w.run("", "node"), 0, "disabled")
	expect(t, w.run("", "deploy"), 2, "no enabled nodes with an ssh alias")
}

// A real local run's ledger, used as what the remote roscoe would write.
func (w *world) ledgerTemplate() string {
	w.t.Helper()
	expect(w.t, w.run("", "run", "seed the ledger", "--task-id", "seed"), 0)
	return filepath.Join(w.home, ".roscoe", "runs", "seed", "events.jsonl")
}

func TestE2EFleetDispatchAndBringHome(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.installFakeFleetTools()
	n := w.addNode("studio", "studio-ts", 2)
	n.set("roscoe", "roscoe v0.0.0-e2e (Darwin)")
	n.set("config", "yes")
	w.ledger = w.ledgerTemplate()

	r := w.run("", "dispatch", "do the thing", "--task-id", "t-9")
	expect(t, r, 0, "[node] studio (studio-ts)", "task t-9", "dir ~/.roscoe/work/t-9", "[remote studio-ts] ran task t-9", "[home] ledger for t-9 is here")
	cmds := w.sshCommands()
	if !containsCmd(cmds, `roscoe run 'do the thing' --task-id 't-9'`) {
		t.Errorf("remote run command wrong:\n%s", strings.Join(cmds, "\n"))
	}
	home := filepath.Join(w.home, ".roscoe", "runs", "t-9", "events.jsonl")
	if !strings.Contains(readFile(home), `"kind":"fleet.home"`) || !strings.Contains(readFile(home), `"node":"studio"`) {
		t.Errorf("the brought-home ledger is not tagged with its node:\n%s", readFile(home))
	}
	if len(w.claudeStarts()) != 1 { // only the seed run started a worker here
		t.Errorf("dispatch ran a worker on this machine: %d starts", len(w.claudeStarts()))
	}
	// Both the seed and the fleet run are on the books.
	sess := w.run("", "sessions")
	expect(t, sess, 0, "studio")
	if strings.Count(sess.stdout, "just now") != 2 {
		t.Errorf("want 2 session rows:\n%s", sess.stdout)
	}

	// run --node picks the node by name; --harness reaches the remote command.
	expect(t, w.run("", "run", "--node", "studio", "--harness", "codex", "again", "--task-id", "t-10"), 0, "[node] studio", "ran task t-10")
	if !containsCmd(w.sshCommands(), `--task-id 't-10' --harness 'codex'`) {
		t.Errorf("harness not forwarded:\n%s", strings.Join(w.sshCommands(), "\n"))
	}

	// Runs that happened on the node without us are fetched by sessions --node.
	os.MkdirAll(filepath.Join(n.runs, "t-old"), 0o755)
	copyFile(t, w.ledger, filepath.Join(n.runs, "t-old", "events.jsonl"))
	expect(t, w.run("", "sessions", "--node", "studio"), 0)
	if _, err := os.Stat(filepath.Join(w.home, ".roscoe", "runs", "t-old", "events.jsonl")); err != nil {
		t.Error("sessions --node did not bring the node's other run home")
	}

	// Not ready: no worker is started and the reason names the fix.
	n.set("login", "false")
	expect(t, w.run("", "dispatch", "p"), 1, "no node can take work right now", "needs login", "ssh -t studio-ts claude auth login")
	expect(t, w.run("", "run", "--node", "studio", "p"), 1, "studio is not ready: needs login")
	n.set("login", "true")
	n.set("busy", "2") // both worker slots taken
	expect(t, w.run("", "dispatch", "p"), 1, "at its worker limit")
	if containsCmd(w.sshCommands(), "--task-id 'p'") || containsCmd(w.sshCommands(), `roscoe run 'p'`) {
		t.Error("a refused dispatch still ran the task")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
