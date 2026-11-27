// Thin client for the younithm HTTP API. Cookie sessions; writes send the
// CSRF token the session endpoints hand back.

let csrf = "";

// The path the app is served under ("" at the root, else "/younithm"). The
// server writes it into index.html as <base href>; the dev server has none.
export const basePath = (document.querySelector("base")?.getAttribute("href") || "/").replace(/\/+$/, "");

// withBase turns a root-relative app URL into one that works under basePath.
export const withBase = (u: string) => (u.startsWith("/") ? basePath + u : u);

// Identical GETs already in flight share one request, so tiles that need the
// same data don't each spend the public rate limit on it.
const inflight = new Map<string, Promise<unknown>>();
export const setCsrf = (t: string) => { csrf = t; };

export class ApiError extends Error {
  constructor(public status: number, public code: string, message?: string) {
    super(message || code);
  }
}

async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  // A write makes any read already in flight possibly stale: don't hand it
  // to callers that ask after this point.
  if (method !== "GET") inflight.clear();
  const headers: Record<string, string> = {};
  let payload: BodyInit | undefined;
  if (body instanceof FormData) payload = body;
  else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  if (method !== "GET" && csrf) headers["X-CSRF-Token"] = csrf;
  const res = await fetch(withBase(url), { method, headers, body: payload, credentials: "same-origin" });
  const text = await res.text();
  const data = text ? safeJSON(text) : null;
  if (!res.ok) {
    const d = (data || {}) as { error?: string; message?: string };
    throw new ApiError(res.status, d.error || String(res.status), d.message);
  }
  return data as T;
}

function safeJSON(t: string) {
  try { return JSON.parse(t); } catch { return null; }
}

function get<T>(url: string): Promise<T> {
  let p = inflight.get(url) as Promise<T> | undefined;
  if (!p) {
    const req = request<T>("GET", url).finally(() => { if (inflight.get(url) === req) inflight.delete(url); });
    p = req;
    inflight.set(url, p);
  }
  return p;
}

export const api = {
  get,
  post: <T,>(u: string, b?: unknown) => request<T>("POST", u, b ?? {}),
  put: <T,>(u: string, b?: unknown) => request<T>("PUT", u, b ?? {}),
  patch: <T,>(u: string, b?: unknown) => request<T>("PATCH", u, b ?? {}),
  del: <T,>(u: string) => request<T>("DELETE", u),
};

// ---------- Types ----------

export type NamingPref = { mode: string; order: string[] };
export type LanguagePrefs = { languages: string[] };
export type Range = "week" | "month" | "year" | "all";
// "latest_week" is the seven days up to the newest listen, for when nothing is recent.
export type TopRange = Range | "latest_week";
export type TopKind = "artists" | "albums" | "tracks";

export interface DebugState {
  fake_now_playing: boolean; import_limit: number;
  counts: { scrobbles: number; recordings: number; releases: number; artists: number; imports: number };
}

export interface Profile { display_name: string; avatar: string }

export interface Me {
  authenticated: boolean;
  username?: string;
  csrf?: string;
  step_up?: boolean;
  setup_required?: boolean;
}

export interface NowPlaying {
  playing: boolean;
  track?: { artist: string; title: string; album?: string; image?: string; duration_ms?: number; recording_id?: number };
  source?: string;
  started_at?: string;
  progress_ms?: number;
}

export interface Listen {
  id: number; played_at: string; artist: string; title: string; album: string | null; recording_id?: number | null; release_id?: number | null; image?: string;
  artists?: Credit[]; // credited artists in order; empty when unmatched
}

export interface ChartPoint { label: string; plays: number }
export interface Chart { month: ChartPoint[]; year: ChartPoint[]; all: ChartPoint[] }
export interface Credit { id: number; name: string; join_phrase?: string; credited_name?: string }
// artist: whom the album is credited to (appears on only).
export interface ArtistAlbum { id: number; title: string; artist?: string; date: string | null; type: string | null; plays: number; image?: string }
export interface ArtistPage {
  id: number; name: string; display_name?: string | null; image?: string; image_custom?: boolean; aliases: string[]; plays: number; rank: number; first: string | null; last: string | null;
  links: Record<string, string>; chart: Chart;
  top_tracks: { id: number; title: string; artist: string; plays: number; image?: string }[];
  albums: ArtistAlbum[];
  appears_on: ArtistAlbum[];
}
export interface ReleasePage {
  id: number; title: string; display_name?: string | null; image?: string; image_custom?: boolean; date: string | null; type: string | null; artists: Credit[]; links: Record<string, string>;
  plays: number; plays_any: number; first: string | null; last: string | null; chart: Chart;
  tracks: { id: number; title: string; artist: string; disc: number; position: number | null; duration_ms: number | null; plays: number }[];
}
export interface NameVariant { source: string; title: string; artists?: string }
export interface SearchHit { id: number; name: string; sub?: string }
export interface MergeCandidate { id: number; name: string; sub?: string; plays: number; image?: string; reason?: string }
export interface CreditIn { artist_id?: number; name?: string; join_phrase?: string; credited_name?: string }
export interface RecordingPage {
  names: NameVariant[];
  id: number; title: string; display_name?: string | null; image?: string; duration_ms: number | null; artists: Credit[]; aliases: string[]; links: Record<string, string>;
  plays: number; first: string | null; last: string | null; chart: Chart;
  releases: { id: number; title: string; date: string | null; type: string | null; primary: boolean; plays: number; image?: string }[];
  no_album: boolean;
}
export interface TopItem { name: string; artist?: string; id?: number; plays: number; image?: string }
export interface Stats {
  range: string; plays: number; distinct_tracks: number; distinct_artists: number;
  listened_ms: number; per_day: { date: string; plays: number }[];
}

export type LibraryKind = "artists" | "albums" | "tracks";
export type LibrarySort = "plays" | "name" | "last";
export interface LibraryItem { id: number; name: string; artist?: string; artists?: Credit[]; album_id?: number; album?: string; plays: number; last_played: string | null; image?: string }
export interface Scrobble extends Listen {
  source: string | null; recording_id: number | null; release_id: number | null; matched: boolean;
  artists: Credit[]; // credited artists in order; empty when unmatched
}

export interface ScrobbleEditBody {
  played_at?: string;
  recording_id?: number | null;
  release_id?: number | null;
  link_release?: boolean;
  new_recording?: { title: string; artists: ({ artist_id: number } | { name: string })[] };
  new_release?: string;
}
export interface EditResult { edit_id: number; summary: string; changes: number }

export interface ReviewItem {
  recording_id: number; title: string; artist: string; album: string | null; duration_ms: number | null;
  candidates: {
    link_id: number; provider: string; provider_id: string; method: string; confidence: number;
    title?: string; artists?: string[]; releases?: string[]; duration_ms?: number;
    release_details?: { title: string; date?: string; type?: string; status?: string }[];
    release_count?: number; isrcs?: string[]; known?: boolean; disambiguation?: string;
  }[];
}

export interface ImportJob {
  id: number; status: string; phase: string; filename: string; format: string;
  progress: { rows_seen: number; parsed: number; skipped: number; errors: number; new: number; duplicates: number; inserted: number };
  preview?: {
    date_range?: { earliest?: string; latest?: string };
    counts?: { new?: number; duplicates?: number; skipped?: number; parsed?: number };
    out_of_range?: number;
    sample?: { played_at: string; artist: string; title: string; album?: string; outcome: string }[];
  };
  error?: string; missing?: string[]; created_at: string; finished_at?: string;
  options?: { debug_limit?: number; fuzzy_consent?: boolean; from?: string; to?: string };
}

export interface SecretMeta { name: string; set: boolean; hint?: string; updated_at?: string }

// ---------- Endpoints ----------

export interface AliasEntry { name: string; search_only: boolean; source?: string; display?: string }

export const endpoints = {
  me: () => api.get<Me>("/api/v1/auth/me"),
  login: (username: string, password: string) =>
    api.post<{ username: string; csrf: string }>("/api/v1/auth/login", { username, password }),
  logout: () => api.post("/api/v1/auth/logout"),
  changeUsername: (current: string, username: string) =>
    api.post<{ username: string }>("/api/v1/auth/username", { current, username }),
  changePassword: (current: string, next: string, signOutOthers: boolean) =>
    api.post("/api/v1/auth/password", { current, new: next, sign_out_others: signOutOthers }),
  setup: (username: string, password: string) =>
    api.post<{ username: string; csrf: string }>("/api/v1/setup", { username, password }),

  nowPlaying: () => api.get<NowPlaying>("/api/v1/now-playing"),
  recent: (limit = 10) => api.get<{ items: Listen[] }>(`/api/v1/recent?limit=${limit}`),
  top: (kind: TopKind, range: TopRange, limit = 5) =>
    api.get<{ items: TopItem[] }>(`/api/v1/top/${kind}?range=${range}&limit=${limit}`),
  artist: (id: string) => api.get<ArtistPage>(`/api/v1/artists/${id}`),
  release: (id: string) => api.get<ReleasePage>(`/api/v1/releases/${id}`),
  recording: (id: string) => api.get<RecordingPage>(`/api/v1/recordings/${id}`),
  recordingHistory: (id: string) => api.get<{ items: { id: number; played_at: string; release_id: number | null; album: string | null }[] }>(`/api/v1/recordings/${id}/history`),
  librarySearch: (kind: LibraryKind, q: string, limit: number) =>
    api.get<{ total: number; items: LibraryItem[] }>(`/api/v1/library/${kind}?sort=plays&limit=${limit}&q=${encodeURIComponent(q)}`),
  artistTracks: (artist: number, sort: LibrarySort, q: string, offset = 0) =>
    api.get<{ total: number; items: LibraryItem[] }>(`/api/v1/library/tracks?artist=${artist}&sort=${sort}&limit=60&offset=${offset}${q ? `&q=${encodeURIComponent(q)}` : ""}`),
  library: (kind: LibraryKind, sort: LibrarySort, q: string, offset = 0) =>
    api.get<{ total: number; items: LibraryItem[] }>(`/api/v1/library/${kind}?sort=${sort}&limit=60&offset=${offset}${q ? `&q=${encodeURIComponent(q)}` : ""}`),
  site: () => api.get<{ sections: Record<string, boolean> }>("/api/v1/site"),
  // tz: per_day is counted in the viewer's days, so "today" is theirs.
  stats: (range: Range) => api.get<Stats>(`/api/v1/stats?range=${range}&tz=${encodeURIComponent(viewerTimeZone())}`),

  page: () => api.get<{ page: unknown }>("/api/v1/site/page"),
  savePage: (page: unknown) => api.put("/api/v1/admin/site/page", { page }),
  debug: () => api.get<DebugState>("/api/v1/admin/debug"),
  setDebug: (p: { fake_now_playing?: boolean; import_limit?: number }) => api.put<DebugState>("/api/v1/admin/debug", p),
  clearLibrary: () => api.post<DebugState>("/api/v1/admin/debug/clear-library", { confirm: "DELETE" }),
  profile: () => api.get<Profile>("/api/v1/site/profile"),
  saveProfile: (p: { display_name?: string; avatar?: string }) => api.put<Profile>("/api/v1/admin/profile", p),

  scrobbles: (q: string, cursor?: string) =>
    api.get<{ items: Scrobble[]; next_cursor?: string; total: number }>(
      `/api/v1/scrobbles?limit=50${q ? `&q=${encodeURIComponent(q)}` : ""}${cursor ? `&cursor=${cursor}` : ""}`),

  review: (after?: number) => api.get<{ items: ReviewItem[]; next_after_id: number | null }>(`/api/v1/admin/review?limit=100${after ? `&after_id=${after}` : ""}`),
  manualLink: (recordingId: number, url: string) => api.post(`/api/v1/admin/review/recordings/${recordingId}/link`, { url }),
  confirmLink: (id: number) => api.post(`/api/v1/admin/review/links/${id}/confirm`),
  rejectLink: (id: number) => api.post(`/api/v1/admin/review/links/${id}/reject`),
  rematch: (id: number) => api.post(`/api/v1/admin/review/recordings/${id}/rematch`),
  matching: () => api.get<{ high_precision: boolean }>("/api/v1/admin/matching"),
  setMatching: (high_precision: boolean) => api.put("/api/v1/admin/matching", { high_precision }),
  enrichment: () => api.get<Record<string, number>>("/api/v1/admin/enrichment"),
  requeue: () => api.post<{ queued: number }>("/api/v1/admin/enrichment/requeue"),

  imports: () => api.get<{ jobs: ImportJob[] }>("/api/v1/admin/imports"),
  importJob: (id: number) => api.get<ImportJob>(`/api/v1/admin/imports/${id}`),
  upload: (files: File[], opts: { debugLimit?: number; fuzzyConsent?: boolean; from?: string; to?: string } = {}) => {
    const fd = new FormData();
    files.forEach((f) => fd.append("file", f));
    const q = new URLSearchParams();
    if (opts.debugLimit) q.set("debug_limit", String(opts.debugLimit));
    if (opts.fuzzyConsent) q.set("fuzzy_consent", "true");
    if (opts.from) q.set("from", opts.from);
    if (opts.to) q.set("to", opts.to);
    if (opts.from || opts.to) q.set("tz", viewerTimeZone());
    const qs = q.toString();
    return api.post<ImportJob>(`/api/v1/admin/imports${qs ? `?${qs}` : ""}`, fd);
  },
  confirmImport: (id: number) => api.post<ImportJob>(`/api/v1/admin/imports/${id}/confirm`),
  // Yes now, even while the files are still awaited or being read: it applies once previewed.
  confirmImportWhenReady: (id: number) => api.post<ImportJob>(`/api/v1/admin/imports/${id}/confirm?when_ready=1`),
  addImportFile: (id: number, file: File) => {
    const fd = new FormData();
    fd.append("file", file);
    return api.post<ImportJob>(`/api/v1/admin/imports/${id}/files`, fd);
  },
  cancelImport: (id: number) => api.post(`/api/v1/admin/imports/${id}/cancel`),
  undoImport: (id: number) => api.post<{ removed: number }>(`/api/v1/admin/imports/${id}/undo`),

  secrets: () => api.get<{ secrets: SecretMeta[] }>("/api/v1/admin/secrets"),
  setSecret: (name: string, value: string) => api.put(`/api/v1/admin/secrets/${name}`, { value }),
  deleteSecret: (name: string) => api.del(`/api/v1/admin/secrets/${name}`),
  // Tries these details against the service without storing them.
  checkConnectionWith: (provider: string, values: Record<string, string>) =>
    api.post<{ ok: boolean; error_kind?: string; detail?: string }>(`/api/v1/admin/connections/${provider}/check`, { values }),
  onboarding: () => api.get<{ done: boolean }>("/api/v1/admin/onboarding"),
  finishOnboarding: () => api.post<{ done: boolean }>("/api/v1/admin/onboarding/done"),
  scrobbling: () => api.get<{ require_token: boolean; token_set: boolean; username: string }>("/api/v1/admin/scrobbling"),
  setScrobbling: (require_token: boolean, token: string) => api.put("/api/v1/admin/scrobbling", { require_token, token }),
  checkImportFile: (body: { head: string } | { entries: string[] }) => api.post<{ format: string; roles?: string[] }>("/api/v1/admin/imports/check", body),

  visibility: () => api.get<{ sections: { key: string; label: string; public: boolean }[]; debug?: boolean }>("/api/v1/admin/settings"),

  editEntity: (kind: string, id: number, fields: Record<string, unknown>) =>
    api.patch(`/api/v1/admin/entities/${kind}/${id}`, { fields }),
  naming: () => api.get<NamingPref>("/api/v1/admin/naming"),
  setNaming: (mode: string, order?: string[]) => api.put("/api/v1/admin/naming", { mode, order }),
  languages: () => api.get<LanguagePrefs>("/api/v1/admin/languages"),
  setLanguages: (languages: string[]) => api.put<LanguagePrefs>("/api/v1/admin/languages", { languages }),
  search: (kind: "artist" | "recording" | "release", q: string) => api.get<{ items: SearchHit[] }>(`/api/v1/admin/search?kind=${kind}&q=${encodeURIComponent(q)}`),
  setCredits: (kind: string, id: number, artists: CreditIn[]) => api.put(`/api/v1/admin/entities/${kind}/${id}/artists`, { artists }),
  // An entity's other names; search-only ones never match a listen.
  aliases: (kind: string, id: number) => api.get<{ items: AliasEntry[] }>(`/api/v1/admin/entities/${kind}/${id}/aliases`),
  setAliasSearchOnly: (kind: string, id: number, name: string, search_only: boolean) =>
    api.put<EditResult>(`/api/v1/admin/entities/${kind}/${id}/aliases`, { name, search_only }),
  // Removed names stay removed: no catalogue or listen adds them back.
  addAlias: (kind: string, id: number, name: string, search_only: boolean) =>
    api.post<EditResult & { search_only: boolean }>(`/api/v1/admin/entities/${kind}/${id}/aliases`, { name, search_only }),
  // Capitalization and spacing of a name; the name itself stays the same.
  setAliasSpelling: (kind: string, id: number, name: string, display: string) =>
    api.put<EditResult>(`/api/v1/admin/entities/${kind}/${id}/aliases/spelling`, { name, display }),
  removeAlias: (kind: string, id: number, name: string) => api.post<EditResult>(`/api/v1/admin/entities/${kind}/${id}/aliases/remove`, { name }),
  // The album's artists onto every track on it, as one undoable edit.
  applyAlbumArtists: (rel: number) => api.post<EditResult>(`/api/v1/admin/releases/${rel}/apply-artists`),
  // Looks a track, or every track on an album, up in the catalogues again.
  refresh: (kind: string, id: number) => api.post<{ queued: boolean }>(`/api/v1/admin/entities/${kind}/${id}/refresh`),
  setLink: (kind: string, id: number, url: string) => api.put(`/api/v1/admin/entities/${kind}/${id}/links`, { url }),
  removeLink: (kind: string, id: number, provider: string) => api.del(`/api/v1/admin/entities/${kind}/${id}/links/${provider}`),
  addRelease: (rec: number, body: { release_id?: number; title?: string }) => api.post(`/api/v1/admin/recordings/${rec}/releases`, body),
  removeRelease: (rec: number, rel: number) => api.del(`/api/v1/admin/recordings/${rec}/releases/${rel}`),
  setPrimaryRelease: (rec: number, rel: number) => api.put(`/api/v1/admin/recordings/${rec}/primary-release`, { release_id: rel }),
  setNoAlbum: (rec: number, on: boolean) => api.put<EditResult>(`/api/v1/admin/recordings/${rec}/no-album`, { no_album: on }),
  setImage: (kind: string, id: number, file: File) => {
    const fd = new FormData();
    fd.append("file", file);
    return api.post<EditResult>(`/api/v1/admin/entities/${kind}/${id}/image`, fd);
  },
  resetImage: (kind: string, id: number) => api.del<EditResult>(`/api/v1/admin/entities/${kind}/${id}/image`),
  removeImage: (kind: string, id: number) => api.post<EditResult>(`/api/v1/admin/entities/${kind}/${id}/image/remove`),
  candidates: (kind: string, q: string) =>
    api.get<{ items: MergeCandidate[] }>(`/api/v1/admin/entities/${kind}/candidates?q=${encodeURIComponent(q)}`),
  mergeCandidates: (kind: string, id: number, q: string) =>
    api.get<{ items: MergeCandidate[] }>(`/api/v1/admin/entities/${kind}/${id}/merge-candidates${q ? `?q=${encodeURIComponent(q)}` : ""}`),
  // once: merge what is there now without sending later listens under the old name to the kept entry.
  merge: (kind: string, id: number, into: number, once = false) => api.post<EditResult>(`/api/v1/admin/entities/${kind}/${id}/merge`, { into, once }),
  // Deletes an artist, album or track and every scrobble of it.
  deleteEntity: (kind: string, id: number) => api.del<EditResult>(`/api/v1/admin/entities/${kind}/${id}`),
  splitArtist: (id: number, artists: CreditIn[]) => api.post<EditResult>(`/api/v1/admin/artists/${id}/split`, { artists }),
  undo: (editId: number) => api.post(`/api/v1/admin/edits/${editId}/undo`),
  editScrobble: (id: number, body: ScrobbleEditBody) => api.patch<EditResult>(`/api/v1/admin/scrobbles/${id}`, body),
  deleteScrobble: (id: number) => api.del<EditResult>(`/api/v1/admin/scrobbles/${id}`),
  deleteScrobbles: (ids: number[]) => api.post<EditResult>("/api/v1/admin/scrobbles/delete", { ids }),
};

export const reauth = (password: string) => api.post("/api/v1/auth/reauth", { password });

// Runs fn; if the server wants a fresh password first, asks once and retries.
export async function withStepUp<T>(fn: () => Promise<T>, ask: () => Promise<string | null>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    if (e instanceof ApiError && e.code === "step_up_required") {
      const pw = await ask();
      if (!pw) throw e;
      await reauth(pw);
      return fn();
    }
    throw e;
  }
}

/** The browser's IANA time zone, or "" when it cannot tell. */
export function viewerTimeZone(): string {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch { return ""; }
}
