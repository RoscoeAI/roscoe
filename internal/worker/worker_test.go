package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/streamjson"
)

// sniffedVars are the env vars the stub echoes back. Every stub run clears
// them in the test process (t.Setenv to "") so values from the developer's
// real environment can never leak into assertions. Run appends its own
// entries after os.Environ() and exec keeps the last duplicate, so Run's
// additions still win in the child.
var sniffedVars = []string{
	"CLAUDE_CONFIG_DIR",
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"ANTHROPIC_AUTH_TOKEN",
	"API_TIMEOUT_MS",
	"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS",
	"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH",
}

// stubScript stands in for the claude binary. It dumps its argv
// (NUL-separated, so prompts with newlines survive) and selected env vars
// into $ROSCOE_STUB_OUT, then emits stream-json according to
// $ROSCOE_STUB_MODE.
const stubScript = `#!/bin/sh
out="$ROSCOE_STUB_OUT"
if [ -n "$out" ]; then
  : > "$out/args"
  for a in "$@"; do printf '%s\0' "$a" >> "$out/args"; done
  {
    echo "CLAUDE_CONFIG_DIR=$CLAUDE_CONFIG_DIR"
    echo "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL"
    echo "CLAUDE_CODE_SUBAGENT_MODEL=$CLAUDE_CODE_SUBAGENT_MODEL"
    echo "ANTHROPIC_DEFAULT_HAIKU_MODEL=$ANTHROPIC_DEFAULT_HAIKU_MODEL"
    echo "CLAUDE_CODE_OAUTH_TOKEN=$CLAUDE_CODE_OAUTH_TOKEN"
    echo "ANTHROPIC_AUTH_TOKEN=$ANTHROPIC_AUTH_TOKEN"
    echo "API_TIMEOUT_MS=$API_TIMEOUT_MS"
    echo "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS=$CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS"
    echo "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH=$CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"
    echo "STUB_PWD=$(pwd -P)"
  } > "$out/env"
fi
case "$ROSCOE_STUB_MODE" in
noresult)
  echo '{"type":"system","subtype":"init","session_id":"stub-session"}'
  exit 0
  ;;
fail)
  echo "stub exploded" >&2
  exit 3
  ;;
resultfail)
  echo '{"type":"result","result":"partial","session_id":"stub-session","total_cost_usd":0.5,"is_error":true}'
  exit 2
  ;;
hang)
  trap 'exit 130' INT TERM
  sleep 30 >/dev/null 2>&1 &
  wait
  exit 0
  ;;
*)
  echo '{"type":"system","subtype":"init","session_id":"stub-session","model":"stub-model"}'
  echo ""
  echo 'this line is not json'
  echo '{"type":"result","result":"stub done","session_id":"stub-session","total_cost_usd":1.25,"is_error":false}'
  ;;
esac
`

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// stubRun wires up a stub claude binary, a state dir, and a baseline config
// for one Run invocation.
type stubRun struct {
	outDir   string
	stateDir string
	cfg      *config.Config
	opts     Opts
}

func newStubRun(t *testing.T) *stubRun {
	t.Helper()
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "claude-stub")
	if err := os.WriteFile(bin, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROSCOE_STUB_OUT", outDir)
	t.Setenv("ROSCOE_STUB_MODE", "")
	for _, k := range sniffedVars {
		t.Setenv(k, "")
	}

	stateDir := t.TempDir()
	cfg := &config.Config{
		StateDir: stateDir,
		Tiers: config.Tiers{
			Middle: config.MiddleTier{
				Model:               "sonnet-test",
				PermissionMode:      "acceptEdits",
				AllowedTools:        []string{"Read", "Edit", "Bash"},
				MaxBudgetUSDPerTask: 2.5,
				APITimeoutMS:        12345,
			},
			Subagents: config.SubagentTier{
				VirtualModel:  "roscoe/tier3-test",
				MapHaikuAlias: true,
				MaxConcurrent: 8,
				MaxDepth:      2,
				Agents: map[string]config.AgentDef{
					"impl":  {Description: "implements things", Tools: []string{"Read", "Edit"}},
					"scout": {Description: "scouts around", Prompt: "custom scout prompt"},
				},
			},
		},
	}
	return &stubRun{
		outDir:   outDir,
		stateDir: stateDir,
		cfg:      cfg,
		opts: Opts{
			Cfg:        cfg,
			RouterAddr: "127.0.0.1:18484",
			ClaudeBin:  bin,
		},
	}
}

// args returns the argv the stub saw (excluding argv[0]).
func (s *stubRun) args(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.outDir, "args"))
	if err != nil {
		t.Fatalf("read args dump: %v", err)
	}
	parts := strings.Split(string(b), "\x00")
	return parts[:len(parts)-1] // drop empty tail after trailing NUL
}

// env returns the env vars the stub saw, as reported by the dump file.
func (s *stubRun) env(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.outDir, "env"))
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	env := make(map[string]string)
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad env dump line %q", line)
		}
		env[k] = v
	}
	return env
}

func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Fatalf("flag %s has no value in args %q", flag, args)
			}
			return args[i+1]
		}
	}
	t.Fatalf("flag %s not found in args %q", flag, args)
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func wantEnv(t *testing.T, env map[string]string, key, want string) {
	t.Helper()
	if got := env[key]; got != want {
		t.Errorf("child env %s = %q, want %q", key, got, want)
	}
}

func TestRunFreshArgsEnvAndResult(t *testing.T) {
	s := newStubRun(t)
	var events []*streamjson.Event
	s.opts.OnEvent = func(ev *streamjson.Event) { events = append(events, ev) }

	prompt := "do the thing\nacross two lines with 'quotes' and $VARS"
	res, err := Run(context.Background(), Task{ID: "task-1", Prompt: prompt}, s.opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result")
	}
	if res.Result != "stub done" || res.TotalCostUSD != 1.25 || res.IsError {
		t.Errorf("result = %+v, want Result=stub done TotalCostUSD=1.25 IsError=false", res)
	}
	if res.SessionID != "stub-session" {
		t.Errorf("result session_id = %q, want stub-session", res.SessionID)
	}

	// OnEvent sees every parsed line: init, the non-JSON garbage line
	// (tolerated as a typeless event), and the result. Blank lines skipped.
	if len(events) != 3 {
		t.Fatalf("OnEvent saw %d events, want 3", len(events))
	}
	if events[0].Type != "system" || events[0].Subtype != "init" {
		t.Errorf("first event = %s/%s, want system/init", events[0].Type, events[0].Subtype)
	}
	if events[1].Type != "" {
		t.Errorf("garbled line event type = %q, want \"\"", events[1].Type)
	}
	if events[2].Type != "result" {
		t.Errorf("last event type = %q, want result", events[2].Type)
	}

	args := s.args(t)
	if got := argValue(t, args, "-p"); got != prompt {
		t.Errorf("-p = %q, want %q", got, prompt)
	}
	if got := argValue(t, args, "--output-format"); got != "stream-json" {
		t.Errorf("--output-format = %q, want stream-json", got)
	}
	if !hasFlag(args, "--verbose") {
		t.Error("--verbose missing")
	}
	if !hasFlag(args, "--forward-subagent-text") {
		t.Error("--forward-subagent-text missing")
	}
	if got := argValue(t, args, "--permission-mode"); got != "acceptEdits" {
		t.Errorf("--permission-mode = %q, want acceptEdits", got)
	}
	if got := argValue(t, args, "--allowedTools"); got != "Read,Edit,Bash" {
		t.Errorf("--allowedTools = %q, want Read,Edit,Bash", got)
	}
	if got := argValue(t, args, "--max-budget-usd"); got != "2.5" {
		t.Errorf("--max-budget-usd = %q, want 2.5", got)
	}
	if got := argValue(t, args, "--model"); got != "sonnet-test" {
		t.Errorf("--model = %q, want sonnet-test", got)
	}
	sid := argValue(t, args, "--session-id")
	if !uuidV4Re.MatchString(sid) {
		t.Errorf("--session-id %q is not a v4 UUID", sid)
	}
	if hasFlag(args, "--resume") {
		t.Error("--resume present on a fresh run")
	}

	env := s.env(t)
	// No account token: the worker must inherit the operator's own claude
	// config and login. Overriding CLAUDE_CONFIG_DIR here would hand claude an
	// empty credential store and every claude-* call would 401.
	wantEnv(t, env, "CLAUDE_CONFIG_DIR", "")
	wantEnv(t, env, "ANTHROPIC_AUTH_TOKEN", "")
	wantEnv(t, env, "CLAUDE_CODE_OAUTH_TOKEN", "")
	wantEnv(t, env, "ANTHROPIC_BASE_URL", "http://127.0.0.1:18484")
	wantEnv(t, env, "CLAUDE_CODE_SUBAGENT_MODEL", "roscoe/tier3-test")
	wantEnv(t, env, "ANTHROPIC_DEFAULT_HAIKU_MODEL", "roscoe/tier3-test")
	wantEnv(t, env, "API_TIMEOUT_MS", "12345")
	wantEnv(t, env, "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "8")
	wantEnv(t, env, "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH", "2")
}

func TestRunAgentsArg(t *testing.T) {
	s := newStubRun(t)
	if _, err := Run(context.Background(), Task{ID: "t-agents", Prompt: "p"}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw := argValue(t, s.args(t), "--agents")

	var agents map[string]struct {
		Description string   `json:"description"`
		Prompt      string   `json:"prompt"`
		Tools       []string `json:"tools"`
		Model       string   `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &agents); err != nil {
		t.Fatalf("--agents is not valid JSON: %v\n%s", err, raw)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %v, want 2 entries", agents)
	}
	impl, ok := agents["impl"]
	if !ok {
		t.Fatal("agent impl missing")
	}
	if impl.Model != "roscoe/tier3-test" {
		t.Errorf("impl.model = %q, want virtual model roscoe/tier3-test", impl.Model)
	}
	if impl.Prompt != "implements things" {
		t.Errorf("impl.prompt = %q, want defaulted to description", impl.Prompt)
	}
	if len(impl.Tools) != 2 || impl.Tools[0] != "Read" || impl.Tools[1] != "Edit" {
		t.Errorf("impl.tools = %v, want [Read Edit]", impl.Tools)
	}
	scout := agents["scout"]
	if scout.Model != "roscoe/tier3-test" {
		t.Errorf("scout.model = %q, want virtual model", scout.Model)
	}
	if scout.Prompt != "custom scout prompt" {
		t.Errorf("scout.prompt = %q, want the explicit prompt kept", scout.Prompt)
	}
}

func TestRunTaskIDDefaultsToSessionID(t *testing.T) {
	s := newStubRun(t)
	if _, err := Run(context.Background(), Task{Prompt: "p"}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sid := argValue(t, s.args(t), "--session-id")
	if sid == "" {
		t.Fatal("no --session-id on a fresh run")
	}
	// Fleet mode (token present) is what isolates the config dir.
	s2 := newStubRun(t)
	if _, err := Run(context.Background(), Task{ID: "t-iso", Prompt: "p", Token: "sk-test-1"}, s2.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ccfg := filepath.Join(s2.stateDir, "workers", "t-iso", "ccfg")
	if fi, err := os.Stat(ccfg); err != nil || !fi.IsDir() {
		t.Errorf("isolated config dir %s missing: err=%v", ccfg, err)
	}
	wantEnv(t, s2.env(t), "CLAUDE_CONFIG_DIR", ccfg)
}

func TestRunOAuthTokenEnv(t *testing.T) {
	s := newStubRun(t)
	if _, err := Run(context.Background(), Task{ID: "t-token", Prompt: "p", Token: "sk-test-123"}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := s.env(t)
	wantEnv(t, env, "CLAUDE_CODE_OAUTH_TOKEN", "sk-test-123")
	wantEnv(t, env, "ANTHROPIC_AUTH_TOKEN", "") // dummy bearer only when no token
}

func TestRunZeroValuedOptionalEnvSkipped(t *testing.T) {
	s := newStubRun(t)
	s.cfg.Tiers.Subagents.MapHaikuAlias = false
	s.cfg.Tiers.Middle.APITimeoutMS = 0
	s.cfg.Tiers.Subagents.MaxConcurrent = 0
	s.cfg.Tiers.Subagents.MaxDepth = 0
	if _, err := Run(context.Background(), Task{ID: "t-zero", Prompt: "p"}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := s.env(t)
	wantEnv(t, env, "ANTHROPIC_DEFAULT_HAIKU_MODEL", "")
	wantEnv(t, env, "API_TIMEOUT_MS", "")
	wantEnv(t, env, "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS", "")
	wantEnv(t, env, "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH", "")
	// The always-set vars are still there.
	wantEnv(t, env, "CLAUDE_CODE_SUBAGENT_MODEL", "roscoe/tier3-test")
}

func TestRunResumeImportsTranscript(t *testing.T) {
	// HOME is redirected: with no account token the worker runs under the
	// operator's own ~/.claude, and a test must never write into the real one.
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := newStubRun(t)
	src := filepath.Join(t.TempDir(), "projects", "-Users-tim-code-app")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "11111111-2222-4333-8444-555555555555"
	srcFile := filepath.Join(src, sessionID+".jsonl")
	if err := os.WriteFile(srcFile, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), Task{
		ID: sessionID, Prompt: "keep going", Resume: sessionID, ResumeFrom: srcFile,
	}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := s.args(t)
	if got := argValue(t, args, "--resume"); got != sessionID {
		t.Errorf("--resume = %q, want %q", got, sessionID)
	}
	if hasFlag(args, "--session-id") {
		t.Error("--session-id must not accompany --resume")
	}
	// Own-auth mode imports into the operator's config dir.
	dest := filepath.Join(home, ".claude", "projects", "-Users-tim-code-app", sessionID+".jsonl")
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("imported transcript missing: %v", err)
	}
}

func TestRunResumeImportsIntoIsolatedDirWithToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newStubRun(t)
	src := filepath.Join(t.TempDir(), "projects", "-Users-tim-code-app")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "22222222-3333-4444-8555-666666666666"
	srcFile := filepath.Join(src, sessionID+".jsonl")
	if err := os.WriteFile(srcFile, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), Task{
		ID: "iso-resume", Prompt: "keep going", Token: "sk-test-9",
		Resume: sessionID, ResumeFrom: srcFile,
	}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dest := filepath.Join(s.stateDir, "workers", "iso-resume", "ccfg", "projects", "-Users-tim-code-app", sessionID+".jsonl")
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("fleet mode should import into the isolated config dir: %v", err)
	}
}

func TestRunCreatesTaskDir(t *testing.T) {
	s := newStubRun(t)
	taskDir := filepath.Join(t.TempDir(), "nested", "work")
	if _, err := Run(context.Background(), Task{ID: "t-dir", Prompt: "p", Dir: taskDir}, s.opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi, err := os.Stat(taskDir); err != nil || !fi.IsDir() {
		t.Fatalf("task dir %s not created: err=%v", taskDir, err)
	}
	// The child actually ran in it. Compare physical paths (macOS tempdirs
	// are behind a /var -> /private/var symlink).
	wantPwd, err := filepath.EvalSymlinks(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv(t, s.env(t), "STUB_PWD", wantPwd)
}

func TestRunResultReturnedDespiteNonZeroExit(t *testing.T) {
	s := newStubRun(t)
	t.Setenv("ROSCOE_STUB_MODE", "resultfail")
	res, err := Run(context.Background(), Task{ID: "t-rf", Prompt: "p"}, s.opts)
	if err != nil {
		t.Fatalf("Run: %v (a captured result must win over a non-zero exit)", err)
	}
	if res == nil || res.Result != "partial" || !res.IsError || res.TotalCostUSD != 0.5 {
		t.Errorf("result = %+v, want partial/is_error/0.5", res)
	}
}

func TestRunZeroExitWithoutResult(t *testing.T) {
	s := newStubRun(t)
	t.Setenv("ROSCOE_STUB_MODE", "noresult")
	res, err := Run(context.Background(), Task{ID: "t-nr", Prompt: "p"}, s.opts)
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if err == nil || !strings.Contains(err.Error(), "exited 0 without a result event") {
		t.Errorf("err = %v, want 'exited 0 without a result event'", err)
	}
	if err != nil && !strings.Contains(err.Error(), "t-nr") {
		t.Errorf("err %v does not name the task id", err)
	}
}

func TestRunFailureCarriesStderr(t *testing.T) {
	s := newStubRun(t)
	t.Setenv("ROSCOE_STUB_MODE", "fail")
	res, err := Run(context.Background(), Task{ID: "t-fail", Prompt: "p"}, s.opts)
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude failed without result") {
		t.Errorf("err = %q, want 'claude failed without result'", msg)
	}
	if !strings.Contains(msg, "stub exploded") {
		t.Errorf("err = %q, want stderr tail 'stub exploded'", msg)
	}
	if !strings.Contains(msg, "exit status 3") {
		t.Errorf("err = %q, want wrapped 'exit status 3'", msg)
	}
}

func TestRunNilConfig(t *testing.T) {
	res, err := Run(context.Background(), Task{Prompt: "p"}, Opts{})
	if res != nil || err == nil {
		t.Fatalf("Run(nil cfg) = %v, %v; want nil result and error", res, err)
	}
}

func TestRunPreCanceledContext(t *testing.T) {
	s := newStubRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Run(ctx, Task{ID: "t-pc", Prompt: "p"}, s.opts)
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if err == nil || !strings.Contains(err.Error(), "not starting") {
		t.Errorf("err = %v, want 'not starting'", err)
	}
	// No worker state should have been created for a run that never started.
	if _, statErr := os.Stat(filepath.Join(s.stateDir, "workers", "t-pc")); !os.IsNotExist(statErr) {
		t.Errorf("worker dir created for a run that never started (stat err = %v)", statErr)
	}
}

func TestRunCancelInterruptsChild(t *testing.T) {
	s := newStubRun(t)
	t.Setenv("ROSCOE_STUB_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, Task{ID: "t-hang", Prompt: "p"}, s.opts)
	elapsed := time.Since(start)

	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if err == nil || !strings.Contains(err.Error(), "interrupted before result") {
		t.Errorf("err = %v, want 'interrupted before result'", err)
	}
	// SIGINT must have done the job well inside the 10s SIGKILL grace and
	// far inside the child's 30s sleep.
	if elapsed > 8*time.Second {
		t.Errorf("Run took %v after cancel; SIGINT path did not terminate the child promptly", elapsed)
	}
}

func TestBuildAgentsJSON(t *testing.T) {
	cfg := &config.Config{
		Tiers: config.Tiers{
			Subagents: config.SubagentTier{
				VirtualModel: "roscoe/vm",
				Agents: map[string]config.AgentDef{
					"bare":    {Description: "just a description"},
					"full":    {Description: "d", Prompt: "explicit prompt", Tools: []string{"Read"}},
					"ignored": {Description: "model should still be forced", Prompt: "p"},
				},
			},
		},
	}
	got, err := BuildAgentsJSON(cfg)
	if err != nil {
		t.Fatalf("BuildAgentsJSON: %v", err)
	}

	var decoded map[string]struct {
		Description string   `json:"description"`
		Prompt      string   `json:"prompt"`
		Tools       []string `json:"tools"`
		Model       string   `json:"model"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, got)
	}
	if len(decoded) != 3 {
		t.Fatalf("decoded %d agents, want 3", len(decoded))
	}
	for name, a := range decoded {
		if a.Model != "roscoe/vm" {
			t.Errorf("agent %s model = %q, want forced roscoe/vm", name, a.Model)
		}
	}
	if decoded["bare"].Prompt != "just a description" {
		t.Errorf("bare.prompt = %q, want defaulted to description", decoded["bare"].Prompt)
	}
	if decoded["full"].Prompt != "explicit prompt" {
		t.Errorf("full.prompt = %q, want explicit prompt kept", decoded["full"].Prompt)
	}

	// tools is omitempty: absent for the agent without tools.
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(got), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["bare"]["tools"]; ok {
		t.Error("bare has a tools key; want omitted when empty")
	}
	if _, ok := raw["full"]["tools"]; !ok {
		t.Error("full lost its tools key")
	}

	// Deterministic output (json.Marshal sorts map keys).
	again, err := BuildAgentsJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != again {
		t.Errorf("non-deterministic output:\n%s\n%s", got, again)
	}

	// No agents renders as an empty object, still valid for --agents.
	empty, err := BuildAgentsJSON(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "{}" {
		t.Errorf("empty agents = %q, want {}", empty)
	}
}
