# Roscoe

**Automate your agent fleet in one Go binary.**

You hold one interactive session, Claude Code or Codex, either harness. Roscoe
dispatches the rest in three tiers: headless workers, each fanning out to swarms
of subagents on cheap open models, by default
[`zai-org/GLM-5.3-Flash`](https://deepinfra.com/zai-org/GLM-5.3-Flash) via
DeepInfra's Anthropic-compatible API. One JSON config decides where every tier's
model comes from (`deepinfra` | `local` | `anthropic`), which machines run
workers, and how many.

Two principles run through everything:

- **A dial for autonomy.** `autonomy.level` goes 0 to 100. At 100 Roscoe never
  interrupts you: the model quorum answers every question, each decision lands
  in the ledger, and the fleet runs until the credits run out. Below 100, the
  rare decision that clears your threshold reaches you by SMS and you settle it
  by replying to a text.
- **Memory that compounds.** Roscoe is opinionated about memory: it leans on
  [Graphify](https://github.com/Graphify-Labs/graphify), with a per-project
  knowledge graph under `~/.roscoe/graph` next to the rest of the fleet state.
  Before each iteration the fleet asks the graph what it already knows and
  writes the answer into the run's working memory; after each one, the quorum's
  decision goes back as a signal. So run 40 does not relearn what run 3 found.
  Memory is never in the hot path: a graph that is missing, stale, or slow
  costs you the recall and nothing else. `roscoe memory status` says where it
  stands.

```
curl -fsSL https://roscoe.sh/install | sh
```

## How it works

- **Tier 1, main.** Your stock interactive `claude` session (or `roscoe run`
  headless). Nothing about your daily driver changes.
- **Tier 2, workers.** Spawn-per-task `claude -p` sessions: the full Claude
  Code harness (tools, subagents, Workflows), headless, one fresh git worktree
  and isolated `CLAUDE_CONFIG_DIR` per task. A worker is not your desktop, so
  it runs lean: no MCP servers, no user-level skills or agents, and per-machine
  sections moved out of the system prompt. Every round trip a worker makes
  re-reads its whole prompt prefix, which makes that prefix the single largest
  cost in a fleet. Measured on the same charter, the same three iterations:
  186 tools and 191 commands fall to 27 and 48, and the run goes from $0.7152
  to $0.3354. Turn it off with
  `roscoe config set tiers.middle.lean_context false` if workers need your MCP
  tools. Workers run at
  `tiers.middle.effort: ultracode`, so each substantive task is planned as a
  workflow and fanned out rather than ground through in one thread. That is
  the whole economic argument: the planning happens on a top-tier model, the
  work it hands out lands on tier 3. Dial it back with
  `roscoe config set tiers.middle.effort high`.
- **Tier 3, the swarm.** Native Claude Code subagents whose requests carry a
  virtual model name (`roscoe/tier3`). A ~400-line loopback reverse proxy
  dispatches each request by model name: `claude-*` passes through to Anthropic
  untouched (your own auth headers, byte-for-byte, prompt caching intact);
  the virtual name is rewritten to the configured tier-3 provider. No protocol
  translation anywhere: DeepInfra, Ollama, and LM Studio all speak the
  Anthropic Messages API natively.

## Quickstart

```sh
roscoe init                      # write roscoe.json
roscoe config set tiers.subagents.provider deepinfra
echo "DEEP_INFRA_API_KEY=..." >> ~/.roscoe/.env
roscoe smoke --full              # proves the whole path: a real claude -p
                                 # harness driven end-to-end by GLM-5.3-Flash
roscoe run "refactor the billing module"
roscoe run "port the tests" "update the README" "bump deps"   # several at once, up to limits.max_parallel_tasks
```

Every setting is one path. `roscoe config` lists them most-changed first,
`roscoe config show tiers.middle.effort` shows one with its value, its
options, and what the choice costs, and `roscoe config set` changes it; in
chat, `/config` does the same and tab walks one level at a time. Every config value is CLI-settable (`roscoe config set quorum.enabled true`);
precedence is flag > `ROSCOE_*` env > `roscoe.json` > defaults. See
[`roscoe.example.json`](roscoe.example.json) for the full shape and
[`SPEC.md`](SPEC.md) for the package contracts.

Nobody memorizes a config schema, so in `roscoe chat` the settings are
something you look at rather than recall. `/settings` puts all three tiers on
one screen, each with its model, provider, and effort, and says plainly where
a knob does not exist rather than leaving a gap (tier 3 has no per-subagent
effort in Claude Code, and the row says so):

```
  tier 1   your session, the one you talk to
  › model      opus  →  claude-opus-5
    provider   anthropic
    effort     ultracode

  tier 2   workers, one spawned per task
    model      sonnet  →  claude-sonnet-5
    provider   anthropic
    effort     ultracode
    harness    claude
    lean       true   no MCP servers or personal skills, much cheaper prefix
    cache      1h prompt cache

  tier 3   the swarm each worker fans out to
    model      zai-org/GLM-5.3-Flash
    provider   deepinfra
    effort     claude code has no per-subagent effort knob; tier 2's applies
    width      8 at once
```

Model aliases resolve to what they actually are, so a row reads
`sonnet  →  claude-sonnet-5` rather than leaving you to guess which sonnet.
Roscoe learns that three ways, none needing a credential it does not already
have: from the harness's init event on every run; from each provider's published
list where one exists; and by asking the installed `claude` directly, pointing
it at a local endpoint that records the model it puts on the wire and refuses
the request. That last one costs no tokens and is the only way to answer for
tier 1, where nothing ever runs through roscoe. It takes about two seconds
for every alias at once. `roscoe models --refresh` does
all three.

Up and down move, left and right step through a setting's values, enter types
one in, esc closes. Every change is validated and written to `roscoe.json` as
you make it.

The prompt is a real line editor: arrows, home and end, word jumps, the
readline kill keys. Paste a stack trace and it arrives whole, since the
terminal brackets the paste and newlines inside it insert rather than send;
alt-enter or a trailing backslash adds a line by hand, and the box grows to
fit.

A chat's transcript is kept from growing without bound. `--resume` trims a large
session to its recent messages on import, and after every turn roscoe checks
the size again and trims before the next one, so a long conversation costs what
its recent context costs rather than everything it has ever said. You are told
when it happens.

For everything outside those tiers there is `/config`, which walks the whole
schema the same way: it lists the top-level keys with a line on what each one
is, naming one goes a level deeper, tab completes and descends, and the line
above the prompt describes whatever you are pointing at as you type.

```
/config                      accounts, providers, nodes, tiers, autonomy, …
/config tiers                main, middle, subagents
/config tiers.middle         every worker setting, with its current value
/config tiers.middle.effort high
```

The reply streams as it is written rather than landing whole at the end of
the turn, so a long answer is watched rather than waited for.

Workers get exactly the MCP servers you declare in `tiers.middle.mcp_servers`,
in Claude Code's own shape, and nothing else. Every server's tool definitions
ride in the prompt prefix on every round trip, so the default is none and
declaring one is a cost decision made on purpose.

Finding a conversation again does not need its id. `roscoe sessions` lists what
has run, newest first, with what each cost and the first thing you said;
A conversation too large to reload whole is resumed from its most recent
messages, sized to fit the model; if the model still refuses the window as
too long, chat halves it and retries on its own. `roscoe chat --last` resumes the newest, and `roscoe chat --pick` offers a
numbered list.

## Working a charter, not a prompt

`roscoe run` answers a prompt. `roscoe loop` works a charter until it is done:

```
roscoe loop "get the integration suite green" --max-iterations 8 --budget 5
```

The loop is deterministic Go, not a prompt: dispatch an iteration, read what
the worker left in `loop.md`, judge it, dispatch again. That matters in three
ways. It works the same for either harness, since Codex has no loop of its own.
Its state is on disk rather than inside a session, so you can kill the run,
read the file, edit it, and pick up where it stopped. And it puts a decision
point between iterations, which is where the quorum and `autonomy.level` will
sit.

`loop.md` is the working memory: the charter, the plan, what has been tried,
and durable notes. The worker never opens it. The supervisor inlines the parts
worth carrying into the prompt, and the worker ends its reply with a small
block that the supervisor folds back in, so bookkeeping costs no tool calls and
the format is parsed rather than trusted. A status of `continuing`, `done`, or
`blocked` says where the run stands. `done` ends the run, `blocked` escalates.
What has been tried and learned only ever accumulates: the supervisor snapshots
the file before each iteration and puts back anything the worker dropped, so a
careless rewrite cannot erase a dead end and send the next iteration walking
back into it. That is enforced in code rather than asked for in the prompt,
because a prompt is a contract a model can always break.
The iteration ceiling and the budget are the loop's to enforce, not the
worker's, so no judgment call can spend past them. Esc stops after the current
iteration so the worker still writes its memory rather than being cut off.

The judge is a quorum, not the worker's own opinion of its work. Several models
read the result and the working memory and vote done, continue, or escalate;
majority rules, and a split or a low-confidence verdict escalates rather than
being resolved by one model. `autonomy.level` sets how much they absorb: at 100
an unresolvable vote becomes more work rather than a question for you. The one
thing the dial does not override is `quorum.always_escalate`, because a list of
things nobody wants decided unattended is worth least at the exact moment the
dial is highest. Run with `--no-quorum` to fall back to the worker's status
line.

## Text-message escalations

When a run needs a human decision, Roscoe texts you and you reply from any
phone. No app, no webhook, no tunnel.

```
roscoe upgrade --phone +15551234567
```

One command links this machine to Roscoe's hosted number for $5/month: sign
in, confirm SMS consent, and finish checkout in the browser (or upgrade at
[roscoe.sh/account](https://roscoe.sh/account) first and link the CLI after).
Replies arrive over an outbound WebSocket and queue server-side while your
laptop sleeps. Inspect with `roscoe relay status`, tail replies with
`roscoe relay listen`.

## The day at a glance

```
roscoe top                       # spend today and this week, what is running here and on the fleet, recent sessions
roscoe top --watch 10s           # keep it on a second screen
```

Spend comes from the run ledgers: what the harness billed for its own
model, plus what the router priced for requests it forwarded to another
provider (tier 3), which the harness never sees. A model the harness does
not know (every routed one) it prices by guesswork, and that guess is left
out; older runs with no router record show a `*` and a footnote with the
token count instead. Fleet runs count once they are brought home. `--no-fleet` skips the ssh
probe.

When a cost needs explaining, `ROSCOE_DUMP_REQUESTS=<dir> roscoe run ...`
writes every request body the router forwards into that directory,
numbered in order and without headers, so two runs can be diffed. That is
how it was found that a worker's first message carries `git status` under
its own cache breakpoint, rewritten whenever the tree changes.

## Accounts

Workers run under the first account in `tiers.middle.accounts` whose token
is present. A token lives in the macOS keychain (`keychain:<service>`) or an
env var (`env:<VAR>`, from the env file). `roscoe accounts` shows every
account, whether its token is there (checked without reading it), how old a
minted token is, and which tier uses it. `roscoe accounts set <name>` stores
a token: the keychain tool prompts for it with echo off, so roscoe never sees
the value. Mint one with `claude setup-token`; they expire at twelve months
and the table says so from eleven. Every run prints which account it used,
or every reason none did. A Mac reached over ssh has a locked keychain, so
nodes use `env:` refs and `roscoe deploy --env`.

## More machines

Every machine under `nodes[]` in roscoe.json is reached over your own ssh
aliases and keys. Roscoe adds no daemon and opens no port.

```
roscoe node                      # what is on each machine, and what it needs
roscoe deploy                    # install roscoe there, pinned to this version, and push roscoe.json
roscoe deploy --node studio --claude --env   # one node; also install Claude Code and push the env file
ssh -t studio-ts claude auth login           # the one step deploy cannot do for you
roscoe run --node studio "port the tests"    # run a task there; its stream and your keys pass through ssh
roscoe dispatch "port the tests"             # same, on whichever node has the most free worker slots
roscoe up                                    # deploy everywhere, then show what is left to do
```

`node` says `ready` or `needs roscoe, config, claude, env`, and its last line
is the one command that gets the fleet closer. `deploy` pins the install to
the version you are running (`ROSCOE_VERSION` in the installer), so nodes never
disagree about what roscoe is. The env file is your API keys and only moves
with `--env`; it lands with mode 600.

After a fleet run its ledger is copied home and tagged with the node, so
`roscoe sessions` lists it as `on <node>` next to local runs;
`roscoe sessions --node <name>` (or `all`) pulls any it missed.
`dispatch` probes every node at once (about half a second for a fleet) and
picks the ready one with the most free worker slots, `nodes[].workers` less
the `roscoe run` processes on it; ties go to the first name. When nothing can
take work it shows the whole table so every node's reason is on screen.
`status` is the table. `run --node` probes the node first and refuses in one
line if it is not ready, naming the fix. The task runs in `~/.roscoe/work/<task-id>` on the
node unless `--dir` names a checkout there; its ledger and session stay on
the node. `claude auth status` is what the table trusts for login, and it is
Claude Code's own belief: a credential that has expired reads as logged in
until its first use, which is when Claude Code discovers it and clears it.

## Status

Early and moving fast. Working today: `init`, `config`, `router`, `smoke`,
`chat` (a conversation with one worker), `loop` (a charter worked to
completion), `memory` (the Graphify knowledge graph), `run` (single-node; Claude Code
workers with full subagent swarms, or Codex workers via `--harness codex` /
`tiers.middle.harness`), `notify`,
`upgrade` + `relay` (hosted SMS), `version`. Codex workers are single-agent
for now: the tier-3 swarm and `--resume` are Claude Code features. Under
`harness: codex`, `tiers.middle.model` is passed through when it is a codex
model; a claude alias left over from the default is not, and the settings
screen names the model codex will actually run instead. Multi-node:
`node`/`status` (what is on each machine), `deploy` and `up` (put roscoe
there, pinned to your version), `run --node` (one task on one machine) and
`dispatch` (one task on the freest machine) work today; what is still in
flight is
as are the account vault (`accounts`), MCP dispatch into your interactive
session, and a `roscoe top` TUI.
Pure Go; a single dependency (`coder/websocket`, for the relay bridge).

## Requirements

- [Claude Code](https://claude.com/claude-code) ≥ 2.1.x on each worker node
- A model provider for tier 3: a DeepInfra API key, a local
  Ollama/LM Studio serving an Anthropic-compatible endpoint, or Anthropic
- macOS or Linux, arm64/amd64
