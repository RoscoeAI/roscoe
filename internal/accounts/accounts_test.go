package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

type fakeKC struct {
	items  map[string]string
	locked bool
	gets   int
}

func (f *fakeKC) Has(s string) (bool, error) {
	if f.locked {
		return false, ErrLocked
	}
	_, ok := f.items[s]
	return ok, nil
}
func (f *fakeKC) Get(s string) (string, error) {
	f.gets++
	if f.locked {
		return "", ErrLocked
	}
	t, ok := f.items[s]
	if !ok {
		return "", errors.New("not found")
	}
	return t, nil
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]string{"keychain:roscoe-account-primary": "keychain", "env:ANTHROPIC_API_KEY": "env"} {
		r, err := ParseRef(in)
		if err != nil || r.Scheme != want || r.String() != in {
			t.Errorf("ParseRef(%q) = %+v, %v", in, r, err)
		}
	}
	for _, bad := range []string{"", "keychain:", "vault:x", "ANTHROPIC_API_KEY"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

// The default config's accounts are keychain refs. Before this package the
// resolver only read env: refs, so the defaults never resolved and every run
// said "relying on claude's own auth" without saying why.
func TestResolveReadsTheKeychainAndSaysWhyNot(t *testing.T) {
	cfg := config.Default()
	kc := &fakeKC{items: map[string]string{"roscoe-account-secondary": "sk-ant-oat01-test"}}
	name, tok, tried := Resolve(cfg, cfg.Tiers.Middle.Accounts, nil, nil, kc)
	if name != "secondary" || tok != "sk-ant-oat01-test" {
		t.Fatalf("resolved %q %q", name, tok)
	}
	if len(tried) != 1 || tried[0].Account != "primary" || !strings.Contains(tried[0].Why, "not found") {
		t.Errorf("tried = %+v, want primary explained", tried)
	}

	// Nothing anywhere: every account gets a reason, and Describe shows them.
	empty := &fakeKC{items: map[string]string{}}
	name, _, tried = Resolve(cfg, cfg.Tiers.Middle.Accounts, nil, nil, empty)
	if name != "" || len(tried) != 2 {
		t.Fatalf("resolved %q, tried %+v", name, tried)
	}
	d := Describe(name, tried)
	for _, want := range []string{"primary: keychain roscoe-account-primary: not found", "secondary:", "roscoe accounts explains"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe lacks %q: %s", want, d)
		}
	}
	if Describe("primary", nil) != "[account] primary" {
		t.Error("a resolved account should be named plainly")
	}
	if !strings.Contains(Describe("", nil), "no accounts listed") {
		t.Error("an empty wanted list should say so")
	}
}

// Over ssh the keychain is locked; the reason must say so and point at env.
func TestResolveLockedKeychainAndEnvFallback(t *testing.T) {
	cfg := config.Default()
	on := true
	cfg.Accounts[2].Enabled = &on // api-fallback: env:ANTHROPIC_API_KEY
	wanted := []string{"primary", "api-fallback"}
	name, tok, tried := Resolve(cfg, wanted, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api-test"}, nil, &fakeKC{locked: true})
	if name != "api-fallback" || tok != "sk-ant-api-test" {
		t.Fatalf("resolved %q", name)
	}
	// ErrLocked's text differs per platform (CI is Linux); the reason must
	// carry it whole, and every variant points at env: refs.
	if len(tried) != 1 || !strings.Contains(tried[0].Why, ErrLocked.Error()) || !strings.Contains(tried[0].Why, "env:") {
		t.Errorf("locked keychain reason = %+v", tried)
	}
	// Process env is the fallback behind the env file.
	name, _, _ = Resolve(cfg, []string{"api-fallback"}, nil, func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return "from-process"
		}
		return ""
	}, nil)
	if name != "api-fallback" {
		t.Error("process env not consulted")
	}
	// Disabled and unknown accounts are named, not skipped silently.
	off := false
	cfg.Accounts[2].Enabled = &off
	_, _, tried = Resolve(cfg, []string{"api-fallback", "ghost"}, nil, nil, nil)
	if len(tried) != 2 || tried[0].Why != "disabled" || tried[1].Why != "not in accounts[]" {
		t.Errorf("tried = %+v", tried)
	}
}

// Status must never read a token: presence comes from Has.
func TestStatusNeverReadsTokens(t *testing.T) {
	cfg := config.Default()
	kc := &fakeKC{items: map[string]string{"roscoe-account-primary": "secret"}}
	cfg.Accounts[0].MintedAt = "2025-09-15"
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	rows := Status(cfg, nil, nil, kc, now)
	if kc.gets != 0 {
		t.Fatal("Status read a token")
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	p := rows[0]
	if p.Name != "primary" || p.Present != "yes" || !p.Enabled || strings.Join(p.UsedBy, ",") != "tier 1,tier 2" {
		t.Errorf("primary = %+v", p)
	}
	if !strings.HasPrefix(p.Age, "11 months") || !strings.Contains(p.Age, "renew") {
		t.Errorf("age at 11.5 months = %q", p.Age)
	}
	if rows[1].Present != "no" || rows[2].Present != "no" || rows[2].Enabled {
		t.Errorf("rows = %+v", rows[1:])
	}
	for _, r := range rows {
		if strings.Contains(r.Present, "secret") || strings.Contains(r.Age, "secret") {
			t.Fatal("a token leaked into the status")
		}
	}
	// A locked keychain is reported as such, per row, not as a crash.
	rows = Status(cfg, nil, nil, &fakeKC{locked: true}, now)
	if rows[0].Present != ErrLocked.Error() {
		t.Errorf("locked present = %q, want %q", rows[0].Present, ErrLocked.Error())
	}
}

func TestMintedAge(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"":           "",
		"2026-09-01": "new",
		"2026-03-01": "6 months",
		"2025-09-20": "11 months; expires at 12, renew soon",
		"2025-06-01": "15 months; expired at 12, renew: roscoe accounts set",
		"yesterday":  "minted_at yesterday is not a date",
	}
	for in, want := range cases {
		if got := MintedAge(in, now); got != want {
			t.Errorf("MintedAge(%q) = %q, want %q", in, got, want)
		}
	}
}
