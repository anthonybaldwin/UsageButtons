/**
 * Compute the approximate bounding box of an SVG path's `d` attribute.
 *
 * Handles M/L/C/S/Q/T/H/V/A (both absolute and relative). For curves,
 * includes control points which gives a conservative (slightly oversized)
 * box — fine for layout/fitting purposes.
 */

export interface BBox {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
}

export function pathBBox(d: string): BBox {
  let minX = Infinity,
    minY = Infinity,
    maxX = -Infinity,
    maxY = -Infinity;
  let cx = 0,
    cy = 0;

  function mark(x: number, y: number): void {
    if (x < minX) minX = x;
    if (x > maxX) maxX = x;
    if (y < minY) minY = y;
    if (y > maxY) maxY = y;
  }

  const tokens = d.match(
    /[a-zA-Z]|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?/g,
  );
  if (!tokens) return { minX: 0, minY: 0, maxX: 0, maxY: 0 };

  let cmd = "M";
  let i = 0;

  function nextNum(): number {
    while (i < tokens!.length && /^[a-zA-Z]$/.test(tokens![i]!)) i++;
    return i < tokens!.length ? parseFloat(tokens![i++]!) : 0;
  }

  while (i < tokens.length) {
    const tok = tokens[i]!;
    if (/^[a-zA-Z]$/.test(tok)) {
      cmd = tok;
      i++;
    }

    const rel = cmd === cmd.toLowerCase();
    const CMD = cmd.toUpperCase();

    switch (CMD) {
      case "M":
      case "L":
      case "T": {
        let x = nextNum(),
          y = nextNum();
        if (rel) {
          x += cx;
          y += cy;
        }
        mark(x, y);
        cx = x;
        cy = y;
        if (CMD === "M") cmd = rel ? "l" : "L";
        break;
      }
      case "H": {
        let x = nextNum();
        if (rel) x += cx;
        mark(x, cy);
        cx = x;
        break;
      }
      case "V": {
        let y = nextNum();
        if (rel) y += cy;
        mark(cx, y);
        cy = y;
        break;
      }
      case "C": {
        let x1 = nextNum(),
          y1 = nextNum();
        let x2 = nextNum(),
          y2 = nextNum();
        let x = nextNum(),
          y = nextNum();
        if (rel) {
          x1 += cx;
          y1 += cy;
          x2 += cx;
          y2 += cy;
          x += cx;
          y += cy;
        }
        mark(x1, y1);
        mark(x2, y2);
        mark(x, y);
        cx = x;
        cy = y;
        break;
      }
      case "S":
      case "Q": {
        let x1 = nextNum(),
          y1 = nextNum();
        let x = nextNum(),
          y = nextNum();
        if (rel) {
          x1 += cx;
          y1 += cy;
          x += cx;
          y += cy;
        }
        mark(x1, y1);
        mark(x, y);
        cx = x;
        cy = y;
        break;
      }
      case "A": {
        nextNum();
        nextNum();
        nextNum();
        nextNum();
        nextNum();
        let x = nextNum(),
          y = nextNum();
        if (rel) {
          x += cx;
          y += cy;
        }
        mark(x, y);
        cx = x;
        cy = y;
        break;
      }
      case "Z":
        break;
      default:
        i++;
    }
  }
  return { minX, minY, maxX, maxY };
}

/**
 * Compute a scale + translate that fits the path's actual content
 * (not viewBox) into a target rectangle. Returns the SVG transform string.
 */
export function contentFitTransform(
  d: string,
  targetX: number,
  targetY: number,
  targetW: number,
  targetH: number,
): string {
  const bbox = pathBBox(d);
  const bw = bbox.maxX - bbox.minX;
  const bh = bbox.maxY - bbox.minY;
  if (bw <= 0 || bh <= 0) return "";
  const scale = Math.min(targetW / bw, targetH / bh);
  const tx = targetX + (targetW - bw * scale) / 2 - bbox.minX * scale;
  const ty = targetY + (targetH - bh * scale) / 2 - bbox.minY * scale;
  return `translate(${tx},${ty}) scale(${scale})`;
}
