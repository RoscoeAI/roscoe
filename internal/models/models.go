// Package models keeps track of what "opus" and "sonnet" actually resolve to.
//
// An alias is convenient to type and useless to read back: a settings screen
// showing "opus" cannot tell you whether that is Opus 5 or something two
// releases old. This package answers that from two sources, neither of which
// requires a credential roscoe does not already have.
//
// A provider that publishes a model list is asked for it. Anthropic's list
// needs an API key, which an operator on a subscription login does not have,
// so the second source matters more: every worker's init event reports the
// model the harness actually resolved, and roscoe already parses that event.
// Watching what a run resolved costs nothing and works on exactly the auth the
// fleet is already using.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// FileName is the cache under the state dir.
const FileName = "models.json"

// fetchTimeout bounds a catalogue refresh. Knowing what "opus" means is a
// nicety; waiting on it is not.
const fetchTimeout = 15 * time.Second

// Provider is what is known about one upstream.
type Provider struct {
	// Models is the published list, when the provider publishes one.
	Models []string `json:"models,omitempty"`
	// Aliases maps what roscoe asked for to what the harness resolved,
	// learned from init events.
	Aliases map[string]string `json:"aliases,omitempty"`
	// FetchedAt is when Models was last refreshed.
	FetchedAt string `json:"fetched_at,omitempty"`
}

// Catalog is the on-disk cache.
type Catalog struct {
	mu        sync.Mutex
	path      string
	dirty     bool
	Providers map[string]*Provider `json:"providers"`

	// Client is the HTTP client used for refreshes; tests replace it.
	Client *http.Client
}

// Open loads the catalogue, returning an empty one when there is none.
func Open(stateDir string) *Catalog {
	c := &Catalog{
		path:      filepath.Join(config.ExpandPath(stateDir), FileName),
		Providers: map[string]*Provider{},
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var on struct {
		Providers map[string]*Provider `json:"providers"`
	}
	if err := json.Unmarshal(b, &on); err == nil && on.Providers != nil {
		c.Providers = on.Providers
	}
	return c
}

func (c *Catalog) provider(name string) *Provider {
	p := c.Providers[name]
	if p == nil {
		p = &Provider{}
		c.Providers[name] = p
	}
	return p
}

// Learn records that a provider resolved an alias to a concrete model. It is
// a no-op when the two are the same, so a config naming a full model id does
// not fill the cache with identity mappings.
func (c *Catalog) Learn(provider, alias, resolved string) {
	alias, resolved = strings.TrimSpace(alias), strings.TrimSpace(resolved)
	if provider == "" || alias == "" || resolved == "" || alias == resolved {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.provider(provider)
	if p.Aliases == nil {
		p.Aliases = map[string]string{}
	}
	if p.Aliases[alias] == resolved {
		return
	}
	p.Aliases[alias] = resolved
	c.dirty = true
}

// Resolve returns what an alias is known to mean, or "".
func (c *Catalog) Resolve(provider, alias string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.Providers[provider]; p != nil {
		return p.Aliases[alias]
	}
	return ""
}

// Describe renders a model for a human: the alias plus what it resolves to,
// when that is known and different.
func (c *Catalog) Describe(provider, alias string) string {
	if r := c.Resolve(provider, alias); r != "" {
		return alias + "  →  " + r
	}
	return alias
}

// Models returns a provider's known models: its published list plus anything
// an alias has been seen to resolve to.
func (c *Catalog) Models(provider string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.Providers[provider]
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range p.Models {
		add(m)
	}
	for _, r := range p.Aliases {
		add(r)
	}
	sort.Strings(out)
	return out
}

// Save writes the catalogue when anything changed.
func (c *Catalog) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	b, err := json.MarshalIndent(struct {
		Providers map[string]*Provider `json:"providers"`
	}{c.Providers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	c.dirty = false
	return nil
}

// modelPaths are tried in order. Anthropic publishes /v1/models; DeepInfra
// serves its list from the OpenAI-compatible surface even though roscoe talks
// to its Anthropic one, so the second path is not a fallback but a different
// provider's actual answer.
var modelPaths = []string{"/v1/models", "/v1/openai/models"}

// Refresh asks a provider for its model list. Providers that publish none are
// not an error: their aliases still get learned from init events.
func (c *Catalog) Refresh(ctx context.Context, name string, p config.Provider, auth string) (int, error) {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	base := strings.TrimRight(p.BaseURL, "/")
	// DeepInfra's Anthropic surface has no list; its OpenAI one does, and it
	// lives one level up from /anthropic.
	roots := []string{base}
	if i := strings.LastIndex(base, "/anthropic"); i > 0 {
		roots = append(roots, base[:i])
	}

	var lastErr error
	for _, root := range roots {
		for _, path := range modelPaths {
			ids, err := fetchList(ctx, client, root+path, auth)
			if err != nil {
				lastErr = err
				continue
			}
			if len(ids) == 0 {
				continue
			}
			c.mu.Lock()
			pr := c.provider(name)
			pr.Models = ids
			pr.FetchedAt = time.Now().UTC().Format(time.RFC3339)
			c.dirty = true
			c.mu.Unlock()
			return len(ids), nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no model list published")
	}
	return 0, lastErr
}

func fetchList(ctx context.Context, client *http.Client, url, auth string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
		// Anthropic accepts either; sending both costs nothing and saves a
		// round trip guessing which surface this is.
		req.Header.Set("x-api-key", strings.TrimPrefix(auth, "Bearer "))
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// LearnFromRuns mines past ledgers for what a harness resolved. A provider
// that publishes no list, or needs a credential the supervisor cannot read,
// still gets answered this way: the fleet has been recording the answer in
// every run it has ever done.
func (c *Catalog) LearnFromRuns(runsDir, provider, alias string) int {
	matches, err := filepath.Glob(filepath.Join(config.ExpandPath(runsDir), "*", "events.jsonl"))
	if err != nil {
		return 0
	}
	learned := 0
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		dec := json.NewDecoder(f)
		for {
			var rec map[string]json.RawMessage
			if err := dec.Decode(&rec); err != nil {
				break
			}
			raw, ok := rec["event"]
			if !ok {
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				Model   string `json:"model"`
			}
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			if ev.Type == "system" && ev.Subtype == "init" && ev.Model != "" {
				before := c.Resolve(provider, alias)
				c.Learn(provider, alias, ev.Model)
				if before != ev.Model {
					learned++
				}
			}
		}
		f.Close()
	}
	return learned
}
