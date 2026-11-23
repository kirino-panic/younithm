// Artist, album and track pages.
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { ChevronDown, Pencil, Trash2 } from "lucide-react";
import { ApiError, endpoints, type ArtistAlbum, type Chart as ChartData, type ChartPoint, type Credit, type LibrarySort, type RecordingPage, type Scrobble } from "../api";
import { useSession } from "../session";
import { fmtDate, fmtDay, fmtDuration, fmtNum } from "../format";
import { Cover, Segmented, useData, useEscape, useToast } from "../ui";
import { EditPanel } from "../admin/EditPanel";
import { ScrobbleEditor } from "../admin/ScrobbleEditor";
import { DeleteDialog, useDeleteScrobble } from "../library/Scrobbles";
import { BrowseTable, SearchField, libraryHref, useBrowse, useInfinite } from "../library/Library";

type Period = keyof ChartData;
type Kind = "artist" | "release" | "recording";

const NAMES: Record<string, string> = { musicbrainz: "MusicBrainz", spotify: "Spotify", apple: "Apple Music" };
const SOURCES: Record<string, string> = { ...NAMES, stored: "As scrobbled", submitted: "Other scrobbles" };
const PATHS: Record<string, Record<Kind, string>> = {
  musicbrainz: { artist: "https://musicbrainz.org/artist/", release: "https://musicbrainz.org/release/", recording: "https://musicbrainz.org/recording/" },
  spotify: { artist: "https://open.spotify.com/artist/", release: "https://open.spotify.com/album/", recording: "https://open.spotify.com/track/" },
  apple: { artist: "https://music.apple.com/artist/", release: "https://music.apple.com/album/", recording: "https://music.apple.com/song/" },
};

function Tile({ title, end, children }: { title?: ReactNode; end?: ReactNode; children: ReactNode }) {
  return (
    <section className="tile tile-auto">
      {title && <div className="tile-title"><span>{title}</span>{end}</div>}
      {children}
    </section>
  );
}

function Links({ kind, ids }: { kind: Kind; ids: Record<string, string> }) {
  const entries = Object.entries(ids).filter(([p]) => PATHS[p]);
  if (!entries.length) return null;
  return (
    <div className="hero-links">
      {entries.map(([p, id]) => (
        <a key={p} className="badge" href={PATHS[p][kind] + id} target="_blank" rel="noreferrer noopener">{NAMES[p]}</a>
      ))}
    </div>
  );
}

// Chart labels are "2026" (all time), "2026-10" (months) or "2026-10-18" (days).
function prettyLabel(l: string) {
  const [y, m, d] = l.split("-").map(Number);
  if (!m) return String(y);
  return new Date(y, m - 1, d || 1).toLocaleDateString(undefined, d ? { year: "numeric", month: "short", day: "numeric" } : { year: "numeric", month: "short" });
}

// Hover, focus or tap a bar to read it in a tooltip; tap again to release.
function Chart({ points }: { points: ChartPoint[] }) {
  const [hover, setHover] = useState<number | null>(null);
  const [pin, setPin] = useState<number | null>(null);
  if (!points.length) return null;
  const max = Math.max(1, ...points.map((p) => p.plays));
  const active = hover ?? pin;
  const cur = active === null ? null : points[active];
  return (
    <div className="chart">
      <div className="chart-plot" role="group" aria-label="Plays over time" onMouseLeave={() => setHover(null)}>
        {points.map((p, i) => (
          <button key={p.label} type="button" className={`chart-col${i === active ? " on" : ""}`}
            aria-label={`${prettyLabel(p.label)}: ${fmtNum(p.plays)} plays`} aria-pressed={pin === i}
            onMouseEnter={() => setHover(i)} onFocus={() => setHover(i)} onBlur={() => setHover(null)}
            onClick={() => setPin((v) => (v === i ? null : i))}>
            <i style={{ height: `${(p.plays / max) * 100}%` }} />
          </button>
        ))}
        {cur && active !== null && (
          <div className="chart-tip num" aria-hidden
            style={{ left: `clamp(60px, ${((active + 0.5) / points.length) * 100}%, calc(100% - 60px))`, top: `max(0px, calc(${100 - (cur.plays / max) * 100}% - 40px))` }}>
            <span>{prettyLabel(cur.label)}</span> <strong>{fmtNum(cur.plays)} {cur.plays === 1 ? "play" : "plays"}</strong>
          </div>
        )}
      </div>
    </div>
  );
}

function PeriodPicker({ value, onChange }: { value: Period; onChange: (p: Period) => void }) {
  return <Segmented label="Period" value={value} onChange={onChange} options={[{ value: "month", label: "Month" }, { value: "year", label: "Year" }, { value: "all", label: "All" }]} />;
}

function PlayStats({ plays, note, first, last }: { plays: number; note?: string; first?: string | null; last?: string | null }) {
  return (
    <Tile title="Your stats">
      <div className="stat-grid">
        <div className="stat"><span className="stat-label">Play count</span><span className="stat-value num">{fmtNum(plays)}{note && <span className="stat-note">{note}</span>}</span></div>
        <div className="stat"><span className="stat-label">First played</span><span className="stat-value num">{first ? fmtDay(first) : "–"}</span></div>
        <div className="stat"><span className="stat-label">Last played</span><span className="stat-value num">{last ? fmtDay(last) : "–"}</span></div>
      </div>
    </Tile>
  );
}

function PlaysChart({ chart }: { chart: ChartData }) {
  const [period, setPeriod] = useState<Period>("year");
  return <Tile title="Plays over time" end={<PeriodPicker value={period} onChange={setPeriod} />}><Chart key={period} points={chart[period]} /></Tile>;
}

function EditButton({ onClick }: { onClick: () => void }) {
  const { admin } = useSession();
  if (!admin) return null;
  return <button className="btn btn-icon hero-edit" aria-label="Edit" onClick={onClick}><Pencil size={16} /></button>;
}

function Credits({ artists }: { artists: Credit[] }) {
  return (
    <>
      {artists.map((a, i) => (
        <span key={a.id}>
          <Link to={`/artist/${a.id}`} className="credit">{a.credited_name || a.name}</Link>
          {i < artists.length - 1 && (a.join_phrase || ", ")}
        </span>
      ))}
    </>
  );
}

const IMAGE_TYPES = ["image/jpeg", "image/png", "image/webp", "image/gif"];
const MAX_IMAGE = 8 << 20;

// For the admin, an image file dropped on an artist's or album's cover
// replaces it at once (one undoable edit, as Choose… in the edit panel does).
function DropCover({ kind, id, round, onChanged, children }: { kind: "artist" | "release"; id: number; round?: boolean; onChanged: () => void; children: ReactNode }) {
  const { admin } = useSession();
  const toast = useToast();
  const [over, setOver] = useState(false);
  const [busy, setBusy] = useState(false);
  // A file dropped just beside the cover would otherwise open in the tab.
  useEffect(() => {
    if (!admin) return;
    const stop = (e: DragEvent) => { if (e.dataTransfer?.types.includes("Files")) e.preventDefault(); };
    window.addEventListener("dragover", stop);
    window.addEventListener("drop", stop);
    return () => { window.removeEventListener("dragover", stop); window.removeEventListener("drop", stop); };
  }, [admin]);
  if (!admin) return <>{children}</>;

  const isFile = (e: React.DragEvent) => e.dataTransfer.types.includes("Files");
  const enter = (e: React.DragEvent) => {
    if (!isFile(e) || busy) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
    setOver(true);
  };
  const drop = async (e: React.DragEvent) => {
    if (!isFile(e)) return;
    e.preventDefault();
    setOver(false);
    const f = e.dataTransfer.files[0];
    if (!f || busy) return;
    if (!IMAGE_TYPES.includes(f.type)) { toast({ text: "Drop a JPEG, PNG, WebP or GIF image." }); return; }
    if (f.size > MAX_IMAGE) { toast({ text: "Choose an image up to 8 MB." }); return; }
    setBusy(true);
    try {
      const res = await endpoints.setImage(kind, id, f);
      onChanged();
      toast({ text: "Image replaced", action: { label: "Undo", run: () => endpoints.undo(res.edit_id).then(onChanged).catch(() => toast({ text: "Couldn't undo." })) } });
    } catch (err) {
      toast({ text: err instanceof ApiError && err.message ? err.message : "Couldn't replace the image." });
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className={`drop-cover${round ? " round" : ""}${over ? " over" : ""}${busy ? " busy" : ""}`}
      onDragEnter={enter} onDragOver={enter} onDrop={drop}
      onDragLeave={(e) => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(false); }}>
      {children}
      <span className="drop-hint" aria-hidden>{busy ? "Uploading…" : "Drop to replace"}</span>
    </div>
  );
}

// The header all three pages share: cover, title, one subtitle line, facts, links.
function Hero({ cover, title, sub, keepSub, note, meta, links, onEdit }: {
  // keepSub holds the subtitle's line open when there is none, so the title stays put.
  cover: ReactNode; title: ReactNode; sub?: ReactNode; keepSub?: boolean; note?: ReactNode;
  meta?: (string | false | null | undefined | 0)[]; links: ReactNode; onEdit: () => void;
}) {
  const facts = (meta || []).filter(Boolean) as string[];
  return (
    <header className="hero">
      {cover}
      <div className="hero-text">
        {typeof title === "string" ? <h1>{title}</h1> : title}
        {keepSub ? <div className="hero-sub hero-sub-keep">{sub || "\u00a0"}</div> : sub && <div className="hero-sub">{sub}</div>}
        {note && <div className="hero-note">{note}</div>}
        {facts.length > 0 && <div className="hero-meta num">{facts.map((f) => <span key={f}>{f}</span>)}</div>}
        {links}
      </div>
      <EditButton onClick={onEdit} />
    </header>
  );
}

// fill: rows share the card's spare height instead of leaving it blank below.
function Rows({ children, empty, fill }: { children: ReactNode[]; empty: string; fill?: boolean }) {
  return children.length ? <div className={fill ? "list list-fill" : "list"}>{children}</div> : <div className="empty">{empty}</div>;
}

// Loading, hidden and missing states, shared by the three pages.
function Gate<T>({ q, children }: { q: { data?: T; error?: unknown; loading: boolean }; children: (d: T) => ReactNode }) {
  if (q.data) return <>{children(q.data)}</>;
  if (q.loading) return null;
  const e = q.error;
  const msg = e instanceof ApiError && e.status === 404 ? "Not found." : e instanceof ApiError && e.status === 403 ? "Hidden." : "Couldn't load this page.";
  return <div className="page"><div className="empty">{msg}</div></div>;
}

const TOP_TRACKS = 5;
const year = (d?: string | null) => d?.slice(0, 4) || "";
const cap = (s?: string | null) => (s ? s[0].toUpperCase() + s.slice(1) : "");

// ---------- Artist ----------

export function ArtistPage() {
  const { id = "" } = useParams();
  const q = useData(() => endpoints.artist(id), [id]);
  const [editing, setEditing] = useState(false);
  return (
    <Gate q={q}>{(a) => (
      <div className="page">
        <Hero
          cover={<DropCover kind="artist" id={a.id} round onChanged={q.reload}><Cover name={a.name} src={a.image} size={148} round /></DropCover>}
          title={a.name}
          sub={a.aliases.length > 0 && <>Also known as {a.aliases.join(", ")}</>}
          keepSub
          meta={["Artist"]}
          links={<Links kind="artist" ids={a.links} />}
          onEdit={() => setEditing(true)}
        />

        <div className="stack">
          <div className="grid-2">
            <div className="stack">
              <PlayStats plays={a.plays} note={a.rank > 0 ? `#${a.rank} all time` : undefined} first={a.first} last={a.last} />
              <PlaysChart chart={a.chart} />
            </div>
            <Tile title="Top tracks">
              <Rows empty="No tracks yet." fill>
                {a.top_tracks.slice(0, TOP_TRACKS).map((t, i) => (
                  <Link key={t.id} to={`/track/${t.id}`} className="list-row">
                    <span className="list-rank num">{String(i + 1).padStart(2, "0")}</span>
                    <span className="list-main">
                      <span className="list-title truncate">{t.title}</span>
                      <span className="list-bar" aria-hidden><i style={{ width: `${Math.max(2, (t.plays / Math.max(1, a.top_tracks[0].plays)) * 100)}%` }} /></span>
                    </span>
                    <span className="list-end list-count num">{fmtNum(t.plays)}</span>
                  </Link>
                ))}
              </Rows>
            </Tile>
          </div>
          {a.albums.length > 0 && <AlbumGrid title="Albums" albums={a.albums} />}
          {a.appears_on.length > 0 && <AlbumGrid title="Appears on" albums={a.appears_on} />}
          <AllTracks artist={a.id} />
        </div>
        {editing && <EditPanel kind="artist" id={a.id} name={a.name} override={a.display_name} image={a.image} imageCustom={a.image_custom} links={a.links} onClose={() => setEditing(false)} onSaved={q.reload} />}
      </div>
    )}</Gate>
  );
}

const TRACK_SORTS: { value: LibrarySort; label: string }[] = [
  { value: "plays", label: "Plays" }, { value: "name", label: "Name" }, { value: "last", label: "Recent" },
];

// Every track the artist is credited on, played or not, as the library lists tracks.
function AllTracks({ artist }: { artist: number }) {
  const [sort, setSort] = useState<LibrarySort>("plays");
  const [q, setQ] = useState("");
  const [query, setQuery] = useState("");
  useEffect(() => { const t = window.setTimeout(() => setQuery(q.trim()), 250); return () => window.clearTimeout(t); }, [q]);
  const [total, setTotal] = useState<number | null>(null);
  return (
    <Tile title="All tracks" end={total !== null && <span className="muted num">{fmtNum(total)}</span>}>
      <div className="lib-bar artist-tracks-bar">
        <SearchField value={q} onChange={setQ} placeholder="Title, artist or album" />
        <div className="lib-controls">
          <Segmented label="Sort" value={sort} onChange={setSort} options={TRACK_SORTS} />
        </div>
      </div>
      <TrackPages key={`${artist}/${sort}/${query}`} artist={artist} sort={sort} query={query} onTotal={(n) => { if (!query) setTotal(n); }} />
    </Tile>
  );
}

function TrackPages({ artist, sort, query, onTotal }: { artist: number; sort: LibrarySort; query: string; onTotal: (n: number) => void }) {
  const load = useCallback((offset: number) => endpoints.artistTracks(artist, sort, query, offset), [artist, sort, query]);
  const b = useBrowse(load);
  const sentinel = useInfinite(b.more, b.done || b.failed, b.busy);
  useEffect(() => { if (b.total !== null) onTotal(b.total); }, [b.total]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <>
      <BrowseTable kind="tracks" items={b.items} href={libraryHref("tracks")} />
      {b.total === 0 && <div className="empty">{query ? "Nothing matches." : "No tracks yet."}</div>}
      {b.failed && <div className="empty">Couldn't load. <button className="btn" onClick={b.more}>Retry</button></div>}
      {!b.done && !b.failed && <div ref={sentinel} className="empty faint">{b.busy ? "Loading…" : ""}</div>}
    </>
  );
}

// ---------- Album ----------

export function AlbumPage() {
  const { id = "" } = useParams();
  const q = useData(() => endpoints.release(id), [id]);
  const [editing, setEditing] = useState(false);
  return (
    <Gate q={q}>{(al) => {
      const discs = [...new Set(al.tracks.map((t) => t.disc))];
      const albumArtists = al.artists.map((a, i) => (a.credited_name || a.name) + (i < al.artists.length - 1 ? a.join_phrase || ", " : "")).join("");
      return (
        <div className="page">
          <Hero
            cover={<DropCover kind="release" id={al.id} onChanged={q.reload}><Cover name={al.title} src={al.image} size={148} /></DropCover>}
            title={al.title}
            sub={al.artists.length > 0 && <Credits artists={al.artists} />}
            meta={[cap(al.type) || "Album", year(al.date), al.tracks.length > 0 && `${al.tracks.length} ${al.tracks.length === 1 ? "track" : "tracks"}`]}
            links={<Links kind="release" ids={al.links} />}
            onEdit={() => setEditing(true)}
          />
          <div className="stack">
            <div className="grid-2">
              <div className="stack">
                <PlayStats plays={al.plays} first={al.first} last={al.last} />
                <PlaysChart chart={al.chart} />
              </div>
              <Tile title="Tracklist">
                {!al.tracks.length ? <div className="empty">No tracklist yet.</div> : discs.map((d) => (
                  <div key={d}>
                    {discs.length > 1 && <div className="section-label disc-label">Disc {d}</div>}
                    <div className="list">
                      {al.tracks.filter((t) => t.disc === d).map((t) => (
                        <Link key={t.id} to={`/track/${t.id}`} className="list-row">
                          <span className="list-rank num">{t.position ?? "–"}</span>
                          <span className="list-main">
                            <span className="list-title truncate">{t.title}</span>
                            {t.artist && t.artist !== albumArtists && <span className="list-sub truncate">{t.artist}</span>}
                          </span>
                          <span className="list-end list-col num">{t.duration_ms ? fmtDuration(t.duration_ms) : ""}</span>
                          <span className={`list-end list-col num${t.plays ? "" : " faint"}`}>{t.plays}</span>
                        </Link>
                      ))}
                    </div>
                  </div>
                ))}
              </Tile>
            </div>
          </div>
          {editing && <EditPanel kind="release" id={al.id} name={al.title} override={al.display_name} releaseDate={al.date} image={al.image} imageCustom={al.image_custom} credits={al.artists} links={al.links} onClose={() => setEditing(false)} onSaved={q.reload} />}
        </div>
      );
    }}</Gate>
  );
}

// ---------- Track ----------

// A page title with a chevron menu of its other names. Each entry has an
// optional second line, such as the credit or where the name comes from.
type OtherName = { title: string; sub?: string };

function NamesTitle({ title, others }: { title: string; others: OtherName[] }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEscape(() => setOpen(false));
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => { if (!ref.current?.contains(e.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  return (
    <div className="hero-title" ref={ref}>
      <h1>
        {title}
        {others.length > 0 && (
          <button className="title-more" aria-label="Other names" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
            <ChevronDown size={20} />
          </button>
        )}
      </h1>
      {open && (
        <div className="popover names-popover" role="dialog" aria-label="Other names">
          <div className="section-label" style={{ margin: 0 }}>Other names</div>
          <div className="list">
            {others.map((n) => (
              <div key={n.title + (n.sub || "")} className="list-row">
                <span className="list-main">
                  <span className="list-title">{n.title}</span>
                  {n.sub && <span className="list-sub">{n.sub}</span>}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// The track's other spellings and where each comes from. The spelling already
// shown is left out.
function trackNames(names: { source: string; title: string; artists?: string }[], title: string, artists: Credit[]): OtherName[] {
  const line = artists.map((a, i) => (a.credited_name || a.name) + (i < artists.length - 1 ? a.join_phrase || ", " : "")).join("");
  const other = new Map<string, { title: string; by: string; from: string[] }>();
  for (const n of names) {
    const by = n.artists && n.artists !== line ? n.artists : "";
    if (n.title === title && !by) continue;
    const k = `${n.title}\u0000${by}`;
    const cur = other.get(k) || { title: n.title, by, from: [] };
    cur.from.push(SOURCES[n.source] || n.source);
    other.set(k, cur);
  }
  return [...other.values()].map((n) => ({ title: n.title, sub: [n.by, n.from.join(", ")].filter(Boolean).join(" · ") }));
}

export function TrackPage() {
  const { id = "" } = useParams();
  const q = useData(() => endpoints.recording(id), [id]);
  const { admin } = useSession();
  const history = useData(() => (admin ? endpoints.recordingHistory(id) : Promise.resolve(null)), [id, admin]);
  const [editing, setEditing] = useState(false);
  return (
    <Gate q={q}>{(t) => (
      <div className="page">
        <Hero
          cover={<Cover name={t.releases[0]?.title || t.title} src={t.image} size={148} />}
          title={<NamesTitle title={t.title} others={trackNames(t.names, t.title, t.artists)} />}
          sub={t.artists.length > 0 && <Credits artists={t.artists} />}
          meta={["Track", year((t.releases.find((r) => r.primary) || t.releases[0])?.date), !!t.duration_ms && (t.duration_ms < 60000 ? `${Math.round(t.duration_ms / 1000)} sec` : `${Math.round(t.duration_ms / 60000)} min`)]}
          links={<Links kind="recording" ids={t.links} />}
          onEdit={() => setEditing(true)}
        />
        <div className="stack">
          <div className="grid-2">
            <div className="stack">
              <PlayStats plays={t.plays} first={t.first} last={t.last} />
              <PlaysChart chart={t.chart} />
            </div>
            <Tile title="Appears on">
              <Rows empty="No albums yet.">
                {t.releases.map((al) => (
                  <Link key={al.id} to={`/album/${al.id}`} className="list-row">
                    <Cover name={al.title} src={al.image} size={36} />
                    <span className="list-main">
                      <span className="list-title truncate">{al.title}</span>
                      <span className="list-sub">{[cap(al.type), year(al.date), al.primary ? "Primary" : ""].filter(Boolean).join(" · ")}</span>
                    </span>
                    <span className="list-end num">{fmtNum(al.plays)}</span>
                  </Link>
                ))}
              </Rows>
            </Tile>
          </div>
          {admin && !!history.data?.items.length && (
            <ListeningHistory items={history.data.items} track={t} onChanged={() => { q.reload(); history.reload(); }} />
          )}
        </div>
        {editing && <EditPanel kind="recording" id={t.id} name={t.title} override={t.display_name} image={t.image} credits={t.artists} links={t.links}
          releases={t.releases.map((r) => ({ id: r.id, title: r.title, primary: r.primary }))} noAlbum={t.no_album}
          onClose={() => setEditing(false)} onSaved={() => { q.reload(); history.reload(); }} />}
      </div>
    )}</Gate>
  );
}

type HistoryRow = { id: number; played_at: string; release_id: number | null; album: string | null };

// The track's scrobbles. Edit and delete show on the row under the pointer.
function ListeningHistory({ items, track, onChanged }: { items: HistoryRow[]; track: RecordingPage; onChanged: () => void }) {
  const toast = useToast();
  const [editing, setEditing] = useState<Scrobble | null>(null);
  const [removing, setRemoving] = useState<HistoryRow | null>(null);
  const deleteNow = useDeleteScrobble(onChanged);
  const line = track.artists.map((a, i) => (a.credited_name || a.name) + (i < track.artists.length - 1 ? a.join_phrase || ", " : "")).join("");
  const asScrobble = (h: HistoryRow): Scrobble => ({
    id: h.id, played_at: h.played_at, title: track.title, artist: line, album: h.album, image: track.image,
    recording_id: track.id, release_id: h.release_id, source: null, matched: true, artists: track.artists,
  });
  // Shift skips the confirmation, as on the Scrobbles page.
  const askDelete = (h: HistoryRow, e: React.MouseEvent) => {
    if (e.shiftKey) deleteNow(h).catch((err: Error) => toast({ text: err.message }));
    else setRemoving(h);
  };
  // The sheet and dialog sit outside the tile: the tile's backdrop would pin them inside it.
  return (
    <>
    <Tile title="Listening history">
      <Rows empty="">
        {items.map((h) => (
          <div key={h.id} className="list-row history-row">
            <span className="list-main"><span className="list-title num">{fmtDate(h.played_at)}</span></span>
            {h.album && <span className="list-end truncate">{h.album}</span>}
            <span className="row-tools">
              <button className="btn btn-plain btn-icon" aria-label="Edit scrobble" onClick={() => setEditing(asScrobble(h))}><Pencil size={15} /></button>
              <button className="btn btn-plain btn-icon btn-danger" aria-label="Delete scrobble" title="Delete (Shift-click skips the confirmation)" onClick={(e) => askDelete(h, e)}><Trash2 size={15} /></button>
            </span>
          </div>
        ))}
      </Rows>
    </Tile>
    {editing && <ScrobbleEditor s={editing} onClose={() => setEditing(null)} onSaved={onChanged} />}
    {removing && <DeleteDialog count={1} onClose={() => setRemoving(null)} remove={() => deleteNow(removing)} />}
    </>
  );
}

// A tile of album covers; an album credited to someone else names them.
function AlbumGrid({ title, albums }: { title: string; albums: ArtistAlbum[] }) {
  return (
    <Tile title={title}>
      <div className="cover-grid">
        {albums.map((al) => (
          <Link key={al.id} to={`/album/${al.id}`} className="cover-card">
            <Cover name={al.title} src={al.image} size="100%" />
            <span className="list-title truncate">{al.title}</span>
            {al.artist && <span className="list-sub truncate">{al.artist}</span>}
            <span className="list-sub num">{[year(al.date), `${fmtNum(al.plays)} plays`].filter(Boolean).join(" · ")}</span>
          </Link>
        ))}
      </div>
    </Tile>
  );
}
