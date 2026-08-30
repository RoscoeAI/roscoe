package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// twilio sends SMS via the Twilio REST API and receives replies on a
// webhook. The env contract matches the original roscoe.sh TS stack so its
// .env drops in unchanged: TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, and either
// TWILIO_MESSAGING_SERVICE_SID (preferred — A2P campaign sending) or
// TWILIO_FROM / TWILIO_FROM_NUMBER; TWILIO_TO is the operator's phone.
// Inbound signature validation uses ROSCOE_SMS_WEBHOOK_PUBLIC_URL (legacy
// name) or TWILIO_WEBHOOK_URL — the exact public URL Twilio POSTs to.
type twilio struct {
	sid        string
	token      string
	msgService string // MessagingServiceSid; wins over from when both set
	from       string
	to         string
	webhookURL string // "" = dev mode: accept inbound without validation
	client     *http.Client
}

func newTwilio(_ config.NotifyCfg, env map[string]string) (*twilio, error) {
	from := env["TWILIO_FROM"]
	if from == "" {
		from = env["TWILIO_FROM_NUMBER"]
	}
	webhookURL := env["ROSCOE_SMS_WEBHOOK_PUBLIC_URL"]
	if webhookURL == "" {
		webhookURL = env["TWILIO_WEBHOOK_URL"]
	}
	t := &twilio{
		sid:        env["TWILIO_ACCOUNT_SID"],
		token:      env["TWILIO_AUTH_TOKEN"],
		msgService: env["TWILIO_MESSAGING_SERVICE_SID"],
		from:       from,
		to:         env["TWILIO_TO"],
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
	var missing []string
	for _, v := range []struct{ name, val string }{
		{"TWILIO_ACCOUNT_SID", t.sid},
		{"TWILIO_AUTH_TOKEN", t.token},
		{"TWILIO_TO", t.to},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if t.msgService == "" && t.from == "" {
		missing = append(missing, "TWILIO_MESSAGING_SERVICE_SID or TWILIO_FROM")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("notify: twilio-sms requires env %s", strings.Join(missing, ", "))
	}
	return t, nil
}

func (t *twilio) Send(ctx context.Context, m Message) error {
	// SMS has no title field; fold it into the body.
	body := m.Body
	if m.Title != "" {
		body = m.Title + "\n" + m.Body
	}
	form := url.Values{
		"To":   {t.to},
		"Body": {body},
	}
	// Mirror the original TS stack: MessagingServiceSid (A2P campaign
	// sending) wins over a bare From number.
	if t.msgService != "" {
		form.Set("MessagingServiceSid", t.msgService)
	} else {
		form.Set("From", t.from)
	}
	u := "https://api.twilio.com/2010-04-01/Accounts/" + url.PathEscape(t.sid) + "/Messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("notify: build twilio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.sid, t.token)
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: twilio send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify: twilio send: status %d: %s", resp.StatusCode, readExcerpt(resp.Body))
	}
	io.Copy(io.Discard, resp.Body) //nolint — drain so the connection is reused
	return nil
}

// InboundHandler parses Twilio's url-encoded webhook POST (From, Body),
// validates X-Twilio-Signature, and answers with empty TwiML. When
// TWILIO_WEBHOOK_URL is unset it accepts the request and logs a warning
// (dev mode — signature validation needs the public URL).
func (t *twilio) InboundHandler(onReply func(Reply)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if t.webhookURL == "" {
			fmt.Fprintln(os.Stderr, "roscoe: warning: TWILIO_WEBHOOK_URL unset; accepting inbound SMS without signature validation (dev mode)")
		} else if !validTwilioSignature(t.token, t.webhookURL, r.PostForm, r.Header.Get("X-Twilio-Signature")) {
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
		if onReply != nil {
			onReply(Reply{
				From: r.PostForm.Get("From"),
				Body: r.PostForm.Get("Body"),
				At:   time.Now(),
			})
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "<Response/>")
	})
}

// validTwilioSignature implements Twilio's request-signing scheme:
// HMAC-SHA1 over the webhook URL followed by every POST parameter sorted
// by key, each appended as key+value; base64; constant-time compare.
func validTwilioSignature(authToken, webhookURL string, params url.Values, got string) bool {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	mac := hmac.New(sha1.New, []byte(authToken))
	io.WriteString(mac, webhookURL)
	for _, k := range keys {
		for _, v := range params[k] {
			io.WriteString(mac, k)
			io.WriteString(mac, v)
		}
	}
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(got))
}
