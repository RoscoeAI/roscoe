package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/models"
	"roscoe.sh/roscoe/internal/router"
	"roscoe.sh/roscoe/internal/sessions"
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
	last := fl.Bool("last", false, "resume the most recent session")
	pick := fl.Bool("pick", false, "choose a recent session to resume from a list")
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

	// --last and --pick resolve to a session id the same way --resume takes
	// one, so everything after this point is one code path.
	if *last || *pick {
		id, ok := chooseSession(cfg, *pick)
		if !ok {
			return 1
		}
		*resume = id
	}

	// One session for the whole chat: the first turn may import an existing
	// transcript, every later turn resumes what the worker just wrote.
	session, resumeFrom := "", ""
	resumeBudget := 0 // 0 = default window; halved when the model refuses it as too long
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
	defer recordRouterTotals(led, r, cfg, *taskID)
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
	sc.Print(fleetLine(cfg))

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

	// pendingPrompt is what the operator typed while the last turn was still
	// running. It goes straight through rather than making them retype it.
	pendingPrompt := ""
	for {
		var line string
		var ok bool
		if pendingPrompt != "" {
			line, ok, pendingPrompt = pendingPrompt, true, ""
		} else {
			line, ok = keys.ReadLineOn(sc, "› ", "", history, comp)
		}
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
		case msg == "/settings":
			runSettings(sc, keys, cfg, explicit)
			harnessLabel = harnessOf(cfg)
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

		// A mistyped slash command must not become a prompt. Falling through
		// spawns a worker, costs a turn, and answers with Claude Code's own
		// "unknown command" text, which reads as though roscoe said it.
		if strings.HasPrefix(msg, "/") {
			sc.Printf("%sunknown command %s%s", ansiDim, strings.Fields(msg)[0], ansiReset)
			if near := nearestCommand(strings.Fields(msg)[0]); near != "" {
				sc.Printf("%sdid you mean %s%s%s", ansiDim, ansiGreen, near, ansiReset)
			} else {
				sc.Printf("%s/help lists them%s", ansiFaint, ansiReset)
			}
			continue
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		// The box stays live for the whole turn: type to queue, tab to steer,
		// esc to stop. Waiting with a cursor that swallows input is how a slow
		// turn becomes indistinguishable from a dead one.
		ti := &turnInput{}
		inputDone := make(chan pending, 1)
		go func() { inputDone <- ti.run(turnCtx, sc, keys, cancelTurn) }()
		nar := &narrator{sc: sc} // per turn: streaming state must not leak across turns

		opts := worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led,
			OnNotice: func(m string) { sc.Printf("%s%s%s", ansiDim, m, ansiReset) },
			OnEvent: func(ev *streamjson.Event) {
				if ie, ok := ev.AsInit(); ok {
					if ie.SessionID != "" {
						session = ie.SessionID
					}
					// The harness just told us what the alias resolves to.
					learnResolvedModel(cfg, cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model, ie.Model)
				}
				narrateTo(sc, ev)
			}}
		var res *streamjson.ResultEvent
		var runErr error
		// A resumed window the model refuses as too long costs nothing and
		// says so in the result. Halve the window and send the same message
		// again, a few times, rather than leaving the operator with a stalled
		// box and an error that is roscoe's to fix.
		for attempt := 0; ; attempt++ {
			res, runErr = worker.Run(turnCtx,
				worker.Task{ID: *taskID, Prompt: msg, Dir: *dir, Account: account, Token: token,
					Resume: session, ResumeFrom: resumeFrom, ResumeBudget: resumeBudget},
				opts)
			if runErr != nil || res == nil || !res.IsError || !worker.PromptTooLong(res.Result) || attempt >= 3 || turnCtx.Err() != nil {
				break
			}
			dir, derr := worker.SessionConfigDir(cfg, *taskID, token)
			if derr != nil {
				break
			}
			path, ferr := worker.FindSession(dir, session)
			if ferr != nil {
				break
			}
			resumeBudget = worker.HalveBudget(resumeBudget)
			resumeFrom = path
			sc.Printf("%sthe model refused that much history · retrying with the most recent %dKB%s", ansiDim, resumeBudget/1024, ansiReset)
		}
		cancelTurn()
		acted := <-inputDone
		sc.SetPrompt("", "", "", "")
		resumeFrom = "" // the transcript now lives in this task's config dir

		if res != nil && res.SessionID != "" {
			session = res.SessionID
		}
		// The import trims once; `--resume` then appends every turn. Left
		// alone the transcript grows until a turn cannot resume at all, and
		// every turn before that pays to carry it. Re-trim between turns
		// through the same path the first import used: set resumeFrom and
		// the next worker.Run imports the session into a fresh, smaller id.
		if session != "" {
			if dir, derr := worker.SessionConfigDir(cfg, *taskID, token); derr == nil {
				if path, ferr := worker.FindSession(dir, session); ferr == nil && worker.OversizedBy(path, resumeBudget) {
					resumeFrom = path
					sc.Printf("%stranscript is large · trimming to recent messages before the next turn%s", ansiFaint, ansiReset)
				}
			}
		}
		switch {
		case acted.Esc:
			sc.Print(ansiDim + "stopped · say what you want instead" + ansiReset)
		case ctx.Err() != nil:
			sc.Leave()
			fmt.Fprintln(os.Stderr, "roscoe chat: bye")
			return 0
		case runErr != nil:
			sc.Printf("%serror · %v%s", ansiGreen, runErr, ansiReset)
		case res != nil:
			// The reply was watched as it was written; printing it again
			// would show it twice. A turn that streamed nothing (tool-only, or
			// an older harness without partial messages) still gets it here.
			sc.EndStream()
			if !nar.StreamedAny() {
				for _, l := range strings.Split(strings.TrimSpace(res.Result), "\n") {
					sc.Print(l)
				}
			}
			spent += res.TotalCostUSD
			turns++
			sc.Printf("%s%.4f USD this turn · %.4f total%s", ansiFaint, res.TotalCostUSD, spent, ansiReset)
		}

		// Anything the operator committed mid-turn becomes the next prompt,
		// without them having to retype it now that the turn is over.
		if next := acted.next(); next != "" {
			pendingPrompt = next
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
// nearestCommand finds the closest known command to a mistyped one: a prefix
// match first, then one within a small edit distance. It returns "" rather
// than guess wildly, because a wrong suggestion is worse than none.
func nearestCommand(typed string) string {
	best, bestD := "", 3 // no suggestion beyond two edits
	for _, c := range commands {
		if strings.HasPrefix(c, typed) || strings.HasPrefix(typed, c) {
			return c
		}
		if d := editDistance(typed, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

// editDistance is Levenshtein over two short strings, iterative with one row.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

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
	"/settings":  "every tier's model and effort on one screen, arrows to change",
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

// fleetLine states all three tiers in one line, so what is running is never
// something you have to go and look up.
func fleetLine(cfg *config.Config) string {
	effort := cfg.Tiers.Middle.Effort
	if effort == "" {
		effort = "default effort"
	}
	mid := cfg.Tiers.Middle.Model
	if harnessOf(cfg) == "codex" {
		mid, _ = models.CodexModel(mid)
	}
	return fmt.Sprintf("%stier 1 %s · tier 2 %s %s · tier 3 %s %d wide · autonomy %d%s   %s/settings%s",
		ansiFaint, cfg.Tiers.Main.Model,
		mid, effort,
		shortModel(cfg.Tiers.Subagents.Model), cfg.Tiers.Subagents.MaxConcurrent,
		cfg.Autonomy.Level, ansiReset, ansiDim, ansiReset)
}

// shortModel drops a vendor prefix for display: zai-org/GLM-5.3-Flash reads
// as GLM-5.3-Flash.
func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
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
var commands = []string{"/autonomy", "/config", "/cost", "/effort", "/exit", "/harness", "/help", "/model", "/new", "/session", "/settings", "/subagents"}

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
// modelChoicesWith offers the concrete models a provider publishes alongside
// the aliases, so completion can suggest what actually exists rather than only
// the three names roscoe happens to hardcode.
func modelChoicesWith(cfg *config.Config, cat *models.Catalog) []string {
	out := modelChoices(cfg)
	if cat == nil {
		return out
	}
	seen := map[string]bool{}
	for _, v := range out {
		seen[v] = true
	}
	for _, prov := range []string{cfg.Tiers.Main.Provider, cfg.Tiers.Middle.Provider, cfg.Tiers.Subagents.Provider} {
		for _, m := range cat.Models(prov) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

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

// chooseSession returns the most recent resumable session, or with pick set,
// shows the recent ones and reads a number. It runs before the screen takes
// over, so it prints plainly to stderr and reads a line from the terminal.
func chooseSession(cfg *config.Config, pick bool) (string, bool) {
	dir := runsDir(cfg)
	if !pick {
		s, ok := sessions.Latest(dir, enrichSession)
		if !ok {
			fmt.Fprintln(os.Stderr, "roscoe chat: no session to resume yet")
			return "", false
		}
		fmt.Fprintf(os.Stderr, "[resume] latest: %s · %s\n", s.ID[:8], oneLineOf(s.About, 70))
		return s.ID, true
	}
	list, err := sessions.List(dir, 12, enrichSession)
	if err != nil || len(list) == 0 {
		fmt.Fprintln(os.Stderr, "roscoe chat: no sessions to choose from")
		return "", false
	}
	var resumable []sessions.Session
	for _, s := range list {
		if s.Resumable() {
			resumable = append(resumable, s)
		}
	}
	if len(resumable) == 0 {
		fmt.Fprintln(os.Stderr, "roscoe chat: no resumable sessions")
		return "", false
	}
	now := time.Now()
	for i, s := range resumable {
		about := oneLineOf(s.About, 60)
		if about == "" {
			about = shortDir(s.Dir)
		}
		fmt.Fprintf(os.Stderr, "  %2d  %-9s %-9s $%-7.2f %s\n", i+1, sessions.Age(s.Ended, now), s.ID[:8], s.CostUSD, about)
	}
	fmt.Fprint(os.Stderr, "resume which? (number, enter for 1, esc to quit) ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return resumable[0].ID, true
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(resumable) {
		fmt.Fprintln(os.Stderr, "roscoe chat: not a listed number")
		return "", false
	}
	return resumable[n-1].ID, true
}
