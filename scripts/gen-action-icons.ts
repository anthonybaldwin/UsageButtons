import { PROVIDER_ICONS } from "../src/providers/provider-icons.generated.ts";
import { pathBBox, contentFitTransform, type BBox } from "../src/svg-bbox.ts";

const BRAND_COLORS: Record<string, string> = {
  claude:     "#cc7c5e",
  codex:      "#49a3b0",
  copilot:    "#a855f7",
  cursor:     "#00bfa5",
  openrouter: "#6467f2",
  warp:       "#938bb4",
  zai:        "#e85a6a",
  "kimi-k2":  "#4c00ff",
};

const ICON_KEY: Record<string, string> = {
  claude: "claude",
  codex: "codex",
  copilot: "copilot",
  cursor: "cursor",
  warp: "warp",
  zai: "zai",
  "kimi-k2": "kimi",
  openrouter: "openRouter",
};

const ASSETS = "io.github.anthonybaldwin.UsageButtons.sdPlugin/assets";

/** Merge multiple bounding boxes. */
function mergeBBoxes(boxes: BBox[]): BBox {
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  for (const b of boxes) {
    if (b.minX < minX) minX = b.minX;
    if (b.minY < minY) minY = b.minY;
    if (b.maxX > maxX) maxX = b.maxX;
    if (b.maxY > maxY) maxY = b.maxY;
  }
  return { minX, minY, maxX, maxY };
}

function fitTransform(
  bbox: BBox,
  targetX: number, targetY: number,
  targetW: number, targetH: number,
): string {
  const bw = bbox.maxX - bbox.minX;
  const bh = bbox.maxY - bbox.minY;
  if (bw <= 0 || bh <= 0) return "";
  const scale = Math.min(targetW / bw, targetH / bh);
  const tx = targetX + (targetW - bw * scale) / 2 - bbox.minX * scale;
  const ty = targetY + (targetH - bh * scale) / 2 - bbox.minY * scale;
  return `translate(${tx},${ty}) scale(${scale})`;
}

// ── Multi-path SVGs ─────────────────────────────────────────────
interface RawSvg {
  paths: { d: string; strokeWidth?: number; fill?: boolean }[];
}

const RAW_SVGS: Record<string, RawSvg> = {
  openrouter: {
    paths: [
      { d: "M3 248.945C18 248.945 76 236 106 219C136 202 136 202 198 158C276.497 102.293 332 120.945 423 120.945", strokeWidth: 90, fill: false },
      { d: "M511 121.5L357.25 210.268L357.25 32.7324L511 121.5Z", fill: true },
      { d: "M0 249C15 249 73 261.945 103 278.945C133 295.945 133 295.945 195 339.945C273.497 395.652 329 377 420 377", strokeWidth: 90, fill: false },
      { d: "M508 376.445L354.25 287.678L354.25 465.213L508 376.445Z", fill: true },
    ],
  },
  zai: {
    paths: [
      { d: "M52.3767 10.0721L45.8028 19.4273C44.7914 20.8938 43.072 21.804 41.2516 21.804H5.34765V10.0215C5.29708 10.0721 52.3767 10.0721 52.3767 10.0721Z", fill: true },
      { d: "M97.0291 10.0722L40.5942 90.0216H2.97095L59.4058 10.0722H97.0291Z", fill: true },
      { d: "M47.6233 90.0215L54.2478 80.6157C55.2592 79.1492 56.9785 78.2389 58.799 78.2389H94.6524V90.0215H47.6233Z", fill: true },
    ],
  },
};

function renderRawPaths(raw: RawSvg, color: string): string {
  return raw.paths.map((p) => {
    if (p.fill === false) {
      return `<path d="${p.d}" fill="none" stroke="${color}" stroke-width="${p.strokeWidth ?? 1}"/>`;
    }
    return `<path d="${p.d}" fill="${color}"/>`;
  }).join("\n    ");
}

function rawBBox(raw: RawSvg): BBox {
  const boxes = raw.paths.map((p) => {
    const bb = pathBBox(p.d);
    const sw = p.strokeWidth ?? 0;
    return {
      minX: bb.minX - sw / 2,
      minY: bb.minY - sw / 2,
      maxX: bb.maxX + sw / 2,
      maxY: bb.maxY + sw / 2,
    };
  });
  return mergeBBoxes(boxes);
}

// ── Generate ────────────────────────────────────────────────────

for (const [providerId, brandColor] of Object.entries(BRAND_COLORS)) {
  const raw = RAW_SVGS[providerId];

  let actionSvg: string;
  let keySvg: string;

  if (raw) {
    const bbox = rawBBox(raw);
    const actionXf = fitTransform(bbox, 2, 2, 16, 16);
    const keyXf = fitTransform(bbox, 28, 28, 88, 88);

    actionSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
  <g transform="${actionXf}">
    ${renderRawPaths(raw, "#d1d5db")}
  </g>
</svg>`;

    keySvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <g transform="${keyXf}" opacity="0.85">
    ${renderRawPaths(raw, brandColor)}
  </g>
</svg>`;
  } else {
    const iconKey = ICON_KEY[providerId];
    const icon = iconKey ? PROVIDER_ICONS[iconKey] : undefined;

    if (icon) {
      const bbox = pathBBox(icon.d);
      const actionXf = fitTransform(bbox, 2, 2, 16, 16);
      const keyXf = fitTransform(bbox, 28, 28, 88, 88);

      actionSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
  <path d="${icon.d}" fill="#d1d5db" transform="${actionXf}"/>
</svg>`;

      keySvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <path d="${icon.d}" fill="${brandColor}" opacity="0.85" transform="${keyXf}"/>
</svg>`;
    } else {
      const letter = providerId[0]!.toUpperCase();
      actionSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
  <circle cx="10" cy="10" r="8" fill="${brandColor}"/>
  <text x="10" y="13.5" font-family="Helvetica,Arial,sans-serif" font-size="9" font-weight="700" text-anchor="middle" fill="#fff">${letter}</text>
</svg>`;

      keySvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <circle cx="72" cy="72" r="44" fill="${brandColor}" opacity="0.85"/>
  <text x="72" y="86" font-family="Helvetica,Arial,sans-serif" font-size="40" font-weight="800" text-anchor="middle" fill="#fff">${letter}</text>
</svg>`;
    }
  }

  await Bun.write(`${ASSETS}/action-${providerId}.svg`, actionSvg);
  await Bun.write(`${ASSETS}/action-${providerId}-key.svg`, keySvg);
  console.log(`✓ ${providerId}`);
}

console.log("Done.");
