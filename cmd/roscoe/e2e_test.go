package main

// End-to-end tests over the real binary. Every subcommand is exercised the
// way an operator would hit it: exec'd with arguments, an isolated HOME, a
// working directory with no roscoe.json in it, and a PATH whose only
// "claude", "security" and "ssh" are fakes that record what was asked of
// them. Nothing here reaches the network or the real keychain.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var roscoeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "roscoe-e2e")
	if err != nil {
		panic(err)
	}
	roscoeBin = filepath.Join(dir, "roscoe")
	build := exec.Command("go", "build", "-ldflags", "-X main.Version=v0.0.0-e2e", "-o", roscoeBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic("build roscoe for e2e: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// world is one isolated operator: a home, an empty cwd, fake tools.
type world struct {
	t      *testing.T
	home   string
	cwd    string
	bin    string
	kcDir  string // the fake keychain's store
	sshLog string // touched whenever the fake ssh is called
	// claudeLog gets one line per fake claude start (its argv); claudeMode
	// "fail" makes the fake die like a refused request.
	claudeLog  string
	claudeMode string
	warmDelay  string // seconds the fake waits before its first assistant event
	resultFile string // when set, the fake's result text is this file's content
	cfgPath    string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	// macOS hands out /var/... which is really /private/var/...; the binary
	// reports the resolved form from os.Getwd, so compare against that.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	w := &world{
		t:      t,
		home:   filepath.Join(root, "home"),
		cwd:    filepath.Join(root, "cwd"),
		bin:    filepath.Join(root, "bin"),
		kcDir:  filepath.Join(root, "kc"),
		sshLog: filepath.Join(root, "ssh-called"),
	}
	w.claudeLog = filepath.Join(root, "claude-argv")
	for _, d := range []string{w.home, w.cwd, w.bin, w.kcDir, filepath.Join(w.home, ".roscoe")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.fake("claude", `#!/bin/sh
# A stand-in worker. It records every argv line it was started with, then
# speaks stream-json the way claude -p --output-format stream-json does: an
# init, one assistant turn, a result with usage. FAKE_CLAUDE_MODE=fail makes
# it die the way a refused request does.
# One line per start, however many lines the prompt has.
printf '%s ' "$@" | tr '\n' ' ' >> "$FAKE_CLAUDE_LOG"; echo >> "$FAKE_CLAUDE_LOG"
case "$1" in
  --version) echo "2.1.259 (Claude Code)"; exit 0;;
esac
if [ "$FAKE_CLAUDE_MODE" = "fail" ]; then
  echo '{"type":"error","error":{"type":"invalid_request_error","message":"fake"}}' >&2
  exit 1
fi
prompt=""; sid="fake-session"; model="claude-sonnet-5"; last=""; resumed=""
for a in "$@"; do
  case "$last" in
    -p) prompt="$a";;
    --session-id) sid="$a";;
    --resume) sid="$a"; resumed=1;;
  esac
  last="$a"
done
# toolong: the first resumed turn is refused for size, the way the API
# refuses an oversized window; every turn after that succeeds.
if [ "$FAKE_CLAUDE_MODE" = "toolong" ] && [ -n "$resumed" ] && [ ! -f "$FAKE_CLAUDE_LOG.toolong" ]; then
  : > "$FAKE_CLAUDE_LOG.toolong"
  printf '{"type":"system","subtype":"init","session_id":"%s","model":"%s"}\n' "$sid" "$model"
  printf '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Prompt is too long","session_id":"%s","total_cost_usd":0,"num_turns":0}\n' "$sid"
  exit 0
fi
# JSON-safe echo of the prompt: first line, no quotes or backslashes.
short=$(printf '%s' "$prompt" | head -n1 | tr -d '"\\')
answer="fake answer to: $short"
# FAKE_CLAUDE_RESULT_FILE: the result text comes from this file, one JSON
# string (newlines escaped), so a test can hand the worker a loop block.
if [ -n "$FAKE_CLAUDE_RESULT_FILE" ] && [ -f "$FAKE_CLAUDE_RESULT_FILE" ]; then
  answer=$(tr -d '"\\' < "$FAKE_CLAUDE_RESULT_FILE" | awk '{printf "%s\\n", $0}')
fi
echo "start $short" >> "$FAKE_CLAUDE_EVENTS"
printf '{"type":"system","subtype":"init","session_id":"%s","model":"%s","tools":["Read","Bash"]}\n' "$sid" "$model"
[ -n "$FAKE_CLAUDE_WARM_DELAY" ] && sleep "$FAKE_CLAUDE_WARM_DELAY"
echo "assistant $short" >> "$FAKE_CLAUDE_EVENTS"
printf '{"type":"assistant","message":{"role":"assistant","model":"%s","content":[{"type":"text","text":"%s"}],"usage":{"input_tokens":1200,"cache_creation_input_tokens":0,"cache_read_input_tokens":40000,"output_tokens":30}},"session_id":"%s"}\n' "$model" "$answer" "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","session_id":"%s","total_cost_usd":0.0123,"num_turns":1,"duration_ms":420,"usage":{"input_tokens":1200,"cache_creation_input_tokens":0,"cache_read_input_tokens":40000,"output_tokens":30},"modelUsage":{"%s":{"inputTokens":1200,"outputTokens":30,"cacheReadInputTokens":40000,"cacheCreationInputTokens":0,"costUSD":0.0123,"contextWindow":200000}}}\n' "$answer" "$sid" "$model"
exit 0
`)
	w.fake("ssh", `#!/bin/sh
echo "$@" >> "$ROSCOE_E2E_SSH_LOG"
echo "fake ssh: nothing should reach a remote machine in tests" >&2
exit 255
`)
	// The fake security tool keeps one file per service under $FAKE_KC.
	// Exit codes follow the real one: 44 not found.
	w.fake("security", `#!/bin/sh
sub="$1"; shift
svc=""; want_w=0; last=""
for a in "$@"; do
  if [ "$last" = "-s" ]; then svc="$a"; fi
  last="$a"
done
case "$sub" in
  find-generic-password)
    [ -f "$FAKE_KC/$svc" ] || { echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2; exit 44; }
    case " $* " in *" -w "*) cat "$FAKE_KC/$svc";; esac
    exit 0;;
  add-generic-password)
    if [ "$last" = "-w" ]; then
      printf 'password data for new item: ' >&2
      IFS= read -r secret
      printf '%s' "$secret" > "$FAKE_KC/$svc"
    else
      # No trailing -w: the real tool stores an empty password silently.
      : > "$FAKE_KC/$svc"
    fi
    exit 0;;
  delete-generic-password)
    rm -f "$FAKE_KC/$svc"; exit 0;;
esac
echo "fake security: unhandled $sub" >&2; exit 1
`)
	w.cfgPath = filepath.Join(w.home, ".roscoe", "roscoe.json")
	return w
}

func (w *world) fake(name, script string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.bin, name), []byte(script), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// init writes the default config where the fallback lookup finds it.
func (w *world) init() {
	w.t.Helper()
	r := w.run("", "--config", w.cfgPath, "init")
	if r.code != 0 {
		w.t.Fatalf("init: %s", r)
	}
}

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return "exit " + itoa(r.code) + "\n--- stdout ---\n" + r.stdout + "\n--- stderr ---\n" + r.stderr
}

func (r result) all() string { return r.stdout + r.stderr }

func itoa(n int) string { return strconv.Itoa(n) }

// run executes the binary with stdin as a pipe (never a terminal), so every
// "needs a terminal" gate is the one under test.
func (w *world) run(stdin string, args ...string) result {
	w.t.Helper()
	cmd := exec.Command(roscoeBin, args...)
	cmd.Dir = w.cwd
	cmd.Env = w.env()
	if runtime.GOOS == "windows" {
		w.t.Skip("posix shell fakes")
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatalf("exec %v: %v", args, err)
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (w *world) sshCalled() bool {
	_, err := os.Stat(w.sshLog)
	return err == nil
}

func expect(t *testing.T, r result, code int, needles ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit %d, want %d\n%s", r.code, code, r)
	}
	for _, n := range needles {
		if !strings.Contains(r.all(), n) {
			t.Errorf("output lacks %q\n%s", n, r)
		}
	}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

// --- the surface -----------------------------------------------------------

func TestE2ENoArgsPrintsUsage(t *testing.T) {
	w := newWorld(t)
	expect(t, w.run(""), 2, "usage: roscoe [--config <path>] <command>")
	expect(t, w.run("", "bogus"), 2, `unknown command "bogus"`)
}

// Every command main dispatches must be in the usage text, or an operator
// has no way to find it. Read the dispatch from source so the two cannot
// drift apart unnoticed.
func TestE2EUsageNamesEveryCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	cases := regexp.MustCompile(`(?m)^\tcase "([a-z]+)":`).FindAllStringSubmatch(string(src), -1)
	if len(cases) < 20 {
		t.Fatalf("found only %d dispatched commands in main.go", len(cases))
	}
	usage := newWorld(t).run("").stderr
	for _, c := range cases {
		if !regexp.MustCompile(`(?m)^  ` + c[1] + `\b`).MatchString(usage) {
			t.Errorf("command %q is dispatched but not in usage", c[1])
		}
	}
}

func TestE2EVersion(t *testing.T) {
	w := newWorld(t)
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		expect(t, w.run("", args...), 0, "roscoe v0.0.0-e2e")
	}
}

func TestE2EInit(t *testing.T) {
	w := newWorld(t)
	r := w.run("", "init") // no --config: writes ./roscoe.json in cwd
	expect(t, r, 0, "wrote roscoe.json")
	if _, err := os.Stat(filepath.Join(w.cwd, "roscoe.json")); err != nil {
		t.Fatal("init did not write roscoe.json in the working directory")
	}
	expect(t, w.run("", "init"), 1, "already exists; refusing to overwrite")
	// The fresh file loads and validates as-is.
	expect(t, w.run("", "config", "get", "tiers.middle.model"), 0, "sonnet")
}

func TestE2ENoConfigSaysHowToGetOne(t *testing.T) {
	w := newWorld(t)
	for _, args := range [][]string{{"config"}, {"accounts"}, {"sessions"}, {"models"}, {"top", "--no-fleet"}, {"smoke"}} {
		expect(t, w.run("", args...), 1, `run "roscoe init"`)
	}
	// An explicit path that does not exist says so, naming the path.
	expect(t, w.run("", "--config", "/nope/roscoe.json", "config"), 1, "/nope/roscoe.json")
}

func TestE2EConfig(t *testing.T) {
	w := newWorld(t)
	w.init() // lands in ~/.roscoe, the fallback location

	expect(t, w.run("", "config"), 0, "tiers", "autonomy", "roscoe config show <path>")
	expect(t, w.run("", "config", "get", "tiers.middle.model"), 0, "sonnet")
	expect(t, w.run("", "config", "get", "no.such.path"), 1, "no.such.path")
	expect(t, w.run("", "config", "show"), 2, "usage: roscoe config")
	expect(t, w.run("", "config", "show", "tiers.middle.effort"), 0, "low", "ultracode")
	expect(t, w.run("", "config", "show", "tiers"), 0, "middle")

	expect(t, w.run("", "config", "set", "tiers.middle.effort", "high"), 0)
	expect(t, w.run("", "config", "get", "tiers.middle.effort"), 0, "high")
	// A bad value is refused and the file is left as it was.
	r := w.run("", "config", "set", "tiers.middle.effort", "turbo")
	if r.code == 0 {
		t.Errorf("an invalid effort was accepted:\n%s", r)
	}
	expect(t, w.run("", "config", "get", "tiers.middle.effort"), 0, "high")
	expect(t, w.run("", "config", "set", "limits.max_parallel_tasks", "3"), 0)
	expect(t, w.run("", "config", "get", "limits.max_parallel_tasks"), 0, "3")
	expect(t, w.run("", "config", "set", "limits.max_parallel_tasks", "lots"), 1)

	// --config points at any file; the cwd fallback is not consulted.
	other := filepath.Join(w.cwd, "alt.json")
	expect(t, w.run("", "--config", other, "init"), 0)
	expect(t, w.run("", "--config", other, "config", "get", "tiers.middle.effort"), 0)
	if strings.Contains(w.run("", "--config", other, "config", "get", "tiers.middle.effort").stdout, "high") {
		t.Error("--config read the fallback file instead of the one named")
	}
}

func TestE2EAccounts(t *testing.T) {
	w := newWorld(t)
	w.init()

	r := w.run("", "accounts")
	expect(t, r, 0, "primary", "secondary", "api-fallback (off)")
	if runtime.GOOS != "darwin" {
		// No keychain here: the table says so and the hint steers to env:
		// refs instead of asking for a paste that cannot land.
		expect(t, r, 0, "no keychain on this platform", "env:NAME")
		if strings.Contains(r.stdout, "first present account") {
			t.Errorf("claims a present account with none:\n%s", r.stdout)
		}
		expect(t, w.run("", "accounts", "set"), 2, "usage: roscoe accounts set <name>")
		return
	}
	expect(t, r, 0, "roscoe accounts set primary")
	rows := strings.Split(plain(r.stdout), "\n")
	for _, row := range rows[1:4] {
		if !strings.Contains(row, " no ") {
			t.Errorf("an account with no token does not say so: %q", row)
		}
	}

	expect(t, w.run("", "accounts", "set"), 2, "usage: roscoe accounts set <name>")
	expect(t, w.run("", "accounts", "bogus"), 2, `unknown subcommand "bogus"`)
	expect(t, w.run("", "accounts", "set", "nobody"), 2, `no account named "nobody"`)
	expect(t, w.run("", "accounts", "set", "api-fallback"), 2, "env file")
	// Storing a token goes through the keychain's own prompt, which needs a
	// terminal. With stdin a pipe the command must refuse rather than store
	// an empty secret.
	expect(t, w.run("sk-ant-oat01-should-not-be-read\n", "accounts", "set", "primary"), 2, "needs a terminal")
	if _, err := os.Stat(filepath.Join(w.kcDir, "roscoe-account-primary")); err == nil {
		t.Error("a token was written to the keychain from a non-terminal stdin")
	}

	// Once a token is present the table and the hint both change.
	os.WriteFile(filepath.Join(w.kcDir, "roscoe-account-primary"), []byte("sk-ant-oat01-x"), 0o600)
	r = w.run("", "accounts")
	expect(t, r, 0, "yes")
	if strings.Contains(r.stdout, "roscoe accounts set primary") {
		t.Errorf("still asking for primary's token after it was stored:\n%s", r.stdout)
	}
	for _, line := range strings.Split(r.all(), "\n") {
		if strings.Contains(line, "sk-ant-oat01") {
			t.Fatalf("a token value reached the terminal: %q", line)
		}
	}
}

func TestE2ESessionsEmpty(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "sessions"), 0, "no sessions yet")
	expect(t, w.run("", "sessions", "--limit", "3"), 0, "no sessions yet")
	expect(t, w.run("", "sessions", "--bogus"), 2, "flag provided but not defined")
}

func TestE2EModelsOffline(t *testing.T) {
	w := newWorld(t)
	w.init()
	// Without --refresh nothing is fetched; each tier's alias is still named.
	expect(t, w.run("", "models"), 0, "tier 1", "tier 2", "tier 3", "sonnet", "--refresh")
}

func TestE2EMemory(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "memory"), 2, "usage: roscoe memory")
	expect(t, w.run("", "memory", "status"), 0, "engine", "graphify", "signals")
}

func TestE2ERunArgumentGates(t *testing.T) {
	w := newWorld(t)
	w.init()
	// No prompt and no terminal: usage, not a hang waiting on stdin.
	expect(t, w.run("", "run"), 2, `usage: roscoe run "<prompt>"`)
	expect(t, w.run("", "run", "a", "b", "--node", "x"), 2, "--resume and --node take one prompt")
	expect(t, w.run("", "run", "--resume", "s", "--node", "n", "p"), 2, "do not combine")
	expect(t, w.run("", "run", "--node", "nowhere", "p"), 2, "nowhere")
	if w.sshCalled() {
		t.Error("an argument error still reached for ssh")
	}
}

func TestE2ELoopNeedsACharter(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "loop"), 2, `usage: roscoe loop "<charter>"`)
}

func TestE2EChatNeedsATerminal(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "chat"), 2, "needs a terminal", "roscoe run")
}

func TestE2ENotifyRelayUpgrade(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "notify"), 2, "usage: roscoe notify test")
	// The default channel is Twilio with nothing configured: the error
	// names every variable it needs rather than failing on the first.
	expect(t, w.run("", "notify", "test"), 1, "TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN")

	expect(t, w.run("", "upgrade"), 2, "--phone is required")
	expect(t, w.run("", "relay"), 2, "usage: roscoe relay status")
	expect(t, w.run("", "relay", "status"), 1, "no relay credentials", "roscoe upgrade")
	expect(t, w.run("", "relay", "unlink"), 0, "not linked")
}

// The default fleet is this machine alone. Nothing here may ssh anywhere,
// and the table must not describe the machine it is running on as
// unreachable.
func TestE2EFleetLocalOnly(t *testing.T) {
	w := newWorld(t)
	w.init()
	for _, args := range [][]string{{"node"}, {"status"}} {
		r := w.run("", args...)
		expect(t, r, 0, "local", "here")
		if strings.Contains(r.stdout, "unreachable") {
			t.Errorf("the local node is called unreachable:\n%s", r.stdout)
		}
	}
	expect(t, w.run("", "dispatch", "p"), 2, "no enabled nodes with an ssh alias")
	expect(t, w.run("", "dispatch"), 2, "usage: roscoe dispatch")
	expect(t, w.run("", "deploy"), 2, "no enabled nodes with an ssh alias")
	expect(t, w.run("", "up"), 2, "no enabled nodes with an ssh alias")
	expect(t, w.run("", "top", "--no-fleet"), 0)
	expect(t, w.run("", "top"), 0)
	if w.sshCalled() {
		t.Fatalf("ssh was invoked for a fleet of one local node:\n%s", readFile(w.sshLog))
	}
}

func TestE2ESmokeOffline(t *testing.T) {
	w := newWorld(t)
	w.init()
	// No env file at all: the check fails and says so, and the run is red.
	r := w.run("", "smoke")
	expect(t, r, 1, "config-validate", "env-file", "claude-bin", "2.1.259")
	expect(t, w.run("", "smoke", "extra"), 2, "unexpected arguments")
	// Nothing in smoke without --full may spawn a worker.
	if strings.Contains(r.stdout, "harness-probe") && !strings.Contains(plain(r.stdout), "– harness-probe") {
		t.Errorf("harness probe ran without --full:\n%s", r.stdout)
	}
}

func TestE2ECalibrateThenTop(t *testing.T) {
	w := newWorld(t)
	w.init()
	before := w.run("", "top", "--no-fleet")
	expect(t, before, 0, "limits", "not yet")

	r := w.run("", "calibrate")
	expect(t, r, 0, "machine", "harnesses", "claude 2.1.259", "recommend", "limits.max_parallel_tasks", "saved to", "--apply")
	if _, err := os.Stat(filepath.Join(w.home, ".roscoe", "calibration.json")); err != nil {
		t.Fatal("calibrate did not save its report under state_dir")
	}
	expect(t, w.run("", "top", "--no-fleet"), 0, "calibrated just now")

	// --apply writes the recommendation into the config, and top agrees.
	expect(t, w.run("", "calibrate", "--apply"), 0)
	got := w.run("", "config", "get", "limits.max_parallel_tasks")
	expect(t, got, 0)
	rec := regexp.MustCompile(`limits\.max_parallel_tasks (\d+)`).FindStringSubmatch(r.stdout)
	if rec == nil || strings.TrimSpace(got.stdout) != rec[1] {
		t.Errorf("recommended %v, config now has %q", rec, got.stdout)
	}
	if w.sshCalled() {
		t.Error("calibrate reached for ssh")
	}
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// claudeStarts returns the argv of every worker the fake claude saw, one
// per start, excluding version probes.
func (w *world) claudeStarts() []string {
	var out []string
	for _, l := range strings.Split(readFile(w.claudeLog), "\n") {
		if l != "" && strings.TrimSpace(l) != "--version" {
			out = append(out, l)
		}
	}
	return out
}

// claudeEvents is the fake's own account of the order things happened in:
// "start <prompt>" when a worker began, "assistant <prompt>" when it produced
// its first response, across every worker.
func (w *world) claudeEvents() []string {
	var out []string
	for _, l := range strings.Split(readFile(w.claudeLog+".events"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestE2ERunOne(t *testing.T) {
	w := newWorld(t)
	w.init()
	r := w.run("", "run", "say hi", "--task-id", "t-1")
	expect(t, r, 0, "[task] t-1", "[init] model=claude-sonnet-5", "[result] success cost=$0.0123", "[done] cost=$0.0123")
	if strings.TrimSpace(r.stdout) != "fake answer to: say hi" {
		t.Errorf("stdout should be the answer alone, got %q", r.stdout)
	}
	if strings.Contains(r.all(), "[account] ") && !strings.Contains(r.all(), "roscoe accounts explains") {
		t.Errorf("the account line does not say where to learn more:\n%s", r.stderr)
	}
	starts := w.claudeStarts()
	if len(starts) != 1 {
		t.Fatalf("claude started %d times, want 1:\n%s", len(starts), strings.Join(starts, "\n"))
	}
	argv := starts[0]
	for _, want := range []string{"-p say hi ", "--output-format stream-json", "--model sonnet", "--permission-mode bypassPermissions",
		"--strict-mcp-config", "--exclude-dynamic-system-prompt-sections", "--session-id ", "--max-budget-usd 8"} {
		if !strings.Contains(argv, want) {
			t.Errorf("worker argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "--resume") {
		t.Error("a fresh run passed --resume")
	}

	// The settings reach the worker: effort, lean, model, budget.
	expect(t, w.run("", "config", "set", "tiers.middle.effort", "high"), 0)
	expect(t, w.run("", "config", "set", "tiers.middle.lean_context", "false"), 0)
	expect(t, w.run("", "config", "set", "tiers.middle.model", "opus"), 0)
	expect(t, w.run("", "run", "again"), 0)
	argv = w.claudeStarts()[1]
	if !strings.Contains(argv, "--effort high") || !strings.Contains(argv, "--model opus") {
		t.Errorf("effort/model changes did not reach the worker:\n%s", argv)
	}
	if strings.Contains(argv, "--strict-mcp-config") {
		t.Errorf("lean off, but the worker still has the strict flag:\n%s", argv)
	}
	if w.sshCalled() {
		t.Error("a local run reached for ssh")
	}
}

func TestE2ERunOneFails(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.claudeMode = "fail"
	r := w.run("", "run", "doomed")
	if r.code == 0 {
		t.Errorf("a worker that died still exited 0:\n%s", r)
	}
	expect(t, r, r.code, "claude failed without result", "invalid_request_error")
}

// Several prompts share one pool: the first runs alone until it is warm
// (its first assistant event), then the rest start, up to the limit.
func TestE2ERunManyPool(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.warmDelay = "0.4"
	r := w.run("", "run", "a", "b", "c")
	expect(t, r, 0, "[tasks] 3 prompts · 3 at a time", "the first warms the prompt cache",
		"done · $0.0123 · fake answer to: a", "done · $0.0123 · fake answer to: b", "done · $0.0123 · fake answer to: c",
		"[tasks] 3 done, 0 failed · $0.0369")
	for _, p := range []string{"a", "b", "c"} {
		if !strings.Contains(r.stdout, "fake answer to: "+p) {
			t.Errorf("stdout lacks the answer to %q:\n%s", p, r.stdout)
		}
	}
	ev := w.claudeEvents()
	if len(ev) != 6 {
		t.Fatalf("fake saw %d events, want 6: %v", len(ev), ev)
	}
	if ev[0] != "start a" || ev[1] != "assistant a" {
		t.Errorf("the first prompt did not run alone until warm: %v", ev)
	}
	if strings.HasPrefix(ev[1], "start") {
		t.Errorf("a second worker started before the first was warm: %v", ev)
	}

	// The limit is honoured and stated.
	expect(t, w.run("", "config", "set", "limits.max_parallel_tasks", "2"), 0)
	r = w.run("", "run", "d", "e", "f")
	expect(t, r, 0, "[tasks] 3 prompts · 2 at a time", "[tasks] 3 done, 0 failed")
}

func TestE2ERunManyFailures(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.claudeMode = "fail"
	r := w.run("", "run", "a", "b")
	if r.code == 0 {
		t.Errorf("every worker failed, exit 0:\n%s", r)
	}
	expect(t, r, r.code, "[tasks] 0 done, 2 failed", "claude failed without result")
}

func TestE2ESessionsAndTopAfterRuns(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "run", "one"), 0)
	expect(t, w.run("", "run", "two", "three"), 0)

	r := w.run("", "sessions")
	expect(t, r, 0, "when", "session", "cost", "turns", "$0.01", "resume one:")
	rows := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if n := strings.Count(r.stdout, "just now"); n != 3 {
		t.Errorf("want 3 session rows, got %d:\n%s", n, r.stdout)
	}
	_ = rows
	r = w.run("", "sessions", "--limit", "1")
	if n := strings.Count(r.stdout, "just now"); n != 1 {
		t.Errorf("--limit 1 showed %d rows:\n%s", n, r.stdout)
	}
	// top sums the same ledgers: three runs, one turn each, $0.0369.
	expect(t, w.run("", "top", "--no-fleet"), 0, "today     $0.04 · 3 runs · 3 turns", "week      $0.04 · 3 runs · 3 turns", "just now")
}

// writeTranscript puts a claude-style session log where FindSession looks,
// with n user/assistant pairs of about 1KB each plus the bookkeeping records
// (file history, attachments) that make real logs enormous and that the
// trimmer drops.
func (w *world) writeTranscript(configDir, sessionID string, pairs int) string {
	w.t.Helper()
	dir := filepath.Join(configDir, "projects", "-Users-someone-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	var b strings.Builder
	pad := strings.Repeat("x", 900)
	for i := 0; i < pairs; i++ {
		fmt.Fprintf(&b, `{"type":"user","sessionId":"%s","uuid":"u%d","message":{"role":"user","content":"question %d %s"}}`+"\n", sessionID, i, i, pad)
		fmt.Fprintf(&b, `{"type":"assistant","sessionId":"%s","uuid":"a%d","message":{"role":"assistant","content":[{"type":"text","text":"answer %d %s"}]}}`+"\n", sessionID, i, i, pad)
		fmt.Fprintf(&b, `{"type":"file-history-snapshot","sessionId":"%s","snapshot":{"trackedFileBackups":{"f%d":"%s"}}}`+"\n", sessionID, i, pad)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return path
}

func TestE2ERunResume(t *testing.T) {
	w := newWorld(t)
	w.init()
	claudeHome := filepath.Join(w.home, ".claude")

	// Unknown session: say so and where it looked.
	expect(t, w.run("", "run", "--resume", "nope", "go on"), 1, "session nope not found", "/projects")

	// A small session resumes as itself.
	w.writeTranscript(claudeHome, "abc-small", 3)
	r := w.run("", "run", "--resume", "abc-small", "go on")
	expect(t, r, 0, "[migrate] importing session abc-small", "[done]")
	argv := w.claudeStarts()[0]
	if !strings.Contains(argv, "--resume abc-small") || strings.Contains(argv, "--session-id") {
		t.Errorf("a resumed run should pass --resume and no --session-id:\n%s", argv)
	}
	if strings.Contains(r.all(), "too large") {
		t.Errorf("a 9KB session was trimmed:\n%s", r.stderr)
	}

	// A source elsewhere is found through --from-config-dir.
	other := filepath.Join(w.home, "elsewhere")
	w.writeTranscript(other, "abc-other", 2)
	expect(t, w.run("", "run", "--resume", "abc-other", "--from-config-dir", other, "go on"), 0, "[migrate] importing session abc-other")
}

// A transcript past the window is trimmed to its most recent records, under
// a new session id, and the operator is told how much came along.
func TestE2ERunResumeTrimsAnOversizedSession(t *testing.T) {
	w := newWorld(t)
	w.init()
	claudeHome := filepath.Join(w.home, ".claude")
	src := w.writeTranscript(claudeHome, "abc-big", 200) // ~600KB on disk
	fi, _ := os.Stat(src)
	if fi.Size() < 400<<10 {
		t.Fatalf("fixture is only %d bytes", fi.Size())
	}

	r := w.run("", "run", "--resume", "abc-big", "go on")
	expect(t, r, 0, "too large to reload whole; resuming its most recent", "[done]")
	m := regexp.MustCompile(`most recent (\d+) messages \((\d+)KB\)`).FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("no trim notice:\n%s", r.stderr)
	}
	kb, _ := strconv.Atoi(m[2])
	if kb > 240 || kb < 200 {
		t.Errorf("trimmed window is %dKB; want just under the 240KB cap", kb)
	}
	argv := w.claudeStarts()[0]
	sid := regexp.MustCompile(`--resume (\S+)`).FindStringSubmatch(argv)
	if sid == nil || sid[1] == "abc-big" {
		t.Fatalf("the trimmed session should resume under a new id:\n%s", argv)
	}
	trimmed, err := os.ReadFile(filepath.Join(claudeHome, "projects", "-Users-someone-proj", sid[1]+".jsonl"))
	if err != nil {
		t.Fatalf("no trimmed transcript for %s: %v", sid[1], err)
	}
	if strings.Contains(string(trimmed), "file-history-snapshot") {
		t.Error("bookkeeping records survived the trim")
	}
	if !strings.Contains(string(trimmed), "question 199") || strings.Contains(string(trimmed), `"question 0 `) {
		t.Error("the trim should keep the newest records and drop the oldest")
	}
	if strings.Contains(string(trimmed), "abc-big") {
		t.Error("the trimmed transcript still carries the old session id")
	}
}

// When the model still refuses the window as too long, run halves it and
// sends the same prompt again rather than handing the operator the error.
func TestE2ERunResumeRetriesWhenPromptIsTooLong(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.claudeMode = "toolong"
	w.writeTranscript(filepath.Join(w.home, ".claude"), "abc-big", 200)

	r := w.run("", "run", "--resume", "abc-big", "go on")
	expect(t, r, 0, "the model refused that much history; retrying with the most recent 120KB", "[done]")
	starts := w.claudeStarts()
	if len(starts) != 2 {
		t.Fatalf("claude started %d times, want 2 (refused, then retried):\n%s", len(starts), strings.Join(starts, "\n"))
	}
	for i, a := range starts {
		if !strings.Contains(a, "--resume ") {
			t.Errorf("start %d was not a resume:\n%s", i, a)
		}
	}
	if strings.Contains(r.stdout, "Prompt is too long") {
		t.Errorf("the refusal leaked into the answer:\n%s", r.stdout)
	}
}

// TestE2ELoopOnce: one iteration, the worker's loop block folded into
// loop.md by the supervisor, and the summary an operator reads.
func TestE2ELoopOnce(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.resultFile = filepath.Join(w.home, "result.txt")
	os.WriteFile(w.resultFile, []byte(`Tidied the thing.

`+"```loop"+`
STATUS: done
PLAN:
- [x] tidy the thing
TRIED:
- tidied it; nothing failed
NOTES:
- the thing lives in things/
`+"```\n"), 0o600)

	r := w.run("", "loop", "Tidy the thing", "--once")
	expect(t, r, 0, "[loop] ", "[iteration 1]", "[iteration 1] done · done · 0.0123 USD · --once",
		"[loop] done after 1 iterations · 0.0123 USD", "[loop] working memory: ", "roscoe run --resume ")
	md := readFile(filepath.Join(w.cwd, "loop.md"))
	if !strings.HasPrefix(md, "# Tidy the thing\n") {
		t.Errorf("loop.md was not seeded with the charter:\n%s", md)
	}
	for _, want := range []string{"## Status\ndone", "- [x] tidy the thing", "- tidied it; nothing failed", "- the thing lives in things/"} {
		if !strings.Contains(md, want) {
			t.Errorf("loop.md lacks %q after the tail was applied:\n%s", want, md)
		}
	}
	if strings.Contains(md, "_nothing yet_") {
		t.Errorf("the seed placeholder survived a Tried entry:\n%s", md)
	}
	starts := w.claudeStarts()
	if len(starts) != 1 {
		t.Fatalf("%d worker starts for --once", len(starts))
	}
	for _, want := range []string{"Charter: Tidy the thing", "Do not edit loop.md yourself", "STATUS: continuing | done | blocked"} {
		if !strings.Contains(starts[0], want) {
			t.Errorf("the kernel prompt lacks %q", want)
		}
	}
	// A loop.md that already says done is not worked again.
	r = w.run("", "loop", "Tidy the thing", "--once")
	expect(t, r, 0, "loop.md already reports done")
	if len(w.claudeStarts()) != 1 {
		t.Error("a finished loop dispatched a worker")
	}
}

// Without a loop block the status stays continuing; the run resumes the same
// session each iteration and stops at the ceiling with exit 3.
func TestE2ELoopCeilingAndResume(t *testing.T) {
	w := newWorld(t)
	w.init()
	r := w.run("", "loop", "Keep going", "--max-iterations", "2", "--no-quorum")
	expect(t, r, 3, "[judge] the worker's own status line", "[iteration 1] continuing · continue",
		"[iteration 2] continuing", "hit the 2 iteration ceiling still continuing", "0.0246 USD")
	starts := w.claudeStarts()
	if len(starts) != 2 {
		t.Fatalf("%d worker starts, want 2", len(starts))
	}
	if strings.Contains(starts[0], "--resume") || !strings.Contains(starts[1], "--resume ") {
		t.Errorf("the second iteration should resume the first's session:\n%s\n%s", starts[0], starts[1])
	}
	if !strings.Contains(starts[1], "--- MEMORY (loop.md) ---") {
		t.Error("the second iteration's prompt does not carry loop.md")
	}
}

func TestE2ELoopBudgetStops(t *testing.T) {
	w := newWorld(t)
	w.init()
	r := w.run("", "loop", "Spend little", "--budget", "0.02", "--max-iterations", "9", "--no-quorum")
	expect(t, r, 3, "spent 0.02 of a 0.02 budget", "escalate after 2 iterations")
	if n := len(w.claudeStarts()); n != 2 {
		t.Errorf("%d worker starts; a $0.02 budget at $0.0123 a turn allows 2", n)
	}
}

// A worker that keeps dying stops the run before it can spend a budget
// discovering that, and says how many times it failed.
func TestE2ELoopStopsAfterRepeatedFailures(t *testing.T) {
	w := newWorld(t)
	w.init()
	w.claudeMode = "fail"
	r := w.run("", "loop", "Doomed", "--max-iterations", "9", "--no-quorum")
	expect(t, r, 1, "3 iterations in a row failed", "claude failed without result")
	if n := len(w.claudeStarts()); n != 3 {
		t.Errorf("%d worker starts, want 3 then stop", n)
	}
	md := readFile(filepath.Join(w.cwd, "loop.md"))
	if !strings.Contains(md, "## Status\ncontinuing") {
		t.Errorf("loop.md after failures:\n%s", md)
	}
}

// env is the isolated environment every spawned roscoe gets.
func (w *world) env() []string {
	system := "/usr/bin:/bin:/usr/sbin:/sbin"
	return []string{
		"HOME=" + w.home,
		"PATH=" + w.bin + ":" + system,
		"FAKE_KC=" + w.kcDir,
		"ROSCOE_E2E_SSH_LOG=" + w.sshLog,
		"FAKE_CLAUDE_LOG=" + w.claudeLog,
		"FAKE_CLAUDE_MODE=" + w.claudeMode,
		"FAKE_CLAUDE_EVENTS=" + w.claudeLog + ".events",
		"FAKE_CLAUDE_WARM_DELAY=" + w.warmDelay,
		"FAKE_CLAUDE_RESULT_FILE=" + w.resultFile,
		"ROSCOE_RELAY_STATE=" + filepath.Join(w.home, ".roscoe", "relay.json"),
		"TERM=dumb",
		"NO_COLOR=1",
	}
}
