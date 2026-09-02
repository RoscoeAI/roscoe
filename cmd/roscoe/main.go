// Command roscoe is the CLI for the roscoe orchestrator (slice 1: laptop,
// single node, no ssh). main.go holds dispatch and shared helpers; each
// subcommand lives in commands.go.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Version is stamped at build time via -ldflags "-X main.Version=v...".
var Version = "dev"

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Accept --version/-v before flag parsing so the conventional spelling
	// works alongside the "version" subcommand.
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" || a == "-v" {
			return cmdVersion()
		}
	}

	// So a config error can name the roscoe that rejected it.
	config.Version = Version

	flag.Usage = usage
	cfgPath := flag.String("config", "", "path to roscoe.json (default ./roscoe.json, fallback ~/.roscoe/roscoe.json)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "version":
		return cmdVersion()
	case "init":
		return cmdInit(*cfgPath)
	case "config":
		return cmdConfig(*cfgPath, rest)
	case "router":
		return cmdRouter(ctx, *cfgPath, rest)
	case "smoke":
		return cmdSmoke(ctx, *cfgPath, rest)
	case "run":
		return cmdRun(ctx, *cfgPath, rest)
	case "chat":
		return cmdChat(ctx, *cfgPath, rest)
	case "loop":
		return cmdLoop(ctx, *cfgPath, rest)
	case "sessions":
		return cmdSessions(ctx, *cfgPath, rest)
	case "models":
		return cmdModels(ctx, *cfgPath, rest)
	case "memory":
		return cmdMemory(ctx, *cfgPath, rest)
	case "notify":
		return cmdNotify(ctx, *cfgPath, rest)
	case "upgrade":
		return cmdUpgrade(ctx, *cfgPath, rest)
	case "relay":
		return cmdRelay(ctx, *cfgPath, rest)
	case "node":
		return cmdNode(ctx, *cfgPath, rest)
	case "deploy":
		return cmdDeploy(ctx, *cfgPath, rest)
	case "dispatch":
		return cmdDispatch(ctx, *cfgPath, rest)
	case "status":
		return cmdNode(ctx, *cfgPath, rest)
	case "up":
		return cmdUp(ctx, *cfgPath, rest)
	case "accounts":
		return cmdAccounts(ctx, *cfgPath, rest)
	case "top":
		return cmdTop(ctx, *cfgPath, rest)
	case "calibrate":
		return cmdCalibrate(ctx, *cfgPath, rest)
	default:
		fmt.Fprintf(os.Stderr, "roscoe: unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `roscoe — a fleet orchestrator for Claude Code

usage: roscoe [--config <path>] <command> [args]

commands:
  init                                    write a default roscoe.json (refuses to overwrite)
  config [show <path> | get <path> | set <path> <value>]
                                          the settings; show gives options and what a choice costs
  router [--bind B] [--port N]            run the model router in the foreground
  smoke [--full]                          run the environment smoke checks
  run "<prompt>" ["<prompt>"...] [--task-id X] [--dir D] [--resume <session-id>] [--node N]
                                          run a task through a worker here, or on node N; several prompts run at once,
                                          up to limits.max_parallel_tasks;
                                          esc interrupts + lets you type a redirect;
                                          --resume migrates + continues an existing claude session
  chat [--dir D] [--resume <session-id>]  hold a conversation with one worker
  loop "<charter>" [--max-iterations N] [--budget USD] [--dir D] [--once]
                                          work a charter to completion: dispatch,
                                          read loop.md, judge, dispatch again;
                                          esc stops after the current iteration
  sessions [--limit N]                    what roscoe has run, newest first, with ids to resume
  memory status | build | query | reflect inspect and maintain cross-run memory
  models [--refresh]                      what each tier's model alias resolves to
  notify test [msg] | notify serve        exercise the escalation channel
  upgrade --phone +1XXXXXXXXXX            link the $5/mo hosted SMS relay
  relay status | listen | unlink          inspect the link, tail replies, or clear it
  version                                 print the roscoe version
  node                                    every machine in nodes[]: reachable, what is installed, ready or not
  deploy [--node N] [--claude] [--env]    put this roscoe version and the config on each enabled node
  dispatch "<prompt>" [--dir D] [--harness H]
                                          run one task on the node with the most free worker slots
  status                                  the node table (same as node)
  up [deploy flags]                       deploy to every enabled node, then show what is left to do
  accounts [set <name>]                   which Claude credentials the fleet has, and storing one
  top [--watch 10s] [--no-fleet]          the day at a glance: spend today and this week, what is running, recent sessions
  calibrate [--spend] [--apply]           measure this machine and recommend the fleet's limits; --apply writes them

--config defaults to ./roscoe.json, falling back to ~/.roscoe/roscoe.json.
`)
}

// resolveConfigPath picks the config file to read: an explicit --config wins,
// then ./roscoe.json, then ~/.roscoe/roscoe.json.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if _, err := os.Stat("roscoe.json"); err == nil {
		return "roscoe.json", nil
	}
	fallback := config.ExpandPath("~/.roscoe/roscoe.json")
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}
	return "", errors.New(`no roscoe.json found in . or ~/.roscoe (run "roscoe init")`)
}

// loadConfigAndEnv loads the resolved config and its env file. A missing env
// file is tolerated with a warning — smoke reports it as a failed check, and
// the other commands can still do useful work without it.
func loadConfigAndEnv(explicit string) (cfg *config.Config, env map[string]string, path string, err error) {
	path, err = resolveConfigPath(explicit)
	if err != nil {
		return nil, nil, "", err
	}
	cfg, err = config.Load(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load %s: %w", path, err)
	}
	env = map[string]string{}
	if cfg.EnvFile != "" {
		envPath := config.ExpandPath(cfg.EnvFile)
		env, err = config.LoadEnvFile(envPath)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, "", fmt.Errorf("load env file %s: %w", envPath, err)
			}
			fmt.Fprintf(os.Stderr, "roscoe: warning: env file %s not found; continuing without it\n", envPath)
			env = map[string]string{}
		}
	}
	return cfg, env, path, nil
}

// waitHealthz polls GET /healthz on the router until it answers 200, the
// timeout elapses, the router goroutine exits (errCh), or ctx is canceled.
// It returns the router's actual host:port.
func waitHealthz(ctx context.Context, addr func() string, errCh <-chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		select {
		case err := <-errCh:
			if err == nil {
				err = errors.New("exited before becoming ready")
			}
			return "", fmt.Errorf("router: %w", err)
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for router: %w", ctx.Err())
		default:
		}
		if a := addr(); a != "" {
			req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a+"/healthz", nil)
			if rerr == nil {
				resp, derr := client.Do(req)
				if derr == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return a, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("router did not answer /healthz within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// newTaskID returns "task-YYYYMMDD-HHMMSS-xxxx" (UTC + 2 random bytes) —
// readable in ledger paths, unique enough for a laptop CLI.
func newTaskID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("task-%s-%x", time.Now().UTC().Format("20060102-150405"), b)
}
