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

	cap := newCapture()
	defer cap.Close()

	out := map[string]string{}
	var firstErr error
	for _, alias := range todo {
		cap.reset()
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := run(pctx, claudeBin, cap.URL(), alias)
		cancel()
		got := cap.model()
		if got == "" {
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", alias, err)
			}
			continue
		}
		out[alias] = got
		c.Learn(provider, alias, got)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// probeTimeout bounds one alias. The harness normally gives up within a
// second or two of the refused request; anything longer is a stuck process.
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
	// Refuse in the API's own error shape so the harness stops cleanly rather
	// than retrying.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"roscoe model probe"}}`)
}

func (c *capture) model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func (c *capture) reset() {
	c.mu.Lock()
	c.got = ""
	c.mu.Unlock()
}

func (c *capture) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.srv.Shutdown(ctx)
}
