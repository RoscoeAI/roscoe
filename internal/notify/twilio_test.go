package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// --- newTwilio env contract ---------------------------------------------

func TestNewTwilioMissingEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // exact error message
	}{
		{
			name: "everything missing lists all names in order",
			env:  map[string]string{},
			want: "notify: twilio-sms requires env TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_TO, TWILIO_MESSAGING_SERVICE_SID or TWILIO_FROM",
		},
		{
			name: "only account sid missing",
			env: map[string]string{
				"TWILIO_AUTH_TOKEN":            "tok",
				"TWILIO_TO":                    "+15550001111",
				"TWILIO_MESSAGING_SERVICE_SID": "MG1",
			},
			want: "notify: twilio-sms requires env TWILIO_ACCOUNT_SID",
		},
		{
			name: "neither messaging service nor from",
			env: map[string]string{
				"TWILIO_ACCOUNT_SID": "AC1",
				"TWILIO_AUTH_TOKEN":  "tok",
				"TWILIO_TO":          "+15550001111",
			},
			want: "notify: twilio-sms requires env TWILIO_MESSAGING_SERVICE_SID or TWILIO_FROM",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tw, err := newTwilio(config.NotifyCfg{}, tt.env)
			if err == nil {
				t.Fatalf("newTwilio = %+v, want error", tw)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q\nwant    %q", err.Error(), tt.want)
			}
		})
	}
}

func TestNewTwilioFromResolution(t *testing.T) {
	base := map[string]string{
		"TWILIO_ACCOUNT_SID": "AC1",
		"TWILIO_AUTH_TOKEN":  "tok",
		"TWILIO_TO":          "+15550001111",
	}
	tests := []struct {
		name           string
		extra          map[string]string
		wantFrom       string
		wantMsgService string
	}{
		{
			name:           "messaging service alone is enough",
			extra:          map[string]string{"TWILIO_MESSAGING_SERVICE_SID": "MG1"},
			wantMsgService: "MG1",
		},
		{
			name:     "TWILIO_FROM alone is enough",
			extra:    map[string]string{"TWILIO_FROM": "+15559990000"},
			wantFrom: "+15559990000",
		},
		{
			name:     "TWILIO_FROM_NUMBER is a fallback for TWILIO_FROM",
			extra:    map[string]string{"TWILIO_FROM_NUMBER": "+15558880000"},
			wantFrom: "+15558880000",
		},
		{
			name: "TWILIO_FROM wins over TWILIO_FROM_NUMBER",
			extra: map[string]string{
				"TWILIO_FROM":        "+15559990000",
				"TWILIO_FROM_NUMBER": "+15558880000",
			},
			wantFrom: "+15559990000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tt.extra {
				env[k] = v
			}
			tw, err := newTwilio(config.NotifyCfg{}, env)
			if err != nil {
				t.Fatalf("newTwilio error = %v", err)
			}
			if tw.from != tt.wantFrom {
				t.Errorf("from = %q, want %q", tw.from, tt.wantFrom)
			}
			if tw.msgService != tt.wantMsgService {
				t.Errorf("msgService = %q, want %q", tw.msgService, tt.wantMsgService)
			}
		})
	}
}

func TestNewTwilioWebhookURLPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{
			name:  "unset means dev mode",
			extra: map[string]string{},
			want:  "",
		},
		{
			name:  "TWILIO_WEBHOOK_URL used when legacy unset",
			extra: map[string]string{"TWILIO_WEBHOOK_URL": "https://b.example/hook"},
			want:  "https://b.example/hook",
		},
		{
			name: "legacy ROSCOE_SMS_WEBHOOK_PUBLIC_URL wins",
			extra: map[string]string{
				"ROSCOE_SMS_WEBHOOK_PUBLIC_URL": "https://a.example/hook",
				"TWILIO_WEBHOOK_URL":            "https://b.example/hook",
			},
			want: "https://a.example/hook",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{
				"TWILIO_ACCOUNT_SID":           "AC1",
				"TWILIO_AUTH_TOKEN":            "tok",
				"TWILIO_TO":                    "+15550001111",
				"TWILIO_MESSAGING_SERVICE_SID": "MG1",
			}
			for k, v := range tt.extra {
				env[k] = v
			}
			tw, err := newTwilio(config.NotifyCfg{}, env)
			if err != nil {
				t.Fatalf("newTwilio error = %v", err)
			}
			if tw.webhookURL != tt.want {
				t.Errorf("webhookURL = %q, want %q", tw.webhookURL, tt.want)
			}
		})
	}
}

// --- Send ----------------------------------------------------------------

// rewriteTransport redirects every request to the httptest server while
// recording the URL the client originally asked for. This lets Send's
// hardcoded https://api.twilio.com URL land on a local listener without
// touching the source.
type rewriteTransport struct {
	target      *url.URL
	originalURL *string
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*rt.originalURL = req.URL.String()
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// capturedRequest is what the fake Twilio server saw.
type capturedRequest struct {
	method      string
	path        string
	contentType string
	user, pass  string
	authOK      bool
	form        url.Values
}

// newSendFixture builds a twilio whose client talks to a fake Twilio
// server responding with status. It returns the twilio, a pointer filled
// with the last captured request, and a pointer to the original URL.
func newSendFixture(t *testing.T, tw *twilio, status int, respBody string) (*capturedRequest, *string) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("server: ParseForm: %v", err)
		}
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.contentType = r.Header.Get("Content-Type")
		captured.user, captured.pass, captured.authOK = r.BasicAuth()
		captured.form = r.PostForm
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	originalURL := new(string)
	tw.client = &http.Client{Transport: rewriteTransport{target: target, originalURL: originalURL}}
	return captured, originalURL
}

func TestSendRequestConstruction(t *testing.T) {
	tests := []struct {
		name       string
		tw         twilio
		msg        Message
		wantBody   string
		wantParams map[string]string // param -> value; "" means must be absent
	}{
		{
			name:     "messaging service wins over from",
			tw:       twilio{sid: "AC123", token: "tok", msgService: "MG456", from: "+15559990000", to: "+15550001111"},
			msg:      Message{Title: "Quorum", Body: "2 of 3 voted yes"},
			wantBody: "Quorum\n2 of 3 voted yes",
			wantParams: map[string]string{
				"MessagingServiceSid": "MG456",
				"From":                "",
			},
		},
		{
			name:     "bare from number when no messaging service",
			tw:       twilio{sid: "AC123", token: "tok", from: "+15559990000", to: "+15550001111"},
			msg:      Message{Title: "Done", Body: "task finished"},
			wantBody: "Done\ntask finished",
			wantParams: map[string]string{
				"From":                "+15559990000",
				"MessagingServiceSid": "",
			},
		},
		{
			name:     "empty title is not folded in",
			tw:       twilio{sid: "AC123", token: "tok", msgService: "MG456", to: "+15550001111"},
			msg:      Message{Body: "just a body"},
			wantBody: "just a body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tw := tt.tw
			captured, originalURL := newSendFixture(t, &tw, http.StatusCreated, `{"sid":"SM1"}`)

			if err := tw.Send(context.Background(), tt.msg); err != nil {
				t.Fatalf("Send error = %v", err)
			}
			if want := "https://api.twilio.com/2010-04-01/Accounts/AC123/Messages.json"; *originalURL != want {
				t.Errorf("request URL = %q, want %q", *originalURL, want)
			}
			if captured.method != http.MethodPost {
				t.Errorf("method = %q, want POST", captured.method)
			}
			if captured.contentType != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", captured.contentType)
			}
			if !captured.authOK || captured.user != tw.sid || captured.pass != tw.token {
				t.Errorf("basic auth = (%q, %q, ok=%v), want (%q, %q, ok=true)",
					captured.user, captured.pass, captured.authOK, tw.sid, tw.token)
			}
			if got := captured.form.Get("To"); got != tw.to {
				t.Errorf("To = %q, want %q", got, tw.to)
			}
			if got := captured.form.Get("Body"); got != tt.wantBody {
				t.Errorf("Body = %q, want %q", got, tt.wantBody)
			}
			for param, want := range tt.wantParams {
				got := captured.form.Get(param)
				if want == "" {
					if _, present := captured.form[param]; present {
						t.Errorf("param %s = %q, want absent", param, got)
					}
				} else if got != want {
					t.Errorf("param %s = %q, want %q", param, got, want)
				}
			}
		})
	}
}

func TestSendNon2xxIsError(t *testing.T) {
	tw := twilio{sid: "AC123", token: "tok", msgService: "MG456", to: "+15550001111"}
	newSendFixture(t, &tw, http.StatusBadRequest, `{"code": 21211,
		"message": "Invalid 'To' Phone Number"}`)

	err := tw.Send(context.Background(), Message{Body: "hi"})
	if err == nil {
		t.Fatal("Send with 400 response: want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "status 400") {
		t.Errorf("error %q missing %q", msg, "status 400")
	}
	// The body excerpt must be flattened onto one line.
	if !strings.Contains(msg, `"message": "Invalid 'To' Phone Number"`) || strings.Contains(msg, "\n") {
		t.Errorf("error %q should contain the one-line body excerpt", msg)
	}
}

func TestSendContextCanceled(t *testing.T) {
	tw := twilio{sid: "AC123", token: "tok", msgService: "MG456", to: "+15550001111"}
	newSendFixture(t, &tw, http.StatusCreated, "{}")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tw.Send(ctx, Message{Body: "hi"}); err == nil {
		t.Fatal("Send with canceled context: want error, got nil")
	}
}

// --- signature validation ------------------------------------------------

// twilioSign re-implements Twilio's documented signing scheme in the test:
// HMAC-SHA1 over the webhook URL then each POST param sorted by key
// appended as key+value, base64-encoded.
func twilioSign(authToken, webhookURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(webhookURL))
	for _, k := range keys {
		for _, v := range form[k] {
			mac.Write([]byte(k))
			mac.Write([]byte(v))
		}
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func goldenForm() url.Values {
	return url.Values{
		"From":       {"+15551234567"},
		"Body":       {"LGTM ship it"},
		"MessageSid": {"SM1234567890abcdef"},
	}
}

const (
	goldenToken = "test-auth-token"
	goldenURL   = "https://hooks.example.com/sms/inbound"
	// Precomputed HMAC-SHA1/base64 for goldenToken+goldenURL+goldenForm,
	// pinning the algorithm against regressions.
	goldenSig = "5puR+0C8Tfez/FTcMRUu2RWxm0o="
)

func TestValidTwilioSignatureGolden(t *testing.T) {
	if !validTwilioSignature(goldenToken, goldenURL, goldenForm(), goldenSig) {
		t.Error("golden signature rejected")
	}
	// Sanity check that the test's independent implementation agrees.
	if got := twilioSign(goldenToken, goldenURL, goldenForm()); got != goldenSig {
		t.Errorf("test signer produced %q, want golden %q", got, goldenSig)
	}
}

func TestValidTwilioSignatureRejects(t *testing.T) {
	tamperedForm := goldenForm()
	tamperedForm.Set("Body", "LGTM ship it!")
	extraParam := goldenForm()
	extraParam.Set("Injected", "1")
	tests := []struct {
		name  string
		token string
		url   string
		form  url.Values
		sig   string
	}{
		{"wrong token", "other-token", goldenURL, goldenForm(), goldenSig},
		{"wrong url", goldenToken, "https://evil.example/sms/inbound", goldenForm(), goldenSig},
		{"tampered param value", goldenToken, goldenURL, tamperedForm, goldenSig},
		{"extra param", goldenToken, goldenURL, extraParam, goldenSig},
		{"garbage signature", goldenToken, goldenURL, goldenForm(), "not-base64-at-all"},
		{"empty signature", goldenToken, goldenURL, goldenForm(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validTwilioSignature(tt.token, tt.url, tt.form, tt.sig) {
				t.Error("signature accepted, want rejected")
			}
		})
	}
}

// --- InboundHandler ------------------------------------------------------

// postInbound sends form to the handler as Twilio would, optionally with
// an X-Twilio-Signature header.
func postInbound(handler http.Handler, form url.Values, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/sms/inbound", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if signature != "" {
		req.Header.Set("X-Twilio-Signature", signature)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestInboundHandlerMethodNotAllowed(t *testing.T) {
	tw := &twilio{token: goldenToken, webhookURL: goldenURL}
	called := false
	h := tw.InboundHandler(func(Reply) { called = true })

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/sms/inbound", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
	if called {
		t.Error("onReply called for non-POST request")
	}
}

func TestInboundHandlerValidSignature(t *testing.T) {
	tw := &twilio{token: goldenToken, webhookURL: goldenURL}
	var got Reply
	called := false
	h := tw.InboundHandler(func(r Reply) { got = r; called = true })

	form := goldenForm()
	before := time.Now()
	rec := postInbound(h, form, twilioSign(goldenToken, goldenURL, form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/xml" {
		t.Errorf("Content-Type = %q, want text/xml", ct)
	}
	if body := rec.Body.String(); body != "<Response/>" {
		t.Errorf("body = %q, want <Response/>", body)
	}
	if !called {
		t.Fatal("onReply not called")
	}
	if got.From != "+15551234567" {
		t.Errorf("Reply.From = %q, want +15551234567", got.From)
	}
	if got.Body != "LGTM ship it" {
		t.Errorf("Reply.Body = %q, want %q", got.Body, "LGTM ship it")
	}
	if got.At.Before(before) || got.At.After(time.Now()) {
		t.Errorf("Reply.At = %v, want between %v and now", got.At, before)
	}
}

func TestInboundHandlerTamperedRejected(t *testing.T) {
	tw := &twilio{token: goldenToken, webhookURL: goldenURL}
	form := goldenForm()
	validSig := twilioSign(goldenToken, goldenURL, form)
	tampered := goldenForm()
	tampered.Set("Body", "attacker text")

	tests := []struct {
		name string
		form url.Values
		sig  string
	}{
		{"tampered body with stale signature", tampered, validSig},
		{"missing signature header", form, ""},
		{"garbage signature", form, "AAAA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := tw.InboundHandler(func(Reply) { called = true })
			rec := postInbound(h, tt.form, tt.sig)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if called {
				t.Error("onReply called for rejected request")
			}
		})
	}
}

func TestInboundHandlerDevModeAcceptsUnsigned(t *testing.T) {
	// webhookURL "" = dev mode: no signature required.
	tw := &twilio{token: goldenToken, webhookURL: ""}
	var got Reply
	called := false
	h := tw.InboundHandler(func(r Reply) { got = r; called = true })

	form := url.Values{"From": {"+15557654321"}, "Body": {"yes"}}
	rec := postInbound(h, form, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "<Response/>" {
		t.Errorf("body = %q, want <Response/>", body)
	}
	if !called {
		t.Fatal("onReply not called in dev mode")
	}
	if got.From != "+15557654321" || got.Body != "yes" {
		t.Errorf("Reply = %+v, want From=+15557654321 Body=yes", got)
	}
}

func TestInboundHandlerNilOnReply(t *testing.T) {
	tw := &twilio{token: goldenToken, webhookURL: ""}
	h := tw.InboundHandler(nil)
	rec := postInbound(h, url.Values{"From": {"+1555"}, "Body": {"ok"}}, "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (nil onReply must not panic)", rec.Code)
	}
}

func TestInboundHandlerBadForm(t *testing.T) {
	tw := &twilio{token: goldenToken, webhookURL: goldenURL}
	called := false
	h := tw.InboundHandler(func(Reply) { called = true })

	// "%zz" is an invalid percent-escape; ParseForm must fail.
	req := httptest.NewRequest(http.MethodPost, "/sms/inbound", strings.NewReader("From=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if called {
		t.Error("onReply called for malformed form")
	}
}
