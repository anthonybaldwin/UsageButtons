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

export interface ButtonRenderInput {
  /** Top label (provider or metric), e.g. "CLAUDE". Short + uppercase. */
  label?: string;
  /** Big center value, e.g. "87%" or "42" or "4d". */
  value: string;
  /** Optional sub-value under the main value, e.g. "2h 15m". */
  subvalue?: string;
  /** Normalised 0..1 fill ratio (NaN/undefined → empty). */
  ratio?: number;
  /** Direction the fill grows in as ratio → 1. */
  direction?: FillDirection;
  /** Foreground color (text + border). */
  fg?: string;
  /** Fill color — the growing/shrinking rectangle. */
  fill?: string;
  /** Background color behind the fill. */
  bg?: string;
  /** "Dim" the entire card to signal stale/error data. */
  stale?: boolean;
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

export function renderButtonSvg(input: ButtonRenderInput): string {
  const ratio = clamp01(input.ratio);
  const direction: FillDirection = input.direction ?? "up";
  const fg = input.fg ?? "#f9fafb";
  const fill = input.fill ?? "#3b82f6";
  const bg = input.bg ?? "#111827";
  const opacity = input.stale ? "0.45" : "1";
  const rect = fillRect(ratio, direction);

  const label = input.label ? escapeXml(input.label) : "";
  const value = escapeXml(input.value);
  const subvalue = input.subvalue ? escapeXml(input.subvalue) : "";

  // Layout notes:
  //   - Outer rounded square (bg)
  //   - Fill rectangle clipped to the inner rounded rect
  //   - Label on top, value centered, subvalue underneath
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
  <rect x="0.75" y="0.75" width="${CANVAS - 1.5}" height="${CANVAS - 1.5}" rx="16" ry="16"
        fill="none" stroke="${fg}" stroke-opacity="0.18" stroke-width="1.5"/>
  ${label ? `<text x="${CANVAS / 2}" y="26" font-family="Helvetica,Arial,sans-serif" font-size="16" font-weight="700" text-anchor="middle" fill="${fg}" fill-opacity="0.85">${label}</text>` : ""}
  <text x="${CANVAS / 2}" y="${subvalue ? 86 : 92}" font-family="Helvetica,Arial,sans-serif" font-size="44" font-weight="800" text-anchor="middle" fill="${fg}">${value}</text>
  ${subvalue ? `<text x="${CANVAS / 2}" y="118" font-family="Helvetica,Arial,sans-serif" font-size="16" font-weight="600" text-anchor="middle" fill="${fg}" fill-opacity="0.75">${subvalue}</text>` : ""}
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
