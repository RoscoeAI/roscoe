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
	Autonomy  Autonomy            `json:"autonomy"`
	Memory    Memory              `json:"memory"`
	Quorum    Quorum              `json:"quorum"`
	Router    RouterCfg           `json:"router"`
	Limits    Limits              `json:"limits"`
	Reporting Reporting           `json:"reporting"`
}

// Autonomy is the fleet's escalation dial, 0-100. It governs how much the
// quorum absorbs before a human is interrupted: at 100 Roscoe never texts
// you at all — the quorum answers every question, each decision lands in
// the ledger and memory, and the only remaining escalation is exhausted
// credits/budget. Lower levels widen the always-escalate surface.
type Autonomy struct {
	Level int `json:"level"`
}

// Memory: Roscoe is opinionated — Graphify
// (github.com/Graphify-Labs/graphify) is the fleet's memory. Runs,
// quorum decisions, and outcomes feed a knowledge graph that lives with
// the rest of the fleet state under ~/.roscoe.
type Memory struct {
	Engine  string `json:"engine"` // "graphify"
	Path    string `json:"path"`   // "~/.roscoe/graph"
	Enabled bool   `json:"enabled"`
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
	// Harness picks the worker CLI: "claude" (default; full tier-3 subagent
	// swarms via the router) or "codex" (single-agent workers via
	// `codex exec`; codex owns its own auth/model config, and tier-3
	// swarms/resume don't apply).
	Harness string `json:"harness,omitempty"`
	// Effort is claude's reasoning effort for workers: low, medium, high,
	// xhigh, max, or ultracode. Empty leaves claude's default. "ultracode"
	// is xhigh reasoning plus a workflow planned for each substantive task,
	// which is roscoe's shape exactly: the worker plans, and the fan-out
	// lands on the cheap tier-3 swarm through the router.
	Effort string `json:"effort,omitempty"`
	// Orchestrate nudges each worker to fan out with the Workflow tool
	// instead of doing everything in one thread. Redundant under
	// effort "ultracode" (and skipped there); use it to get the same fan-out
	// at a lower effort level.
	Orchestrate         bool     `json:"orchestrate,omitempty"`
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

// NotifyCfg: Channel "twilio-sms" (bring-your-own number; reads
// TWILIO_ACCOUNT_SID / TWILIO_AUTH_TOKEN / TWILIO_MESSAGING_SERVICE_SID or
// TWILIO_FROM / TWILIO_TO from the env file) or "roscoe-relay" (hosted
// shared number; linked via `roscoe upgrade`, credentials in
// ~/.roscoe/relay.json).
type NotifyCfg struct {
	Channel string   `json:"channel"`
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
