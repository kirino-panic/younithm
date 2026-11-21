// The home page document: theme, tiles and custom CSS. Stored as one JSON
// object at /api/v1/site/page.

export type TileType = "now" | "top" | "activity" | "stats" | "recent" | "range" | "text" | "divider" | "profile";

export interface TileDef {
  id: string;
  type: TileType;
  x: number; y: number; w: number; h: number;
  opts: Record<string, unknown>;
  hideMobile?: boolean;
}

export interface Theme {
  mode: "dark" | "light" | "auto";
  accent: string; // a hex colour, or "auto" to follow the background
  bg: { kind: "color" | "gradient" | "image"; color: string; color2: string; image: string; blur: number; dim: number };
  tiles: "solid" | "glass" | "outline" | "none";
  radius: number;
  font: "inter" | "native" | "rounded" | "serif" | "mono"; // native = the system UI font
}

// One tile in the phone layout: order in the list, one or two columns wide.
export interface MobileItem { id: string; span: 1 | 2; hidden?: boolean }

export interface PageDoc {
  version: 1; theme: Theme; tiles: TileDef[]; css: string;
  mirror?: boolean;        // phone layout follows the desktop one (default)
  mobile?: MobileItem[];   // the separate phone layout, used when mirror is false
  greeting?: Greeting;
}

// A header above the tiles, with separate text for the owner (logged in) and
// visitors (logged out). In both, {greeting} is the time of day and {user} the
// display name. An empty text shows nothing to that audience.
export interface Greeting { show: boolean; owner: string; visitor: string }
export const defaultGreeting: Greeting = { show: false, owner: "{greeting}, {user}", visitor: "{user}'s listening stats" };

export function renderGreeting(template: string, user: string, now = new Date()): string {
  return template.replace(/\{greeting\}/g, timeGreeting(now)).replace(/\{(user|name)\}/g, user).replace(/\s+,/g, ",").trim();
}

export function timeGreeting(d = new Date()): string {
  const h = d.getHours();
  return h >= 5 && h < 12 ? "Good morning" : h < 17 && h >= 12 ? "Good afternoon" : h >= 17 && h < 21 ? "Good evening" : "Good night";
}

export const mirrorsDesktop = (d: PageDoc) => d.mirror !== false;

// The phone layout implied by the desktop one: reading order, wide tiles span both columns.
export function mirroredMobile(tiles: TileDef[]): MobileItem[] {
  return [...tiles].sort((a, b) => a.y - b.y || a.x - b.x)
    .map((t) => ({ id: t.id, span: t.w > 3 ? 2 : 1, hidden: t.hideMobile || undefined }));
}

// What the phone shows. A separate layout is kept in step with the tile list:
// removed tiles drop out and new ones are added at the end.
export function mobileLayout(d: PageDoc): MobileItem[] {
  if (mirrorsDesktop(d) || !d.mobile) return mirroredMobile(d.tiles);
  const ids = new Set(d.tiles.map((t) => t.id));
  const own = d.mobile.filter((m) => ids.has(m.id));
  const have = new Set(own.map((m) => m.id));
  return [...own, ...mirroredMobile(d.tiles.filter((t) => !have.has(t.id)))];
}

export const ACCENTS = ["#c8775a", "#8f7cf0", "#3cb38a", "#4b8fe8", "#e0608f", "#e2a43a"];

export const defaultTheme: Theme = {
  mode: "dark",
  accent: "auto",
  bg: { kind: "color", color: "#0e0d10", color2: "#2a1d2e", image: "", blur: 12, dim: 0.35 },
  tiles: "solid",
  radius: 14,
  font: "inter",
};

let n = 0;
export const newId = () => `t${Date.now().toString(36)}${(n++).toString(36)}`;

const t = (type: TileType, x: number, y: number, w: number, h: number, opts: Record<string, unknown> = {}): TileDef =>
  ({ id: newId(), type, x, y, w, h, opts });

// The page a new install starts from, and what "Reset to default" returns to.
export function defaultDoc(): PageDoc {
  return {
    version: 1,
    theme: { ...defaultTheme, tiles: "none", bg: { ...defaultTheme.bg, kind: "gradient" } },
    tiles: [
      t("profile", 0, 0, 4, 3),
      t("now", 0, 3, 4, 3),
      t("activity", 0, 6, 4, 4),
      t("recent", 4, 0, 8, 10, { density: "comfortable" }),
      t("stats", 0, 10, 12, 3, { metrics: ["minutes", "plays", "tracks", "artists", "days", "avg"], density: "comfortable" }),
      t("divider", 0, 13, 12, 1, { label: "" }),
      t("top", 0, 14, 4, 12, { kind: "artists", range: "page" }),
      t("top", 4, 14, 4, 12, { kind: "albums", range: "page" }),
      t("top", 8, 14, 4, 12, { kind: "tracks", range: "page", bars: "on" }),
      t("range", 0, 26, 12, 1),
    ],
    css: "",
    greeting: { show: true, owner: "{greeting}", visitor: "{user}'s Younithm" },
  };
}

export function normalize(raw: unknown): PageDoc {
  const d = (raw && typeof raw === "object" ? raw : {}) as Partial<PageDoc>;
  if (!Array.isArray(d.tiles)) return defaultDoc();
  return {
    version: 1,
    // "system" was the old default; the default is Inter now.
    theme: { ...defaultTheme, ...(d.theme || {}), font: (d.theme?.font as string) === "system" ? "inter" : d.theme?.font ?? defaultTheme.font, bg: { ...defaultTheme.bg, ...(d.theme?.bg || {}) } },
    tiles: d.tiles,
    css: typeof d.css === "string" ? d.css : "",
    mirror: d.mirror !== false,
    mobile: Array.isArray(d.mobile) ? d.mobile : undefined,
    greeting: { ...defaultGreeting, ...(d.greeting || {}) },
  };
}

const FONTS: Record<Theme["font"], string> = {
  inter: "var(--font)",
  native: "var(--font-native)",
  rounded: "var(--font-round)",
  serif: 'ui-serif, "New York", Georgia, serif',
  mono: "var(--font-mono)",
};

// Background and tile style for public pages. Accent and mode are set app-wide by PageProvider.
export function themeVars(th: Theme): Record<string, string> {
  const v: Record<string, string> = {};
  const bg = th.bg;
  v["--page-bg"] = bg.kind === "gradient" ? `linear-gradient(160deg, ${bg.color}, ${bg.color2})` : bg.color;
  v["--page-image"] = bg.kind === "image" && bg.image ? `url("${bg.image.replace(/"/g, "")}")` : "none";
  v["--page-blur"] = bg.kind === "image" ? `${bg.blur}px` : "0px";
  v["--page-dim"] = bg.kind === "image" ? String(bg.dim) : "0";
  v["--radius-tile"] = `${th.radius}px`;
  v["font-family"] = FONTS[th.font];
  if (th.tiles === "glass") {
    v["--tile-bg"] = "color-mix(in srgb, var(--surface) 55%, transparent)";
    v["--tile-blur"] = "18px";
    v["--tile-border"] = "var(--sep)";
  } else if (th.tiles === "none") {
    // No box: same size and padding, but the card itself is invisible.
    v["--tile-bg"] = "transparent";
    v["--tile-blur"] = "0px";
    v["--tile-border"] = "transparent";
    v["--row-sep"] = "transparent";
  } else if (th.tiles === "outline") {
    v["--tile-bg"] = "transparent";
    v["--tile-blur"] = "0px";
    v["--tile-border"] = "var(--sep-strong)";
  }
  return v;
}

export function resolvedMode(th: Theme): "dark" | "light" {
  if (th.mode !== "auto") return th.mode;
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}
