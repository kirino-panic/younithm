import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Pencil, Trash2 } from "lucide-react";
import { ApiError, endpoints, type Scrobble } from "../api";
import { fmtDate, fmtNum } from "../format";
import { useSession } from "../session";
import { Badge, Modal, Segmented, useData, useToast } from "../ui";
import { AdminHead } from "../admin/AdminLayout";
import { ScrobbleEditor } from "../admin/ScrobbleEditor";
import { ArtistLinks, Private, SearchField, useInfinite, useSearchParam } from "./Library";

// Every scrobble, newest first. The admin can edit, delete and filter to unmatched.

export function Scrobbles() {
  const { admin } = useSession();
  const [params, setParams] = useSearchParams();
  const site = useData(() => (admin ? Promise.resolve(null) : endpoints.site()), [admin]);
  const [q, setQ, query] = useSearchParam(params, setParams);
  if (!admin && site.loading) return null;
  if (!admin && site.data?.sections.scrobbles === false) return <Private title="Scrobbles" />;
  return <ScrobbleList q={q} setQ={setQ} query={query} admin={admin} />;
}

function useScrobbles(query: string) {
  const [items, setItems] = useState<Scrobble[]>([]);
  const [total, setTotal] = useState<number | null>(null);
  const [cursor, setCursor] = useState<string | undefined>();
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const gen = useRef(0);

  const load = useCallback((reset: boolean) => {
    const g = reset ? ++gen.current : gen.current;
    setBusy(true); setFailed(false);
    endpoints.scrobbles(query, reset ? undefined : cursor).then((r) => {
      if (g !== gen.current) return;
      setItems((cur) => (reset ? r.items : [...cur, ...r.items]));
      setTotal(r.total); setCursor(r.next_cursor); setDone(!r.next_cursor);
    }, () => g === gen.current && setFailed(true)).finally(() => g === gen.current && setBusy(false));
  }, [query, cursor]);

  useEffect(() => { load(true); }, [query]); // eslint-disable-line react-hooks/exhaustive-deps
  return { items, total, done, busy, failed, more: () => load(false), reload: () => load(true) };
}

function ScrobbleList({ q, setQ, query, admin }: { q: string; setQ: (v: string) => void; query: string; admin: boolean }) {
  const nav = useNavigate();
  const toast = useToast();
  const [onlyUnmatched, setOnlyUnmatched] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [timing, setTiming] = useState<Scrobble | null>(null);
  const [removing, setRemoving] = useState<Scrobble | null>(null);
  const [removingMany, setRemovingMany] = useState<number[] | null>(null);

  const { items, total, more, done, busy, failed, reload } = useScrobbles(query);
  const sentinel = useInfinite(more, done || failed, busy);
  const rows = items.filter((r) => !onlyUnmatched || !r.matched);
  const toggle = (id: number) => setSelected((s) => { const n = new Set(s); n.has(id) ? n.delete(id) : n.add(id); return n; });
  const onDeleted = (id: number) => {
    setSelected((s) => { const n = new Set(s); n.delete(id); return n; });
    reload();
  };
  const deleteNow = useDeleteScrobble(onDeleted);
  // Shift skips the confirmation.
  const askDelete = (r: Scrobble, e: React.MouseEvent) => {
    if (e.shiftKey) deleteNow(r).catch((err: Error) => toast({ text: err.message }));
    else setRemoving(r);
  };
  // The selected scrobbles go as one edit, so one Undo brings them all back.
  const deleteSelected = async (ids: number[]) => {
    try {
      const r = await endpoints.deleteScrobbles(ids);
      setSelected(new Set());
      reload();
      const n = ids.length;
      toast({ text: n === 1 ? "Scrobble deleted" : `${n} scrobbles deleted`, action: { label: "Undo", run: () => endpoints.undo(r.edit_id).then(reload).catch(() => toast({ text: "Couldn't undo." })) } });
    } catch (e) {
      throw new Error(e instanceof ApiError && e.message ? e.message : "Couldn't delete.");
    }
  };
  const askDeleteSelected = (e: React.MouseEvent) => {
    const ids = [...selected];
    if (e.shiftKey) deleteSelected(ids).catch((err: Error) => toast({ text: err.message }));
    else setRemovingMany(ids);
  };

  return (
    <div className="page lib">
      <AdminHead title="Scrobbles" />
      <div className="lib-bar">
        <SearchField value={q} onChange={setQ} placeholder="Artist, title or album" />
        {admin && (
          <div className="lib-controls">
            <Segmented label="Show" value={onlyUnmatched ? "unmatched" : "all"} onChange={(v) => setOnlyUnmatched(v === "unmatched")}
              options={[{ value: "all", label: "All" }, { value: "unmatched", label: "Unmatched" }]} />
          </div>
        )}
      </div>
      {total !== null && <p className="faint num lib-count">{fmtNum(total)} scrobbles</p>}
      <div className="card">
        <table className="table lib-table fixed-table">
          <thead><tr>
            {admin && <th style={{ width: 28 }} />}
            <th>Track</th>
            <th className="hide-sm" style={{ width: "22%" }}>Artist</th>
            <th className="hide-sm" style={{ width: "22%" }}>Album</th>
            {admin && <th className="hide-sm" style={{ width: 110 }}>Source</th>}
            <th className="end hide-sm" style={{ width: 150 }}>Played</th>
            {admin && <th style={{ width: 80 }} />}
          </tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.id} onClick={() => r.recording_id && nav(`/track/${r.recording_id}`)} style={{ cursor: r.recording_id ? "pointer" : undefined }}>
                {admin && <td onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label="Select" checked={selected.has(r.id)} onChange={() => toggle(r.id)} /></td>}
                <td>
                  <div className="cell-line">
                    <span className="list-title truncate" title={r.title}>
                      {r.recording_id ? <Link to={`/track/${r.recording_id}`} onClick={stop}>{r.title}</Link> : r.title}
                    </span>
                    {admin && !r.matched && <Badge tone="warning">Unmatched</Badge>}
                  </div>
                  <div className="list-sub show-sm num truncate" title={r.artist}><ArtistLinks artists={r.artists} fallback={r.artist} /> · {fmtDate(r.played_at)}</div>
                </td>
                <td className="muted hide-sm truncate" title={r.artist}><ArtistLinks artists={r.artists} fallback={r.artist} /></td>
                <td className="muted hide-sm truncate" title={r.album || undefined}>
                  {r.album
                    ? (r.release_id ? <Link to={`/album/${r.release_id}`} onClick={stop}>{r.album}</Link> : r.album)
                    : (admin ? <Badge tone="warning">No album</Badge> : "–")}
                </td>
                {admin && <td className="faint hide-sm truncate" title={r.source || undefined}>{r.source || "–"}</td>}
                <td className="faint num end hide-sm" title={fmtDate(r.played_at)}>{fmtDate(r.played_at)}</td>
                {admin && (
                  <td onClick={(e) => e.stopPropagation()} style={{ whiteSpace: "nowrap", textAlign: "right" }}>
                    <button className="btn btn-plain btn-icon" aria-label="Edit scrobble" onClick={() => setTiming(r)}><Pencil size={15} /></button>
                    <button className="btn btn-plain btn-icon btn-danger" aria-label="Delete scrobble" title="Delete (Shift-click skips the confirmation)" onClick={(e) => askDelete(r, e)}><Trash2 size={15} /></button>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
        {total !== null && !rows.length && done && <div className="empty">No scrobbles.</div>}
      </div>
      {failed && <div className="empty">Couldn't load. <button className="btn" onClick={more}>Retry</button></div>}
      {!done && !failed && <div ref={sentinel} className="empty faint">{busy ? "Loading…" : ""}</div>}

      {selected.size > 0 && (
        <div className="floatbar">
          <span style={{ alignSelf: "center", padding: "0 8px", fontSize: 13 }} className="num">{selected.size} selected</span>
          <button className="btn btn-primary">Edit…</button>
          <button className="btn">Move to album…</button>
          <button className="btn btn-danger" title="Delete (Shift-click skips the confirmation)" onClick={askDeleteSelected}>Delete</button>
          <button className="btn btn-plain" onClick={() => setSelected(new Set())}>Clear</button>
        </div>
      )}

      {timing && <ScrobbleEditor s={timing} onClose={() => setTiming(null)} onSaved={reload} />}
      {removing && <DeleteDialog count={1} onClose={() => setRemoving(null)} remove={() => deleteNow(removing)} />}
      {removingMany && <DeleteDialog count={removingMany.length} onClose={() => setRemovingMany(null)} remove={() => deleteSelected(removingMany)} />}
    </div>
  );
}

const stop = (e: React.MouseEvent) => e.stopPropagation();


// Deletes one scrobble and offers Undo. Rejects with a message if it fails.
function useDeleteScrobble(onDeleted: (id: number) => void) {
  const toast = useToast();
  return async (s: Scrobble) => {
    try {
      const r = await endpoints.deleteScrobble(s.id);
      onDeleted(s.id);
      toast({ text: "Scrobble deleted", action: { label: "Undo", run: () => endpoints.undo(r.edit_id).then(() => onDeleted(s.id)).catch(() => toast({ text: "Couldn't undo." })) } });
    } catch (e) {
      throw new Error(e instanceof ApiError && e.message ? e.message : "Couldn't delete.");
    }
  };
}

function DeleteDialog({ count, onClose, remove: doRemove }: { count: number; onClose: () => void; remove: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const remove = async () => {
    setBusy(true); setErr("");
    try {
      await doRemove();
      onClose();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Couldn't delete.");
      setBusy(false);
    }
  };

  return (
    <Modal onClose={onClose}>
      <h3 style={{ fontSize: 17 }}>{count === 1 ? "Delete this scrobble?" : `Delete ${count} scrobbles?`}</h3>
      <p className="muted" style={{ fontSize: 13 }}>Tip: Holding Shift while clicking delete skips this dialogue box.</p>
      {err && <p className="muted" role="alert" style={{ color: "var(--danger)", fontSize: 13 }}>{err}</p>}
      <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn btn-primary btn-danger" disabled={busy} onClick={remove}>Delete</button>
      </div>
    </Modal>
  );
}
