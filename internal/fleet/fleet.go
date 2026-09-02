// Package fleet reaches the machines in nodes[]: what is on them, and putting
// roscoe there. Everything goes over the operator's own ssh, using their
// aliases and keys; roscoe adds no daemon, no port, no agent of its own.
//
// The two operations here are the ones the fleet needs before it can run a
// single task elsewhere. Probe answers "can this machine do work", which the
// first live check answered with: reachable, 28 cores, and nothing installed.
// Deploy fixes that, pinned to the control plane's own version, because a
// fleet where machines disagree about what roscoe is repeats the stale-binary
// failure once per node.
package fleet

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Runner executes a shell command on a host and returns its combined output.
// The real one is ssh; tests inject a fake so nothing here needs a network.
type Runner func(ctx context.Context, host, command string) (string, error)

// Copier copies a local file to host:remotePath. The real one is scp.
type Copier func(ctx context.Context, host, localPath, remotePath string) error

// probeTimeout bounds one node. A machine that is off should cost the listing
// a few seconds, not hang it.
const probeTimeout = 15 * time.Second

// userPath is prepended to PATH in every remote command. A non-interactive
// ssh shell gets the system PATH only, and that is not where the claude and
// roscoe installers put their binaries: the first live deploy installed roscoe
// and then reported it missing, and called a node with claude "missing" too.
const userPath = `export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; `

// probeScript prints one key=value per line so the parser is trivial and a
// partially failing probe still yields the fields that worked.
const probeScript = userPath + `echo "host=$(hostname 2>/dev/null)"; ` +
	`echo "arch=$(uname -m 2>/dev/null)"; ` +
	`echo "cores=$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null)"; ` +
	`echo "claude=$(command -v claude >/dev/null 2>&1 && claude --version 2>/dev/null | head -1 || echo missing)"; ` +
	`echo "roscoe=$(command -v roscoe >/dev/null 2>&1 && roscoe version 2>/dev/null || echo missing)"; ` +
	`echo "login=$(command -v claude >/dev/null 2>&1 && claude auth status 2>/dev/null </dev/null | tr -d ' \n' | grep -o '"loggedIn":[a-z]*' | cut -d: -f2)"; ` +
	`echo "config=$([ -f "$HOME/.roscoe/roscoe.json" ] && echo yes || echo no)"; ` +
	`echo "env=$([ -f "$HOME/.roscoe/.env" ] && echo yes || echo no)"`

// Probe is what one node reported.
type Probe struct {
	Node      config.Node
	Reachable bool
	Host      string
	Arch      string
	Cores     int
	Claude    string // version, "missing", or "" when unreachable
	LoggedIn  bool   // claude auth status said loggedIn:true
	Roscoe    string
	HasConfig bool
	HasEnv    bool
	Err       error
	Elapsed   time.Duration
}

// Ready reports whether the node could run a worker right now: reachable,
// with roscoe and a config present, and claude installed and logged in.
// Deploy gets a node everything but the login; that is a credential, and it
// is done on the node (ssh <alias> claude auth login) or by the account vault.
func (p Probe) Ready() bool {
	return p.Reachable && p.hasClaude() && p.LoggedIn &&
		p.Roscoe != "" && p.Roscoe != "missing" && p.HasConfig
}

func (p Probe) hasClaude() bool { return p.Claude != "" && p.Claude != "missing" }

// Missing lists what stops the node being Ready, in the order deploy would
// fix it.
func (p Probe) Missing() []string {
	if !p.Reachable {
		return []string{"unreachable"}
	}
	var m []string
	if p.Roscoe == "" || p.Roscoe == "missing" {
		m = append(m, "roscoe")
	}
	if !p.HasConfig {
		m = append(m, "config")
	}
	if !p.hasClaude() {
		m = append(m, "claude")
	} else if !p.LoggedIn {
		m = append(m, "login")
	}
	if !p.HasEnv {
		m = append(m, "env")
	}
	return m
}

// ProbeAll probes every node in parallel. Disabled nodes are reported but not
// contacted, so the listing still shows the whole configured fleet.
func ProbeAll(ctx context.Context, nodes []config.Node, run Runner) []Probe {
	out := make([]Probe, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		out[i] = Probe{Node: n}
		if !n.Enabled || n.SSH == "" {
			continue
		}
		wg.Add(1)
		go func(i int, n config.Node) {
			defer wg.Done()
			out[i] = probeOne(ctx, n, run)
		}(i, n)
	}
	wg.Wait()
	return out
}

func probeOne(ctx context.Context, n config.Node, run Runner) Probe {
	p := Probe{Node: n}
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := run(pctx, n.SSH, probeScript)
	p.Elapsed = time.Since(start)
	if err != nil && strings.TrimSpace(out) == "" {
		p.Err = err
		return p
	}
	p.Reachable = true
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "host":
			p.Host = v
		case "arch":
			p.Arch = v
		case "cores":
			p.Cores, _ = strconv.Atoi(v)
		case "claude":
			p.Claude = v
		case "roscoe":
			p.Roscoe = v
		case "login":
			p.LoggedIn = v == "true"
		case "config":
			p.HasConfig = v == "yes"
		case "env":
			p.HasEnv = v == "yes"
		}
	}
	return p
}

// DeployOpts is what to put on a node.
type DeployOpts struct {
	// Version pins the roscoe release to install, e.g. "v0.28.0". Empty means
	// latest, which is the wrong default for a fleet, so callers pass the
	// control plane's own version.
	Version string
	// ConfigPath is the local roscoe.json to push to ~/.roscoe/roscoe.json.
	ConfigPath string
	// EnvPath, when set, pushes the env file (API keys) to ~/.roscoe/.env.
	// Off unless asked: it is a secrets file.
	EnvPath string
	// Claude installs Claude Code too, via its own installer. Login still has
	// to happen per node; that is a credential, not a deploy step.
	Claude bool
}

// DeployResult is what happened on one node.
type DeployResult struct {
	Node    config.Node
	Steps   []string // what was done, in order
	Roscoe  string   // version reported afterwards
	Claude  string
	Err     error
	Elapsed time.Duration
}

const deployTimeout = 4 * time.Minute

// Deploy puts roscoe (and optionally claude) on one node and verifies what is
// there afterwards. Each step is one ssh or scp; the first failure stops the
// sequence, and whatever succeeded is reported so a retry knows where it got.
func Deploy(ctx context.Context, n config.Node, o DeployOpts, run Runner, copy Copier) DeployResult {
	r := DeployResult{Node: n}
	start := time.Now()
	defer func() { r.Elapsed = time.Since(start) }()
	dctx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()

	step := func(name, cmd string) bool {
		if _, err := run(dctx, n.SSH, cmd); err != nil {
			r.Err = fmt.Errorf("%s: %w", name, err)
			return false
		}
		r.Steps = append(r.Steps, name)
		return true
	}

	// roscoe itself, pinned. ROSCOE_VERSION is honoured by install.sh.
	inst := "curl -fsSL https://roscoe.sh/install -o /tmp/roscoe-install.sh && sh /tmp/roscoe-install.sh"
	if o.Version != "" {
		inst = "ROSCOE_VERSION=" + shellQuote(o.Version) + " " + inst
	}
	if !step("install roscoe "+orLatest(o.Version), inst) {
		return r
	}
	if !step("create ~/.roscoe", "mkdir -p \"$HOME/.roscoe\"") {
		return r
	}
	if o.ConfigPath != "" {
		if err := copy(dctx, n.SSH, o.ConfigPath, "~/.roscoe/roscoe.json"); err != nil {
			r.Err = fmt.Errorf("push config: %w", err)
			return r
		}
		r.Steps = append(r.Steps, "push roscoe.json")
	}
	if o.EnvPath != "" {
		if err := copy(dctx, n.SSH, o.EnvPath, "~/.roscoe/.env"); err != nil {
			r.Err = fmt.Errorf("push env: %w", err)
			return r
		}
		// Secrets: make sure they are not world-readable where they land.
		if !step("chmod env", "chmod 600 \"$HOME/.roscoe/.env\"") {
			return r
		}
		r.Steps = append(r.Steps, "push .env")
	}
	if o.Claude {
		if !step("install claude", "curl -fsSL https://claude.ai/install.sh | sh") {
			return r
		}
	}

	// Verify: the whole point of pinning is that this line matches everywhere.
	out, err := run(dctx, n.SSH, userPath+
		`echo "roscoe=$(command -v roscoe >/dev/null 2>&1 && roscoe version 2>/dev/null || echo missing)"; `+
		`echo "claude=$(command -v claude >/dev/null 2>&1 && claude --version 2>/dev/null | head -1 || echo missing)"`)
	if err != nil {
		r.Err = fmt.Errorf("verify: %w", err)
		return r
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "roscoe":
			r.Roscoe = strings.TrimSpace(v)
		case "claude":
			r.Claude = strings.TrimSpace(v)
		}
	}
	// An installer that printed nothing wrong and left nothing behind is still
	// a failed deploy; saying "ok" here is what let the first one lie.
	if r.Roscoe == "" || r.Roscoe == "missing" {
		r.Err = fmt.Errorf("verify: roscoe is not on the node's PATH after install")
		return r
	}
	if o.Claude && (r.Claude == "" || r.Claude == "missing") {
		r.Err = fmt.Errorf("verify: claude is not on the node's PATH after install")
		return r
	}
	if o.Version != "" && !strings.Contains(r.Roscoe, o.Version) {
		r.Err = fmt.Errorf("verify: asked for roscoe %s, node runs %s (does the installer honour ROSCOE_VERSION?)", o.Version, r.Roscoe)
		return r
	}
	r.Steps = append(r.Steps, "verify")
	return r
}

func orLatest(v string) string {
	if v == "" {
		return "(latest)"
	}
	return v
}

// shellQuote single-quotes a value for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Enabled returns the nodes deploy and dispatch should touch, by name.
func Enabled(nodes []config.Node) []config.Node {
	var out []config.Node
	for _, n := range nodes {
		if n.Enabled && n.SSH != "" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
