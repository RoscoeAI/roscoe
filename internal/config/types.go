// Package config defines roscoe.json — the single source of truth for the
// roscoe orchestrator — and its load/save/validate/get/set operations.
package config

// Config mirrors roscoe.json exactly. Field docs describe operational meaning;
// see ARCHITECTURE.md for the design.
type Config struct {
	Version   int                 `json:"version"`
	Project   string              `json:"project"`
	StateDir  string              `json:"state_dir"`
	EnvFile   string              `json:"env_file"`
	Accounts  []Account           `json:"accounts"`
	Providers map[string]Provider `json:"providers"`
	Nodes     []Node              `json:"nodes"`
	Tiers     Tiers               `json:"tiers"`
	Quorum    Quorum              `json:"quorum"`
	Router    RouterCfg           `json:"router"`
	Limits    Limits              `json:"limits"`
	Reporting Reporting           `json:"reporting"`
}

// Account is one Claude credential. Kind: "claude-subscription" (setup-token
// OAuth, sk-ant-oat01-…) or "anthropic-api-key". TokenRef: "keychain:<service>"
// or "env:<VAR>". Enabled defaults to true when nil.
type Account struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	TokenRef string `json:"token_ref"`
	Enabled  *bool  `json:"enabled,omitempty"`
	// MintedAt (RFC3339 date) tracks setup-token age; tokens expire silently
	// at 12 months, roscoe nags at month 11.
	MintedAt string `json:"minted_at,omitempty"`
}

// Provider is an Anthropic-protocol (or OpenAI-protocol, future) upstream.
// Auth: "account" (forward the worker's own auth headers untouched),
// "env:<VAR>" (Bearer from env file), or "static:<value>" (literal Bearer).
// CountTokens: "" = forward upstream, "estimate" = answer locally with
// len(body)/4 (Ollama count_tokens hang workaround).
type Provider struct {
	Protocol       string         `json:"protocol"`
	BaseURL        string         `json:"base_url"`
	Auth           string         `json:"auth"`
	CountTokens    string         `json:"count_tokens,omitempty"`
	PricingPerMtok *Pricing       `json:"pricing_per_mtok,omitempty"`
	Serve          map[string]any `json:"serve,omitempty"`
}

type Pricing struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
}

// Node is one machine. SSH is the ssh alias/host ("" = local). Workers is the
// middle-tier worker slot count.
type Node struct {
	Name    string `json:"name"`
	SSH     string `json:"ssh"`
	Workers int    `json:"workers"`
	Enabled bool   `json:"enabled"`
}

type Tiers struct {
	Main      MainTier     `json:"main"`
	Middle    MiddleTier   `json:"middle"`
	Subagents SubagentTier `json:"subagents"`
}

type MainTier struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account"`
}

type MiddleTier struct {
	Provider            string   `json:"provider"`
	Model               string   `json:"model"`
	Accounts            []string `json:"accounts"`
	Session             string   `json:"session"` // "per-task" (only supported mode)
	PermissionMode      string   `json:"permission_mode"`
	AllowedTools        []string `json:"allowed_tools"`
	MaxBudgetUSDPerTask float64  `json:"max_budget_usd_per_task"`
	APITimeoutMS        int      `json:"api_timeout_ms"`
}

// SubagentTier: VirtualModel is the name tier-3 requests carry on the wire
// ("roscoe/tier3"); the router rewrites it to Model on Provider.
type SubagentTier struct {
	Provider      string              `json:"provider"`
	Model         string              `json:"model"`
	VirtualModel  string              `json:"virtual_model"`
	MapHaikuAlias bool                `json:"map_haiku_alias"`
	MaxConcurrent int                 `json:"max_concurrent"`
	MaxDepth      int                 `json:"max_depth"`
	Agents        map[string]AgentDef `json:"agents"`
}

// AgentDef renders into the claude -p --agents JSON; Model is always forced to
// the tier's VirtualModel at render time, Prompt defaults to Description.
type AgentDef struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

type Quorum struct {
	Enabled        bool      `json:"enabled"`
	Voters         []Voter   `json:"voters"`
	Decide         string    `json:"decide"` // "majority"
	MinConfidence  float64   `json:"min_confidence"`
	AutoAnswer     []string  `json:"auto_answer"`
	AlwaysEscalate []string  `json:"always_escalate"`
	Notify         NotifyCfg `json:"notify"`
}

type Voter struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account,omitempty"`
}

// NotifyCfg: Channel "twilio-sms" (primary) or "ntfy". Twilio reads
// TWILIO_ACCOUNT_SID / TWILIO_AUTH_TOKEN / TWILIO_FROM / TWILIO_TO from the
// env file; ntfy uses Server+Topic.
type NotifyCfg struct {
	Channel string   `json:"channel"`
	Server  string   `json:"server,omitempty"`
	Topic   string   `json:"topic,omitempty"`
	On      []string `json:"on"`
}

type RouterCfg struct {
	Bind         string `json:"bind"`
	Port         int    `json:"port"`
	DefaultRoute string `json:"default_route"` // tier name: "middle"
}

type Limits struct {
	MaxParallelTasks        int     `json:"max_parallel_tasks"`
	PerAccountMaxConcurrent int     `json:"per_account_max_concurrent"`
	RunBudgetUSD            float64 `json:"run_budget_usd"`
}

type Reporting struct {
	Ledger    string `json:"ledger"`
	Artifacts string `json:"artifacts"`
	GitRemote string `json:"git_remote"`
}
