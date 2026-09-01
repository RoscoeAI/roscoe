package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// recorder is a fake graphify that records how it was called, so the whole
// package is testable without the real binary or a network.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
	out   string
	err   error
	delay time.Duration
}

func (r *recorder) runner(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	return []byte(r.out), r.err
}

func (r *recorder) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// withGraph builds a Memory that has a graph on disk and a fake runner.
func withGraph(t *testing.T, r *recorder) *Memory {
	t.Helper()
	dir := t.TempDir()
	graph := filepath.Join(dir, "graphify-out", "graph.json")
	if err := os.MkdirAll(filepath.Dir(graph), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, []byte(`{"nodes":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Memory{Dir: dir, Project: "p", Enabled: true, Runner: r.runner}
}

func TestRecallQueriesThisProjectsGraph(t *testing.T) {
	r := &recorder{out: "  the auth module is in internal/auth  \n"}
	m := withGraph(t, r)
	got, err := m.Recall(context.Background(), "how does auth work", 900)
	if err != nil {
		t.Fatal(err)
	}
	if got != "the auth module is in internal/auth" {
		t.Errorf("recall = %q", got)
	}
	args := r.last()
	if args[0] != "query" || args[1] != "how does auth work" {
		t.Errorf("called graphify %v", args)
	}
	if argAfter(args, "--budget") != "900" {
		t.Errorf("budget not passed: %v", args)
	}
	if argAfter(args, "--graph") != m.GraphPath() {
		t.Errorf("queried %q, want this project's graph", argAfter(args, "--graph"))
	}
}

// Recall is a bonus, never a dependency: none of these may become an error the
// loop has to handle.
func TestRecallIsSilentWhenUnusable(t *testing.T) {
	cases := map[string]*Memory{
		"disabled":  {Dir: t.TempDir(), Enabled: false, Runner: (&recorder{}).runner},
		"no graph":  {Dir: t.TempDir(), Enabled: true, Runner: (&recorder{}).runner},
		"nil memry": nil,
	}
	for name, m := range cases {
		if m == nil {
			continue // Ready() on a nil receiver is covered below
		}
		got, err := m.Recall(context.Background(), "q", 0)
		if err != nil || got != "" {
			t.Errorf("%s: got (%q, %v), want ('', nil)", name, got, err)
		}
	}
	var nilMem *Memory
	if nilMem.Ready() {
		t.Error("a nil Memory reported ready")
	}
}

func TestRecallEmptyQuestionDoesNotCall(t *testing.T) {
	r := &recorder{out: "x"}
	m := withGraph(t, r)
	if _, err := m.Recall(context.Background(), "   ", 0); err != nil {
		t.Fatal(err)
	}
	if r.count() != 0 {
		t.Errorf("called graphify %d times for an empty question", r.count())
	}
}

// A slow graph must not hold up an iteration; recall that arrives after the
// worker started is worth nothing.
func TestRecallIsTimeBoxed(t *testing.T) {
	r := &recorder{delay: 2 * time.Second, out: "late"}
	m := withGraph(t, r)
	m.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := m.Recall(context.Background(), "q", 0)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("recall took %s; the timeout did not bite", elapsed)
	}
	if err == nil {
		t.Error("a timed-out recall should report the error to its caller")
	}
}

func TestRecordSendsTheOutcome(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	if err := m.Record(context.Background(), "the charter", "what it recalled", DeadEnd); err != nil {
		t.Fatal(err)
	}
	args := r.last()
	if args[0] != "save-result" {
		t.Fatalf("called %v", args)
	}
	if argAfter(args, "--outcome") != "dead_end" {
		t.Errorf("outcome = %q", argAfter(args, "--outcome"))
	}
	if argAfter(args, "--question") != "the charter" {
		t.Errorf("question = %q", argAfter(args, "--question"))
	}
	if argAfter(args, "--memory-dir") != m.MemoryDir() {
		t.Errorf("memory-dir = %q", argAfter(args, "--memory-dir"))
	}
	if _, err := os.Stat(m.MemoryDir()); err != nil {
		t.Errorf("the memory dir was not created: %v", err)
	}
}

func TestRecordNoopsWhenDisabled(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	m.Enabled = false
	if err := m.Record(context.Background(), "q", "a", Useful); err != nil {
		t.Fatal(err)
	}
	if r.count() != 0 {
		t.Errorf("recorded a signal while disabled")
	}
}

// Signals are worth sending even before a graph exists: they are what makes
// the first graph worth building.
func TestRecordWorksWithoutAGraph(t *testing.T) {
	r := &recorder{}
	m := &Memory{Dir: t.TempDir(), Enabled: true, Runner: r.runner}
	if err := m.Record(context.Background(), "q", "a", Useful); err != nil {
		t.Fatal(err)
	}
	if r.count() != 1 {
		t.Errorf("made %d calls, want 1", r.count())
	}
}

func TestReflectWritesLessons(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	if err := os.MkdirAll(m.MemoryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := m.Reflect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != m.LessonsPath() {
		t.Errorf("lessons path = %q", path)
	}
	args := r.last()
	if args[0] != "reflect" || argAfter(args, "--out") != m.LessonsPath() {
		t.Errorf("called %v", args)
	}
	if !hasArg(args, "--graph") {
		t.Error("reflect should pass the graph when one exists, for community grouping")
	}
}

func TestReflectSkipsWithNothingRecorded(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	path, err := m.Reflect(context.Background())
	if err != nil || path != "" {
		t.Errorf("got (%q, %v), want ('', nil) with no signals", path, err)
	}
	if r.count() != 0 {
		t.Error("reflect called graphify with nothing to reflect on")
	}
}

func TestBuildIncrementalByDefault(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	if err := m.Build(context.Background(), "/some/corpus", true); err != nil {
		t.Fatal(err)
	}
	if args := r.last(); args[0] != "update" || args[1] != "/some/corpus" {
		t.Errorf("incremental build called %v, want update", args)
	}
	if err := m.Build(context.Background(), "/some/corpus", false); err != nil {
		t.Fatal(err)
	}
	if args := r.last(); args[0] != "extract" {
		t.Errorf("full build called %v, want extract", args)
	}
}

// Build is the one operation an operator triggers, so unlike recall it must
// say why it cannot run.
func TestBuildReportsWhyItCannotRun(t *testing.T) {
	m := &Memory{Dir: t.TempDir(), Enabled: false, Runner: (&recorder{}).runner}
	if err := m.Build(context.Background(), ".", true); err == nil {
		t.Error("a disabled build should explain itself")
	}
	m = &Memory{Dir: t.TempDir(), Enabled: true, Bin: "definitely-not-installed-xyz"}
	if err := m.Build(context.Background(), ".", true); err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Errorf("err = %v, want it to name the missing binary", err)
	}
}

func TestRunnerErrorsSurface(t *testing.T) {
	r := &recorder{err: errors.New("graphify blew up")}
	m := withGraph(t, r)
	if _, err := m.Recall(context.Background(), "q", 0); err == nil {
		t.Error("a failing runner should surface its error to the caller")
	}
}

// Two checkouts of the same repo must not share a graph, and the directory
// should still be readable by a human.
func TestProjectSlugIsStableAndDistinct(t *testing.T) {
	a := projectSlug("/Users/x/Projects/orch")
	b := projectSlug("/Users/x/Projects/orch")
	c := projectSlug("/Users/x/other/orch")
	if a != b {
		t.Errorf("slug is not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Error("two different checkouts of the same repo name collided")
	}
	if !strings.HasPrefix(a, "orch-") {
		t.Errorf("slug %q should stay readable", a)
	}
}

func TestNewDerivesPathsFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Memory.Path = t.TempDir()
	m := New(cfg, "/tmp/someproject")
	if !strings.HasPrefix(m.Dir, cfg.Memory.Path) {
		t.Errorf("graph dir %q is not under memory.path", m.Dir)
	}
	if !strings.HasSuffix(m.GraphPath(), filepath.Join("graphify-out", "graph.json")) {
		t.Errorf("graph path = %q", m.GraphPath())
	}
	if !m.Enabled {
		t.Error("memory should be enabled for the default config")
	}

	cfg.Memory.Engine = "something-else"
	if New(cfg, "/tmp/p").Enabled {
		t.Error("an unknown engine should not enable graphify")
	}
	cfg.Memory.Engine = "graphify"
	cfg.Memory.Enabled = false
	if New(cfg, "/tmp/p").Enabled {
		t.Error("memory.enabled false should win")
	}
}

// Graph extraction should run on the cheap tier through roscoe's own router,
// which graphify supports natively via these two variables.
func TestBuildEnvPointsAtTheRouter(t *testing.T) {
	env := BuildEnv("127.0.0.1:8484", "roscoe/tier3")
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://127.0.0.1:8484") {
		t.Errorf("env = %v", env)
	}
	if !strings.Contains(joined, "ANTHROPIC_MODEL=roscoe/tier3") {
		t.Errorf("env = %v", env)
	}
	if BuildEnv("", "m") != nil {
		t.Error("no router means no override")
	}
}

func TestStatusReportsWhatIsMissing(t *testing.T) {
	r := &recorder{}
	m := withGraph(t, r)
	s := m.Status()
	if !s.Enabled || !s.Installed || !s.HasGraph {
		t.Errorf("status = %+v, want all three true", s)
	}
	if s.Signals != 0 {
		t.Errorf("signals = %d", s.Signals)
	}
	if err := os.MkdirAll(m.MemoryDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.MemoryDir(), "a.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.Signals != 1 {
		t.Errorf("signals = %d, want 1", s.Signals)
	}

	empty := &Memory{Dir: t.TempDir(), Enabled: true, Bin: "definitely-not-installed-xyz"}
	if s := empty.Status(); s.Installed || s.HasGraph {
		t.Errorf("status = %+v, want installed and hasGraph false", s)
	}
}

// A zero-byte graph.json is what a killed build leaves behind, and querying it
// would fail every iteration.
func TestEmptyGraphFileIsNotAGraph(t *testing.T) {
	dir := t.TempDir()
	graph := filepath.Join(dir, "graphify-out", "graph.json")
	if err := os.MkdirAll(filepath.Dir(graph), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Memory{Dir: dir, Enabled: true, Runner: (&recorder{}).runner}
	if m.HasGraph() || m.Ready() {
		t.Error("a zero-byte graph.json was treated as a usable graph")
	}
}
