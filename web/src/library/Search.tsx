import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { endpoints, type LibraryItem, type LibraryKind } from "../api";
import { fmtNum } from "../format";
import { useSession } from "../session";
import { useData } from "../ui";
import { AdminHead } from "../admin/AdminLayout";
import { BrowseTable, CoverGrid, Private, SearchField, libraryHref, storedLibraryView, useSearchParam } from "./Library";

// Searches artists, albums and tracks at once, laid out as the Library is.
// Results are the library's, best played first; each kind links on to its
// full list in the library.

const KINDS: { kind: LibraryKind; label: string }[] = [
  { kind: "artists", label: "Artists" }, { kind: "albums", label: "Albums" }, { kind: "tracks", label: "Tracks" },
];
const PER_KIND = 12;

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
  const grid = storedLibraryView() === "grid";

  useEffect(() => {
    if (!query) { setRes(null); setFailed(false); return; }
    let live = true;
    Promise.all(KINDS.map((k) => endpoints.librarySearch(k.kind, query, PER_KIND))).then(
      ([artists, albums, tracks]) => { if (live) { setRes({ q: query, r: { artists, albums, tracks } }); setFailed(false); } },
      () => live && setFailed(true),
    );
    return () => { live = false; };
  }, [query]);

  // Enter opens the first result.
  const first = res && KINDS.map((k) => res.r[k.kind].items[0] && libraryHref(k.kind)(res.r[k.kind].items[0])).find(Boolean);
  const onKey = (e: React.KeyboardEvent) => { if (e.key === "Enter" && first) nav(first); };

  const empty = res && KINDS.every((k) => !res.r[k.kind].items.length);
  return (
    <div className="page lib search-page">
      <AdminHead title="Search" />
      <div className="lib-bar" onKeyDown={onKey}>
        <SearchField value={q} onChange={setQ} placeholder="Artist, album or track" autoFocus />
      </div>
      {failed ? <div className="card"><div className="empty">Couldn't search.</div></div>
        : empty ? <div className="card"><div className="empty">Nothing matches.</div></div>
        : res && KINDS.map(({ kind, label }) => {
          const { items, total } = res.r[kind];
          if (!items.length) return null;
          return (
            <section key={kind} className="lib search-section">
              <div className="search-head">
                <h2>{label}</h2>
                {total > items.length && (
                  <Link to={`/library?${new URLSearchParams({ ...(kind !== "artists" && { tab: kind }), q: res.q })}`} className="faint num lib-count search-all">
                    See all {fmtNum(total)}
                  </Link>
                )}
              </div>
              {grid && kind !== "tracks"
                ? <CoverGrid kind={kind} items={items} />
                : <div className="card"><BrowseTable kind={kind} items={items} href={libraryHref(kind)} /></div>}
            </section>
          );
        })}
    </div>
  );
}
