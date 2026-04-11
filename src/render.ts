/**
 * SVG renderer for Stream Deck button faces.
 *
 * Stream Deck keys are rendered at 144×144 on most current hardware
 * and we emit a single SVG string that the Stream Deck software
 * rasterises client-side. The fill effect — CodexBar's signature look
 * adapted for a larger canvas — is simply a solid rectangle whose
 * height (or width, depending on `direction`) is proportional to the
 * current value.
 *
 * This file has NO runtime dependencies. It must stay tree-shakable
 * and portable so `bun build --compile` can inline it into the plugin
 * binary.
 */

export type FillDirection =
  /** Fill grows from the bottom upward as value → 100. Classic "tank". */
  | "up"
  /** Fill shrinks from the top downward as value → 0. "Draining". */
  | "down"
  /** Fill grows left → right. */
  | "right"
  /** Fill grows right → left. */
  | "left";

/** Single-path provider glyph — see providers/provider-icons.generated.ts. */
export interface ProviderGlyph {
  /** SVG viewBox for the path, e.g. "0 0 100 100". */
  viewBox: string;
  /** Raw path `d` attribute. */
  d: string;
}

export type ValueSize = "small" | "medium" | "large";

export interface ButtonRenderInput {
  /** Top label (provider or metric), e.g. "CLAUDE". Multi-line supported via `\n`. */
  label?: string;
  /** Big center value, e.g. "87%" or "42" or "4d". */
  value: string;
  /** Optional sub-value under the main value, e.g. "2h 15m". */
  subvalue?: string;
  /** Normalised 0..1 fill ratio (NaN/undefined → empty). */
  ratio?: number;
  /** Direction the fill grows in as ratio → 1. */
  direction?: FillDirection;
  /** Foreground / text color. */
  fg?: string;
  /** Fill color — the growing/shrinking rectangle. */
  fill?: string;
  /** Background color behind the fill. */
  bg?: string;
  /** "Dim" the entire card to signal stale/error data. */
  stale?: boolean;
  /** Value text size. Default "large". */
  valueSize?: ValueSize;
  /**
   * Subvalue text size (reset countdown / supplementary text).
   * Default "large" so the reset countdown is legible on the key.
   */
  subvalueSize?: ValueSize;
  /** Render the outer rounded-rect border stroke. Default on. */
  border?: boolean;
  /** Optional provider glyph (single SVG path) to render. */
  glyph?: ProviderGlyph;
  /** Hide the glyph entirely when false. Default true (visible). */
  showGlyph?: boolean;
  /**
   * How to render the glyph:
   *
   *   - "watermark" (default) — big (88×88), centered, BEHIND the
   *     fill rect at low opacity. As the meter fills, the brand
   *     color naturally "consumes" the logo from the bottom up,
   *     leaving the unfilled (dark) portion of the card showing
   *     the brand mark. Self-coloring — works at any user-chosen
   *     fill / bg combo without explicit glyph color tuning.
   *
   *   - "centered" — big (88×88), centered, ON TOP of everything
   *     at high opacity. Used for the "MAX'D" / blocked face so
   *     the brand logo dominates the tile. Skips the big value
   *     text since the logo *is* the value.
   *
   *   - "corner" — small (20×20) top-right badge, foreground color
   *     at 0.7 opacity. Legacy / opt-in.
   *
   *   - "none" — don't render the glyph at all.
   */
  glyphMode?: "watermark" | "centered" | "corner" | "none";
}

const CANVAS = 144;

function clamp01(n: number | undefined): number {
  if (n === undefined || Number.isNaN(n)) return 0;
  if (n < 0) return 0;
  if (n > 1) return 1;
  return n;
}

function escapeXml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;");
}

/**
 * Compute the fill rectangle for a given direction + ratio.
 * All coordinates are in the 0..CANVAS space.
 */
function fillRect(
  ratio: number,
  direction: FillDirection,
): { x: number; y: number; w: number; h: number } {
  const full = CANVAS;
  const fill = Math.round(full * ratio);
  switch (direction) {
    case "up":
      return { x: 0, y: full - fill, w: full, h: fill };
    case "down":
      return { x: 0, y: 0, w: full, h: fill };
    case "right":
      return { x: 0, y: 0, w: fill, h: full };
    case "left":
      return { x: full - fill, y: 0, w: fill, h: full };
  }
}

const VALUE_FONT_SIZE: Record<ValueSize, number> = {
  small: 32,
  medium: 40,
  large: 48,
};

/**
 * Estimate how many pixels wide a string renders at a given
 * font-size in Helvetica Bold. 0.58em per character is the
 * average across digits, currency glyphs, and the few letters we
 * use. Good enough for auto-fit decisions — we only use the
 * estimate to decide whether to shrink the font, never to position
 * anything precisely.
 */
function estimateTextWidth(text: string, fontSize: number): number {
  return text.length * fontSize * 0.58;
}

/**
 * Pick a font size that fits `text` within `maxWidth` pixels,
 * starting from `preferredSize` and shrinking down to `minSize`
 * only if needed. Used so that "$204.80" at Large doesn't overflow
 * the 144px canvas while "42%" at Large still renders at full size.
 */
function fitFontSize(
  text: string,
  maxWidth: number,
  preferredSize: number,
  minSize = 14,
): number {
  if (!text) return preferredSize;
  if (estimateTextWidth(text, preferredSize) <= maxWidth) return preferredSize;
  // Solve for the size that fits exactly, clamped to minSize.
  const solved = Math.floor(maxWidth / (text.length * 0.58));
  return Math.max(minSize, Math.min(preferredSize, solved));
}
const LABEL_FONT_MAX = 16;
const LABEL_FONT_MIN = 10;
/**
 * Subvalue (reset countdown / supplementary text) font sizes.
 * Default bumped to "large" because the old fixed 16px rendered
 * the "4h 13m" / "5d" line almost invisible on a Stream Deck key.
 */
const SUBVALUE_FONT_SIZE: Record<ValueSize, number> = {
  small: 14,
  medium: 18,
  large: 22,
};

export function renderButtonSvg(input: ButtonRenderInput): string {
  const ratio = clamp01(input.ratio);
  const direction: FillDirection = input.direction ?? "up";
  const fg = input.fg ?? "#f9fafb";
  const fill = input.fill ?? "#3b82f6";
  const bg = input.bg ?? "#111827";
  const opacity = input.stale ? "0.45" : "1";
  const rect = fillRect(ratio, direction);
  const valueSize: ValueSize = input.valueSize ?? "large";
  const preferredValueFont = VALUE_FONT_SIZE[valueSize];
  // Auto-fit the big-number text to the canvas width. Leave ~12px
  // of horizontal padding on each side (to account for the
  // rounded-corner inset + border stroke). This shrinks long
  // strings like "$1,234.56" at Large from 48px to whatever fits
  // without clipping, while leaving short strings ("42%") at full size.
  const valueFontSize = fitFontSize(
    input.value,
    CANVAS - 24,
    preferredValueFont,
  );
  const subvalueSize: ValueSize = input.subvalueSize ?? "large";
  const subvalueFontSize = SUBVALUE_FONT_SIZE[subvalueSize];
  const showBorder = input.border !== false;

  const labelLinesRaw = input.label
    ? input.label.split(/\r?\n/).map((line) => escapeXml(line))
    : [];
  const value = escapeXml(input.value);
  const subvalue = input.subvalue ? escapeXml(input.subvalue) : "";

  // Auto-fit the label font so long titles (e.g. "EXTRA USAGE") and
  // user-entered multi-line overrides don't overflow the 144px
  // canvas. We compute a shrunk font size that fits the longest
  // line within (CANVAS - 20)px and use it for every line — then
  // the value text below doesn't jump around as wildly as it
  // would if we let line count alone drive the block height.
  const longestLabelLen = labelLinesRaw.reduce(
    (m, line) => Math.max(m, line.length),
    0,
  );
  const labelFontSize =
    longestLabelLen > 0
      ? fitFontSize(
          "M".repeat(longestLabelLen),
          CANVAS - 20,
          LABEL_FONT_MAX,
          LABEL_FONT_MIN,
        )
      : LABEL_FONT_MAX;
  const labelLineHeight = Math.round(labelFontSize * 1.08);
  const labelLines = labelLinesRaw;

  // Layout: compute the vertical center for the value block based on
  // which surrounding elements are present. When both label and
  // subvalue are hidden, the value sits at canvas center. When only
  // one is present it shifts slightly to compensate. This is what
  // the user means by "if default title is hidden and there is no
  // title, numbers should shift up/center more".
  const hasLabel = labelLines.length > 0;
  const hasSub = subvalue.length > 0;
  const labelBlockHeight = hasLabel ? labelLines.length * labelLineHeight : 0;
  const labelBottom = hasLabel ? 14 + labelBlockHeight : 0;
  // Subvalue baseline needs more room when the text is larger.
  // Leave `subvalueFontSize * 0.85` pixels of bottom padding so
  // descenders don't clip the rounded card edge.
  const subvalueBaselineY = CANVAS - Math.round(subvalueFontSize * 0.35);
  const subvalueTop = hasSub
    ? subvalueBaselineY - Math.round(subvalueFontSize * 0.85)
    : CANVAS;
  // Available vertical range for the value baseline.
  const top = labelBottom + valueFontSize * 0.75;
  const bot = subvalueTop - valueFontSize * 0.15;
  const valueY = Math.round((top + bot) / 2);

  const labelElements = labelLines
    .map((line, i) => {
      const y = 14 + labelFontSize + i * labelLineHeight;
      return `<text x="${CANVAS / 2}" y="${y}" font-family="Helvetica,Arial,sans-serif" font-size="${labelFontSize}" font-weight="700" text-anchor="middle" fill="${fg}" fill-opacity="0.85">${line}</text>`;
    })
    .join("");

  const borderElement = showBorder
    ? `<rect x="0.75" y="0.75" width="${CANVAS - 1.5}" height="${CANVAS - 1.5}" rx="16" ry="16" fill="none" stroke="${fg}" stroke-opacity="0.18" stroke-width="1.5"/>`
    : "";

  const subvalueElement = hasSub
    ? `<text x="${CANVAS / 2}" y="${subvalueBaselineY}" font-family="Helvetica,Arial,sans-serif" font-size="${subvalueFontSize}" font-weight="700" text-anchor="middle" fill="${fg}" fill-opacity="0.85">${subvalue}</text>`
    : "";

  // Glyph rendering — see ButtonRenderInput.glyphMode docs above
  // for the full design rationale. Three positioned variants
  // produce three different visual effects:
  //
  //   watermark: big (88×88), centered, BEHIND the fill. As the
  //              meter fills, the brand color "consumes" the logo
  //              from the bottom up, leaving it visible in the
  //              unfilled (dark) portion of the card. Self-coloring
  //              so it works against any user-chosen fill/bg combo
  //              without manual tuning.
  //   centered : big (88×88), centered, ON TOP of everything at
  //              high opacity. Used for MAX'D / blocked — the logo
  //              IS the focal point. Value text is suppressed.
  //   corner   : small (20×20), top-right, legacy badge.
  const showGlyph =
    input.showGlyph !== false && !!input.glyph && input.glyphMode !== "none";
  const glyphMode = input.glyphMode ?? "watermark";

  let glyphElementBack = "";
  let glyphElementFront = "";
  if (showGlyph && input.glyph) {
    if (glyphMode === "watermark") {
      // 76px centered watermark BEHIND the fill rect at 0.40
      // opacity. White-on-dark is highly visible at 40%; the
      // brand-colored fill rect covers the bottom portion as the
      // meter rises.
      const gSize = 76;
      const gOff = (CANVAS - gSize) / 2;
      glyphElementBack = `<g transform="translate(${gOff} ${gOff}) scale(${gSize / 100})" fill="${fg}" fill-opacity="0.40"><path d="${input.glyph.d}"/></g>`;
    } else if (glyphMode === "centered") {
      // 60px focal logo. Smaller than the watermark so it doesn't
      // crowd the border + label + countdown around it. Currently
      // unused (the MAX'D face was replaced by a synthesized
      // normal-looking metric in plugin.ts) but kept for any
      // future render path that wants the focal logo treatment.
      const gSize = 60;
      const gOff = (CANVAS - gSize) / 2;
      glyphElementFront = `<g transform="translate(${gOff} ${gOff}) scale(${gSize / 100})" fill="${fg}" fill-opacity="0.92"><path d="${input.glyph.d}"/></g>`;
    } else if (glyphMode === "corner") {
      const gSize = 20;
      const gx = CANVAS - gSize - 6;
      const gy = 6;
      glyphElementFront = `<g transform="translate(${gx} ${gy}) scale(${gSize / 100})" fill="${fg}" fill-opacity="0.7"><path d="${input.glyph.d}"/></g>`;
    }
  }

  // In "centered" mode the glyph IS the focal — suppress the big
  // value text entirely so the logo owns the visual center.
  // Subvalue (reset countdown) still renders below it.
  const showValueText = !(glyphMode === "centered" && showGlyph);

  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${CANVAS} ${CANVAS}" opacity="${opacity}">
  <defs>
    <clipPath id="card">
      <rect width="${CANVAS}" height="${CANVAS}" rx="16" ry="16"/>
    </clipPath>
  </defs>
  <g clip-path="url(#card)">
    <rect width="${CANVAS}" height="${CANVAS}" fill="${bg}"/>
    ${glyphElementBack}
    <rect x="${rect.x}" y="${rect.y}" width="${rect.w}" height="${rect.h}" fill="${fill}"/>
  </g>
  ${borderElement}
  ${glyphElementFront}
  ${labelElements}
  ${showValueText ? `<text x="${CANVAS / 2}" y="${valueY}" font-family="Helvetica,Arial,sans-serif" font-size="${valueFontSize}" font-weight="800" text-anchor="middle" fill="${fg}">${value}</text>` : ""}
  ${subvalueElement}
</svg>`;
}

/**
 * Render a "loading" face — what a button shows between willAppear
 * and the first successful provider fetch. Minimal: bg + border +
 * the provider's glyph centered at about 40% of canvas size.
 * No label, no value text, no "···" glyph. The logo is the signal.
 */
export function renderLoadingSvg(opts: {
  glyph?: ProviderGlyph;
  fill?: string;
  bg?: string;
  fg?: string;
  border?: boolean;
}): string {
  const fg = opts.fg ?? "#f9fafb";
  const bg = opts.bg ?? "#111827";
  const showBorder = opts.border !== false;

  // Glyph sized at 56×56 px centered on the 144×144 canvas. Scales
  // the upstream 100×100 viewBox down to 56. Rendered in the
  // provider brand color (if known via `fill`) at 0.85 opacity so
  // it's clearly visible but reads as a "placeholder" state rather
  // than active data.
  const glyphSize = 56;
  const glyphOffset = (CANVAS - glyphSize) / 2;
  const glyphColor = opts.fill ?? fg;
  const glyphElement = opts.glyph
    ? `<g transform="translate(${glyphOffset} ${glyphOffset}) scale(${glyphSize / 100})" fill="${glyphColor}" fill-opacity="0.85"><path d="${opts.glyph.d}"/></g>`
    : // Fallback when no glyph is available for the provider: a
      // simple centered dot so the user still sees *something*.
      `<circle cx="${CANVAS / 2}" cy="${CANVAS / 2}" r="4" fill="${fg}" fill-opacity="0.4"/>`;

  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${CANVAS} ${CANVAS}">
  <defs>
    <clipPath id="card-loading">
      <rect width="${CANVAS}" height="${CANVAS}" rx="16" ry="16"/>
    </clipPath>
  </defs>
  <g clip-path="url(#card-loading)">
    <rect width="${CANVAS}" height="${CANVAS}" fill="${bg}"/>
  </g>
  ${showBorder ? `<rect x="0.75" y="0.75" width="${CANVAS - 1.5}" height="${CANVAS - 1.5}" rx="16" ry="16" fill="none" stroke="${fg}" stroke-opacity="0.18" stroke-width="1.5"/>` : ""}
  ${glyphElement}
</svg>`;
}

/** Convenience: render "NN%" with an "up-fill" bar from the ratio. */
export function renderPercentButton(opts: {
  label?: string;
  percent: number;
  subvalue?: string;
  fill?: string;
  bg?: string;
  fg?: string;
  stale?: boolean;
  /** If true, the fill represents *remaining* (so it drains as value drops). */
  remaining?: boolean;
}): string {
  const pct = Math.max(0, Math.min(100, Math.round(opts.percent)));
  const ratio = pct / 100;
  const fill = opts.fill ?? (opts.remaining ? "#10b981" : "#3b82f6");
  const input: ButtonRenderInput = {
    value: `${pct}%`,
    ratio,
    direction: "up",
    fill,
  };
  if (opts.label !== undefined) input.label = opts.label;
  if (opts.subvalue !== undefined) input.subvalue = opts.subvalue;
  if (opts.bg !== undefined) input.bg = opts.bg;
  if (opts.fg !== undefined) input.fg = opts.fg;
  if (opts.stale !== undefined) input.stale = opts.stale;
  return renderButtonSvg(input);
}
