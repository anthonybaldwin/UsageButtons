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
  /** Render the outer rounded-rect border stroke. Default on. */
  border?: boolean;
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
const LABEL_FONT_SIZE = 16;
const LABEL_LINE_HEIGHT = 17;
const SUBVALUE_FONT_SIZE = 16;

export function renderButtonSvg(input: ButtonRenderInput): string {
  const ratio = clamp01(input.ratio);
  const direction: FillDirection = input.direction ?? "up";
  const fg = input.fg ?? "#f9fafb";
  const fill = input.fill ?? "#3b82f6";
  const bg = input.bg ?? "#111827";
  const opacity = input.stale ? "0.45" : "1";
  const rect = fillRect(ratio, direction);
  const valueSize: ValueSize = input.valueSize ?? "large";
  const valueFontSize = VALUE_FONT_SIZE[valueSize];
  const showBorder = input.border !== false;

  const labelLines = input.label
    ? input.label.split(/\r?\n/).map((line) => escapeXml(line))
    : [];
  const value = escapeXml(input.value);
  const subvalue = input.subvalue ? escapeXml(input.subvalue) : "";

  // Layout: compute the vertical center for the value block based on
  // which surrounding elements are present. When both label and
  // subvalue are hidden, the value sits at canvas center. When only
  // one is present it shifts slightly to compensate. This is what
  // the user means by "if default title is hidden and there is no
  // title, numbers should shift up/center more".
  const hasLabel = labelLines.length > 0;
  const hasSub = subvalue.length > 0;
  const labelBlockHeight = hasLabel ? labelLines.length * LABEL_LINE_HEIGHT : 0;
  const labelBottom = hasLabel ? 14 + labelBlockHeight : 0;
  const subvalueTop = hasSub ? CANVAS - 26 : CANVAS;
  // Available vertical range for the value baseline:
  //   [labelBottom + valueFontSize*0.75, subvalueTop - valueFontSize*0.15]
  // We center the value within that range.
  const top = labelBottom + valueFontSize * 0.75;
  const bot = subvalueTop - valueFontSize * 0.15;
  const valueY = Math.round((top + bot) / 2);

  const labelElements = labelLines
    .map((line, i) => {
      const y = 14 + LABEL_FONT_SIZE + i * LABEL_LINE_HEIGHT;
      return `<text x="${CANVAS / 2}" y="${y}" font-family="Helvetica,Arial,sans-serif" font-size="${LABEL_FONT_SIZE}" font-weight="700" text-anchor="middle" fill="${fg}" fill-opacity="0.85">${line}</text>`;
    })
    .join("");

  const borderElement = showBorder
    ? `<rect x="0.75" y="0.75" width="${CANVAS - 1.5}" height="${CANVAS - 1.5}" rx="16" ry="16" fill="none" stroke="${fg}" stroke-opacity="0.18" stroke-width="1.5"/>`
    : "";

  const subvalueElement = hasSub
    ? `<text x="${CANVAS / 2}" y="${CANVAS - 12}" font-family="Helvetica,Arial,sans-serif" font-size="${SUBVALUE_FONT_SIZE}" font-weight="600" text-anchor="middle" fill="${fg}" fill-opacity="0.75">${subvalue}</text>`
    : "";

  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${CANVAS} ${CANVAS}" opacity="${opacity}">
  <defs>
    <clipPath id="card">
      <rect width="${CANVAS}" height="${CANVAS}" rx="16" ry="16"/>
    </clipPath>
  </defs>
  <g clip-path="url(#card)">
    <rect width="${CANVAS}" height="${CANVAS}" fill="${bg}"/>
    <rect x="${rect.x}" y="${rect.y}" width="${rect.w}" height="${rect.h}" fill="${fill}"/>
  </g>
  ${borderElement}
  ${labelElements}
  <text x="${CANVAS / 2}" y="${valueY}" font-family="Helvetica,Arial,sans-serif" font-size="${valueFontSize}" font-weight="800" text-anchor="middle" fill="${fg}">${value}</text>
  ${subvalueElement}
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
