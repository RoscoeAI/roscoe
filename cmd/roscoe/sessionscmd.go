package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/fleet"
	"roscoe.sh/roscoe/internal/sessions"
	"roscoe.sh/roscoe/internal/worker"
)

// cmdSessions lists what roscoe has run, newest first, so a conversation can
// be found and resumed without remembering its id.
func cmdSessions(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	limit := fs.Int("limit", 15, "how many to show (0 for all)")
	node := fs.String("node", "", "first bring home the ledgers of runs on this node (a name from nodes[], or all)")
	_ = fs.Parse(args)

	cfg, _, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe sessions: %v\n", err)
		return 1
	}
	if *node != "" {
		if code := bringHome(ctx, cfg, *node); code != 0 {
			return code
		}
	}
	list, err := sessions.List(runsDir(cfg), *limit, enrichSession)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe sessions: %v\n", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Println("no sessions yet; roscoe chat or roscoe run will create one")
		return 0
	}
	fmt.Print(sessionsTable(list, time.Now()))
	fmt.Println()
	fmt.Println("resume one:  roscoe chat --resume <session>     the latest:  roscoe chat --last")
	return 0
}

// bringHome fetches the ledgers a node holds that this machine does not, so
// the listing is the whole fleet's history. "all" means every enabled node.
func bringHome(ctx context.Context, cfg *config.Config, name string) int {
	nodes := fleet.Enabled(cfg.Nodes)
	if name != "all" {
		nodes = pickNode(nodes, name)
		if len(nodes) == 0 {
			fmt.Fprintf(os.Stderr, "roscoe sessions: no enabled node named %q; roscoe node lists them\n", name)
			return 2
		}
	}
	failed := false
	for _, n := range nodes {
		ids, err := fleet.RemoteRuns(ctx, n, fleet.SSH)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe sessions: %s: %v\n", n.Name, err)
			failed = true
			continue
		}
		got, err := fleet.BringHome(ctx, n, ids, runsDir(cfg), fleet.SCPFrom)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe sessions: %v\n", err)
			failed = true
		}
		fmt.Fprintf(os.Stderr, "[home] %s: %d run(s) there, %d brought home\n", n.Name, len(ids), len(got))
	}
	if failed {
		return 1
	}
	return 0
}

func runsDir(cfg *config.Config) string {
	return filepath.Join(config.ExpandPath(cfg.StateDir), "runs")
}

// enrichSession adds what the ledger does not carry: the first prompt and the
// transcript size, read from the session file where the worker wrote it.
func enrichSession(s *sessions.Session) {
	if s.ID == "" {
		return
	}
	path, err := worker.FindSession("", s.ID)
	if err != nil {
		return
	}
	if fi, err := os.Stat(path); err == nil {
		s.Bytes = fi.Size()
	}
	if m, err := worker.FirstMessage(path); err == nil {
		s.About = aboutFromPrompt(m.Text)
	}
}

// aboutFromPrompt turns a first prompt into the line a listing shows. A loop
// iteration's first prompt is the kernel prompt, identical for every loop, so
// the charter it ends with is the part that identifies the run.
func aboutFromPrompt(text string) string {
	if i := strings.LastIndex(text, "\nCharter: "); i >= 0 {
		return strings.TrimSpace(text[i+len("\nCharter: "):])
	}
	return text
}

// sessionsTable renders the listing. The first prompt is the thing people
// actually recognise a conversation by, so it gets the room.
func sessionsTable(list []sessions.Session, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-9s %-9s %-9s %-6s %s\n", "when", "session", "cost", "turns", "about")
	unpriced := 0
	for _, s := range list {
		id := s.ID
		if id == "" {
			id = "(none)"
		} else if len(id) > 8 {
			id = id[:8]
		}
		cost := fmt.Sprintf("$%.2f", s.CostUSD)
		if s.CostUSD == 0 {
			cost = "-"
		}
		if s.Unpriced > 0 {
			cost += "*"
			unpriced += s.Unpriced
		}
		about := oneLineOf(s.About, 60)
		if s.Node != "" { // a fleet run: say where, because --resume there is a different command
			about = ansiFaint + "on " + s.Node + ansiReset + " · " + about
			if s.About == "" {
				about = ansiFaint + "on " + s.Node + " · " + shortDir(s.Dir) + ansiReset
			}
		}
		if about == "" {
			// No prompt to show: fall back to where it ran, and never emit
			// colour codes around nothing, which print as garbage when piped.
			if d := shortDir(s.Dir); d != "" {
				about = ansiFaint + d + ansiReset
			} else {
				about = ansiFaint + "(no transcript)" + ansiReset
			}
		}
		fmt.Fprintf(&b, "  %-9s %-9s %-9s %-6d %s\n", sessions.Age(s.Ended, now), id, cost, s.Turns, about)
	}
	if unpriced > 0 {
		fmt.Fprintf(&b, "  %s* plus %s tokens on a routed model the harness priced by guesswork; older runs, no router record. Left out.%s\n",
			ansiFaint, humanTokens(unpriced), ansiReset)
	}
	return b.String()
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

func oneLineOf(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// shortDir renders a working directory for a listing: ~ for the home, and a
// long path (a scratch dir under /private/tmp runs to 120 characters) cut to
// its last two components with an ellipsis, which is what identifies it.
func shortDir(dir string) string {
	if dir == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
		dir = "~" + dir[len(home):]
	}
	const max = 48
	if len(dir) <= max {
		return dir
	}
	parts := strings.Split(strings.TrimRight(dir, "/"), "/")
	if len(parts) <= 2 {
		return dir
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}
