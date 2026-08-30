package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/notify"
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
	const use = "usage: roscoe config get <dotted.path> | roscoe config set <dotted.path> <value>"
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, use)
		return 2
	}
	sub, rest := args[0], args[1:]

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

	switch sub {
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
	_ = fl.Parse(args)
	rest := fl.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, `usage: roscoe run "<prompt>" [--task-id X] [--dir D] [--resume <session-id>]`)
		return 2
	}
	prompt := rest[0]

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
	if len(rest) > 1 { // accept flags after the prompt too, per the synopsis
		_ = fl.Parse(rest[1:])
		if fl.NArg() > 0 {
			fmt.Fprintf(os.Stderr, "roscoe run: unexpected arguments %q (quote the prompt)\n", fl.Args())
			return 2
		}
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
	errCh := make(chan error, 1)
	go func() { errCh <- r.ListenAndServe(rctx) }()

	addr, err := waitHealthz(ctx, r.Addr, errCh, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[router] listening on %s\n", addr)

	account, token := resolveMiddleAccount(cfg, env)
	if account == "" {
		fmt.Fprintln(os.Stderr, "[account] no enabled middle-tier account with a resolvable env: token; relying on claude's own auth")
	} else {
		fmt.Fprintf(os.Stderr, "[account] %s\n", account)
	}
	fmt.Fprintf(os.Stderr, "[task] %s dir=%s\n", *taskID, *dir)

	res, err := worker.Run(ctx,
		worker.Task{ID: *taskID, Prompt: prompt, Dir: *dir, Account: account, Token: token, Resume: *resume, ResumeFrom: resumeFrom},
		worker.Opts{Cfg: cfg, RouterAddr: addr, Ledger: led, OnEvent: narrate},
	)

	// Stop the router before reporting so nothing interleaves with the output.
	rcancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe run: %v\n", err)
		return 1
	}
	if res == nil { // contract says non-nil on success; guard anyway
		fmt.Fprintln(os.Stderr, "roscoe run: worker returned no result event")
		return 1
	}

	out := res.Result
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	os.Stdout.WriteString(out)

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
	for _, want := range cfg.Tiers.Middle.Accounts {
		for _, a := range cfg.Accounts {
			if a.Name != want {
				continue
			}
			if a.Enabled != nil && !*a.Enabled {
				continue
			}
			v, ok := strings.CutPrefix(a.TokenRef, "env:")
			if !ok {
				continue
			}
			if t := env[v]; t != "" {
				return a.Name, t
			}
			if t := os.Getenv(v); t != "" {
				return a.Name, t
			}
		}
	}
	return "", ""
}

// narrate prints one readable line per stream-json event to stderr.
func narrate(ev *streamjson.Event) {
	if ev == nil {
		return
	}
	w := os.Stderr
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
		switch {
		case text != "":
			fmt.Fprintf(w, "[assistant] %s\n", snippet(text))
		case len(tools) > 0:
			fmt.Fprintf(w, "[assistant] tool_use: %s\n", strings.Join(tools, ", "))
		default:
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

	default:
		if ev.Subtype != "" {
			fmt.Fprintf(w, "[%s/%s]\n", ev.Type, ev.Subtype)
		} else {
			fmt.Fprintf(w, "[%s]\n", ev.Type)
		}
	}
}

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
	phone := fl.String("phone", "", "your mobile in E.164 form, e.g. +15551234567")
	baseURL := fl.String("base-url", relay.DefaultBaseURL, "relay control plane")
	_ = fl.Parse(args)
	if *phone == "" {
		fmt.Fprintln(os.Stderr, "roscoe upgrade: --phone is required (E.164, e.g. --phone +15551234567) — it's the number your escalation texts go to")
		return 2
	}

	// Already linked and refreshable → nothing to do.
	if creds, err := relay.LoadCreds(); err == nil {
		if err := creds.EnsureFresh(ctx); err == nil {
			fmt.Fprintf(os.Stderr, "roscoe upgrade: already linked (client %s…, phone %s). Use \"roscoe relay status\" to inspect.\n", creds.ClientID[:8], creds.Phone)
			return 0
		}
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
func cmdRelay(ctx context.Context, _ string, args []string) int {
	if len(args) == 0 || (args[0] != "status" && args[0] != "listen") {
		fmt.Fprintln(os.Stderr, "usage: roscoe relay status | roscoe relay listen")
		return 2
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
