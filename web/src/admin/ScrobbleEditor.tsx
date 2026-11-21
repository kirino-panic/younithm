import { useCallback, useState } from "react";
import { X } from "lucide-react";
import { ApiError, endpoints, type Scrobble, type ScrobbleEditBody } from "../api";
import { Autocomplete, Badge, Sheet, useData, useToast } from "../ui";
import { PickDialog } from "./EditPanel";

type Artist = { id?: number; name: string };
type Album = { kind: "keep" } | { kind: "none" } | { kind: "existing"; id: number; name: string } | { kind: "new"; title: string };

// datetime-local works in local time; the API stores UTC.
const toLocalInput = (iso: string) => {
  const d = new Date(iso);
  return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
};

// Loads the scrobble's track first so the artist field starts with its real artists.
export function ScrobbleEditor(props: { s: Scrobble; onClose: () => void; onSaved: () => void }) {
  const { s } = props;
  const rec = useData(() => (s.recording_id ? endpoints.recording(String(s.recording_id)) : Promise.resolve(null)), [s.recording_id]);
  if (rec.loading) return null;
  const start = rec.data
    ? { title: rec.data.title, artists: rec.data.artists.map((a) => ({ id: a.id, name: a.name })) }
    : { title: s.title, artists: s.artist ? [{ name: s.artist }] : [] };
  return <Form {...props} start={start} />;
}

// Changes what one scrobble counts for (track, artists, album) and when it was
// played. Only this scrobble changes, and what was originally submitted is kept.
function Form({ s, start, onClose, onSaved }: { s: Scrobble; start: { title: string; artists: Artist[] }; onClose: () => void; onSaved: () => void }) {
  const toast = useToast();
  const startTime = toLocalInput(s.played_at);
  const [title, setTitle] = useState(start.title);
  const [artists, setArtists] = useState<Artist[]>(start.artists);
  const [album, setAlbum] = useState<Album>({ kind: "keep" });
  const [time, setTime] = useState(startTime);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [relinking, setRelinking] = useState(false);
  const [relinkTo, setRelinkTo] = useState<{ id: number; name: string } | null>(null);

  const searchArtists = useCallback((q: string) => endpoints.search("artist", q).then((r) => r.items), []);
  const searchAlbums = useCallback((q: string) => endpoints.search("release", q).then((r) => r.items), []);

  const key = (a: Artist[]) => JSON.stringify(a.map((x) => x.id ?? x.name));
  const trackChanged = title.trim() !== start.title || key(artists) !== key(start.artists);
  const timeValid = !Number.isNaN(new Date(time).getTime());
  const timeChanged = timeValid && time !== startTime;
  const ready = title.trim() !== "" && artists.length > 0 && timeValid;
  const changed = trackChanged || timeChanged || album.kind !== "keep";

  const save = async () => {
    const body: ScrobbleEditBody = {};
    if (timeChanged) body.played_at = new Date(time).toISOString();
    if (trackChanged) body.new_recording = { title: title.trim(), artists: artists.map((a) => (a.id ? { artist_id: a.id } : { name: a.name })) };
    if (album.kind === "none") body.release_id = null;
    if (album.kind === "existing") { body.release_id = album.id; body.link_release = true; }
    if (album.kind === "new") body.new_release = album.title.trim();
    setBusy(true); setErr("");
    try {
      const r = await endpoints.editScrobble(s.id, body);
      onSaved(); onClose();
      toast({ text: "Scrobble updated", action: { label: "Undo", run: () => endpoints.undo(r.edit_id).then(onSaved).catch(() => toast({ text: "Couldn't undo." })) } });
    } catch (e) {
      setErr(e instanceof ApiError && e.message ? e.message : "Couldn't save.");
      setBusy(false);
    }
  };

  // Points only this scrobble at another existing track. The album stays if
  // that track is on it.
  const relink = async () => {
    if (!relinkTo) return;
    setBusy(true); setErr("");
    try {
      const r = await endpoints.editScrobble(s.id, { recording_id: relinkTo.id });
      onSaved(); onClose();
      toast({ text: `Re-linked to ${relinkTo.name}`, action: { label: "Undo", run: () => endpoints.undo(r.edit_id).then(onSaved).catch(() => toast({ text: "Couldn't undo." })) } });
    } catch (e) {
      setErr(e instanceof ApiError && e.message ? e.message : "Couldn't re-link.");
      setBusy(false);
    }
  };

  const albumName = album.kind === "existing" ? album.name : album.kind === "new" ? album.title : album.kind === "none" ? "" : s.album || "";

  return (
    <Sheet wide title="Edit scrobble" onClose={onClose}
      footer={<><button className="btn btn-plain foot-start" onClick={() => setRelinking(true)}>Re-link…</button><button className="btn" onClick={onClose}>Cancel</button><button className="btn btn-primary" disabled={busy || !changed || !ready} onClick={save}>Save</button></>}>
      <section>
        <label className="field">
          <span className="field-label">Track</span>
          <input className="input" value={title} onChange={(e) => setTitle(e.target.value)} />
        </label>
      </section>

      <section>
        <div className="section-label">Artist</div>
        <div style={{ display: "grid", gap: 6 }}>
          {artists.map((a, i) => (
            <div key={`${a.id ?? "new"}-${a.name}`} className="edit-row">
              <span className="grow truncate">{a.name}{!a.id && <> <Badge>New</Badge></>}</span>
              <button className="btn btn-icon btn-plain" aria-label={`Remove ${a.name}`} onClick={() => setArtists((x) => x.filter((_, j) => j !== i))}><X size={15} /></button>
            </div>
          ))}
          <Autocomplete label="Add an artist" placeholder="Add an artist" search={searchArtists} allowNew newLabel={(q) => `Add “${q}” as a new artist`}
            onPick={(v) => { if (!v.id || !artists.some((a) => a.id === v.id)) setArtists((x) => [...x, v]); }} />
        </div>
      </section>

      <section>
        <div className="section-label">Album</div>
        <div style={{ display: "grid", gap: 6 }}>
          <div className="edit-row">
            <span className="grow truncate">
              {album.kind === "none" || (album.kind === "keep" && !s.album) ? <span className="faint">No album</span> : albumName}
              {album.kind === "new" && <> <Badge>New</Badge></>}
            </span>
            {album.kind !== "keep" && <button className="btn btn-plain" onClick={() => setAlbum({ kind: "keep" })}>Reset</button>}
            {(album.kind === "keep" ? !!s.album : album.kind !== "none") && <button className="btn btn-plain" onClick={() => setAlbum({ kind: "none" })}>Remove</button>}
          </div>
          <Autocomplete label="Change album" placeholder="Search for an album" search={searchAlbums} allowNew newLabel={(q) => `Create album “${q}”`}
            onPick={(v) => setAlbum(v.id ? { kind: "existing", id: v.id, name: v.name } : { kind: "new", title: v.name })} />
          {trackChanged && album.kind === "keep" && s.album && <p className="faint" style={{ fontSize: 12 }}>The album is kept only if the track is on it.</p>}
        </div>
      </section>

      <section>
        <label className="field">
          <span className="field-label">Played at</span>
          <input className="input" type="datetime-local" step={1} value={time} onChange={(e) => setTime(e.target.value)} />
        </label>
      </section>

      {err && !relinking && <p role="alert" style={{ color: "var(--danger)", fontSize: 13 }}>{err}</p>}

      {relinking && (
        <PickDialog kind="recording" title="Re-link scrobble" busy={busy} err={err}
          load={(q) => (s.recording_id ? endpoints.mergeCandidates("recording", s.recording_id, q) : endpoints.candidates("recording", q))}
          initialQuery={s.recording_id ? "" : s.title} summary={(c) => <>Link this scrobble to <strong style={{ color: "var(--label)" }}>{c.name}</strong></>} action="Re-link"
          onCancel={() => { setRelinking(false); setRelinkTo(null); setErr(""); }}
          selected={relinkTo} onSelect={setRelinkTo} onConfirm={relink} />
      )}
    </Sheet>
  );
}
