package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// undocumented lists fields deliberately absent from roscoe.example.json, with
// the reason. Everything else must appear there: the example is the file people
// copy, and a setting missing from it is a setting nobody finds. Adding a name
// here should be a decision someone made on purpose, not a gap that opened
// because a field shipped without one.
var undocumented = map[string]string{
	"accounts.*.minted_at": "written by `roscoe accounts`, not by hand",
	"accounts.*.enabled":   "defaults to true; the example shows a disabled account instead",
	"tiers.middle.orchestrate": "only useful below ultracode effort, where the " +
		"example does not sit",
	"providers.*.count_tokens":        "an Ollama workaround, shown on the local provider only",
	"providers.*.serve":               "local-engine only",
	"providers.*.pricing_per_mtok":    "priced providers only",
	"tiers.subagents.agents.*.prompt": "optional; defaults to the agent's description",
	"quorum.voters.*.account":         "optional; the example shows one voter with and two without",
}

// This is the test that would have caught lean_context and cache_ttl shipping
// into the binary without ever reaching the config people copy.
func TestExampleConfigDocumentsEveryField(t *testing.T) {
	example := loadExample(t)
	have := jsonPaths(example)

	var missing []string
	for _, want := range structPaths(reflect.TypeOf(Config{}), "") {
		if matchesAny(want, have) || allowlisted(want) {
			continue
		}
		missing = append(missing, want)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("roscoe.example.json never mentions %d setting(s): %s\n"+
			"Add them to the example, or add them to `undocumented` with a reason.",
			len(missing), strings.Join(missing, ", "))
	}
}

// The reverse drift: a field removed from the struct but left in the example
// would make the shipped file fail to load on the binary that ships with it.
func TestExampleConfigLoadsAndValidates(t *testing.T) {
	path := examplePath(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example does not load with the strict decoder: %v", err)
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Errorf("the shipped example does not validate: %v", errs)
	}
}

// Whatever a roscoe writes, the same roscoe must be able to read. Save uses
// MarshalIndent and Load uses a decoder with DisallowUnknownFields, so a field
// whose tags disagree breaks the round trip and nothing else would notice.
func TestDefaultConfigRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roscoe.json")
	want := Default()
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("a config this binary wrote does not load in the same binary: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round trip changed the config:\nwrote %+v\nread  %+v", want, got)
	}
}

// Every path the settings surfaces and the docs describe must survive a round
// trip through Save, or `roscoe config set` writes something unreadable.
func TestEverySettablePathSurvivesSaving(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roscoe.json")
	cfg := Default()

	for _, p := range []string{
		"tiers.middle.effort", "tiers.middle.lean_context", "tiers.middle.cache_ttl",
		"tiers.middle.harness", "tiers.middle.orchestrate", "autonomy.level",
		"memory.enabled", "quorum.enabled",
	} {
		if err := cfg.SetPath(p, settableValue(p)); err != nil {
			t.Fatalf("SetPath(%s): %v", p, err)
		}
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("a config with every knob set does not reload: %v", err)
	}
	if got.Tiers.Middle.Effort != "high" || got.Tiers.Middle.TTL() != "5m" || got.Tiers.Middle.Lean() {
		t.Errorf("knobs did not survive: effort=%q ttl=%q lean=%v",
			got.Tiers.Middle.Effort, got.Tiers.Middle.TTL(), got.Tiers.Middle.Lean())
	}
}

func settableValue(path string) string {
	switch path {
	case "tiers.middle.effort":
		return "high"
	case "tiers.middle.cache_ttl":
		return "5m"
	case "tiers.middle.harness":
		return "codex"
	case "autonomy.level":
		return "50"
	case "tiers.middle.lean_context":
		return "false"
	default:
		return "true"
	}
}

// A config written by a newer roscoe must fail with something a person can act
// on. "unknown field" alone sends people to the config to delete a line that
// is not the problem.
func TestNewerConfigGivesAnActionableError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roscoe.json")
	raw, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["a_setting_from_the_future"] = true
	raw, _ = json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("an unknown field should not load silently")
	}
	msg := err.Error()
	if !strings.Contains(msg, "a_setting_from_the_future") {
		t.Errorf("the error does not name the field: %v", msg)
	}
	for _, want := range []string{"newer", "roscoe"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("the error does not say what to do (%q missing): %v", want, msg)
		}
	}
}

// matchesAny reports whether a struct path matches any concrete JSON path.
// A Go map is a "*" in the struct walk but a real key in JSON, so
// providers.*.auth has to match providers.anthropic.auth.
func matchesAny(pattern string, have map[string]bool) bool {
	if have[pattern] {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	want := strings.Split(pattern, ".")
	for got := range have {
		if segmentsMatch(want, strings.Split(got, ".")) {
			return true
		}
	}
	return false
}

func segmentsMatch(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != got[i] {
			return false
		}
	}
	return true
}

// allowlisted matches a path or anything nested under one, so allowing a
// struct does not require listing each of its fields.
func allowlisted(path string) bool {
	for k := range undocumented {
		if path == k || strings.HasPrefix(path, k+".") {
			return true
		}
	}
	return false
}

func examplePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "roscoe.example.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("roscoe.example.json is missing; the public repo ships it: %v", err)
	}
	return p
}

func loadExample(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("roscoe.example.json is not valid JSON: %v", err)
	}
	return m
}

// structPaths walks the config type and returns every leaf's dotted json path,
// with "*" standing for a slice index or map key. Deriving this from the type
// rather than a marshalled value is the point: an omitempty field left at its
// zero value vanishes from a marshalled instance, which is exactly how
// lean_context and cache_ttl escaped notice.
func structPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			p := name
			if prefix != "" {
				p = prefix + "." + name
			}
			out = append(out, p)
			out = append(out, structPaths(f.Type, p)...)
		}
	case reflect.Slice, reflect.Array:
		if el := t.Elem(); el.Kind() == reflect.Struct || el.Kind() == reflect.Pointer {
			out = append(out, structPaths(el, prefix+".*")...)
		}
	case reflect.Map:
		if el := t.Elem(); el.Kind() == reflect.Struct || el.Kind() == reflect.Pointer {
			out = append(out, structPaths(el, prefix+".*")...)
		}
	}
	return out
}

// jsonPaths walks parsed JSON the same way, so the two sets are comparable.
func jsonPaths(v any) map[string]bool {
	out := map[string]bool{}
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch n := node.(type) {
		case map[string]any:
			for k, child := range n {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out[p] = true
				walk(p, child)
			}
		case []any:
			for _, child := range n {
				walk(prefix+".*", child)
			}
		}
	}
	walk("", v)
	return out
}
