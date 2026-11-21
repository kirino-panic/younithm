import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { LayoutGrid, List, Search } from "lucide-react";
import { endpoints, type LibraryItem, type LibraryKind, type LibrarySort } from "../api";
import { fmtAgo, fmtDate, fmtNum } from "../format";
import { useSession } from "../session";
import { Cover, Segmented, useData } from "../ui";
import { AdminHead } from "../admin/AdminLayout";

type View = "grid" | "list";

const TABS: { value: LibraryKind; label: string }[] = [
  { value: "artists", label: "Artists" }, { value: "albums", label: "Albums" }, { value: "tracks", label: "Tracks" },
];
const SORTS: { value: LibrarySort; label: string }[] = [
  { value: "name", label: "Name" }, { value: "plays", label: "Plays" }, { value: "last", label: "Recent" },
];

const pick = <T extends string>(v: string | null, ok: readonly T[], d: T): T => (ok.includes(v as T) ? (v as T) : d);
const stored = (k: string) => { try { return localStorage.getItem(k); } catch { return null; } };
const store = (k: string, v: string) => { try { localStorage.setItem(k, v); } catch { /* private mode */ } };

export function Library() {
  const { admin } = useSession();
  const [params, setParams] = useSearchParams();
  const site = useData(() => (admin ? Promise.resolve(null) : endpoints.site()), [admin]);
  const tab = pick(params.get("tab"), ["artists", "albums", "tracks"] as const, "artists");
  const sort = pick(params.get("sort"), ["plays", "name", "last"] as const, "name");
  const [view, setViewState] = useState<View>(() => pick(stored("library.view"), ["grid", "list"] as const, "grid"));
  const setView = (v: View) => { setViewState(v); store("library.view", v); };
  const [q, setQ, query] = useSearchParam(params, setParams);
  const set = (patch: Record<string, string | null>) => setParams((p) => withParams(p, patch), { replace: true });

  if (!admin && site.loading) return null;
  if (!admin && site.data?.sections.library === false) return <Private title="Library" />;

  const grid = view === "grid" && tab !== "tracks";
  return (
    <div className="page lib">
      <AdminHead title="Library">
        <Segmented label="Show" value={tab} onChange={(v) => set({ tab: v === "artists" ? null : v })} options={TABS} />
      </AdminHead>
      <div className="lib-bar">
        <SearchField value={q} onChange={setQ} placeholder={tab === "artists" ? "Artist" : "Artist, title or album"} />
        <div className="lib-controls">
          <Segmented label="Sort" value={sort} onChange={(v) => set({ sort: v === "name" ? null : v })} options={SORTS} />
          {tab !== "tracks" && (
            <Segmented label="View" value={view} onChange={setView} options={[
              { value: "grid", label: <><LayoutGrid size={15} /><span className="sr-only">Grid</span></> },
              { value: "list", label: <><List size={15} /><span className="sr-only">List</span></> },
            ]} />
          )}
        </div>
      </div>
      <Browse key={`${tab}/${sort}/${query}`} kind={tab} sort={sort} query={query} grid={grid} />
    </div>
  );
}

// ---------- Shared with Scrobbles ----------

export function withParams(p: URLSearchParams, patch: Record<string, string | null>) {
  const n = new URLSearchParams(p);
  for (const [k, v] of Object.entries(patch)) v ? n.set(k, v) : n.delete(k);
  return n;
}

// The search text, debounced into ?q= so a reload or back keeps it.
export function useSearchParam(params: URLSearchParams, setParams: ReturnType<typeof useSearchParams>[1]) {
  const [q, setQ] = useState(params.get("q") ?? "");
  const [query, setQuery] = useState(q.trim());
  useEffect(() => { const t = window.setTimeout(() => setQuery(q.trim()), 250); return () => window.clearTimeout(t); }, [q]);
  useEffect(() => {
    if ((params.get("q") ?? "") !== query) setParams((p) => withParams(p, { q: query || null }), { replace: true });
  }, [query]); // eslint-disable-line react-hooks/exhaustive-deps
  return [q, setQ, query] as const;
}

export function SearchField({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <div className="lib-search">
      <Search size={15} className="faint" />
      <input className="input" type="search" aria-label="Search" placeholder={placeholder} value={value} onChange={(e) => onChange(e.target.value)} />
    </div>
  );
}

// Each credited artist links to its own page, joined as credited. Without
// credits (an unmatched scrobble) the plain text shows instead.
export function ArtistLinks({ artists, fallback }: { artists?: { id: number; name: string; join_phrase?: string }[]; fallback?: string }) {
  if (!artists?.length) return <>{fallback || "–"}</>;
  return <>{artists.map((a, i) => (
    <span key={`${a.id}-${i}`}>
      <Link to={`/artist/${a.id}`} onClick={(e) => e.stopPropagation()}>{a.name}</Link>
      {i < artists.length - 1 && (a.join_phrase || ", ")}
    </span>
  ))}</>;
}

// Calls more() when the returned element nears the viewport.
export function useInfinite(more: () => void, stop: boolean, busy: boolean) {
  const ref = useRef<HTMLDivElement>(null);
  const fn = useRef(more);
  fn.current = more;
  useEffect(() => {
    const el = ref.current;
    if (!el || stop || busy) return;
    const io = new IntersectionObserver((e) => { if (e[0].isIntersecting) fn.current(); }, { rootMargin: "600px" });
    io.observe(el);
    return () => io.disconnect();
  }, [stop, busy]);
  return ref;
}

export function Private({ title }: { title: string }) {
  return <div className="page lib"><AdminHead title={title} /><div className="card"><div className="empty">This is private.</div></div></div>;
}

// ---------- Artists, albums, tracks ----------

function useBrowse(kind: LibraryKind, sort: LibrarySort, query: string) {
  const [items, setItems] = useState<LibraryItem[]>([]);
  const [total, setTotal] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const busyRef = useRef(false);

  const more = useCallback(() => {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setFailed(false);
    endpoints.library(kind, sort, query, items.length).then((r) => {
      setItems((cur) => [...cur, ...r.items]);
      setTotal(r.total);
    }, () => setFailed(true)).finally(() => { busyRef.current = false; setBusy(false); });
  }, [kind, sort, query, items.length]);

  useEffect(() => { more(); }, []); // eslint-disable-line react-hooks/exhaustive-deps
  return { items, total, busy, failed, more, done: total !== null && items.length >= total };
}

function Browse({ kind, sort, query, grid }: { kind: LibraryKind; sort: LibrarySort; query: string; grid: boolean }) {
  const b = useBrowse(kind, sort, query);
  const { more, done, busy, failed } = b;
  const sentinel = useInfinite(more, done || failed, busy);

  const empty = b.total === 0 && <div className="empty">{query ? "Nothing matches." : `No ${kind} yet.`}</div>;
  const sub = (it: LibraryItem) => (sort === "last" ? fmtAgo(it.last_played) : `${fmtNum(it.plays)} ${it.plays === 1 ? "play" : "plays"}`);
  const href = (it: LibraryItem) => `/${kind === "artists" ? "artist" : kind === "albums" ? "album" : "track"}/${it.id}`;

  return (
    <>
      {b.total !== null && <p className="faint num lib-count">{fmtNum(b.total)} {kind}</p>}
      {grid ? (
        empty || (
          <div className="cover-grid lib-grid">
            {b.items.map((it) => (
              <Link key={it.id} to={href(it)} className="cover-card">
                <Cover name={it.name} src={it.image} size="100%" round={kind === "artists"} />
                <span className="list-title truncate">{it.name}</span>
                <span className="list-sub truncate num">{kind === "albums" && it.artist ? `${it.artist} · ${sub(it)}` : sub(it)}</span>
              </Link>
            ))}
          </div>
        )
      ) : (
        <div className="card">
          <BrowseTable kind={kind} items={b.items} href={href} />
          {empty}
        </div>
      )}
      {failed && <div className="empty">Couldn't load. <button className="btn" onClick={more}>Retry</button></div>}
      {!done && !failed && <div ref={sentinel} className="empty faint">{busy ? "Loading…" : ""}</div>}
    </>
  );
}

function BrowseTable({ kind, items, href }: { kind: LibraryKind; items: LibraryItem[]; href: (it: LibraryItem) => string }) {
  const nav = useNavigate();
  if (!items.length) return null;
  return (
    <table className="table lib-table fixed-table">
      <thead>
        <tr>
          <th style={{ width: 48 }} />
          <th>{kind === "artists" ? "Artist" : "Title"}</th>
          {kind !== "artists" && <th className="hide-sm" style={{ width: kind === "tracks" ? "24%" : "32%" }}>Artist</th>}
          {kind === "tracks" && <th className="hide-sm" style={{ width: "24%" }}>Album</th>}
          <th className="end" style={{ width: 72 }}>Plays</th>
          <th className="end hide-sm" style={{ width: 110 }}>Last played</th>
        </tr>
      </thead>
      <tbody>
        {items.map((it) => (
          <tr key={it.id} onClick={() => nav(href(it))} style={{ cursor: "pointer" }}>
            <td><Cover name={it.name} src={it.image} size={32} round={kind === "artists"} /></td>
            <td>
              <Link to={href(it)} className="list-title truncate" style={{ display: "block" }} title={it.name} onClick={(e) => e.stopPropagation()}>{it.name}</Link>
              {it.artist && <div className="list-sub show-sm truncate" title={it.artist}><ArtistLinks artists={it.artists} fallback={it.artist} /></div>}
            </td>
            {kind !== "artists" && <td className="muted hide-sm truncate" title={it.artist || undefined}><ArtistLinks artists={it.artists} fallback={it.artist} /></td>}
            {kind === "tracks" && (
              <td className="muted hide-sm truncate" title={it.album || undefined} onClick={(e) => e.stopPropagation()}>
                {it.album_id ? <Link to={`/album/${it.album_id}`}>{it.album}</Link> : "–"}
              </td>
            )}
            <td className="num end">{fmtNum(it.plays)}</td>
            <td className="faint num end hide-sm" title={fmtDate(it.last_played)}>{fmtAgo(it.last_played)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
