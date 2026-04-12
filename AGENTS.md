# AGENTS.md

Shared instructions for any coding agent (Claude, Codex, Cursor, etc.)
working on this repo. Claude-specific notes live in `CLAUDE.md`.

## Mission

This is a Stream Deck plugin that displays AI-coding-assistant usage
stats (session %, weekly %, credits, reset countdowns, ...) as live
buttons. Each stat can be added to a Stream Deck key, and each button
renders a dynamic SVG icon whose background fill grows or shrinks to
reflect the current value.

Cross-platform: **macOS and Windows**. Primary dev machine is Windows;
Mac builds happen later.

Reference implementation (macOS menu bar, Swift): `tmp/CodexBar/`.
Treat it as prior art to port concepts from, not code to vendor.

## Runtime

- **Go** (1.26+). Single static binary per platform.
- Only external dep: `github.com/gorilla/websocket`.
- Binary: ~10MB on disk, ~5MB RSS at runtime.

## Repo layout

See `README.md`.

## Build & run

```
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe ./cmd/plugin/   # Windows
GOOS=darwin GOARCH=arm64 go build -o ...sdPlugin/bin/plugin-mac-arm64 ./cmd/plugin/            # macOS arm64
GOOS=darwin GOARCH=amd64 go build -o ...sdPlugin/bin/plugin-mac-x64 ./cmd/plugin/              # macOS x64
go vet ./...                                                                                    # lint
./scripts/install-dev.sh --restart                                                              # link + restart SD
./scripts/sync-codexbar.sh                                                                      # refresh CodexBar ref
```

### macOS dual-arch

The release workflow builds two binaries (`plugin-mac-arm64`, `plugin-mac-x64`)
and writes a shell wrapper (`plugin-mac`) that dispatches via `uname -m`.
`manifest.json` points `CodePathMac` at the wrapper.

Cross-compilation from any host: `GOOS=darwin GOARCH=arm64 go build ...`

## Releasing

```
./scripts/release.sh patch   # or minor / major / explicit version
```

The release script bumps the version in `manifest.json`, commits,
tags, and pushes. The GitHub Actions workflow builds and publishes
the release from the tag.

**Important:** after cutting a release, always rebuild locally so the
running binary matches the new version:

```
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe ./cmd/plugin/
```

If you skip this, the plugin's update checker will block every button
with an "UPDATE" face on your own dev machine.

## GitHub repo metadata

Keep the repo description, topics, and homepage in sync when
providers are added/removed or the project scope changes:

```
gh repo edit --description "..." --add-topic foo --remove-topic bar
```

Current topics: `go`, `golang`, `stream-deck`, `stream-deck-plugin`,
`elgato`, `ai-tools`, `usage-monitoring`, `claude`, `copilot`, `cursor`.

## Stream Deck plugin notes

- Plugin UUID: `io.github.anthonybaldwin.UsageButtons`
- Manifest at `io.github.anthonybaldwin.UsageButtons.sdPlugin/manifest.json`
- `CodePathWin` -> `bin/plugin-win.exe`; `CodePathMac` -> `bin/plugin-mac`.
- Plugin connects to Stream Deck over a local WebSocket; see
  `internal/streamdeck/` for the protocol wrapper.
- Button images are sent as SVG data URIs via `setImage`.
- `UserTitleEnabled: false` on all actions — we own the full 144x144
  canvas. Never use `setTitle()` or re-enable native titles.

## CodexBar reference

- `tmp/CodexBar/` is a git clone of
  https://github.com/steipete/CodexBar — gitignored.
- Refresh with `./scripts/sync-codexbar.sh`.
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
| `chore`    | tooling, config, deps, gitignore                  |
| `refactor` | code change that is neither feat nor fix          |
| `perf`     | performance improvement                           |
| `test`     | adding or fixing tests                            |
| `build`    | build system, compile scripts                     |
| `ci`       | CI config                                         |
| `style`    | formatting only, no code change                   |
| `revert`   | reverting a prior commit                          |

Rules:

1. **Commit often.** After each logical task is done and the repo is in
   a green state, commit. Do **not** batch unrelated changes.
2. **Before saying a task is complete** or moving to the next task,
   commit the work for that task. A task is not done until it is
   committed.
3. Keep the subject <= 72 chars, imperative mood ("add", not "added").
4. Put the *why* in the body when the change is non-obvious.
5. Never use `git commit --amend` on anything that has been pushed.
6. Never use `--no-verify` to skip hooks.

## What NOT to do

- Do not vendor CodexBar code. Port ideas, write fresh Go.
- Do not store secrets in the repo. Use Stream Deck action settings
  or env vars.
- Do not crawl the user's filesystem. Only read the specific well-known
  paths a given provider documents.
