package config

import (
	"strings"
	"testing"
)

// ChildPaths must offer exactly one level: the point is that a user walks the
// config, not that they read 150 leaf paths at once.
func TestChildPathsOneLevel(t *testing.T) {
	c := Default()

	top := c.ChildPaths("")
	for _, want := range []string{"accounts", "providers", "tiers", "autonomy", "quorum", "router", "limits"} {
		if !contains(top, want) {
			t.Errorf("top level missing %q; got %v", want, top)
		}
	}
	for _, p := range top {
		if strings.Contains(p, ".") {
			t.Errorf("top level offered a nested path %q", p)
		}
	}

	kids := c.ChildPaths("tiers")
	if got, want := strings.Join(kids, " "), "tiers.main tiers.middle tiers.subagents"; got != want {
		t.Errorf("ChildPaths(tiers) = %q, want %q", got, want)
	}

	for _, p := range c.ChildPaths("tiers.middle") {
		if strings.Count(p, ".") != 2 {
			t.Errorf("ChildPaths(tiers.middle) returned %q, deeper than one level", p)
		}
	}

	if kids := c.ChildPaths("autonomy.level"); len(kids) != 0 {
		t.Errorf("a leaf should have no children, got %v", kids)
	}
	if kids := c.ChildPaths("nope"); len(kids) != 0 {
		t.Errorf("unknown prefix should have no children, got %v", kids)
	}
}

// Settings that are unset in the file still have to be discoverable, or an
// omitempty field can never be found by anyone who does not already know it.
func TestChildPathsIncludesUnsetSettings(t *testing.T) {
	c := Default() // leaves tiers.middle.effort and .orchestrate at their zero values
	kids := c.ChildPaths("tiers.middle")
	for _, want := range []string{"tiers.middle.effort", "tiers.middle.orchestrate", "tiers.middle.harness"} {
		if !contains(kids, want) {
			t.Errorf("unset setting %q is not discoverable; got %v", want, kids)
		}
	}
}

func TestDescribeEveryPath(t *testing.T) {
	c := Default()
	var missing []string
	for _, p := range c.Paths() {
		if Describe(p) == "" {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d config paths have no description: %s", len(missing), strings.Join(missing, " "))
	}
	for _, p := range c.ChildPaths("") {
		if Describe(p) == "" {
			t.Errorf("top-level key %q has no description", p)
		}
	}
}

func TestDescribeMatching(t *testing.T) {
	cases := []struct{ path, want string }{
		{"", "everything roscoe knows: accounts, providers, nodes, tiers, limits"},
		{"autonomy.level", "0-100; at 100 only exhausted credits interrupt you"},
		{"accounts.0.token_ref", "where the token lives: keychain:<service> or env:<VAR>"},           // index -> *
		{"providers.deepinfra.base_url", "where requests go"},                                        // map key -> *
		{"providers.local.serve.engine", "the local server: ollama, vllm, mlx"},                      // two wildcards
		{"tiers.middle.allowed_tools.3", "tools workers may use"},                                    // list item takes its list
		{"tiers.subagents.agents.scout.description", "when a worker should reach for this subagent"}, // named map entry
	}
	for _, tc := range cases {
		if got := Describe(tc.path); got != tc.want {
			t.Errorf("Describe(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := Describe("not.a.real.path"); got != "" {
		t.Errorf("Describe of an unknown path = %q, want empty", got)
	}
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}

// Standing inside a branch, the note should describe the branch, not go blank.
func TestDescribeTrailingDot(t *testing.T) {
	if got, want := Describe("tiers."), Describe("tiers"); got != want || got == "" {
		t.Errorf("Describe(%q) = %q, want %q", "tiers.", got, want)
	}
}

// A typo in effort must fail loudly: claude only warns and falls back to its
// default, which silently costs the reasoning that was asked for.
func TestValidateEffort(t *testing.T) {
	for _, e := range effortLevels {
		c := Default()
		c.Tiers.Middle.Effort = e
		if errs := c.Validate(); len(errs) > 0 {
			t.Errorf("effort %q rejected: %v", e, errs)
		}
	}
	c := Default()
	c.Tiers.Middle.Effort = ""
	if errs := c.Validate(); len(errs) > 0 {
		t.Errorf("empty effort should be allowed (claude's default): %v", errs)
	}
	c.Tiers.Middle.Effort = "ultra"
	errs := c.Validate()
	if len(errs) == 0 {
		t.Fatal("a misspelled effort was accepted")
	}
	if !strings.Contains(errs[0].Error(), "ultracode") {
		t.Errorf("the error should list the valid values, got %v", errs[0])
	}
}

func TestValidateCacheTTL(t *testing.T) {
	for _, ttl := range []string{"", "1h", "5m"} {
		c := Default()
		c.Tiers.Middle.CacheTTL = ttl
		if errs := c.Validate(); len(errs) > 0 {
			t.Errorf("cache_ttl %q rejected: %v", ttl, errs)
		}
	}
	c := Default()
	c.Tiers.Middle.CacheTTL = "30m"
	if errs := c.Validate(); len(errs) == 0 {
		t.Error("an unsupported cache_ttl was accepted")
	}
	if got := Default().Tiers.Middle.TTL(); got != "1h" {
		t.Errorf("unset cache_ttl = %q, want claude's own default 1h", got)
	}
}

// A declared MCP server without a way to start or reach it is a typo that
// would otherwise surface as a worker failing to boot.
func TestValidateMCPServers(t *testing.T) {
	c := Default()
	c.Tiers.Middle.MCPServers = map[string]map[string]any{
		"ctx7": {"type": "stdio", "command": "npx", "args": []any{"-y", "@upstash/context7-mcp"}},
		"neon": {"type": "http", "url": "https://mcp.neon.tech/mcp"},
	}
	if errs := c.Validate(); len(errs) > 0 {
		t.Errorf("valid servers rejected: %v", errs)
	}
	c.Tiers.Middle.MCPServers["broken"] = map[string]any{"type": "stdio", "args": []any{"x"}}
	errs := c.Validate()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "broken") {
		t.Errorf("a server with neither command nor url passed: %v", errs)
	}
	if Describe("tiers.middle.mcp_servers") == "" || Describe("tiers.middle.mcp_servers.ctx7") == "" {
		t.Error("mcp_servers is undocumented")
	}
}
