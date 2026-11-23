import { useRef, useState } from "react";
import { ChevronRight, Sparkles } from "lucide-react";
import { usePage } from "./PageContext";
import { AUTO } from "./accent";
import { ACCENTS, defaultDoc, defaultGreeting, mirroredMobile, renderGreeting, mirrorsDesktop, type Theme } from "./doc";
import { Segmented, Sheet, Switch, useData } from "../ui";
import { endpoints } from "../api";
import { useSession } from "../session";

export function PersonalizePanel({ onClose }: { onClose: () => void }) {
  const page = usePage();
  const start = useRef(page.doc); // how things were when the panel opened
  const th = page.doc.theme;
  const [advanced, setAdvanced] = useState(false);
  const set = (patch: Partial<Theme>) => page.update((d) => ({ ...d, theme: { ...d.theme, ...patch } }));
  const { me } = useSession();
  const profile = useData(() => endpoints.profile(), []);
  const myName = profile.data?.display_name || me?.username || "";
  const greeting = { ...defaultGreeting, ...page.doc.greeting };
  const setGreeting = (patch: Partial<typeof greeting>) => page.update((d) => ({ ...d, greeting: { ...greeting, ...patch } }));
  const custom = th.accent !== AUTO && !ACCENTS.includes(th.accent);
  const setBg = (patch: Partial<Theme["bg"]>) => set({ bg: { ...th.bg, ...patch } });

  const upload = (f?: File) => {
    if (!f) return;
    if (f.size > 180 * 1024) { alert("Image is too large for now (180 KB max). Paste a URL instead."); return; }
    const r = new FileReader();
    r.onload = () => setBg({ image: String(r.result) });
    r.readAsDataURL(f);
  };

  return (
    <Sheet title="Personalize" onClose={onClose} noClose footer={
      <>
        <button className="btn" onClick={() => { if (page.doc !== start.current) page.update(() => start.current); onClose(); }}>Cancel</button>
        <button className="btn btn-primary" onClick={onClose}>Save</button>
      </>
    }>
      <section>
        <div className="section-label">Mobile</div>
        <label className="row-setting" style={{ minHeight: 0 }}>
          <span>Mirror desktop layout on mobile</span>
          <Switch label="Mirror desktop layout on mobile" checked={mirrorsDesktop(page.doc)} onChange={(v) =>
            page.update((d) => v ? { ...d, mirror: true } : { ...d, mirror: false, mobile: d.mobile ?? mirroredMobile(d.tiles) })} />
        </label>
        <p className="faint" style={{ fontSize: 12, marginTop: 6 }}>
          {mirrorsDesktop(page.doc) ? "Phones show your desktop tiles in reading order. Hide tiles from the tile menu." : "Phones use their own layout. Edit it with the Mobile view while editing."}
        </p>
      </section>

      <section>
        <div className="section-label">Greeting</div>
        <label className="row-setting" style={{ minHeight: 0 }}>
          <span>Show greeting</span>
          <Switch label="Show greeting" checked={greeting.show} onChange={(show) => setGreeting({ show })} />
        </label>
        {greeting.show && (
          <div style={{ display: "grid", gap: 10, marginTop: 10 }}>
            <GreetingField title="Logged in" note="What you see when signed in." value={greeting.owner}
              placeholder={defaultGreeting.owner} preview={renderGreeting(greeting.owner, myName)} onChange={(owner) => setGreeting({ owner })} />
            <GreetingField title="Logged out" note="What visitors see." value={greeting.visitor}
              placeholder={defaultGreeting.visitor} preview={renderGreeting(greeting.visitor, profile.data?.display_name || "Someone")} onChange={(visitor) => setGreeting({ visitor })} />
            <p className="faint" style={{ fontSize: 12 }}>
              Use {"{greeting}"} for the time of day and {"{user}"} for your display name. Leave a box empty to show nothing.
            </p>
          </div>
        )}
      </section>

      <section className="vlist" style={{ gap: "var(--s4)" }}>
        <div>
          <div className="section-label">Background</div>
          <Segmented full label="Background" value={th.bg.kind} onChange={(kind) => setBg({ kind })}
            options={[{ value: "color", label: "Color" }, { value: "gradient", label: "Gradient" }, { value: "image", label: "Image" }]} />
          <div style={{ display: "grid", gap: 10, marginTop: 12 }}>
            {th.bg.kind !== "image" && (
              <div style={{ display: "flex", gap: 8 }}>
                <input type="color" aria-label="Color" value={th.bg.color} onChange={(e) => setBg({ color: e.target.value })} />
                {th.bg.kind === "gradient" && <input type="color" aria-label="Second color" value={th.bg.color2} onChange={(e) => setBg({ color2: e.target.value })} />}
              </div>
            )}
            {th.bg.kind === "image" && (
              <>
                <div style={{ display: "flex", gap: 8 }}>
                  <input className="input" placeholder="https://…" value={th.bg.image.startsWith("data:") ? "" : th.bg.image} onChange={(e) => setBg({ image: e.target.value })} />
                  <label className="btn">Upload<input type="file" accept="image/*" hidden onChange={(e) => upload(e.target.files?.[0])} /></label>
                </div>
                <Slider label="Blur" min={0} max={40} value={th.bg.blur} onChange={(blur) => setBg({ blur })} />
                <Slider label="Dim" min={0} max={0.9} step={0.05} value={th.bg.dim} onChange={(dim) => setBg({ dim })} />
              </>
            )}
          </div>
        </div>
        <div>
          <div className="section-label">Accent</div>
          <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
            <button className="swatch swatch-auto" style={{ background: th.accent === AUTO ? page.accent : undefined }} title="Auto"
              aria-label="Auto, matches the background" aria-pressed={th.accent === AUTO} onClick={() => set({ accent: AUTO })}>
              <Sparkles size={14} />
            </button>
            {ACCENTS.map((c) => <button key={c} className="swatch" style={{ background: c }} aria-label={c} aria-pressed={th.accent === c} onClick={() => set({ accent: c })} />)}
            <label className="swatch swatch-custom" title="Custom" aria-label="Custom accent" aria-pressed={custom}>
              {custom && <i style={{ background: th.accent }} />}
              <input type="color" value={page.accent} onChange={(e) => set({ accent: e.target.value })} />
            </label>
          </div>
        </div>
        <div>
          <div className="section-label">Mode</div>
          <Segmented full label="Mode" value={th.mode} onChange={(mode) => set({ mode })}
            options={[{ value: "light", label: "Light" }, { value: "dark", label: "Dark" }, { value: "auto", label: "Auto" }]} />
        </div>
        <div>
          <div className="section-label">Tiles</div>
          <Segmented full label="Tile style" value={th.tiles} onChange={(tiles) => set({ tiles })}
            options={[{ value: "solid", label: "Solid" }, { value: "glass", label: "Glass" }, { value: "outline", label: "Outline" }, { value: "none", label: "None" }]} />
          <div style={{ marginTop: 12 }}><Slider label="Corners" min={0} max={28} value={th.radius} onChange={(radius) => set({ radius })} /></div>
        </div>
      </section>

      <section>
        <div className="section-label">Font</div>
        <select className="select" value={th.font} onChange={(e) => set({ font: e.target.value as Theme["font"] })}>
          <option value="inter">Inter</option>
          <option value="native">System UI</option>
          <option value="rounded">Rounded</option>
          <option value="serif">Serif</option>
          <option value="mono">Mono</option>
        </select>
      </section>

      <section>
        <button className="btn btn-plain" style={{ width: "100%", justifyContent: "space-between", padding: 0 }} onClick={() => setAdvanced((a) => !a)} aria-expanded={advanced}>
          <span className="section-label" style={{ margin: 0 }}>Custom CSS</span>
          <ChevronRight size={16} style={{ transform: advanced ? "rotate(90deg)" : undefined, transition: "transform var(--dur)" }} />
        </button>
        {advanced && (
          <>
            <textarea className="textarea" rows={8} spellCheck={false} style={{ marginTop: 8 }} placeholder=".tile-top { … }"
              value={page.doc.css} onChange={(e) => page.update((d) => ({ ...d, css: e.target.value }))} />
            <p className="faint" style={{ fontSize: 12, marginTop: 6 }}>Classes: .tile, .tile-now, .tile-top, .tile-activity, .tile-stats, .tile-recent, .tile-text, .tile-divider</p>
          </>
        )}
      </section>

      <section>
        <button className="btn btn-danger" style={{ width: "100%" }} onClick={() => page.update(() => defaultDoc())}>Reset to default</button>
        <p className="faint" style={{ fontSize: 12, marginTop: 6 }}>Restores the default layout, theme and greeting. Nothing changes on your page until you press Done, and Undo brings your layout back.</p>
      </section>
    </Sheet>
  );
}

function Slider({ label, min, max, step = 1, value, onChange }: { label: string; min: number; max: number; step?: number; value: number; onChange: (v: number) => void }) {
  return (
    <label style={{ display: "grid", gridTemplateColumns: "64px 1fr", alignItems: "center", gap: 10, fontSize: 13 }}>
      <span className="muted">{label}</span>
      <input type="range" min={min} max={max} step={step} value={value} onChange={(e) => onChange(Number(e.target.value))} style={{ accentColor: "var(--accent)" }} />
    </label>
  );
}

function GreetingField({ title, note, value, placeholder, preview, onChange }: {
  title: string; note: string; value: string; placeholder: string; preview: string; onChange: (v: string) => void;
}) {
  return (
    <div className="greeting-field">
      <div className="greeting-field-head"><strong>{title}</strong><span className="faint">{note}</span></div>
      <input className="input" aria-label={`Greeting, ${title.toLowerCase()}`} placeholder={placeholder} value={value} onChange={(e) => onChange(e.target.value)} />
      <div className="greeting-preview"><span className="faint">Preview</span> {preview || <span className="faint">Nothing shown</span>}</div>
    </div>
  );
}
