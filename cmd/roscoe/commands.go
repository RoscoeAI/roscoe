package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"roscoe.sh/roscoe/internal/accounts"
	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/fleet"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/notify"
	"roscoe.sh/roscoe/internal/pool"
	"roscoe.sh/roscoe/internal/relay"
	"roscoe.sh/roscoe/internal/router"
	"roscoe.sh/roscoe/internal/smoke"
	"roscoe.sh/roscoe/internal/streamjson"
	"roscoe.sh/roscoe/internal/worker"
)

func cmdVersion() int {
	fmt.Printf("roscoe %s (%s)\n", Version, runtime.Version())
	return 0
}

func cmdInit(explicit string) int {
	path := explicit
	if path == "" {
		path = "roscoe.json"
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "roscoe init: %s already exists; refusing to overwrite\n", path)
		return 1
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "roscoe init: stat %s: %v\n", path, err)
		return 1
	}
	if err := config.Default().Save(path); err != nil {
		fmt.Fprintf(os.Stderr, "roscoe init: %v\n", err)
		return 1
	}
	fmt.Printf("wrote %s\n", path)
	return 0
}

func cmdConfig(explicit string, args []string) int {
	const use = "usage: roscoe config                       list the settings\n" +
		"       roscoe config show <path>           one setting: value, options, what it costs\n" +
		"       roscoe config get <path>            the bare value, for scripts\n" +
		"       roscoe config set <path> <value>"
	path, err := resolveConfigPath(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe config: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe config: load %s: %v\n", path, err)
		return 1
	}
	if len(args) < 1 {
		for _, l := range levelLines(cfg, "") {
			fmt.Println(l)
		}
		fmt.Println("\nroscoe config show <path> opens one; roscoe config set <path> <value> changes it")
		return 0
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, use)
			return 2
		}
		if len(cfg.ChildPaths(rest[0])) > 0 {
			for _, l := range levelLines(cfg, rest[0]) {
				fmt.Println(l)
			}
			return 0
		}
		if _, err := cfg.Get(rest[0]); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config show: %v\n", err)
			return 1
		}
		for _, l := range buildLeafCard(cfg, rest[0], "roscoe config set", "").lines() {
			fmt.Println(l)
		}
		return 0
	case "get":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, use)
			return 2
		}
		v, err := cfg.Get(rest[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config get: %v\n", err)
			return 1
		}
		if s, ok := v.(string); ok {
			fmt.Println(s) // bare strings, git-config style
			return 0
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config get: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
		return 0

	case "set":
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, use)
			return 2
		}
		oldStr := "(unset)"
		if old, err := cfg.Get(rest[0]); err == nil {
			oldStr = jsonCompact(old)
		}
		if err := cfg.SetPath(rest[0], rest[1]); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config set: %v\n", err)
			return 1
		}
		if errs := cfg.Validate(); len(errs) > 0 {
			fmt.Fprintln(os.Stderr, "roscoe config set: refusing to save an invalid config:")
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  - %v\n", e)
			}
			return 1
		}
		newV, err := cfg.Get(rest[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config set: re-read %s: %v\n", rest[0], err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe config set: %v\n", err)
			return 1
		}
		fmt.Printf("%s: %s → %s\n", rest[0], oldStr, jsonCompact(newV))
		return 0

	default:
		fmt.Fprintf(os.Stderr, "roscoe config: unknown subcommand %q\n%s\n", sub, use)
		return 2
	}
}

func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func cmdRouter(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("router", flag.ExitOnError)
	bind := fl.String("bind", "", "bind address override (default: router.bind from config)")
	port := fl.Int("port", 0, "port override (default: router.port from config)")
	_ = fl.Parse(args) // ExitOnError
	if fl.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "roscoe router: unexpected arguments %q\n", fl.Args())
		return 2
	}

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe router: %v\n", err)
		return 1
	}

	r, err := router.New(router.Options{Cfg: cfg, Env: env, LogW: os.Stdout, Bind: *bind, Port: *port})
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe router: %v\n", err)
		return 1
	}

	errCh := make(chan error, 1)
	go func() { errCh <- r.ListenAndServe(ctx) }()

	addr, err := waitHealthz(ctx, r.Addr, errCh, 3*time.Second)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0 // interrupted before ready — clean exit
		}
		fmt.Fprintf(os.Stderr, "roscoe router: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "roscoe router: listening on %s (SIGINT to stop)\n", addr)

	err = <-errCh
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "roscoe router: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "roscoe router: shut down")
	return 0
}

func cmdSmoke(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("smoke", flag.ExitOnError)
	full := fl.Bool("full", false, "also run the live harness probe (spawns claude)")
	_ = fl.Parse(args)
	if fl.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "roscoe smoke: unexpected arguments %q\n", fl.Args())
		return 2
	}

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe smoke: %v\n", err)
		return 1
	}

	checks := smoke.Run(ctx, cfg, env, *full)
	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	failed := false
	for _, c := range checks {
		sym := "✓"
		switch {
		case c.Skipped:
			sym = "–"
		case !c.OK:
			sym = "✗"
			failed = true
		}
		fmt.Printf("%s %-*s  %s\n", sym, width, c.Name, c.Detail)
	}
	if failed {
		return 1
	}
	return 0
}

func cmdRun(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("run", flag.ExitOnError)
	taskID := fl.String("task-id", "", "task id (default: generated)")
	dir := fl.String("dir", "", "working directory for the task (default: current directory)")
	resume := fl.String("resume", "", "continue an existing claude session by id (migrates the transcript into the fleet)")
	fromConfig := fl.String("from-config-dir", "", "CLAUDE_CONFIG_DIR to migrate --resume's session from (default: ~/.claude)")
	harness := fl.String("harness", "", `worker harness: "claude" (default) or "codex" (overrides tiers.middle.harness)`)
	node := fl.String("node", "", "run on this node from nodes[] instead of here (see roscoe node)")
	_ = fl.Parse(args)
	rest := fl.Args()
	var prompt string
	if len(rest) == 0 {
		// No prompt argument: ask for it, so `roscoe run --resume <id>` alone
		// is a usable entry point. Non-interactive callers still get usage.
		if !isTTY(os.Stdin) {
			fmt.Fprintln(os.Stderr, `usage: roscoe run "<prompt>" ["<prompt>"...] [--task-id X] [--dir D] [--resume <session-id>]`)
			return 2
		}
		fmt.Fprint(os.Stderr, "prompt> ")
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 10<<20)
		if !sc.Scan() {
			fmt.Fprintln(os.Stderr)
			return 130
		}
		prompt = strings.TrimSpace(sc.Text())
		if prompt == "" {
			fmt.Fprintln(os.Stderr, "roscoe run: empty prompt")
			return 2
		}
	} else {
		prompt = rest[0]
	}

	// Flags may follow the prompt, and further quoted arguments are further
	// prompts: roscoe run "a" "b" "c" runs the three at once.
	var more []string
	if len(rest) > 1 {
		_ = fl.Parse(rest[1:])
		more = fl.Args()
	}
	if len(more) > 0 && (*resume != "" || *node != "") {
		fmt.Fprintln(os.Stderr, "roscoe run: several prompts run here, fresh; --resume and --node take one prompt")
		return 2
	}
	// Flags are all known from here on; --resume read before this point was
	// ignored when it came after the prompt.
	if *node != "" && *resume != "" {
		fmt.Fprintln(os.Stderr, "roscoe run: --resume and --node do not combine; the session's transcript is on this machine")
		return 2
	}
	resumeFrom := ""
	if *resume != "" {
		src, err := worker.FindSession(*fromConfig, *resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
			return 1
		}
		resumeFrom = src
		fmt.Fprintf(os.Stderr, "[migrate] importing session %s from %s\n", *resume, src)
	}

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
		return 1
	}
	if *harness != "" {
		cfg.Tiers.Middle.Harness = *harness
	}

	if *taskID == "" {
		*taskID = newTaskID()
	}
	if *node != "" {
		return runOnNode(ctx, cfg, *node, prompt, fleet.RemoteOpts{TaskID: *taskID, Dir: *dir, Harness: *harness})
	}
	if *dir == "" {
		if *dir, err = os.Getwd(); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe run: getwd: %v\n", err)
			return 1
		}
	}

	var led *ledger.Ledger
	if cfg.Reporting.Ledger != "" {
		p := config.ExpandPath(strings.ReplaceAll(cfg.Reporting.Ledger, "{run_id}", *taskID))
		led, err = ledger.Open(filepath.Dir(p))
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe run: open ledger: %v\n", err)
			return 1
		}
		defer led.Close()
	}

	// Router in-process; Bind/Port zero values defer to cfg.Router.
	r, err := router.New(router.Options{Cfg: cfg, Env: env})
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
		return 1
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	defer recordRouterTotals(led, r, cfg, *taskID) // before led.Close, after the run
	errCh := make(chan error, 1)
	go func() { errCh <- r.ListenAndServe(rctx) }()

	addr, err := waitHealthz(ctx, r.Addr, errCh, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[router] listening on %s\n", addr)

	account, token := resolveMiddleAccount(cfg, env)
	if len(more) > 0 {
		// Every account with a token, not just the first: parallel workers
		// spread across them, each account under its own ceiling.
		creds, _ := accounts.ResolveAll(cfg, cfg.Tiers.Middle.Accounts, env, os.Getenv, accounts.MacKeychain{})
		accts := newAccountPool(creds, cfg.Limits.PerAccountMaxConcurrent)
		limit := pool.EffectiveLimit(cfg.Limits.MaxParallelTasks, accts.slots(), 1+len(more))
		if len(creds) > 1 {
			names := make([]string, len(creds))
			for i, c := range creds {
				names[i] = c.Name
			}
			fmt.Fprintf(os.Stderr, "[accounts] %d in play: %s · %d each at once\n", len(creds), strings.Join(names, ", "), cfg.Limits.PerAccountMaxConcurrent)
		}
		_ = account
		return runMany(ctx, os.Stderr, append([]string{prompt}, more...), *taskID, limit, workerTask(cfg, addr, accts, *dir))
	}
	fmt.Fprintf(os.Stderr, "[task] %s dir=%s\n", *taskID, *dir)

	// Esc interrupts the running task at a clean point; typing a line then
	// resumes the same session with the new instruction.
	var keys *keyReader
	if isTTY(os.Stdin) {
		if k, restoreTTY, kerr := newKeyReader(); kerr == nil {
			keys = k
			defer restoreTTY()
			fmt.Fprintln(os.Stderr, "[keys] esc interrupts the task; you can then type a redirect")
		}
	}

	runPrompt, runResume, runResumeFrom := prompt, *resume, resumeFrom
	var lastSession string
	nar := &narrator{}
	onEvent := func(ev *streamjson.Event) {
		if ie, ok := ev.AsInit(); ok {
			if ie.SessionID != "" {
				lastSession = ie.SessionID
			}
			learnResolvedModel(cfg, cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model, ie.Model)
		}
		nar.event(ev)
	}

	var res *streamjson.ResultEvent
	var runErr error
	runResumeBudget, tooLong := 0, 0 // window shrinks when the model refuses it
	for {
		taskCtx, cancelTask := context.WithCancel(ctx)
		var escPressed atomic.Bool
		if keys != nil {
			go func() {
				if keys.WaitEsc(taskCtx) {
					escPressed.Store(true)
					fmt.Fprintln(os.Stderr, "\n[esc] interrupting; the worker stops at a clean point…")
					cancelTask()
				}
			}()
		}

		res, runErr = worker.Run(taskCtx,
			worker.Task{ID: *taskID, Prompt: runPrompt, Dir: *dir, Account: account, Token: token,
				Resume: runResume, ResumeFrom: runResumeFrom, ResumeBudget: runResumeBudget},
			worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led, OnEvent: onEvent},
		)
		cancelTask()

		// The resumed window was refused as too long: free, and roscoe's to
		// fix. Halve it and send the same prompt again, as chat does.
		if runErr == nil && ctx.Err() == nil && lastSession != "" && worker.RetryTooLong(res, tooLong) {
			if cdir, derr := worker.SessionConfigDir(cfg, *taskID, token); derr == nil {
				if path, ferr := worker.FindSession(cdir, lastSession); ferr == nil {
					tooLong++
					runResumeBudget = worker.HalveBudget(runResumeBudget)
					runResume, runResumeFrom = lastSession, path
					fmt.Fprintf(os.Stderr, "[resume] the model refused that much history; retrying with the most recent %dKB\n", runResumeBudget/1024)
					continue
				}
			}
		}

		if escPressed.Load() && ctx.Err() == nil {
			if res != nil && res.SessionID != "" {
				lastSession = res.SessionID
			}
			if cfg.Tiers.Middle.Harness == "codex" {
				fmt.Fprintln(os.Stderr, "[esc] stopped. The codex harness can't resume a session yet.")
				return 130
			}
			if lastSession == "" {
				fmt.Fprintln(os.Stderr, "[esc] stopped before the session started; nothing to resume.")
				return 130
			}
			line, ok := keys.ReadLine("redirect> ")
			if !ok || strings.TrimSpace(line) == "" {
				fmt.Fprintf(os.Stderr, "[esc] stopped. Pick it back up any time: roscoe run --resume %s \"...\"\n", lastSession)
				return 130
			}
			runPrompt = strings.TrimSpace(line)
			runResume = lastSession
			runResumeFrom = "" // the transcript already lives in this task's config dir
			fmt.Fprintf(os.Stderr, "[redirect] resuming session %s\n", lastSession)
			continue
		}
		break
	}

	// Stop the router before reporting so nothing interleaves with the output.
	rcancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", runErr)
		return 1
	}
	if res == nil { // contract says non-nil on success; guard anyway
		fmt.Fprintln(os.Stderr, "roscoe run: worker returned no result event")
		return 1
	}

	// stdout carries the answer: that is roscoe run's contract for scripts and
	// pipes. The stream on stderr is for a person watching. When stdout is
	// the same terminal, the person has already read the answer as it was
	// written, and printing it again shows it twice; when stdout is a pipe,
	// the consumer still needs it.
	if !(nar.StreamedAny() && isTTY(os.Stdout)) {
		out := res.Result
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		os.Stdout.WriteString(out)
	}

	status := "done"
	if res.IsError {
		status = "error"
	}
	fmt.Fprintf(os.Stderr, "[%s] cost=$%.4f session=%s\n", status, res.TotalCostUSD, res.SessionID)
	if res.IsError {
		return 1
	}
	return 0
}

// resolveMiddleAccount returns the first enabled middle-tier account whose
// token_ref is an env: reference with a non-empty value (env file first,
// process env as fallback). Slice 1 has no keychain access, so keychain:
// refs are skipped; no match means "" — claude's own auth takes over.
func resolveMiddleAccount(cfg *config.Config, env map[string]string) (name, token string) {
	name, token, tried := accounts.Resolve(cfg, cfg.Tiers.Middle.Accounts, env, os.Getenv, accounts.MacKeychain{})
	fmt.Fprintln(os.Stderr, accounts.Describe(name, tried))
	return name, token
}

// recordRouterTotals says and records what went through the router. A worker
// bills itself for its own model only; everything it forwarded to another
// provider (tier 3) is spend the harness never sees, and when the harness
// does see a routed model it guesses a price (roscoe/tier3 at 60K input
// tokens came out at $0.30, opus money for half a cent of GLM). The note's
// priced_here marks the upstreams whose cost is real here and not already
// on the harness's own bill, so the listing adds exactly those.
func recordRouterTotals(led *ledger.Ledger, r *router.Router, cfg *config.Config, taskID string) {
	for up, t := range r.Totals() {
		line := fmt.Sprintf("[router] %s · %d requests · %d in / %d out", up, t.Requests, t.Input, t.Output)
		if t.CacheRead+t.CacheWrite > 0 {
			line += fmt.Sprintf(" · cache %d read / %d write (%.0f%% of prompt)",
				t.CacheRead, t.CacheWrite, t.CacheHitRate()*100)
		} else {
			line += " · no prompt caching reported"
		}
		if t.CostUSD > 0 {
			line += fmt.Sprintf(" · $%.4f", t.CostUSD)
		}
		fmt.Fprintln(os.Stderr, line)
		if led != nil {
			_ = led.Note("router.totals", map[string]any{
				"task": taskID, "upstream": up, "total": t,
				"priced_here": pricedHere(cfg, up),
			})
		}
	}
}

// pricedHere reports whether an upstream's router-priced cost is spend the
// harness does not already bill: anything but the worker's own provider.
func pricedHere(cfg *config.Config, upstream string) bool {
	return upstream != cfg.Tiers.Middle.Provider
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// narrate prints one readable line per stream-json event to stderr.
// narrator renders worker events for a person. With a screen it writes into
// the TUI; without one it writes plain lines to stderr. It is stateful because
// streaming needs memory: once a message's text has arrived as deltas, the
// whole-message event that follows must not print that text a second time.
type narrator struct {
	sc *screen
	// errW is where the plain (no-screen) path writes; nil means stderr.
	// Tests inject a buffer.
	errW io.Writer
	// midLine is true after a delta was written to errW without a trailing
	// newline, so the next whole line knows to start on a fresh one rather
	// than gluing "[rate_limit_event]" onto a half-written sentence.
	midLine bool
	// streamed is set while the current assistant message's text has been
	// arriving as deltas; streamedAny records that any text streamed this
	// turn, so a caller can skip re-printing the final result.
	streamed    bool
	streamedAny bool
}

// StreamedAny reports whether any assistant text was streamed this turn.
func (n *narrator) StreamedAny() bool { return n != nil && n.streamedAny }

// event renders one worker event.
func (n *narrator) event(ev *streamjson.Event) {
	if ev == nil {
		return
	}
	// Streamed text: only the top-level conversation. Subagent chatter is
	// forwarded too, and letting every parallel worker append to one line
	// would make the line unreadable.
	if d, ok := ev.AsTextDelta(); ok {
		if d.ParentToolUseID != "" || d.Text == "" {
			return
		}
		n.streamed, n.streamedAny = true, true
		if n.sc != nil {
			n.sc.Stream(d.Text, "")
		} else {
			fmt.Fprint(n.ew(), d.Text)
			n.midLine = true
		}
		return
	}
	if ev.IsStreamEnd() {
		if n.streamed {
			if n.sc != nil {
				n.sc.EndStream()
			} else {
				n.endLine()
			}
		}
		return
	}
	if n.sc != nil {
		n.toScreen(ev)
	} else {
		n.toStderr(ev)
	}
}

func (n *narrator) ew() io.Writer {
	if n.errW != nil {
		return n.errW
	}
	return os.Stderr
}

// endLine finishes a streamed line on the plain path, once.
func (n *narrator) endLine() {
	if n.midLine {
		fmt.Fprintln(n.ew())
		n.midLine = false
	}
}

func (n *narrator) toStderr(ev *streamjson.Event) {
	w := n.ew()
	// Anything printed as a whole line must not land mid-sentence.
	n.endLine()
	switch {
	case ev.Type == "system" && ev.Subtype == "init":
		if ie, ok := ev.AsInit(); ok {
			fmt.Fprintf(w, "[init] model=%s session=%s\n", ie.Model, ie.SessionID)
			return
		}
		fmt.Fprintln(w, "[init]")

	case ev.Type == "system" && ev.Subtype == "api_retry":
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Raw, &m)
		if m.Message != "" {
			fmt.Fprintf(w, "[api_retry] %s\n", snippet(m.Message))
		} else {
			fmt.Fprintln(w, "[api_retry]")
		}

	case ev.Type == "assistant":
		text, tools := assistantContent(ev.Raw)
		wasStreamed := n.streamed
		n.streamed = false
		switch {
		case text != "" && !wasStreamed:
			fmt.Fprintf(w, "[assistant] %s\n", snippet(text))
		case len(tools) > 0:
			fmt.Fprintf(w, "[assistant] tool_use: %s\n", strings.Join(tools, ", "))
		case text == "" && !wasStreamed:
			fmt.Fprintln(w, "[assistant]")
		}

	case ev.Type == "result":
		if re, ok := ev.AsResult(); ok {
			status := "success"
			if re.IsError {
				status = "error"
			}
			fmt.Fprintf(w, "[result] %s cost=$%.4f session=%s\n", status, re.TotalCostUSD, re.SessionID)
			return
		}
		fmt.Fprintln(w, "[result]")

	case ev.Type == "stream_event":
		// Block starts, message metadata: nothing a person needs to see.

	default:
		if ev.Subtype != "" {
			fmt.Fprintf(w, "[%s/%s]\n", ev.Type, ev.Subtype)
		} else {
			fmt.Fprintf(w, "[%s]\n", ev.Type)
		}
	}
}

func (n *narrator) toScreen(ev *streamjson.Event) {
	sc := n.sc
	if ie, ok := ev.AsInit(); ok {
		sc.Printf("%s%s · session %s%s", ansiFaint, ie.Model, shortID(ie.SessionID), ansiReset)
		return
	}
	if _, ok := ev.AsResult(); ok {
		return // the caller prints the answer (if it was not streamed) and the cost
	}
	switch ev.Type {
	case "assistant":
		text, tools := assistantContent(ev.Raw)
		wasStreamed := n.streamed
		n.streamed = false
		switch {
		case text != "" && !wasStreamed:
			sc.Printf("%s%s%s", ansiDim, snippet(text), ansiReset)
		case len(tools) > 0:
			sc.Printf("%s· %s%s", ansiFaint, strings.Join(tools, ", "), ansiReset)
		}
	case "system":
		if ev.Subtype == "api_retry" {
			sc.Printf("%s· retrying after an API error%s", ansiFaint, ansiReset)
		}
	}
}

// narrate and narrateTo keep the old call shape for callers that do not need
// to ask about streaming afterwards.
func narrate(ev *streamjson.Event) { (&narrator{}).event(ev) }

func narrateTo(sc *screen, ev *streamjson.Event) { (&narrator{sc: sc}).event(ev) }

// assistantContent pulls text and tool_use names out of an assistant event
// without depending on the full message schema.
func assistantContent(raw json.RawMessage) (text string, tools []string) {
	var m struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil
	}
	var parts []string
	for _, c := range m.Message.Content {
		switch c.Type {
		case "text":
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		case "tool_use":
			tools = append(tools, c.Name)
		}
	}
	return strings.Join(parts, " "), tools
}

// snippet collapses whitespace and truncates to 120 runes for one-line narration.
func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

var stubBlurbs = map[string]string{
	"up":       "start the supervisor control plane: task ledger, scheduler, account vault, quorum voting, and notifications for the whole fleet.",
	"node":     "run the per-node agent (launched over ssh by the supervisor): an embedded router on 127.0.0.1 plus a spawn-per-task claude worker pool.",
	"accounts": "manage the Claude credential vault: add runs `claude setup-token` and stores the result in the macOS Keychain; list/rotate/rm manage token lifecycle.",
	"deploy":   "copy the roscoe binary to every enabled node over ssh and pin a single claude version fleet-wide.",
	"dispatch": "send one task to a specific node's worker pool and stream its events back into the run ledger.",
	"status":   "report fleet and task status from the run ledger: nodes, workers, accounts, and spend.",
	"top":      "show a live terminal view of workers, tasks, and per-account spend across the fleet.",
}

// cmdNotify exercises the quorum escalation channel end to end:
//
//	roscoe notify test [message]      send one message via the configured channel
//	roscoe notify serve [--port N]    run the inbound webhook, print replies
func cmdNotify(ctx context.Context, explicit string, args []string) int {
	if len(args) == 0 || (args[0] != "test" && args[0] != "serve") {
		fmt.Fprintln(os.Stderr, "usage: roscoe notify test [message] | roscoe notify serve [--port N]")
		return 2
	}
	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe notify: %v\n", err)
		return 1
	}
	n, err := buildNotifier(cfg, env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe notify: %v\n", err)
		return 1
	}

	switch args[0] {
	case "test":
		msg := "roscoe notify test: the escalation channel works. Reply to test the inbound path."
		if len(args) > 1 {
			msg = strings.Join(args[1:], " ")
		}
		if err := n.Send(ctx, notify.Message{Title: "roscoe", Body: msg}); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe notify test: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "roscoe notify test: sent via %s\n", cfg.Quorum.Notify.Channel)
		return 0

	case "serve":
		fl := flag.NewFlagSet("notify serve", flag.ExitOnError)
		port := fl.Int("port", 8080, "port for the inbound webhook listener")
		path := fl.String("path", "/twilio/sms", "webhook path")
		_ = fl.Parse(args[1:])
		h := n.InboundHandler(func(r notify.Reply) {
			fmt.Printf("[reply] from=%s at=%s body=%q\n", r.From, r.At.Format(time.RFC3339), r.Body)
		})
		if h == nil {
			fmt.Fprintf(os.Stderr, "roscoe notify serve: channel %q has no inbound path\n", cfg.Quorum.Notify.Channel)
			return 1
		}
		mux := http.NewServeMux()
		mux.Handle("POST "+*path, h)
		srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: mux}
		go func() { <-ctx.Done(); srv.Close() }()
		fmt.Fprintf(os.Stderr, "roscoe notify serve: listening on 127.0.0.1:%d%s (put this behind the tunnel; SIGINT to stop)\n", *port, *path)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "roscoe notify serve: %v\n", err)
			return 1
		}
		return 0
	}
	return 2
}

// buildNotifier constructs the configured channel; "roscoe-relay" lives in
// the relay package to keep notify's imports one-directional.
func buildNotifier(cfg *config.Config, env map[string]string) (notify.Notifier, error) {
	if cfg.Quorum.Notify.Channel == "roscoe-relay" {
		return relay.NewNotifier()
	}
	return notify.New(cfg.Quorum.Notify, env)
}

// cmdUpgrade links this machine to the hosted relay ($5/mo shared SMS
// number): device flow in the browser, tokens to ~/.roscoe/relay.json,
// notify channel flipped to roscoe-relay.
func cmdUpgrade(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("upgrade", flag.ExitOnError)
	phone := fl.String("phone", "", "your mobile number with the country code first, like +15551234567")
	baseURL := fl.String("base-url", relay.DefaultBaseURL, "relay control plane")
	_ = fl.Parse(args)
	if *phone == "" {
		fmt.Fprintln(os.Stderr, "roscoe upgrade: --phone is required, like --phone +15551234567. It is the number your escalation texts go to.")
		return 2
	}
	normalized := normalizePhone(*phone)
	if normalized != *phone {
		fmt.Fprintf(os.Stderr, "roscoe upgrade: using %s\n", normalized)
	}
	*phone = normalized

	// Already linked → only short-circuit if the SERVER still recognizes the
	// link. A locally-valid access token proves nothing when the account was
	// deleted or the subscription was cancelled upstream, so the billing
	// endpoint (not the token's expiry) is the source of truth. Any doubt
	// falls through to a fresh link rather than stranding the user.
	if creds, err := relay.LoadCreds(); err == nil && creds.Phone == *phone {
		if err := creds.EnsureFresh(ctx); err == nil {
			if _, err := relay.GetBillingStatus(ctx, creds.BaseURL, creds.Phone, creds.ClientID); err == nil {
				fmt.Fprintf(os.Stderr, "roscoe upgrade: already linked (client %s…, phone %s). Use \"roscoe relay status\" to inspect.\n", creds.ClientID[:8], creds.Phone)
				return 0
			}
		}
		fmt.Fprintln(os.Stderr, "roscoe upgrade: the saved link is no longer valid; starting a fresh one")
	}

	clientID := relay.NewClientID()
	start, err := relay.StartLink(ctx, *baseURL, clientID, *phone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe upgrade: %v\n", err)
		return 1
	}

	link := start.VerificationURLComplete
	if link == "" {
		link = start.VerificationURL
	}
	copied := copyToClipboard(link)
	fmt.Fprintf(os.Stderr, "\n  Finish linking in your browser:\n\n    %s\n\n", link)
	if copied {
		fmt.Fprintln(os.Stderr, "  The link is in your clipboard too — paste it into any browser")
		fmt.Fprintln(os.Stderr, "  signed into the account you want to use.")
	}
	fmt.Fprintln(os.Stderr, "  (sign in, confirm SMS consent, and complete the $5/mo checkout if prompted)")
	openBrowser(link)
	fmt.Fprintln(os.Stderr, "\n  Waiting for approval…")

	creds, err := relay.PollLink(ctx, *baseURL, clientID, start.DeviceCode, *phone, start.PollIntervalSeconds, func() { fmt.Fprint(os.Stderr, ".") })
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe upgrade: %v\n", err)
		return 1
	}
	if err := creds.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "roscoe upgrade: %v\n", err)
		return 1
	}

	// Flip the notify channel in config.
	cfg, _, cfgPath, err := loadConfigAndEnv(explicit)
	if err == nil && cfg.Quorum.Notify.Channel != "roscoe-relay" {
		cfg.Quorum.Notify.Channel = "roscoe-relay"
		if err := cfg.Save(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe upgrade: linked, but could not update %s: %v\n", cfgPath, err)
		} else {
			fmt.Fprintf(os.Stderr, "  quorum.notify.channel → roscoe-relay (%s)\n", cfgPath)
		}
	}

	if bs, err := relay.GetBillingStatus(ctx, *baseURL, creds.Phone, creds.ClientID); err == nil {
		fmt.Fprintf(os.Stderr, "  Linked. phone=%s subscription=%s active=%v\n", bs.Phone, bs.SubscriptionStatus, bs.Active)
		if !bs.Active {
			fmt.Fprintf(os.Stderr, "  Subscription not active yet — finish checkout at %s/link\n", *baseURL)
		}
	} else {
		fmt.Fprintln(os.Stderr, "  Linked.")
	}
	fmt.Fprintln(os.Stderr, "  Test it: roscoe notify test   ·   watch replies: roscoe relay listen")
	return 0
}

// cmdRelay: status | listen.
// normalizePhone accepts however a human types their number: strips
// punctuation, assumes US (+1) for bare 10-digit numbers, adds the
// missing + otherwise.
func normalizePhone(raw string) string {
	trimmed := strings.TrimSpace(raw)
	var digits strings.Builder
	for _, r := range trimmed {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(trimmed, "+"):
		return "+" + d
	case len(d) == 10:
		return "+1" + d
	case len(d) == 11 && strings.HasPrefix(d, "1"):
		return "+" + d
	case d != "":
		return "+" + d
	}
	return ""
}

func cmdRelay(ctx context.Context, _ string, args []string) int {
	if len(args) == 0 || (args[0] != "status" && args[0] != "listen" && args[0] != "unlink") {
		fmt.Fprintln(os.Stderr, "usage: roscoe relay status | roscoe relay listen | roscoe relay unlink")
		return 2
	}
	if args[0] == "unlink" {
		path := relay.CredsPath()
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "roscoe relay unlink: this machine is not linked")
				return 0
			}
			fmt.Fprintf(os.Stderr, "roscoe relay unlink: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "roscoe relay unlink: removed %s. Run \"roscoe upgrade --phone +1...\" to link again.\n", path)
		return 0
	}
	creds, err := relay.LoadCreds()
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe relay: %v\n", err)
		return 1
	}
	switch args[0] {
	case "status":
		mask := func(s string) string {
			if len(s) <= 4 {
				return "…"
			}
			return "…" + s[len(s)-4:]
		}
		fmt.Printf("client:  %s\nphone:   %s\nserver:  %s\naccess:  %s (expires %s)\nrefresh: %s (expires %s)\n",
			creds.ClientID, creds.Phone, creds.BaseURL,
			mask(creds.AccessToken), creds.AccessTokenExpiresAt.Format(time.RFC3339),
			mask(creds.RefreshToken), creds.RefreshTokenExpiresAt.Format(time.RFC3339))
		if bs, err := relay.GetBillingStatus(ctx, creds.BaseURL, creds.Phone, creds.ClientID); err == nil {
			fmt.Printf("billing: subscription=%s active=%v round-trip-verified=%v\n", bs.SubscriptionStatus, bs.Active, bs.RoundTripVerified)
		} else {
			fmt.Printf("billing: %v\n", err)
		}
		return 0
	case "listen":
		fmt.Fprintln(os.Stderr, "roscoe relay listen: holding the bridge open (SIGINT to stop)")
		err := creds.Connect(ctx,
			func(in relay.Inbound) { fmt.Printf("[reply] from=%s at=%s body=%q\n", in.From, in.ReceivedAt, in.Body) },
			func(s string) { fmt.Fprintf(os.Stderr, "[bridge] %s\n", s) })
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "roscoe relay listen: %v\n", err)
			return 1
		}
		return 0
	}
	return 2
}

func copyToClipboard(s string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		cmd = exec.Command("xclip", "-selection", "clipboard")
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run() == nil
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", u).Start()
	default:
		_ = exec.Command("xdg-open", u).Start()
	}
}

func cmdStub(name string) int {
	fmt.Fprintf(os.Stderr, "roscoe %s: coming in slice 2. It will %s See ARCHITECTURE.md for the design.\n", name, stubBlurbs[name])
	return 2
}
