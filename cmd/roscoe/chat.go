package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/router"
	"roscoe.sh/roscoe/internal/streamjson"
	"roscoe.sh/roscoe/internal/worker"
)

// cmdChat is a conversation with one worker: the router stays up, every turn
// resumes the same session, and Esc interrupts a turn without ending the chat.
func cmdChat(ctx context.Context, explicit string, args []string) int {
	fl := flag.NewFlagSet("chat", flag.ExitOnError)
	dir := fl.String("dir", "", "working directory (default: current directory)")
	resume := fl.String("resume", "", "start from an existing claude session id (migrates its transcript)")
	fromConfig := fl.String("from-config-dir", "", "CLAUDE_CONFIG_DIR to migrate --resume's session from (default: ~/.claude)")
	harness := fl.String("harness", "", `worker harness: "claude" (default) or "codex"`)
	taskID := fl.String("task-id", "", "task id (default: generated)")
	_ = fl.Parse(args)

	if !isTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "roscoe chat: needs a terminal; use \"roscoe run\" for scripted tasks")
		return 2
	}

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", err)
		return 1
	}
	if *harness != "" {
		cfg.Tiers.Middle.Harness = *harness
	}
	if *dir == "" {
		if *dir, err = os.Getwd(); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe chat: getwd: %v\n", err)
			return 1
		}
	}
	if *taskID == "" {
		*taskID = newTaskID()
	}

	// One session for the whole chat: the first turn may import an existing
	// transcript, every later turn resumes what the worker just wrote.
	session, resumeFrom := "", ""
	if *resume != "" {
		src, err := worker.FindSession(*fromConfig, *resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", err)
			return 1
		}
		session, resumeFrom = *resume, src
		fmt.Fprintf(os.Stderr, "[migrate] continuing session %s\n", *resume)
	}

	var led *ledger.Ledger
	if p := cfg.Reporting.Ledger; p != "" {
		expanded := config.ExpandPath(strings.ReplaceAll(p, "{run_id}", *taskID))
		if l, lerr := ledger.Open(filepath.Dir(expanded)); lerr == nil {
			led = l
			defer led.Close()
		}
	}

	r, err := router.New(router.Options{Cfg: cfg, Env: env})
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", err)
		return 1
	}
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.ListenAndServe(rctx) }()
	addr, err := waitHealthz(ctx, r.Addr, errCh, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", err)
		return 1
	}

	account, token := resolveMiddleAccount(cfg, env)
	var spent float64
	var turns int
	var history []string

	keys, restore, err := newKeyReader()
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", err)
		return 1
	}
	defer restore()

	harnessLabel := cfg.Tiers.Middle.Harness
	if harnessLabel == "" {
		harnessLabel = "claude"
	}
	sc := newScreen()
	sc.Enter()
	defer sc.Leave()
	go watchResize(sc)
	sc.Banner(cfg.Tiers.Middle.Model, harnessLabel, *dir)
	sc.Printf("%sautonomy %d · subagents %s · %d wide%s", ansiFaint,
		cfg.Autonomy.Level, cfg.Tiers.Subagents.Model, cfg.Tiers.Subagents.MaxConcurrent, ansiReset)

	// A resumed conversation should be visible, not just loaded: replay the
	// tail so the operator picks up where they left off.
	if resumeFrom != "" {
		if msgs, mErr := worker.RecentMessages(resumeFrom, 40); mErr == nil && len(msgs) > 0 {
			sc.Print("")
			sc.Printf("%s─ earlier in this conversation ─%s", ansiFaint, ansiReset)
			for _, m := range msgs {
				sc.Print("")
				if m.Role == "user" {
					sc.Print(ansiGreen + "› " + ansiReset + ansiBold + firstLines(m.Text, 6, 600) + ansiReset)
					continue
				}
				sc.Print(ansiDim + firstLines(m.Text, 8, 800) + ansiReset)
			}
			sc.Print("")
			sc.Printf("%s─ picking up here ─%s", ansiFaint, ansiReset)
		}
	}

	for {
		line, ok := keys.ReadLineOn(sc, "› ", history)
		if !ok {
			sc.Leave()
			fmt.Fprintln(os.Stderr, "roscoe chat: bye")
			return 0
		}
		msg := strings.TrimSpace(line)
		if msg != "" {
			history = append(history, msg)
		}
		if msg != "" && !strings.HasPrefix(msg, "/") {
			sc.Print("")
			sc.Print(ansiGreen + "› " + ansiReset + ansiBold + msg + ansiReset)
		}
		switch {
		case msg == "":
			continue
		case msg == "/exit" || msg == "/quit":
			sc.Leave()
			fmt.Fprintln(os.Stderr, "roscoe chat: bye")
			return 0
		case msg == "/session":
			if session == "" {
				sc.Print(ansiDim + "no session yet" + ansiReset)
			} else {
				sc.Printf("%ssession %s · resume later: roscoe chat --resume %s%s", ansiDim, session, session, ansiReset)
			}
			continue
		case msg == "/new":
			session, resumeFrom = "", ""
			*taskID = newTaskID()
			sc.Print(ansiDim + "started a fresh session" + ansiReset)
			continue
		case msg == "/help":
			for _, l := range []string{
				"/model <name>        middle-tier model for this fleet",
				"/harness claude|codex worker harness",
				"/autonomy 0-100      how much the quorum absorbs before texting you",
				"/subagents <n>       how many subagents each worker may run",
				"/config <path> [v]   read or set any roscoe.json value",
				"/cost                what this chat has spent",
				"/session             session id, to resume later",
				"/new                 start a fresh session",
				"/exit                leave (esc interrupts a running turn)",
			} {
				sc.Print(ansiDim + l + ansiReset)
			}
			continue
		case msg == "/cost":
			sc.Printf("%s%.4f USD across %d turns%s", ansiDim, spent, turns, ansiReset)
			continue
		case strings.HasPrefix(msg, "/model"):
			arg := strings.TrimSpace(strings.TrimPrefix(msg, "/model"))
			if arg == "" {
				sc.Printf("%smodel %s%s", ansiDim, cfg.Tiers.Middle.Model, ansiReset)
				continue
			}
			cfg.Tiers.Middle.Model = arg
			persist(sc, explicit, "tiers.middle.model", arg)
			continue
		case strings.HasPrefix(msg, "/harness"):
			arg := strings.TrimSpace(strings.TrimPrefix(msg, "/harness"))
			if arg == "" {
				sc.Printf("%sharness %s%s", ansiDim, harnessLabel, ansiReset)
				continue
			}
			if arg != "claude" && arg != "codex" {
				sc.Print(ansiDim + "usage: /harness claude|codex" + ansiReset)
				continue
			}
			cfg.Tiers.Middle.Harness, harnessLabel = arg, arg
			persist(sc, explicit, "tiers.middle.harness", arg)
			continue
		case strings.HasPrefix(msg, "/subagents"):
			arg := strings.TrimSpace(strings.TrimPrefix(msg, "/subagents"))
			if arg == "" {
				sc.Printf("%s%d subagents per worker%s", ansiDim, cfg.Tiers.Subagents.MaxConcurrent, ansiReset)
				continue
			}
			n, convErr := strconv.Atoi(arg)
			if convErr != nil || n < 1 || n > 64 {
				sc.Print(ansiDim + "usage: /subagents 1-64" + ansiReset)
				continue
			}
			cfg.Tiers.Subagents.MaxConcurrent = n
			persist(sc, explicit, "tiers.subagents.max_concurrent", arg)
			continue
		case strings.HasPrefix(msg, "/config"):
			parts := strings.Fields(strings.TrimPrefix(msg, "/config"))
			if len(parts) == 0 {
				sc.Print(ansiDim + "usage: /config <dotted.path> [value]" + ansiReset)
				continue
			}
			if len(parts) == 1 {
				if v, gErr := cfg.Get(parts[0]); gErr == nil {
					sc.Printf("%s%s = %v%s", ansiDim, parts[0], v, ansiReset)
				} else {
					sc.Printf("%s%v%s", ansiDim, gErr, ansiReset)
				}
				continue
			}
			value := strings.Join(parts[1:], " ")
			if sErr := cfg.SetPath(parts[0], value); sErr != nil {
				sc.Printf("%s%v%s", ansiDim, sErr, ansiReset)
				continue
			}
			persist(sc, explicit, parts[0], value)
			continue
		case strings.HasPrefix(msg, "/autonomy"):
			arg := strings.TrimSpace(strings.TrimPrefix(msg, "/autonomy"))
			if arg == "" {
				sc.Printf("%sautonomy %d · 100 means roscoe never interrupts you%s", ansiDim, cfg.Autonomy.Level, ansiReset)
				continue
			}
			level, convErr := strconv.Atoi(arg)
			if convErr != nil || level < 0 || level > 100 {
				sc.Print(ansiDim + "usage: /autonomy 0-100" + ansiReset)
				continue
			}
			cfg.Autonomy.Level = level
			persist(sc, explicit, "autonomy.level", arg)
			continue
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		var escPressed atomic.Bool
		go func() {
			if keys.WaitEsc(turnCtx) {
				escPressed.Store(true)
				sc.Print(ansiDim + "esc · interrupting this turn…" + ansiReset)
				cancelTurn()
			}
		}()

		res, runErr := worker.Run(turnCtx,
			worker.Task{ID: *taskID, Prompt: msg, Dir: *dir, Account: account, Token: token, Resume: session, ResumeFrom: resumeFrom},
			worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led, OnEvent: func(ev *streamjson.Event) {
				if ie, ok := ev.AsInit(); ok && ie.SessionID != "" {
					session = ie.SessionID
				}
				narrateTo(sc, ev)
			}},
		)
		cancelTurn()
		resumeFrom = "" // the transcript now lives in this task's config dir

		if res != nil && res.SessionID != "" {
			session = res.SessionID
		}
		switch {
		case escPressed.Load():
			sc.Print(ansiDim + "stopped · say what you want instead" + ansiReset)
		case ctx.Err() != nil:
			sc.Leave()
			fmt.Fprintln(os.Stderr, "roscoe chat: bye")
			return 0
		case runErr != nil:
			sc.Printf("%serror · %v%s", ansiGreen, runErr, ansiReset)
		case res != nil:
			for _, l := range strings.Split(strings.TrimSpace(res.Result), "\n") {
				sc.Print(l)
			}
			spent += res.TotalCostUSD
			turns++
			sc.Printf("%s%.4f USD this turn · %.4f total%s", ansiFaint, res.TotalCostUSD, spent, ansiReset)
		}
	}
}

// watchResize keeps the pinned prompt correct when the window changes.
func watchResize(sc *screen) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		sc.Resize()
	}
}

// firstLines renders a preview of replayed history: at most n lines and
// maxChars characters, with an ellipsis when it was cut.
func firstLines(text string, n, maxChars int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	cut := false
	if len(lines) > n {
		lines, cut = lines[:n], true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxChars {
		out, cut = out[:maxChars], true
	}
	if cut {
		out += " …"
	}
	return out
}

// persist writes one config value to the roscoe.json this chat was started
// with, so a setting changed mid-conversation survives the session.
func persist(sc *screen, explicit, path, value string) {
	target, err := resolveConfigPath(explicit)
	if err == nil {
		var saved *config.Config
		if saved, err = config.Load(target); err == nil {
			if err = saved.SetPath(path, value); err == nil {
				err = saved.Save(target)
			}
		}
	}
	if err != nil {
		sc.Printf("%s%s = %s · this session only (%v)%s", ansiDim, path, value, err, ansiReset)
		return
	}
	sc.Printf("%s%s = %s%s", ansiGreen, path, value, ansiReset)
}
