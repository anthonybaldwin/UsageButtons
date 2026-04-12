# CLAUDE.md

Claude-specific guidance for this repo. Shared instructions live in
[AGENTS.md](AGENTS.md) — read that first. Only put things **here** that
are genuinely Claude-specific (workflow conveniences, Claude Code tool
usage, memory notes). Anything a Codex/Cursor agent would also benefit
from belongs in AGENTS.md.

## Claude Code workflow

- **Prefer subagents for CodexBar archaeology.** CodexBar is a large
  Swift codebase; reading it inline blows through context. Spawn an
  Explore agent with a specific, narrow question.
- **Ask before destructive git actions.** User explicitly asked for
  safety around git.

## Commands the user may invoke

If the user references `/commit`, that's the Claude Code commit skill —
use the Skill tool. Commit messages in this repo must follow the
Conventional Commits format described in AGENTS.md.

## Memory

Save project facts via Claude Code's built-in auto-memory system
(stored in the per-project memory directory under `~/.claude/projects/`).
Do not duplicate facts already in AGENTS.md or derivable from the code.
