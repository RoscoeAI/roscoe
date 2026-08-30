package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default returns opinionated defaults mirroring the repo's roscoe.json
// (same providers/tiers/quorum, generic account names).
func Default() *Config {
	off := false
	return &Config{
		Version:  1,
		Project:  "roscoe.sh",
		StateDir: "~/.roscoe",
		EnvFile:  "~/.roscoe/.env",
		Accounts: []Account{
			{Name: "primary", Kind: "claude-subscription", TokenRef: "keychain:roscoe-account-primary"},
			{Name: "secondary", Kind: "claude-subscription", TokenRef: "keychain:roscoe-account-secondary"},
			{Name: "api-fallback", Kind: "anthropic-api-key", TokenRef: "env:ANTHROPIC_API_KEY", Enabled: &off},
		},
		Providers: map[string]Provider{
			"anthropic": {
				Protocol: "anthropic",
				BaseURL:  "https://api.anthropic.com",
				Auth:     "account",
			},
			"deepinfra": {
				Protocol:       "anthropic",
				BaseURL:        "https://api.deepinfra.com/anthropic",
				Auth:           "env:DEEP_INFRA_API_KEY",
				PricingPerMtok: &Pricing{Input: 0.075, Output: 0.25, CachedInput: 0.015},
			},
			"local": {
				Protocol:    "anthropic",
				BaseURL:     "http://roscoe-2tb:11434",
				Auth:        "static:ollama",
				CountTokens: "estimate",
				Serve: map[string]any{
					"engine": "ollama",
					"model":  "glm-4.7-flash",
					"note":   "flip to glm-5.3-flash the day glm5_next lands in mainline llama.cpp/Ollama",
				},
			},
		},
		Nodes: []Node{
			{Name: "roscoe", SSH: "roscoe-ts", Workers: 2, Enabled: true},
			{Name: "roscoe-2tb", SSH: "roscoe-2tb-ts", Workers: 2, Enabled: true},
			{Name: "laptop", SSH: "", Workers: 1, Enabled: false},
		},
		Tiers: Tiers{
			Main: MainTier{
				Kind:     "interactive-mcp",
				Provider: "anthropic",
				Model:    "opus",
				Account:  "primary",
			},
			Middle: MiddleTier{
				Provider:            "anthropic",
				Model:               "sonnet",
				Accounts:            []string{"primary", "secondary"},
				Session:             "per-task",
				PermissionMode:      "bypassPermissions",
				AllowedTools:        []string{"Agent", "Task", "Workflow", "Read", "Edit", "Write", "Glob", "Grep", "Bash", "WebFetch", "WebSearch"},
				MaxBudgetUSDPerTask: 8.0,
				APITimeoutMS:        3000000,
			},
			Subagents: SubagentTier{
				Provider:      "deepinfra",
				Model:         "zai-org/GLM-5.3-Flash",
				VirtualModel:  "roscoe/tier3",
				MapHaikuAlias: true,
				MaxConcurrent: 8,
				MaxDepth:      2,
				Agents: map[string]AgentDef{
					"impl":   {Description: "Implements one well-scoped subtask end to end", Tools: []string{"Read", "Edit", "Write", "Bash", "Glob", "Grep"}},
					"scout":  {Description: "Fast read-only search, reading, and summarization", Tools: []string{"Read", "Glob", "Grep"}},
					"review": {Description: "Reviews a diff for correctness issues", Tools: []string{"Read", "Glob", "Grep", "Bash"}},
				},
			},
		},
		Quorum: Quorum{
			Enabled: true,
			Voters: []Voter{
				{Provider: "deepinfra", Model: "zai-org/GLM-5.3-Flash"},
				{Provider: "deepinfra", Model: "zai-org/GLM-5.3-Flash"},
				{Provider: "anthropic", Model: "sonnet", Account: "secondary"},
			},
			Decide:         "majority",
			MinConfidence:  0.7,
			AutoAnswer:     []string{"clarifying-questions", "permission-prompts", "retry-or-accept"},
			AlwaysEscalate: []string{"destructive-actions", "spend-over-usd:20", "external-publishing"},
			Notify: NotifyCfg{
				Channel: "twilio-sms",
				On:      []string{"auto-answer", "escalation", "task-done"},
			},
		},
		Router: RouterCfg{Bind: "127.0.0.1", Port: 8484, DefaultRoute: "middle"},
		Limits: Limits{MaxParallelTasks: 4, PerAccountMaxConcurrent: 2, RunBudgetUSD: 50},
		Reporting: Reporting{
			Ledger:    "~/.roscoe/runs/{run_id}/events.jsonl",
			Artifacts: "git-branch-per-task",
			GitRemote: "git@github.com:YOURUSER/roscoe-artifacts.git",
		},
	}
}

// Load reads path with a strict decoder (unknown fields rejected), then
// validates. All validation errors are joined into the returned error.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("validate %s: %w", path, errors.Join(errs...))
	}
	return &c, nil
}

// Save writes the config atomically: marshal-indent to a temp file in the
// destination directory, then rename over path. Mode 0644.
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// Validate checks internal consistency: referenced providers/accounts exist,
// the tier-3 virtual model is non-empty, and the router port is sane. It
// returns all problems found, nil when the config is valid.
func (c *Config) Validate() []error {
	var errs []error

	accounts := make(map[string]bool, len(c.Accounts))
	for _, a := range c.Accounts {
		accounts[a.Name] = true
	}
	provider := func(where, name string) {
		if _, ok := c.Providers[name]; !ok {
			errs = append(errs, fmt.Errorf("%s: unknown provider %q", where, name))
		}
	}
	account := func(where, name string) {
		if !accounts[name] {
			errs = append(errs, fmt.Errorf("%s: unknown account %q", where, name))
		}
	}

	provider("tiers.main.provider", c.Tiers.Main.Provider)
	if c.Tiers.Main.Account != "" {
		account("tiers.main.account", c.Tiers.Main.Account)
	}
	provider("tiers.middle.provider", c.Tiers.Middle.Provider)
	for i, a := range c.Tiers.Middle.Accounts {
		account(fmt.Sprintf("tiers.middle.accounts.%d", i), a)
	}
	provider("tiers.subagents.provider", c.Tiers.Subagents.Provider)
	if c.Tiers.Subagents.VirtualModel == "" {
		errs = append(errs, errors.New("tiers.subagents.virtual_model must not be empty"))
	}
	for i, v := range c.Quorum.Voters {
		provider(fmt.Sprintf("quorum.voters.%d.provider", i), v.Provider)
		if v.Account != "" {
			account(fmt.Sprintf("quorum.voters.%d.account", i), v.Account)
		}
	}
	if c.Router.Port < 1 || c.Router.Port > 65535 {
		errs = append(errs, fmt.Errorf("router.port %d out of range 1-65535", c.Router.Port))
	}
	switch c.Router.DefaultRoute {
	case "main", "middle", "subagents":
	default:
		errs = append(errs, fmt.Errorf("router.default_route %q is not a tier name (main|middle|subagents)", c.Router.DefaultRoute))
	}
	return errs
}

// Get resolves a dotted path ("tiers.subagents.model", arrays by index
// "nodes.0.ssh") against the JSON form of the config. Numbers come back as
// json.Number so they print exactly as stored.
func (c *Config) Get(dotted string) (any, error) {
	if dotted == "" {
		return nil, errors.New("empty path")
	}
	m, err := c.toMap()
	if err != nil {
		return nil, err
	}
	var cur any = m
	segs := strings.Split(dotted, ".")
	for i, seg := range segs {
		cur, err = step(cur, seg, strings.Join(segs[:i+1], "."))
		if err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// SetPath sets a dotted path to raw, parsed as JSON when it is valid JSON
// (true, 3, 1.5, "x", null, [..], {..}) and as a plain string otherwise. The
// mutation happens on a map round-trip; the result is strict-re-decoded into
// Config so type errors are caught before c is touched. Array indices must
// already exist (no append), map keys may be new.
func (c *Config) SetPath(dotted string, raw string) error {
	if dotted == "" {
		return errors.New("empty path")
	}
	var val any
	vdec := json.NewDecoder(strings.NewReader(raw))
	vdec.UseNumber()
	if err := vdec.Decode(&val); err != nil || vdec.More() {
		val = raw // not (one) valid JSON value: treat as a plain string
	}

	m, err := c.toMap()
	if err != nil {
		return err
	}
	segs := strings.Split(dotted, ".")
	var cur any = m
	for i, seg := range segs[:len(segs)-1] {
		cur, err = step(cur, seg, strings.Join(segs[:i+1], "."))
		if err != nil {
			return err
		}
	}
	last := segs[len(segs)-1]
	switch container := cur.(type) {
	case map[string]any:
		container[last] = val
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil {
			return fmt.Errorf("%q: array index must be a number", dotted)
		}
		if i < 0 || i >= len(container) {
			return fmt.Errorf("%q: index %d out of range (len %d)", dotted, i, len(container))
		}
		container[i] = val
	default:
		return fmt.Errorf("cannot set %q: parent is not an object or array", dotted)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("re-marshal config: %w", err)
	}
	rdec := json.NewDecoder(bytes.NewReader(data))
	rdec.DisallowUnknownFields()
	var next Config
	if err := rdec.Decode(&next); err != nil {
		return fmt.Errorf("set %s: %w", dotted, err)
	}
	*c = next
	return nil
}

// ExpandPath expands a leading "~" or "~/" to the user's home directory;
// anything else (including "~user/...") passes through unchanged.
func ExpandPath(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// LoadEnvFile parses KEY=VAL lines. Blank lines and lines starting with #
// are skipped; keys and values are whitespace-trimmed, values otherwise
// verbatim (no quote handling).
func LoadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	env := make(map[string]string)
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not a KEY=VAL line", path, i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, i+1)
		}
		env[key] = strings.TrimSpace(val)
	}
	return env, nil
}

// toMap round-trips the config through JSON into a generic map, preserving
// number literals via json.Number.
func (c *Config) toMap() (map[string]any, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode config map: %w", err)
	}
	return m, nil
}

// step resolves one dotted-path segment against a generic JSON container.
// sofar is the path up to and including seg, used in error messages.
func step(cur any, seg, sofar string) (any, error) {
	switch v := cur.(type) {
	case map[string]any:
		val, ok := v[seg]
		if !ok {
			return nil, fmt.Errorf("no such key %q", sofar)
		}
		return val, nil
	case []any:
		i, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf("%q: array index must be a number", sofar)
		}
		if i < 0 || i >= len(v) {
			return nil, fmt.Errorf("%q: index %d out of range (len %d)", sofar, i, len(v))
		}
		return v[i], nil
	default:
		return nil, fmt.Errorf("%q: value is not an object or array", sofar)
	}
}
