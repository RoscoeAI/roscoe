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
	for _, p := range probes {
		if !p.Node.Enabled || p.Ready() {
			continue
		}
		for _, m := range p.Missing() {
			switch m {
			case "roscoe", "config", "claude":
				return " Put roscoe on the rest:  roscoe deploy"
			case "login":
				needLogin = append(needLogin, p.Node.SSH)
			}
		}
	}
	if len(needLogin) > 0 {
		return fmt.Sprintf(" Log in on each node:  ssh -t %s claude auth login", needLogin[0])
	}
	return ""
}

func nodesTable(probes []fleet.Probe) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %s\n", "node", "ssh", "cores", "claude", "roscoe", "state")
	for _, p := range probes {
		n := p.Node
		switch {
		case !n.Enabled:
			fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %sdisabled%s\n", n.Name, orDash(n.SSH), "-", "-", "-", ansiFaint, ansiReset)
		case !p.Reachable:
			why := "unreachable"
			if p.Err != nil {
				why = sshReason(p.Err)
			}
			fmt.Fprintf(&b, "  %-12s %-16s %-9s %-20s %-12s %s%s%s\n", n.Name, n.SSH, "-", "-", "-", ansiDim, why, ansiReset)
		default:
			state := ansiGreen + "ready" + ansiReset
			if !p.Ready() {
				state = ansiDim + "needs " + strings.Join(p.Missing(), ", ") + ansiReset
			}
			fmt.Fprintf(&b, "  %-12s %-16s %-9d %-20s %-12s %s\n", n.Name, n.SSH, p.Cores, claudeCell(p), shortVersion(p.Roscoe), state)
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
	fmt.Fprintf(os.Stderr, "[deploy] roscoe %s + %s to %d node(s)\n", opts.Version, cfgPath, len(nodes))

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
