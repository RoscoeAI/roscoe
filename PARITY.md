# Claude Code parity

Where roscoe stands against the tool people already know, as of v0.27.0
(2026-08-31). Parity is a means, not the goal: roscoe should feel familiar
enough that nobody has to relearn the basics, then do things Claude Code
does not.

**Everyday loop: ~87%. Whole surface: ~51%.**

Roscoe starts with an unfair advantage: a worker *is* Claude Code
(`claude -p`), so every tool, permission mode, hook, skill, and CLAUDE.md
comes along for free. What roscoe owes you is the shell around it — the
conversation, the controls, the session handling.

## The everyday loop

| | Claude Code | roscoe | Notes |
|---|---|---|---|
| Conversation with memory | yes | **yes** | `roscoe chat`, one session per chat |
| Pinned input box | yes | **yes** | bordered, bottom-anchored, resize-aware |
| Prompt history | yes | **yes** | up and down walk previous prompts once the line is empty |
| Line editing | yes | **yes** | left/right, home/end, word jumps, delete, and the readline kill keys; the same editor serves the during-turn box |
| Multi-line input | yes | **yes** | bracketed paste keeps a pasted block whole; alt-enter or a trailing backslash adds a line; the box grows to fit |
| Session picker | yes | **yes** | `roscoe sessions` lists runs with cost and first prompt; `chat --last` and `chat --pick` resume without an id |
| Scrollback | yes | **yes** | arrows and page up/down move the viewport over the whole conversation |
| Live output while working | yes | **yes** | tool calls as they happen, and the reply streams as it is written |
| Interrupt mid-turn | Esc | **Esc** | stops at a clean point, then you redirect |
| Resume a session | `--resume` | **yes, better** | trims oversized transcripts so long chats reload at all |
| Replay history on resume | yes | **yes** | last exchanges reprinted above the prompt |
| Tools (read/edit/bash/search) | yes | **yes** | inherited; the worker is Claude Code |
| Subagents | yes | **yes, cheaper** | routed to GLM-5.3-Flash, 8 wide by default |
| Slash settings | yes | **yes, deeper** | `/settings` puts all three tiers on one screen and edits them in place; `/config` walks the rest of the schema a level at a time, describing each setting as you type |
| Cost visibility | yes | **yes** | per turn and running total |
| Your auth and billing | yes | **yes** | runs under your own login |
| Web search and fetch | yes | **yes** | in the default allowed tools |

## Missing from the basics

The gaps you feel within a minute of typing:

| | Status | Why it matters |
|---|---|---|
| Permission prompts | **no** | workers run pre-approved; there is no "allow this once?" |
| Images in a message | **no** | text only |
| `@file` mentions and completion | **no** | say the path instead |
| MCP servers | **no** | roscoe does not pass `--mcp-config` yet |
| Plan mode, checkpoints, rewind | **no** | no equivalent |
| Status line, themes | **no** | one look |

## Where roscoe is not trying to match

Deviation is the point in these:

- **Three tiers.** One planning model, headless workers, swarms of cheap
  subagents. Claude Code has one session and its subagents.
- **A dial for autonomy.** `autonomy.level` decides how much a quorum
  absorbs before a human is asked. Nothing like it exists in Claude Code.
  *(Set and displayed today; enforcement lands with the quorum.)*
- **Escalation by text message.** Claude Code waits for you; roscoe texts
  you and takes your reply as the answer.
- **A fleet across machines.** `up`/`node`/`deploy` over ssh. Coming.
- **Memory that compounds.** A Graphify knowledge graph under `~/.roscoe`.
  Planned.
- **Either harness.** Claude Code or Codex workers behind one interface.
- **Workers that orchestrate.** Every worker runs at `ultracode` effort, so a
  substantive task is planned as a workflow and fanned out to the cheap tier
  instead of ground through in one expensive thread.
- **A ledger of everything.** Every event of every task in
  `~/.roscoe/runs/<id>/events.jsonl`, greppable and tailable.

## What to build next for parity that is felt

1. **MCP passthrough**, so existing servers work in workers.

Permission prompts are deliberately further down: a fleet running at
autonomy 90 is meant to keep going, and the quorum plus SMS escalation is
roscoe's answer to the same question.
