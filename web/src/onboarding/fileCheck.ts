import { ApiError, endpoints } from "../api";

// What a picked file turned out to be, checked as soon as it's chosen. Only
// the start of the file (or a zip's list of names) goes to the server.
export type FileCheck =
  | { state: "checking" }
  | { state: "ok"; format: string; roles: string[] }
  | { state: "error"; kind: "unknown" | "network" };

export const FORMAT_NAMES: Record<string, string> = {
  spotify: "Spotify", applemusic: "Apple Music", listenbrainz: "ListenBrainz", lastfm: "Last.fm", maloja: "Maloja", koito: "Koito",
};

export const APPLE_FILES: Record<string, string> = {
  play_activity: "Apple Music Play Activity.csv",
  daily_tracks: "Apple Music - Play History Daily Tracks.csv",
};

export async function checkFile(f: File): Promise<FileCheck> {
  try {
    const sig = new Uint8Array(await f.slice(0, 4).arrayBuffer());
    const zip = sig[0] === 0x50 && sig[1] === 0x4b && sig[2] === 3 && sig[3] === 4;
    const r = zip
      ? await endpoints.checkImportFile({ entries: await zipNames(f) })
      : await endpoints.checkImportFile({ head: base64(new Uint8Array(await f.slice(0, 64 * 1024).arrayBuffer())) });
    return { state: "ok", format: r.format, roles: r.roles || [] };
  } catch (e) {
    return { state: "error", kind: e instanceof ApiError && e.status < 500 ? "unknown" : "network" };
  }
}

function base64(b: Uint8Array): string {
  let s = "";
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000));
  return btoa(s);
}

// The entry names from a zip's central directory, read from the end of the
// file without loading the rest. Handles zip64 (exports can be large).
async function zipNames(f: File): Promise<string[]> {
  const tailLen = Math.min(f.size, 22 + 0xffff + 20);
  const tail = new DataView(await f.slice(f.size - tailLen).arrayBuffer());
  let eocd = -1;
  for (let i = tail.byteLength - 22; i >= 0; i--) if (tail.getUint32(i, true) === 0x06054b50) { eocd = i; break; }
  if (eocd < 0) throw new ApiError(422, "unknown_format");
  let size = tail.getUint32(eocd + 12, true);
  let offset = tail.getUint32(eocd + 16, true);
  if (offset === 0xffffffff && eocd >= 20 && tail.getUint32(eocd - 20, true) === 0x07064b50) {
    const at = Number(tail.getBigUint64(eocd - 12, true));
    const z64 = new DataView(await f.slice(at, at + 56).arrayBuffer());
    size = Number(z64.getBigUint64(40, true));
    offset = Number(z64.getBigUint64(48, true));
  }
  const cd = new DataView(await f.slice(offset, offset + size).arrayBuffer());
  const dec = new TextDecoder();
  const names: string[] = [];
  for (let p = 0; p + 46 <= cd.byteLength && cd.getUint32(p, true) === 0x02014b50;) {
    const n = cd.getUint16(p + 28, true), x = cd.getUint16(p + 30, true), c = cd.getUint16(p + 32, true);
    names.push(dec.decode(new Uint8Array(cd.buffer, p + 46, n)));
    p += 46 + n + x + c;
  }
  return names;
}

// What stops an import from being usable, if anything.
export type ImportProblem =
  | { kind: "checking" }
  | { kind: "unknown"; file: string }
  | { kind: "network"; file: string }
  | { kind: "notMulti"; file: string }
  | { kind: "appleMissing"; file: string };

export function importProblem(files: File[], checks: FileCheck[]): ImportProblem | null {
  if (checks.some((c) => !c || c.state === "checking")) return { kind: "checking" };
  for (let i = 0; i < files.length; i++) {
    const c = checks[i];
    if (c.state === "error") return { kind: c.kind, file: files[i].name };
  }
  const ok = checks as Extract<FileCheck, { state: "ok" }>[];
  if (files.length > 1) {
    const odd = ok.findIndex((c) => c.format !== "applemusic");
    if (odd >= 0) return { kind: "notMulti", file: files[odd].name };
  }
  if (ok[0].format === "applemusic") {
    const have = new Set(ok.flatMap((c) => c.roles));
    const missing = Object.keys(APPLE_FILES).find((r) => !have.has(r));
    if (missing) return { kind: "appleMissing", file: APPLE_FILES[missing] };
  }
  return null;
}
