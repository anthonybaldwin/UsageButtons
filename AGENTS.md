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
- Only external dep: `github.com/coder/websocket`.
- Single static binary per platform, low memory footprint.

## Repo layout

See `README.md`.

## Build & run

```
# Plugin binary
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe ./cmd/plugin/
GOOS=darwin GOARCH=arm64 go build -o ...sdPlugin/bin/plugin-mac-arm64 ./cmd/plugin/
GOOS=darwin GOARCH=amd64 go build -o ...sdPlugin/bin/plugin-mac-x64 ./cmd/plugin/

# Native-messaging host binary (ships alongside the plugin as the
# bridge to the Usage Buttons Helper extension). See internal/cookies/
# and chrome-extension/.
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/usagebuttons-native-host-win.exe ./cmd/native-host/
GOOS=darwin GOARCH=arm64 go build -o ...sdPlugin/bin/usagebuttons-native-host-mac-arm64 ./cmd/native-host/
GOOS=darwin GOARCH=amd64 go build -o ...sdPlugin/bin/usagebuttons-native-host-mac-x64 ./cmd/native-host/

go vet ./...                                                                                    # lint
go test ./...                                                                                   # tests
./scripts/install-dev.sh --restart                                                              # link + restart SD
./scripts/sync-codexbar.sh                                                                      # refresh CodexBar ref
```

### macOS dual-arch

The release workflow builds two binaries (`plugin-mac-arm64`, `plugin-mac-x64`)
and writes a shell wrapper (`plugin-mac`) that dispatches via `uname -m`.
`manifest.json` points `CodePathMac` at the wrapper.

Cross-compilation from any host: `GOOS=darwin GOARCH=arm64 go build ...`

## Releasing

Releases are cut via the `release` workflow — manually dispatched
from GitHub Actions or the CLI:

```
gh workflow run release.yml --field bump=patch   # 0.3.2 → 0.3.3
gh workflow run release.yml --field bump=minor   # 0.3.2 → 0.4.0
gh workflow run release.yml --field bump=major   # 0.3.2 → 1.0.0
gh workflow run release.yml --field bump=custom --field custom_version=0.4.0
gh workflow run release.yml --field bump=patch --field draft=true  # draft release
```

No local releasing — the workflow bumps both manifests (plugin +
Helper extension), commits to `main`, tags, builds binaries for
Windows + macOS (both arches), packages the Helper zip, and
publishes the GitHub Release with all three artifacts attached.

**After cutting a release**, pull + rebuild locally so your dev
binary matches the new version (otherwise the update checker on
your own machine shows an "UPDATE" face on every button):

```
git pull
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe ./cmd/plugin/
go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/usagebuttons-native-host-win.exe ./cmd/native-host/
```

## GitHub repo metadata

**When adding/removing a provider, changing the runtime, or shifting
project scope, update ALL of these in the same commit or PR:**

1. GitHub topics: `gh repo edit --add-topic foo --remove-topic bar`
2. GitHub description: `gh repo edit --description "..."`
3. README.md (repo layout, provider list, build instructions)
4. docs/index.html (website — providers list, install steps, features)
5. AGENTS.md (this file — topics list, build commands)

This is a hard rule, not a nice-to-have. Stale metadata confuses
users and search engines.

Current topics: `go`, `golang`, `stream-deck`, `stream-deck-plugin`,
`elgato`, `ai-tools`, `usage-monitoring`, `claude`, `copilot`,
`cursor`, `ollama`, `openrouter`, `openai-codex`, `warp`.

## Stream Deck plugin notes

- Plugin UUID: `io.github.anthonybaldwin.UsageButtons`
- Manifest at `io.github.anthonybaldwin.UsageButtons.sdPlugin/manifest.json`
- `CodePathWin` -> `bin/plugin-win.exe`; `CodePathMac` -> `bin/plugin-mac`.
- Plugin connects to Stream Deck over a local WebSocket; see
  `internal/streamdeck/` for the protocol wrapper.
- Button images are sent as SVG data URIs via `setImage`.
- Metric labels are rendered via the SDK's native title (`setTitle`),
  not in the SVG. All actions ship with `UserTitleEnabled: true` and
  `ShowTitle: true` so users can override the label per-key from the
  Stream Deck UI. The SVG owns the value, glyph, and ratio fill; the
  title bar owns the label text. Send labels in UPPERCASE
  (`SESSION`, `WEEKLY`, …) to match the title font's expected look.

## Browser fetch bridge (Usage Buttons Helper extension)

Cookie-gated providers (Claude web extras, Cursor, Ollama) route
requests through the companion Chrome extension in
`chrome-extension/` (Usage Buttons Helper), which proxies `fetch()`
for a narrow allowlist of origins. Cookies never leave the browser —
the plugin only sees API response bodies.

Architecture:

- `chrome-extension/` — MV3 service worker that holds a persistent
  `connectNative` port to the native host, proxies `fetch()` for
  `claude.ai` / `cursor.com` / `ollama.com`, and passes base64 bodies
  on the wire.
- `cmd/native-host/` — Go binary Chrome spawns on `connectNative`.
  Reads/writes Chrome's stdin/stdout framing, listens on a local
  AF_UNIX socket for the plugin, and correlates plugin requests to
  extension replies by request ID.
- `internal/cookies/` — shared Go package: frame codec, protocol
  types, `Bridge`, IPC transport, install helpers for all major
  Chromium-based browsers (Chrome, Edge, Brave, Chromium) plus
  Firefox paths (pre-wired for a future Firefox extension).
- `internal/providers/cookieaux/` — user-facing message helpers
  (`MissingMessage`, `StaleMessage`) shared by the three cookie-gated
  providers so they surface consistent snapshot-error strings.

Hard rule: cookie-gated providers must check `cookies.HostAvailable`
before dispatching any request. Cold-start (Stream Deck launched
before Chrome) stays quiet — no 403 loops.

Adding a cookie-gated provider requires three coordinated changes:

1. Go `cookies.Allowed` in `internal/cookies/allowed.go`
2. Extension `ALLOWED` in `chrome-extension/service-worker.js`
3. Extension `host_permissions` in `chrome-extension/manifest.json`

…plus cut a new plugin release so the Helper zip on GitHub Releases
matches.

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
