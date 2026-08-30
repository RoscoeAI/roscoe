# roscoe build spec — slice 1 (day 1–2)

Module `roscoe.sh/roscoe`, Go 1.26, **stdlib only — zero external dependencies**.
`internal/config/types.go` is the authored contract — import it, never modify it.
Style: small files, no cleverness, errors wrapped with `fmt.Errorf("...: %w", err)`,
contexts threaded everywhere, no global state except `main`.

Slice-1 goal: `roscoe init | config | router | smoke [--full] | run "goal" | version`
working on the laptop (single node, no ssh). `up/node/accounts/deploy/dispatch/status`
are stubs printing what they will do (pointer to ARCHITECTURE.md).

## internal/config (config.go — types.go already exists)

```go
func Default() *Config                                  // opinionated defaults ≈ roscoe.json in repo root
func Load(path string) (*Config, error)                 // strict json.Decoder(DisallowUnknownFields), then Validate
func (c *Config) Save(path string) error                // MarshalIndent, 0644, atomic (tmp+rename)
func (c *Config) Validate() []error                     // referenced providers/accounts exist; virtual_model non-empty; ports sane
func (c *Config) Get(dotted string) (any, error)        // "tiers.subagents.model", arrays by index "nodes.0.ssh" — via reflection or map round-trip
func (c *Config) SetPath(dotted string, raw string) error // parse raw as JSON scalar first, fallback string; map round-trip then re-decode
func ExpandPath(p string) string                        // "~/x" → $HOME/x
func LoadEnvFile(path string) (map[string]string, error) // KEY=VAL lines, # comments, no quoting gymnastics
```

## internal/router (router.go)

```go
type Options struct {
    Cfg      *config.Config
    Env      map[string]string // from LoadEnvFile
    LogW     io.Writer         // JSONL request log, may be nil
    Bind     string            // override; "" → cfg.Router.Bind
    Port     int               // override; 0 → cfg.Router.Port
}
func New(o Options) (*Router, error)
func (r *Router) Handler() http.Handler
func (r *Router) ListenAndServe(ctx context.Context) error // graceful shutdown on ctx.Done
func (r *Router) Addr() string                             // actual bind addr (supports Port 0 for tests)
```

Behavior (POST `/v1/messages`, POST `/v1/messages/count_tokens`, GET `/healthz`):
1. Read body (cap 10MB), extract `"model"` via a minimal struct decode into
   `map[string]json.RawMessage` — do not re-serialize the rest.
2. Route: model == `cfg.Tiers.Subagents.VirtualModel` → subagents tier provider,
   **rewrite the model field** to the tier's Model (splice the raw JSON:
   re-marshal only the model value); else → provider of `cfg.Router.DefaultRoute`
   tier (middle). claude-* passthrough keeps body byte-identical.
3. Auth per provider.Auth: `"account"` → copy inbound `Authorization`,
   `x-api-key`, and all `anthropic-*` headers verbatim; `"env:VAR"` → set
   `Authorization: Bearer <env[VAR]>`, drop inbound auth; `"static:v"` → same
   with literal. Always forward `content-type`, `anthropic-version`,
   `anthropic-beta`, `accept`.
4. Stream response: copy status + headers, then `io.Copy` through a
   flush-per-write writer (`http.Flusher` after every Write) so SSE frames
   never buffer. No body inspection on the response path.
5. count_tokens: if provider.CountTokens == "estimate" respond locally
   `{"input_tokens": len(body)/4}`; else forward with same auth rules.
6. Log JSONL per request to LogW: `{ts, path, model_in, model_out, upstream,
   status, latency_ms, bytes_in, bytes_out}`.
7. Timeouts: no read/write deadline on streaming paths; dial timeout 10s;
   response header timeout 600s (GLM reasoning is slow).

## internal/streamjson (streamjson.go)

Claude Code `--output-format stream-json --verbose` events (NDJSON).

```go
type Event struct {
    Type    string          // "system" | "assistant" | "user" | "result"
    Subtype string          // "init", "api_retry", "success", ...
    Raw     json.RawMessage // full line, always kept
}
func NewScanner(r io.Reader) *Scanner      // bufio, 10MB max line
func (s *Scanner) Next() (*Event, error)   // io.EOF at end
type ResultEvent struct {
    Result       string          `json:"result"`
    SessionID    string          `json:"session_id"`
    TotalCostUSD float64         `json:"total_cost_usd"`
    IsError      bool            `json:"is_error"`
    Usage        json.RawMessage `json:"usage"`
    PermissionDenials json.RawMessage `json:"permission_denials"`
}
func (e *Event) AsResult() (*ResultEvent, bool)  // Type=="result"
type InitEvent struct { SessionID, Model string; Tools []string; Capabilities []string }
func (e *Event) AsInit() (*InitEvent, bool)      // Type=="system" && Subtype=="init"
```

## internal/ledger (ledger.go)

```go
func Open(dir string) (*Ledger, error)      // mkdir -p; events.jsonl append-only
func (l *Ledger) Event(node, worker, task string, ev *streamjson.Event) error // envelope {ts,node,worker,task,seq,event}
func (l *Ledger) Note(kind string, v any) error  // {ts,kind,...v}
func (l *Ledger) Close() error
```
Single writer, mutex-guarded, fsync on Close only.

## internal/worker (worker.go, agents.go)

```go
type Task struct {
    ID      string
    Prompt  string
    Dir     string            // cwd; created if missing
    Account string            // account name; token resolved by caller
    Token   string            // CLAUDE_CODE_OAUTH_TOKEN value ("" → rely on claude's own auth)
}
type Opts struct {
    Cfg        *config.Config
    RouterAddr string         // "127.0.0.1:8484"
    ClaudeBin  string         // "" → "claude" from PATH
    Ledger     *ledger.Ledger // may be nil
    OnEvent    func(*streamjson.Event) // may be nil
}
func Run(ctx context.Context, t Task, o Opts) (*streamjson.ResultEvent, error)
func BuildAgentsJSON(cfg *config.Config) (string, error) // tier3 agents, model forced to VirtualModel, prompt defaulted from description
```

Run builds exactly this invocation (per ARCHITECTURE.md):
- env (on top of os.Environ): `CLAUDE_CONFIG_DIR=<state>/workers/<task>/ccfg` (mkdir),
  `ANTHROPIC_BASE_URL=http://<RouterAddr>`, `CLAUDE_CODE_OAUTH_TOKEN` (if Token != ""),
  `CLAUDE_CODE_SUBAGENT_MODEL=<virtual>`, `ANTHROPIC_DEFAULT_HAIKU_MODEL=<virtual>`
  (only when MapHaikuAlias), `API_TIMEOUT_MS=<cfg>`,
  `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`, `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`
- argv: `-p <prompt> --output-format stream-json --verbose --forward-subagent-text
  --permission-mode <cfg> --allowedTools <comma-joined> --agents <BuildAgentsJSON>
  --session-id <uuid v4 via crypto/rand> --max-budget-usd <cfg> --model <middle.Model>`
- pipe stdout → streamjson.Scanner → Ledger.Event + OnEvent per event; capture the
  ResultEvent; stderr → collected (returned in error on failure).
- ctx cancel: SIGINT, 10s grace, then SIGKILL. Non-zero exit without a ResultEvent = error.

## internal/smoke (smoke.go)

```go
type Check struct { Name string; OK bool; Skipped bool; Detail string }
func Run(ctx context.Context, cfg *config.Config, env map[string]string, full bool) []Check
```
Checks in order (each independent, failures don't stop the rest):
1. config-validate; 2. env-file loads, DEEP_INFRA_API_KEY present;
3. router-start (Port 0, in-process); 4. tier3 count_tokens through router
   (POST count_tokens, model=virtual, tiny body) expects 200;
5. tier3 1-token live ping (`{"model":<virtual>,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`) expects 200;
6. anthropic-leg count_tokens IF any enabled claude-subscription account token
   resolvable from env (`env:` refs only in slice 1) else Skipped;
7. claude-bin found (`claude --version` captured);
8. (full only) harness-probe: exec `claude -p 'Reply with exactly: pong'
   --output-format json --model <virtual>` with `ANTHROPIC_BASE_URL=http://<router>`,
   `ANTHROPIC_AUTH_TOKEN=roscoe-local`, isolated CLAUDE_CONFIG_DIR under os.TempDir,
   `--permission-mode bypassPermissions --max-budget-usd 0.25`; OK if exit 0 and
   result contains "pong". This proves GLM-5.3-Flash drives the full harness.

## internal/notify (notify.go, twilio.go, ntfy.go)

```go
type Message struct { Title, Body string; Priority int }
type Reply struct { From, Body string; At time.Time }
type Notifier interface {
    Send(ctx context.Context, m Message) error
    // InboundHandler returns nil when the channel has no inbound path.
    InboundHandler(onReply func(Reply)) http.Handler
}
func New(cfg config.NotifyCfg, env map[string]string) (Notifier, error) // "twilio-sms" | "ntfy"
```
- twilio.go: Send = POST `https://api.twilio.com/2010-04-01/Accounts/<SID>/Messages.json`
  (url-encoded form: To, From, Body; basic auth SID/TOKEN from env
  TWILIO_ACCOUNT_SID/TWILIO_AUTH_TOKEN/TWILIO_FROM/TWILIO_TO). InboundHandler
  parses `application/x-www-form-urlencoded` (From, Body), validates
  `X-Twilio-Signature` (HMAC-SHA1 of URL + sorted params, base64, per Twilio docs —
  needs the public URL prefix: read env TWILIO_WEBHOOK_URL), replies 200 with
  empty `<Response/>` TwiML, `Content-Type: text/xml`.
- ntfy.go: Send = POST `<server>/<topic>` body=Body, headers Title/Priority
  (+ `Authorization: Bearer <env NTFY_TOKEN>` when set). InboundHandler → nil
  (replies arrive via subscribe; out of slice).

## cmd/roscoe (main.go, commands.go)

Stdlib `flag` per subcommand, no framework. Global: `--config` (default:
`./roscoe.json`, fallback `~/.roscoe/roscoe.json`). Commands:
- `version` — prints `roscoe <version> (<go version>)`; `var Version = "dev"`.
- `init` — writes Default() to --config path if absent (refuses overwrite).
- `config get <path>` / `config set <path> <value>` — Load, Get/SetPath, Save; set prints old → new.
- `router [--bind B] [--port N]` — LoadEnvFile, run router foreground, JSONL log to
  stdout, SIGINT graceful.
- `smoke [--full]` — run checks, aligned table to stdout (`✓ ✗ –` + detail), exit 1 on any failure.
- `run <prompt> [--task-id X] [--dir D]` — starts router in-process (Port from cfg),
  resolves first enabled middle-tier account with `env:` token-ref (else no token),
  worker.Run, live event narration to stderr (event type + subtype + assistant text
  snippets), final result text to stdout, cost + session-id summary to stderr.
- stubs: `up node accounts deploy dispatch status top` → one-paragraph "coming in
  slice 2" + ARCHITECTURE.md pointer, exit 2.

Wire SIGINT via signal.NotifyContext at top of main.
