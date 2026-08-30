// Package router is roscoe's model-dispatch reverse proxy. Every worker
// points ANTHROPIC_BASE_URL here; the router reads each request body's
// "model" field and forwards to the matching Anthropic-protocol upstream.
// The claude-* passthrough leg forwards the body byte-identical; only the
// virtual-model leg re-marshals the body (to splice in the real model name).
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

const maxBodyBytes = 10 << 20 // inbound request body cap

// Options configures New. Bind/Port override cfg.Router when set; a resolved
// port of 0 binds an ephemeral port, discoverable via (*Router).Addr.
type Options struct {
	Cfg  *config.Config
	Env  map[string]string // from LoadEnvFile
	LogW io.Writer         // JSONL request log, may be nil
	Bind string            // override; "" → cfg.Router.Bind
	Port int               // override; 0 → cfg.Router.Port
}

// Router dispatches /v1/messages traffic between Anthropic-protocol upstreams
// by the request body's model name. The listener is claimed in New so Addr is
// valid immediately (ephemeral ports included); ListenAndServe serves on it.
type Router struct {
	env    map[string]string
	logW   io.Writer
	logMu  sync.Mutex
	client *http.Client
	mux    *http.ServeMux
	ln     net.Listener
	addr   string

	virtual     string // tier-3 wire name, e.g. "roscoe/tier3"
	subModel    string // what the virtual name rewrites to
	subProvName string
	subProv     config.Provider
	defProvName string
	defProv     config.Provider
}

// New validates the routing config and binds the listen address.
func New(o Options) (*Router, error) {
	cfg := o.Cfg
	if cfg == nil {
		return nil, errors.New("router: Options.Cfg is nil")
	}
	virtual := cfg.Tiers.Subagents.VirtualModel
	if virtual == "" {
		return nil, errors.New("router: tiers.subagents.virtual_model is empty")
	}
	subName := cfg.Tiers.Subagents.Provider
	subProv, ok := cfg.Providers[subName]
	if !ok {
		return nil, fmt.Errorf("router: subagents tier provider %q not in providers", subName)
	}
	defName, err := tierProvider(cfg, cfg.Router.DefaultRoute)
	if err != nil {
		return nil, fmt.Errorf("router: default_route: %w", err)
	}
	defProv, ok := cfg.Providers[defName]
	if !ok {
		return nil, fmt.Errorf("router: default route provider %q not in providers", defName)
	}

	bind := o.Bind
	if bind == "" {
		bind = cfg.Router.Bind
	}
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := o.Port
	if port == 0 {
		port = cfg.Router.Port
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("router: listen %s port %d: %w", bind, port, err)
	}

	rt := &Router{
		env:  o.Env,
		logW: o.LogW,
		client: &http.Client{
			// No overall timeout: SSE streams run for minutes.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 600 * time.Second, // GLM reasoning is slow
				ForceAttemptHTTP2:     true,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
			},
			// A proxy hands redirects back to the client, never follows them.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ln:          ln,
		addr:        ln.Addr().String(),
		virtual:     virtual,
		subModel:    cfg.Tiers.Subagents.Model,
		subProvName: subName,
		subProv:     subProv,
		defProvName: defName,
		defProv:     defProv,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", rt.handleProxy)
	mux.HandleFunc("POST /v1/messages/count_tokens", rt.handleProxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	rt.mux = mux
	return rt, nil
}

// Handler returns the router's HTTP handler (useful for in-process tests).
func (rt *Router) Handler() http.Handler { return rt.mux }

// Addr returns the actual bound address ("127.0.0.1:54321"), valid as soon as
// New returns — this is how a Port-0 (ephemeral) bind is discovered.
func (rt *Router) Addr() string { return rt.addr }

// ListenAndServe serves until ctx is done, then shuts down gracefully
// (10s grace for in-flight streams, then hard close). Returns nil on a
// ctx-driven shutdown.
func (rt *Router) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Handler: rt.mux,
		// No read/write deadlines: both directions stream.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(rt.ln) }()
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			srv.Close()
		}
		<-errCh // always http.ErrServerClosed after Shutdown/Close
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("router: serve: %w", err)
	}
}

// tierProvider maps a router.default_route tier name to its provider name.
func tierProvider(cfg *config.Config, tier string) (string, error) {
	switch tier {
	case "main":
		return cfg.Tiers.Main.Provider, nil
	case "middle":
		return cfg.Tiers.Middle.Provider, nil
	case "subagents":
		return cfg.Tiers.Subagents.Provider, nil
	default:
		return "", fmt.Errorf("unknown tier %q", tier)
	}
}

// logEntry is one JSONL request-log line.
type logEntry struct {
	TS        string `json:"ts"`
	Path      string `json:"path"`
	ModelIn   string `json:"model_in"`
	ModelOut  string `json:"model_out"`
	Upstream  string `json:"upstream"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	BytesIn   int    `json:"bytes_in"`
	BytesOut  int64  `json:"bytes_out"`
}

func (rt *Router) log(e logEntry) {
	if rt.logW == nil {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	rt.logMu.Lock()
	defer rt.logMu.Unlock()
	rt.logW.Write(append(b, '\n'))
}

// handleProxy serves both /v1/messages and /v1/messages/count_tokens.
func (rt *Router) handleProxy(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	e := logEntry{Path: req.URL.Path}
	defer func() {
		e.LatencyMS = time.Since(start).Milliseconds()
		rt.log(e)
	}()

	req.Body = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		status := http.StatusBadRequest
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
		}
		e.Status = status
		writeAPIError(w, status, fmt.Sprintf("read request body: %v", err))
		return
	}
	e.BytesIn = len(body)

	// Route by model. An unparsable body or absent model falls through to the
	// default route untouched — the upstream owns rejecting it.
	modelIn, parsed := extractModel(body)
	e.ModelIn = modelIn
	provName, prov := rt.defProvName, rt.defProv
	outBody := body // passthrough leg: byte-identical
	modelOut := modelIn
	if modelIn != "" && modelIn == rt.virtual {
		provName, prov = rt.subProvName, rt.subProv
		modelOut = rt.subModel
		outBody, err = spliceModel(parsed, rt.subModel)
		if err != nil {
			e.Status = http.StatusInternalServerError
			e.Upstream = provName
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("splice model: %v", err))
			return
		}
	}
	e.ModelOut = modelOut
	e.Upstream = provName

	// count_tokens: some upstreams (Ollama) hang on it — answer locally.
	if strings.HasSuffix(req.URL.Path, "/count_tokens") && prov.CountTokens == "estimate" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n, _ := fmt.Fprintf(w, `{"input_tokens":%d}`, len(body)/4)
		e.Status = http.StatusOK
		e.BytesOut = int64(n)
		return
	}

	hdr, err := rt.outboundHeaders(req.Header, prov.Auth)
	if err != nil {
		e.Status = http.StatusBadGateway
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	url := strings.TrimRight(prov.BaseURL, "/") + req.URL.Path
	if req.URL.RawQuery != "" {
		url += "?" + req.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, url, bytes.NewReader(outBody))
	if err != nil {
		e.Status = http.StatusBadGateway
		writeAPIError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err))
		return
	}
	upReq.Header = hdr

	resp, err := rt.client.Do(upReq)
	if err != nil {
		e.Status = http.StatusBadGateway
		writeAPIError(w, http.StatusBadGateway, fmt.Sprintf("upstream %s: %v", provName, err))
		return
	}
	defer resp.Body.Close()

	// Response path: copy status + headers, then stream with per-write Flush
	// so SSE frames never buffer. No body inspection.
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	e.Status = resp.StatusCode
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	fw := &flushWriter{w: w, f: fl}
	io.Copy(fw, resp.Body) // an error here means a broken peer; nothing left to send
	e.BytesOut = fw.n
}

// outboundHeaders builds the upstream header set per the provider's auth mode.
// Only the allowlisted headers ever cross the router.
func (rt *Router) outboundHeaders(in http.Header, auth string) (http.Header, error) {
	h := make(http.Header)
	for _, k := range []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta", "Accept"} {
		if vs := in.Values(k); len(vs) > 0 {
			h[k] = append([]string(nil), vs...)
		}
	}
	switch {
	case auth == "account":
		// Forward the worker's own credential shape verbatim — the client
		// composes subscription-OAuth requests the router must not rewrite.
		for k, vs := range in {
			if k == "Authorization" || k == "X-Api-Key" || strings.HasPrefix(k, "Anthropic-") {
				h[k] = append([]string(nil), vs...)
			}
		}
	case strings.HasPrefix(auth, "env:"):
		name := strings.TrimPrefix(auth, "env:")
		v := rt.env[name]
		if v == "" {
			return nil, fmt.Errorf("provider auth %q: %s not set in env file", auth, name)
		}
		h.Set("Authorization", "Bearer "+v)
	case strings.HasPrefix(auth, "static:"):
		h.Set("Authorization", "Bearer "+strings.TrimPrefix(auth, "static:"))
	default:
		return nil, fmt.Errorf("unsupported provider auth %q", auth)
	}
	return h, nil
}

// extractModel parses body just enough to read "model". Returns the parsed
// top-level map so the rewrite leg can splice without a second decode; the
// passthrough leg never uses it.
func extractModel(body []byte) (string, map[string]json.RawMessage) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return "", nil
	}
	raw, ok := m["model"]
	if !ok {
		return "", m
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", m
	}
	return s, m
}

// spliceModel replaces only the "model" value and re-marshals. Key order may
// change (Go sorts map keys) — acceptable on the rewrite leg only; every other
// value is a json.RawMessage carried byte-for-byte.
func spliceModel(m map[string]json.RawMessage, model string) ([]byte, error) {
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("marshal model name: %w", err)
	}
	m["model"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-marshal body: %w", err)
	}
	return out, nil
}

// hop-by-hop headers are the proxy's own business, never copied downstream.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[k] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// flushWriter flushes after every Write so SSE frames leave immediately.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
	n int64
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.n += int64(n)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// writeAPIError responds with an Anthropic-shaped error body so SDK clients
// surface it cleanly.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	quoted, err := json.Marshal(msg)
	if err != nil {
		quoted = []byte(`"internal error"`)
	}
	fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":%s}}`, quoted)
}
