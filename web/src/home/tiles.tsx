import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { BarChart3, UserRound, CalendarDays, Clock, ListOrdered, Play, Minus, SlidersHorizontal, Type, type LucideIcon } from "lucide-react";
import { endpoints, withBase, type NowPlaying, type Range, type TopItem, type TopKind } from "../api";
import { fmtAgo, fmtNum, ranks } from "../format";
import { Cover, Segmented, useData } from "../ui";
import { usePage } from "./PageContext";
import { useSession } from "../session";
import type { TileDef, TileType } from "./doc";
import { ArtistLinks } from "../library/Library";

// Tiles come in a few set sizes (columns of 12 by rows of 52 px), chosen from a
// menu rather than dragged. Heights are 1, 2, 3, 5 or 10 so tiles stack to equal lengths.
export interface TileSize { label: string; w: number; h: number }

export interface Option {
  key: string; label: string; choices: { value: string; label: string }[];
  fallback?: (t: TileDef) => string; // the value in force when none is stored, if not the first choice
}

interface TileSpec {
  label: string;
  icon: LucideIcon;
  sizes: TileSize[]; // named sizes in the menu; the first is the default
  resize?: { minW: number; minH: number; maxW: number; maxH: number }; // set when the tile can also be dragged to any size in range
  opts: Record<string, unknown>;
  options: Option[]; // shown in the tile popover, at most three
  plain?: boolean;
  render: (t: TileDef) => ReactNode;
}

const RANGE_CHOICES = [
  { value: "page", label: "Follow page" }, { value: "week", label: "Week" },
  { value: "month", label: "Month" }, { value: "year", label: "Year" }, { value: "all", label: "All time" },
];
const RANGE_LABEL: Record<string, string> = { week: "This week", month: "This month", year: "This year", all: "All time" };

const DENSITY: Option = { key: "density", label: "Density", choices: [{ value: "comfortable", label: "Comfortable" }, { value: "compact", label: "Compact" }] };
const isCompact = (t: TileDef) => t.opts.density === "compact";

export const TILES: Record<TileType, TileSpec> = {
  now: {
    label: "Now playing", icon: Play, sizes: [{ label: "Medium", w: 6, h: 3 }, { label: "Small", w: 4, h: 3 }, { label: "Large", w: 8, h: 3 }, { label: "Wide", w: 12, h: 3 }], opts: {}, options: [DENSITY],
    render: (t) => <NowPlayingTile compact={isCompact(t)} />,
  },
  top: {
    label: "Top list", icon: ListOrdered, sizes: [{ label: "Standard", w: 4, h: 12 }],
    opts: { kind: "artists", range: "page" },
    options: [
      { key: "kind", label: "Show", choices: [{ value: "artists", label: "Artists" }, { value: "albums", label: "Albums" }, { value: "tracks", label: "Tracks" }] },
      { key: "range", label: "Range", choices: RANGE_CHOICES },
      { key: "bars", label: "Bars", choices: [{ value: "on", label: "Show" }, { value: "off", label: "Hide" }], fallback: (t) => (t.opts.kind === "tracks" ? "on" : "off") },
    ],
    render: (t) => <TopTile t={t} />,
  },
  activity: {
    label: "Activity", icon: CalendarDays, sizes: [{ label: "Large", w: 8, h: 5 }, { label: "Medium", w: 6, h: 5 }, { label: "Small", w: 4, h: 5 }, { label: "Wide", w: 12, h: 5 }],
    resize: { minW: 4, minH: 3, maxW: 12, maxH: 10 }, opts: {}, options: [],
    render: () => <ActivityTile />,
  },
  stats: {
    label: "Stats", icon: BarChart3, sizes: [{ label: "Small", w: 4, h: 3 }, { label: "Medium", w: 4, h: 5 }, { label: "Wide", w: 12, h: 3 }],
    opts: { metrics: ["plays", "minutes", "tracks", "artists"] }, options: [DENSITY],
    render: (t) => <StatsTile t={t} />,
  },
  recent: {
    label: "Recent", icon: Clock, sizes: [{ label: "Standard", w: 4, h: 12 }, { label: "Short", w: 4, h: 6 }, { label: "Wide", w: 8, h: 12 }],
    resize: { minW: 3, minH: 3, maxW: 12, maxH: 20 }, opts: {}, options: [DENSITY],
    render: (t) => <RecentTile t={t} />,
  },
  range: {
    label: "Range bar", icon: SlidersHorizontal, sizes: [{ label: "Full width", w: 12, h: 1 }], opts: {}, options: [], plain: true,
    render: () => <RangeTile />,
  },
  profile: {
    label: "Profile", icon: UserRound, sizes: [{ label: "Small", w: 4, h: 3 }, { label: "Medium", w: 4, h: 5 }, { label: "Wide", w: 8, h: 3 }],
    resize: { minW: 4, minH: 3, maxW: 12, maxH: 10 }, opts: {}, options: [],
    render: () => <ProfileTile />,
  },
  divider: {
    label: "Separator", icon: Minus, sizes: [{ label: "Full width", w: 12, h: 1 }], opts: { label: "" }, options: [], plain: true,
    render: (t) => <Divider label={String(t.opts.label || "")} />,
  },
  text: {
    label: "Text", icon: Type, sizes: [{ label: "Small", w: 4, h: 3 }, { label: "Medium", w: 4, h: 5 }, { label: "Wide", w: 8, h: 3 }, { label: "Banner", w: 12, h: 2 }],
    resize: { minW: 3, minH: 1, maxW: 12, maxH: 10 }, opts: { text: "Hello." }, options: [],
    render: (t) => <div className="tile-body" style={{ whiteSpace: "pre-wrap", overflow: "auto" }}>{String(t.opts.text || "")}</div>,
  },
};

// The allowed size closest to a stored one, so older pages settle onto the grid.
export function fitSize(type: TileType, w: number, h: number): TileSize {
  const spec = TILES[type];
  if (spec.resize) { // free within the range
    const r = spec.resize;
    return { label: "Custom", w: Math.max(r.minW, Math.min(r.maxW, w)), h: Math.max(r.minH, Math.min(r.maxH, h)) };
  }
  const sizes = spec.sizes;
  return sizes.reduce((best, z) => (Math.abs(z.w - w) * 2 + Math.abs(z.h - h) < Math.abs(best.w - w) * 2 + Math.abs(best.h - h) ? z : best), sizes[0]);
}

// Tiles sit on column lines that match their width, so edges line up down the page.
export function snapX(x: number, w: number): number {
  const step = w >= 12 ? 12 : w % 4 === 0 ? 4 : 3;
  return Math.max(0, Math.min(12 - w, Math.round(x / step) * step));
}

// ---------- Profile ----------

// Avatar, display name and the year of the first listen. Wide tiles lay out in a
// row, tall ones stack; the avatar scales with the tile.
function ProfileTile() {
  const { me, admin } = useSession();
  const profile = useData(() => endpoints.profile(), []);
  const stats = useData(() => endpoints.stats("all"), []);
  const name = profile.data?.display_name || (admin ? me?.username : "") || "Listener";
  const since = stats.data?.per_day.find((d) => d.plays > 0)?.date.slice(0, 4);
  return (
    <div className="profile">
      <div className="profile-avatar"><Cover name={name} src={profile.data?.avatar || undefined} size="100%" round /></div>
      <div className="profile-text">
        <div className="profile-name">{name}</div>
        {since && <div className="muted num" style={{ fontSize: "var(--t-footnote)" }}>est. {since}</div>}
      </div>
    </div>
  );
}

// A rule across the page, with an optional label that breaks the page into sections.
function Divider({ label }: { label: string }) {
  return (
    <div className="divider">
      <i />{label && <span className="truncate">{label}</span>}{label && <i />}
    </div>
  );
}

function Title({ children, end }: { children: ReactNode; end?: ReactNode }) {
  return <div className="tile-title"><span className="truncate">{children}</span>{end}</div>;
}

function Empty({ children = "No listens yet" }: { children?: ReactNode }) {
  return <div className="tile-empty">{children}</div>;
}

// ---------- Now playing ----------

function NowPlayingTile({ compact }: { compact: boolean }) {
  const [np, setNp] = useState<NowPlaying | null>(null);
  useEffect(() => {
    let es: EventSource | null = null;
    let poll: number | undefined;
    endpoints.nowPlaying().then(setNp).catch(() => setNp({ playing: false }));
    try {
      es = new EventSource(withBase("/api/v1/now-playing/stream"));
      const on = (e: MessageEvent) => { try { setNp(JSON.parse(e.data)); } catch { /* ignore */ } };
      es.addEventListener("now-playing", on);
      es.onmessage = on;
      es.onerror = () => {
        es?.close();
        poll = window.setInterval(() => endpoints.nowPlaying().then(setNp).catch(() => {}), 15000);
      };
    } catch { /* no SSE */ }
    return () => { es?.close(); window.clearInterval(poll); };
  }, []);

  // With nothing playing, show the most-played track of the last seven days of
  // listening: the past week, or if that is empty, the week up to the newest listen.
  const fav = useData(async () => {
    const recent = (await endpoints.top("tracks", "week", 1)).items[0];
    return recent || (await endpoints.top("tracks", "latest_week", 1)).items[0];
  }, []);
  const f = fav.data;
  const playing = np?.track;
  const tr = playing ? { title: playing.title, sub: `${playing.artist}${playing.album && !compact ? ` · ${playing.album}` : ""}`, image: playing.image, cover: playing.album || playing.title, to: playing.recording_id ? `/track/${playing.recording_id}` : "" }
    : f ? { title: f.name, sub: f.artist || "", image: f.image, cover: f.name, to: f.id ? `/track/${f.id}` : "" } : null;
  const pct = playing?.duration_ms && np?.progress_ms ? Math.min(100, (np.progress_ms / playing.duration_ms) * 100) : 0;
  return (
    <>
      <Title>{playing || !np ? "Now playing" : "Current favorite"}</Title>
      {!np ? null : !tr ? <Empty>Nothing playing</Empty> : (
        <MaybeLink to={tr.to} className="np-link" style={{ display: "flex", gap: compact ? 10 : 14, alignItems: "center", minWidth: 0, flex: 1 }}>
          <Cover name={tr.cover} src={tr.image} size={compact ? 40 : 64} />
          <div style={{ minWidth: 0, flex: 1, display: "grid", gap: 2 }}>
            <div className="truncate np-title" style={{ fontWeight: 600, fontSize: compact ? 14 : 17 }}>{tr.title}</div>
            <div className="truncate muted" style={{ fontSize: 13 }}>{tr.sub}</div>
            {pct > 0 && <div className="progress" style={{ marginTop: compact ? 4 : 8 }}><i style={{ width: `${pct}%` }} /></div>}
          </div>
        </MaybeLink>
      )}
    </>
  );
}

// ---------- Top list ----------

const kindPath: Record<TopKind, string> = { artists: "artist", albums: "album", tracks: "track" };

export const topCount = (kind: TopKind) => (kind === "tracks" ? 10 : 5);

function TopTile({ t }: { t: TileDef }) {
  const page = usePage();
  const kind = (t.opts.kind as TopKind) || "artists";
  const rangeOpt = (t.opts.range as string) || "page";
  const range = (rangeOpt === "page" ? page.range : rangeOpt) as Range;
  const count = topCount(kind);
  const { data, error } = useData(() => endpoints.top(kind, range, count), [kind, range, count]);
  const items = (data?.items || []).slice(0, count);
  const rk = ranks(items.map((i) => i.plays));
  const href = (i: TopItem) => (i.id ? `/${kindPath[kind]}/${i.id}` : "");
  const bars = (t.opts.bars ?? (kind === "tracks" ? "on" : "off")) === "on"; // relative plays, as on the artist page
  const lead = kind !== "tracks"; // artists and albums: #1 gets the square, 2-5 are rows
  const rows = lead ? items.slice(1) : items;

  return (
    <>
      <Title end={<span className="faint" style={{ fontWeight: 500 }}>{RANGE_LABEL[range]}</span>}>Top {kind}</Title>
      {error ? <Empty>Hidden</Empty> : !items.length ? <Empty /> : (
        <div className="tile-body list list-fill">
          {lead && (
            <MaybeLink to={href(items[0])} className="top-lead">
              <div className="top-lead-square">
                <Cover name={items[0].name} src={items[0].image} size="100%" />
                <span className="top-lead-text">
                  <span className="top-lead-rank num">{String(rk[0]).padStart(2, "0")}</span>
                  <span className="top-lead-name">{items[0].name}</span>
                  <span className="top-lead-sub truncate">{items[0].artist ? `${items[0].artist} · ` : ""}{fmtNum(items[0].plays)} plays</span>
                </span>
              </div>
            </MaybeLink>
          )}
          {rows.map((i, n) => {
            const idx = n + (lead ? 1 : 0);
            return (
              <MaybeLink key={idx} to={href(i)} className="list-row">
                <span className="list-rank num">{String(rk[idx]).padStart(2, "0")}</span>
                <Cover name={i.name} src={i.image} size={36} round={kind === "artists"} />
                <span className="list-main">
                  <span className="list-title truncate">{i.name}</span>
                  {i.artist && <span className="list-sub truncate">{i.artist}</span>}
                  {bars && <span className="list-bar" aria-hidden><i style={{ width: `${Math.max(2, (i.plays / Math.max(1, items[0].plays)) * 100)}%` }} /></span>}
                </span>
                <span className="list-end num">{fmtNum(i.plays)}</span>
              </MaybeLink>
            );
          })}
        </div>
      )}
    </>
  );
}

function MaybeLink({ to, children, ...rest }: { to: string; children: ReactNode; className?: string; style?: React.CSSProperties; "aria-label"?: string }) {
  return to ? <Link to={to} {...rest}>{children}</Link> : <div {...rest}>{children}</div>;
}

// ---------- Activity heatmap ----------

function ActivityTile() {
  const { data, error } = useData(() => endpoints.stats("year"), []);
  const [weeks, setWeeks] = useState(53);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setWeeks(Math.max(8, Math.min(53, Math.floor(e.contentRect.width / 14)))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const cells = useMemo(() => buildHeat(data?.per_day || [], weeks), [data, weeks]);
  const max = Math.max(1, ...cells.map((c) => c.plays));
  const streak = useMemo(() => {
    let s = 0;
    for (let i = cells.length - 1; i >= 0 && !cells[i].future; i--) {
      if (cells[i].plays > 0) s++;
      else if (s > 0 || i < cells.length - 1) break;
    }
    return s;
  }, [cells]);
  return (
    <>
      <Title end={<span className="faint num" style={{ fontWeight: 500 }}>Streak {streak}</span>}>Activity</Title>
      {error ? <Empty>Hidden</Empty> : (
        <div ref={ref} className="tile-body" style={{ display: "flex", alignItems: "center" }}>
          <div className="heat" style={{ width: "100%" }}>
            {cells.map((c) => (
              <i key={c.date} title={c.future ? undefined : `${c.date} · ${c.plays} plays`}
                style={{ background: c.future ? "transparent" : c.plays ? `color-mix(in srgb, var(--accent) ${20 + (c.plays / max) * 80}%, transparent)` : undefined }} />
            ))}
          </div>
        </div>
      )}
    </>
  );
}

function buildHeat(perDay: { date: string; plays: number }[], weeks: number) {
  const map = new Map(perDay.map((d) => [d.date, d.plays]));
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const start = new Date(today);
  start.setDate(start.getDate() - (weeks - 1) * 7 - start.getDay());
  const out: { date: string; plays: number; future: boolean }[] = [];
  for (let d = new Date(start); out.length < weeks * 7; d.setDate(d.getDate() + 1)) {
    const key = d.toISOString().slice(0, 10);
    out.push({ date: key, plays: map.get(key) || 0, future: d > today });
  }
  return out;
}

// ---------- Stats ----------

// Metrics that fit: the small tile has room for two rows of two.
export const statsCap = (t: TileDef) => (t.w <= 4 && t.h <= 3 ? 4 : 6);
const statsCompact = (t: TileDef) => isCompact(t) || (t.w <= 4 && t.h <= 3);

const METRICS: Record<string, string> = { plays: "Plays", minutes: "Minutes", tracks: "Tracks", artists: "Artists", days: "Days active", avg: "Avg. daily plays" };

function StatsTile({ t }: { t: TileDef }) {
  const { data, error } = useData(() => endpoints.stats("all"), []);
  const metrics = ((t.opts.metrics as string[]) || ["plays"]).slice(0, statsCap(t));
  const compact = statsCompact(t);
  const wide = t.w >= 8; // spread across the tile and centred, so a short row of metrics doesn't huddle on the left
  const val = (m: string) => {
    if (!data) return "–";
    const days = data.per_day.filter((d) => d.plays > 0).length;
    switch (m) {
      case "plays": return fmtNum(data.plays);
      case "minutes": return fmtNum(data.listened_ms / 60000);
      case "tracks": return fmtNum(data.distinct_tracks);
      case "artists": return fmtNum(data.distinct_artists);
      case "days": return fmtNum(days);
      case "avg": return days ? (data.plays / days).toFixed(1) : "0";
    }
    return "–";
  };
  return (
    <>
      <Title>All-time stats</Title>
      {error ? <Empty>Hidden</Empty> : (
        <div className="tile-body" style={{
          display: "grid", gridTemplateColumns: `repeat(${wide ? "auto-fit" : "auto-fill"}, minmax(${compact ? 84 : 120}px, 1fr))`,
          gap: compact ? "6px 12px" : "10px 16px", alignContent: wide ? "center" : "start", textAlign: wide ? "center" : undefined,
        }}>
          {metrics.map((m) => (
            <div key={m}>
              <div className="num" style={{ fontSize: compact ? 16 : 22, fontWeight: 700, letterSpacing: "-0.01em" }}>{val(m)}</div>
              <div className="muted" style={{ fontSize: 12 }}>{METRICS[m] || m}</div>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

// ---------- Recent ----------

// Rows are never shorter than this, so however tall the tile is, every row shown fits.
const RECENT_MIN_ROW = { comfortable: 44, compact: 30 };
const RECENT_FETCH = 30;

function RecentTile({ t }: { t: TileDef }) {
  const compact = isCompact(t);
  const minRow = compact ? RECENT_MIN_ROW.compact : RECENT_MIN_ROW.comfortable;
  const { data, error } = useData(() => endpoints.recent(RECENT_FETCH), []);
  const ref = useRef<HTMLDivElement>(null);
  const [fit, setFit] = useState(0); // 0 until measured
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (el.closest(".tile-auto")) { setFit(compact ? 15 : 10); return; } // on phones the tile grows with its rows
    const ro = new ResizeObserver(([e]) => setFit(Math.max(1, Math.floor(e.contentRect.height / minRow))));
    ro.observe(el);
    return () => ro.disconnect();
  }, [minRow, compact, data]);
  const all = data?.items || [];
  const items = all.slice(0, fit || (compact ? 15 : 10));
  return (
    <>
      <Title>Recent</Title>
      {error ? <Empty>Hidden</Empty> : !all.length ? <Empty /> : (
        <div ref={ref} className="tile-body list list-fill">
          {items.map((l) => {
            const track = l.recording_id ? `/track/${l.recording_id}` : "";
            return (
              // The cover opens the album, the title the track, each artist its own page.
              <div key={l.id} className={`list-row recent-row${compact ? " compact" : ""}`}>
                <MaybeLink to={l.release_id ? `/album/${l.release_id}` : track} className="recent-cover" aria-label={l.album || l.title}>
                  <Cover name={l.album || l.title} src={l.image} size={compact ? 24 : 36} />
                </MaybeLink>
                <span className="list-main">
                  <MaybeLink to={track} className="list-title truncate">{l.title}</MaybeLink>
                  <span className="list-sub truncate"><ArtistLinks artists={l.artists} fallback={l.artist} /></span>
                </span>
                <span className="list-end">{fmtAgo(l.played_at)}</span>
              </div>
            );
          })}
        </div>
      )}
    </>
  );
}

// ---------- Range bar ----------

function RangeTile() {
  const page = usePage();
  return (
    <div style={{ height: "100%", display: "flex", alignItems: "center", justifyContent: "center" }}>
      <Segmented label="Time range" value={page.range} onChange={page.setRange}
        options={[{ value: "week", label: "Week" }, { value: "month", label: "Month" }, { value: "year", label: "Year" }, { value: "all", label: "All" }]} />
    </div>
  );
}

