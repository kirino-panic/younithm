import { useEffect, useMemo, useState } from "react";
import GridLayout, { WidthProvider, type Layout } from "react-grid-layout";
import { ArrowDown, ArrowUp, Eye, EyeOff, Maximize2, Minimize2, Palette, Plus, Trash2, Undo2 } from "lucide-react";
import { usePage } from "./PageContext";
import { fitSize, snapX, statsCap, TILES } from "./tiles";
import { defaultGreeting, mirrorsDesktop, mobileLayout, newId, renderGreeting, type MobileItem, type TileDef, type TileType } from "./doc";
import { PersonalizePanel } from "./PersonalizePanel";
import { Modal, Segmented, Switch, useData, useToast } from "../ui";
import { endpoints } from "../api";
import { useSession } from "../session";

const Grid = WidthProvider(GridLayout);
const ADD = "__add";

function useNarrow() {
  const q = "(max-width: 719px)";
  const [narrow, set] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const h = () => set(m.matches);
    m.addEventListener("change", h);
    return () => m.removeEventListener("change", h);
  }, []);
  return narrow;
}

export function HomePage() {
  const page = usePage();
  const narrow = useNarrow();
  const toast = useToast();
  const [selected, setSelected] = useState<string | null>(null);
  const [picker, setPicker] = useState(false);
  const [theme, setTheme] = useState(false);
  const [view, setView] = useState<"desktop" | "mobile">("desktop");
  const editing = page.editing && !narrow;
  const tiles = page.doc.tiles;

  const mirror = mirrorsDesktop(page.doc);
  useEffect(() => { if (!page.editing) { setSelected(null); setTheme(false); setView("desktop"); } }, [page.editing]);
  useEffect(() => { if (mirror) setView("desktop"); }, [mirror]);

  const layout = useMemo<Layout[]>(() => {
    const l: Layout[] = tiles.map((t) => {
      if (!TILES[t.type]) return { i: t.id, x: t.x, y: t.y, w: t.w, h: t.h };
      const z = fitSize(t.type, t.w, t.h);
      const r = TILES[t.type].resize;
      return r ? { i: t.id, x: snapX(t.x, z.w), y: t.y, w: z.w, h: z.h, minW: r.minW, minH: r.minH, maxW: r.maxW, maxH: r.maxH, isResizable: editing }
        : { i: t.id, x: snapX(t.x, z.w), y: t.y, w: z.w, h: z.h, isResizable: false };
    });
    if (editing) {
      const bottom = tiles.reduce((m, t) => Math.max(m, t.y + t.h), 0);
      // Full width and pushed far down, so layout compaction always settles it under the last tile.
      l.push({ i: ADD, x: 0, y: bottom + 1000, w: 12, h: 2, isDraggable: false, isResizable: false });
    }
    return l;
  }, [tiles, editing]);

  const commit = (l: Layout[]) => {
    page.update((d) => ({
      ...d,
      tiles: d.tiles.map((t) => {
        const p = l.find((x) => x.i === t.id);
        return p ? { ...t, x: snapX(p.x, p.w), y: p.y, w: p.w, h: p.h } : t;
      }),
    }));
  };

  const addTile = (type: TileType) => {
    const spec = TILES[type];
    const bottom = tiles.reduce((m, t) => Math.max(m, t.y + t.h), 0);
    const t: TileDef = { id: newId(), type, x: 0, y: bottom, w: spec.sizes[0].w, h: spec.sizes[0].h, opts: { ...spec.opts } };
    page.update((d) => ({ ...d, tiles: [...d.tiles, t] }));
    setPicker(false);
    setSelected(t.id);
  };

  const publish = async () => {
    try {
      await page.publish();
      toast({ text: "Saved" });
    } catch {
      toast({ text: "Couldn't save. Try again." });
    }
  };

  const floatbar = editing && (
    <div className="floatbar" onClick={(e) => e.stopPropagation()}>
      <button className="btn btn-icon" onClick={page.undo} disabled={!page.canUndo} aria-label="Undo"><Undo2 size={16} /></button>
      {!mirror && <Segmented label="Layout" value={view} onChange={setView} options={[{ value: "desktop", label: "Desktop" }, { value: "mobile", label: "Mobile" }]} />}
      <button className="btn" onClick={() => setTheme(true)}><Palette size={16} />Personalize</button>
      <button className="btn btn-plain" onClick={page.discard}>Cancel</button>
      <button className="btn btn-primary" onClick={publish}>Done</button>
    </div>
  );

  // Render tiles only once the saved layout is known: the default layout has
  // different tile ids, so showing it first would mount every tile twice.
  if (!page.loaded) return <div className="page" />;

  if (narrow) {
    return (
      <div className="page">
        <Greeting />
        <MobileTiles items={mobileLayout(page.doc).filter((m) => !m.hidden)} tiles={tiles} />
      </div>
    );
  }

  if (editing && view === "mobile") {
    return (
      <div className="page editing">
        <div className="phone"><Greeting /><MobileTiles items={mobileLayout(page.doc)} tiles={tiles} editable /></div>
        {floatbar}
      </div>
    );
  }

  return (
    <div className={`page${editing ? " editing" : ""}`} onClick={() => setSelected(null)}>
      <Greeting />
      <Grid
        layout={layout}
        cols={12}
        rowHeight={40}
        margin={[12, 12]}
        containerPadding={[0, 0]}
        isDraggable={editing}
        isResizable={editing}
        compactType="vertical"
        draggableCancel=".no-drag"
        onDragStop={(l, from, to) => {
          if (from.x === to.x && from.y === to.y) setSelected(to.i); // a click, not a drag
          else commit(l);
        }}
        onResizeStop={(l, from, to) => { if (from.w !== to.w || from.h !== to.h) commit(l); }}
      >
        {tiles.map((t) => {
          const spec = TILES[t.type];
          if (!spec) return <div key={t.id} />;
          const isSel = selected === t.id;
          return (
            <div key={t.id} className={isSel ? "selected" : ""} style={{ zIndex: isSel ? 5 : undefined }}
              onClick={(e) => { if (editing) { e.stopPropagation(); setSelected(t.id); } }}>
              <div className={`tile tile-${t.type}${spec.plain && !editing ? " tile-plain" : ""}`} style={editing ? { pointerEvents: "auto" } : undefined}>
                <div style={editing ? { pointerEvents: "none", display: "contents" } : { display: "contents" }}>{spec.render(t)}</div>
              </div>
              {isSel && <TilePopover t={t} mirror={mirror} onClose={() => setSelected(null)} />}
            </div>
          );
        })}
        {editing && (
          <div key={ADD}>
            <button className="add-tile no-drag" onClick={(e) => { e.stopPropagation(); setPicker(true); }}><Plus size={16} />Add tile</button>
          </div>
        )}
      </Grid>

      {floatbar}
      {picker && <TilePicker onPick={addTile} onClose={() => setPicker(false)} />}
      {theme && <PersonalizePanel onClose={() => setTheme(false)} />}
    </div>
  );
}

function TilePopover({ t, mirror, onClose }: { t: TileDef; mirror: boolean; onClose: () => void }) {
  const page = usePage();
  const spec = TILES[t.type];
  const set = (patch: Partial<TileDef>) => page.update((d) => ({ ...d, tiles: d.tiles.map((x) => (x.id === t.id ? { ...x, ...patch } : x)) }));
  const setOpt = (k: string, v: unknown) => set({ opts: { ...t.opts, [k]: v } });
  const remove = () => { page.update((d) => ({ ...d, tiles: d.tiles.filter((x) => x.id !== t.id) })); onClose(); };

  return (
    <div className="popover no-drag" style={{ top: 8, right: 8 }} onClick={(e) => e.stopPropagation()} onMouseDown={(e) => e.stopPropagation()}>
      <div className="popover-title">{spec.label}</div>
      {spec.sizes.length > 1 && (
        <label className="row-setting">
          <span className="muted">Size</span>
          <select className="select" style={{ width: 130 }} value={spec.sizes.find((z) => z.w === t.w && z.h === t.h)?.label ?? (spec.resize ? "Custom" : fitSize(t.type, t.w, t.h).label)}
            onChange={(e) => { const z = spec.sizes.find((x) => x.label === e.target.value)!; set({ w: z.w, h: z.h, x: snapX(t.x, z.w) }); }}>
            {spec.resize && !spec.sizes.some((z) => z.w === t.w && z.h === t.h) && <option value="Custom">Custom · {t.w}×{t.h}</option>}
            {spec.sizes.map((z) => <option key={z.label} value={z.label}>{z.label} · {z.w}×{z.h}</option>)}
          </select>
        </label>
      )}
      {spec.options.map((o) => (
        <label key={o.key} className="row-setting">
          <span className="muted">{o.label}</span>
          <select className="select" style={{ width: 130 }} value={String(t.opts[o.key] ?? o.fallback?.(t) ?? o.choices[0].value)} onChange={(e) => setOpt(o.key, e.target.value)}>
            {o.choices.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
          </select>
        </label>
      ))}
      <MoreOptions t={t} setOpt={setOpt} />
      {mirror && (
        <div className="row-setting">
          <span className="muted">Show on mobile</span>
          <Switch label="Show on mobile" checked={!t.hideMobile} onChange={(v) => set({ hideMobile: !v })} />
        </div>
      )}
      <div className="popover-foot">
        <button className="btn btn-plain btn-icon btn-danger" onClick={remove} aria-label="Remove tile"><Trash2 size={16} /></button>
      </div>
    </div>
  );
}

const ALL_METRICS = [["plays", "Plays"], ["minutes", "Minutes"], ["tracks", "Tracks"], ["artists", "Artists"], ["days", "Days active"], ["avg", "Avg. daily"]];

function MoreOptions({ t, setOpt }: { t: TileDef; setOpt: (k: string, v: unknown) => void }) {
  if (t.type === "stats") {
    const cur = (t.opts.metrics as string[]) || [];
    const cap = statsCap(t);
    return (
      <div className="popover-block" style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 6, fontSize: 13 }}>
        {ALL_METRICS.map(([k, l]) => (
          <label key={k} style={{ display: "flex", gap: 6, alignItems: "center" }}>
            <input type="checkbox" checked={cur.includes(k)} disabled={!cur.includes(k) && cur.length >= cap} onChange={(e) => setOpt("metrics", e.target.checked ? [...cur, k] : cur.filter((x) => x !== k))} />{l}
          </label>
        ))}
      </div>
    );
  }
  if (t.type === "divider") {
    return <div className="popover-block"><input className="input" placeholder="Label (optional)" maxLength={60} value={String(t.opts.label || "")} onChange={(e) => setOpt("label", e.target.value)} /></div>;
  }
  if (t.type === "text") {
    return <div className="popover-block"><textarea className="textarea" rows={3} value={String(t.opts.text || "")} onChange={(e) => setOpt("text", e.target.value)} style={{ fontFamily: "var(--font)" }} /></div>;
  }
  return null;
}

function TilePicker({ onPick, onClose }: { onPick: (t: TileType) => void; onClose: () => void }) {
  return (
    <Modal onClose={onClose}>
      <h3 style={{ fontSize: 17 }}>Add tile</h3>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(140px, 1fr))", gap: 8 }}>
        {(Object.keys(TILES) as TileType[]).map((k) => {
          const { icon: Icon, label } = TILES[k];
          return (
            <button key={k} className="btn" style={{ height: 64, flexDirection: "column", gap: 6 }} onClick={() => onPick(k)}>
              <Icon size={20} strokeWidth={1.8} />{label}
            </button>
          );
        })}
      </div>
    </Modal>
  );
}

// The phone layout: a two-column flow. Public pages render it as is; the
// editor adds controls to move, resize and hide each tile.
function MobileTiles({ items, tiles, editable }: { items: MobileItem[]; tiles: TileDef[]; editable?: boolean }) {
  const page = usePage();
  const byId = new Map(tiles.map((t) => [t.id, t]));
  const patch = (id: string, p: Partial<MobileItem>) =>
    page.update((d) => ({ ...d, mobile: mobileLayout(d).map((m) => (m.id === id ? { ...m, ...p } : m)) }));
  const move = (i: number, dir: number) =>
    page.update((d) => {
      const l = mobileLayout(d);
      [l[i], l[i + dir]] = [l[i + dir], l[i]];
      return { ...d, mobile: l };
    });
  return (
    <div className="stack-grid">
      {items.map((m, i) => {
        const t = byId.get(m.id);
        const spec = t && TILES[t.type];
        if (!t || !spec) return null;
        const auto = t.type === "divider" || t.type === "top" || t.type === "recent" || t.type === "stats" || t.type === "text" || t.type === "now" || t.type === "range";
        const h = auto ? undefined : t.type === "activity" ? 170 : Math.min(460, Math.max(160, t.h * 34));
        const cls = `tile tile-${t.type}${spec.plain && !editable ? " tile-plain" : ""}${t.type === "top" || t.type === "recent" ? " tile-auto" : ""}`;
        return (
          <div key={t.id} className={`${m.span === 2 ? "span-2 " : ""}${editable ? "m-edit" : ""}${m.hidden ? " m-hidden" : ""}`} style={{ height: h }}>
            <div className={cls}>{spec.render(t)}</div>
            {editable && (
              <div className="m-ctl" onClick={(e) => e.stopPropagation()}>
                <button className="btn btn-icon" aria-label="Move up" disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={15} /></button>
                <button className="btn btn-icon" aria-label="Move down" disabled={i === items.length - 1} onClick={() => move(i, 1)}><ArrowDown size={15} /></button>
                <button className="btn btn-icon" aria-label={m.span === 2 ? "Make half width" : "Make full width"} onClick={() => patch(m.id, { span: m.span === 2 ? 1 : 2 })}>
                  {m.span === 2 ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
                </button>
                <button className="btn btn-icon" aria-label={m.hidden ? "Show on mobile" : "Hide on mobile"} onClick={() => patch(m.id, { hidden: !m.hidden || undefined })}>
                  {m.hidden ? <EyeOff size={15} /> : <Eye size={15} />}
                </button>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

function Greeting() {
  const { doc } = usePage();
  const { me, admin } = useSession();
  const profile = useData(() => endpoints.profile(), []);
  const g = { ...defaultGreeting, ...doc.greeting };
  if (!g.show) return null;
  const name = profile.data?.display_name || "";
  const text = admin ? renderGreeting(g.owner, name || me?.username || "") : renderGreeting(g.visitor, name || "Someone");
  if (!text) return null;
  return <h1 className="greeting">{text}</h1>;
}
