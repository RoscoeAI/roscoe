package config

import (
	"fmt"
	"reflect"
	"testing"
)

// Every documented choice, format and effect must name a setting that
// exists, and every leaf with a choice list must offer its default as one:
// otherwise the card lies about what can be set.
func TestDocMetadataNamesRealPaths(t *testing.T) {
	cfg := Default()
	// Defaults omit optional fields on save, so the example config, which the
	// compat tests hold to cover every field, is the complete path set.
	all := jsonPaths(loadExample(t))
	for _, p := range structPaths(reflect.TypeOf(Config{}), "") {
		all[p] = true // wildcard forms, straight from the struct
	}
	var walk func(prefix string)
	walk = func(prefix string) {
		for _, k := range cfg.ChildPaths(prefix) {
			all[k] = true
			walk(k)
		}
	}
	walk("")
	exists := func(pattern string) bool {
		if all[pattern] {
			return true
		}
		probe := map[string]bool{pattern: true}
		for p := range all {
			if resolveKey(p, probe) == pattern {
				return true
			}
		}
		return false
	}
	for k := range choices {
		if !exists(k) {
			t.Errorf("choices names %q, which is not a setting", k)
		}
	}
	for k := range formats {
		if !exists(k) {
			t.Errorf("formats names %q, which is not a setting", k)
		}
	}
	for k := range effects {
		if !exists(k) {
			t.Errorf("effects names %q, which is not a setting", k)
		}
		if Describe(k) == "" {
			t.Errorf("effects names %q, which has no description", k)
		}
	}
	for p := range all {
		d := DocFor(p)
		if len(d.Choices) == 0 {
			continue
		}
		v, err := cfg.Get(p)
		if err != nil {
			continue
		}
		got := fmt.Sprintf("%v", v)
		if got == "" {
			continue
		}
		found := false
		for _, c := range d.Choices {
			if c == got {
				found = true
			}
		}
		if !found {
			t.Errorf("%s defaults to %q, which its choices %v do not include", p, got, d.Choices)
		}
	}
}

// The effort card is the one people open most: all three parts, and the
// wildcard forms resolve for list items and named entries alike.
func TestDocForResolves(t *testing.T) {
	d := DocFor("tiers.middle.effort")
	if d.What == "" || len(d.Choices) != len(EffortLevels()) || d.Effect == "" {
		t.Errorf("effort doc = %+v", d)
	}
	if d := DocFor("accounts.0.kind"); len(d.Choices) != 2 || d.What == "" {
		t.Errorf("accounts.0.kind doc = %+v", d)
	}
	if DocFor("nodes.1.ssh").Format == "" {
		t.Error("nodes.*.ssh has no format")
	}
	if DocFor("providers.deepinfra.serve.engine").Choices == nil {
		t.Error("providers.*.serve.engine choices did not resolve through a named provider")
	}
	if got := DocFor("no.such.path"); got.What != "" || got.Choices != nil || got.Format != "" || got.Effect != "" {
		t.Errorf("an unknown path got a doc: %+v", got)
	}
}

// Ordering must never lose a key, and the top level must lead with what
// people change and end with setup.
func TestOrderedKeepsEveryChildOnce(t *testing.T) {
	cfg := Default()
	for _, prefix := range []string{"", "tiers", "tiers.middle", "tiers.subagents", "providers", "quorum"} {
		kids := cfg.ChildPaths(prefix)
		got := Ordered(prefix, kids)
		if len(got) != len(kids) {
			t.Fatalf("%q: ordered %d of %d children", prefix, len(got), len(kids))
		}
		seen := map[string]bool{}
		for _, k := range got {
			if seen[k] {
				t.Errorf("%q: %s listed twice", prefix, k)
			}
			seen[k] = true
		}
		for _, k := range kids {
			if !seen[k] {
				t.Errorf("%q: %s dropped", prefix, k)
			}
		}
	}
	top := Ordered("", cfg.ChildPaths(""))
	if top[0] != "tiers" || top[len(top)-1] != "version" {
		t.Errorf("top order = %v", top)
	}
	rareSeen := false
	for _, k := range top {
		if Rare(k) {
			rareSeen = true
		} else if rareSeen {
			t.Errorf("knob %s listed after setup keys", k)
		}
	}
}
