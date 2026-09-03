package main

import (
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/accounts"
)

func accountRows() []accounts.Row {
	return []accounts.Row{
		{Name: "primary", Kind: "claude-subscription", Ref: "keychain:roscoe-account-primary", Enabled: true, Present: "no", UsedBy: []string{"tier 1", "tier 2"}},
		{Name: "secondary", Kind: "claude-subscription", Ref: "keychain:roscoe-account-secondary", Enabled: true, Present: "yes", Age: "3 months", UsedBy: []string{"tier 2"}},
		{Name: "api-fallback", Kind: "anthropic-api-key", Ref: "env:ANTHROPIC_API_KEY", Enabled: false, Present: "no"},
	}
}

// The table must say, for every account, whether it is there and who uses
// it, without a token anywhere near it.
func TestAccountsTable(t *testing.T) {
	out := accountsTable(accountRows())
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines:\n%s", out)
	}
	if !strings.Contains(lines[1], "tier 1, tier 2") || !strings.Contains(lines[1], " no ") {
		t.Errorf("primary row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "yes") || !strings.Contains(lines[2], "3 months") {
		t.Errorf("secondary row = %q", lines[2])
	}
	if !strings.Contains(lines[3], "(off)") || !strings.Contains(lines[3], "env:ANTHROPIC_API_KEY") {
		t.Errorf("disabled row = %q", lines[3])
	}
}

// One next step, in priority: renew an old token, store a missing one that a
// tier uses, fill an env var, else nothing to do.
func TestAccountsHintPriority(t *testing.T) {
	rows := accountRows()
	if got := accountsHint(rows); !strings.Contains(got, "roscoe accounts set primary") {
		t.Errorf("missing keychain token hint = %q", got)
	}
	rows[1].Age = "11 months; expires at 12, renew soon"
	if got := accountsHint(rows); !strings.Contains(got, "renew secondary") {
		t.Errorf("renewal should outrank a missing token: %q", got)
	}
	all := accountRows()
	all[0].Present = "yes"
	if got := accountsHint(all); !strings.Contains(got, "first present account") {
		t.Errorf("all-present hint = %q", got)
	}
	// A disabled account never produces a hint; an unused one neither.
	only := []accounts.Row{{Name: "spare", Ref: "keychain:x", Enabled: true, Present: "no"}}
	if got := accountsHint(only); strings.Contains(got, "spare") {
		t.Errorf("an unused account asked for a token: %q", got)
	}
}

// Off macOS, or over ssh, the keychain answers "locked" rather than yes or
// no. The hint must then point at an env: ref, and must never claim a
// present account when there is none.
func TestAccountsHintWhenKeychainUnavailable(t *testing.T) {
	rows := []accounts.Row{
		{Name: "primary", Ref: "keychain:roscoe-account-primary", Enabled: true, Present: "no keychain on this platform; use an env: ref", UsedBy: []string{"tier 2"}},
		{Name: "api-fallback", Ref: "env:ANTHROPIC_API_KEY", Enabled: false, Present: "no"},
	}
	got := accountsHint(rows)
	if !strings.Contains(got, "env:NAME") || !strings.Contains(got, "primary") {
		t.Errorf("hint = %q, want it to steer primary to an env: ref", got)
	}
	if strings.Contains(got, "first present") {
		t.Errorf("hint claims a present account when none is: %q", got)
	}
	none := []accounts.Row{{Name: "spare", Ref: "keychain:x", Enabled: true, Present: "no"}}
	if got := accountsHint(none); !strings.Contains(got, "claude's own login") {
		t.Errorf("with nothing present and nothing used, hint = %q", got)
	}
}
