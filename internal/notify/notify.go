// Package notify delivers fleet notifications (quorum auto-answers,
// escalations, task completion) over a configured channel and, where the
// channel supports it, receives human replies.
package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Message is one outbound notification. Priority follows ntfy semantics
// (1 min .. 5 max, 0 = channel default); channels without a priority
// concept ignore it.
type Message struct {
	Title    string
	Body     string
	Priority int
}

// Reply is one inbound human reply.
type Reply struct {
	From string
	Body string
	At   time.Time
}

type Notifier interface {
	Send(ctx context.Context, m Message) error
	// InboundHandler returns nil when the channel has no inbound path.
	InboundHandler(onReply func(Reply)) http.Handler
}

// New builds the Notifier for cfg.Channel: "twilio-sms" or "ntfy".
// Credentials come from env (the loaded env file), never from cfg.
func New(cfg config.NotifyCfg, env map[string]string) (Notifier, error) {
	switch cfg.Channel {
	case "twilio-sms":
		return newTwilio(cfg, env)
	case "ntfy":
		return newNtfy(cfg, env)
	default:
		return nil, fmt.Errorf("notify: unknown channel %q (want \"twilio-sms\" or \"ntfy\")", cfg.Channel)
	}
}

// readExcerpt drains up to 4KB of an error-response body into a single
// log-friendly line.
func readExcerpt(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 4<<10))
	if err != nil && len(b) == 0 {
		return fmt.Sprintf("(unreadable body: %v)", err)
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	const max = 200
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
