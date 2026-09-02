package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/accounts"
	"roscoe.sh/roscoe/internal/config"
)

// cmdAccounts shows which Claude credentials the fleet may use and whether
// each is actually there; `set <name>` stores one.
func cmdAccounts(_ context.Context, explicit string, args []string) int {
	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe accounts: %v\n", err)
		return 1
	}
	if len(args) > 0 && args[0] == "set" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: roscoe accounts set <name>    (stores the token for a keychain: account; you will be prompted)")
			return 2
		}
		return setAccount(cfg.Accounts, args[1])
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "roscoe accounts: unknown subcommand %q (only: set <name>)\n", args[0])
		return 2
	}
	rows := accounts.Status(cfg, env, os.Getenv, accounts.MacKeychain{}, time.Now())
	if len(rows) == 0 {
		fmt.Println("no accounts configured; add one under accounts[] in roscoe.json")
		return 0
	}
	fmt.Print(accountsTable(rows))
	fmt.Println()
	fmt.Println(accountsHint(rows))
	return 0
}

func accountsTable(rows []accounts.Row) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-20s %-20s %-36s %-9s %-14s %s\n", "account", "kind", "token", "present", "used by", "age")
	for _, r := range rows {
		name := r.Name
		if !r.Enabled {
			name += " (off)"
		}
		used := strings.Join(r.UsedBy, ", ")
		if used == "" {
			used = "-"
		}
		present := oneLineOf(r.Present, 40)
		line := fmt.Sprintf("  %-20s %-20s %-36s %-9s %-14s %s", name, r.Kind, r.Ref, present, used, r.Age)
		// Colour after padding, so escape codes never shift a column.
		switch {
		case !r.Enabled:
			line = ansiFaint + line + ansiReset
		case r.Present == "yes":
			line = strings.Replace(line, " yes ", " "+ansiGreen+"yes"+ansiReset+" ", 1)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}

// accountsHint is the one next step: store a missing token, renew an old one,
// or nothing.
func accountsHint(rows []accounts.Row) string {
	for _, r := range rows {
		if r.Enabled && strings.Contains(r.Age, "renew") {
			return fmt.Sprintf("renew %s:  claude setup-token, then roscoe accounts set %s", r.Name, r.Name)
		}
	}
	for _, r := range rows {
		if r.Enabled && r.Present == "no" && strings.HasPrefix(r.Ref, "keychain:") && len(r.UsedBy) > 0 {
			return fmt.Sprintf("store %s's token:  claude setup-token, then roscoe accounts set %s   (without one, workers use claude's own login)", r.Name, r.Name)
		}
	}
	for _, r := range rows {
		if r.Enabled && r.Present == "no" && strings.HasPrefix(r.Ref, "env:") && len(r.UsedBy) > 0 {
			return fmt.Sprintf("%s needs %s in the env file", r.Name, strings.TrimPrefix(r.Ref, "env:"))
		}
	}
	return "workers run under the first present account in tiers.middle.accounts"
}

// setAccount stores a token for a keychain: account. The security tool asks
// for the value itself, with echo off, so it never passes through roscoe.
func setAccount(all []config.Account, name string) int {
	for _, a := range all {
		if a.Name != name {
			continue
		}
		ref, err := accounts.ParseRef(a.TokenRef)
		if err != nil {
			fmt.Fprintf(os.Stderr, "roscoe accounts: %v\n", err)
			return 1
		}
		if ref.Scheme != "keychain" {
			fmt.Fprintf(os.Stderr, "roscoe accounts: %s is %s; put the value in the env file instead\n", name, a.TokenRef)
			return 2
		}
		if !isTTY(os.Stdin) {
			fmt.Fprintln(os.Stderr, "roscoe accounts set: needs a terminal (the keychain prompts for the token)")
			return 2
		}
		fmt.Fprintf(os.Stderr, "Paste the token for %s when asked (claude setup-token mints one); it is not echoed.\n", name)
		if err := (accounts.MacKeychain{}).Set(ref.Name); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe accounts: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "stored under keychain:%s. Record when it was minted:  roscoe config set accounts.%s.minted_at %s\n",
			ref.Name, name, time.Now().Format("2006-01-02"))
		return 0
	}
	fmt.Fprintf(os.Stderr, "roscoe accounts: no account named %q; roscoe accounts lists them\n", name)
	return 2
}
