package main

// End-to-end tests over the real binary. Every subcommand is exercised the
// way an operator would hit it: exec'd with arguments, an isolated HOME, a
// working directory with no roscoe.json in it, and a PATH whose only
// "claude", "security" and "ssh" are fakes that record what was asked of
// them. Nothing here reaches the network or the real keychain.

import (
	"bytes"
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
	t       *testing.T
	home    string
	cwd     string
	bin     string
	kcDir   string // the fake keychain's store
	sshLog  string // touched whenever the fake ssh is called
	cfgPath string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{
		t:      t,
		home:   filepath.Join(root, "home"),
		cwd:    filepath.Join(root, "cwd"),
		bin:    filepath.Join(root, "bin"),
		kcDir:  filepath.Join(root, "kc"),
		sshLog: filepath.Join(root, "ssh-called"),
	}
	for _, d := range []string{w.home, w.cwd, w.bin, w.kcDir, filepath.Join(w.home, ".roscoe")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.fake("claude", `#!/bin/sh
case "$1" in
  --version) echo "2.1.259 (Claude Code)"; exit 0;;
esac
# A worker probe: fail fast, the way a refused request does.
echo '{"type":"error","error":{"type":"invalid_request_error","message":"fake"}}' >&2
exit 1
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
	system := "/usr/bin:/bin:/usr/sbin:/sbin"
	cmd.Env = []string{
		"HOME=" + w.home,
		"PATH=" + w.bin + ":" + system,
		"FAKE_KC=" + w.kcDir,
		"ROSCOE_E2E_SSH_LOG=" + w.sshLog,
		"ROSCOE_RELAY_STATE=" + filepath.Join(w.home, ".roscoe", "relay.json"),
		"TERM=dumb",
		"NO_COLOR=1",
	}
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
	expect(t, r, 0, "primary", "secondary", "api-fallback (off)", "roscoe accounts set primary")
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
