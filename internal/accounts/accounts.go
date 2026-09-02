// Package accounts is the credential side of the fleet: which Claude
// accounts roscoe may run workers under, where each token lives, and which
// one a tier actually gets. Tokens are read into memory for the worker's
// environment and nowhere else; presence checks never read them at all.
package accounts

import (
	"fmt"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Ref is a parsed token_ref: "keychain:<service>" or "env:<VAR>".
type Ref struct {
	Scheme string // "keychain" or "env"
	Name   string // the service or the variable
}

func (r Ref) String() string { return r.Scheme + ":" + r.Name }

// ParseRef splits a token_ref. An unknown scheme is an error, because a typo
// here would otherwise read as "no token" forever.
func ParseRef(s string) (Ref, error) {
	scheme, name, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || name == "" {
		return Ref{}, fmt.Errorf("token_ref %q: want keychain:<service> or env:<VAR>", s)
	}
	switch scheme {
	case "keychain", "env":
		return Ref{Scheme: scheme, Name: name}, nil
	}
	return Ref{}, fmt.Errorf("token_ref %q: unknown scheme %q (keychain or env)", s, scheme)
}

// Keychain is the operator's credential store. The real one is the macOS
// keychain via the security tool; tests inject a map.
type Keychain interface {
	// Has reports whether an item exists, without reading it.
	Has(service string) (bool, error)
	// Get reads the token. Callers hand it to a worker's environment only.
	Get(service string) (string, error)
}

// Attempt is one account tried by Resolve and why it did not yield a token.
type Attempt struct {
	Account string
	Why     string
}

// Resolve returns the first account in wanted that yields a token, and what
// happened to each one before it (or to all of them when none did). env is
// the loaded env file; getenv covers the process environment behind it.
func Resolve(cfg *config.Config, wanted []string, env map[string]string, getenv func(string) string, kc Keychain) (name, token string, tried []Attempt) {
	for _, want := range wanted {
		a, ok := find(cfg, want)
		if !ok {
			tried = append(tried, Attempt{want, "not in accounts[]"})
			continue
		}
		if a.Enabled != nil && !*a.Enabled {
			tried = append(tried, Attempt{want, "disabled"})
			continue
		}
		ref, err := ParseRef(a.TokenRef)
		if err != nil {
			tried = append(tried, Attempt{want, err.Error()})
			continue
		}
		switch ref.Scheme {
		case "env":
			if t := env[ref.Name]; t != "" {
				return a.Name, t, tried
			}
			if getenv != nil {
				if t := getenv(ref.Name); t != "" {
					return a.Name, t, tried
				}
			}
			tried = append(tried, Attempt{want, ref.Name + " is unset"})
		case "keychain":
			if kc == nil {
				tried = append(tried, Attempt{want, "no keychain here"})
				continue
			}
			t, err := kc.Get(ref.Name)
			if err != nil {
				tried = append(tried, Attempt{want, "keychain " + ref.Name + ": " + err.Error()})
				continue
			}
			if t == "" {
				tried = append(tried, Attempt{want, "keychain " + ref.Name + " is empty"})
				continue
			}
			return a.Name, t, tried
		}
	}
	return "", "", tried
}

func find(cfg *config.Config, name string) (config.Account, bool) {
	for _, a := range cfg.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return config.Account{}, false
}

// Describe is the one line a command prints about the account it resolved,
// or did not. It names every reason so "relying on claude's own auth" is
// never a mystery.
func Describe(name string, tried []Attempt) string {
	if name != "" {
		return "[account] " + name
	}
	if len(tried) == 0 {
		return "[account] no accounts listed for this tier; using claude's own login"
	}
	parts := make([]string, 0, len(tried))
	for _, t := range tried {
		parts = append(parts, t.Account+": "+t.Why)
	}
	return "[account] none resolved (" + strings.Join(parts, "; ") + "); using claude's own login. roscoe accounts explains"
}

// Row is one account as the accounts table shows it.
type Row struct {
	Name    string
	Kind    string
	Ref     string
	Enabled bool
	// Present is "yes", "no", or the error the store gave (a locked keychain
	// over ssh, say). Never the token.
	Present string
	// Age is how old a minted token is, with the renewal warning when due.
	Age string
	// UsedBy lists the tiers that name this account.
	UsedBy []string
}

// Status describes every configured account without reading a token.
func Status(cfg *config.Config, env map[string]string, getenv func(string) string, kc Keychain, now time.Time) []Row {
	rows := make([]Row, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		r := Row{Name: a.Name, Kind: a.Kind, Ref: a.TokenRef, Enabled: a.Enabled == nil || *a.Enabled}
		ref, err := ParseRef(a.TokenRef)
		switch {
		case err != nil:
			r.Present = err.Error()
		case ref.Scheme == "env":
			if env[ref.Name] != "" || (getenv != nil && getenv(ref.Name) != "") {
				r.Present = "yes"
			} else {
				r.Present = "no"
			}
		case kc == nil:
			r.Present = "no keychain here"
		default:
			has, err := kc.Has(ref.Name)
			switch {
			case err != nil:
				r.Present = err.Error()
			case has:
				r.Present = "yes"
			default:
				r.Present = "no"
			}
		}
		r.Age = MintedAge(a.MintedAt, now)
		if cfg.Tiers.Main.Account == a.Name {
			r.UsedBy = append(r.UsedBy, "tier 1")
		}
		for _, w := range cfg.Tiers.Middle.Accounts {
			if w == a.Name {
				r.UsedBy = append(r.UsedBy, "tier 2")
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// MintedAge renders a minted_at date's age. Setup tokens expire at twelve
// months without a word, so from eleven the line says to renew.
func MintedAge(minted string, now time.Time) string {
	if minted == "" {
		return ""
	}
	t, err := time.Parse("2006-01-02", minted)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, minted); err != nil {
			return "minted_at " + minted + " is not a date"
		}
	}
	months := int(now.Sub(t).Hours() / 24 / 30.4)
	switch {
	case months >= 12:
		return fmt.Sprintf("%d months; expired at 12, renew: roscoe accounts set", months)
	case months >= 11:
		return fmt.Sprintf("%d months; expires at 12, renew soon", months)
	case months <= 0:
		return "new"
	}
	return fmt.Sprintf("%d months", months)
}
