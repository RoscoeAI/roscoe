package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/memory"
)

// cmdMemory inspects and maintains the fleet's cross-run memory. Recall
// happens automatically inside a loop; this is for seeing what it knows and
// for the one step that is deliberately manual, building the graph.
func cmdMemory(ctx context.Context, explicit string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: roscoe memory status | build [--full] [path] | query \"<question>\" | reflect")
		return 2
	}
	sub, rest := args[0], args[1:]

	cfg, _, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe memory: %v\n", err)
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe memory: getwd: %v\n", err)
		return 1
	}
	m := memory.New(cfg, dir)

	switch sub {
	case "status":
		s := m.Status()
		fmt.Printf("project   %s\n", m.Project)
		fmt.Printf("engine    %s (%s)\n", cfg.Memory.Engine, yesNo(s.Enabled, "enabled", "disabled"))
		fmt.Printf("graphify  %s\n", yesNo(s.Installed, "on PATH", "not installed"))
		fmt.Printf("graph     %s\n", pathState(s.Graph, s.HasGraph))
		fmt.Printf("lessons   %s\n", pathState(s.Lessons, fileExists(s.Lessons)))
		fmt.Printf("signals   %d awaiting reflect\n", s.Signals)
		if !s.Installed {
			fmt.Println("\nInstall graphify to turn this on: https://github.com/Graphify-Labs/graphify")
		} else if !s.HasGraph {
			fmt.Println("\nNo graph yet. Build one: roscoe memory build")
		}
		return 0

	case "build":
		fs := flag.NewFlagSet("memory build", flag.ExitOnError)
		full := fs.Bool("full", false, "full extraction with a model (default: incremental, no model)")
		_ = fs.Parse(rest)
		corpus := dir
		if fs.NArg() > 0 {
			corpus = fs.Arg(0)
		}
		what := "incremental update"
		if *full {
			what = "full extraction"
		}
		fmt.Fprintf(os.Stderr, "[memory] %s of %s into %s\n", what, corpus, m.Dir)
		start := time.Now()
		if err := m.Build(ctx, corpus, !*full); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe memory: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "[memory] done in %s · %s\n", time.Since(start).Round(time.Second), m.GraphPath())
		return 0

	case "query":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, `usage: roscoe memory query "<question>"`)
			return 2
		}
		if !m.Ready() {
			fmt.Fprintln(os.Stderr, "roscoe memory: nothing to query yet (see: roscoe memory status)")
			return 1
		}
		out, err := m.Recall(ctx, strings.Join(rest, " "), 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe memory: %v\n", err)
			return 1
		}
		if out == "" {
			fmt.Fprintln(os.Stderr, "(the graph had nothing to say)")
			return 0
		}
		fmt.Println(out)
		return 0

	case "reflect":
		path, err := m.Reflect(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe memory: %v\n", err)
			return 1
		}
		if path == "" {
			fmt.Fprintln(os.Stderr, "nothing recorded yet; lessons come from loop runs")
			return 0
		}
		fmt.Fprintf(os.Stderr, "[memory] %s\n", path)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "roscoe memory: unknown subcommand %q\n", sub)
		return 2
	}
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func pathState(p string, present bool) string {
	if present {
		return p
	}
	return p + "  (none yet)"
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}
