package main

// The hosted relay, stood in for by a loopback control plane: the device
// link, token refresh, billing status, and the websocket bridge that
// carries operator messages out and replies back.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeRelay struct {
	url      string
	mu       sync.Mutex
	polls    int
	refreshs int
	refuse   bool // refresh answers 401: relink
	sent     []string
	acked    []string
	auths    []string
}

func startFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRelay{url: "http://" + ln.Addr().String()}
	session := func(tag string) map[string]any {
		now := time.Now().UTC()
		return map[string]any{
			"ok": true, "status": "linked",
			"accessToken": "acc-" + tag, "accessTokenExpiresAt": now.Add(time.Hour).Format(time.RFC3339),
			"refreshToken": "ref-" + tag, "refreshTokenExpiresAt": now.Add(720 * time.Hour).Format(time.RFC3339),
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "deviceCode": "dev-1", "verificationUrl": fr.url + "/link",
			"verificationUrlComplete": fr.url + "/link?code=ABCD-EFGH", "pollIntervalSeconds": 1,
		})
	})
	mux.HandleFunc("POST /api/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.polls++
		n := fr.polls
		fr.mu.Unlock()
		if n == 1 { // the person has not approved yet
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(session("1"))
	})
	mux.HandleFunc("POST /api/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.refreshs++
		refuse := fr.refuse
		fr.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "refresh token revoked"})
			return
		}
		json.NewEncoder(w).Encode(session("2"))
	})
	mux.HandleFunc("GET /api/relay/billing/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": map[string]any{
			"phone": r.URL.Query().Get("phone"), "subscriptionStatus": "active", "active": true, "roundTripVerified": true,
		}})
	})
	mux.HandleFunc("/api/relay/ws", func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.auths = append(fr.auths, r.Header.Get("Authorization"))
		fr.mu.Unlock()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		defer conn.Close(websocket.StatusNormalClosure, "")
		hello := `{"type":"hello-ack","phone":"+15551234567","clientId":"x","active":true,"subscriptionStatus":"active","userEmail":"op@example.com"}`
		if conn.Write(ctx, websocket.MessageText, []byte(hello)) != nil {
			return
		}
		// A reply from the operator's phone arrives shortly after connecting.
		go func() {
			time.Sleep(150 * time.Millisecond)
			in := `{"type":"inbound-channel-message","message":{"id":"m-1","from":"+15550001111","body":"yes, go ahead","receivedAt":"2026-09-03T21:00:00Z"}}`
			_ = conn.Write(ctx, websocket.MessageText, []byte(in))
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type       string   `json:"type"`
				RequestID  string   `json:"requestId"`
				Body       string   `json:"body"`
				MessageIDs []string `json:"messageIds"`
			}
			_ = json.Unmarshal(data, &msg)
			switch msg.Type {
			case "send-operator-message":
				fr.mu.Lock()
				fr.sent = append(fr.sent, msg.Body)
				fr.mu.Unlock()
				_ = conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"outbound-message-result","requestId":%q,"result":{"ok":true}}`, msg.RequestID)))
			case "ack-inbound":
				fr.mu.Lock()
				fr.acked = append(fr.acked, msg.MessageIDs...)
				fr.mu.Unlock()
			}
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fr
}

func (fr *fakeRelay) snapshot() (sent, acked, auths []string, refreshs int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]string{}, fr.sent...), append([]string{}, fr.acked...), append([]string{}, fr.auths...), fr.refreshs
}

func TestE2ERelayLinkAndUse(t *testing.T) {
	w := newWorld(t)
	w.init()
	fr := startFakeRelay(t)
	credsPath := filepath.Join(w.home, ".roscoe", "relay.json")

	// The device link: a URL to finish in the browser, a pending poll, then
	// linked; the notify channel flips and billing is reported.
	r := w.run("", "upgrade", "--phone", "5551234567", "--base-url", fr.url)
	expect(t, r, 0, "using +15551234567", "Finish linking in your browser:", fr.url+"/link?code=ABCD-EFGH",
		"Waiting for approval", "quorum.notify.channel → roscoe-relay",
		"Linked. phone=+15551234567 subscription=active active=true", "roscoe notify test")
	if _, err := os.Stat(credsPath); err != nil {
		t.Fatal("no relay.json after linking")
	}
	if strings.Contains(r.all(), "acc-1") || strings.Contains(r.all(), "ref-1") {
		t.Fatal("a relay token was printed")
	}
	expect(t, w.run("", "config", "get", "quorum.notify.channel"), 0, "roscoe-relay")
	// Linking again with the same phone is a no-op.
	expect(t, w.run("", "upgrade", "--phone", "+15551234567", "--base-url", fr.url), 0, "already linked")

	// Status masks the tokens and asks the server about billing.
	r = w.run("", "relay", "status")
	expect(t, r, 0, "phone:   +15551234567", "server:  "+fr.url, "access:  …cc-1", "refresh: …ef-1",
		"billing: subscription=active active=true round-trip-verified=true")
	if strings.Contains(r.stdout, "acc-1") {
		t.Error("the access token was printed in full")
	}

	// notify test goes out over the bridge as one operator message.
	expect(t, w.run("", "notify", "test", "deploy is waiting on you"), 0, "sent via roscoe-relay")
	sent, _, auths, _ := fr.snapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "deploy is waiting on you") {
		t.Errorf("relay received %q", sent)
	}
	if len(auths) == 0 || auths[0] != "Bearer acc-1" {
		t.Errorf("bridge auth = %q", auths)
	}

	// listen holds the bridge open, prints replies, and acks them.
	p := w.startBackground("relay", "listen")
	p.waitFor(`\[bridge\] connected as \+15551234567 \(active\)`)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.outText(), "[reply]") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if out := p.outText(); !strings.Contains(out, `from=+15550001111`) || !strings.Contains(out, `body="yes, go ahead"`) {
		t.Errorf("listen output = %q", out)
	}
	if code := p.stop(); code != 0 {
		t.Errorf("listen exit %d after SIGINT:\n%s", code, p.errText())
	}
	if _, acked, _, _ := fr.snapshot(); len(acked) != 1 || acked[0] != "m-1" {
		t.Errorf("acked %v, want m-1", acked)
	}

	// An expired access token is refreshed before the bridge is dialled,
	// and the fresh tokens are saved.
	expireAccess(t, credsPath)
	expect(t, w.run("", "notify", "test", "again"), 0, "sent via roscoe-relay")
	if _, _, auths, refreshs := fr.snapshot(); refreshs != 1 || auths[len(auths)-1] != "Bearer acc-2" {
		t.Errorf("refreshs=%d auths=%v; want one refresh and the new token on the wire", refreshs, auths)
	}
	if !strings.Contains(readFile(credsPath), "acc-2") {
		t.Error("the refreshed token was not saved")
	}

	// A revoked refresh token means relinking, said plainly.
	fr.mu.Lock()
	fr.refuse = true
	fr.mu.Unlock()
	expireAccess(t, credsPath)
	r = w.run("", "notify", "test", "once more")
	if r.code == 0 || !strings.Contains(strings.ToLower(r.all()), "relink") && !strings.Contains(r.all(), "roscoe upgrade") {
		t.Errorf("revoked refresh: %s", r)
	}

	// unlink removes the link; status then points at upgrade.
	expect(t, w.run("", "relay", "unlink"), 0, "removed", "roscoe upgrade --phone")
	expect(t, w.run("", "relay", "status"), 1, "no relay credentials")
	_ = context.Background
}

// expireAccess rewrites the saved credentials so the access token is past
// its expiry and the next use has to refresh it.
func expireAccess(t *testing.T, credsPath string) {
	t.Helper()
	var creds map[string]any
	if err := json.Unmarshal([]byte(readFile(credsPath)), &creds); err != nil {
		t.Fatalf("relay.json: %v", err)
	}
	creds["access_token_expires_at"] = "2020-01-01T00:00:00Z"
	b, _ := json.Marshal(creds)
	if err := os.WriteFile(credsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
