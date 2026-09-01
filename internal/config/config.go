package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
				Effort:              "ultracode",
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
		Autonomy: Autonomy{Level: 90},
		Memory:   Memory{Engine: "graphify", Path: "~/.roscoe/graph", Enabled: true},
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

// effortLevels are the values claude accepts for --effort. "ultracode" is
// absent from claude's own --help but accepted (an unknown value warns; this
// one does not), and adds workflow planning on top of xhigh reasoning.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max", "ultracode"}

// EffortLevels returns the accepted --effort values, cheapest first.
func EffortLevels() []string {
	return append([]string(nil), effortLevels...)
}

var validEffort = func() map[string]bool {
	m := make(map[string]bool, len(effortLevels))
	for _, e := range effortLevels {
		m[e] = true
	}
	return m
}()

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
		return nil, fmt.Errorf("parse %s: %w", path, explainDecode(err))
	}
	if errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("validate %s: %w", path, errors.Join(errs...))
	}
	return &c, nil
}

// unknownFieldRE pulls the field name out of encoding/json's message, which
// reads: json: unknown field "effort".
var unknownFieldRE = regexp.MustCompile(`unknown field "([^"]+)"`)

// explainDecode turns a strict-decoding failure into something a person can
// act on. Rejecting unknown fields catches typos, which is worth keeping, but
// it means a config written by a newer roscoe fails on an older one with a
// message that points at the config when the problem is the binary. That is a
// bad half-hour for whoever hits it.
func explainDecode(err error) error {
	m := unknownFieldRE.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	return fmt.Errorf("this config sets %q, which this roscoe does not know. "+
		"It was probably written by a newer roscoe than the one you are running "+
		"(%s). Update with: curl -fsSL https://roscoe.sh/install | sh. "+
		"If you added it by hand, it is a typo: %w", m[1], Version, err)
}

// Version is the running binary's version, set from main so a config error can
// say which roscoe rejected it. "dev" until a release stamps it.
var Version = "dev"

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

	if c.Autonomy.Level < 0 || c.Autonomy.Level > 100 {
		errs = append(errs, fmt.Errorf("autonomy.level must be 0-100, got %d", c.Autonomy.Level))
	}

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
	if t := c.Tiers.Middle.CacheTTL; t != "" && t != "1h" && t != "5m" {
		errs = append(errs, fmt.Errorf("tiers.middle.cache_ttl %q is not 1h or 5m", t))
	}
	if e := c.Tiers.Middle.Effort; e != "" && !validEffort[e] {
		// claude only warns and falls back to its default, which silently
		// costs you the reasoning you asked for; fail here instead.
		errs = append(errs, fmt.Errorf("tiers.middle.effort %q is not one of %s", e, strings.Join(effortLevels, ", ")))
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

// Paths returns every dotted path in this config, leaves and containers
// alike, so a UI can complete them. Derived from the marshalled config, so
// map keys (providers.deepinfra.base_url) and array indices (nodes.0.ssh)
// are real rather than guessed.
func (c *Config) Paths() []string {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	var out []string
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out = append(out, p)
				walk(p, child)
			}
		case []any:
			for i, child := range v {
				p := fmt.Sprintf("%s.%d", prefix, i)
				out = append(out, p)
				walk(p, child)
			}
		}
	}
	walk("", tree)
	sort.Strings(out)
	return out
}

// docs maps config paths to one-line descriptions. Array indices and map keys
// match "*", so providers.deepinfra.base_url finds providers.*.base_url.
var docs = map[string]string{
	"version":                                   "config schema version",
	"project":                                   "name shown in fleet output",
	"state_dir":                                 "where roscoe keeps runs, workers, and credentials",
	"env_file":                                  "file holding secrets (API keys, Twilio); never the config itself",
	"accounts":                                  "Claude credentials the fleet may use",
	"accounts.*":                                "one credential: name, kind, and where the token lives",
	"accounts.*.name":                           "how tiers refer to this account",
	"accounts.*.kind":                           "claude-subscription or anthropic-api-key",
	"accounts.*.token_ref":                      "where the token lives: keychain:<service> or env:<VAR>",
	"accounts.*.enabled":                        "false parks an account without deleting it",
	"accounts.*.minted_at":                      "when a setup-token was created; they expire at 12 months",
	"providers":                                 "model endpoints the router can reach",
	"providers.*":                               "one endpoint: protocol, base URL, and how to authenticate",
	"providers.*.protocol":                      "wire protocol; anthropic today",
	"providers.*.base_url":                      "where requests go",
	"providers.*.auth":                          "account (pass the worker's own headers), env:<VAR>, or static:<value>",
	"providers.*.count_tokens":                  "estimate answers count_tokens locally instead of upstream",
	"providers.*.pricing_per_mtok":              "dollars per million tokens, for the ledger",
	"providers.*.serve":                         "how a local engine serves this model",
	"nodes":                                     "machines that can run workers",
	"nodes.*":                                   "one machine: ssh alias, worker slots, enabled",
	"nodes.*.ssh":                               "ssh alias; empty means this machine",
	"nodes.*.workers":                           "how many workers this machine may run",
	"tiers":                                     "the three tiers: your session, the workers, the swarm",
	"tiers.main":                                "the session you talk to",
	"tiers.middle":                              "the headless workers that do the work",
	"tiers.middle.harness":                      "claude or codex",
	"tiers.middle.cache_ttl":                    "how long the prompt prefix stays cached: 1h or 5m",
	"tiers.middle.lean_context":                 "strip your MCP servers and personal skills from workers; a much cheaper prompt prefix",
	"tiers.middle.effort":                       "reasoning effort; ultracode adds a planned workflow per task",
	"tiers.middle.orchestrate":                  "fan out with workflows below ultracode effort; ultracode does it already",
	"tiers.middle.provider":                     "which provider serves the worker model",
	"tiers.middle.model":                        "the worker model",
	"tiers.middle.accounts":                     "accounts workers may draw from",
	"tiers.middle.permission_mode":              "how much a worker may do unattended",
	"tiers.middle.allowed_tools":                "tools workers may use",
	"tiers.middle.max_budget_usd_per_task":      "spend ceiling for one task",
	"tiers.middle.api_timeout_ms":               "how long a worker waits on the API",
	"tiers.subagents":                           "the cheap swarm each worker fans out to",
	"tiers.subagents.provider":                  "who serves the swarm; DeepInfra by default",
	"tiers.subagents.model":                     "the swarm model",
	"tiers.subagents.virtual_model":             "name subagent requests carry; the router rewrites it",
	"tiers.subagents.map_haiku_alias":           "send haiku-alias traffic to the swarm too",
	"tiers.subagents.max_concurrent":            "how many subagents one worker may run at once",
	"tiers.subagents.max_depth":                 "how deep subagents may spawn subagents",
	"tiers.subagents.agents":                    "named subagents workers can call",
	"autonomy":                                  "how much roscoe decides without you",
	"autonomy.level":                            "0-100; at 100 only exhausted credits interrupt you",
	"memory":                                    "the knowledge graph the fleet learns into",
	"memory.engine":                             "graphify",
	"memory.path":                               "where the graph lives",
	"quorum":                                    "the models that answer questions in your place",
	"quorum.voters":                             "who votes",
	"quorum.decide":                             "how votes resolve; majority",
	"quorum.min_confidence":                     "below this, escalate instead of deciding",
	"quorum.auto_answer":                        "question kinds the quorum may answer",
	"quorum.always_escalate":                    "kinds that always reach you, whatever the dial says",
	"quorum.notify":                             "how escalations reach you",
	"quorum.notify.channel":                     "twilio-sms or roscoe-relay",
	"router":                                    "the loopback proxy that splits traffic by model name",
	"router.port":                               "port it listens on; busy falls back to ephemeral",
	"router.default_route":                      "tier whose provider serves unmatched models",
	"limits":                                    "fleet-wide ceilings",
	"limits.max_parallel_tasks":                 "tasks running at once",
	"limits.per_account_max_concurrent":         "workers per account, to stay under rate limits",
	"limits.run_budget_usd":                     "spend ceiling for a run",
	"reporting":                                 "where the ledger and work products go",
	"reporting.ledger":                          "append-only event log per run",
	"reporting.artifacts":                       "how work comes back; a git branch per task",
	"reporting.git_remote":                      "where task branches are pushed; blank keeps them local",
	"providers.*.pricing_per_mtok.input":        "dollars per million input tokens",
	"providers.*.pricing_per_mtok.output":       "dollars per million output tokens",
	"providers.*.pricing_per_mtok.cached_input": "dollars per million cached input tokens",
	"providers.*.serve.engine":                  "the local server: ollama, vllm, mlx",
	"providers.*.serve.model":                   "the model tag that engine loads",
	"providers.*.serve.note":                    "a reminder to yourself; roscoe ignores it",
	"nodes.*.name":                              "how you refer to this machine",
	"nodes.*.enabled":                           "false parks a machine without removing it",
	"memory.enabled":                            "whether runs feed the graph",
	"quorum.enabled":                            "whether a quorum answers in your place at all",
	"quorum.voters.*":                           "one voter: provider, model, and account",
	"quorum.voters.*.provider":                  "who serves this voter",
	"quorum.voters.*.model":                     "the voting model",
	"quorum.voters.*.account":                   "which account pays for it; blank uses the default",
	"quorum.notify.on":                          "the events worth a message",
	"router.bind":                               "interface it binds; loopback",
	"tiers.main.kind":                           "how the top tier runs: interactive or headless",
	"tiers.main.provider":                       "who serves your own session",
	"tiers.main.model":                          "the model you talk to",
	"tiers.main.account":                        "which account your session runs under",
	"tiers.middle.session":                      "per-task; each task gets a fresh worker",
	"tiers.subagents.agents.*":                  "one named subagent: what it is for and what it may use",
	"tiers.subagents.agents.*.description":      "when a worker should reach for this subagent",
	"tiers.subagents.agents.*.prompt":           "its system prompt; defaults to the description",
	"tiers.subagents.agents.*.tools":            "tools this subagent may use",
}

// Describe returns a one-line description of a config path, or "".
func Describe(path string) string {
	// Mid-walk ("tiers.") the branch itself is what needs describing.
	path = strings.TrimSuffix(path, ".")
	if path == "" {
		return "everything roscoe knows: accounts, providers, nodes, tiers, limits"
	}
	if d, ok := docs[path]; ok {
		return d
	}
	segs := strings.Split(path, ".")
	// A list item takes its list's description: allowed_tools.3 is one of
	// "tools workers may use".
	if len(segs) > 1 {
		if _, err := strconv.Atoi(segs[len(segs)-1]); err == nil {
			if d := Describe(strings.Join(segs[:len(segs)-1], ".")); d != "" {
				return d
			}
		}
	}
	// Try wildcard forms, replacing segments right to left.
	for i := len(segs) - 1; i >= 0; i-- {
		probe := append([]string(nil), segs...)
		probe[i] = "*"
		if d, ok := docs[strings.Join(probe, ".")]; ok {
			return d
		}
		for j := i + 1; j < len(segs); j++ {
			probe2 := append([]string(nil), probe...)
			probe2[j] = "*"
			if d, ok := docs[strings.Join(probe2, ".")]; ok {
				return d
			}
		}
	}
	return ""
}

// ChildPaths returns the next level of paths under prefix: the top-level keys
// when prefix is empty, otherwise prefix's immediate children. This keeps
// completion a walk rather than a dump of every leaf.
func (c *Config) ChildPaths(prefix string) []string {
	// Union what the file holds with what the schema documents, so a setting
	// that is simply unset (an omitempty field) is still discoverable.
	all := c.Paths()
	have := make(map[string]bool, len(all))
	for _, p := range all {
		have[p] = true
	}
	for p := range docs {
		if !strings.Contains(p, "*") && !have[p] {
			all = append(all, p)
			have[p] = true
		}
	}
	depth := 1
	if prefix != "" {
		depth = strings.Count(prefix, ".") + 2
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range all {
		if prefix != "" && !strings.HasPrefix(p, prefix+".") && p != prefix {
			continue
		}
		segs := strings.Split(p, ".")
		if len(segs) < depth {
			continue
		}
		candidate := strings.Join(segs[:depth], ".")
		if !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}
