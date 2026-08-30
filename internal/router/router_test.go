// Tests for the router package: model dispatch, header policy, local
// count_tokens estimation, error shapes, streaming, and listener binding.
package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/router"
)

// upstream is a capturing httptest Anthropic-protocol upstream.
type upstream struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    int
	body    []byte
	header  http.Header
	path    string
	rawq    string
	respond func(w http.ResponseWriter, r *http.Request) // optional override
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream: read body: %v", err)
		}
		u.mu.Lock()
		u.hits++
		u.body = body
		u.header = r.Header.Clone()
		u.path = r.URL.Path
		u.rawq = r.URL.RawQuery
		respond := u.respond
		u.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message"}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) snapshot() (hits int, body []byte, header http.Header, path, rawq string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits, u.body, u.header, u.path, u.rawq
}

// baseConfig routes default traffic to defURL (auth "account") and the
// virtual subagent tier to subURL (auth "env:ZAI_KEY").
func baseConfig(defURL, subURL string) *config.Config {
	return &config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Protocol: "anthropic", BaseURL: defURL, Auth: "account"},
			"zai":       {Protocol: "anthropic", BaseURL: subURL, Auth: "env:ZAI_KEY"},
		},
		Tiers: config.Tiers{
			Main:      config.MainTier{Provider: "anthropic"},
			Middle:    config.MiddleTier{Provider: "anthropic"},
			Subagents: config.SubagentTier{Provider: "zai", Model: "glm-4.6", VirtualModel: "roscoe/tier3"},
		},
		Router: config.RouterCfg{DefaultRoute: "middle"},
	}
}

// newTestRouter builds a Router and serves its handler on an httptest server,
// returning the router and the base URL to hit.
func newTestRouter(t *testing.T, cfg *config.Config, env map[string]string) (*router.Router, string) {
	t.Helper()
	rt, err := router.New(router.Options{Cfg: cfg, Env: env})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	srv := httptest.NewServer(rt.Handler())
	t.Cleanup(srv.Close)
	return rt, srv.URL
}

func postJSON(t *testing.T, url string, hdr map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeAPIError asserts the Anthropic error envelope shape and returns the
// inner error type and message.
func decodeAPIError(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type = %q, want application/json", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, b)
	}
	if e.Type != "error" {
		t.Errorf("error envelope type = %q, want %q (body %q)", e.Type, "error", b)
	}
	return e.Error.Type, e.Error.Message
}

func TestNewValidation(t *testing.T) {
	valid := func() *config.Config { return baseConfig("http://127.0.0.1:9", "http://127.0.0.1:9") }

	tests := []struct {
		name    string
		cfg     *config.Config
		wantSub string // substring of the error
	}{
		{"nil config", nil, "Options.Cfg is nil"},
		{"empty virtual model", func() *config.Config {
			c := valid()
			c.Tiers.Subagents.VirtualModel = ""
			return c
		}(), "virtual_model is empty"},
		{"subagent provider missing", func() *config.Config {
			c := valid()
			c.Tiers.Subagents.Provider = "nope"
			return c
		}(), `provider "nope" not in providers`},
		{"unknown default route tier", func() *config.Config {
			c := valid()
			c.Router.DefaultRoute = "bogus"
			return c
		}(), `unknown tier "bogus"`},
		{"default route provider missing", func() *config.Config {
			c := valid()
			c.Tiers.Middle.Provider = "ghost"
			return c
		}(), `provider "ghost" not in providers`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := router.New(router.Options{Cfg: tt.cfg})
			if err == nil {
				t.Fatalf("New succeeded, want error containing %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err, tt.wantSub)
			}
		})
	}

	t.Run("valid config binds", func(t *testing.T) {
		rt, err := router.New(router.Options{Cfg: valid()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if rt.Addr() == "" {
			t.Error("Addr() empty after successful New")
		}
	})
}

func TestVirtualModelRewrite(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	_, base := newTestRouter(t, cfg, map[string]string{"ZAI_KEY": "zai-secret"})

	body := []byte(`{"model":"roscoe/tier3","temperature":0.5,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp := postJSON(t, base+"/v1/messages", map[string]string{
		"Authorization":     "Bearer worker-oauth",
		"Anthropic-Version": "2023-06-01",
		"X-Api-Key":         "sk-should-not-cross",
		"Anthropic-Custom":  "should-not-cross-on-env-leg",
	}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if hits, _, _, _, _ := def.snapshot(); hits != 0 {
		t.Errorf("default upstream hit %d times, want 0", hits)
	}
	hits, upBody, hdr, path, _ := sub.snapshot()
	if hits != 1 {
		t.Fatalf("subagent upstream hit %d times, want 1", hits)
	}
	if path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", path)
	}

	// Model rewritten, all other fields carried byte-for-byte.
	var got map[string]json.RawMessage
	if err := json.Unmarshal(upBody, &got); err != nil {
		t.Fatalf("upstream body is not JSON: %v (body %q)", err, upBody)
	}
	if string(got["model"]) != `"glm-4.6"` {
		t.Errorf("model = %s, want %q", got["model"], `"glm-4.6"`)
	}
	for key, want := range map[string]string{
		"temperature": `0.5`,
		"messages":    `[{"role":"user","content":"hi"}]`,
		"stream":      `false`,
	} {
		if string(got[key]) != want {
			t.Errorf("field %q = %s, want %s", key, got[key], want)
		}
	}

	// Authorization replaced from Options.Env; the worker's credentials and
	// non-allowlisted anthropic headers must not cross on the env leg.
	if auth := hdr.Get("Authorization"); auth != "Bearer zai-secret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer zai-secret")
	}
	if v := hdr.Get("X-Api-Key"); v != "" {
		t.Errorf("X-Api-Key = %q leaked to env-auth upstream", v)
	}
	if v := hdr.Get("Anthropic-Custom"); v != "" {
		t.Errorf("Anthropic-Custom = %q leaked to env-auth upstream", v)
	}
	if v := hdr.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want 2023-06-01 (allowlisted)", v)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestPassthroughByteIdentical(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	_, base := newTestRouter(t, cfg, nil)

	// Odd key order and whitespace: any re-marshal would change these bytes.
	body := []byte(`{"model":"claude-opus-4-6",  "zzz":1,"aaa":{"nested":  true},"messages":[{"role":"user","content":"hi"}]}`)
	resp := postJSON(t, base+"/v1/messages", map[string]string{
		"Authorization":   "Bearer worker-oauth",
		"X-Api-Key":       "sk-worker-key",
		"Anthropic-Beta":  "oauth-2025",
		"Anthropic-Extra": "kept",
		"X-Custom-Secret": "must-not-cross",
	}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != `{"id":"msg_1","type":"message"}` {
		t.Errorf("response body = %q, want upstream body verbatim", respBody)
	}

	if hits, _, _, _, _ := sub.snapshot(); hits != 0 {
		t.Errorf("subagent upstream hit %d times, want 0", hits)
	}
	hits, upBody, hdr, _, _ := def.snapshot()
	if hits != 1 {
		t.Fatalf("default upstream hit %d times, want 1", hits)
	}
	if !bytes.Equal(upBody, body) {
		t.Errorf("passthrough body not byte-identical:\n got %q\nwant %q", upBody, body)
	}

	// Auth "account": worker credential shape forwarded verbatim.
	if v := hdr.Get("Authorization"); v != "Bearer worker-oauth" {
		t.Errorf("Authorization = %q, want %q", v, "Bearer worker-oauth")
	}
	if v := hdr.Get("X-Api-Key"); v != "sk-worker-key" {
		t.Errorf("X-Api-Key = %q, want %q", v, "sk-worker-key")
	}
	if v := hdr.Get("Anthropic-Beta"); v != "oauth-2025" {
		t.Errorf("Anthropic-Beta = %q, want %q", v, "oauth-2025")
	}
	if v := hdr.Get("Anthropic-Extra"); v != "kept" {
		t.Errorf("Anthropic-Extra = %q, want %q (anthropic-* forwarded on account leg)", v, "kept")
	}
	if v := hdr.Get("X-Custom-Secret"); v != "" {
		t.Errorf("X-Custom-Secret = %q crossed the router, want stripped", v)
	}
}

func TestDefaultRouteFallthrough(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown model", `{"model":"gpt-5o-mega","messages":[]}`},
		{"no model field", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"non-string model", `{"model":42,"messages":[]}`},
		{"unparsable body", `this is not json {{{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := newUpstream(t)
			sub := newUpstream(t)
			cfg := baseConfig(def.srv.URL, sub.srv.URL)
			_, base := newTestRouter(t, cfg, nil)

			resp := postJSON(t, base+"/v1/messages", nil, []byte(tt.body))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if hits, _, _, _, _ := sub.snapshot(); hits != 0 {
				t.Errorf("subagent upstream hit %d times, want 0", hits)
			}
			hits, upBody, _, _, _ := def.snapshot()
			if hits != 1 {
				t.Fatalf("default upstream hit %d times, want 1", hits)
			}
			if !bytes.Equal(upBody, []byte(tt.body)) {
				t.Errorf("body not preserved:\n got %q\nwant %q", upBody, tt.body)
			}
		})
	}
}

func TestCountTokensEstimate(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	// Default provider answers count_tokens locally.
	p := cfg.Providers["anthropic"]
	p.CountTokens = "estimate"
	cfg.Providers["anthropic"] = p
	_, base := newTestRouter(t, cfg, nil)

	body := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"count me"}]}`)
	resp := postJSON(t, base+"/v1/messages/count_tokens", nil, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	want := fmt.Sprintf(`{"input_tokens":%d}`, len(body)/4)
	if string(b) != want {
		t.Errorf("body = %q, want %q", b, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if hits, _, _, _, _ := def.snapshot(); hits != 0 {
		t.Errorf("default upstream hit %d times, want 0 (estimate must answer locally)", hits)
	}
	if hits, _, _, _, _ := sub.snapshot(); hits != 0 {
		t.Errorf("subagent upstream hit %d times, want 0", hits)
	}
}

func TestCountTokensForwardedWhenNotEstimate(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL) // CountTokens "" → forward
	_, base := newTestRouter(t, cfg, nil)

	body := []byte(`{"model":"claude-opus-4-6","messages":[]}`)
	resp := postJSON(t, base+"/v1/messages/count_tokens?beta=true", nil, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	hits, upBody, _, path, rawq := def.snapshot()
	if hits != 1 {
		t.Fatalf("default upstream hit %d times, want 1", hits)
	}
	if path != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want /v1/messages/count_tokens", path)
	}
	if rawq != "beta=true" {
		t.Errorf("upstream query = %q, want %q", rawq, "beta=true")
	}
	if !bytes.Equal(upBody, body) {
		t.Errorf("body not preserved: got %q want %q", upBody, body)
	}
}

func TestMissingEnvVarIs502(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	// Env deliberately lacks ZAI_KEY.
	_, base := newTestRouter(t, cfg, map[string]string{})

	body := []byte(`{"model":"roscoe/tier3","messages":[]}`)
	resp := postJSON(t, base+"/v1/messages", nil, body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	errType, msg := decodeAPIError(t, resp)
	if errType != "api_error" {
		t.Errorf("error.type = %q, want api_error", errType)
	}
	if !strings.Contains(msg, "ZAI_KEY") {
		t.Errorf("error message %q does not name the missing var ZAI_KEY", msg)
	}
	if hits, _, _, _, _ := sub.snapshot(); hits != 0 {
		t.Errorf("subagent upstream hit %d times, want 0", hits)
	}
	if hits, _, _, _, _ := def.snapshot(); hits != 0 {
		t.Errorf("default upstream hit %d times, want 0", hits)
	}
}

func TestHealthzAndMethodNotAllowed(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	_, base := newTestRouter(t, cfg, nil)

	t.Run("GET healthz", func(t *testing.T) {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if string(b) != `{"ok":true}` {
			t.Errorf("body = %q, want %q", b, `{"ok":true}`)
		}
	})

	t.Run("POST healthz is 405", func(t *testing.T) {
		resp, err := http.Post(base+"/healthz", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST /healthz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("GET messages is 405", func(t *testing.T) {
		resp, err := http.Get(base + "/v1/messages")
		if err != nil {
			t.Fatalf("GET /v1/messages: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})

	if hits, _, _, _, _ := def.snapshot(); hits != 0 {
		t.Errorf("default upstream hit %d times, want 0", hits)
	}
	_ = sub
}

func TestUpstreamStatusAndHeadersCopied(t *testing.T) {
	def := newUpstream(t)
	def.mu.Lock()
	def.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Upstream-Id", "abc123")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}
	def.mu.Unlock()
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	_, base := newTestRouter(t, cfg, nil)

	resp := postJSON(t, base+"/v1/messages", nil, []byte(`{"model":"claude-opus-4-6"}`))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passed through", resp.StatusCode)
	}
	if v := resp.Header.Get("Retry-After"); v != "7" {
		t.Errorf("Retry-After = %q, want 7", v)
	}
	if v := resp.Header.Get("X-Upstream-Id"); v != "abc123" {
		t.Errorf("X-Upstream-Id = %q, want abc123", v)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "rate_limit_error") {
		t.Errorf("body = %q, want upstream error body passed through", b)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	_, base := newTestRouter(t, cfg, nil)

	big := bytes.Repeat([]byte("a"), 10<<20+1) // maxBodyBytes is 10 MiB
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST oversized: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	errType, _ := decodeAPIError(t, resp)
	if errType != "api_error" {
		t.Errorf("error.type = %q, want api_error", errType)
	}
	if hits, _, _, _, _ := def.snapshot(); hits != 0 {
		t.Errorf("default upstream hit %d times, want 0", hits)
	}
	_ = sub
}

func TestStreamingFlushesThrough(t *testing.T) {
	const chunk1 = "event: message_start\ndata: {\"n\":1}\n\n"
	const chunk2 = "event: message_stop\ndata: {\"n\":2}\n\n"
	sendSecond := make(chan struct{})

	def := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunk1)
		fl.Flush()
		select {
		case <-sendSecond:
		case <-time.After(10 * time.Second):
			t.Error("client never acknowledged first chunk; router is buffering the stream")
			return
		}
		io.WriteString(w, chunk2)
		fl.Flush()
	}))
	defer def.Close()
	sub := newUpstream(t)

	cfg := baseConfig(def.URL, sub.srv.URL)
	rt, err := router.New(router.Options{Cfg: cfg, Env: nil})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- rt.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("ListenAndServe returned %v after ctx cancel, want nil", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("ListenAndServe did not return after ctx cancel")
		}
	}()

	body := `{"model":"claude-opus-4-6","stream":true,"messages":[]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+rt.Addr()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// The first chunk must be readable while the upstream is still blocked
	// waiting on sendSecond — that proves per-write flush through the router.
	buf := make([]byte, len(chunk1))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(buf) != chunk1 {
		t.Fatalf("first chunk = %q, want %q", buf, chunk1)
	}
	close(sendSecond)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	if string(rest) != chunk2 {
		t.Errorf("rest of stream = %q, want %q", rest, chunk2)
	}
	client.CloseIdleConnections()
}

func TestEphemeralPortBindAndServe(t *testing.T) {
	cfg := baseConfig("http://127.0.0.1:9", "http://127.0.0.1:9")
	// Bind/Port unset everywhere → 127.0.0.1 with an ephemeral port.
	rt, err := router.New(router.Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	host, port, err := net.SplitHostPort(rt.Addr())
	if err != nil {
		t.Fatalf("Addr() %q is not host:port: %v", rt.Addr(), err)
	}
	if host != "127.0.0.1" {
		t.Errorf("bound host = %q, want 127.0.0.1", host)
	}
	if port == "0" || port == "" {
		t.Errorf("bound port = %q, want a real ephemeral port", port)
	}

	// Addr is dialable immediately after New, before ListenAndServe.
	conn, err := net.DialTimeout("tcp", rt.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", rt.Addr(), err)
	}
	conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- rt.ListenAndServe(ctx) }()

	resp, err := http.Get("http://" + rt.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on %s: %v", rt.Addr(), err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != `{"ok":true}` {
		t.Errorf("healthz = %d %q, want 200 {\"ok\":true}", resp.StatusCode, b)
	}
	http.DefaultClient.CloseIdleConnections()

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("ListenAndServe returned %v after ctx cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("ListenAndServe did not return after ctx cancel")
	}
}

func TestStaticAndUnsupportedAuth(t *testing.T) {
	t.Run("static auth sets literal bearer", func(t *testing.T) {
		def := newUpstream(t)
		sub := newUpstream(t)
		cfg := baseConfig(def.srv.URL, sub.srv.URL)
		p := cfg.Providers["anthropic"]
		p.Auth = "static:ollama"
		cfg.Providers["anthropic"] = p
		_, base := newTestRouter(t, cfg, nil)

		resp := postJSON(t, base+"/v1/messages", map[string]string{
			"Authorization": "Bearer worker-oauth",
		}, []byte(`{"model":"claude-opus-4-6"}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		_, _, hdr, _, _ := def.snapshot()
		if v := hdr.Get("Authorization"); v != "Bearer ollama" {
			t.Errorf("Authorization = %q, want %q", v, "Bearer ollama")
		}
	})

	t.Run("unsupported auth mode is 502", func(t *testing.T) {
		def := newUpstream(t)
		sub := newUpstream(t)
		cfg := baseConfig(def.srv.URL, sub.srv.URL)
		p := cfg.Providers["anthropic"]
		p.Auth = "keychain:whatever"
		cfg.Providers["anthropic"] = p
		_, base := newTestRouter(t, cfg, nil)

		resp := postJSON(t, base+"/v1/messages", nil, []byte(`{"model":"claude-opus-4-6"}`))
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		errType, msg := decodeAPIError(t, resp)
		if errType != "api_error" {
			t.Errorf("error.type = %q, want api_error", errType)
		}
		if !strings.Contains(msg, "unsupported provider auth") {
			t.Errorf("message %q does not mention unsupported provider auth", msg)
		}
		if hits, _, _, _, _ := def.snapshot(); hits != 0 {
			t.Errorf("default upstream hit %d times, want 0", hits)
		}
	})
}

// syncBuffer is a mutex-guarded writer for the JSONL request log.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRequestLogLine(t *testing.T) {
	def := newUpstream(t)
	sub := newUpstream(t)
	cfg := baseConfig(def.srv.URL, sub.srv.URL)
	logw := &syncBuffer{}
	rt, err := router.New(router.Options{Cfg: cfg, Env: map[string]string{"ZAI_KEY": "k"}, LogW: logw})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	srv := httptest.NewServer(rt.Handler())
	t.Cleanup(srv.Close)

	body := []byte(`{"model":"roscoe/tier3","messages":[]}`)
	resp := postJSON(t, srv.URL+"/v1/messages", nil, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The log line is written in a defer after the response body; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	var line string
	for time.Now().Before(deadline) {
		if s := logw.String(); strings.Contains(s, "\n") {
			line = strings.SplitN(s, "\n", 2)[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line == "" {
		t.Fatal("no JSONL log line written")
	}
	var e struct {
		Path     string `json:"path"`
		ModelIn  string `json:"model_in"`
		ModelOut string `json:"model_out"`
		Upstream string `json:"upstream"`
		Status   int    `json:"status"`
		BytesIn  int    `json:"bytes_in"`
	}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("log line is not JSON: %v (line %q)", err, line)
	}
	if e.Path != "/v1/messages" {
		t.Errorf("log path = %q, want /v1/messages", e.Path)
	}
	if e.ModelIn != "roscoe/tier3" {
		t.Errorf("log model_in = %q, want roscoe/tier3", e.ModelIn)
	}
	if e.ModelOut != "glm-4.6" {
		t.Errorf("log model_out = %q, want glm-4.6", e.ModelOut)
	}
	if e.Upstream != "zai" {
		t.Errorf("log upstream = %q, want zai", e.Upstream)
	}
	if e.Status != http.StatusOK {
		t.Errorf("log status = %d, want 200", e.Status)
	}
	if e.BytesIn != len(body) {
		t.Errorf("log bytes_in = %d, want %d", e.BytesIn, len(body))
	}
}
