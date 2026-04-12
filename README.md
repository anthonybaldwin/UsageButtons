# UsageButtons

Stream Deck plugin that turns every AI-coding-assistant usage stat into a
live button — session % remaining, weekly %, credits, reset countdowns,
per-model quotas, and more. Each button renders a dynamic icon whose
background fills (or de-fills) in proportion to the current value, so
you can tell at a glance how much runway you have left.

![Usage Buttons in Stream Deck](docs/main.png)

Inspired by [CodexBar](https://github.com/steipete/CodexBar) for macOS.
Runs on **Windows and macOS**.

## Settings

Each provider is its own action — drag **Claude**, **Codex**, **Copilot**,
etc. onto a key and configure the metric, colors, and thresholds from the
Property Inspector.

| Per-button settings | Plugin-wide defaults |
|---|---|
| ![Button settings](docs/button-settings.png) | ![Plugin settings](docs/plugin-settings.png) |

## Runtime

Written in **Go**. Compiles to a single static binary per platform —
no runtime dependencies. End users just install the plugin; the
Stream Deck software launches the binary directly.

- **Single binary**, no runtime dependencies
- **Low memory footprint**
- **Only external dep**: [gorilla/websocket](https://github.com/gorilla/websocket)

## Repo layout

```
UsageButtons/
├── io.github.anthonybaldwin.UsageButtons.sdPlugin/  # Stream Deck plugin bundle
│   ├── manifest.json
│   ├── ui/                               # Property Inspector HTML
│   ├── assets/                           # icons shipped with the plugin
│   └── bin/                              # compiled binaries (gitignored)
├── cmd/plugin/                           # Go entry point
├── internal/
│   ├── streamdeck/                       # SD WebSocket protocol
│   ├── render/                           # SVG button renderer + bbox
│   ├── providers/                        # provider interface, cache, mock
│   │   ├── claude/                       # Claude (OAuth + cookie web API)
│   │   ├── codex/                        # Codex (OAuth)
│   │   ├── copilot/                      # GitHub Copilot
│   │   ├── cursor/                       # Cursor (cookie)
│   │   ├── ollama/                       # Ollama (cookie)
│   │   ├── openrouter/                   # OpenRouter (API key)
│   │   ├── warp/                         # Warp (GraphQL)
│   │   ├── zai/                          # z.ai (API token)
│   │   └── kimik2/                       # Kimi K2 (API key)
│   ├── settings/                         # global + per-key settings
│   ├── icons/                            # provider SVG glyph paths
│   ├── update/                           # GitHub Releases update checker
│   └── httputil/                         # shared HTTP+JSON helpers
├── scripts/                              # build, release, icon generation
├── tmp/CodexBar/                         # upstream reference (gitignored)
├── CLAUDE.md                             # Claude-specific agent notes
├── AGENTS.md                             # shared agent instructions
└── README.md
```

## Build from source

### Windows

1. Install [Go](https://go.dev/dl/)
2. `go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe ./cmd/plugin/`
3. `./scripts/install-dev.sh --restart` — junctions the .sdPlugin folder
   into `%APPDATA%\Elgato\StreamDeck\Plugins\` and relaunches
   Stream Deck
4. Drag a provider (e.g. **Claude**, **Codex**, **Copilot**) onto a key
   and pick a metric from the Property Inspector

### macOS

1. Install [Go](https://go.dev/dl/)
2. `GOOS=darwin GOARCH=arm64 go build -o io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-mac ./cmd/plugin/`
3. `./scripts/install-dev.sh --restart`
4. Same Property Inspector flow as Windows

Cross-compilation from Windows: `GOOS=darwin GOARCH=arm64 go build ...`

Full dev workflow lives in [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE) · [Third-party licenses](THIRD_PARTY_LICENSES.md)
