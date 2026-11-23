export const fmtNum = (n: number) => new Intl.NumberFormat().format(Math.round(n));

export function fmtAgo(iso: string) {
  const t = new Date(iso).getTime();
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  if (s < 86400 * 7) return `${Math.floor(s / 86400)}d`;
  const d = new Date(iso);
  const year = d.getFullYear() !== new Date().getFullYear() ? "numeric" : undefined;
  return d.toLocaleDateString(undefined, { year, month: "short", day: "numeric" });
}

// The year shows only when it isn't this year's.
export function fmtDate(iso: string) {
  const d = new Date(iso);
  const year = d.getFullYear() !== new Date().getFullYear() ? "numeric" : undefined;
  return d.toLocaleString(undefined, { year, month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function fmtDuration(ms?: number | null) {
  if (!ms) return "";
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

// Ties share a rank: 1, 2, 3, 3, 5.
export function ranks(plays: number[]) {
  return plays.map((p) => plays.indexOf(p) + 1);
}

// Stable hue from a string, for placeholder covers.
export function hue(s: string) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h % 360;
}

export const fmtDay = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
