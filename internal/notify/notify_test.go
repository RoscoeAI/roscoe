package notify

import (
	"errors"
	"io"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

// fullTwilioEnv returns an env map that satisfies every twilio-sms
// requirement, for tests that need construction to succeed.
func fullTwilioEnv() map[string]string {
	return map[string]string{
		"TWILIO_ACCOUNT_SID":           "AC0123456789abcdef",
		"TWILIO_AUTH_TOKEN":            "test-auth-token",
		"TWILIO_TO":                    "+15550001111",
		"TWILIO_MESSAGING_SERVICE_SID": "MG0123456789abcdef",
	}
}

func TestNewTwilioSMS(t *testing.T) {
	n, err := New(config.NotifyCfg{Channel: "twilio-sms"}, fullTwilioEnv())
	if err != nil {
		t.Fatalf("New(twilio-sms) error = %v, want nil", err)
	}
	if n == nil {
		t.Fatal("New(twilio-sms) returned nil Notifier")
	}
	if _, ok := n.(*twilio); !ok {
		t.Fatalf("New(twilio-sms) returned %T, want *twilio", n)
	}
}

func TestNewTwilioSMSPropagatesEnvError(t *testing.T) {
	n, err := New(config.NotifyCfg{Channel: "twilio-sms"}, map[string]string{})
	if err == nil {
		t.Fatal("New(twilio-sms) with empty env: want error, got nil")
	}
	if !strings.Contains(err.Error(), "twilio-sms requires env") {
		t.Errorf("error %q does not mention missing env requirement", err)
	}
	// Note: New returns newTwilio's (*twilio)(nil) inside a non-nil
	// interface on this path (typed-nil gotcha), so we deliberately do
	// not assert n == nil — callers must gate on err, not the Notifier.
	if tw, ok := n.(*twilio); ok && tw != nil {
		t.Errorf("notifier = %+v, want nil concrete value on error", tw)
	}
}

func TestNewRoscoeRelay(t *testing.T) {
	n, err := New(config.NotifyCfg{Channel: "roscoe-relay"}, nil)
	if err == nil {
		t.Fatal("New(roscoe-relay) want guidance error, got nil")
	}
	if n != nil {
		t.Fatalf("New(roscoe-relay) notifier = %v, want nil", n)
	}
	for _, want := range []string{`channel "roscoe-relay"`, "relay package", "roscoe upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New(roscoe-relay) error %q missing %q", err, want)
		}
	}
}

func TestNewUnknownChannel(t *testing.T) {
	tests := []struct {
		name    string
		channel string
	}{
		{"unknown name", "slack"},
		{"empty channel", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := New(config.NotifyCfg{Channel: tt.channel}, nil)
			if err == nil {
				t.Fatalf("New(%q) want error, got nil", tt.channel)
			}
			if n != nil {
				t.Fatalf("New(%q) notifier = %v, want nil", tt.channel, n)
			}
			msg := err.Error()
			if !strings.Contains(msg, "unknown channel") {
				t.Errorf("error %q missing %q", msg, "unknown channel")
			}
			// The error should name the offered channels so the operator
			// can fix the config without reading source.
			for _, want := range []string{`"twilio-sms"`, `"roscoe-relay"`} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q missing valid channel %s", msg, want)
				}
			}
		})
	}
}

// errAfter reads from r, then returns err once r is drained.
type errAfter struct {
	r   io.Reader
	err error
}

func (e errAfter) Read(p []byte) (int, error) {
	n, rerr := e.r.Read(p)
	if rerr == io.EOF {
		return n, e.err
	}
	return n, rerr
}

func TestReadExcerpt(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		r    io.Reader
		want string
	}{
		{
			name: "collapses whitespace to one line",
			r:    strings.NewReader("a\n  b\t\tc \r\n d"),
			want: "a b c d",
		},
		{
			name: "empty body",
			r:    strings.NewReader(""),
			want: "",
		},
		{
			name: "truncates past 200 chars with ellipsis",
			r:    strings.NewReader(strings.Repeat("x", 300)),
			want: strings.Repeat("x", 200) + "...",
		},
		{
			name: "unreadable body reports the error",
			r:    errAfter{r: strings.NewReader(""), err: boom},
			want: "(unreadable body: boom)",
		},
		{
			name: "partial read before error keeps the bytes",
			r:    errAfter{r: strings.NewReader("partial data"), err: boom},
			want: "partial data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readExcerpt(tt.r); got != tt.want {
				t.Errorf("readExcerpt = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadExcerptStopsAt4KB(t *testing.T) {
	// A body larger than the 4KB drain limit must still come back as a
	// bounded single line (the limit feeds the 200-char cut).
	got := readExcerpt(strings.NewReader(strings.Repeat("y", 8<<10)))
	want := strings.Repeat("y", 200) + "..."
	if got != want {
		t.Errorf("readExcerpt(8KB) = %d chars, want %d", len(got), len(want))
	}
}
