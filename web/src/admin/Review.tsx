import { useCallback, useEffect, useState } from "react";
import { ExternalLink } from "lucide-react";
import { ApiError, endpoints, type ReviewItem } from "../api";
import { fmtDuration, fmtNum } from "../format";
import { ActionRow, Badge, Group, useToast } from "../ui";
import { AdminHead, useQueue } from "./AdminLayout";

const PROVIDER: Record<string, string> = { musicbrainz: "MusicBrainz", spotify: "Spotify", apple: "Apple Music" };
const METHOD: Record<string, string> = { isrc: "ISRC", lookup: "Lookup", name: "Name", fuzzy: "Fuzzy", url_rel: "URL", mapping: "Mapping", submitted_id: "Submitted id", submitted_mbid: "Submitted id", manual: "Manual" };

function providerURL(p: string, id: string) {
  if (p === "musicbrainz") return `https://musicbrainz.org/recording/${id}`;
  if (p === "spotify") return `https://open.spotify.com/track/${id}`;
  if (p === "apple") return `https://music.apple.com/song/${id}`;
  return "";
}

export function Review() {
  const toast = useToast();
  const { queue, reloadQueue } = useQueue();
  const [items, setItems] = useState<ReviewItem[] | null>(null);
  const [next, setNext] = useState<number | null>(null);

  const load = useCallback(async () => {
    const r = await endpoints.review().catch(() => null);
    if (r) { setItems(r.items); setNext(r.next_after_id); }
  }, []);
  useEffect(() => { load(); }, [load]);

  // New guesses arrive while matching runs; pick them up when the count moves.
  const pending = queue.review_pending || 0;
  useEffect(() => { if (items && pending > items.length && !next) load(); }, [pending]); // eslint-disable-line react-hooks/exhaustive-deps

  const more = async () => {
    if (!next) return;
    const r = await endpoints.review(next);
    setItems((cur) => [...(cur || []), ...r.items]);
    setNext(r.next_after_id);
  };

  const act = useCallback(async (id: number, fn: () => Promise<unknown>, msg: string) => {
    try {
      await fn();
      setItems((cur) => cur?.filter((i) => i.recording_id !== id) || null);
      toast({ text: msg });
      reloadQueue();
    } catch (e) {
      toast({ text: e instanceof ApiError ? e.message : "That didn't work. Try again." });
      throw e;
    }
  }, [toast, reloadQueue]);

  return (
    <>
      <AdminHead title="Review">{!!pending && <span className="muted num">{fmtNum(pending)}</span>}</AdminHead>
      {items === null ? null : !items.length ? (
        <Group><div className="row-setting muted">All caught up.</div></Group>
      ) : (
        <div className="groups">
          {items.map((item) => <ReviewCard key={item.recording_id} item={item} act={act} />)}
          {next && <button className="btn" style={{ justifySelf: "center" }} onClick={more}>Show more</button>}
        </div>
      )}
    </>
  );
}

type Act = (id: number, fn: () => Promise<unknown>, msg: string) => Promise<void>;

function ReviewCard({ item, act }: { item: ReviewItem; act: Act }) {
  const [url, setUrl] = useState("");
  const id = item.recording_id;
  const manual = (e: React.FormEvent) => {
    e.preventDefault();
    if (url.trim()) act(id, () => endpoints.manualLink(id, url.trim()), "Linked").then(() => setUrl(""), () => {});
  };
  const by = [item.artist, item.album, item.duration_ms ? fmtDuration(item.duration_ms) : ""].filter(Boolean).join(" · ");
  return (
    <Group title={<>{item.title} <span className="faint" style={{ fontWeight: 400 }}>{by}</span></>} tip={[item.title, by].filter(Boolean).join(" · ")}>
      {item.candidates.slice(0, 3).map((c) => {
        const rels = c.release_details || [];
        const link = providerURL(c.provider, c.provider_id);
        return (
          <div key={c.link_id} className="row-setting" style={{ alignItems: "flex-start", paddingTop: 10, paddingBottom: 10 }}>
            <span className="list-main" style={{ gap: 3 }}>
              <span className="list-title">
                {c.title || c.provider_id}
                {c.disambiguation && <span className="muted" style={{ fontWeight: 400 }}> ({c.disambiguation})</span>}
                <span className="faint num" style={{ fontWeight: 400 }}> {fmtDuration(c.duration_ms)}</span>
              </span>
              <span className="list-sub truncate" title={c.artists?.join(", ")}>{c.artists?.join(", ")}</span>
              <span className="list-sub" style={{ display: "grid", gap: 1 }}>
                {!c.known ? <span className="faint">No details fetched</span> : !rels.length ? <span className="faint">No releases listed</span> : rels.map((r, i) => {
                  const meta = [r.date?.slice(0, 4), r.type, r.status && r.status !== "official" ? r.status : ""].filter(Boolean).map((x) => ` · ${x}`).join("");
                  return <span key={i} className="truncate" title={r.title + meta}>{r.title}<span className="faint">{meta}</span></span>;
                })}
                {(c.release_count || 0) > rels.length && <span className="faint">+{(c.release_count || 0) - rels.length} more releases</span>}
              </span>
              <span style={{ display: "flex", gap: 6, flexWrap: "wrap", marginTop: 2 }}>
                <Badge>{PROVIDER[c.provider] || c.provider}</Badge>
                <Badge>via {METHOD[c.method] || c.method}</Badge>
                {c.isrcs?.slice(0, 2).map((i) => <Badge key={i}>{i}</Badge>)}
                {link && <a className="badge" href={link} target="_blank" rel="noreferrer noopener">{c.provider_id.slice(0, 8)} <ExternalLink size={11} /></a>}
              </span>
            </span>
            <span style={{ display: "flex", gap: 10, alignItems: "center" }}>
              <span className="muted num">{Math.round(c.confidence * 100)}%</span>
              <button className="btn" onClick={() => act(id, () => endpoints.confirmLink(c.link_id), "Linked").catch(() => {})}>Link</button>
            </span>
          </div>
        );
      })}
      <form className="row-setting" onSubmit={manual}>
        <input className="input" style={{ flex: 1 }} placeholder="MusicBrainz, Spotify or Apple Music link" aria-label="Link manually"
          value={url} onChange={(e) => setUrl(e.target.value)} />
        <button className="btn" type="submit" disabled={!url.trim()}>Link</button>
      </form>
      <ActionRow onClick={() => act(id, () => Promise.all(item.candidates.map((c) => endpoints.rejectLink(c.link_id))), "Rejected").catch(() => {})}>None of these</ActionRow>
      <ActionRow onClick={() => act(id, () => endpoints.rematch(id), "Searching again").catch(() => {})}>Search again</ActionRow>
    </Group>
  );
}
