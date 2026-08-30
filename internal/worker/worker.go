// Package worker spawns one Claude Code harness per task (`claude -p`),
// streams its stream-json stdout into the ledger, and returns the final
// ResultEvent. Spawn-per-task by design: see ARCHITECTURE.md "Worker
// lifecycle".
package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/streamjson"
)

// Task is one unit of work handed to a fresh claude -p session.
type Task struct {
	ID      string
	Prompt  string
	Dir     string // cwd; created if missing
	Account string // account name; token resolved by caller
	Token   string // CLAUDE_CODE_OAUTH_TOKEN value ("" → rely on claude's own auth)
	// Resume continues an existing claude session by id instead of starting
	// fresh. When ResumeFrom (a source transcript path, usually from
	// FindSession) is also set, the transcript is imported into the task's
	// isolated config dir first — this is how an interactive ~/.claude
	// session migrates into the fleet.
	Resume     string
	ResumeFrom string
}

// Opts carries the shared wiring a worker run needs.
type Opts struct {
	Cfg        *config.Config
	RouterAddr string         // "127.0.0.1:8484"
	ClaudeBin  string         // "" → "claude" from PATH
	CodexBin   string         // "" → "codex" from PATH (harness "codex")
	Ledger     *ledger.Ledger // may be nil
	OnEvent    func(*streamjson.Event)
}

// killGrace is how long a SIGINT'd claude gets to shut down before SIGKILL.
const killGrace = 10 * time.Second

// Run executes one task in a fresh claude -p harness and blocks until the
// process exits (it always reaps the child, even on cancellation). A non-zero
// exit without a captured ResultEvent is an error; a captured ResultEvent is
// returned even if the exit status was non-zero.
func Run(ctx context.Context, t Task, o Opts) (*streamjson.ResultEvent, error) {
	if o.Cfg == nil {
		return nil, errors.New("worker: nil config")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("worker: not starting: %w", err)
	}

	sessionID, err := uuidV4()
	if err != nil {
		return nil, fmt.Errorf("worker: session id: %w", err)
	}
	if t.Resume != "" {
		sessionID = t.Resume
	}
	if t.ID == "" {
		t.ID = sessionID
	}

	if t.Dir != "" {
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("worker: create task dir: %w", err)
		}
	}
	// Isolated per-task config dir — concurrent claude processes sharing one
	// CLAUDE_CONFIG_DIR corrupt session state.
	ccfgDir := filepath.Join(config.ExpandPath(o.Cfg.StateDir), "workers", t.ID, "ccfg")
	if err := os.MkdirAll(ccfgDir, 0o755); err != nil {
		return nil, fmt.Errorf("worker: create config dir: %w", err)
	}
	if t.Resume != "" && t.ResumeFrom != "" {
		if err := importSession(t.ResumeFrom, ccfgDir); err != nil {
			return nil, err
		}
	}

	agentsJSON, err := BuildAgentsJSON(o.Cfg)
	if err != nil {
		return nil, fmt.Errorf("worker: build agents: %w", err)
	}

	mid := o.Cfg.Tiers.Middle
	sub := o.Cfg.Tiers.Subagents

	harness := mid.Harness
	if harness == "" {
		harness = "claude"
	}

	var (
		bin          string
		args         []string
		env          []string
		codexLastMsg string
	)
	switch harness {
	case "codex":
		// Single-agent worker via `codex exec`. Codex owns its auth and model
		// config; the router, virtual-model routing, and subagent swarms are
		// claude-harness features and don't apply here.
		if t.Resume != "" {
			return nil, fmt.Errorf("worker: task %s: --resume is not supported for the codex harness yet", t.ID)
		}
		bin = o.CodexBin
		if bin == "" {
			bin = "codex"
		}
		codexLastMsg = filepath.Join(filepath.Dir(ccfgDir), "last-message.txt")
		args = []string{"exec", "--json", "--skip-git-repo-check", "-o", codexLastMsg}
		if t.Dir != "" {
			args = append(args, "-C", t.Dir)
		}
		if mid.PermissionMode == "bypassPermissions" {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "-s", "workspace-write", "--approve-for-me")
		}
		args = append(args, t.Prompt)
		env = os.Environ()

	default: // claude
		bin = o.ClaudeBin
		if bin == "" {
			bin = "claude"
		}
		args = []string{
			"-p", t.Prompt,
			"--output-format", "stream-json",
			"--verbose",
			"--forward-subagent-text",
			"--permission-mode", mid.PermissionMode,
			"--allowedTools", strings.Join(mid.AllowedTools, ","),
			"--agents", agentsJSON,
			"--max-budget-usd", strconv.FormatFloat(mid.MaxBudgetUSDPerTask, 'f', -1, 64),
			"--model", mid.Model,
		}
		if t.Resume != "" {
			args = append(args, "--resume", t.Resume)
		} else {
			args = append(args, "--session-id", sessionID)
		}

		env = append(os.Environ(),
			"CLAUDE_CONFIG_DIR="+ccfgDir,
			"ANTHROPIC_BASE_URL=http://"+o.RouterAddr,
		)
		if t.Token != "" {
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+t.Token)
		} else {
			// No account token: a fresh CLAUDE_CONFIG_DIR has no credentials and
			// claude refuses to run ("Not logged in"). A dummy gateway bearer
			// satisfies auth against the loopback router; it only works end-to-end
			// when no route needs "account" (header-passthrough) auth — i.e.
			// all-tier3/all-env-auth configs.
			env = append(env, "ANTHROPIC_AUTH_TOKEN=roscoe-local")
		}
		env = append(env, "CLAUDE_CODE_SUBAGENT_MODEL="+sub.VirtualModel)
		if sub.MapHaikuAlias {
			env = append(env, "ANTHROPIC_DEFAULT_HAIKU_MODEL="+sub.VirtualModel)
		}
		// Zero values are skipped rather than exported: API_TIMEOUT_MS=0 would
		// mean an instant timeout, not "unset".
		if mid.APITimeoutMS > 0 {
			env = append(env, "API_TIMEOUT_MS="+strconv.Itoa(mid.APITimeoutMS))
		}
		if sub.MaxConcurrent > 0 {
			env = append(env, "CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS="+strconv.Itoa(sub.MaxConcurrent))
		}
		if sub.MaxDepth > 0 {
			env = append(env, "CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH="+strconv.Itoa(sub.MaxDepth))
		}
	}

	// Not CommandContext: cancellation runs the SIGINT → grace → SIGKILL
	// sequence below instead of an immediate kill.
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.Dir
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker: start %s: %w", bin, err)
	}

	// Killer: on ctx cancel, SIGINT then SIGKILL after killGrace. done is
	// closed once the child is reaped; Signal/Kill on a reaped process is a
	// harmless os.ErrProcessDone.
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(killGrace):
				_ = cmd.Process.Kill()
			}
		}
	}()

	var (
		result  *streamjson.ResultEvent
		scanErr error
	)
	sc := streamjson.NewScanner(stdout)
	for {
		ev, err := sc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			scanErr = err
			break
		}
		if o.Ledger != nil {
			// A ledger write failure must not kill a running task; the
			// stream continues and the caller still sees every event.
			_ = o.Ledger.Event("local", t.Account, t.ID, ev)
		}
		if o.OnEvent != nil {
			o.OnEvent(ev)
		}
		if res, ok := ev.AsResult(); ok {
			result = res
		}
	}
	// Drain any remaining stdout so Wait never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)

	waitErr := cmd.Wait() // always reap
	close(done)

	if scanErr != nil {
		return nil, fmt.Errorf("worker: task %s: reading stream-json: %w", t.ID, scanErr)
	}
	// Codex emits its own JSONL (ledgered above as generic events) and never
	// a claude result event; the final message lands in the -o file.
	if result == nil && codexLastMsg != "" {
		if b, err := os.ReadFile(codexLastMsg); err == nil {
			if msg := strings.TrimSpace(string(b)); msg != "" {
				result = &streamjson.ResultEvent{Result: msg, IsError: waitErr != nil}
			}
		}
	}
	if result != nil {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("worker: task %s interrupted before result: %w", t.ID, err)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("worker: task %s: %s failed without result (stderr: %s): %w",
			t.ID, harness, stderrTail(stderr.Bytes()), waitErr)
	}
	return nil, fmt.Errorf("worker: task %s: %s exited 0 without a result event (stderr: %s)",
		t.ID, harness, stderrTail(stderr.Bytes()))
}

// stderrTail keeps error messages bounded; claude can be chatty on stderr.
func stderrTail(b []byte) string {
	const max = 2048
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "<empty>"
	}
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}

// uuidV4 formats 16 random bytes as an RFC 4122 version-4 UUID.
func uuidV4() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
