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

	// Completion knowledge lives here: command names, then per-command
	// arguments (config paths come from the config schema itself).
	comp := newChatCompleter(cfg)

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
		line, ok := keys.ReadLineOn(sc, "› ", history, comp)
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
			// Same text the prompt shows while you type, so the two cannot
			// drift apart.
			width := 0
			for _, c := range commands {
				if n := len(c + commandArgs[c]); n > width {
					width = n
				}
			}
			for _, c := range commands {
				sc.Printf("  %s%-*s%s  %s%s%s", ansiGreen, width, c+commandArgs[c], ansiReset,
					ansiDim, commandHelp[c], ansiReset)
			}
			sc.Printf("%sesc interrupts a running turn; up and down scroll%s", ansiFaint, ansiReset)
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
		case strings.HasPrefix(msg, "/effort"):
			arg := strings.TrimSpace(strings.TrimPrefix(msg, "/effort"))
			if arg == "" {
				cur := cfg.Tiers.Middle.Effort
				if cur == "" {
					cur = "claude's default"
				}
				sc.Printf("%seffort %s · one of %s%s", ansiDim, cur,
					strings.Join(config.EffortLevels(), ", "), ansiReset)
				continue
			}
			if !contains(config.EffortLevels(), arg) {
				sc.Printf("%susage: /effort %s%s", ansiDim, strings.Join(config.EffortLevels(), "|"), ansiReset)
				continue
			}
			cfg.Tiers.Middle.Effort = arg
			persist(sc, explicit, "tiers.middle.effort", arg)
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
				printConfigLevel(sc, cfg, "")
				continue
			}
			if len(parts) == 1 {
				// A branch lists what is under it; only a leaf has a value.
				if kids := cfg.ChildPaths(parts[0]); len(kids) > 0 {
					printConfigLevel(sc, cfg, parts[0])
					continue
				}
				if v, gErr := cfg.Get(parts[0]); gErr == nil {
					sc.Printf("%s%s = %v%s", ansiDim, parts[0], v, ansiReset)
					if d := config.Describe(parts[0]); d != "" {
						sc.Printf("%s%s%s", ansiFaint, d, ansiReset)
					}
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

// matching returns the candidates carrying prefix, in order.
// commandHelp is the one-line description shown above the input box while a
// slash command is being typed.
var commandHelp = map[string]string{
	"/autonomy":  "0-100; how much roscoe decides without asking you",
	"/config":    "read or set any setting by path; tab walks down a level",
	"/cost":      "what this chat has spent so far",
	"/effort":    "worker reasoning; ultracode plans a workflow per task and fans out",
	"/exit":      "leave the chat; the session keeps its id",
	"/harness":   "which CLI the workers run: claude or codex",
	"/help":      "the commands, with what each one does",
	"/model":     "the model your workers run",
	"/new":       "start a fresh session, leaving this one on disk",
	"/session":   "the current session id, for resuming later",
	"/subagents": "how many cheap subagents a worker may run at once",
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}

// parentPath is the dotted path one level up: "tiers.middle.effort" ->
// "tiers.middle", "tiers" -> "".
func parentPath(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i]
	}
	return ""
}

func newChatCompleter(cfg *config.Config) *completer {
	return &completer{
		candidates: func(input string) []string {
			fields := strings.Fields(input)
			token := currentToken(input)
			if !strings.HasPrefix(strings.TrimSpace(input), "/") {
				return nil
			}
			if len(fields) <= 1 && token != "" { // still naming the command
				return matching(commands, token)
			}
			if len(fields) == 0 {
				return commands
			}
			switch fields[0] {
			case "/config":
				// One level at a time: top-level keys first, then the
				// children of whatever has been typed. Dumping every leaf
				// path teaches nobody what the settings are.
				if len(fields) > 2 || (len(fields) == 2 && token == "") {
					return nil // past the path; typing the value
				}
				return matching(cfg.ChildPaths(parentPath(token)), token)
			case "/harness":
				return matching([]string{"claude", "codex"}, token)
			case "/effort":
				return matching(config.EffortLevels(), token)
			case "/model":
				return matching(modelChoices(cfg), token)
			case "/autonomy":
				return matching([]string{"0", "25", "50", "75", "90", "100"}, token)
			case "/subagents":
				return matching([]string{"1", "2", "4", "8", "12", "16", "24"}, token)
			}
			return nil
		},
		descends: func(candidate string) bool {
			return len(cfg.ChildPaths(candidate)) > 0
		},
		note: func(input string) string {
			fields := strings.Fields(input)
			if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
				return ""
			}
			cmd := fields[0]
			if len(fields) == 1 && !strings.HasSuffix(input, " ") {
				if d := commandHelp[cmd]; d != "" {
					return cmd + ": " + d
				}
				if m := matching(commands, cmd); len(m) == 1 {
					return m[0] + ": " + commandHelp[m[0]]
				}
				return ""
			}
			if cmd == "/config" && len(fields) >= 2 {
				path := fields[1]
				if d := config.Describe(path); d != "" {
					return strings.TrimSuffix(path, ".") + ": " + d
				}
				// Partway through a name: describe it once it is unambiguous.
				if m := matching(cfg.ChildPaths(parentPath(path)), path); len(m) == 1 {
					if d := config.Describe(m[0]); d != "" {
						return m[0] + ": " + d
					}
				}
				return ""
			}
			if d := commandHelp[cmd]; d != "" {
				return cmd + ": " + d
			}
			return ""
		},
	}
}

// printConfigLevel lists one level of the config: the children of prefix
// (top-level keys when it is empty), each with its description and, for
// leaves, its current value.
func printConfigLevel(sc *screen, cfg *config.Config, prefix string) {
	kids := cfg.ChildPaths(prefix)
	if len(kids) == 0 {
		sc.Printf("%sno settings under %s%s", ansiDim, prefix, ansiReset)
		return
	}
	if d := config.Describe(prefix); d != "" {
		sc.Printf("%s%s%s", ansiFaint, d, ansiReset)
	}
	width := 0
	for _, k := range kids {
		if n := len(strings.TrimPrefix(k, prefix+".")); n > width {
			width = n
		}
	}
	for _, k := range kids {
		name := strings.TrimPrefix(k, prefix+".")
		detail := config.Describe(k)
		if len(cfg.ChildPaths(k)) == 0 {
			if v, err := cfg.Get(k); err == nil {
				detail = fmt.Sprintf("%v", v)
				if d := config.Describe(k); d != "" {
					detail = fmt.Sprintf("%v  %s%s", v, ansiFaint, d)
				}
			}
		}
		sc.Printf("  %s%-*s%s  %s%s", ansiGreen, width, name, ansiReset, ansiDim+detail, ansiReset)
	}
	if prefix == "" {
		sc.Printf("%s/config <key> to go deeper, /config <path> <value> to set%s", ansiFaint, ansiReset)
	}
}

// commandArgs is what each command takes, appended to its name in /help.
var commandArgs = map[string]string{
	"/autonomy":  " 0-100",
	"/config":    " <path> [value]",
	"/effort":    " <level>",
	"/harness":   " claude|codex",
	"/model":     " <name>",
	"/subagents": " <n>",
}

// commands are the slash commands offered in chat, in the order they
// complete.
var commands = []string{"/autonomy", "/config", "/cost", "/effort", "/exit", "/harness", "/help", "/model", "/new", "/session", "/subagents"}

func matching(candidates []string, prefix string) []string {
	if prefix == "" {
		return candidates
	}
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// modelChoices offers the aliases claude understands plus whatever this
// config already names, so switching back is a tab away.
func modelChoices(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range []string{"sonnet", "opus", "haiku"} {
		add(v)
	}
	add(cfg.Tiers.Subagents.VirtualModel)
	add(cfg.Tiers.Subagents.Model)
	add(cfg.Tiers.Middle.Model)
	add(cfg.Tiers.Main.Model)
	return out
}
