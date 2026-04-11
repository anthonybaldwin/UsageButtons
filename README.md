# usage-buttons

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
usage-buttons/
├── com.baldwin.usage-buttons.sdPlugin/   # Stream Deck plugin bundle
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

## Install (dev, Windows)

1. `bun install`
2. `bun run build` — compiles `bin/plugin-win.exe` into the .sdPlugin
3. Copy the `.sdPlugin/` folder to
   `%APPDATA%\Elgato\StreamDeck\Plugins\` (or symlink it) and restart
   the Stream Deck app.
4. Add a "Usage" action to a key and configure the provider + metric.

Full dev workflow lives in [AGENTS.md](AGENTS.md).

## License

TBD.
