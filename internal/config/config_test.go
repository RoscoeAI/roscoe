package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Default / Validate
// ---------------------------------------------------------------------------

func TestDefaultPassesValidate(t *testing.T) {
	c := Default()
	if errs := c.Validate(); len(errs) > 0 {
		t.Fatalf("Default() should validate cleanly, got %d errors: %v", len(errs), errors.Join(errs...))
	}
}

func TestValidateAutonomyBounds(t *testing.T) {
	tests := []struct {
		level   int
		wantErr bool
	}{
		{-1, true},
		{101, true},
		{0, false},
		{100, false},
		{90, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("level=%d", tt.level), func(t *testing.T) {
			c := Default()
			c.Autonomy.Level = tt.level
			errs := c.Validate()
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), "autonomy.level") {
					found = true
				}
			}
			if found != tt.wantErr {
				t.Fatalf("level %d: autonomy error present=%v, want %v (errs: %v)", tt.level, found, tt.wantErr, errs)
			}
		})
	}
}

func TestValidateUnknownProviderRefs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // substring expected in some error
	}{
		{
			name:   "main tier provider",
			mutate: func(c *Config) { c.Tiers.Main.Provider = "nope" },
			want:   `tiers.main.provider: unknown provider "nope"`,
		},
		{
			name:   "middle tier provider",
			mutate: func(c *Config) { c.Tiers.Middle.Provider = "ghost" },
			want:   `tiers.middle.provider: unknown provider "ghost"`,
		},
		{
			name:   "subagent tier provider",
			mutate: func(c *Config) { c.Tiers.Subagents.Provider = "void" },
			want:   `tiers.subagents.provider: unknown provider "void"`,
		},
		{
			name:   "quorum voter provider",
			mutate: func(c *Config) { c.Quorum.Voters[1].Provider = "missing" },
			want:   `quorum.voters.1.provider: unknown provider "missing"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(c)
			errs := c.Validate()
			if !errsContain(errs, tt.want) {
				t.Fatalf("want error containing %q, got: %v", tt.want, errs)
			}
		})
	}
}

func TestValidateUnknownAccountRefs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "main tier account",
			mutate: func(c *Config) { c.Tiers.Main.Account = "ghost" },
			want:   `tiers.main.account: unknown account "ghost"`,
		},
		{
			name:   "middle tier accounts list",
			mutate: func(c *Config) { c.Tiers.Middle.Accounts = []string{"primary", "phantom"} },
			want:   `tiers.middle.accounts.1: unknown account "phantom"`,
		},
		{
			name:   "quorum voter account",
			mutate: func(c *Config) { c.Quorum.Voters[2].Account = "nobody" },
			want:   `quorum.voters.2.account: unknown account "nobody"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(c)
			errs := c.Validate()
			if !errsContain(errs, tt.want) {
				t.Fatalf("want error containing %q, got: %v", tt.want, errs)
			}
		})
	}
}

func TestValidateEmptyAccountRefsAllowed(t *testing.T) {
	// Empty main-tier account and empty voter account are explicitly optional.
	c := Default()
	c.Tiers.Main.Account = ""
	c.Quorum.Voters[2].Account = ""
	if errs := c.Validate(); len(errs) > 0 {
		t.Fatalf("empty optional account refs should be valid, got: %v", errs)
	}
}

func TestValidateRouterPort(t *testing.T) {
	tests := []struct {
		port    int
		wantErr bool
	}{
		{0, true},
		{-1, true},
		{65536, true},
		{1, false},
		{65535, false},
		{8484, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("port=%d", tt.port), func(t *testing.T) {
			c := Default()
			c.Router.Port = tt.port
			errs := c.Validate()
			found := errsContain(errs, "router.port")
			if found != tt.wantErr {
				t.Fatalf("port %d: router.port error present=%v, want %v (errs: %v)", tt.port, found, tt.wantErr, errs)
			}
		})
	}
}

func TestValidateDefaultRoute(t *testing.T) {
	for _, route := range []string{"main", "middle", "subagents"} {
		c := Default()
		c.Router.DefaultRoute = route
		if errs := c.Validate(); len(errs) > 0 {
			t.Errorf("default_route %q should be valid, got: %v", route, errs)
		}
	}
	c := Default()
	c.Router.DefaultRoute = "bogus"
	if !errsContain(c.Validate(), `router.default_route "bogus"`) {
		t.Fatalf("want default_route error, got: %v", c.Validate())
	}
}

func TestValidateEmptyVirtualModel(t *testing.T) {
	c := Default()
	c.Tiers.Subagents.VirtualModel = ""
	if !errsContain(c.Validate(), "tiers.subagents.virtual_model") {
		t.Fatalf("want virtual_model error, got: %v", c.Validate())
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	c := Default()
	c.Autonomy.Level = 200
	c.Router.Port = 0
	c.Tiers.Main.Provider = "nope"
	c.Tiers.Subagents.VirtualModel = ""
	errs := c.Validate()
	if len(errs) != 4 {
		t.Fatalf("want all 4 problems reported, got %d: %v", len(errs), errs)
	}
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	c, err := Load(path)
	if c != nil {
		t.Fatalf("want nil config on missing file, got %+v", c)
	}
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want error wrapping fs.ErrNotExist, got: %v", err)
	}
	if !strings.Contains(err.Error(), "open config") {
		t.Fatalf("want 'open config' context in error, got: %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roscoe.json")
	// A valid default config plus one stray top-level field.
	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["bogus_field"] = true
	data, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if c != nil || err == nil {
		t.Fatalf("want strict-decode failure, got config=%v err=%v", c, err)
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "bogus_field") {
		t.Fatalf("want unknown-field error naming bogus_field, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("want path %q in error, got: %v", path, err)
	}
}

func TestLoadRejectsUnknownNestedField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roscoe.json")
	// Strictness must apply below the top level too.
	if err := os.WriteFile(path, []byte(`{"router":{"bind":"127.0.0.1","port":1,"default_route":"middle","stray":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want nested unknown-field error, got: %v", err)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	// Save does not validate, so it can persist a broken config; Load must
	// refuse it and report every problem.
	c := Default()
	c.Autonomy.Level = 150
	c.Router.Port = 0
	path := filepath.Join(t.TempDir(), "roscoe.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if loaded != nil || err == nil {
		t.Fatalf("want validation failure, got config=%v err=%v", loaded, err)
	}
	for _, want := range []string{"validate", "autonomy.level", "router.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roscoe.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "parse "+path) {
		t.Fatalf("want parse error naming the file, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Save
// ---------------------------------------------------------------------------

func TestSaveLoadRoundTrip(t *testing.T) {
	c := Default()
	// Save into a directory that does not exist yet (MkdirAll path).
	dir := filepath.Join(t.TempDir(), "nested", "cfgdir")
	path := filepath.Join(dir, "roscoe.json")
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !reflect.DeepEqual(c, loaded) {
		t.Fatalf("round-trip mismatch:\nsaved:  %+v\nloaded: %+v", c, loaded)
	}

	// Atomic write hygiene: no temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "roscoe.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("want exactly [roscoe.json] in dir, got %v", names)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %o, want 644", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("saved file should end with a newline")
	}
	if !strings.HasPrefix(string(data), "{\n  ") {
		t.Error("saved file should be indented JSON")
	}
}

func TestSaveOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roscoe.json")

	c := Default()
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	c.Project = "renamed-project"
	if err := c.Save(path); err != nil {
		t.Fatalf("Save over existing file: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project != "renamed-project" {
		t.Fatalf("overwrite did not take: project = %q", loaded.Project)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files leaked: %d entries in dir", len(entries))
	}
}

// ---------------------------------------------------------------------------
// ExpandPath
// ---------------------------------------------------------------------------

func TestExpandPath(t *testing.T) {
	home := "/fake/home"
	t.Setenv("HOME", home)

	tests := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/", home},
		{"~/x", filepath.Join(home, "x")},
		{"~/a/b/c.json", filepath.Join(home, "a/b/c.json")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"~user/x", "~user/x"}, // ~user form passes through unchanged
		{"~x", "~x"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ExpandPath(tt.in); got != tt.want {
				t.Fatalf("ExpandPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpandPathNoHome(t *testing.T) {
	// With HOME empty, os.UserHomeDir fails and the input passes through.
	t.Setenv("HOME", "")
	if got := ExpandPath("~/x"); got != "~/x" {
		t.Fatalf("ExpandPath with no HOME = %q, want passthrough %q", got, "~/x")
	}
	if got := ExpandPath("~"); got != "~" {
		t.Fatalf("ExpandPath(~) with no HOME = %q, want ~", got)
	}
}

// ---------------------------------------------------------------------------
// LoadEnvFile
// ---------------------------------------------------------------------------

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		"# leading comment",
		"",
		"API_KEY=abc123",
		"   # indented comment",
		"URL=https://example.com/x?a=1&b=2",
		"EMBED=a=b=c",
		"  SPACED  =  padded value  ",
		"EMPTYVAL=",
		"\t",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	want := map[string]string{
		"API_KEY":  "abc123",
		"URL":      "https://example.com/x?a=1&b=2", // cut at FIRST '=', rest verbatim
		"EMBED":    "a=b=c",
		"SPACED":   "padded value",
		"EMPTYVAL": "",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env mismatch:\ngot:  %v\nwant: %v", env, want)
	}
}

func TestLoadEnvFileErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string // substring; %d is the 1-based line of the bad line
		line    int
	}{
		{
			name:    "not a KEY=VAL line",
			content: "GOOD=1\nsecond\nTHIRD=3",
			want:    "not a KEY=VAL line",
			line:    2,
		},
		{
			name:    "empty key",
			content: "OK=1\n# c\n=nope",
			want:    "empty key",
			line:    3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			env, err := LoadEnvFile(path)
			if env != nil || err == nil {
				t.Fatalf("want error, got env=%v err=%v", env, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q in error, got: %v", tt.want, err)
			}
			wantLoc := fmt.Sprintf("%s:%d:", path, tt.line)
			if !strings.Contains(err.Error(), wantLoc) {
				t.Fatalf("want file:line prefix %q in error, got: %v", wantLoc, err)
			}
		})
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	env, err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if env != nil || err == nil {
		t.Fatalf("want error for missing env file, got env=%v err=%v", env, err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGet(t *testing.T) {
	c := Default()
	tests := []struct {
		path string
		want any
	}{
		{"project", "roscoe.sh"},
		{"tiers.subagents.model", "zai-org/GLM-5.3-Flash"},
		{"nodes.0.ssh", "roscoe-ts"},
		{"nodes.2.enabled", false},
		{"router.port", json.Number("8484")},
		{"quorum.min_confidence", json.Number("0.7")},
		{"providers.deepinfra.pricing_per_mtok.input", json.Number("0.075")},
		{"tiers.middle.allowed_tools.1", "Task"},
		{"quorum.voters.2.account", "secondary"},
		{"memory.enabled", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := c.Get(tt.path)
			if err != nil {
				t.Fatalf("Get(%q): %v", tt.path, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Get(%q) = %v (%T), want %v (%T)", tt.path, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestGetNumberRendersExactly(t *testing.T) {
	// json.Number must print the literal as stored, not float-formatted.
	c := Default()
	got, err := c.Get("limits.run_budget_usd")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := got.(json.Number)
	if !ok {
		t.Fatalf("want json.Number, got %T", got)
	}
	if n.String() != "50" {
		t.Fatalf("run_budget_usd rendered %q, want \"50\"", n.String())
	}
}

func TestGetContainerValues(t *testing.T) {
	// A path landing on an object or array returns the container itself.
	c := Default()
	got, err := c.Get("autonomy")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Get(autonomy) = %T, want map", got)
	}
	if lvl := m["level"]; !reflect.DeepEqual(lvl, json.Number("90")) {
		t.Fatalf("autonomy.level in container = %v (%T)", lvl, lvl)
	}
	got, err = c.Get("nodes")
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got.([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("Get(nodes) = %T len-check failed: %v", got, got)
	}
}

func TestGetErrors(t *testing.T) {
	c := Default()
	tests := []struct {
		path string
		want string
	}{
		{"", "empty path"},
		{"nope", `no such key "nope"`},
		{"tiers.nope", `no such key "tiers.nope"`},
		{"nodes.abc", `"nodes.abc": array index must be a number`},
		{"nodes.3", `"nodes.3": index 3 out of range (len 3)`},
		{"nodes.-1", "out of range"},
		{"project.sub", `"project.sub": value is not an object or array`},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := c.Get(tt.path)
			if err == nil {
				t.Fatalf("Get(%q) = %v, want error", tt.path, got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Get(%q) error = %v, want substring %q", tt.path, err, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SetPath
// ---------------------------------------------------------------------------

func TestSetPathValidSets(t *testing.T) {
	tests := []struct {
		path  string
		raw   string
		check func(t *testing.T, c *Config)
	}{
		{
			path: "project", raw: "hello world", // not valid JSON -> plain string
			check: func(t *testing.T, c *Config) {
				if c.Project != "hello world" {
					t.Fatalf("project = %q", c.Project)
				}
			},
		},
		{
			path: "project", raw: `"quoted"`, // valid JSON string -> unquoted
			check: func(t *testing.T, c *Config) {
				if c.Project != "quoted" {
					t.Fatalf("project = %q", c.Project)
				}
			},
		},
		{
			path: "router.port", raw: "9090",
			check: func(t *testing.T, c *Config) {
				if c.Router.Port != 9090 {
					t.Fatalf("port = %d", c.Router.Port)
				}
			},
		},
		{
			path: "quorum.min_confidence", raw: "0.9",
			check: func(t *testing.T, c *Config) {
				if c.Quorum.MinConfidence != 0.9 {
					t.Fatalf("min_confidence = %v", c.Quorum.MinConfidence)
				}
			},
		},
		{
			path: "memory.enabled", raw: "false",
			check: func(t *testing.T, c *Config) {
				if c.Memory.Enabled {
					t.Fatal("memory.enabled should be false")
				}
			},
		},
		{
			path: "nodes.1.workers", raw: "5", // array index into struct field
			check: func(t *testing.T, c *Config) {
				if c.Nodes[1].Workers != 5 {
					t.Fatalf("nodes[1].workers = %d", c.Nodes[1].Workers)
				}
			},
		},
		{
			path: "nodes.2", raw: `{"name":"newnode","ssh":"nn-ts","workers":3,"enabled":true}`,
			check: func(t *testing.T, c *Config) {
				want := Node{Name: "newnode", SSH: "nn-ts", Workers: 3, Enabled: true}
				if c.Nodes[2] != want {
					t.Fatalf("nodes[2] = %+v", c.Nodes[2])
				}
			},
		},
		{
			path: "tiers.middle.allowed_tools", raw: `["Read","Bash"]`, // JSON array value
			check: func(t *testing.T, c *Config) {
				if !reflect.DeepEqual(c.Tiers.Middle.AllowedTools, []string{"Read", "Bash"}) {
					t.Fatalf("allowed_tools = %v", c.Tiers.Middle.AllowedTools)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.path+"="+tt.raw, func(t *testing.T) {
			c := Default()
			if err := c.SetPath(tt.path, tt.raw); err != nil {
				t.Fatalf("SetPath(%q, %q): %v", tt.path, tt.raw, err)
			}
			tt.check(t, c)
		})
	}
}

func TestSetPathNewMapKey(t *testing.T) {
	// Map keys may be new: adding a provider must work and survive re-decode.
	c := Default()
	err := c.SetPath("providers.openrouter", `{"protocol":"anthropic","base_url":"https://openrouter.example","auth":"env:OR_KEY"}`)
	if err != nil {
		t.Fatalf("SetPath new provider: %v", err)
	}
	p, ok := c.Providers["openrouter"]
	if !ok {
		t.Fatal("provider openrouter not present after SetPath")
	}
	if p.BaseURL != "https://openrouter.example" || p.Auth != "env:OR_KEY" {
		t.Fatalf("provider content wrong: %+v", p)
	}
}

func TestSetPathRoundTripsThroughSaveLoad(t *testing.T) {
	c := Default()
	if err := c.SetPath("tiers.subagents.model", "my/replacement-model"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPath("router.port", "9191"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "roscoe.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, loaded) {
		t.Fatalf("SetPath result did not round-trip:\nin-mem: %+v\nloaded: %+v", c, loaded)
	}
	if loaded.Tiers.Subagents.Model != "my/replacement-model" || loaded.Router.Port != 9191 {
		t.Fatalf("values lost in round-trip: model=%q port=%d", loaded.Tiers.Subagents.Model, loaded.Router.Port)
	}
}

func TestSetPathTypeErrorLeavesReceiverUnmodified(t *testing.T) {
	tests := []struct {
		name string
		path string
		raw  string
	}{
		{"string into int field", "router.port", "not-a-number"},
		{"quoted string into int field", "autonomy.level", `"banana"`},
		{"object into string field", "project", `{"x":1}`},
		{"float into int field", "router.port", "1.5"},
		{"unknown top-level key", "no_such_field", "1"},
		{"unknown nested struct key", "router.stray", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			err := c.SetPath(tt.path, tt.raw)
			if err == nil {
				t.Fatalf("SetPath(%q, %q): want error, got nil", tt.path, tt.raw)
			}
			if !reflect.DeepEqual(c, Default()) {
				t.Fatalf("receiver modified after failed SetPath(%q, %q)", tt.path, tt.raw)
			}
		})
	}
}

func TestSetPathPathErrors(t *testing.T) {
	tests := []struct {
		path string
		raw  string
		want string
	}{
		{"", "x", "empty path"},
		{"nodes.abc", "x", "array index must be a number"},
		{"nodes.9", "x", "index 9 out of range (len 3)"},
		{"nodes.9.ssh", "x", "out of range"}, // intermediate index checked too
		{"project.sub", "x", "parent is not an object or array"},
		{"tiers.nope.deeper", "x", `no such key "tiers.nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c := Default()
			err := c.SetPath(tt.path, tt.raw)
			if err == nil {
				t.Fatalf("SetPath(%q): want error", tt.path)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("SetPath(%q) error = %v, want substring %q", tt.path, err, tt.want)
			}
			if !reflect.DeepEqual(c, Default()) {
				t.Fatalf("receiver modified after failed SetPath(%q)", tt.path)
			}
		})
	}
}

func TestSetPathMultiValueRawTreatedAsString(t *testing.T) {
	// "1 2" is a valid JSON value followed by more input -> whole raw string.
	c := Default()
	if err := c.SetPath("project", "1 2"); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	if c.Project != "1 2" {
		t.Fatalf("project = %q, want \"1 2\"", c.Project)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func errsContain(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
