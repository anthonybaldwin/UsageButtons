# AGENTS.md

Shared instructions for any coding agent (Claude, Codex, Cursor, etc.)
working on this repo. Claude-specific notes live in `CLAUDE.md`.

## Mission

This is a Stream Deck plugin that displays AI-coding-assistant usage
stats (session %, weekly %, credits, reset countdowns, …) as live
buttons. Each stat can be added to a Stream Deck key, and each button
renders a dynamic SVG icon whose background fill grows or shrinks to
reflect the current value.

Cross-platform: **macOS and Windows**. Primary dev machine is Windows;
Mac builds happen later.

Reference implementation (macOS menu bar, Swift): `tmp/CodexBar/`.
Treat it as prior art to port concepts from, not code to vendor.

## Runtime

- **Bun** (already installed). Do not add a Node requirement.
- TypeScript with strict mode.
- Ship via `bun build --compile` → standalone native binary per OS
  (`bin/plugin-win.exe`, `bin/plugin-mac`). End users install **no**
  runtime.

## Repo layout

See `README.md`.

## Build & run

```
bun install               # install dev dependencies
bun run typecheck         # tsc --noEmit
bun run build             # compile platform binary into the .sdPlugin
bun run sync:codexbar     # refresh tmp/CodexBar from upstream
```

During development on Windows, copy or symlink
`com.baldwin.usage-buttons.sdPlugin/` into
`%APPDATA%\Elgato\StreamDeck\Plugins\` and restart Stream Deck after
each rebuild.

## Stream Deck plugin notes

- Plugin UUID: `com.baldwin.usage-buttons`
- Manifest at `com.baldwin.usage-buttons.sdPlugin/manifest.json`
- `CodePathWin` → `bin/plugin-win.exe`; `CodePathMac` → `bin/plugin-mac`.
- Plugin connects to Stream Deck over a local WebSocket; see
  `src/streamdeck.ts` for the protocol wrapper.
- Button images are sent as SVG data URIs via `setImage` — no canvas
  library, no PNG encoding.

## CodexBar reference

- `tmp/CodexBar/` is a git clone of
  https://github.com/steipete/CodexBar — gitignored.
- Refresh with `bun run sync:codexbar` (or
  `./scripts/sync-codexbar.sh`). This is a one-way pull, not a submodule.
- When implementing or modifying a provider, read the matching file
  under `tmp/CodexBar/Sources/CodexBarCore/Providers/<Name>/` and the
  doc at `tmp/CodexBar/docs/<provider>.md` first.

## Commit conventions

Use **Conventional Commits**:

```
<type>(<optional-scope>): <short imperative summary>

<optional body explaining the WHY>
```

Allowed types:

| type       | use for                                           |
|------------|---------------------------------------------------|
| `feat`     | new feature visible to the user                   |
| `fix`      | bug fix                                           |
| `docs`     | README / CLAUDE.md / AGENTS.md / inline docs      |
| `chore`    | tooling, config, deps, gitignore, lockfile        |
| `refactor` | code change that is neither feat nor fix          |
| `perf`     | performance improvement                           |
| `test`     | adding or fixing tests                            |
| `build`    | build system, bun compile scripts                 |
| `ci`       | CI config                                         |
| `style`    | formatting only, no code change                   |
| `revert`   | reverting a prior commit                          |

Examples:

```
feat(providers): add Claude OAuth fetcher
fix(render): clamp fill to [0,100] to avoid SVG overflow
chore: gitignore tmp/ and plugin bin output
docs(agents): document bun build --compile workflow
```

Rules:

1. **Commit often.** After each logical task is done and the repo is in
   a green state, commit. Do **not** batch unrelated changes.
2. **Before saying a task is complete** or moving to the next task,
   commit the work for that task. A task is not done until it is
   committed.
3. Keep the subject ≤ 72 chars, imperative mood ("add", not "added").
4. Put the *why* in the body when the change is non-obvious.
5. Never use `git commit --amend` on anything that has been pushed.
6. Never use `--no-verify` to skip hooks.

## What NOT to do

- Do not vendor CodexBar code into `src/`. Port ideas, write fresh TS.
- Do not add a Node.js dependency to the runtime. Dev deps are fine.
- Do not store secrets in the repo. Use Stream Deck action settings
  (per-action) or `~/.codexbar/config.json` (shared with CodexBar) or
  env vars.
- Do not crawl the user's filesystem. Only read the specific well-known
  paths a given provider documents.
