package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

func TestLearnAndDescribe(t *testing.T) {
	c := Open(t.TempDir())
	c.Learn("anthropic", "sonnet", "claude-sonnet-5")

	if got := c.Resolve("anthropic", "sonnet"); got != "claude-sonnet-5" {
		t.Errorf("resolve = %q", got)
	}
	if got := c.Describe("anthropic", "sonnet"); got != "sonnet  →  claude-sonnet-5" {
		t.Errorf("describe = %q", got)
	}
	// An unknown alias reads back as itself rather than as a gap.
	if got := c.Describe("anthropic", "opus"); got != "opus" {
		t.Errorf("describe of an unlearned alias = %q", got)
	}
	if got := c.Describe("nope", "x"); got != "x" {
		t.Errorf("describe for an unknown provider = %q", got)
	}
}

// A config naming a full model id must not fill the cache with identity
// mappings, or every panel row grows a pointless "x → x".
func TestLearnIgnoresIdentityAndBlanks(t *testing.T) {
	c := Open(t.TempDir())
	c.Learn("p", "claude-sonnet-5", "claude-sonnet-5")
	c.Learn("p", "", "x")
	c.Learn("p", "x", "")
	c.Learn("", "a", "b")
	if got := c.Describe("p", "claude-sonnet-5"); got != "claude-sonnet-5" {
		t.Errorf("describe = %q, want no arrow", got)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPersists(t *testing.T) {
	dir := t.TempDir()
	c := Open(dir)
	c.Learn("anthropic", "opus", "claude-opus-5")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Open(dir).Resolve("anthropic", "opus"); got != "claude-opus-5" {
		t.Errorf("after reload, resolve = %q", got)
	}
	// A corrupt cache must not stop roscoe starting; it is a convenience.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := Open(dir); c == nil || len(c.Providers) != 0 {
		t.Error("a corrupt catalogue should open empty rather than fail")
	}
}

func TestSaveOnlyWritesWhenChanged(t *testing.T) {
	dir := t.TempDir()
	c := Open(dir)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Error("saving an unchanged empty catalogue wrote a file")
	}
}

func TestRefreshReadsAPublishedList(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "claude-opus-5"}, {"id": "claude-sonnet-5"}},
		})
	}))
	defer srv.Close()

	c := Open(t.TempDir())
	n, err := c.Refresh(context.Background(), "anthropic",
		config.Provider{BaseURL: srv.URL}, "Bearer k")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("fetched %d models, want 2", n)
	}
	if gotAuth != "Bearer k" || gotPath != "/v1/models" {
		t.Errorf("auth=%q path=%q", gotAuth, gotPath)
	}
	if got := c.Models("anthropic"); len(got) != 2 || got[0] != "claude-opus-5" {
		t.Errorf("models = %v, want them sorted", got)
	}
}

// DeepInfra serves its list from the OpenAI surface one level up from the
// /anthropic base roscoe actually talks to. That is not a fallback, it is
// where the answer lives.
func TestRefreshFindsTheOpenAISurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/openai/models" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "zai-org/GLM-5.3-Flash"}},
		})
	}))
	defer srv.Close()

	c := Open(t.TempDir())
	n, err := c.Refresh(context.Background(), "deepinfra",
		config.Provider{BaseURL: srv.URL + "/anthropic"}, "Bearer k")
	if err != nil {
		t.Fatalf("did not find the list one level up: %v", err)
	}
	if n != 1 || c.Models("deepinfra")[0] != "zai-org/GLM-5.3-Flash" {
		t.Errorf("models = %v", c.Models("deepinfra"))
	}
}

// A provider that publishes nothing is not an error worth failing on: its
// aliases still get learned from runs.
func TestRefreshOnAProviderWithNoList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := Open(t.TempDir())
	if _, err := c.Refresh(context.Background(), "p", config.Provider{BaseURL: srv.URL}, ""); err == nil {
		t.Error("a provider with no list should say so")
	}
	if got := c.Models("p"); len(got) != 0 {
		t.Errorf("models = %v, want none", got)
	}
}

// The source that needs no credential: what past runs actually resolved.
func TestLearnFromRuns(t *testing.T) {
	state := t.TempDir()
	runs := filepath.Join(state, "runs", "task-1")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"event":{"type":"system","subtype":"init","model":"claude-sonnet-5"}}`,
		`{"event":{"type":"assistant"}}`,
		`{"kind":"loop.iteration"}`,
		`not json at all`,
	}
	if err := os.WriteFile(filepath.Join(runs, "events.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := Open(state)
	n := c.LearnFromRuns(filepath.Join(state, "runs"), "anthropic", "sonnet")
	if n != 1 {
		t.Errorf("learned %d, want 1", n)
	}
	if got := c.Resolve("anthropic", "sonnet"); got != "claude-sonnet-5" {
		t.Errorf("resolve = %q", got)
	}
	// Idempotent: a second pass over the same ledgers learns nothing new.
	if n := c.LearnFromRuns(filepath.Join(state, "runs"), "anthropic", "sonnet"); n != 0 {
		t.Errorf("re-learned %d from the same runs", n)
	}
	// No runs directory at all is fine.
	if n := c.LearnFromRuns(filepath.Join(state, "nope"), "anthropic", "sonnet"); n != 0 {
		t.Errorf("learned %d from a missing directory", n)
	}
}

func TestModelsUnionsListAndAliases(t *testing.T) {
	c := Open(t.TempDir())
	c.Providers["p"] = &Provider{Models: []string{"b", "a"}}
	c.Learn("p", "alias", "c")
	got := c.Models("p")
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("models = %v, want the list and the resolved alias, sorted", got)
	}
	if got := c.Models("unknown"); got != nil {
		t.Errorf("models of an unknown provider = %v", got)
	}
}
