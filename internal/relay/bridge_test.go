package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newWSServer starts an httptest server that upgrades to WebSocket and runs
// script on the accepted connection. The request is passed through so scripts
// can inspect handshake headers.
func newWSServer(t *testing.T, script func(ctx context.Context, conn *websocket.Conn, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		defer conn.CloseNow()
		script(r.Context(), conn, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bridgeCreds returns credentials whose access token is fresh (so dial's
// EnsureFresh never phones home) and whose state file lives in a temp dir.
func bridgeCreds(t *testing.T, baseURL string) *Credentials {
	t.Helper()
	t.Setenv("ROSCOE_RELAY_STATE", filepath.Join(t.TempDir(), "relay.json"))
	return &Credentials{
		ClientID:              "cid-bridge",
		Phone:                 "+15550100",
		BaseURL:               baseURL,
		AccessToken:           "bridge-access-token",
		AccessTokenExpiresAt:  time.Now().Add(time.Hour),
		RefreshToken:          "bridge-refresh-token",
		RefreshTokenExpiresAt: time.Now().Add(24 * time.Hour),
	}
}

const helloActive = `{"type":"hello-ack","phone":"+15550100","clientId":"cid-bridge",` +
	`"active":true,"subscriptionStatus":"active","userEmail":"op@example.com"}`

func TestWSURL(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{"https://roscoe.sh", "wss://roscoe.sh/api/relay/ws"},
		{"https://roscoe.sh/", "wss://roscoe.sh/api/relay/ws"},
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080/api/relay/ws"},
		{"http://localhost:3000/", "ws://localhost:3000/api/relay/ws"},
	}
	for _, tt := range tests {
		c := &Credentials{BaseURL: tt.base}
		if got := c.wsURL(); got != tt.want {
			t.Errorf("wsURL(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func TestServerMessageErrorText(t *testing.T) {
	m := &serverMessage{Message: json.RawMessage(`"quota exceeded"`)}
	if got := m.errorText(); got != "quota exceeded" {
		t.Errorf("errorText JSON string = %q, want unquoted text", got)
	}
	m = &serverMessage{Message: json.RawMessage(`{"detail":"odd"}`)}
	if got := m.errorText(); got != `{"detail":"odd"}` {
		t.Errorf("errorText non-string = %q, want raw message", got)
	}
}

func TestDialSendsBearerAndParsesHello(t *testing.T) {
	authCh := make(chan string, 1)
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		authCh <- r.Header.Get("Authorization")
		if err := conn.Write(ctx, websocket.MessageText, []byte(helloActive)); err != nil {
			return
		}
		conn.Read(ctx) // hold open until the client closes
	})

	c := bridgeCreds(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, hello, err := c.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	select {
	case auth := <-authCh:
		if auth != "Bearer bridge-access-token" {
			t.Errorf("Authorization header = %q, want Bearer bridge-access-token", auth)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported the Authorization header")
	}
	if hello.Phone != "+15550100" || hello.ClientID != "cid-bridge" ||
		!hello.Active || hello.SubscriptionStatus != "active" || hello.UserEmail != "op@example.com" {
		t.Errorf("hello-ack parsed as %+v", hello)
	}
}

func TestDialInactiveSubscription(t *testing.T) {
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		inactive := `{"type":"hello-ack","phone":"+15550100","clientId":"cid-bridge",` +
			`"active":false,"subscriptionStatus":"past_due"}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(inactive)); err != nil {
			return
		}
		conn.Read(ctx)
	})

	c := bridgeCreds(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, _, err := c.dial(ctx)
	if err == nil {
		t.Fatal("dial with active:false: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not active") || !strings.Contains(err.Error(), "past_due") {
		t.Errorf("error %q should name the inactive subscription and its status", err)
	}
}

func TestDialUnexpectedGreeting(t *testing.T) {
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"lane-mirror"}`)); err != nil {
			return
		}
		conn.Read(ctx)
	})

	c := bridgeCreds(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, _, err := c.dial(ctx)
	if err == nil || !strings.Contains(err.Error(), `expected hello-ack, got "lane-mirror"`) {
		t.Fatalf("dial with wrong greeting: err = %v", err)
	}
}

func TestConnectDeliversInboundAndAcks(t *testing.T) {
	ackCh := make(chan []string, 1)
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if err := conn.Write(ctx, websocket.MessageText, []byte(helloActive)); err != nil {
			return
		}
		inbound := `{"type":"inbound-channel-message","message":{"id":"msg-42",` +
			`"messageSid":"SM123","channelId":"ch-1","channelType":"sms",` +
			`"from":"+15550100","body":"yes, ship it","receivedAt":"2026-08-30T12:00:00Z"}}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(inbound)); err != nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var ack struct {
			Type       string   `json:"type"`
			MessageIDs []string `json:"messageIds"`
		}
		if json.Unmarshal(data, &ack) == nil && ack.Type == "ack-inbound" {
			ackCh <- ack.MessageIDs
		}
		conn.Read(ctx) // hold until the client tears down
	})

	c := bridgeCreds(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inboundCh := make(chan Inbound, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Connect(ctx, func(in Inbound) { inboundCh <- in }, nil)
	}()

	select {
	case in := <-inboundCh:
		if in.ID != "msg-42" || in.From != "+15550100" || in.Body != "yes, ship it" ||
			in.MessageSid != "SM123" || in.ChannelType != "sms" {
			t.Errorf("inbound parsed as %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onInbound never fired")
	}

	select {
	case ids := <-ackCh:
		if len(ids) != 1 || ids[0] != "msg-42" {
			t.Errorf("ack-inbound messageIds = %v, want [msg-42]", ids)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received ack-inbound")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Connect returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return after cancel")
	}
}

func TestSendDeliversAndMatchesRequestID(t *testing.T) {
	type sendFrame struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Body      string `json:"body"`
	}
	frameCh := make(chan sendFrame, 1)
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if err := conn.Write(ctx, websocket.MessageText, []byte(helloActive)); err != nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f sendFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		frameCh <- f
		// A result for some other request must be ignored...
		conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"outbound-message-result","requestId":"someone-else"}`))
		// ...then the matching one completes the Send.
		b, _ := json.Marshal(map[string]string{"type": "outbound-message-result", "requestId": f.RequestID})
		conn.Write(ctx, websocket.MessageText, b)
		conn.Read(ctx)
	})

	c := bridgeCreds(t, srv.URL)
	if err := c.Send(context.Background(), "deploy failed on prod — reply YES to roll back"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case f := <-frameCh:
		if f.Type != "send-operator-message" {
			t.Errorf("frame type = %q, want send-operator-message", f.Type)
		}
		if f.Body != "deploy failed on prod — reply YES to roll back" {
			t.Errorf("frame body = %q", f.Body)
		}
		if len(f.RequestID) != 36 {
			t.Errorf("requestId %q does not look like a uuid", f.RequestID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the send frame")
	}
}

func TestSendServerErrorFrame(t *testing.T) {
	srv := newWSServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if err := conn.Write(ctx, websocket.MessageText, []byte(helloActive)); err != nil {
			return
		}
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		conn.Write(ctx, websocket.MessageText, []byte(`{"type":"error","message":"rate limited"}`))
		conn.Read(ctx)
	})

	c := bridgeCreds(t, srv.URL)
	err := c.Send(context.Background(), "hello")
	if err == nil {
		t.Fatal("Send with server error frame: want error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error %q should contain the server's message text", err)
	}
}
