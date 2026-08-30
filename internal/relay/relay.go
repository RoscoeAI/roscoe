// Package relay is the client for Roscoe's hosted SMS relay at roscoe.sh:
// device-link auth, token refresh, and the WebSocket bridge over which
// escalations go out and the operator's SMS replies come back. The $5/mo
// subscription and phone consent live on the website; this package only
// links a CLI to that account and speaks the bridge protocol.
package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const DefaultBaseURL = "https://roscoe.sh"

// ErrRelink means the refresh token was rejected; the user must run
// `roscoe upgrade` again.
var ErrRelink = errors.New("relay session expired; run \"roscoe upgrade\" to re-link")

// ErrNotLinked means no credentials file exists yet.
var ErrNotLinked = errors.New("no relay credentials; run \"roscoe upgrade\" to link this machine")

// Credentials is the persisted state in ~/.roscoe/relay.json (0600).
type Credentials struct {
	ClientID              string    `json:"client_id"`
	Phone                 string    `json:"phone,omitempty"`
	BaseURL               string    `json:"base_url"`
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

// CredsPath honors ROSCOE_RELAY_STATE, defaulting to ~/.roscoe/relay.json.
func CredsPath() string {
	if p := os.Getenv("ROSCOE_RELAY_STATE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roscoe", "relay.json")
}

func LoadCreds() (*Credentials, error) {
	b, err := os.ReadFile(CredsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLinked
	}
	if err != nil {
		return nil, fmt.Errorf("relay: read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("relay: parse %s: %w", CredsPath(), err)
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	return &c, nil
}

func (c *Credentials) Save() error {
	path := CredsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("relay: create state dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("relay: encode credentials: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("relay: write credentials: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("relay: save credentials: %w", err)
	}
	return nil
}

// NewClientID generates the persistent per-machine identity (uuid v4).
func NewClientID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("relay: crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// StartResult is the server's device-link start response.
type StartResult struct {
	DeviceCode              string `json:"deviceCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete"`
	ExpiresAt               string `json:"expiresAt"`
	PollIntervalSeconds     int    `json:"pollIntervalSeconds"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func postJSON(ctx context.Context, url string, body any, out any) (int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("relay: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return 0, fmt.Errorf("relay: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("relay: %s: %w", url, err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if out != nil {
		if err := dec.Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("relay: decode response from %s (status %d): %w", url, resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

type apiError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// StartLink begins the device flow. phone may be empty (the site collects it).
func StartLink(ctx context.Context, baseURL, clientID, phone string) (*StartResult, error) {
	var out struct {
		apiError
		StartResult
	}
	status, err := postJSON(ctx, baseURL+"/api/auth/device/start",
		map[string]string{"clientId": clientID, "phone": phone}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || !out.OK {
		return nil, fmt.Errorf("relay: device start failed (status %d): %s", status, out.Error)
	}
	return &out.StartResult, nil
}

type sessionResponse struct {
	apiError
	Status                string `json:"status"`
	AccessToken           string `json:"accessToken"`
	AccessTokenExpiresAt  string `json:"accessTokenExpiresAt"`
	RefreshToken          string `json:"refreshToken"`
	RefreshTokenExpiresAt string `json:"refreshTokenExpiresAt"`
}

func parseISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *sessionResponse) credentials(baseURL, clientID, phone string) *Credentials {
	return &Credentials{
		ClientID:              clientID,
		Phone:                 phone,
		BaseURL:               baseURL,
		AccessToken:           s.AccessToken,
		AccessTokenExpiresAt:  parseISO(s.AccessTokenExpiresAt),
		RefreshToken:          s.RefreshToken,
		RefreshTokenExpiresAt: parseISO(s.RefreshTokenExpiresAt),
	}
}

// PollLink polls until the browser side approves (status "linked"), the
// device code expires, or ctx is canceled. intervalSeconds comes from the
// start response (0 → 3s). onTick, if non-nil, is called once per poll.
func PollLink(ctx context.Context, baseURL, clientID, deviceCode, phone string, intervalSeconds int, onTick func()) (*Credentials, error) {
	if intervalSeconds <= 0 {
		intervalSeconds = 3
	}
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		var out sessionResponse
		status, err := postJSON(ctx, baseURL+"/api/auth/device/poll",
			map[string]string{"deviceCode": deviceCode, "clientId": clientID}, &out)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusOK && out.Status == "linked":
			return out.credentials(baseURL, clientID, phone), nil
		case status == http.StatusAccepted: // pending
		default:
			return nil, fmt.Errorf("relay: device link failed (status %d): %s", status, out.Error)
		}
		if onTick != nil {
			onTick()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// EnsureFresh refreshes the access token when it is within two minutes of
// expiry, persisting the rotated session. Returns ErrRelink on a rejected
// refresh token.
func (c *Credentials) EnsureFresh(ctx context.Context) error {
	if time.Until(c.AccessTokenExpiresAt) > 2*time.Minute {
		return nil
	}
	var out sessionResponse
	status, err := postJSON(ctx, c.BaseURL+"/api/auth/refresh",
		map[string]string{"refreshToken": c.RefreshToken, "clientId": c.ClientID}, &out)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		return ErrRelink
	}
	if status != http.StatusOK || !out.OK {
		return fmt.Errorf("relay: refresh failed (status %d): %s", status, out.Error)
	}
	fresh := out.credentials(c.BaseURL, c.ClientID, c.Phone)
	*c = *fresh
	return c.Save()
}

// BillingStatus is the subscriber state for a phone.
type BillingStatus struct {
	Phone              string `json:"phone"`
	SubscriptionStatus string `json:"subscriptionStatus"`
	Active             bool   `json:"active"`
	RoundTripVerified  bool   `json:"roundTripVerified"`
}

func GetBillingStatus(ctx context.Context, baseURL, phone, clientID string) (*BillingStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/api/relay/billing/status?phone="+urlQueryEscape(phone)+"&clientId="+urlQueryEscape(clientID), nil)
	if err != nil {
		return nil, fmt.Errorf("relay: build status request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("relay: billing status: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		apiError
		Status BillingStatus `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("relay: decode billing status: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("relay: billing status: %s", out.Error)
	}
	return &out.Status, nil
}
