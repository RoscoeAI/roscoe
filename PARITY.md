# Claude Code parity

Where roscoe stands against the tool people already know, as of v0.8.0
(2026-09-01). Parity is a means, not the goal: roscoe should feel familiar
enough that nobody has to relearn the basics, then do things Claude Code
does not.

**Everyday loop: ~70%. Whole surface: ~40%.**

Roscoe starts with an unfair advantage: a worker *is* Claude Code
(`claude -p`), so every tool, permission mode, hook, skill, and CLAUDE.md
comes along for free. What roscoe owes you is the shell around it — the
conversation, the controls, the session handling.

## The everyday loop

| | Claude Code | roscoe | Notes |
|---|---|---|---|
| Conversation with memory | yes | **yes** | `roscoe chat`, one session per chat |
| Pinned input box | yes | **yes** | bordered, bottom-anchored, resize-aware |
| Live output while working | yes | **partial** | one line per event; no token-by-token streaming |
| Interrupt mid-turn | Esc | **Esc** | stops at a clean point, then you redirect |
| Resume a session | `--resume` | **yes, better** | trims oversized transcripts so long chats reload at all |
| Replay history on resume | yes | **yes** | last exchanges reprinted above the prompt |
| Tools (read/edit/bash/search) | yes | **yes** | inherited; the worker is Claude Code |
| Subagents | yes | **yes, cheaper** | routed to GLM-5.3-Flash, 8 wide by default |
| Slash settings | yes | **partial** | `/model /harness /autonomy /subagents /config /cost /session /new /exit` |
| Cost visibility | yes | **yes** | per turn and running total |
| Your auth and billing | yes | **yes** | runs under your own login |
| Web search and fetch | yes | **yes** | in the default allowed tools |

## Missing from the basics

The gaps you feel within a minute of typing:

| | Status | Why it matters |
|---|---|---|
| Line editing (arrows, word jump) | **no** | backspace only; arrows are ignored rather than fatal (v0.8.0) |
| Prompt history (up arrow) | **no** | retyping a long prompt is the most common annoyance |
| Multi-line input | **no** | enter sends; no shift-enter or paste-safe entry |
| Permission prompts | **no** | workers run pre-approved; there is no "allow this once?" |
| Session picker | **no** | you need the session id; no `roscoe sessions` |
| Streaming assistant text | **no** | the reply lands whole when the turn ends |
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
- **A ledger of everything.** Every event of every task in
  `~/.roscoe/runs/<id>/events.jsonl`, greppable and tailable.

## What to build next for parity that is felt

1. **Line editing and prompt history.** Cheap, and the first thing anyone
   notices.
2. **Multi-line input.** Paste a stack trace without sending it early.
3. **Session picker** (`roscoe sessions`, `chat --last`).
4. **Streaming assistant text**, so a long answer is not a silent wait.
5. **MCP passthrough**, so existing servers work in workers.

Permission prompts are deliberately further down: a fleet running at
autonomy 90 is meant to keep going, and the quorum plus SMS escalation is
roscoe's answer to the same question.
