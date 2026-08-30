package relay

import (
	"context"
	"net/http"

	"roscoe.sh/roscoe/internal/notify"
)

// Notifier adapts the hosted relay to the notify.Notifier interface for
// the "roscoe-relay" channel. Replies do not arrive on a webhook — they
// come over the bridge (Credentials.Connect) — so InboundHandler is nil.
type Notifier struct {
	Creds *Credentials
}

func NewNotifier() (*Notifier, error) {
	c, err := LoadCreds()
	if err != nil {
		return nil, err
	}
	return &Notifier{Creds: c}, nil
}

func (n *Notifier) Send(ctx context.Context, m notify.Message) error {
	body := m.Body
	if m.Title != "" {
		body = m.Title + "\n" + m.Body
	}
	return n.Creds.Send(ctx, body)
}

func (n *Notifier) InboundHandler(func(notify.Reply)) http.Handler { return nil }
