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
bun run build             # compile platform binary (auto-detects host)
bun run build:win         # force Windows target
bun run build:mac         # force macOS target (builds BOTH arm64 + x64)
bun run install:dev       # link the .sdPlugin into SD's Plugins dir
bun run install:dev --restart   # + kill & relaunch Stream Deck
bun run sync:codexbar     # refresh tmp/CodexBar from upstream
```

`bun run build` auto-stops Stream Deck before compiling (to release
the exclusive lock on the running .exe / mach-o binary) and
auto-relaunches it after. Pass `--no-reload` to skip the
kill/relaunch dance (useful for CI or cross-compilation from a
different host).

### macOS native dual-arch build

`build:mac` produces THREE files in `bin/`:

  plugin-mac-arm64    — native Apple Silicon (bun-darwin-arm64)
  plugin-mac-x64      — native Intel          (bun-darwin-x64)
  plugin-mac          — shell wrapper that exec's the right one
                        based on `uname -m` at launch time

Stream Deck's `manifest.json` points `CodePathMac` at the wrapper.
First byte dispatched natively, zero Rosetta. `install:dev` on a
Mac host also runs `chmod +x` on all three files + strips any
`com.apple.quarantine` xattr so Gatekeeper doesn't prompt on first
launch.

Cross-compilation from Windows to Mac works via
`bun run build:mac` on a Windows host — Bun handles the target
switch natively. Move the resulting bin/ files to a Mac and run
`bun run install:dev` there to fix executable bits + quarantine.

## Releasing

```
bun run release patch   # or minor / major / explicit version
```

The release script bumps the version in `manifest.json` and
`package.json`, commits, tags, and pushes. The GitHub Actions
workflow builds and publishes the release from the tag.

**Important:** after cutting a release, always rebuild and reinstall
locally so the running binary matches the new version:

```
bun run build
```

If you skip this, the plugin's built-in update checker will see the
new GitHub Release, compare it against the stale compiled-in version,
and block every button with an "UPDATE" face — on your own dev
machine.

## Stream Deck plugin notes

- Plugin UUID: `io.github.anthonybaldwin.UsageButtons`
- Manifest at `io.github.anthonybaldwin.UsageButtons.sdPlugin/manifest.json`
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
