package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// fakeFleet answers ssh per host, records every command and copy, and can be
// slow or broken per host. No network anywhere.
type fakeFleet struct {
	mu      sync.Mutex
	answers map[string]string // host -> probe output
	down    map[string]bool
	delay   time.Duration
	cmds    map[string][]string
	copies  []string
	failCmd string // a command substring that fails when run
	verify  string // what the post-deploy verify reports; default is a healthy node
}

func (f *fakeFleet) run(ctx context.Context, host, cmd string) (string, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmds == nil {
		f.cmds = map[string][]string{}
	}
	f.cmds[host] = append(f.cmds[host], cmd)
	if f.down[host] {
		return "", errors.New("ssh: connect to host: No route to host")
	}
	if f.failCmd != "" && strings.Contains(cmd, f.failCmd) {
		return "", errors.New("command failed")
	}
	if strings.Contains(cmd, `echo "roscoe=$(command -v roscoe`) && !strings.Contains(cmd, "host=") {
		if f.verify != "" {
			return f.verify, nil
		}
		return "roscoe=roscoe v0.28.0 (go1.26.7)\nclaude=2.1.251 (Claude Code)\n", nil
	}
	return f.answers[host], nil
}

func (f *fakeFleet) copy(ctx context.Context, host, local, remote string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies = append(f.copies, host+":"+remote+"<-"+local)
	return nil
}

const blankNode = "host=roscoe\narch=arm64\ncores=28\nclaude=missing\nroscoe=missing\nconfig=no\nenv=no\n"
const readyNode = "host=roscoe-2tb\narch=arm64\ncores=28\nclaude=2.1.251 (Claude Code)\nroscoe=roscoe v0.28.0 (go1.26.7)\nlogin=true\nconfig=yes\nenv=yes\n"

// Deployed but never logged in: what every node looks like right after
// `roscoe deploy --claude`. Ready must be false with "login" as the reason.
const deployedNode = "host=roscoe\narch=arm64\ncores=28\nclaude=2.1.251 (Claude Code)\nroscoe=roscoe v0.28.0 (go1.26.7)\nlogin=\nconfig=yes\nenv=yes\n"

func nodes() []config.Node {
	return []config.Node{
		{Name: "roscoe", SSH: "roscoe-ts", Workers: 2, Enabled: true},
		{Name: "roscoe-2tb", SSH: "roscoe-2tb-ts", Workers: 2, Enabled: true},
		{Name: "laptop", SSH: "", Workers: 1, Enabled: false},
	}
}

// The first live probe of the real fleet: reachable, 28 cores, nothing on it.
// That reading must come through as "needs roscoe, config, claude, env".
func TestProbeReadsWhatIsThere(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{"roscoe-ts": blankNode, "roscoe-2tb-ts": readyNode}}
	ps := ProbeAll(context.Background(), nodes(), f.run)
	if len(ps) != 3 {
		t.Fatalf("got %d probes", len(ps))
	}
	blank := ps[0]
	if !blank.Reachable || blank.Cores != 28 || blank.Arch != "arm64" || blank.Host != "roscoe" {
		t.Errorf("blank = %+v", blank)
	}
	if blank.Ready() {
		t.Error("a node with nothing installed reported ready")
	}
	if got := strings.Join(blank.Missing(), ","); got != "roscoe,config,claude,env" {
		t.Errorf("missing = %q; order should match what deploy fixes first", got)
	}
	ready := ps[1]
	if !ready.Ready() || len(ready.Missing()) != 0 {
		t.Errorf("ready node = %+v missing=%v", ready, ready.Missing())
	}
	if ready.Roscoe != "roscoe v0.28.0 (go1.26.7)" || !ready.HasEnv {
		t.Errorf("fields = %+v", ready)
	}
}

func TestProbeDeployedButNotLoggedIn(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{"roscoe-ts": deployedNode}}
	p := ProbeAll(context.Background(), nodes()[:1], f.run)[0]
	if p.Ready() || p.LoggedIn {
		t.Errorf("a node with no login reported ready: %+v", p)
	}
	if got := strings.Join(p.Missing(), ","); got != "login" {
		t.Errorf("missing = %q, want just login", got)
	}
	// The probe must not hang on a claude that wants a terminal.
	if !strings.Contains(probeScript, "claude auth status 2>/dev/null </dev/null") {
		t.Error("auth status probe is not detached from stdin")
	}
}

// Disabled nodes are listed but never contacted; unreachable ones say so.
func TestProbeSkipsDisabledAndReportsDown(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{"roscoe-2tb-ts": readyNode}, down: map[string]bool{"roscoe-ts": true}}
	ps := ProbeAll(context.Background(), nodes(), f.run)
	if ps[0].Reachable || ps[0].Err == nil || strings.Join(ps[0].Missing(), "") != "unreachable" {
		t.Errorf("down node = %+v", ps[0])
	}
	if ps[2].Reachable || ps[2].Err != nil {
		t.Errorf("disabled node should be untouched: %+v", ps[2])
	}
	if _, contacted := f.cmds["laptop"]; contacted {
		t.Error("a disabled node was contacted")
	}
	if _, contacted := f.cmds[""]; contacted {
		t.Error("an empty ssh alias was contacted")
	}
}

// A fleet listing must take as long as the slowest node, not the sum.
func TestProbeIsParallel(t *testing.T) {
	f := &fakeFleet{delay: 150 * time.Millisecond, answers: map[string]string{"roscoe-ts": blankNode, "roscoe-2tb-ts": readyNode}}
	start := time.Now()
	ProbeAll(context.Background(), nodes(), f.run)
	if el := time.Since(start); el > 350*time.Millisecond {
		t.Errorf("two 150ms probes took %s; they should overlap", el)
	}
}

func TestDeployPinsPushesAndVerifies(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{}}
	r := Deploy(context.Background(), nodes()[0], DeployOpts{
		Version: "v0.28.0", ConfigPath: "/l/roscoe.json", EnvPath: "/l/.env", Claude: true,
	}, f.run, f.copy)
	if r.Err != nil {
		t.Fatalf("deploy: %v (steps %v)", r.Err, r.Steps)
	}
	cmds := strings.Join(f.cmds["roscoe-ts"], "\n")
	if !strings.Contains(cmds, "ROSCOE_VERSION='v0.28.0' curl -fsSL https://roscoe.sh/install -o /tmp/roscoe-install.sh && sh /tmp/roscoe-install.sh") {
		t.Errorf("install not pinned, or a curl failure could hide behind sh's exit code:\n%s", cmds)
	}
	// Every remote command must see the user-level bin dirs, or an installed
	// binary reads as missing (the first live deploy did exactly that).
	for _, c := range f.cmds["roscoe-ts"] {
		if strings.Contains(c, "command -v") && !strings.Contains(c, `PATH="$HOME/.local/bin:`) {
			t.Errorf("remote command looks for binaries without the user PATH: %s", c)
		}
	}
	if !strings.Contains(cmds, "claude.ai/install.sh") {
		t.Error("claude install requested but not run")
	}
	if !strings.Contains(cmds, "chmod 600") {
		t.Error("pushed env file was not locked down")
	}
	want := []string{"roscoe-ts:~/.roscoe/roscoe.json<-/l/roscoe.json", "roscoe-ts:~/.roscoe/.env<-/l/.env"}
	if strings.Join(f.copies, "|") != strings.Join(want, "|") {
		t.Errorf("copies = %v", f.copies)
	}
	if r.Roscoe != "roscoe v0.28.0 (go1.26.7)" || r.Claude != "2.1.251 (Claude Code)" {
		t.Errorf("verify parsed %q / %q", r.Roscoe, r.Claude)
	}
	if got := strings.Join(r.Steps, ","); got != "install roscoe v0.28.0,create ~/.roscoe,push roscoe.json,chmod env,push .env,install claude,verify" {
		t.Errorf("steps = %q", got)
	}
}

// Without --env and --claude, neither happens: one is secrets, the other a
// bigger footprint, and both are the operator's call.
func TestDeployDefaultsAreMinimal(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{}}
	r := Deploy(context.Background(), nodes()[0], DeployOpts{ConfigPath: "/l/roscoe.json"}, f.run, f.copy)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	cmds := strings.Join(f.cmds["roscoe-ts"], "\n")
	if strings.Contains(cmds, "claude.ai") || strings.Contains(cmds, "ROSCOE_VERSION=") {
		t.Errorf("defaults did too much:\n%s", cmds)
	}
	if len(f.copies) != 1 || !strings.HasSuffix(f.copies[0], "roscoe.json<-/l/roscoe.json") {
		t.Errorf("copies = %v", f.copies)
	}
	if r.Steps[0] != "install roscoe (latest)" {
		t.Errorf("unpinned install should say so: %q", r.Steps[0])
	}
}

// The first failure stops the sequence and the result says how far it got,
// so a retry knows what is already on the machine.
func TestDeployStopsAtFirstFailure(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{}, failCmd: "mkdir -p"}
	r := Deploy(context.Background(), nodes()[0], DeployOpts{Version: "v1", ConfigPath: "/l/c.json"}, f.run, f.copy)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "create ~/.roscoe") {
		t.Errorf("err = %v", r.Err)
	}
	if strings.Join(r.Steps, ",") != "install roscoe v1" {
		t.Errorf("steps = %v; only the install should have completed", r.Steps)
	}
	if len(f.copies) != 0 {
		t.Error("config was pushed after an earlier step failed")
	}
}

// The first live deploy installed roscoe, could not see it, and said ok.
// Verify has to turn "left nothing behind" and "wrong version" into errors.
func TestDeployVerifyRefusesToLie(t *testing.T) {
	cases := map[string]struct{ verify, opts, wantErr string }{
		"nothing installed":    {"roscoe=missing\nclaude=missing\n", "", "not on the node's PATH"},
		"wrong version":        {"roscoe=roscoe v0.27.0 (go1.26.7)\nclaude=missing\n", "pin", "asked for roscoe v0.28.0"},
		"claude asked, absent": {"roscoe=roscoe v0.28.0 (go1.26.7)\nclaude=missing\n", "claude", "claude is not on the node's PATH"},
	}
	for name, tc := range cases {
		f := &fakeFleet{answers: map[string]string{}, verify: tc.verify}
		o := DeployOpts{ConfigPath: "/l/c.json"}
		if tc.opts == "pin" {
			o.Version = "v0.28.0"
		}
		if tc.opts == "claude" {
			o.Claude = true
		}
		r := Deploy(context.Background(), nodes()[0], o, f.run, f.copy)
		if r.Err == nil || !strings.Contains(r.Err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want %q", name, r.Err, tc.wantErr)
		}
		if contains(r.Steps, "verify") {
			t.Errorf("%s: verify counted as a completed step", name)
		}
	}
	// A claude that was not asked for is allowed to be missing.
	f := &fakeFleet{answers: map[string]string{}, verify: "roscoe=roscoe v0.28.0 (go1.26.7)\nclaude=missing\n"}
	if r := Deploy(context.Background(), nodes()[0], DeployOpts{Version: "v0.28.0"}, f.run, f.copy); r.Err != nil {
		t.Errorf("missing claude failed a roscoe-only deploy: %v", r.Err)
	}
	// The probe looks in the same places, so a node the deploy could see is a
	// node the table can see.
	if !strings.HasPrefix(probeScript, userPath) {
		t.Error("probe does not set the user PATH")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestDeployReportsHowLongItTook(t *testing.T) {
	f := &fakeFleet{answers: map[string]string{}, delay: 20 * time.Millisecond}
	r := Deploy(context.Background(), nodes()[0], DeployOpts{}, f.run, f.copy)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if r.Elapsed < 40*time.Millisecond { // at least install + mkdir + verify
		t.Errorf("elapsed = %s for three 20ms steps; the timer is not reaching the result", r.Elapsed)
	}
}

func TestEnabledAndQuoting(t *testing.T) {
	en := Enabled(nodes())
	if len(en) != 2 || en[0].Name != "roscoe" || en[1].Name != "roscoe-2tb" {
		t.Errorf("enabled = %v", en)
	}
	if got := shellQuote("v0.28.0"); got != "'v0.28.0'" {
		t.Errorf("quote = %s", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("quote with apostrophe = %s", got)
	}
}
