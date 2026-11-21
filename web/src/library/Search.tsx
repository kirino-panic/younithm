import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Search as SearchIcon } from "lucide-react";
import { endpoints, type LibraryItem, type LibraryKind } from "../api";
import { fmtNum } from "../format";
import { useSession } from "../session";
import { Cover, useData } from "../ui";
import { AdminHead } from "../admin/AdminLayout";
import { Private, useSearchParam } from "./Library";

// Searches artists, albums and tracks at once. Results are the library's, best
// played first; each section links on to the full list in the library.

const KINDS: { kind: LibraryKind; label: string; path: string }[] = [
  { kind: "artists", label: "Artists", path: "artist" },
  { kind: "albums", label: "Albums", path: "album" },
  { kind: "tracks", label: "Tracks", path: "track" },
];
const PER_KIND = 8;

type Results = Record<LibraryKind, { items: LibraryItem[]; total: number }>;

export function SearchPage() {
  const { admin } = useSession();
  const site = useData(() => (admin ? Promise.resolve(null) : endpoints.site()), [admin]);
  if (!admin && site.loading) return null;
  // Search reads the library, so it is private when the library is.
  if (!admin && site.data?.sections.library === false) return <Private title="Search" />;
  return <Search />;
}

function Search() {
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const [q, setQ, query] = useSearchParam(params, setParams);
  const [res, setRes] = useState<{ q: string; r: Results } | null>(null);
  const [failed, setFailed] = useState(false);
  const [sel, setSel] = useState(-1); // -1: nothing picked with the arrow keys yet
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!query) { setRes(null); setFailed(false); return; }
    let live = true;
    Promise.all(KINDS.map((k) => endpoints.librarySearch(k.kind, query, PER_KIND))).then(
      ([artists, albums, tracks]) => { if (live) { setRes({ q: query, r: { artists, albums, tracks } }); setFailed(false); setSel(-1); } },
      () => live && setFailed(true),
    );
    return () => { live = false; };
  }, [query]);

  // Every result in display order, for the arrow keys.
  const flat = useMemo(() => (res ? KINDS.flatMap((k) => res.r[k.kind].items.map((it) => `/${k.path}/${it.id}`)) : []), [res]);

  useEffect(() => {
    if (sel >= 0) list.current?.querySelector<HTMLElement>(`[data-idx="${sel}"]`)?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  const onKey = (e: React.KeyboardEvent) => {
    if (!flat.length) return;
    if (e.key === "ArrowDown") { e.preventDefault(); setSel((s) => (s + 1) % flat.length); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setSel((s) => (s <= 0 ? flat.length - 1 : s - 1)); }
    else if (e.key === "Enter") { e.preventDefault(); nav(flat[Math.max(sel, 0)]); }
  };

  const empty = res && KINDS.every((k) => !res.r[k.kind].items.length);
  let idx = 0;
  return (
    <div className="page lib">
      <AdminHead title="Search" />
      <div className="search-field">
        <SearchIcon size={18} className="faint" />
        <input ref={input} autoFocus className="input" type="search" aria-label="Search" placeholder="Artists, albums and tracks"
          value={q} onChange={(e) => setQ(e.target.value)} onKeyDown={onKey}
          role="combobox" aria-expanded={!!flat.length} aria-controls="search-results" aria-activedescendant={sel >= 0 ? `search-${sel}` : undefined} />
      </div>
      <div id="search-results" role="listbox" ref={list} className="search-results">
        {failed ? <div className="empty">Couldn't search.</div>
          : empty ? <div className="empty">Nothing matches.</div>
          : res && KINDS.map((k) => {
            const { items, total } = res.r[k.kind];
            if (!items.length) return null;
            return (
              <section key={k.kind} className="card search-section">
                <div className="card-head">
                  <h3>{k.label}</h3>
                  {total > items.length && (
                    <Link to={`/library?${new URLSearchParams({ ...(k.kind !== "artists" && { tab: k.kind }), q: res.q })}`} className="search-all">
                      See all {fmtNum(total)}
                    </Link>
                  )}
                </div>
                <div className="list">
                  {items.map((it) => {
                    const i = idx++;
                    return (
                      <Link key={it.id} id={`search-${i}`} data-idx={i} role="option" aria-selected={i === sel}
                        to={`/${k.path}/${it.id}`} className={`list-row search-row${i === sel ? " selected" : ""}`}>
                        <Cover name={it.name} src={it.image} size={36} round={k.kind === "artists"} />
                        <span className="list-main">
                          <span className="list-title truncate">{it.name}</span>
                          {it.artist && <span className="list-sub truncate">{it.artist}{k.kind === "tracks" && it.album ? ` · ${it.album}` : ""}</span>}
                        </span>
                        <span className="list-end num">{fmtNum(it.plays)} {it.plays === 1 ? "play" : "plays"}</span>
                      </Link>
                    );
                  })}
                </div>
              </section>
            );
          })}
      </div>
    </div>
  );
}
