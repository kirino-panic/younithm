import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { Check, CircleAlert, Info, X } from "lucide-react";
import { hue } from "./format";

export function Cover({ name, src, size = 40, round = false, bare = false }: { name: string; src?: string; size?: number | string; round?: boolean; bare?: boolean }) {
  const [failed, setFailed] = useState<string | null>(null);
  if (src && failed === src) src = undefined; // a broken image falls back to the placeholder
  const h = hue(name || "?");
  const style = {
    width: size, height: typeof size === "number" ? size : undefined, aspectRatio: "1",
    background: src ? "var(--fill)" : `hsl(${h} 32% 34%)`,
    fontSize: typeof size === "number" ? Math.max(11, size * 0.38) : 20,
  };
  return (
    <div className={`cover${round ? " cover-round" : ""}`} style={style} aria-hidden="true">
      {src ? <img src={src} alt="" loading="lazy" onError={() => setFailed(src ?? null)} /> : bare ? null : (name || "?").trim().charAt(0).toUpperCase()}
    </div>
  );
}

export function Segmented<T extends string>({ value, options, onChange, full, label }: {
  value: T; options: { value: T; label: ReactNode }[]; onChange: (v: T) => void; full?: boolean; label?: string;
}) {
  return (
    <div className={`seg${full ? " seg-full" : ""}`} role="group" aria-label={label}>
      {options.map((o) => (
        <button key={o.value} type="button" aria-pressed={o.value === value} onClick={() => onChange(o.value)}>{o.label}</button>
      ))}
    </div>
  );
}

export function Switch({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label: string; disabled?: boolean }) {
  return <button type="button" role="switch" className="switch" aria-checked={checked} aria-label={label} disabled={disabled} onClick={() => onChange(!checked)} />;
}

export function SettingRow({ label, children }: { label: ReactNode; children: ReactNode }) {
  return <div className="row-setting"><span>{label}</span>{children}</div>;
}

// A titled group of rows, as in iOS Settings.
// tip is the header's full text, shown on hover when it is cut short.
export function Group({ title, tip, action, children }: { title?: ReactNode; tip?: string; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="group">
      {(title || action) && <div className="group-head"><h3 className="group-title" title={tip}>{title}</h3>{action}</div>}
      <div className="group-list">{children}</div>
    </section>
  );
}

// A row that is a button, in the accent colour (or red when destructive).
export function ActionRow({ children, onClick, danger, disabled }: { children: ReactNode; onClick: () => void; danger?: boolean; disabled?: boolean }) {
  return <button className={`row-setting row-action${danger ? " danger" : ""}`} disabled={disabled} onClick={onClick}>{children}</button>;
}

export function Sheet({ title, onClose, children, footer, wide, noClose }: {
  title: ReactNode; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean; noClose?: boolean; // noClose: the footer has the buttons
}) {
  useEscape(onClose, true);
  return (
    <>
      <div className="scrim" onClick={onClose} />
      <aside className={`sheet${wide ? " sheet-wide" : ""}`} role="dialog" aria-label={typeof title === "string" ? title : undefined}>
        <div className="sheet-head">{title}{!noClose && <button className="btn btn-plain btn-icon" onClick={onClose} aria-label="Close"><X size={18} /></button>}</div>
        <div className="sheet-body">{children}</div>
        {footer && <div className="sheet-foot">{footer}</div>}
      </aside>
    </>
  );
}

export function Modal({ onClose, children, wide }: { onClose: () => void; children: ReactNode; wide?: boolean }) {
  useEscape(onClose, true);
  return (
    <>
      <div className="scrim" onClick={onClose} />
      <div className="modal-wrap" onClick={onClose}><div className={`modal${wide ? " modal-wide" : ""}`} role="dialog" onClick={(e) => e.stopPropagation()}>{children}</div></div>
    </>
  );
}

// layer: a sheet or dialog. Escape closes only the topmost layer, so a dialog
// over a sheet closes on its own.
const layers: object[] = [];
export function useEscape(fn: () => void, layer = false) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    const me = {};
    if (layer) layers.push(me);
    const h = (e: KeyboardEvent) => { if (e.key === "Escape" && (!layer || layers[layers.length - 1] === me)) ref.current(); };
    window.addEventListener("keydown", h);
    return () => { window.removeEventListener("keydown", h); if (layer) layers.splice(layers.indexOf(me), 1); };
  }, [layer]);
}

// A small (i) that opens a short explanation. Click again, click elsewhere or
// press Escape to close it.
export function InfoTip({ text, label }: { text: string; label: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    const away = (e: MouseEvent) => { if (!ref.current?.contains(e.target as Node)) setOpen(false); };
    // Capture phase, so Escape closes only the tip and not the sheet behind it.
    const esc = (e: KeyboardEvent) => { if (e.key === "Escape") { e.stopPropagation(); setOpen(false); } };
    document.addEventListener("mousedown", away);
    window.addEventListener("keydown", esc, true);
    return () => { document.removeEventListener("mousedown", away); window.removeEventListener("keydown", esc, true); };
  }, [open]);
  return (
    <span ref={ref} className="info-tip">
      <button type="button" className="info-btn" aria-label={label} aria-expanded={open} onClick={() => setOpen(!open)}><Info size={14} /></button>
      {open && <span role="note" className="info-pop">{text}</span>}
    </span>
  );
}

export function Badge({ tone, children }: { tone?: "accent" | "success" | "warning" | "danger"; children: ReactNode }) {
  return <span className={`badge${tone ? ` badge-${tone}` : ""}`}>{children}</span>;
}

// A state written like the row's own text, with a coloured glyph in front.
export function Status({ ok, label }: { ok: boolean | null; label: string }) {
  const Icon = ok === false ? CircleAlert : Check;
  return (
    <span className="status">
      {label}
      {ok !== null && <Icon size={16} strokeWidth={2.4} style={{ color: ok ? "var(--success)" : "var(--danger)" }} aria-hidden />}
    </span>
  );
}

// ---------- Toast ----------

type ToastMsg = { text: string; action?: { label: string; run: () => void } };
const ToastCtx = createContext<(t: ToastMsg) => void>(() => {});
export const useToast = () => useContext(ToastCtx);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [msg, setMsg] = useState<ToastMsg | null>(null);
  const timer = useRef<number>();
  const show = useCallback((t: ToastMsg) => {
    setMsg(t);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setMsg(null), 5000);
  }, []);
  return (
    <ToastCtx.Provider value={show}>
      {children}
      {msg && (
        <div className="toast" role="status">
          {msg.text}
          {msg.action && <button onClick={() => { msg.action!.run(); setMsg(null); }}>{msg.action.label}</button>}
        </div>
      )}
    </ToastCtx.Provider>
  );
}

// ---------- Password prompt (step-up) ----------

const PromptCtx = createContext<() => Promise<string | null>>(async () => null);
export const usePasswordPrompt = () => useContext(PromptCtx);

export function PasswordPromptProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<((v: string | null) => void) | null>(null);
  const [pw, setPw] = useState("");
  const ask = useCallback(() => new Promise<string | null>((resolve) => { setPw(""); setPending(() => resolve); }), []);
  const done = (v: string | null) => { pending?.(v); setPending(null); };
  return (
    <PromptCtx.Provider value={ask}>
      {children}
      {pending && (
        <Modal onClose={() => done(null)}>
          <form onSubmit={(e) => { e.preventDefault(); done(pw); }} style={{ display: "grid", gap: 16 }}>
            <h3 style={{ fontSize: 17 }}>Confirm it's you</h3>
            <input className="input" type="password" autoFocus placeholder="Password" value={pw} onChange={(e) => setPw(e.target.value)} />
            <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
              <button type="button" className="btn" onClick={() => done(null)}>Cancel</button>
              <button className="btn btn-primary">Continue</button>
            </div>
          </form>
        </Modal>
      )}
    </PromptCtx.Provider>
  );
}

// ---------- Data hook ----------

export function useData<T>(load: () => Promise<T>, deps: unknown[] = []) {
  const [state, set] = useState<{ data?: T; error?: unknown; loading: boolean }>({ loading: true });
  const run = useCallback(() => {
    let live = true;
    set((s) => ({ ...s, loading: true }));
    load().then((data) => live && set({ data, loading: false }), (error) => live && set({ error, loading: false }));
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  useEffect(run, [run]);
  return { ...state, reload: run };
}

// Text field with suggestions. onPick gets an existing entry ({id, name}) or,
// when allowNew, the typed text as a new one ({name}).
export function Autocomplete({ search, onPick, placeholder, label, allowNew, newLabel, autoFocus }: {
  search: (q: string) => Promise<{ id: number; name: string; sub?: string }[]>;
  onPick: (v: { id?: number; name: string }) => void;
  placeholder?: string; label: string; allowNew?: boolean; newLabel?: (q: string) => string; autoFocus?: boolean;
}) {
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<{ id: number; name: string; sub?: string }[]>([]);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  useEffect(() => {
    const term = q.trim();
    if (!term) { setHits([]); return; }
    let live = true;
    const t = window.setTimeout(() => { search(term).then((h) => live && setHits(h), () => live && setHits([])); }, 150);
    return () => { live = false; window.clearTimeout(t); };
  }, [q, search]);
  const term = q.trim();
  const opts = [...hits.map((h) => ({ ...h, isNew: false })), ...(allowNew && term && !hits.some((h) => h.name.toLowerCase() === term.toLowerCase())
    ? [{ id: 0, name: term, sub: undefined as string | undefined, isNew: true }] : [])];
  const pick = (i: number) => {
    const o = opts[i];
    if (!o) return;
    onPick(o.isNew ? { name: o.name } : { id: o.id, name: o.name });
    setQ(""); setHits([]); setOpen(false); setActive(0);
  };
  return (
    <div className="ac">
      <input className="input" value={q} placeholder={placeholder} aria-label={label} role="combobox" autoFocus={autoFocus} aria-expanded={open && opts.length > 0}
        onChange={(e) => { setQ(e.target.value); setOpen(true); setActive(0); }}
        onFocus={() => setOpen(true)} onBlur={() => window.setTimeout(() => setOpen(false), 120)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") { e.preventDefault(); setActive((a) => Math.min(a + 1, opts.length - 1)); }
          else if (e.key === "ArrowUp") { e.preventDefault(); setActive((a) => Math.max(a - 1, 0)); }
          else if (e.key === "Enter") { e.preventDefault(); pick(active); }
          else if (e.key === "Escape" && open && opts.length > 0) { e.stopPropagation(); setOpen(false); }
        }} />
      {open && opts.length > 0 && (
        <div className="ac-list" role="listbox">
          {opts.map((o, i) => (
            <button key={o.isNew ? "new" : o.id} type="button" role="option" aria-selected={i === active} className="ac-item"
              onMouseDown={(e) => e.preventDefault()} onClick={() => pick(i)} onMouseEnter={() => setActive(i)}>
              <span className="truncate">{o.isNew ? (newLabel ? newLabel(o.name) : `Add “${o.name}”`) : o.name}</span>
              {o.sub && <span className="faint truncate" style={{ fontSize: 12 }}>{o.sub}</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// Crops to a centred square and scales to 256 px, so uploads stay small.
export function squareAvatar(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => {
      const side = Math.min(img.naturalWidth, img.naturalHeight);
      const c = document.createElement("canvas");
      c.width = c.height = Math.min(256, side);
      c.getContext("2d")!.drawImage(img, (img.naturalWidth - side) / 2, (img.naturalHeight - side) / 2, side, side, 0, 0, c.width, c.height);
      URL.revokeObjectURL(url);
      resolve(c.toDataURL("image/png"));
    };
    img.onerror = () => { URL.revokeObjectURL(url); reject(new Error("bad image")); };
    img.src = url;
  });
}
