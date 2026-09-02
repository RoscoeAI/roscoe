package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/fleet"
	"roscoe.sh/roscoe/internal/sessions"
)

// cmdTop is the day at a glance: what roscoe has cost today and this week,
// what is running here and across the fleet, and the last few sessions. One
// screen, no daemon; --watch redraws it.
func cmdTop(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	watch := fs.Duration("watch", 0, "redraw every interval, e.g. 10s (default: once)")
	noFleet := fs.Bool("no-fleet", false, "skip probing nodes over ssh")
	recent := fs.Int("recent", 5, "how many recent sessions to show")
	_ = fs.Parse(args)

	cfg, _, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe top: %v\n", err)
		return 1
	}
	for {
		list, _ := sessions.List(runsDir(cfg), 0, nil)
		for i := range list {
			if i < *recent {
				enrichSession(&list[i])
			}
		}
		var probes []fleet.Probe
		if !*noFleet && len(cfg.Nodes) > 0 {
			probes = fleet.ProbeAll(ctx, cfg.Nodes, fleet.SSH)
		}
		out := renderTop(topData{
			Now:      time.Now(),
			Sessions: list,
			Here:     runningHere(),
			Probes:   probes,
			Recent:   *recent,
		})
		if *watch > 0 {
			fmt.Print("\x1b[H\x1b[2J") // home, clear
		}
		fmt.Print(out)
		if *watch <= 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*watch):
		}
	}
}

// topData is everything the screen shows, gathered first so rendering is
// pure and testable.
type topData struct {
	Now      time.Time
	Sessions []sessions.Session // newest first
	Here     int                // roscoe workers running on this machine
	Probes   []fleet.Probe      // nil when the fleet was not probed
	Recent   int
}

// spend is one period's totals.
type spend struct {
	Runs  int
	Turns int
	Cost  float64
}

// spendSince sums the sessions that started at or after t.
func spendSince(list []sessions.Session, t time.Time) spend {
	var s spend
	for _, x := range list {
		if x.Started.Before(t) {
			continue
		}
		s.Runs++
		s.Turns += x.Turns
		s.Cost += x.CostUSD
	}
	return s
}

func (s spend) String() string {
	if s.Runs == 0 {
		return "nothing"
	}
	return fmt.Sprintf("$%.2f · %d run%s · %d turn%s", s.Cost, s.Runs, plural(s.Runs), s.Turns, plural(s.Turns))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// startOfDay and startOfWeek (Monday) in local time.
func startOfDay(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

func startOfWeek(now time.Time) time.Time {
	day := startOfDay(now)
	back := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -back)
}

func renderTop(d topData) string {
	var b strings.Builder
	today := spendSince(d.Sessions, startOfDay(d.Now))
	week := spendSince(d.Sessions, startOfWeek(d.Now))
	fmt.Fprintf(&b, "  %-9s %s\n", "today", today)
	fmt.Fprintf(&b, "  %-9s %s\n", "week", week)

	// Running: here, and the fleet's slots.
	running := fmt.Sprintf("%d worker%s here", d.Here, plural(d.Here))
	if d.Probes != nil {
		busy, slots, ready := 0, 0, 0
		for _, p := range d.Probes {
			if !p.Node.Enabled || !p.Reachable {
				continue
			}
			busy += p.Busy
			slots += p.Node.Workers
			if p.Ready() {
				ready++
			}
		}
		running += fmt.Sprintf(" · fleet %d/%d slots busy, %d node%s ready", busy, slots, ready, plural(ready))
	}
	fmt.Fprintf(&b, "  %-9s %s\n", "running", running)

	if d.Probes != nil {
		b.WriteString("\n")
		b.WriteString(nodesTable(d.Probes))
	}

	b.WriteString("\n")
	if len(d.Sessions) == 0 {
		b.WriteString("  no sessions yet\n")
		return b.String()
	}
	recent := d.Sessions
	if d.Recent > 0 && len(recent) > d.Recent {
		recent = recent[:d.Recent]
	}
	b.WriteString(sessionsTable(recent, d.Now))
	return b.String()
}

// runningHere counts roscoe workers on this machine, by the same
// self-excluding pgrep the fleet probe uses on a node.
func runningHere() int {
	out, err := exec.Command("sh", "-c", `pgrep -f '[r]oscoe (run|chat|loop) ' 2>/dev/null | wc -l | tr -d ' '`).Output()
	if err != nil {
		return 0
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}
