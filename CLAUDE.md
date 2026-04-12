# CLAUDE.md

Claude-specific guidance for this repo. Shared instructions live in
[AGENTS.md](AGENTS.md) — read that first. Only put things **here** that
are genuinely Claude-specific (workflow conveniences, Claude Code tool
usage, memory notes). Anything a Codex/Cursor agent would also benefit
from belongs in AGENTS.md.

## Working style in this repo

- **Review CodexBar before implementing any provider.** Dispatch an
  Explore subagent on `tmp/CodexBar/Sources/CodexBarCore/Providers/<X>/`
  + `tmp/CodexBar/docs/<x>.md` before writing a new fetcher. Do not
  assume the protocol from memory.
- **Commit after every task.** See the Conventional Commits rules in
  `AGENTS.md`. A task is not "complete" until its commit exists.
- **Prefer subagents for CodexBar archaeology.** CodexBar is a large
  Swift codebase; reading it inline blows through context. Spawn an
  Explore agent with a specific, narrow question.
- **Ask before destructive git actions.** User explicitly asked for
  safety around git.

## Commands the user may invoke

If the user references `/commit`, that's the Claude Code commit skill —
use the Skill tool. Commit messages in this repo must follow the
Conventional Commits format described in AGENTS.md (`feat(scope): …`,
`fix: …`, etc.).

## Memory hygiene

Facts worth remembering across sessions for this project:
- User runs Windows, only has Windows to test on — Mac builds are
  aspirational until we get hardware access.
- User prefers Bun over Node everywhere possible.
- The CodexBar clone in `tmp/` is **reference only**; never vendor its
  code into `src/`.
- Commit-per-task is a hard rule, not a preference.

Save new facts via Claude Code's built-in auto-memory system
(stored outside the repo, in the per-project memory directory
under your `~/.claude/projects/...` tree — the exact path is
session-derived, don't hardcode it here).

## SDK quirks Claude should remember

- Stream Deck SDK docs assume Node.js 24+. We're deliberately ignoring
  that; we compile with `bun build --compile` and the Stream Deck
  software just launches the resulting native binary. Do not
  reintroduce a Node runtime dependency.
- `Nodejs` block in `manifest.json` is **intentionally omitted** so the
  Stream Deck software treats our `CodePathWin` / `CodePathMac` as
  plain executables.
- Stream Deck accepts SVG data URIs from `setImage`. Use that instead
  of pulling in a canvas library.

## Testing on Windows

The Stream Deck app on Windows reloads plugins when you restart it, or
when a `.sdPlugin` folder is dropped into
`%APPDATA%\Elgato\StreamDeck\Plugins\`. The simplest dev loop:

1. Build: `bun build`
2. Kill the Stream Deck process (`StreamDeck.exe`) from Task Manager.
3. Relaunch Stream Deck.

We can automate this later with a `bun dev` watcher.
