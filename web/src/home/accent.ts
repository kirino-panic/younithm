// The "auto" accent: a colour picked to sit well with the page background.
// It takes the background's dominant hue and gives it an accent's saturation
// and lightness. Near-grey backgrounds have no hue worth following, so they
// fall back to the first preset.

import { useEffect, useState } from "react";
import { ACCENTS, type Theme } from "./doc";

export const AUTO = "auto";
const FALLBACK = ACCENTS[0];

type Rgb = [number, number, number];

function hexToRgb(hex: string): Rgb | null {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

// Hue in degrees and HSV-style chroma (0–1), which ignores how dark the colour is.
function hueOf([r, g, b]: Rgb): { h: number; c: number } {
  const max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min;
  if (d === 0) return { h: 0, c: 0 };
  let h = max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  h *= 60;
  return { h: h < 0 ? h + 360 : h, c: d / max };
}

function hslToHex(h: number, s: number, l: number): string {
  const a = s * Math.min(l, 1 - l);
  const f = (n: number) => {
    const k = (n + h / 30) % 12;
    const v = l - a * Math.max(-1, Math.min(k - 3, 9 - k, 1));
    return Math.round(v * 255).toString(16).padStart(2, "0");
  };
  return `#${f(0)}${f(8)}${f(4)}`;
}

// The weighted dominant hue of some pixels, or null when they are all grey.
function dominantHue(pixels: Rgb[]): number | null {
  const bins = new Array(36).fill(0);
  let total = 0;
  for (const p of pixels) {
    const { h, c } = hueOf(p);
    const w = c * c; // favour colourful pixels
    bins[Math.floor(h / 10) % 36] += w;
    total += w;
  }
  if (total / Math.max(pixels.length, 1) < 0.02) return null;
  // Smooth across neighbours so a hue split over two bins still wins.
  let best = 0, bestScore = -1;
  for (let i = 0; i < 36; i++) {
    const s = bins[(i + 35) % 36] + bins[i] * 2 + bins[(i + 1) % 36];
    if (s > bestScore) { bestScore = s; best = i; }
  }
  return best * 10 + 5;
}

const fromHue = (h: number | null, light: boolean) => (h === null ? FALLBACK : hslToHex(h, 0.68, light ? 0.48 : 0.64));

function fromColors(colors: string[], light: boolean): string {
  const px = colors.map(hexToRgb).filter((x): x is Rgb => !!x);
  return fromHue(dominantHue(px), light);
}

const imageCache = new Map<string, number | null>();

function sampleImage(src: string): Promise<number | null> {
  if (imageCache.has(src)) return Promise.resolve(imageCache.get(src)!);
  return new Promise((resolve) => {
    const img = new Image();
    img.crossOrigin = "anonymous";
    const done = (h: number | null) => { imageCache.set(src, h); resolve(h); };
    img.onload = () => {
      try {
        const size = 32;
        const cv = document.createElement("canvas");
        cv.width = cv.height = size;
        const ctx = cv.getContext("2d");
        if (!ctx) return done(null);
        ctx.drawImage(img, 0, 0, size, size);
        const d = ctx.getImageData(0, 0, size, size).data;
        const px: Rgb[] = [];
        for (let i = 0; i < d.length; i += 4) if (d[i + 3] > 128) px.push([d[i], d[i + 1], d[i + 2]]);
        done(dominantHue(px));
      } catch {
        done(null); // cross-origin image without CORS
      }
    };
    img.onerror = () => done(null);
    img.src = src;
  });
}

// The accent colour to use, resolving "auto" against the background.
export function useResolvedAccent(th: Theme, light: boolean): string {
  const auto = th.accent === AUTO;
  const image = auto && th.bg.kind === "image" ? th.bg.image : "";
  const [imageHue, setImageHue] = useState<{ src: string; h: number | null } | null>(null);

  useEffect(() => {
    if (!image) return;
    let live = true;
    sampleImage(image).then((h) => live && setImageHue({ src: image, h }));
    return () => { live = false; };
  }, [image]);

  if (!auto) return th.accent;
  if (th.bg.kind === "image") {
    if (!image) return FALLBACK;
    return imageHue?.src === image ? fromHue(imageHue.h, light) : FALLBACK;
  }
  return fromColors(th.bg.kind === "gradient" ? [th.bg.color, th.bg.color2] : [th.bg.color], light);
}
