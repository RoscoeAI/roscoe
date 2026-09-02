package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/models"
)

// cmdModels answers "opus what?". An alias is convenient to type and useless
// to read back, so roscoe records what each one actually resolved to.
func cmdModels(ctx context.Context, explicit string, args []string) int {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	refresh := fs.Bool("refresh", false, "ask each provider for its published model list")
	_ = fs.Parse(args)

	cfg, env, _, err := loadConfigAndEnv(explicit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "roscoe models: %v\n", err)
		return 1
	}
	cat := models.Open(cfg.StateDir)

	if *refresh {
		for _, name := range sortedProviders(cfg) {
			p := cfg.Providers[name]
			auth := providerAuth(p, env)
			n, err := cat.Refresh(ctx, name, p, auth)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %-12s %v\n", name, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "  %-12s %d models\n", name, n)
		}
		// Providers roscoe cannot query still get answered: every past run
		// recorded what its alias resolved to.
		if n := cat.LearnFromRuns(cfg.StateDir+"/runs", cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model); n > 0 {
			fmt.Fprintf(os.Stderr, "  %-12s learned %q from past runs\n",
				cfg.Tiers.Middle.Provider, cat.Resolve(cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model))
		}
		// Tier 1 never runs through roscoe, so no init event will ever name
		// its model. The installed harness resolves aliases itself and puts the
		// concrete id on the wire, so ask it: point it at a local endpoint that
		// records the model and refuses the request. Zero tokens, no login
		// needed, and it answers for every alias in the config at once.
		aliases := []string{cfg.Tiers.Main.Model, cfg.Tiers.Middle.Model}
		prov := cfg.Tiers.Middle.Provider
		if got, err := cat.ResolveViaHarness(ctx, prov, "", aliases); err != nil {
			fmt.Fprintf(os.Stderr, "  %-12s harness probe: %v\n", prov, err)
		} else {
			for alias, concrete := range got {
				fmt.Fprintf(os.Stderr, "  %-12s %s  →  %s (asked claude)\n", prov, alias, concrete)
			}
		}
		if err := cat.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "roscoe models: %v\n", err)
			return 1
		}
	}

	for _, name := range sortedProviders(cfg) {
		known := cat.Models(name)
		fmt.Printf("%s\n", name)
		if len(known) == 0 {
			fmt.Println("  (nothing known yet; try --refresh, or run a task and roscoe will learn from it)")
			continue
		}
		for _, m := range known {
			fmt.Printf("  %s\n", m)
		}
	}
	// What the tiers are configured to use, and what that means.
	fmt.Println()
	for _, t := range []struct{ label, prov, model string }{
		{"tier 1", cfg.Tiers.Main.Provider, cfg.Tiers.Main.Model},
		{"tier 2", cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model},
		{"tier 3", cfg.Tiers.Subagents.Provider, cfg.Tiers.Subagents.Model},
	} {
		fmt.Printf("%s  %s\n", t.label, cat.Describe(t.prov, t.model))
	}
	return 0
}

func sortedProviders(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// providerAuth resolves a provider's credential for a plain GET. An
// account-auth provider has none the supervisor can read, which is why the
// catalogue also learns from init events.
func providerAuth(p config.Provider, env map[string]string) string {
	switch {
	case len(p.Auth) > 4 && p.Auth[:4] == "env:":
		name := p.Auth[4:]
		if v := env[name]; v != "" {
			return "Bearer " + v
		}
		return "Bearer " + os.Getenv(name)
	case len(p.Auth) > 7 && p.Auth[:7] == "static:":
		return "Bearer " + p.Auth[7:]
	default:
		return ""
	}
}

// learnResolvedModel records what a harness actually resolved an alias to.
// The init event carries it on every run, so the catalogue fills itself in
// during normal use without any credential the fleet does not already have.
func learnResolvedModel(cfg *config.Config, provider, alias, resolved string) {
	if resolved == "" || alias == "" {
		return
	}
	cat := models.Open(cfg.StateDir)
	cat.Learn(provider, alias, resolved)
	_ = cat.Save()
}
