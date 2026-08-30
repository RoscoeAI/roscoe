package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// Inbound is one operator SMS delivered over the bridge.
type Inbound struct {
	ID          string `json:"id"`
	MessageSid  string `json:"messageSid"`
	ChannelID   string `json:"channelId"`
	ChannelType string `json:"channelType"`
	From        string `json:"from"`
	Body        string `json:"body"`
	ReceivedAt  string `json:"receivedAt"`
}

// HelloAck is the server's greeting after a successful dial.
type HelloAck struct {
	Phone              string `json:"phone"`
	ClientID           string `json:"clientId"`
	Active             bool   `json:"active"`
	SubscriptionStatus string `json:"subscriptionStatus"`
	UserEmail          string `json:"userEmail"`
}

type serverMessage struct {
	Type string `json:"type"`
	// Message is an object for inbound-channel-message and a plain string
	// for type "error" — RawMessage covers both.
	Message   json.RawMessage `json:"message"`
	RequestID string          `json:"requestId"` // outbound-message-result
	Result    json.RawMessage `json:"result"`
	// hello-ack fields inline
	Phone              string `json:"phone"`
	ClientID           string `json:"clientId"`
	Active             bool   `json:"active"`
	SubscriptionStatus string `json:"subscriptionStatus"`
	UserEmail          string `json:"userEmail"`
}

func (m *serverMessage) errorText() string {
	var s string
	if json.Unmarshal(m.Message, &s) == nil && s != "" {
		return s
	}
	return string(m.Message)
}

func (c *Credentials) wsURL() string {
	u := c.BaseURL
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return strings.TrimRight(u, "/") + "/api/relay/ws"
}

// dial opens the bridge and consumes the hello-ack.
func (c *Credentials) dial(ctx context.Context) (*websocket.Conn, *HelloAck, error) {
	if err := c.EnsureFresh(ctx); err != nil {
		return nil, nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.AccessToken)
	conn, _, err := websocket.Dial(ctx, c.wsURL(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, nil, fmt.Errorf("relay: dial %s: %w", c.wsURL(), err)
	}
	conn.SetReadLimit(1 << 20)
	msg, err := readMessage(ctx, conn)
	if err != nil {
		conn.Close(websocket.StatusProtocolError, "no hello-ack")
		return nil, nil, fmt.Errorf("relay: waiting for hello-ack: %w", err)
	}
	if msg.Type != "hello-ack" {
		conn.Close(websocket.StatusProtocolError, "unexpected greeting")
		return nil, nil, fmt.Errorf("relay: expected hello-ack, got %q", msg.Type)
	}
	hello := &HelloAck{
		Phone: msg.Phone, ClientID: msg.ClientID, Active: msg.Active,
		SubscriptionStatus: msg.SubscriptionStatus, UserEmail: msg.UserEmail,
	}
	if !hello.Active {
		conn.Close(websocket.StatusNormalClosure, "inactive subscription")
		return nil, nil, fmt.Errorf("relay: subscription is not active (status %q) — finish checkout at %s/link", hello.SubscriptionStatus, c.BaseURL)
	}
	return conn, hello, nil
}

func readMessage(ctx context.Context, conn *websocket.Conn) (*serverMessage, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("relay: bad frame: %w", err)
	}
	return &msg, nil
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// Connect holds the bridge open, invoking onInbound for every operator
// reply (acking each), reconnecting with capped backoff until ctx ends.
// onState, if non-nil, receives human-readable connection events.
func (c *Credentials) Connect(ctx context.Context, onInbound func(Inbound), onState func(string)) error {
	note := func(s string) {
		if onState != nil {
			onState(s)
		}
	}
	backoff := time.Second
	for {
		conn, hello, err := c.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			note(fmt.Sprintf("connect failed: %v — retrying in %s", err, backoff))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > time.Minute {
				backoff = time.Minute
			}
			continue
		}
		backoff = time.Second
		note(fmt.Sprintf("connected as %s (%s)", hello.Phone, hello.SubscriptionStatus))
		err = c.pump(ctx, conn, onInbound)
		conn.Close(websocket.StatusNormalClosure, "")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		note(fmt.Sprintf("bridge dropped: %v — reconnecting", err))
	}
}

func (c *Credentials) pump(ctx context.Context, conn *websocket.Conn, onInbound func(Inbound)) error {
	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return err
		}
		switch msg.Type {
		case "inbound-channel-message":
			var in Inbound
			if err := json.Unmarshal(msg.Message, &in); err != nil {
				continue
			}
			if onInbound != nil {
				onInbound(in)
			}
			_ = writeJSON(ctx, conn, map[string]any{"type": "ack-inbound", "messageIds": []string{in.ID}})
		case "error":
			// server-reported soft error; keep the connection
		default:
			// lane-mirror / outbound results for other requests: ignore
		}
	}
}

// Send delivers one escalation SMS through the relay: dial, send, await
// the matching outbound-message-result, close.
func (c *Credentials) Send(ctx context.Context, body string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, _, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	reqID := NewClientID()
	if err := writeJSON(ctx, conn, map[string]any{
		"type": "send-operator-message", "requestId": reqID, "body": body,
	}); err != nil {
		return fmt.Errorf("relay: send: %w", err)
	}
	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return fmt.Errorf("relay: awaiting send result: %w", err)
		}
		if msg.Type == "outbound-message-result" && msg.RequestID == reqID {
			return nil
		}
		if msg.Type == "error" {
			return fmt.Errorf("relay: server error during send: %s", msg.errorText())
		}
	}
}
