package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ResolveViaHarness asks the installed claude binary what each alias means.
//
// Claude Code resolves "opus" and "sonnet" client-side and puts the concrete
// id on the wire, so the cheapest authoritative resolver is the binary itself.
// Point it at a local endpoint, ask for one turn, and read the model field out
// of the request it composes. The endpoint refuses the request, so this costs
// no tokens, and the request is built before any credential is checked, so it
// needs no working login. It is the only source that can answer for tier 1,
// where nothing ever runs through roscoe.
//
// Aliases that already look concrete (contain a digit) are skipped: there is
// nothing to learn and the probe would only cost time.
func (c *Catalog) ResolveViaHarness(ctx context.Context, provider, claudeBin string, aliases []string) (map[string]string, error) {
	return c.resolveViaHarness(ctx, provider, claudeBin, aliases, runClaudeProbe)
}

// probeRunner launches the harness once against base and returns when it has
// exited. Tests substitute one that POSTs to base directly.
type probeRunner func(ctx context.Context, bin, base, alias string) error

func (c *Catalog) resolveViaHarness(ctx context.Context, provider, claudeBin string, aliases []string, run probeRunner) (map[string]string, error) {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	var todo []string
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a != "" && !strings.ContainsAny(a, "0123456789") {
			todo = append(todo, a)
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}

	// One capture per alias, all probes at once. Each probe is a claude
	// process that does nothing but compose one request, so a handful in
	// parallel costs no more than one; the bound keeps a long alias list from
	// forking a process storm.
	type result struct {
		alias, model string
		err          error
	}
	results := make([]result, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeParallelism)
	for i, alias := range todo {
		wg.Add(1)
		go func(i int, alias string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cap := newCapture()
			defer cap.Close()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			err := run(pctx, claudeBin, cap.URL(), alias)
			cancel()
			results[i] = result{alias: alias, model: cap.model(), err: err}
		}(i, alias)
	}
	wg.Wait()

	out := map[string]string{}
	var firstErr error
	for _, r := range results { // in alias order, so Learn and errors are deterministic
		if r.model == "" {
			if r.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", r.alias, r.err)
			}
			continue
		}
		out[r.alias] = r.model
		c.Learn(provider, r.alias, r.model)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// probeParallelism bounds concurrent claude probes.
const probeParallelism = 4

// probeTimeout bounds one alias. Refused with a 400 the harness exits in
// under two seconds; anything longer is a stuck process.
const probeTimeout = 20 * time.Second

// runClaudeProbe is the real launcher. The lean flags keep startup fast and
// stop the probe loading the operator's MCP servers just to be refused.
func runClaudeProbe(ctx context.Context, bin, base, alias string) error {
	cmd := exec.CommandContext(ctx, bin,
		"-p", "ok", "--model", alias, "--max-turns", "1",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "project",
	)
	cmd.Env = append(cmd.Environ(), "ANTHROPIC_BASE_URL="+base)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	// A non-zero exit is expected: we refused its only request.
	_ = cmd.Run()
	return ctx.Err()
}

// capture is the local endpoint a probe is pointed at. It records the model
// named in the first POST body and refuses the request.
type capture struct {
	ln  net.Listener
	srv *http.Server
	mu  sync.Mutex
	got string
}

func newCapture() *capture {
	c := &capture{}
	c.ln, _ = net.Listen("tcp", "127.0.0.1:0")
	c.srv = &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second}
	go c.srv.Serve(c.ln)
	return c
}

func (c *capture) URL() string { return "http://" + c.ln.Addr().String() }

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &req) == nil && req.Model != "" {
			c.mu.Lock()
			if c.got == "" {
				c.got = req.Model
			}
			c.mu.Unlock()
		}
	}
	// Refuse in the API's own error shape, as a 400. Measured on claude
	// 2.1.251: a 401 is retried six times and the process is still running at
	// 25s, so every alias cost the whole probe timeout; a 400
	// invalid_request_error gets one request and an exit in 1.6s.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"roscoe model probe"}}`)
}

func (c *capture) model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func (c *capture) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.srv.Shutdown(ctx)
}
