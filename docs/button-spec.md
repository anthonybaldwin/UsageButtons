# Button Spec

Rules every Stream Deck button in this plugin must follow. Read this
before adding a provider or changing the renderer. If you find code
that violates a rule, fix the code — not the rule. If a rule is wrong,
update this file in the same PR as the code change.

The renderer is at `internal/render/svg.go`. Per-provider button
assembly is in `cmd/plugin/main.go` (`renderMetric`, `loadingFaceFor`,
`placeholderFace`). User-visible defaults live in
`internal/settings/settings.go`.

---

## 1. Anatomy

- **Canvas:** 144×144 SVG user units. Constant: `render.Canvas`. Stream
  Deck rasterizes each `setImage` SVG to a static PNG before display,
  so SMIL `<animate>` does not tick — animations come from the plugin
  re-emitting frames (see Grok starfield).
- **Paint order (back→front):** `bg → starfield → glyphBack →
  fillRect → border → glyphFront → textBack → textFill (clipped to
  fill rect)`.
- **Border:** rounded rect `rx=16`, `stroke-opacity=0.18`, on by
  default. Don't disable.
- **Title:** owned by Stream Deck, not the SVG. SVG owns value, glyph,
  ratio fill. Labels are rendered via `setTitle`, in UPPERCASE
  (`SESSION`, `WEEKLY`, …) per `AGENTS.md`.

## 2. The three-color contract

Every button has exactly three colors:

| Color | Role | Source of truth |
| --- | --- | --- |
| `Bg` | Card background | `prov.BrandBg()` on the provider |
| `Fill` | Meter fill (the rectangle that grows with the ratio) | `prov.BrandColor()` on the provider |
| `Fg` | Text + glyph foreground | Plugin/user setting; renderer default `#f9fafb` |

**Rules:**

- Brand colors live on the provider type (`BrandColor()`,
  `BrandBg()`). No central palette table — each provider owns its
  identity. When you add a provider, define both methods.
- Default `Fg` is `#f9fafb`. Don't override per-provider unless the
  brand demands it (e.g. Synthetic's near-white `Bg`).
- Three-tier override precedence applies to all three colors:
  **button > provider > plugin**. The renderer never reads settings;
  `cmd/plugin/main.go` resolves the tier and threads the result into
  `ButtonInput`.

## 3. SmartContrast — when it's mandatory

The renderer can dual-paint text and the watermark glyph so the half
straddling the fill line uses one color and the half over the bg uses
another (`internal/render/svg.go: contrastOver`). It's opt-in
per-render via `ButtonInput.SmartContrast`.

**Mandatory rule:** SmartContrast MUST be the built-in default
(`providerDefaultSmartContrast` in `internal/settings/settings.go`)
for any provider whose palette satisfies either:

- **Light-zone collision:** `Fill` or `Bg` has relative luminance
  ≥ 0.75 *and* default `Fg` (`#f9fafb`) is also light. (Grok, Ollama,
  Synthetic-bg.)
- **Dark-zone collision:** `Fill` or `Bg` has relative luminance
  ≤ 0.06 *and* default `Fg` is dark. (Inverse cases like
  Synthetic-fill on white-bg if a user picks dark text.)

Use `render.IsValidHexColor` and the unexported luminance helpers as
ground truth — don't eyeball it. When you add a new provider, run
`hexRelativeLuminance` mentally on `BrandColor` and `BrandBg`. If
either lands in `[0, 0.06] ∪ [0.75, 1]`, register it in
`providerDefaultSmartContrast` AND in the JS mirror at
`stat.html: SMART_CONTRAST_DEFAULTS`. Keep the two lists in sync.

## 4. Loading state

`render.RenderLoading` produces the face shown before the first
snapshot lands. **Continuity** is the rule — the loaded face must not
visually pop when data arrives.

- **Same canvas:** 144×144. Don't ship a smaller or larger loading
  SVG.
- **Same glyph zone:** loading uses the exact zone math as the
  loaded watermark (between `labelBottom` and `subvalueTop`). The
  glyph never shifts position when data arrives.
- **Same back-layer opacity:** 0.70. Matches the loaded watermark
  back layer, so the glyph reads at the same density.
- **No spinner.** The provider glyph alone is the loading affordance;
  Stream Deck's tile cadence supplies the implicit "is something
  happening?" feedback.
- **Brand colors apply:** `loadingFaceFor` resolves brand colors via
  the same tier chain as `renderMetric`. Don't bypass it.

## 5. Glyph mode

Three modes exist on `ButtonInput.GlyphMode`: `watermark`,
`centered`, `corner`. **Only `watermark` is used in production.** All
metric buttons get watermark; placeholder/configure faces sometimes
get `none` (no glyph). Don't introduce new modes without an
accompanying renderer change and a use case.

When adding a glyph: define it in `internal/icons/icons.go` (path
data or full markup). Keep stroke widths within the existing range so
all watermarks read at the same density.

## 6. Direction

`ButtonInput.Direction` controls how the fill rect grows. **Default
is `up`** — fill grows from the bottom. Every existing provider uses
`up` because every metric is "remaining" (high = good, fill shrinks
as usage grows).

If you add a metric where the natural mental model is "consumed" or
"depleted from a known cap going down," use `down`. Don't mix
directions across the same provider's metrics — pick one and stick
with it.

## 7. Subvalue priority chain

`renderMetric` resolves the subvalue (the small text under the big
value) in this order. Don't reimplement the chain in providers —
just populate the metric fields and let the chain do its job.

```
1. HideSubvalue=true                             → ""
2. metric.ResetInSeconds != nil && showTimer     → FormatCountdown()
3. caption override (per-button setting)         → override
4. ShowRawCounts=true                            → formatRawCounts()
5. metric.Caption != ""                          → caption
6. percent metric (default fallback)             → "Remaining"
```

**Rule (the user's #4 explicitly):** when a reset countdown is
known, show it. When it isn't, show **"Remaining"** (or the
provider's caption if that's what the metric actually represents,
e.g. `"Cost (local)"`). Never hardcode `"Remaining"` in a provider
file — let the fallback handle it. If your provider needs a
provider-specific subtext, set `metric.Caption`.

`FormatCountdown` (`render.FormatCountdown`) is the canonical time
formatter. Don't roll your own. It already handles the trailing-zero
elision (`1d 5h`, not `1d 5h 0m`).

## 8. Snapshot age + reset countdown

When a snapshot ages past its fetch time but a `ResetAt` is known,
the countdown stays accurate by subtracting the snapshot age from
`ResetInSeconds` (`metricWithSnapshotAge` in `cmd/plugin/main.go`).
Don't call `time.Now()` in a provider's render path — write the
absolute `ResetAt` into the snapshot once, at fetch time, and let the
plugin compute the live countdown.

## 9. Stale handling

`MetricValue.Stale` exists. When set, the renderer applies
`opacity=0.75` to the entire button. **Use it** when:

- The provider knows its data is stale but couldn't refresh (cookie
  expired, network down, etc.) AND the data is still semantically
  useful (e.g. a 90-second-old session-percent).
- An extension-gated provider lost the cookie but had a recent
  snapshot in cache.

Don't use it for "I've never had data" — that's the loading or
configure face.

## 10. Three-tier override precedence

For every visual setting (`Fg`, `Bg`, `Fill`, `Border`,
`SmartContrast`, `Starfield`, `ShowGlyph`, etc.):

```
1. Per-button setting (KeySettings)        → wins if set
2. Per-provider setting (ProviderSettings) → next
3. Plugin/global default                   → next
4. Built-in renderer default               → last resort
```

Resolution lives in `cmd/plugin/main.go` and helpers in
`internal/settings/settings.go`. The renderer never reads settings.
Don't shortcut this — providers shouldn't reach into `settings.*`
either; they should just produce snapshots.

## 11. Adding a new provider — checklist

Before opening the PR:

- [ ] `BrandColor()` and `BrandBg()` defined on the provider type.
- [ ] Glyph added to `internal/icons/icons.go`.
- [ ] Run the SmartContrast luminance check (§3). If either color
      lands in `[0, 0.06] ∪ [0.75, 1]`, register the provider in
      `providerDefaultSmartContrast` (Go) AND
      `SMART_CONTRAST_DEFAULTS` (`stat.html`). Keep them in sync.
- [ ] All metrics set `Direction: "up"` unless there's an explicit
      reason to differ (§6).
- [ ] Reset-aware metrics set `ResetAt` at fetch time (§8). Don't
      compute `ResetInSeconds` in the provider — set the absolute
      time and let `metricWithSnapshotAge` derive the live countdown.
- [ ] Don't hardcode `"Remaining"`. Use `metric.Caption` for
      provider-specific subtext or rely on the fallback (§7).
- [ ] Action manifest entry includes `UserTitleEnabled: true`,
      `ShowTitle: true`, and an UPPERCASE default title.
- [ ] `RenderLoading` is wired through `loadingFaceFor`, not bypassed
      with a custom loading SVG.
- [ ] If the provider is cookie-gated, follow the three-step add in
      `AGENTS.md` (Go allowlist + extension allowlist + manifest
      host_permissions).
- [ ] README, `docs/PROVIDERS.md`, `docs/index.html`, and GitHub
      topics updated in the same PR (per `AGENTS.md`).

## 12. Known violations and debt

These are real, file an issue or fix in a follow-up PR. Don't
introduce more.

- **Grok contrast trap (latent).** `Fill=#ffffff`, `Bg=#000000`,
  default `Fg=#f9fafb`. Grok's current metrics all render as
  reference cards (`Ratio=nil`, see `countMetric` in
  `internal/providers/grok/grok.go`), so the white fill rect is
  never drawn — no active bug. **But** if Grok ever ships a
  percent/ratio metric, the `Fg` text would render near-invisibly
  over the white fill. When (if) that happens, register
  `"grok": true` in `providerDefaultSmartContrast` AND
  `SMART_CONTRAST_DEFAULTS` in the same PR.
- **Settings/UI sync drift.** `SMART_CONTRAST_DEFAULTS` in
  `stat.html` lists only `ollama: true`; the Go side
  (`providerDefaultSmartContrast`) lists `ollama` AND `synthetic`.
  The "(default on)" hint in the PI is wrong for Synthetic users.
  Fix: add `synthetic: true` to the JS map.
- **GlyphMode dead code.** `centered` and `corner` modes are
  rendered in `RenderButton` but no caller selects them. Either wire
  them up to a real use case or delete the branches.
- **Stale flag is unused.** No provider sets `MetricValue.Stale`. The
  Claude grace window (`staleResetGrace = 90s`) currently serves
  stale data without dimming it. Consider flipping `Stale=true` when
  serving past `ResetAt` so the user has a visual cue. Audit each
  cookie-gated provider for "I have a snapshot but couldn't refresh"
  paths.

---

When in doubt, match the existing renderer behavior, then file a
follow-up to discuss the rule. Don't fork the conventions per
provider — every divergence is a future bug.
