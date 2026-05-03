# Third-party licenses

UsageButtons bundles assets and reference material from other open
source projects. Their licenses are reproduced / referenced below as
required.

## CodexBar — MIT

<https://github.com/steipete/CodexBar>

Copyright (c) 2026 Peter Steinberger

The following are adapted from CodexBar's
`Sources/CodexBar/Resources/ProviderIcon-*.svg` assets, which are
distributed under the MIT license:

- `internal/icons/<name>.go` and the literal in `internal/icons/icons.go`
  — the `d` attribute of each provider's SVG is embedded into a Go
  map for compile-time inlining. The path data is unmodified from
  upstream; only the surrounding Go map structure is ours. CodexBar
  is the source for the small set of providers that lobehub/lobe-icons
  does not ship a glyph for (warp, factory, abacus, augment, jetbrains,
  kiro, opencodego, synthetic). The remaining ~27 provider glyphs come
  from lobehub/lobe-icons via `scripts/sync-lobe-icons.go` and are
  emitted into `internal/icons/lobe_generated.go`; see the
  lobehub/lobe-icons section below.

- The `BrandColor()` / `BrandBg()` constants on each Go provider
  under `internal/providers/<name>/` mirror the RGB values from
  CodexBar's `<Name>ProviderDescriptor.swift` `branding` blocks.

Full MIT license text is reproduced in `tmp/CodexBar/LICENSE` when
the CodexBar reference clone is present. Authoritative source:
<https://github.com/steipete/CodexBar/blob/main/LICENSE>

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the condition that the above copyright notice and this permission
notice be included in all copies or substantial portions of the
Software. The Software is provided "AS IS", without warranty of any
kind.

## openusage — MIT

<https://github.com/robinebers/openusage>

Copyright (c) 2026 Robin Ebers

The Perplexity provider in `internal/providers/perplexity/perplexity.go`
calls the same `/rest/pplx-api/v2/groups`, `/rest/pplx-api/v2/groups/{id}`,
and `/rest/rate-limit/all` endpoints documented by openusage's
`plugins/perplexity/plugin.js`, with the same flexible field-name
lookup pattern (`balance_usd`/`balanceUsd`/etc. under `apiOrganization`,
`customerInfo`, etc. wrappers). The legacy `/rest/billing/credits`
endpoint (briefly removed in early 2026) is alive again and powers
the credit-balance + per-meter usage tiles; the field set there is
not borrowed from openusage. The Go implementation is ours; the
endpoint set and field-name fallbacks are the borrowed knowledge.

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the condition that the above copyright notice and this permission
notice be included in all copies or substantial portions of the
Software. The Software is provided "AS IS", without warranty of any
kind.

## lobehub/lobe-icons — MIT

<https://github.com/lobehub/lobe-icons>

`internal/icons/lobe_generated.go` is produced by
`scripts/sync-lobe-icons.go`, which fetches monochrome SVGs from
lobehub/lobe-icons (MIT) and embeds each `d` attribute into a Go map
for compile-time inlining. The mapping table inside that script lists
the lobe icon name and variant chosen for each provider ID. Path data
is unmodified from upstream; only the surrounding Go map structure
and (for wordmark variants) the vertical-stretch `<g>` wrapper are
ours.

The same lobe-icons path data is also embedded in the Stream Deck
action thumbnails / key previews under
`io.github.anthonybaldwin.UsageButtons.sdPlugin/assets/action-<name>.svg`
(and the `-key.svg` companions) for providers whose action art was
sourced from lobe-icons (e.g. grok, hermes-agent, nousresearch).

Re-run `go run scripts/sync-lobe-icons.go` after upstream icon
updates or after editing the mapping table.

Authoritative license text:
<https://github.com/lobehub/lobe-icons/blob/master/LICENSE>

## openclaw/openclaw — MIT

<https://github.com/openclaw/openclaw>

The OpenClaw provider's action-library icon (lobster mascot, gradient
body + cyan eyes) is the unmodified upstream favicon
(`ui/public/favicon.svg`) saved as
`io.github.anthonybaldwin.UsageButtons.sdPlugin/assets/action-openclaw.svg`
and `action-openclaw-key.svg`. The on-button glyph in
`internal/icons/icons.go` (`"openclaw"` entry) is a derivative — the
body and claw paths extracted as a monochrome silhouette so the
renderer can fill it with `currentColor` (the body-face composition
needs a single-color fill driven by the brand palette). Path data is
unmodified; the simplification (dropping gradient definitions,
antennae, and eye highlights) for the glyph variant is ours. The
WebSocket JSON-RPC protocol shape (frame types, connect params, method
names like `usage.cost`) is borrowed knowledge from
`ui/src/ui/gateway.ts`, `src/gateway/server-methods/usage.ts`, and
`src/infra/session-cost-usage.types.ts` — the Go re-implementation in
`internal/providers/openclaw/` is ours.

Authoritative license text:
<https://github.com/openclaw/openclaw/blob/main/LICENSE>

## UsmanDevCraft/grok-shooting-stars — MIT

<https://github.com/UsmanDevCraft/grok-shooting-stars>

The static white-dot starfield rendered behind the Grok button face
(see `renderStarfield` in `internal/render/svg.go`) is a Go re-creation
of the positioning + opacity-flicker pattern from upstream's HTML5
canvas implementation. Stream Deck buttons are rasterized once per
poll, so the per-frame flicker / shooting-star animation isn't
reproducible at this layer; only the static field is borrowed. No
upstream code is bundled — the SVG-emitting Go is ours.

Authoritative license text:
<https://github.com/UsmanDevCraft/grok-shooting-stars/blob/main/LICENSE>

