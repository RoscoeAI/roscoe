package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// ntfy publishes to an ntfy.sh-compatible server. Replies arrive via the
// subscribe API, which is out of slice 1 — InboundHandler is nil.
type ntfy struct {
	server string // no trailing slash
	topic  string
	token  string // env NTFY_TOKEN; "" = unauthenticated
	client *http.Client
}

func newNtfy(cfg config.NotifyCfg, env map[string]string) (*ntfy, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("notify: ntfy requires notify.topic")
	}
	server := cfg.Server
	if server == "" {
		server = "https://ntfy.sh"
	}
	return &ntfy{
		server: strings.TrimRight(server, "/"),
		topic:  cfg.Topic,
		token:  env["NTFY_TOKEN"],
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (n *ntfy) Send(ctx context.Context, m Message) error {
	u := n.server + "/" + url.PathEscape(n.topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(m.Body))
	if err != nil {
		return fmt.Errorf("notify: build ntfy request: %w", err)
	}
	if m.Title != "" {
		req.Header.Set("Title", m.Title)
	}
	if m.Priority != 0 {
		req.Header.Set("Priority", strconv.Itoa(m.Priority))
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: ntfy send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify: ntfy send: status %d: %s", resp.StatusCode, readExcerpt(resp.Body))
	}
	io.Copy(io.Discard, resp.Body) //nolint — drain so the connection is reused
	return nil
}

func (n *ntfy) InboundHandler(onReply func(Reply)) http.Handler { return nil }
