// Package smoke runs the preflight checks gating any fan-out: config,
// env, an in-process router on an ephemeral port, live DeepInfra pings
// through it, and (in full mode) a real claude harness probe.
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/router"
)

type Check struct {
	Name    string
	OK      bool
	Skipped bool
	Detail  string
}

const liveTimeout = 60 * time.Second

// Run executes every check in order; each is independent — a failure never
// stops the rest. env is the loaded env file; when nil, Run loads
// cfg.EnvFile itself (check 2 reports the outcome either way).
func Run(ctx context.Context, cfg *config.Config, env map[string]string, full bool) []Check {
	var checks []Check

	// 1. config-validate
	checks = append(checks, checkConfig(cfg))

	// 2. env-file
	envCheck, effEnv := checkEnv(cfg, env)
	checks = append(checks, envCheck)
	env = effEnv

	// 3. router-start (Port 0, in-process)
	rt, rtCheck := startRouter(ctx, cfg, env)
	checks = append(checks, rtCheck)
	if rt != nil {
		defer rt.stop()
	}

	virtual := cfg.Tiers.Subagents.VirtualModel

	// 4. tier3 count_tokens through the router (live DeepInfra).
	checks = append(checks, routerCheck(rt, "tier3-count-tokens", func() Check {
		body := mustJSON(map[string]any{
			"model":    virtual,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		return postExpect200(ctx, "tier3-count-tokens",
			"http://"+rt.addr+"/v1/messages/count_tokens", body, nil)
	}))

	// 5. tier3 1-token live ping (live DeepInfra).
	checks = append(checks, routerCheck(rt, "tier3-live-ping", func() Check {
		body := mustJSON(map[string]any{
			"model":      virtual,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		return postExpect200(ctx, "tier3-live-ping",
			"http://"+rt.addr+"/v1/messages", body, nil)
	}))

	// 6. anthropic-leg count_tokens, only when a subscription token is
	// resolvable from env (env: refs only in slice 1).
	checks = append(checks, checkAnthropicLeg(ctx, cfg, env, rt))

	// 7. claude binary present.
	checks = append(checks, checkClaudeBin(ctx))

	// 8. full-mode harness probe.
	checks = append(checks, checkHarnessProbe(ctx, cfg, rt, full))

	return checks
}

func checkConfig(cfg *config.Config) Check {
	c := Check{Name: "config-validate"}
	errs := cfg.Validate()
	if len(errs) == 0 {
		c.OK = true
		return c
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	c.Detail = strings.Join(msgs, "; ")
	return c
}

func checkEnv(cfg *config.Config, env map[string]string) (Check, map[string]string) {
	c := Check{Name: "env-file"}
	if env == nil {
		loaded, err := config.LoadEnvFile(config.ExpandPath(cfg.EnvFile))
		if err != nil {
			c.Detail = fmt.Sprintf("load %s: %v", cfg.EnvFile, err)
			return c, map[string]string{}
		}
		env = loaded
	}
	if env["DEEP_INFRA_API_KEY"] == "" {
		c.Detail = "DEEP_INFRA_API_KEY not set"
		return c, env
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%d vars, DEEP_INFRA_API_KEY present", len(env))
	return c, env
}

// runningRouter is the in-process router started for the live checks.
type runningRouter struct {
	addr string
	stop func()
}

func startRouter(ctx context.Context, cfg *config.Config, env map[string]string) (*runningRouter, Check) {
	c := Check{Name: "router-start"}
	// Options.Port==0 means "use cfg.Router.Port", so force the ephemeral
	// port through a private shallow copy of the config.
	cc := *cfg
	cc.Router.Port = 0
	if cc.Router.Bind == "" {
		cc.Router.Bind = "127.0.0.1"
	}
	r, err := router.New(router.Options{Cfg: &cc, Env: env})
	if err != nil {
		c.Detail = fmt.Sprintf("router.New: %v", err)
		return nil, c
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.ListenAndServe(rctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}

	// The listener is created inside ListenAndServe; poll until Addr()
	// reports a real port, then confirm /healthz.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			cancel()
			c.Detail = fmt.Sprintf("router exited before ready: %v", err)
			return nil, c
		default:
		}
		if addr := r.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			if status, _, err := doRequest(ctx, http.MethodGet, "http://"+addr+"/healthz", nil, nil, 5*time.Second); err == nil && status == http.StatusOK {
				c.OK = true
				c.Detail = addr
				return &runningRouter{addr: addr, stop: stop}, c
			} else if err == nil {
				stop()
				c.Detail = fmt.Sprintf("healthz status %d", status)
				return nil, c
			}
		}
		if time.Now().After(deadline) {
			stop()
			c.Detail = "not ready after 5s"
			return nil, c
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// routerCheck skips a router-dependent check when the router never came up.
func routerCheck(rt *runningRouter, name string, run func() Check) Check {
	if rt == nil {
		return Check{Name: name, Skipped: true, Detail: "router did not start"}
	}
	return run()
}

func checkAnthropicLeg(ctx context.Context, cfg *config.Config, env map[string]string, rt *runningRouter) Check {
	const name = "anthropic-count-tokens"
	account, token := subscriptionEnvToken(cfg, env)
	if token == "" {
		return Check{Name: name, Skipped: true, Detail: "no enabled claude-subscription account with env: token_ref resolvable"}
	}
	if rt == nil {
		return Check{Name: name, Skipped: true, Detail: "router did not start"}
	}
	// A non-virtual model routes to the default tier's provider
	// (auth "account": the router forwards our auth headers verbatim).
	// Subscription OAuth requests need the oauth anthropic-beta header.
	body := mustJSON(map[string]any{
		"model":    cfg.Tiers.Middle.Model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	c := postExpect200(ctx, name, "http://"+rt.addr+"/v1/messages/count_tokens", body, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
	})
	if c.OK {
		c.Detail = strings.TrimSpace("account " + account + " " + c.Detail)
	}
	return c
}

// subscriptionEnvToken finds the first enabled claude-subscription account
// whose token_ref is an env: reference present in env.
func subscriptionEnvToken(cfg *config.Config, env map[string]string) (name, token string) {
	for _, a := range cfg.Accounts {
		if a.Kind != "claude-subscription" {
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
	}
	return "", ""
}

func checkClaudeBin(ctx context.Context) Check {
	c := Check{Name: "claude-bin"}
	path, err := exec.LookPath("claude")
	if err != nil {
		c.Detail = "claude not found in PATH"
		return c
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, path, "--version").CombinedOutput()
	if err != nil {
		c.Detail = fmt.Sprintf("%s --version: %v: %s", path, err, oneLine(out, 200))
		return c
	}
	c.OK = true
	c.Detail = oneLine(out, 200)
	return c
}

func checkHarnessProbe(ctx context.Context, cfg *config.Config, rt *runningRouter, full bool) Check {
	const name = "harness-probe"
	if !full {
		return Check{Name: name, Skipped: true, Detail: "run with --full"}
	}
	if rt == nil {
		return Check{Name: name, Skipped: true, Detail: "router did not start"}
	}
	c := Check{Name: name}
	bin, err := exec.LookPath("claude")
	if err != nil {
		c.Detail = "claude not found in PATH"
		return c
	}
	ccfg, err := os.MkdirTemp("", "roscoe-smoke-ccfg-")
	if err != nil {
		c.Detail = fmt.Sprintf("mktemp: %v", err)
		return c
	}
	defer os.RemoveAll(ccfg)

	pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(pctx, bin,
		"-p", "Reply with exactly: pong",
		"--output-format", "json",
		"--model", cfg.Tiers.Subagents.VirtualModel,
		"--permission-mode", "bypassPermissions",
		"--max-budget-usd", "0.25",
	)
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL=http://"+rt.addr,
		"ANTHROPIC_AUTH_TOKEN=roscoe-local",
		"CLAUDE_CONFIG_DIR="+ccfg,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Round(time.Millisecond)
	if runErr != nil {
		c.Detail = fmt.Sprintf("%v: stdout=%s stderr=%s", runErr, oneLine(stdout.Bytes(), 200), oneLine(stderr.Bytes(), 200))
		return c
	}
	// --output-format json emits one result object with a "result" field;
	// fall back to raw output if the shape ever changes.
	result := stdout.String()
	var parsed struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(stdout.Bytes(), &parsed) == nil && parsed.Result != "" {
		result = parsed.Result
	}
	if !strings.Contains(result, "pong") {
		c.Detail = fmt.Sprintf("no \"pong\" in result: %s", oneLine([]byte(result), 200))
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("pong in %s", elapsed)
	return c
}

// postExpect200 POSTs a JSON body (60s timeout) and passes only on 200,
// putting the HTTP status plus a response excerpt in Detail otherwise.
func postExpect200(ctx context.Context, name, url string, body []byte, extraHdr map[string]string) Check {
	c := Check{Name: name}
	start := time.Now()
	status, respBody, err := doRequest(ctx, http.MethodPost, url, body, extraHdr, liveTimeout)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	if status != http.StatusOK {
		c.Detail = fmt.Sprintf("status %d: %s", status, oneLine(respBody, 200))
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("200 in %s", elapsed)
	return c
}

func doRequest(ctx context.Context, method, url string, body []byte, extraHdr map[string]string, timeout time.Duration) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, url, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range extraHdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable on a programming error (unmarshalable literal).
		panic(fmt.Sprintf("smoke: marshal request body: %v", err))
	}
	return b
}

// oneLine collapses whitespace and truncates to max bytes for table detail.
func oneLine(b []byte, max int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
