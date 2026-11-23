import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowDown, ArrowUp, ChevronRight, ExternalLink, Plus, RefreshCw, Star, Users, X } from "lucide-react";
import { endpoints, ApiError, withBase, type Credit, type CreditIn, type MergeCandidate } from "../api";
import { Autocomplete, Badge, Cover, InfoTip, Modal, Sheet, Switch, useToast } from "../ui";

type Kind = "recording" | "release" | "artist";
const LABELS: Record<Kind, string> = { recording: "track", release: "album", artist: "artist" };
const ROUTES: Record<Kind, string> = { recording: "/track/", release: "/album/", artist: "/artist/" };
const PROVIDERS = [["spotify", "Spotify"], ["apple", "Apple Music"], ["musicbrainz", "MusicBrainz"]] as const;
const PATHS: Record<string, Record<Kind, string>> = {
  musicbrainz: { artist: "https://musicbrainz.org/artist/", release: "https://musicbrainz.org/release/", recording: "https://musicbrainz.org/recording/" },
  spotify: { artist: "https://open.spotify.com/artist/", release: "https://open.spotify.com/album/", recording: "https://open.spotify.com/track/" },
  apple: { artist: "https://music.apple.com/artist/", release: "https://music.apple.com/album/", recording: "https://music.apple.com/song/" },
};

type CreditRow = { id?: number; name: string; join: string; credited: string };
type AlbumRow = { id?: number; title: string; primary: boolean; removed?: boolean };

const MAX_IMAGE = 8 << 20;

// Edits a track, album or artist: name, image, artists, albums and catalogue
// links. Nothing is sent until Save; each part is its own logged, undoable edit.
export function EditPanel({ kind, id, name, override, image, imageCustom = false, credits, releases, noAlbum = false, links, raw = [], onClose, onSaved }: {
  kind: Kind; id: number; name: string; override?: string | null; image?: string; imageCustom?: boolean; credits?: Credit[]; releases?: { id: number; title: string; primary: boolean }[]; noAlbum?: boolean;
  links: Record<string, string>; raw?: string[]; onClose: () => void; onSaved: () => void;
}) {
  const toast = useToast();
  const navigate = useNavigate();
  const [mergeInto, setMergeInto] = useState<MergeCandidate | null>(null);
  const [merging, setMerging] = useState(false);
  // Off: merge just what is here now; later listens under this name stay apart.
  const [remember, setRemember] = useState(true);
  const [splitting, setSplitting] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [akaOpen, setAkaOpen] = useState(false);
  // Credited names are rare: the section opens by itself only when one is set.
  const [creditedOpen, setCreditedOpen] = useState(() => (credits || []).some((c) => c.credited_name));
  const [adding, setAdding] = useState<"artist" | "album" | null>(null);
  // The display name set by hand; empty means the naming order decides.
  const [title, setTitle] = useState(override || "");
  const startCredits = (credits || []).map((c) => ({ id: c.id, name: c.name, join: c.join_phrase || "", credited: c.credited_name || "" }));
  const [artists, setArtists] = useState<CreditRow[]>(startCredits);
  const [albums, setAlbums] = useState<AlbumRow[]>((releases || []).map((r) => ({ id: r.id, title: r.title, primary: r.primary })));
  const [none, setNone] = useState(noAlbum); // "No album": on no album, and never put on one
  const [linkEdits, setLinkEdits] = useState<Record<string, string | null>>({}); // provider -> new URL ("" while typing), or null to remove
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  // Image: a new file to upload, or "default" to go back to the fetched one.
  const [newImage, setNewImage] = useState<File | "default" | null>(null);
  const [preview, setPreview] = useState<string>();
  const fileInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!(newImage instanceof File)) { setPreview(undefined); return; }
    const u = URL.createObjectURL(newImage);
    setPreview(u);
    return () => URL.revokeObjectURL(u);
  }, [newImage]);
  const custom = newImage instanceof File || (imageCustom && newImage !== "default");

  const searchArtists = useCallback((q: string) => endpoints.search("artist", q).then((r) => r.items), []);
  const searchAlbums = useCallback((q: string) => endpoints.search("release", q).then((r) => r.items), []);

  // Folds this entry into the picked one, then shows the one that's kept. The
  // merged-away page is replaced in history, so Back skips it (it's gone).
  const merge = async () => {
    if (!mergeInto) return;
    setBusy(true); setErr("");
    try {
      const res = await endpoints.merge(kind, id, mergeInto.id, !remember);
      onClose();
      navigate(ROUTES[kind] + mergeInto.id, { replace: true });
      toast({
        text: `Merged into ${mergeInto.name}`,
        action: {
          label: "Undo",
          run: () => endpoints.undo(res.edit_id)
            .then(() => navigate(ROUTES[kind] + id, { replace: true }))
            .catch((e) => toast({ text: e instanceof ApiError ? e.message : "Couldn't undo." })),
        },
      });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't merge.");
    } finally {
      setBusy(false);
    }
  };

  // Deletes this entry and every scrobble of it, then leaves its page (it's gone).
  const remove = async () => {
    try {
      const res = await endpoints.deleteEntity(kind, id);
      onClose();
      navigate("/library", { replace: true });
      toast({
        text: `Deleted ${override || name}`,
        action: {
          label: "Undo",
          run: () => endpoints.undo(res.edit_id)
            .then(() => navigate(ROUTES[kind] + id))
            .catch((e) => toast({ text: e instanceof ApiError ? e.message : "Couldn't undo." })),
        },
      });
    } catch (e) {
      throw new Error(e instanceof ApiError && e.message ? e.message : "Couldn't delete.");
    }
  };

  const creditsChanged = JSON.stringify(artists) !== JSON.stringify(startCredits);

  // Credits every track on this album to its artists, right away (one undo).
  const applyToTracks = async () => {
    setBusy(true); setErr("");
    try {
      const res = await endpoints.applyAlbumArtists(id);
      onSaved();
      const n = res.summary.match(/to (\d+) tracks/)?.[1];
      toast({
        text: n === "1" ? "Applied to 1 track" : `Applied to ${n ?? "the"} tracks`,
        action: {
          label: "Undo",
          run: () => endpoints.undo(res.edit_id).then(onSaved)
            .catch((e) => toast({ text: e instanceof ApiError ? e.message : "Couldn't undo." })),
        },
      });
    } catch (e) {
      if (e instanceof ApiError && e.code === "no_change") toast({ text: "Every track already has these artists" });
      else setErr(e instanceof ApiError ? e.message : "Couldn't apply.");
    } finally {
      setBusy(false);
    }
  };

  // Queues a fresh catalogue lookup; what it finds fills in over the next minute.
  const refresh = async () => {
    setErr("");
    try {
      await endpoints.refresh(kind, id);
      toast({ text: kind === "release" ? "Looking up the album's tracks again" : "Looking this track up again" });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't refresh.");
    }
  };
  const move = (i: number, d: number) => setArtists((a) => {
    const n = [...a];
    [n[i], n[i + d]] = [n[i + d], n[i]];
    return n;
  });

  const save = async () => {
    setBusy(true); setErr("");
    const done: string[] = [];
    try {
      if (title.trim() !== (override || "")) {
        await endpoints.editEntity(kind, id, { display_name: title.trim() || null });
        done.push("name");
      }
      if (newImage instanceof File) {
        await endpoints.setImage(kind, id, newImage);
        done.push("image");
      } else if (newImage === "default" && imageCustom) {
        await endpoints.resetImage(kind, id);
        done.push("image");
      }
      if (kind !== "artist" && credits && creditsChanged) {
        if (!artists.length) throw new ApiError(400, "bad_value", "Add at least one artist.");
        const body: CreditIn[] = artists.map((a) => ({ ...(a.id ? { artist_id: a.id } : { name: a.name }), join_phrase: a.join, credited_name: a.credited.trim() || undefined }));
        await endpoints.setCredits(kind, id, body);
        done.push("artists");
      }
      if (kind === "recording" && none !== noAlbum) {
        await endpoints.setNoAlbum(id, none);
        done.push("albums");
      }
      if (kind === "recording" && releases && !none) {
        const before = new Map(releases.map((r) => [r.id, r]));
        for (const a of albums) {
          if (a.id && a.removed && before.has(a.id)) await endpoints.removeRelease(id, a.id);
          else if (!a.removed && a.id && !before.has(a.id)) await endpoints.addRelease(id, { release_id: a.id });
          else if (!a.removed && !a.id) await endpoints.addRelease(id, { title: a.title });
        }
        const prim = albums.find((a) => a.primary && !a.removed && a.id);
        if (prim?.id && !before.get(prim.id)?.primary) await endpoints.setPrimaryRelease(id, prim.id);
        if (albums.some((a) => a.removed || !a.id || !before.has(a.id)) || (prim && !before.get(prim.id!)?.primary)) done.push("albums");
      }
      for (const [p, v] of Object.entries(linkEdits)) {
        if (v === null) await endpoints.removeLink(kind, id, p);
        else if (v.trim()) await endpoints.setLink(kind, id, v.trim());
        else continue;
        done.push("links");
      }
      if (done.length) toast({ text: "Saved" });
      onSaved();
      onClose();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't save.");
      if (done.length) onSaved();
    } finally {
      setBusy(false);
    }
  };

  // Alias keys are "artist<US>title" match keys; show the ones that differ from
  // the current name, readably.
  const current = [(credits || []).map((c) => (c.credited_name || c.name) + (c.join_phrase || "")).join(""), name].filter(Boolean).join("\x1f").toLowerCase();
  // This entry as the merge summary shows it: what tells it apart from a look-alike.
  const creditLine = (credits || []).map((c, i, all) => (c.credited_name || c.name) + (i < all.length - 1 ? c.join_phrase || ", " : "")).join("");
  const primaryAlbum = (releases || []).find((r) => r.primary) || (releases || [])[0];
  const self: PickEntry = {
    id, name: override || name, image,
    sub: [creditLine, kind === "recording" ? primaryAlbum?.title : ""].filter(Boolean).join(" · ") || undefined,
  };
  const aka = raw.filter((r) => r.toLowerCase() !== current && r.toLowerCase() !== name.toLowerCase()).map((r) => r.split("\x1f").join(" – "));

  return (
    <Sheet wide title={`Edit ${LABELS[kind]}`} onClose={onClose}
      footer={<>
        <span className="foot-start" style={{ display: "flex", gap: 4 }}>
          <button className="btn btn-plain" onClick={() => setMerging(true)}>Merge…</button>
          {kind === "artist" && <button className="btn btn-plain" onClick={() => setSplitting(true)}>Split…</button>}
          <button className="btn btn-plain btn-danger" title="Delete (Shift-click skips the confirmation)"
            onClick={(e) => (e.shiftKey ? remove().catch((err: Error) => setErr(err.message)) : setDeleting(true))}>Delete</button>
        </span>
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn btn-primary" disabled={busy} onClick={save}>Save</button>
      </>}>
      <section>
        <div className="field">
          <span className="field-label" style={{ display: "flex", alignItems: "center", gap: 4 }}>
            Display name
            <InfoTip label="About display name" text="Manually override a display name for this item. Leave blank to use the defaults." />
          </span>
          <input className="input" aria-label="Display name" value={title} placeholder={override ? "Default name" : name}
            onChange={(e) => setTitle(e.target.value)} />
        </div>
      </section>

      {kind !== "recording" && (
        <section>
          <div className="section-label">Image</div>
          <div className="edit-row" style={{ gap: 12 }}>
            <Cover name={override || name} src={preview ?? (newImage === "default" ? undefined : image)} size={56} round={kind === "artist"} />
            <span className="grow muted" style={{ fontSize: 13 }}>
              {newImage === "default" ? "Found automatically, when there is one" : custom ? "Chosen by you" : image ? "Found automatically" : "None yet"}
            </span>
            {custom && <button className="btn btn-plain" onClick={() => setNewImage(imageCustom ? "default" : null)}>Use default</button>}
            {newImage === "default" && <button className="btn btn-plain" onClick={() => setNewImage(null)}>Keep</button>}
            <button className="btn" onClick={() => fileInput.current?.click()}>Choose…</button>
          </div>
          <input ref={fileInput} type="file" hidden accept="image/jpeg,image/png,image/webp,image/gif"
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (!f) return;
              if (f.size > MAX_IMAGE) { setErr("Choose an image up to 8 MB."); return; }
              setErr("");
              setNewImage(f);
            }} />
        </section>
      )}

      {kind !== "artist" && credits && (
        <section>
          <div className="section-label">Artists</div>
          <div className="vlist" style={{ gap: 4 }}>
            {artists.map((a, i) => (
              <div key={`${a.id ?? "new"}-${a.name}`} className="edit-row">
                <span className="grow truncate">{a.name}{!a.id && <> <Badge>New</Badge></>}</span>
                {i < artists.length - 1 && (
                  <input className="input" style={{ width: 64 }} aria-label={`Between ${a.name} and the next artist`} placeholder=", "
                    value={a.join} onChange={(e) => setArtists((x) => x.map((r, j) => (j === i ? { ...r, join: e.target.value } : r)))} />
                )}
                {artists.length > 1 && <>
                  <button className="btn btn-icon btn-plain" aria-label="Move up" disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={15} /></button>
                  <button className="btn btn-icon btn-plain" aria-label="Move down" disabled={i === artists.length - 1} onClick={() => move(i, 1)}><ArrowDown size={15} /></button>
                </>}
                <button className="btn btn-icon btn-plain" aria-label={`Remove ${a.name}`} onClick={() => setArtists((x) => x.filter((_, j) => j !== i))}><X size={15} /></button>
              </div>
            ))}
            {adding === "artist" ? (
              <Autocomplete autoFocus label="Add an artist" placeholder="Artist name" search={searchArtists} allowNew
                newLabel={(q) => `Add “${q}” as a new artist`}
                onPick={(v) => {
                  setAdding(null);
                  if (v.id && artists.some((a) => a.id === v.id)) return;
                  setArtists((x) => [...x.map((r, j) => (j === x.length - 1 && !r.join ? { ...r, join: ", " } : r)), { id: v.id, name: v.name, join: "", credited: "" }]);
                }} />
            ) : (
              <button className="btn edit-add" onClick={() => setAdding("artist")}><Plus size={15} /> Add artist</button>
            )}
            {kind === "release" && startCredits.length > 0 && (
              <div className="edit-row edit-apply">
                <span className="grow muted" style={{ fontSize: 13 }}>{creditsChanged ? "Save first to apply these artists" : "Credit every track to these artists"}</span>
                <button className="btn" disabled={busy || creditsChanged} onClick={applyToTracks}><Users size={15} /> Apply to tracks</button>
              </div>
            )}
          </div>
        </section>
      )}

      {kind !== "artist" && credits && artists.length > 0 && (
        <section>
          <button className="btn btn-plain" style={{ width: "100%", justifyContent: "space-between", padding: 0 }} onClick={() => setCreditedOpen((o) => !o)} aria-expanded={creditedOpen}>
            <span className="section-label" style={{ margin: 0, display: "flex", alignItems: "center", gap: 4 }}>
              Credited as
              <InfoTip label="About credited as" text="The name an artist goes by on this release, when it differs from their own. Leave blank to use the artist's name." />
            </span>
            <ChevronRight size={16} style={{ transform: creditedOpen ? "rotate(90deg)" : undefined, transition: "transform var(--dur)" }} />
          </button>
          {creditedOpen && (
            <div className="vlist" style={{ gap: 4, marginTop: 8 }}>
              {artists.map((a, i) => (
                <div key={`${a.id ?? "new"}-${a.name}`} className="edit-row">
                  <span className="grow truncate">{a.name}</span>
                  <input className="input" style={{ flex: "1 1 0", minWidth: 0, width: 0 }} aria-label={`${a.name} credited as`} placeholder={a.name}
                    value={a.credited} onChange={(e) => setArtists((x) => x.map((r, j) => (j === i ? { ...r, credited: e.target.value } : r)))} />
                </div>
              ))}
            </div>
          )}
        </section>
      )}

      {kind === "recording" && releases && (
        <section>
          <div className="section-label">Albums</div>
          <div className="vlist" style={{ gap: 4 }}>
            {!none && albums.map((a, i) => (
              <div key={`${a.id ?? "new"}-${a.title}`} className={`edit-row${a.removed ? " removed" : ""}`}>
                <span className="grow truncate">{a.title}{!a.id && <> <Badge>New</Badge></>}</span>
                {a.primary && !a.removed ? <Badge tone="accent">Primary</Badge> : !a.removed && a.id && (
                  <button className="btn btn-icon btn-plain" aria-label={`Make ${a.title} the primary album`}
                    onClick={() => setAlbums((x) => x.map((r, j) => ({ ...r, primary: j === i })))}><Star size={15} /></button>
                )}
                {a.removed
                  ? <button className="btn btn-plain" onClick={() => setAlbums((x) => x.map((r, j) => (j === i ? { ...r, removed: false } : r)))}>Keep</button>
                  : <button className="btn btn-icon btn-plain" aria-label={`Remove from ${a.title}`}
                      onClick={() => setAlbums((x) => (a.id ? x.map((r, j) => (j === i ? { ...r, removed: true } : r)) : x.filter((_, j) => j !== i)))}><X size={15} /></button>}
              </div>
            ))}
            {none ? null : adding === "album" ? (
              <Autocomplete autoFocus label="Add to an album" placeholder="Album title" search={searchAlbums} allowNew
                newLabel={(q) => `Create album “${q}”`}
                onPick={(v) => {
                  setAdding(null);
                  if (v.id && albums.some((a) => a.id === v.id && !a.removed)) return;
                  setAlbums((x) => [...x, { id: v.id, title: v.name, primary: !x.some((r) => !r.removed) }]);
                }} />
            ) : (
              <button className="btn edit-add" onClick={() => setAdding("album")}><Plus size={15} /> Add to album</button>
            )}
            <div className="edit-row">
              <span className="grow" style={{ fontSize: 13 }}>This track isn't on an album</span>
              <Switch checked={none} onChange={setNone} label="This track isn't on an album" />
            </div>
          </div>
        </section>
      )}

      <section>
        <div className="section-label section-label-row">
          <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
            Links
            {kind !== "artist" && <InfoTip label="About refresh" text={kind === "release"
              ? "Refresh looks every track on this album up in the linked catalogues again and fills in what's missing, like lengths."
              : "Refresh looks this track up in the linked catalogues again and fills in what's missing, like its length."} />}
          </span>
          {kind !== "artist" && <button className="btn btn-plain" style={{ margin: "-6px 0" }} onClick={refresh}><RefreshCw size={15} /> Refresh</button>}
        </div>
        <div className="vlist" style={{ gap: 4 }}>
          {PROVIDERS.map(([p, label]) => {
            const cur = links[p];
            const edit = linkEdits[p];
            const removed = cur && edit !== undefined;
            return (
              <div key={p} className="vlist" style={{ gap: 6 }}>
                <div className={`edit-row${removed ? " removed" : ""}`}>
                  <span className="grow">{label}</span>
                  {cur && !removed && (
                    <a className="badge" href={PATHS[p][kind] + cur} target="_blank" rel="noreferrer noopener">{cur.slice(0, 10)} <ExternalLink size={11} /></a>
                  )}
                  {cur
                    ? (removed
                      ? <button className="btn btn-plain" onClick={() => setLinkEdits(({ [p]: _, ...rest }) => rest)}>Keep</button>
                      : <button className="btn btn-icon btn-plain" aria-label={`Remove ${label} link`} onClick={() => setLinkEdits((x) => ({ ...x, [p]: null }))}><X size={15} /></button>)
                    : edit === undefined && <button className="btn btn-plain" onClick={() => setLinkEdits((x) => ({ ...x, [p]: "" }))}>Add</button>}
                </div>
                {edit !== undefined && (
                  <input className="input" autoFocus placeholder={cur ? "Paste a replacement, or leave empty" : `Paste a ${label} link`} aria-label={`${label} link`}
                    value={edit || ""} onChange={(e) => setLinkEdits((x) => ({ ...x, [p]: cur && !e.target.value ? null : e.target.value }))} />
                )}
              </div>
            );
          })}
        </div>
      </section>

      {aka.length > 0 && (
        <section>
          <button className="btn btn-plain" style={{ width: "100%", justifyContent: "space-between", padding: 0 }} onClick={() => setAkaOpen((o) => !o)} aria-expanded={akaOpen}>
            <span className="section-label" style={{ margin: 0 }}>Also known as</span>
            <ChevronRight size={16} style={{ transform: akaOpen ? "rotate(90deg)" : undefined, transition: "transform var(--dur)" }} />
          </button>
          {akaOpen && <div className="muted vlist" style={{ fontSize: 13, gap: 4, marginTop: 8, overflowWrap: "anywhere" }}>{aka.map((r) => <span key={r}>{r}</span>)}</div>}
        </section>
      )}
      {err && <p className="error-text">{err}</p>}

      {merging && (
        <PickDialog kind={kind} title={`Merge ${LABELS[kind]}`} busy={busy} err={err}
          load={(q) => endpoints.mergeCandidates(kind, id, q)} action="Merge" danger
          summary={(c) => <>Merge <PickChip kind={kind} entry={self} /> into <PickChip kind={kind} entry={c} /></>}
          onCancel={() => { setMerging(false); setMergeInto(null); setErr(""); setRemember(true); }}
          selected={mergeInto} onSelect={setMergeInto} onConfirm={merge}
          options={
            <div className="edit-row">
              <span className="grow" style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 13 }}>
                Apply changes to all further scrobbles
                <InfoTip label="About applying to further scrobbles" text="Turn this off to only apply this change as a one-off." />
              </span>
              <Switch checked={remember} onChange={setRemember} label="Apply changes to all further scrobbles" />
            </div>
          } />
      )}
      {deleting && <DeleteEntityDialog name={override || name} onCancel={() => setDeleting(false)} remove={remove} />}
      {splitting && <SplitDialog id={id} name={override || name} onCancel={() => setSplitting(false)} onDone={onClose} />}
    </Sheet>
  );
}

// Picks one entry from likely duplicates, then a search over every name entries
// go by. Rows carry what tells look-alikes apart. Used to merge an entry and to
// re-link a single scrobble.
export function PickDialog({ kind, title, intro, load, initialQuery = "", summary, action, danger, busy, err, selected, onSelect, onCancel, onConfirm, options }: {
  kind: Kind; title: string; intro?: string; load: (q: string) => Promise<{ items: MergeCandidate[] }>; initialQuery?: string;
  summary: (picked: MergeCandidate) => ReactNode; action: string; danger?: boolean; busy: boolean; err: string; selected: MergeCandidate | null;
  onSelect: (c: MergeCandidate | null) => void; onCancel: () => void; onConfirm: () => void;
  options?: ReactNode; // shown above the buttons
}) {
  const label = LABELS[kind];
  const [q, setQ] = useState(initialQuery);
  const [items, setItems] = useState<MergeCandidate[] | null>(null);
  const loadRef = useRef(load);
  loadRef.current = load;
  useEffect(() => {
    let live = true;
    setItems(null);
    const t = window.setTimeout(() => {
      loadRef.current(q.trim()).then((r) => live && setItems(r.items), () => live && setItems([]));
    }, q.trim() ? 200 : 0);
    return () => { live = false; window.clearTimeout(t); };
  }, [q]);
  const list = items || [];
  const at = list.findIndex((c) => c.id === selected?.id);
  const move = (d: number) => {
    if (!list.length) return;
    const n = list[Math.min(Math.max(at + d, 0), list.length - 1)];
    onSelect(n);
    document.getElementById(`pick-${n.id}`)?.scrollIntoView({ block: "nearest" });
  };
  return (
    <Modal wide onClose={onCancel}>
      <div className="vlist" style={{ gap: 4 }}>
        <h3 style={{ fontSize: 17 }}>{title}</h3>
        {intro && <p className="muted" style={{ fontSize: 13 }}>{intro}</p>}
      </div>
      <input className="input" autoFocus placeholder="Search" aria-label={`Search ${label}s`} value={q}
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") { e.preventDefault(); move(1); }
          else if (e.key === "ArrowUp") { e.preventDefault(); move(-1); }
        }} />
      <div className="section-label" style={{ marginBottom: -8 }}>{q.trim() ? "Results" : "Possible duplicates"}</div>
      <div className="merge-list" role="listbox" aria-label={q.trim() ? "Results" : "Possible duplicates"}>
        {items === null ? <p className="muted merge-empty">Looking…</p>
          : !list.length ? <p className="muted merge-empty">{q.trim() ? `No ${label}s match.` : "No flagged duplicates"}</p>
          : list.map((c) => (
            <button key={c.id} id={`pick-${c.id}`} type="button" role="option" aria-selected={c.id === selected?.id}
              className="merge-row" onClick={() => onSelect(c)}>
              <Cover name={c.name} src={c.image} size={40} round={kind === "artist"} />
              <span className="grow vlist" style={{ minWidth: 0 }}>
                <span className="truncate">{c.name}</span>
                {c.sub && <span className="muted truncate" style={{ fontSize: 12 }}>{c.sub}</span>}
              </span>
              {c.reason && <Badge tone={c.reason === "Same catalogue entry" || c.reason === "Same release group" ? "accent" : undefined}>{c.reason}</Badge>}
              <span className="muted" style={{ fontSize: 12, whiteSpace: "nowrap" }}>{c.plays} {c.plays === 1 ? "play" : "plays"}</span>
            </button>
          ))}
      </div>
      {options}
      {err && <p className="error-text">{err}</p>}
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <span className="pick-summary muted">
          {selected && summary(selected)}
        </span>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <button className={`btn btn-primary${danger ? " btn-danger" : ""}`} disabled={!selected || busy} onClick={onConfirm}>{action}</button>
      </div>
    </Modal>
  );
}

// Splits an artist whose name is really several ("Danny Brown & Underscores")
// into those artists. Every track and album credited to it is credited to them
// instead, and later listens credited that way get them too.
function SplitDialog({ id, name, onCancel, onDone }: { id: number; name: string; onCancel: () => void; onDone: () => void }) {
  const toast = useToast();
  const navigate = useNavigate();
  const [rows, setRows] = useState<CreditRow[]>([]);
  const [adding, setAdding] = useState(true);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const searchArtists = useCallback((q: string) => endpoints.search("artist", q).then((r) => r.items.filter((a) => a.id !== id)), [id]);
  const move = (i: number, d: number) => setRows((a) => {
    const n = [...a];
    [n[i], n[i + d]] = [n[i + d], n[i]];
    return n;
  });

  const split = async () => {
    setBusy(true); setErr("");
    try {
      const body: CreditIn[] = rows.map((a) => ({ ...(a.id ? { artist_id: a.id } : { name: a.name }), join_phrase: a.join }));
      const res = await endpoints.splitArtist(id, body);
      onDone();
      // This artist is gone: show the first of its parts instead (looked up
      // by name when it was just created).
      let to = rows[0].id;
      if (!to) to = (await endpoints.search("artist", rows[0].name).catch(() => ({ items: [] }))).items.find((a) => a.name === rows[0].name)?.id;
      navigate(to ? `/artist/${to}` : "/library", { replace: true });
      toast({
        text: `Split ${name}`,
        action: {
          label: "Undo",
          run: () => endpoints.undo(res.edit_id)
            .then(() => navigate(`/artist/${id}`, { replace: true }))
            .catch((e) => toast({ text: e instanceof ApiError ? e.message : "Couldn't undo." })),
        },
      });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't split.");
      setBusy(false);
    }
  };

  return (
    <Modal wide onClose={onCancel}>
      <div className="vlist" style={{ gap: 4 }}>
        <h3 style={{ fontSize: 17 }}>Split artist</h3>
        <p className="muted" style={{ fontSize: 13 }}>Select which artists you'd like to split this artist into.</p>
      </div>
      <div className="vlist" style={{ gap: 4 }}>
        {rows.map((a, i) => (
          <div key={`${a.id ?? "new"}-${a.name}`} className="edit-row">
            <span className="grow truncate">{a.name}{!a.id && <> <Badge>New</Badge></>}</span>
            {i < rows.length - 1 && (
              <input className="input" style={{ width: 64 }} aria-label={`Between ${a.name} and the next artist`} placeholder=", "
                value={a.join} onChange={(e) => setRows((x) => x.map((r, j) => (j === i ? { ...r, join: e.target.value } : r)))} />
            )}
            {rows.length > 1 && <>
              <button className="btn btn-icon btn-plain" aria-label="Move up" disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={15} /></button>
              <button className="btn btn-icon btn-plain" aria-label="Move down" disabled={i === rows.length - 1} onClick={() => move(i, 1)}><ArrowDown size={15} /></button>
            </>}
            <button className="btn btn-icon btn-plain" aria-label={`Remove ${a.name}`} onClick={() => setRows((x) => x.filter((_, j) => j !== i))}><X size={15} /></button>
          </div>
        ))}
        {adding ? (
          <Autocomplete autoFocus label="Add an artist" placeholder="Artist name" search={searchArtists} allowNew
            newLabel={(q) => `Add “${q}” as a new artist`}
            onPick={(v) => {
              setAdding(rows.length < 1); // keep the field open until there are two
              if (v.id && rows.some((a) => a.id === v.id)) return;
              if (!v.id && v.name.trim().toLowerCase() === name.trim().toLowerCase()) return;
              setRows((x) => [...x.map((r, j) => (j === x.length - 1 && !r.join ? { ...r, join: " & " } : r)), { id: v.id, name: v.name, join: "", credited: "" }]);
            }} />
        ) : (
          <button className="btn edit-add" onClick={() => setAdding(true)}><Plus size={15} /> Add artist</button>
        )}
      </div>
      {err && <p className="error-text">{err}</p>}
      <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <button className="btn btn-primary btn-danger" disabled={rows.length < 2 || busy} onClick={split}>Split</button>
      </div>
    </Modal>
  );
}

function DeleteEntityDialog({ name, onCancel, remove }: { name: string; onCancel: () => void; remove: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const go = async () => {
    setBusy(true); setErr("");
    try {
      await remove();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Couldn't delete.");
      setBusy(false);
    }
  };
  return (
    <Modal onClose={onCancel}>
      <h3 style={{ fontSize: 17 }}>Delete {name}?</h3>
      <p className="muted" style={{ fontSize: 13 }}>All scrobbles associated with {name} will be deleted.</p>
      <p className="muted" style={{ fontSize: 13 }}>Tip: Holding Shift while clicking delete skips this dialogue box.</p>
      {err && <p className="error-text" role="alert">{err}</p>}
      <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <button className="btn btn-primary btn-danger" disabled={busy} onClick={go}>Delete</button>
      </div>
    </Modal>
  );
}

export type PickEntry = { id: number; name: string; sub?: string; image?: string; plays?: number };

// A name in a dialog's summary line: it opens the entry in a new tab (so the
// dialog stays as it is), and hovering it shows what tells it apart.
export function PickChip({ kind, entry }: { kind: Kind; entry: PickEntry }) {
  return (
    <span className="pick-chip">
      <a className="pick-chip-name" href={withBase(ROUTES[kind] + entry.id)} target="_blank" rel="noopener">{entry.name}</a>
      <span className="pick-pop" role="tooltip">
        <Cover name={entry.name} src={entry.image} size={40} round={kind === "artist"} />
        <span className="vlist" style={{ minWidth: 0 }}>
          <span className="pick-pop-title">{entry.name}</span>
          {entry.sub && <span className="muted">{entry.sub}</span>}
          {entry.plays !== undefined && <span className="faint">{entry.plays} {entry.plays === 1 ? "play" : "plays"}</span>}
        </span>
      </span>
    </span>
  );
}
