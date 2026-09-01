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
  [Graphify](https://github.com/Graphify-Labs/graphify), with the knowledge
  graph living at `~/.roscoe/graph` next to the rest of the fleet state. Runs,
  quorum decisions, and outcomes feed the graph, so the fleet learns your
  codebase and its own mistakes as it goes.

```
curl -fsSL https://roscoe.sh/install | sh
```

## How it works

- **Tier 1, main.** Your stock interactive `claude` session (or `roscoe run`
  headless). Nothing about your daily driver changes.
- **Tier 2, workers.** Spawn-per-task `claude -p` sessions: the full Claude
  Code harness (tools, subagents, Workflows), headless, one fresh git worktree
  and isolated `CLAUDE_CONFIG_DIR` per task. Workers run at
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
```

Every config value is CLI-settable (`roscoe config set quorum.enabled true`);
precedence is flag > `ROSCOE_*` env > `roscoe.json` > defaults. See
[`roscoe.example.json`](roscoe.example.json) for the full shape and
[`SPEC.md`](SPEC.md) for the package contracts.

Nobody memorizes a config schema, so in `roscoe chat` the settings are
something you look at rather than recall. `/settings` puts all three tiers on
one screen, each with its model, provider, and effort, and says plainly where
a knob does not exist rather than leaving a gap:

```
  tier 1   your session, the one you talk to
  › model      opus
    provider   anthropic
    effort     yours to set, not roscoe's: this is your own claude session

  tier 2   workers, one spawned per task
    model      sonnet
    provider   anthropic
    effort     ultracode
    harness    claude

  tier 3   the swarm each worker fans out to
    model      zai-org/GLM-5.3-Flash
    provider   deepinfra
    effort     claude code has no per-subagent effort knob; tier 2's applies
    width      8 at once
```

Up and down move, left and right step through a setting's values, enter types
one in, esc closes. Every change is validated and written to `roscoe.json` as
you make it.

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
and durable notes. Each iteration reads it and rewrites it, and sets a status
of `continuing`, `done`, or `blocked`. `done` ends the run, `blocked` escalates.
The iteration ceiling and the budget are the loop's to enforce, not the
worker's, so no judgment call can spend past them. Esc stops after the current
iteration so the worker still writes its memory rather than being cut off.

Today the judge takes the worker at its word. The quorum replaces it: several
models read the result and the file and decide done, retry, follow up, or
escalate, with `autonomy.level` setting how much they absorb before you get a
text.

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

## Status

Early and moving fast. Working today: `init`, `config`, `router`, `smoke`,
`chat` (a conversation with one worker), `loop` (a charter worked to
completion), `run` (single-node; Claude Code
workers with full subagent swarms, or Codex workers via `--harness codex` /
`tiers.middle.harness`), `notify`,
`upgrade` + `relay` (hosted SMS), `version`. Codex workers are single-agent
for now: the tier-3 swarm and `--resume` are Claude Code features. In
flight: multi-node ssh fan-out (`up`/`node`/`deploy`), the account vault
(`accounts`), the quorum judging each loop iteration with the autonomy dial
enforced, Graphify-backed
memory, MCP dispatch into your interactive session, and a `roscoe top` TUI.
Pure Go; a single dependency (`coder/websocket`, for the relay bridge).

## Requirements

- [Claude Code](https://claude.com/claude-code) ≥ 2.1.x on each worker node
- A model provider for tier 3: a DeepInfra API key, a local
  Ollama/LM Studio serving an Anthropic-compatible endpoint, or Anthropic
- macOS or Linux, arm64/amd64
