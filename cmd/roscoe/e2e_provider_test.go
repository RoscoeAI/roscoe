package main

// A fake model provider the binary can reach over loopback. It speaks just
// enough of the Anthropic protocol for the router and the catalogue, and
// records what it was sent so a test can assert on auth and routing.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type providerCall struct {
	Path  string
	Auth  string
	Model string
}

type fakeProvider struct {
	srv   *http.Server
	url   string
	mu    sync.Mutex
	calls []providerCall
}

func startFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{url: "http://" + ln.Addr().String()}
	mux := http.NewServeMux()
	// Two providers live here: /an is "anthropic", /di is "deepinfra" whose
	// Anthropic surface (/di/anthropic) publishes no list, only /di does.
	mux.HandleFunc("/an/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fp.record(r, "")
		fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5"},{"id":"claude-opus-5"}]}`)
	})
	mux.HandleFunc("/di/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fp.record(r, "")
		fmt.Fprint(w, `{"data":[{"id":"zai-org/GLM-5.3-Flash"},{"id":"zai-org/GLM-5"}]}`)
	})
	messages := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		fp.record(r, req.Model)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.12")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, req.Model)
	}
	mux.HandleFunc("/an/v1/messages", messages)
	mux.HandleFunc("/di/anthropic/v1/messages", messages)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fp.record(r, "")
		http.NotFound(w, r)
	})
	fp.srv = &http.Server{Handler: mux}
	go fp.srv.Serve(ln)
	t.Cleanup(func() { fp.srv.Close() })
	return fp
}

func (fp *fakeProvider) record(r *http.Request, model string) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.calls = append(fp.calls, providerCall{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Model: model})
}

func (fp *fakeProvider) find(path string) (providerCall, bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, c := range fp.calls {
		if c.Path == path {
			return c, true
		}
	}
	return providerCall{}, false
}

func (fp *fakeProvider) paths() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	var out []string
	for _, c := range fp.calls {
		out = append(out, c.Path)
	}
	return out
}

// pointAtFake rewires the config's providers at the fake and gives the env
// file a DeepInfra key, so the binary has a credential to send.
func (w *world) pointAtFake(fp *fakeProvider) {
	w.t.Helper()
	expect(w.t, w.run("", "config", "set", "providers.anthropic.base_url", fp.url+"/an"), 0)
	expect(w.t, w.run("", "config", "set", "providers.deepinfra.base_url", fp.url+"/di/anthropic"), 0)
	if err := os.WriteFile(filepath.Join(w.home, ".roscoe", ".env"), []byte("DEEP_INFRA_API_KEY=dk-test-secret\n"), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func TestE2EModelsRefresh(t *testing.T) {
	w := newWorld(t)
	w.init()
	fp := startFakeProvider(t)
	w.pointAtFake(fp)

	r := w.run("", "models", "--refresh")
	expect(t, r, 0, "anthropic    2 models", "deepinfra    2 models", "zai-org/GLM-5.3-Flash", "claude-opus-5",
		"tier 3  zai-org/GLM-5.3-Flash")
	// local (nothing on :11434) fails and says so without failing the command.
	if !regexp.MustCompile(`local\s+.*(refused|connect|dial)`).MatchString(r.stderr) {
		t.Errorf("the unreachable local provider is not reported:\n%s", r.stderr)
	}
	if strings.Contains(r.all(), "dk-test-secret") {
		t.Fatal("the provider key was printed")
	}
	di, ok := fp.find("/di/v1/models")
	if !ok {
		t.Fatalf("deepinfra's list was not fetched from the /anthropic parent; paths: %v", fp.paths())
	}
	if di.Auth != "Bearer dk-test-secret" {
		t.Errorf("deepinfra list fetched with auth %q", di.Auth)
	}
	if an, ok := fp.find("/an/v1/models"); !ok || an.Auth != "" {
		t.Errorf("anthropic (account auth) list call = %+v, %v; want no bearer", an, ok)
	}

	// The catalogue persisted: a plain `models` now knows the lists.
	r = w.run("", "models")
	expect(t, r, 0, "zai-org/GLM-5", "claude-sonnet-5")
	if strings.Contains(r.stdout, "nothing known yet") && !strings.Contains(r.stdout, "local\n  (nothing known yet") {
		t.Errorf("a refreshed provider still says nothing known:\n%s", r.stdout)
	}
}

// freePort asks the kernel for a port and gives it back; the router binds
// it a moment later.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// TestE2ERouterForeground runs `roscoe router` as an operator would, sends
// it traffic, and stops it with SIGINT.
func TestE2ERouterForeground(t *testing.T) {
	w := newWorld(t)
	w.init()
	fp := startFakeProvider(t)
	w.pointAtFake(fp)
	expect(t, w.run("", "router", "extra"), 2, "unexpected arguments")

	port := freePort(t)
	cmd := exec.Command(roscoeBin, "router", "--bind", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = w.cwd
	cmd.Env = w.env()
	stderr, _ := cmd.StderrPipe()
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// Wait for the ready line.
	errText := make(chan string, 1)
	ready := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			b.Write(buf[:n])
			if m := regexp.MustCompile(`listening on (\S+)`).FindStringSubmatch(b.String()); m != nil {
				select {
				case ready <- m[1]:
				default:
				}
			}
			if err != nil {
				errText <- b.String()
				return
			}
		}
	}()
	var addr string
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("router never reported listening")
	}
	if !strings.HasSuffix(addr, fmt.Sprint(":", port)) {
		t.Errorf("listening on %s, asked for port %d", addr, port)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()

	post := func(model string) (int, string) {
		body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`, model)
		req, _ := http.NewRequest("POST", "http://"+addr+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "client-side-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", model, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// The virtual tier-3 name is rewritten to the real model and sent to
	// the subagents provider with ITS credential.
	if code, body := post("roscoe/tier3"); code != 200 || !strings.Contains(body, "pong") {
		t.Errorf("tier3 call: %d %s", code, body)
	}
	di, ok := fp.find("/di/anthropic/v1/messages")
	if !ok {
		t.Fatalf("tier3 traffic did not reach deepinfra; paths: %v", fp.paths())
	}
	if di.Model != "zai-org/GLM-5.3-Flash" || di.Auth != "Bearer dk-test-secret" {
		t.Errorf("deepinfra saw model %q auth %q", di.Model, di.Auth)
	}
	// Anything else takes the default route (tier 2's provider) untouched.
	if code, _ := post("sonnet"); code != 200 {
		t.Errorf("default route call: %d", code)
	}
	an, ok := fp.find("/an/v1/messages")
	if !ok || an.Model != "sonnet" {
		t.Errorf("default route call = %+v, %v; want sonnet at anthropic", an, ok)
	}

	// SIGINT: a clean stop, exit 0, and the request log on stdout.
	cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("router exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("router did not stop on SIGINT")
	}
	all := <-errText
	if !strings.Contains(all, "shut down") {
		t.Errorf("no shutdown line:\n%s", all)
	}
	log := stdout.String()
	for _, want := range []string{`"model_in":"roscoe/tier3"`, `"model_out":"zai-org/GLM-5.3-Flash"`, `"upstream":"deepinfra"`, `"status":200`, `"model_in":"sonnet"`, `"upstream":"anthropic"`} {
		if !strings.Contains(log, want) {
			t.Errorf("request log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log+all, "dk-test-secret") || strings.Contains(log+all, "client-side-key") {
		t.Fatal("a credential reached the router's output")
	}
}
