# CLAUDE.md

**Read [AGENTS.md](AGENTS.md) first. It is the source of truth for
build commands, commit conventions, repo metadata rules, and project
context. Everything there applies to you. Do not skip it.**

This file contains only Claude Code-specific guidance (tool usage,
subagent patterns, memory). Anything a Codex/Cursor agent would also
benefit from belongs in AGENTS.md, not here.

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
