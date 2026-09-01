// Package memory is roscoe's cross-run memory: a Graphify knowledge graph the
// fleet queries before an iteration and feeds after one.
//
// The division of labour is by lifetime. loop.md is per-run working memory,
// rewritten every iteration and thrown away with the run. The graph is what
// outlives it, so run 40 does not relearn what run 3 discovered.
//
// Memory is never in the hot path. Every call here is time-boxed and every
// failure degrades to no recall, never to a broken loop: a graph that is
// missing, stale, or slow must cost the fleet nothing but the recall itself.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Outcome is the signal a run sends back about whether recall helped. These
// are Graphify's own vocabulary, and `graphify reflect` weighs them with time
// decay to build cross-run lessons.
type Outcome string

const (
	Useful    Outcome = "useful"
	DeadEnd   Outcome = "dead_end"
	Corrected Outcome = "corrected"
)

// DefaultTimeout bounds any single graphify call. Recall that arrives after
// the worker has started is worth nothing, so it is better to skip it.
const DefaultTimeout = 20 * time.Second

// buildTimeout is longer because extraction reads a whole corpus. It is still
// bounded: an operator-triggered build should not hang a terminal forever.
const buildTimeout = 30 * time.Minute

// Memory is a per-project graph.
type Memory struct {
	// Bin is the graphify executable; empty means "graphify" from PATH.
	Bin string
	// Dir is this project's graph home, e.g. ~/.roscoe/graph/orch-1a2b3c4d.
	Dir string
	// Project is the human-readable project name, for messages.
	Project string
	// Enabled is memory.enabled; false makes every call a no-op.
	Enabled bool
	Timeout time.Duration

	// Runner executes a graphify invocation. Tests replace it; nothing else
	// should need to.
	Runner func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// New derives a project's memory from the config and its working directory.
// Graphs are per project because codebase facts do not transfer between repos
// and one merged graph would poison recall for all of them.
func New(cfg *config.Config, workDir string) *Memory {
	base := config.ExpandPath(cfg.Memory.Path)
	if base == "" {
		base = config.ExpandPath("~/.roscoe/graph")
	}
	name := projectSlug(workDir)
	return &Memory{
		Dir:     filepath.Join(base, name),
		Project: filepath.Base(workDir),
		Enabled: cfg.Memory.Enabled && strings.EqualFold(cfg.Memory.Engine, "graphify"),
	}
}

// projectSlug keeps the directory name readable while a short hash of the
// absolute path keeps two checkouts of the same repo apart.
func projectSlug(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	sum := sha256.Sum256([]byte(abs))
	base := filepath.Base(abs)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "project"
	}
	return base + "-" + hex.EncodeToString(sum[:4])
}

// GraphPath is the graph.json this project queries.
func (m *Memory) GraphPath() string { return filepath.Join(m.Dir, "graphify-out", "graph.json") }

// MemoryDir is where save-result signals accumulate for reflect.
func (m *Memory) MemoryDir() string { return filepath.Join(m.Dir, "graphify-out", "memory") }

// LessonsPath is where reflect writes its cross-run digest.
func (m *Memory) LessonsPath() string { return filepath.Join(m.Dir, "LESSONS.md") }

func (m *Memory) bin() string {
	if m.Bin != "" {
		return m.Bin
	}
	return "graphify"
}

func (m *Memory) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return DefaultTimeout
}

// Installed reports whether the graphify executable can be found.
func (m *Memory) Installed() bool {
	if m.Runner != nil {
		return true
	}
	_, err := exec.LookPath(m.bin())
	return err == nil
}

// HasGraph reports whether this project has a graph to query yet.
func (m *Memory) HasGraph() bool {
	fi, err := os.Stat(m.GraphPath())
	return err == nil && fi.Size() > 0
}

// Ready reports whether a query would do anything. Recall is optional by
// design, so callers check this and carry on when it is false.
func (m *Memory) Ready() bool { return m != nil && m.Enabled && m.Installed() && m.HasGraph() }

// Status describes memory for `roscoe memory status`.
type Status struct {
	Enabled   bool
	Installed bool
	HasGraph  bool
	Dir       string
	Graph     string
	Lessons   string
	Signals   int // saved results awaiting reflect
}

func (m *Memory) Status() Status {
	s := Status{
		Enabled: m.Enabled, Installed: m.Installed(), HasGraph: m.HasGraph(),
		Dir: m.Dir, Graph: m.GraphPath(), Lessons: m.LessonsPath(),
	}
	if entries, err := os.ReadDir(m.MemoryDir()); err == nil {
		s.Signals = len(entries)
	}
	return s
}

func (m *Memory) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if m.Runner != nil {
		return m.Runner(rctx, m.bin(), args...)
	}
	cmd := exec.CommandContext(rctx, m.bin(), args...)
	cmd.Dir = m.Dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("graphify %s: %w: %s", args[0], err, oneLine(detail, 200))
	}
	return out.Bytes(), nil
}

// Recall asks the graph a question and returns what it knows, capped at
// budget tokens. An unusable graph is not an error the caller must handle:
// recall is a bonus, so this returns "" and nil.
func (m *Memory) Recall(ctx context.Context, question string, budget int) (string, error) {
	if !m.Ready() || strings.TrimSpace(question) == "" {
		return "", nil
	}
	if budget <= 0 {
		budget = 1200
	}
	out, err := m.run(ctx, m.timeout(), "query", question,
		"--budget", fmt.Sprint(budget), "--graph", m.GraphPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Record sends back whether recall helped. Graphify's reflect weighs these
// with time decay, which is why roscoe emits the signal rather than building
// its own scoring: the machinery already exists and is deterministic.
func (m *Memory) Record(ctx context.Context, question, answer string, outcome Outcome) error {
	if !m.Enabled || !m.Installed() || strings.TrimSpace(question) == "" {
		return nil
	}
	if err := os.MkdirAll(m.MemoryDir(), 0o755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	args := []string{"save-result", "--question", question, "--memory-dir", m.MemoryDir()}
	if answer != "" {
		args = append(args, "--answer", clip(answer, 4000))
	}
	if outcome != "" {
		args = append(args, "--outcome", string(outcome))
	}
	_, err := m.run(ctx, m.timeout(), args...)
	return err
}

// Reflect distils the accumulated signals into LESSONS.md. It is deterministic
// and needs no model, so it is cheap to run at the end of every loop.
func (m *Memory) Reflect(ctx context.Context) (string, error) {
	if !m.Enabled || !m.Installed() {
		return "", nil
	}
	if _, err := os.Stat(m.MemoryDir()); err != nil {
		return "", nil // nothing recorded yet
	}
	args := []string{"reflect", "--memory-dir", m.MemoryDir(), "--out", m.LessonsPath()}
	if m.HasGraph() {
		args = append(args, "--graph", m.GraphPath())
	}
	if _, err := m.run(ctx, m.timeout(), args...); err != nil {
		return "", err
	}
	return m.LessonsPath(), nil
}

// Build extracts or refreshes the graph from a corpus. Incremental is
// graphify's no-LLM path and is the one worth running often.
func (m *Memory) Build(ctx context.Context, corpus string, incremental bool) error {
	if !m.Enabled {
		return errors.New("memory is disabled (memory.enabled)")
	}
	if !m.Installed() {
		return fmt.Errorf("%s is not on PATH", m.bin())
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return fmt.Errorf("create graph dir: %w", err)
	}
	verb := "extract"
	if incremental {
		verb = "update"
	}
	_, err := m.run(ctx, buildTimeout, verb, corpus)
	return err
}

// BuildEnv is the environment a graph build should run under so its extraction
// goes through roscoe's router onto the cheap tier instead of a top-tier model.
// Graphify's claude backend honours ANTHROPIC_BASE_URL and ANTHROPIC_MODEL.
func BuildEnv(routerAddr, virtualModel string) []string {
	if routerAddr == "" {
		return nil
	}
	return []string{
		"ANTHROPIC_BASE_URL=http://" + routerAddr,
		"ANTHROPIC_MODEL=" + virtualModel,
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
