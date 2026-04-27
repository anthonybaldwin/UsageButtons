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

- `internal/icons/icons.go` — the `d` attribute of each provider's
  SVG is embedded into a Go map for compile-time inlining. The path
  data is unmodified from upstream; only the surrounding Go map
  structure is ours. A few entries (codex, grok, hermes) come from
  lobehub/lobe-icons instead and are flagged inline; see the
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
`customerInfo`, etc. wrappers). Perplexity removed the older
`/rest/billing/credits` endpoint in 2026; openusage's mapping was the
shortest path back to working data. The Go implementation is ours; the
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

The `"codex"` and `"grok"` entries in `internal/icons/icons.go` embed
the `d` attribute of those providers' monochrome glyphs from
lobe-icons, distributed under the MIT license. The same path data is
also embedded in `io.github.anthonybaldwin.UsageButtons.sdPlugin/assets/action-grok.svg`
and `action-grok-key.svg` for the Stream Deck action thumbnail / key
preview. The path data is unmodified from upstream; only the
surrounding Go map structure / SVG wrapper is ours.

The Hermes Agent action thumbnails (`action-hermes.png` and
`action-hermes-key.png` in the same `assets/` directory) are the
Hermes Agent avatar from lobe-icons's `static-avatar/avatars/
hermesagent.webp`, downscaled to 144x144 / 288x288 PNG. Image content
is unmodified.

Authoritative license text:
<https://github.com/lobehub/lobe-icons/blob/master/LICENSE>

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

