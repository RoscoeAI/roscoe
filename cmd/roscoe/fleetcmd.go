package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/fleet"
)

// cmdNode shows the fleet: every configured machine, what is on it, and what
// deploy would have to put there.
func cmdNode(ctx context.Context, explicit string, args []string) int {
	cfg, _, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe node: %v\n", err)
		return 1
	}
	if len(cfg.Nodes) == 0 {
		fmt.Println("no nodes configured; add machines under nodes[] in roscoe.json")
		return 0
	}
	probes := fleet.ProbeAll(ctx, cfg.Nodes, fleet.SSH)
	fmt.Print(nodesTable(probes))
	ready := 0
	for _, p := range probes {
		if p.Ready() {
			ready++
		}
	}
	fmt.Printf("\n%d of %d ready.%s\n", ready, len(probes), nextStep(probes))
	return 0
}

// nextStep is the one command that gets the fleet closer to ready: deploy
// while anything is missing that deploy installs, otherwise the login each
// node still needs, since that is the one step deploy cannot do.
func nextStep(probes []fleet.Probe) string {
	var needLogin []string
	needEnv := false
	for _, p := range probes {
		if !p.Node.Enabled || !p.Reachable {
			continue
		}
		for _, m := range p.Missing() {
			switch m {
			case "roscoe", "config", "claude":
				return " Put roscoe on the rest:  roscoe deploy"
			case "login":
				needLogin = append(needLogin, p.Node.SSH)
			case "env":
				needEnv = true
			}
		}
	}
	if len(needLogin) > 0 {
		return fmt.Sprintf(" Log in on each node:  ssh -t %s claude auth login", needLogin[0])
	}
	if needEnv { // optional, so last: workers on their own claude login run without it
		return " Workers there have no API keys (tier 3 routes need them):  roscoe deploy --env"
	}
	return ""
}

func nodesTable(probes []fleet.Probe) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %-8s %s\n", "node", "ssh", "cores", "claude", "roscoe", "free", "state")
	for _, p := range probes {
		n := p.Node
		switch {
		case !n.Enabled:
			fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %-8s %sdisabled%s\n", n.Name, orDash(n.SSH), "-", "-", "-", "-", ansiFaint, ansiReset)
		case n.SSH == "":
			// This machine. It is never probed over ssh, so "unreachable"
			// would be a lie; run and chat use it directly.
			fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %-8d %shere (roscoe run uses it directly)%s\n", n.Name, "-", "-", "-", "-", n.Workers, ansiDim, ansiReset)
		case !p.Reachable:
			why := "unreachable"
			if p.Err != nil {
				why = sshReason(p.Err)
			}
			fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %-8s %s%s%s\n", n.Name, n.SSH, "-", "-", "-", "-", ansiDim, why, ansiReset)
		default:
			state := ansiGreen + "ready" + ansiReset
			if !p.Ready() {
				state = ansiDim + "needs " + strings.Join(p.Missing(), ", ") + ansiReset
			}
			fmt.Fprintf(&b, "  %-12s %-16s %-9d %-20s %-12s %-8s %s\n", n.Name, n.SSH, p.Cores, claudeCell(p), shortVersion(p.Roscoe), freeCell(p), state)
		}
	}
	return b.String()
}

// sshReason keeps the part of an ssh error that says why: "No route to host",
// not the 40-character preamble naming a host the row already names.
func sshReason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "port 22: "); i >= 0 {
		msg = msg[i+len("port 22: "):]
	}
	msg = strings.TrimPrefix(msg, "ssh: ")
	return oneLineOf(msg, 50)
}

func orDash(s string) string {
	if s == "" {
		return "(this machine)"
	}
	return s
}

// freeCell is "free of workers", e.g. "2/2": the number a dispatch reads.
func freeCell(p fleet.Probe) string {
	return fmt.Sprintf("%d/%d", p.Free(), p.Node.Workers)
}

// claudeCell is the version plus whether it can do anything: an installed
// claude that is not logged in runs nothing.
func claudeCell(p fleet.Probe) string {
	v := shortVersion(p.Claude)
	if v == "missing" || p.LoggedIn {
		return v
	}
	return v + ", no login"
}

// shortVersion trims "roscoe v0.28.0 (go1.26.7)" and "2.1.251 (Claude Code)"
// to the version alone.
func shortVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "missing" {
		return "missing"
	}
	s = strings.TrimPrefix(s, "roscoe ")
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	return s
}

// cmdDeploy puts roscoe on every enabled node, pinned to this binary's own
// version, and pushes the config. Claude and the env file are opt-in: one is
// a bigger footprint, the other is secrets.
func cmdDeploy(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	only := fs.String("node", "", "deploy to this node only")
	withClaude := fs.Bool("claude", false, "also install Claude Code on the node")
	withEnv := fs.Bool("env", false, "also push the env file (API keys) to ~/.roscoe/.env")
	_ = fs.Parse(args)

	cfg, _, cfgPath, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe deploy: %v\n", err)
		return 1
	}
	nodes := fleet.Enabled(cfg.Nodes)
	if *only != "" {
		nodes = pickNode(nodes, *only)
		if len(nodes) == 0 {
			fmt.Fprintf(os.Stderr, "roscoe deploy: no enabled node named %q\n", *only)
			return 2
		}
	}
	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, "roscoe deploy: no enabled nodes with an ssh alias")
		return 2
	}
	opts := fleet.DeployOpts{Version: pinnedVersion(), ConfigPath: cfgPath, Claude: *withClaude}
	if *withEnv {
		opts.EnvPath = config.ExpandPath(cfg.EnvFile)
	}
	pin := opts.Version
	if pin == "" {
		pin = "(latest; this is a dev build with no release to pin)"
	}
	fmt.Fprintf(os.Stderr, "[deploy] roscoe %s + %s to %d node(s)\n", pin, cfgPath, len(nodes))

	failed := 0
	for _, n := range nodes {
		fmt.Fprintf(os.Stderr, "  %s (%s)…", n.Name, n.SSH)
		r := fleet.Deploy(ctx, n, opts, fleet.SSH, fleet.SCP)
		if r.Err != nil {
			failed++
			fmt.Fprintf(os.Stderr, " failed after %s: %v\n", strings.Join(r.Steps, ", "), r.Err)
			continue
		}
		fmt.Fprintf(os.Stderr, " ok in %s · roscoe %s · claude %s\n",
			r.Elapsed.Round(1e9), shortVersion(r.Roscoe), shortVersion(r.Claude))
	}
	if failed > 0 {
		return 1
	}
	fmt.Fprintln(os.Stderr, "[deploy] done. Each node still needs its own Claude login:  ssh -t <alias> claude auth login")
	return 0
}

// runOnNode is `roscoe run --node`: the same task, on another machine, with
// its output and the operator's keys passed straight through ssh. The node
// is probed first so "not logged in" is said here in one line, rather than
// discovered as a worker failure on the far side.
func runOnNode(ctx context.Context, cfg *config.Config, name, prompt string, o fleet.RemoteOpts) int {
	nodes := pickNode(fleet.Enabled(cfg.Nodes), name)
	if len(nodes) == 0 {
		fmt.Fprintf(os.Stderr, "roscoe run: no enabled node named %q; roscoe node lists them\n", name)
		return 2
	}
	p := fleet.ProbeAll(ctx, nodes, fleet.SSH)[0]
	if !p.Ready() {
		fmt.Fprintln(os.Stderr, notReady(p))
		return 1
	}
	return execOnNode(ctx, cfg, p, prompt, o)
}

// execOnNode runs the task on an already-probed node.
func execOnNode(ctx context.Context, cfg *config.Config, p fleet.Probe, prompt string, o fleet.RemoteOpts) int {
	n := p.Node
	fmt.Fprintf(os.Stderr, "[node] %s (%s) · claude %s · roscoe %s · %s free · task %s · dir %s\n",
		n.Name, n.SSH, shortVersion(p.Claude), shortVersion(p.Roscoe), freeCell(p), o.TaskID, o.DisplayDir())
	code := fleet.Exec(ctx, n, fleet.RemoteRun(prompt, o), isTTY(os.Stdin) && isTTY(os.Stdout))
	// The run's ledger is on the node; bring it here so roscoe sessions and
	// the memory graph see fleet work like local work. Best effort: the task
	// already ran, and a failed copy must not turn its exit code into ours.
	if got, err := fleet.BringHome(ctx, n, []string{o.TaskID}, runsDir(cfg), fleet.SCPFrom); err != nil {
		fmt.Fprintf(os.Stderr, "[home] %v (roscoe sessions --node %s retries)\n", err, n.Name)
	} else if len(got) == 1 {
		fmt.Fprintf(os.Stderr, "[home] ledger for %s is here; roscoe sessions shows it\n", o.TaskID)
	}
	return code
}

// cmdDispatch runs one task on whichever enabled node has the most free
// worker slots. It is `run --node` with the node chosen for you.
func cmdDispatch(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("dispatch", flag.ExitOnError)
	taskID := fs.String("task-id", "", "task id (default: generated)")
	dir := fs.String("dir", "", "working directory on the node (default: ~/.roscoe/work/<task-id>)")
	harness := fs.String("harness", "", `worker harness on the node: "claude" or "codex" (default: the node's config)`)
	// Flags may come before or after the prompt, as usage promises.
	var prompts []string
	rest := args
	for {
		_ = fs.Parse(rest)
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		prompts = append(prompts, rest[0])
		rest = rest[1:]
	}
	if len(prompts) != 1 {
		fmt.Fprintln(os.Stderr, `usage: roscoe dispatch "<prompt>" [--task-id X] [--dir D] [--harness H]`)
		return 2
	}
	prompt := prompts[0]
	cfg, _, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe dispatch: %v\n", err)
		return 1
	}
	nodes := fleet.Enabled(cfg.Nodes)
	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, "roscoe dispatch: no enabled nodes with an ssh alias; add machines under nodes[]")
		return 2
	}
	probes := fleet.ProbeAll(ctx, nodes, fleet.SSH)
	p, ok := fleet.Pick(probes)
	if !ok {
		fmt.Fprint(os.Stderr, noNodeFree(probes))
		return 1
	}
	if *taskID == "" {
		*taskID = newTaskID()
	}
	return execOnNode(ctx, cfg, p, prompt, fleet.RemoteOpts{TaskID: *taskID, Dir: *dir, Harness: *harness})
}

// noNodeFree explains a refused dispatch: the table, so every node's reason
// is visible at once, then the one command that helps.
func noNodeFree(probes []fleet.Probe) string {
	msg := "roscoe dispatch: no node can take work right now\n" + nodesTable(probes)
	// A ready node was refused only because it is full, so say that; the
	// readiness hint is then about the OTHER nodes (more capacity), never
	// about the full one, whose missing env file is not why it was refused.
	var full, rest []fleet.Probe
	for _, p := range probes {
		if p.Ready() {
			full = append(full, p)
		} else {
			rest = append(rest, p)
		}
	}
	out := msg + "\n"
	if len(full) > 0 {
		out += " every ready node is at its worker limit; wait, or raise nodes[].workers\n"
	}
	if hint := nextStep(rest); hint != "" {
		out += " " + strings.TrimSpace(hint) + "\n"
	} else if len(full) == 0 {
		out += " no node is ready\n"
	}
	return out
}

// cmdUp brings the fleet to ready as far as software can: deploy to every
// enabled node, then show the table with what is left (usually a login).
func cmdUp(ctx context.Context, explicit string, args []string) int {
	if code := cmdDeploy(ctx, explicit, args); code != 0 {
		return code
	}
	fmt.Fprintln(os.Stderr)
	return cmdNode(ctx, explicit, nil)
}

// notReady says why a node cannot take work and what fixes it, in one line.
func notReady(p fleet.Probe) string {
	msg := fmt.Sprintf("roscoe run: %s is not ready: needs %s.", p.Node.Name, strings.Join(p.Missing(), ", "))
	if hint := nextStep([]fleet.Probe{p}); hint != "" {
		msg += hint
	}
	return msg
}

func pickNode(nodes []config.Node, name string) []config.Node {
	for _, n := range nodes {
		if n.Name == name {
			return []config.Node{n}
		}
	}
	return nil
}

// pinnedVersion is the release to put on nodes: this binary's own, so the
// fleet agrees about what roscoe is. A dev build has no release to pin, so
// it deploys latest and says so.
func pinnedVersion() string {
	if Version == "" || Version == "dev" {
		return ""
	}
	return Version
}
