package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setState points ROSCOE_RELAY_STATE at a fresh temp file and returns the path.
func setState(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relay.json")
	t.Setenv("ROSCOE_RELAY_STATE", p)
	return p
}

func TestSaveLoadRoundTrip(t *testing.T) {
	// Use a nested path to prove Save creates the parent directory.
	p := filepath.Join(t.TempDir(), "state", "relay.json")
	t.Setenv("ROSCOE_RELAY_STATE", p)

	want := &Credentials{
		ClientID:              "cid-1234",
		Phone:                 "+15550100",
		BaseURL:               "https://relay.example",
		AccessToken:           "at-1",
		AccessTokenExpiresAt:  time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		RefreshToken:          "rt-1",
		RefreshTokenExpiresAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file mode = %o, want 600", perm)
	}
	if _, err := os.Stat(p + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind after Save: stat err = %v", err)
	}

	got, err := LoadCreds()
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if got.ClientID != want.ClientID || got.Phone != want.Phone || got.BaseURL != want.BaseURL ||
		got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round trip mismatch: got %+v want %+v", got, want)
	}
	if !got.AccessTokenExpiresAt.Equal(want.AccessTokenExpiresAt) {
		t.Errorf("AccessTokenExpiresAt = %v, want %v", got.AccessTokenExpiresAt, want.AccessTokenExpiresAt)
	}
	if !got.RefreshTokenExpiresAt.Equal(want.RefreshTokenExpiresAt) {
		t.Errorf("RefreshTokenExpiresAt = %v, want %v", got.RefreshTokenExpiresAt, want.RefreshTokenExpiresAt)
	}
}

func TestLoadCredsNotLinked(t *testing.T) {
	setState(t) // file never written
	_, err := LoadCreds()
	if !errors.Is(err, ErrNotLinked) {
		t.Fatalf("LoadCreds with no file: err = %v, want ErrNotLinked", err)
	}
}

func TestLoadCredsFillsDefaultBaseURL(t *testing.T) {
	p := setState(t)
	if err := os.WriteFile(p, []byte(`{"client_id":"cid","access_token":"at"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCreds()
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want default %q", c.BaseURL, DefaultBaseURL)
	}
}

func TestLoadCredsBadJSON(t *testing.T) {
	p := setState(t)
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCreds()
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("LoadCreds with bad JSON: err = %v, want parse error", err)
	}
	if errors.Is(err, ErrNotLinked) {
		t.Error("bad JSON must not be reported as ErrNotLinked")
	}
}

func TestNewClientID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewClientID()
		if seen[id] {
			t.Fatalf("duplicate client id generated: %s", id)
		}
		seen[id] = true

		parts := strings.Split(id, "-")
		if len(parts) != 5 {
			t.Fatalf("id %q: want 5 dash groups, got %d", id, len(parts))
		}
		for gi, wantLen := range []int{8, 4, 4, 4, 12} {
			if len(parts[gi]) != wantLen {
				t.Fatalf("id %q: group %d has len %d, want %d", id, gi, len(parts[gi]), wantLen)
			}
		}
		if id != strings.ToLower(id) {
			t.Errorf("id %q: want lowercase hex", id)
		}
		// Version nibble: first char of third group must be '4'.
		if parts[2][0] != '4' {
			t.Errorf("id %q: version nibble = %c, want 4", id, parts[2][0])
		}
		// Variant: first char of fourth group must be 8, 9, a, or b.
		if v := parts[3][0]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("id %q: variant nibble = %c, want one of 89ab", id, v)
		}
	}
}

func TestStartLink(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/device/start" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"deviceCode":"dev-1","verificationUrl":"https://roscoe.sh/link",` +
			`"verificationUrlComplete":"https://roscoe.sh/link?code=dev-1",` +
			`"expiresAt":"2026-08-30T13:00:00Z","pollIntervalSeconds":5}`))
	}))
	defer srv.Close()

	res, err := StartLink(context.Background(), srv.URL, "cid-1", "+15550100")
	if err != nil {
		t.Fatalf("StartLink: %v", err)
	}
	if gotBody["clientId"] != "cid-1" || gotBody["phone"] != "+15550100" {
		t.Errorf("request body = %v, want clientId/phone", gotBody)
	}
	if res.DeviceCode != "dev-1" {
		t.Errorf("DeviceCode = %q, want dev-1", res.DeviceCode)
	}
	if res.VerificationURLComplete != "https://roscoe.sh/link?code=dev-1" {
		t.Errorf("VerificationURLComplete = %q", res.VerificationURLComplete)
	}
	if res.PollIntervalSeconds != 5 {
		t.Errorf("PollIntervalSeconds = %d, want 5", res.PollIntervalSeconds)
	}
}

func TestStartLinkServerRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error":"phone required"}`))
	}))
	defer srv.Close()

	_, err := StartLink(context.Background(), srv.URL, "cid-1", "")
	if err == nil {
		t.Fatal("StartLink: want error, got nil")
	}
	if !strings.Contains(err.Error(), "phone required") || !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q should carry status and server message", err)
	}
}

func TestPollLinkPendingThenLinked(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["deviceCode"] != "dev-1" || body["clientId"] != "cid-1" {
			t.Errorf("poll body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"ok":true,"status":"pending"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"status":"linked",` +
			`"accessToken":"at-new","accessTokenExpiresAt":"2026-08-30T13:00:00Z",` +
			`"refreshToken":"rt-new","refreshTokenExpiresAt":"2026-09-29T12:00:00Z"}`))
	}))
	defer srv.Close()

	var ticks atomic.Int32
	start := time.Now()
	creds, err := PollLink(context.Background(), srv.URL, "cid-1", "dev-1", "+15550100", 1,
		func() { ticks.Add(1) })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PollLink: %v", err)
	}
	// Two pending responses at a 1s interval: the first poll is immediate, so
	// total time is ~2s. Loose bounds to avoid flakes.
	if elapsed < 1500*time.Millisecond {
		t.Errorf("PollLink returned after %v; expected it to honor the 1s interval (~2s)", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("PollLink took %v; interval not respected", elapsed)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d polls, want 3", got)
	}
	if got := ticks.Load(); got != 2 {
		t.Errorf("onTick fired %d times, want 2 (once per pending poll)", got)
	}
	if creds.ClientID != "cid-1" || creds.Phone != "+15550100" || creds.BaseURL != srv.URL {
		t.Errorf("identity fields wrong: %+v", creds)
	}
	if creds.AccessToken != "at-new" || creds.RefreshToken != "rt-new" {
		t.Errorf("tokens wrong: %+v", creds)
	}
	wantAT := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	if !creds.AccessTokenExpiresAt.Equal(wantAT) {
		t.Errorf("AccessTokenExpiresAt = %v, want %v", creds.AccessTokenExpiresAt, wantAT)
	}
	wantRT := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if !creds.RefreshTokenExpiresAt.Equal(wantRT) {
		t.Errorf("RefreshTokenExpiresAt = %v, want %v", creds.RefreshTokenExpiresAt, wantRT)
	}
}

func TestPollLinkRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error":"device code expired"}`))
	}))
	defer srv.Close()

	_, err := PollLink(context.Background(), srv.URL, "cid-1", "dev-1", "", 1, nil)
	if err == nil {
		t.Fatal("PollLink on 400: want error, got nil")
	}
	if !strings.Contains(err.Error(), "device code expired") || !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q should carry status and server message", err)
	}
}

func TestEnsureFreshNoopWhenTokenFresh(t *testing.T) {
	setState(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Credentials{
		BaseURL:              srv.URL,
		AccessToken:          "still-good",
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
		RefreshToken:         "rt",
	}
	if err := c.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("EnsureFresh hit the server %d times for a fresh token; want 0", hits.Load())
	}
	if c.AccessToken != "still-good" {
		t.Errorf("token changed on no-op: %q", c.AccessToken)
	}
}

func TestEnsureFreshRefreshesAndPersists(t *testing.T) {
	statePath := setState(t)
	newAT := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	newRT := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"ok":                    true,
			"accessToken":           "at-rotated",
			"accessTokenExpiresAt":  newAT.Format(time.RFC3339),
			"refreshToken":          "rt-rotated",
			"refreshTokenExpiresAt": newRT.Format(time.RFC3339),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Credentials{
		ClientID:             "cid-1",
		Phone:                "+15550100",
		BaseURL:              srv.URL,
		AccessToken:          "at-old",
		AccessTokenExpiresAt: time.Now().Add(30 * time.Second), // inside the 2-minute window
		RefreshToken:         "rt-old",
	}
	if err := c.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if gotBody["refreshToken"] != "rt-old" || gotBody["clientId"] != "cid-1" {
		t.Errorf("refresh request body = %v", gotBody)
	}
	if c.AccessToken != "at-rotated" || c.RefreshToken != "rt-rotated" {
		t.Errorf("in-memory creds not rotated: %+v", c)
	}
	if !c.AccessTokenExpiresAt.Equal(newAT) {
		t.Errorf("AccessTokenExpiresAt = %v, want %v", c.AccessTokenExpiresAt, newAT)
	}
	if c.ClientID != "cid-1" || c.Phone != "+15550100" || c.BaseURL != srv.URL {
		t.Errorf("identity fields lost across refresh: %+v", c)
	}

	// Rotated session must be persisted to disk.
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var onDisk Credentials
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("state file not valid JSON: %v", err)
	}
	if onDisk.AccessToken != "at-rotated" || onDisk.RefreshToken != "rt-rotated" {
		t.Errorf("persisted creds = %+v, want rotated tokens", onDisk)
	}
}

func TestEnsureFreshRelinkOn401(t *testing.T) {
	setState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error":"refresh token revoked"}`))
	}))
	defer srv.Close()

	c := &Credentials{
		BaseURL:              srv.URL,
		AccessToken:          "at-old",
		AccessTokenExpiresAt: time.Now().Add(-time.Minute), // expired
		RefreshToken:         "rt-old",
	}
	err := c.EnsureFresh(context.Background())
	if !errors.Is(err, ErrRelink) {
		t.Fatalf("EnsureFresh on 401: err = %v, want ErrRelink", err)
	}
	if c.AccessToken != "at-old" {
		t.Errorf("creds mutated on failed refresh: %+v", c)
	}
}

func TestEnsureFreshServerError(t *testing.T) {
	setState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"ok":false,"error":"database on fire"}`))
	}))
	defer srv.Close()

	c := &Credentials{
		BaseURL:              srv.URL,
		AccessTokenExpiresAt: time.Now(), // needs refresh
		RefreshToken:         "rt",
	}
	err := c.EnsureFresh(context.Background())
	if err == nil {
		t.Fatal("EnsureFresh on 500: want error")
	}
	if errors.Is(err, ErrRelink) {
		t.Error("a 500 must not demand a re-link")
	}
	if !strings.Contains(err.Error(), "database on fire") {
		t.Errorf("error %q should carry server message", err)
	}
}

func TestGetBillingStatus(t *testing.T) {
	var gotPhone, gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/relay/billing/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotPhone = r.URL.Query().Get("phone")
		gotClient = r.URL.Query().Get("clientId")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"status":{"phone":"+15550100","subscriptionStatus":"active",` +
			`"active":true,"roundTripVerified":true}}`))
	}))
	defer srv.Close()

	st, err := GetBillingStatus(context.Background(), srv.URL, "+15550100", "cid-1")
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	// "+15550100" must survive query escaping (a raw '+' would decode as a space).
	if gotPhone != "+15550100" {
		t.Errorf("server saw phone %q, want +15550100 (escaping broken)", gotPhone)
	}
	if gotClient != "cid-1" {
		t.Errorf("server saw clientId %q", gotClient)
	}
	if !st.Active || st.SubscriptionStatus != "active" || st.Phone != "+15550100" || !st.RoundTripVerified {
		t.Errorf("parsed status = %+v", st)
	}
}

func TestGetBillingStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error":"unknown phone"}`))
	}))
	defer srv.Close()

	_, err := GetBillingStatus(context.Background(), srv.URL, "+15550100", "cid-1")
	if err == nil || !strings.Contains(err.Error(), "unknown phone") {
		t.Fatalf("err = %v, want server message surfaced", err)
	}
}

func TestParseISO(t *testing.T) {
	got := parseISO("2026-08-30T12:34:56Z")
	want := time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseISO = %v, want %v", got, want)
	}
	if z := parseISO("not-a-time"); !z.IsZero() {
		t.Errorf("parseISO on garbage = %v, want zero time", z)
	}
	if z := parseISO(""); !z.IsZero() {
		t.Errorf("parseISO on empty = %v, want zero time", z)
	}
}
