# UsageButtons

Stream Deck plugin that turns every AI-coding-assistant usage stat into a
live button — session % remaining, weekly %, credits, reset countdowns,
per-model quotas, and more. Each button renders a dynamic icon whose
background fills (or de-fills) in proportion to the current value, so
you can tell at a glance how much runway you have left.

Inspired by [CodexBar](https://github.com/steipete/CodexBar) (macOS menu
bar); this project targets the Stream Deck and runs on **macOS and
Windows**. It is not a fork of CodexBar — we consume the same public
provider data sources but ship an entirely separate plugin.

## Status

Early scaffolding. See `tmp/CodexBar/` (gitignored) for the reference
implementation we're porting concepts from.

## Runtime

- **[Bun](https://bun.sh)** for development and `bun build --compile` to
  produce a standalone native executable per OS. End users do **not**
  need Node or Bun installed — the Stream Deck launches the compiled
  binary directly.
- TypeScript everywhere.

## Repo layout

```
UsageButtons/
├── io.github.anthonybaldwin.UsageButtons.sdPlugin/  # Stream Deck plugin bundle
│   ├── manifest.json
│   ├── assets/                           # icons shipped with the plugin
│   └── bin/                              # compiled binaries (gitignored)
├── src/
│   ├── plugin.ts                         # websocket entrypoint
│   ├── render.ts                         # SVG button renderer
│   ├── streamdeck.ts                     # SD protocol types + helpers
│   └── providers/                        # usage data fetchers
├── scripts/
│   ├── build.ts                          # bun build --compile
│   └── sync-codexbar.sh                  # refresh tmp/CodexBar reference
├── tmp/CodexBar/                         # upstream reference (gitignored)
├── CLAUDE.md                             # Claude-specific agent notes
├── AGENTS.md                             # shared agent instructions
└── README.md
```

## Install (dev)

### Windows

1. `bun install`
2. `bun run build` — compiles `bin/plugin-win.exe` into the .sdPlugin
3. `bun run install:dev --restart` — junctions the .sdPlugin folder
   into `%APPDATA%\Elgato\StreamDeck\Plugins\` and relaunches
   Stream Deck
4. Add a "Usage Stat" action to a key and configure the provider +
   metric from the Property Inspector

### macOS (Apple Silicon or Intel)

1. `bun install`
2. `bun run build:mac` — produces native binaries for **both**
   architectures and an arch-dispatch wrapper:
   - `bin/plugin-mac-arm64` — native Apple Silicon
   - `bin/plugin-mac-x64` — native Intel
   - `bin/plugin-mac` — shell wrapper that picks the right binary
     at launch via `uname -m`. No Rosetta, no universal binary,
     just a tiny sh script that execs the matching native build.
3. `bun run install:dev --restart` — symlinks the .sdPlugin folder
   into `~/Library/Application Support/com.elgato.StreamDeck/Plugins/`,
   runs `chmod +x` on all three Mac files (the wrapper + both
   compiled binaries), strips any `com.apple.quarantine` xattr that
   would otherwise trip Gatekeeper on first launch, and relaunches
   Stream Deck.
4. Same Property Inspector flow as Windows.

You can cross-compile Mac binaries from a Windows host via
`bun run build:mac` on Windows — Bun's compiler handles the
`bun-darwin-arm64` / `bun-darwin-x64` targets directly. When you
move those binaries onto a Mac, `install:dev` restores the
executable bit and the quarantine-strip for you.

Full dev workflow lives in [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE) — same as [CodexBar](https://github.com/steipete/CodexBar),
the Swift macOS menu bar app this plugin takes its design cues from.
Provider icon paths and brand colors under `src/providers/` are adapted
from CodexBar's MIT-licensed resources; see
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md) for full attribution.
