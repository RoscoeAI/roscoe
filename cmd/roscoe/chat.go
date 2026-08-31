package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	fmt.Fprintf(os.Stderr, "roscoe chat · %s · %s in %s\n", cfg.Tiers.Middle.Model, harnessLabel, *dir)
	fmt.Fprintln(os.Stderr, "type a message and press enter · esc interrupts a turn · /exit to quit")

	for {
		line, ok := keys.ReadLine("\nyou> ")
		if !ok {
			fmt.Fprintln(os.Stderr, "\nroscoe chat: bye")
			return 0
		}
		msg := strings.TrimSpace(line)
		switch {
		case msg == "":
			continue
		case msg == "/exit" || msg == "/quit":
			fmt.Fprintln(os.Stderr, "roscoe chat: bye")
			return 0
		case msg == "/session":
			if session == "" {
				fmt.Fprintln(os.Stderr, "no session yet")
			} else {
				fmt.Fprintf(os.Stderr, "session %s · resume later: roscoe chat --resume %s\n", session, session)
			}
			continue
		case msg == "/new":
			session, resumeFrom = "", ""
			*taskID = newTaskID()
			fmt.Fprintln(os.Stderr, "started a fresh session")
			continue
		case msg == "/help":
			fmt.Fprintln(os.Stderr, "/exit  /new  /session  /help · esc interrupts the current turn")
			continue
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		var escPressed atomic.Bool
		go func() {
			if keys.WaitEsc(turnCtx) {
				escPressed.Store(true)
				fmt.Fprintln(os.Stderr, "\n[esc] interrupting this turn…")
				cancelTurn()
			}
		}()

		res, runErr := worker.Run(turnCtx,
			worker.Task{ID: *taskID, Prompt: msg, Dir: *dir, Account: account, Token: token, Resume: session, ResumeFrom: resumeFrom},
			worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led, OnEvent: func(ev *streamjson.Event) {
				if ie, ok := ev.AsInit(); ok && ie.SessionID != "" {
					session = ie.SessionID
				}
				narrate(ev)
			}},
		)
		cancelTurn()
		resumeFrom = "" // the transcript now lives in this task's config dir

		if res != nil && res.SessionID != "" {
			session = res.SessionID
		}
		switch {
		case escPressed.Load():
			fmt.Fprintln(os.Stderr, "[esc] stopped. Say what you want instead.")
		case ctx.Err() != nil:
			fmt.Fprintln(os.Stderr, "\nroscoe chat: bye")
			return 0
		case runErr != nil:
			fmt.Fprintf(os.Stderr, "roscoe chat: %v\n", runErr)
		case res != nil:
			out := strings.TrimSpace(res.Result)
			if out != "" {
				fmt.Printf("\n%s\n", out)
			}
			fmt.Fprintf(os.Stderr, "[turn] cost=$%.4f\n", res.TotalCostUSD)
		}
	}
}

