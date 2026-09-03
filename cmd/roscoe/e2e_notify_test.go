package main

// notify serve: the inbound webhook listener, started as an operator would,
// sent a forged and then a properly signed Twilio POST, stopped with SIGINT.

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// bgProc is a roscoe started in the background: stderr is watched for a
// ready line, stdout collected, and SIGINT stops it.
type bgProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	mu     sync.Mutex
	stderr strings.Builder
	stdout strings.Builder
}

func (w *world) startBackground(args ...string) *bgProc {
	w.t.Helper()
	p := &bgProc{t: w.t, cmd: exec.Command(roscoeBin, args...)}
	p.cmd.Dir = w.cwd
	p.cmd.Env = w.env()
	p.cmd.Stdout = &lockedBuilder{mu: &p.mu, b: &p.stdout}
	p.cmd.Stderr = &lockedBuilder{mu: &p.mu, b: &p.stderr}
	if err := p.cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { p.cmd.Process.Kill() })
	return p
}

type lockedBuilder struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (l *lockedBuilder) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}

func (p *bgProc) errText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *bgProc) outText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.String()
}

// waitFor blocks until re matches stderr and returns the first submatch.
func (p *bgProc) waitFor(re string) string {
	p.t.Helper()
	rx := regexp.MustCompile(re)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := rx.FindStringSubmatch(p.errText()); m != nil {
			if len(m) > 1 {
				return m[1]
			}
			return m[0]
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("never saw %q on stderr; saw:\n%s", re, p.errText())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop sends SIGINT and returns the exit code.
func (p *bgProc) stop() int {
	p.t.Helper()
	p.cmd.Process.Signal(syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			p.t.Fatalf("wait: %v", err)
		}
		return 0
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		p.t.Fatal("did not stop on SIGINT")
		return -1
	}
}

func twilioSignature(token, webhookURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	mac := hmac.New(sha1.New, []byte(token))
	io.WriteString(mac, webhookURL)
	for _, k := range keys {
		for _, v := range form[k] {
			io.WriteString(mac, k)
			io.WriteString(mac, v)
		}
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func postForm(t *testing.T, target string, form url.Values, sig string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sig != "" {
		req.Header.Set("X-Twilio-Signature", sig)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestE2ENotifyServe(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "notify", "serve"), 1, "TWILIO_ACCOUNT_SID")

	port := freePort(t)
	webhook := fmt.Sprintf("http://127.0.0.1:%d/twilio/sms", port)
	env := "TWILIO_ACCOUNT_SID=AC123\nTWILIO_AUTH_TOKEN=tok-secret\nTWILIO_TO=+15550001111\nTWILIO_FROM=+15550002222\nTWILIO_WEBHOOK_URL=" + webhook + "\n"
	os.WriteFile(filepath.Join(w.home, ".roscoe", ".env"), []byte(env), 0o600)

	p := w.startBackground("notify", "serve", "--port", fmt.Sprint(port))
	p.waitFor(`listening on (127\.0\.0\.1:` + fmt.Sprint(port) + `/twilio/sms)`)

	form := url.Values{"From": {"+15550001111"}, "Body": {"yes, go ahead"}, "MessageSid": {"SM1"}}
	// Unsigned, and wrongly signed: refused, and not reported as a reply.
	if code, _ := postForm(t, webhook, form, ""); code != http.StatusForbidden {
		t.Errorf("unsigned webhook accepted with %d", code)
	}
	if code, _ := postForm(t, webhook, form, twilioSignature("wrong-token", webhook, form)); code != http.StatusForbidden {
		t.Errorf("wrongly signed webhook accepted with %d", code)
	}
	if strings.Contains(p.outText(), "[reply]") {
		t.Fatalf("a refused webhook was reported as a reply:\n%s", p.outText())
	}
	// Signed with the account's token: accepted, TwiML back, reply reported.
	code, body := postForm(t, webhook, form, twilioSignature("tok-secret", webhook, form))
	if code != 200 || !strings.Contains(body, "<Response/>") {
		t.Errorf("signed webhook: %d %s", code, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(p.outText(), "[reply]") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if out := p.outText(); !strings.Contains(out, `from=+15550001111`) || !strings.Contains(out, `body="yes, go ahead"`) {
		t.Errorf("reply line = %q", out)
	}
	// GET is not a webhook.
	resp, err := http.Get(webhook)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %v %v", err, resp)
	}
	if code := p.stop(); code != 0 {
		t.Errorf("exit %d after SIGINT:\n%s", code, p.errText())
	}
	if strings.Contains(p.errText()+p.outText(), "tok-secret") {
		t.Fatal("the auth token reached the output")
	}
}

// Without the public URL there is nothing to sign against, so serve accepts
// inbound posts and says so, once per request, on stderr.
func TestE2ENotifyServeDevMode(t *testing.T) {
	w := newWorld(t)
	w.init()
	port := freePort(t)
	os.WriteFile(filepath.Join(w.home, ".roscoe", ".env"),
		[]byte("TWILIO_ACCOUNT_SID=AC123\nTWILIO_AUTH_TOKEN=tok\nTWILIO_TO=+1\nTWILIO_FROM=+2\n"), 0o600)
	p := w.startBackground("notify", "serve", "--port", fmt.Sprint(port), "--path", "/hook")
	p.waitFor(`listening on 127\.0\.0\.1:` + fmt.Sprint(port) + `/hook`)
	code, _ := postForm(t, fmt.Sprintf("http://127.0.0.1:%d/hook", port), url.Values{"From": {"+1"}, "Body": {"ok"}}, "")
	if code != 200 {
		t.Errorf("dev-mode webhook: %d", code)
	}
	p.waitFor(`without signature validation`)
	if code := p.stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestE2ERelayListenUnlinked(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "relay", "listen"), 1, "no relay credentials", "roscoe upgrade")
}
