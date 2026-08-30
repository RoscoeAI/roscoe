# Roscoe

**One binary that runs your Claude Code fleet.**

Roscoe orchestrates N real Claude Code sessions in three tiers: your interactive
session delegates to headless Claude Code workers, which fan out to swarms of
subagents running on cheap open models — by default
[`zai-org/GLM-5.3-Flash`](https://deepinfra.com/zai-org/GLM-5.3-Flash) via
DeepInfra's Anthropic-compatible API. One JSON config decides where every tier's
model comes from (`deepinfra` | `local` | `anthropic`), which machines run
workers, and how many. When a run needs a human decision, a model quorum answers
the routine ones and the rare real one reaches you by SMS — you reply by text.

```
curl -fsSL https://roscoe.sh/install | sh
```

## How it works

- **Tier 1 — main.** Your stock interactive `claude` session (or `roscoe run`
  headless). Nothing about your daily driver changes.
- **Tier 2 — workers.** Spawn-per-task `claude -p` sessions — the full Claude
  Code harness (tools, subagents, Workflows), headless, one fresh git worktree
  and isolated `CLAUDE_CONFIG_DIR` per task.
- **Tier 3 — the swarm.** Native Claude Code subagents whose requests carry a
  virtual model name (`roscoe/tier3`). A ~400-line loopback reverse proxy
  dispatches each request by model name: `claude-*` passes through to Anthropic
  untouched (your own auth headers, byte-for-byte, prompt caching intact);
  the virtual name is rewritten to the configured tier-3 provider. No protocol
  translation anywhere — DeepInfra, Ollama, and LM Studio all speak the
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

## Status

Early and moving fast. Working today: `init`, `config`, `router`, `smoke`,
`run` (single-node), `notify` (Twilio SMS + ntfy), `version`. In flight:
multi-node ssh fan-out (`up`/`node`/`deploy`), the account vault
(`accounts`), MCP dispatch into your interactive session, the quorum, and a
`roscoe top` TUI. Built pure-Go, stdlib only, zero runtime dependencies.

## Requirements

- [Claude Code](https://claude.com/claude-code) ≥ 2.1.x on each worker node
- A model provider for tier 3: a DeepInfra API key, a local
  Ollama/LM Studio serving an Anthropic-compatible endpoint, or Anthropic
- macOS or Linux, arm64/amd64
