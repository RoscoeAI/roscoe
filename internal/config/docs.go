package config

import (
	"sort"
	"strconv"
	"strings"
)

// Doc is everything a person needs before changing one setting: what it
// does, what it can be set to, and what the choice costs them. The three
// are kept apart so a screen can lay them out, rather than parsed back out
// of one sentence.
type Doc struct {
	What    string   // one plain sentence
	Choices []string // the allowed values, when the set is small
	Format  string   // the shape of a free-form value, when it is not obvious
	Effect  string   // the consequence that decides the choice: cost, speed, interruptions
}

// DocFor resolves a dotted path, wildcards included, to its Doc. A path
// nobody documented gets an empty Doc, never a guess.
func DocFor(path string) Doc {
	path = strings.TrimSuffix(path, ".")
	d := Doc{What: Describe(path)}
	if k := resolveKey(path, choices); k != "" {
		d.Choices = append([]string(nil), choices[k]()...)
	}
	if k := resolveKey(path, formats); k != "" {
		d.Format = formats[k]
	}
	if k := resolveKey(path, effects); k != "" {
		d.Effect = effects[k]
	}
	return d
}

// resolveKey finds the key in m that documents path: the path itself, its
// list when path is a list index, or a wildcard form with one or two
// segments replaced, tried right to left. Same rules Describe uses.
func resolveKey[V any](path string, m map[string]V) string {
	if _, ok := m[path]; ok {
		return path
	}
	segs := strings.Split(path, ".")
	if len(segs) > 1 {
		if _, err := strconv.Atoi(segs[len(segs)-1]); err == nil {
			if k := resolveKey(strings.Join(segs[:len(segs)-1], "."), m); k != "" {
				return k
			}
		}
	}
	for i := len(segs) - 1; i >= 0; i-- {
		probe := append([]string(nil), segs...)
		probe[i] = "*"
		if k := strings.Join(probe, "."); has(m, k) {
			return k
		}
		for j := i + 1; j < len(segs); j++ {
			probe2 := append([]string(nil), probe...)
			probe2[j] = "*"
			if k := strings.Join(probe2, "."); has(m, k) {
				return k
			}
		}
	}
	return ""
}

func has[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

var yesNo = func() []string { return []string{"true", "false"} }

// choices lists the allowed values for settings that have a small set. A
// function, so lists that live elsewhere (effort levels) are never copied.
var choices = map[string]func() []string{
	"tiers.main.effort":               EffortLevels,
	"tiers.middle.effort":             EffortLevels,
	"tiers.middle.harness":            func() []string { return []string{"claude", "codex"} },
	"tiers.middle.cache_ttl":          func() []string { return []string{"1h", "5m"} },
	"tiers.middle.lean_context":       yesNo,
	"tiers.middle.orchestrate":        yesNo,
	"tiers.middle.permission_mode":    func() []string { return []string{"bypassPermissions", "acceptEdits", "default", "plan", "dontAsk"} },
	"tiers.main.kind":                 func() []string { return []string{"interactive-mcp", "headless"} },
	"tiers.subagents.map_haiku_alias": yesNo,
	"tiers.subagents.max_concurrent":  func() []string { return []string{"1", "2", "4", "8", "12", "16", "24"} },
	"accounts.*.kind":                 func() []string { return []string{"claude-subscription", "anthropic-api-key"} },
	"accounts.*.enabled":              yesNo,
	"nodes.*.enabled":                 yesNo,
	"providers.*.protocol":            func() []string { return []string{"anthropic"} },
	"providers.*.count_tokens":        func() []string { return []string{"estimate", "upstream"} },
	"providers.*.serve.engine":        func() []string { return []string{"ollama", "vllm", "mlx"} },
	"memory.engine":                   func() []string { return []string{"graphify"} },
	"memory.enabled":                  yesNo,
	"quorum.enabled":                  yesNo,
	"quorum.decide":                   func() []string { return []string{"majority"} },
	"quorum.notify.channel":           func() []string { return []string{"twilio-sms", "roscoe-relay"} },
	"router.default_route":            func() []string { return []string{"main", "middle", "subagents"} },
}

// formats describe free-form values whose shape is not obvious from a name.
var formats = map[string]string{
	"autonomy.level":                       "0 to 100",
	"tiers.main.model":                     "an alias (opus, sonnet) or a full model id; roscoe models lists them",
	"tiers.middle.model":                   "an alias (opus, sonnet) or a full model id; roscoe models lists them",
	"tiers.subagents.model":                "the provider's model id; roscoe models lists them",
	"tiers.middle.max_budget_usd_per_task": "dollars, e.g. 8",
	"tiers.middle.api_timeout_ms":          "milliseconds",
	"tiers.subagents.max_depth":            "a small whole number; 1 is subagents that cannot spawn",
	"tiers.middle.accounts":                "account names from accounts[], most preferred first",
	"tiers.middle.allowed_tools":           "tool names; mcp__<server> admits a whole MCP server",
	"tiers.middle.mcp_servers":             "a map: name to {type, command, args} or {type, url}",
	"accounts.*.token_ref":                 "keychain:<service> or env:<VAR>",
	"accounts.*.minted_at":                 "a date, 2026-09-02",
	"nodes.*.ssh":                          "an alias from ~/.ssh/config",
	"nodes.*.workers":                      "a whole number of worker slots",
	"providers.*.base_url":                 "https://...",
	"providers.*.auth":                     "account, env:<VAR>, or static:<value>",
	"quorum.min_confidence":                "0 to 1",
	"limits.run_budget_usd":                "dollars",
	"limits.max_parallel_tasks":            "a whole number",
	"limits.per_account_max_concurrent":    "a whole number",
	"router.port":                          "a port number; 0 picks a free one",
	"reporting.git_remote":                 "a git remote name or URL; blank keeps branches local",
	"memory.path":                          "a directory",
	"state_dir":                            "a directory",
	"env_file":                             "a file path",
}

// effects is the line that decides a choice: what it costs, what it speeds
// up, who it interrupts. Only knobs with a real consequence have one.
var effects = map[string]string{
	"tiers.middle.effort":                  "ultracode adds ~6K tokens of prefix per task and plans a workflow; lower levels answer faster and cheaper",
	"tiers.main.effort":                    "higher thinks longer on every reply; ultracode plans a workflow per task",
	"tiers.middle.model":                   "the main lever on cost per turn; opus costs several times sonnet",
	"tiers.middle.lean_context":            "true is much cheaper: one small prefix, cached once, read on every call. false loads your whole ~/.claude into every worker",
	"tiers.middle.cache_ttl":               "1h costs more to write once and stays warm across an hour of runs; 5m is cheaper to write and is gone between runs",
	"tiers.middle.mcp_servers":             "every server adds its tool schemas to every worker's prefix, on every call",
	"tiers.middle.permission_mode":         "bypassPermissions is what unattended work needs; anything else stalls on prompts nobody is there to answer",
	"tiers.middle.max_budget_usd_per_task": "a worker that reaches it is stopped mid-task",
	"tiers.subagents.model":                "the swarm makes many calls per task, so a cheap model here matters more than anywhere",
	"tiers.subagents.provider":             "the swarm's bill goes here; the router prices it from providers.*.pricing_per_mtok",
	"tiers.subagents.max_concurrent":       "more is faster and spends faster; the provider's rate limit is the real ceiling",
	"autonomy.level":                       "higher means fewer interruptions and more decisions made for you by the quorum",
	"quorum.enabled":                       "off, every question a worker asks waits for you",
	"quorum.min_confidence":                "higher means more questions reach you",
	"limits.run_budget_usd":                "a run that reaches it is stopped",
	"limits.max_parallel_tasks":            "more runs at once, more spend at once",
	"nodes.*.workers":                      "dispatch fills this many slots on the machine before choosing another",
	"accounts.*.enabled":                   "a disabled account is skipped; workers fall through to the next",
	"memory.enabled":                       "on, each run recalls what earlier runs learned and adds to it; off, every run starts cold",
}

// order lists a branch's children most-changed first. Branches absent here
// list alphabetically. Anything a list forgets is appended, so a new field
// can never vanish from the screen.
var order = map[string][]string{
	"":                {"tiers", "autonomy", "quorum", "accounts", "nodes", "providers", "limits", "memory", "reporting", "router", "project", "state_dir", "env_file", "version"},
	"tiers":           {"main", "middle", "subagents"},
	"tiers.main":      {"model", "provider", "effort", "account", "kind"},
	"tiers.middle":    {"model", "provider", "harness", "effort", "lean_context", "cache_ttl", "mcp_servers", "accounts", "permission_mode", "allowed_tools", "max_budget_usd_per_task", "orchestrate", "session", "api_timeout_ms"},
	"tiers.subagents": {"model", "provider", "max_concurrent", "max_depth", "agents", "virtual_model", "map_haiku_alias"},
	"autonomy":        {"level"},
	"quorum":          {"enabled", "voters", "auto_answer", "always_escalate", "min_confidence", "decide", "notify"},
	"limits":          {"run_budget_usd", "max_parallel_tasks", "per_account_max_concurrent"},
}

// rare is the setup a person does once. The top-level listing sets these
// apart so the knobs that matter every day are not lost among them.
var rare = map[string]bool{"project": true, "state_dir": true, "env_file": true, "version": true}

// Ordered returns children of prefix, most-changed first, every child
// exactly once.
func Ordered(prefix string, children []string) []string {
	want := order[strings.TrimSuffix(prefix, ".")]
	if len(want) == 0 {
		out := append([]string(nil), children...)
		sort.Strings(out)
		return out
	}
	byName := map[string]string{}
	for _, c := range children {
		byName[c[strings.LastIndex(c, ".")+1:]] = c
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range want {
		if full, ok := byName[n]; ok {
			out = append(out, full)
			seen[full] = true
		}
	}
	var rest []string
	for _, c := range children {
		if !seen[c] {
			rest = append(rest, c)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Rare reports whether a top-level key is one-time setup rather than a knob.
func Rare(path string) bool { return rare[path] }

// implied is what a setting means when it is absent from the file. These are
// omitted when empty on save, so a listing that only read the file would show
// a blank where the fleet is in fact running with a value.
var implied = map[string]string{
	"tiers.middle.harness":      "claude",
	"tiers.middle.lean_context": "true",
	"tiers.middle.cache_ttl":    "1h",
	"tiers.middle.orchestrate":  "false",
	"tiers.middle.mcp_servers":  "none",
	"tiers.middle.effort":       "claude's default",
	"tiers.main.effort":         "claude's default",
	"tiers.main.account":        "the first enabled account",
	"reporting.git_remote":      "local branches only",
	"accounts.*.enabled":        "true",
	"nodes.*.enabled":           "true",
	"memory.enabled":            "true",
}

// Implied is the value a setting has when it is not in the file, and whether
// there is one.
func Implied(path string) (string, bool) {
	k := resolveKey(strings.TrimSuffix(path, "."), implied)
	if k == "" {
		return "", false
	}
	return implied[k], true
}
