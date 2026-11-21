import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { endpoints, type Range } from "../api";
import { defaultDoc, normalize, resolvedMode, themeVars, type PageDoc } from "./doc";
import { useResolvedAccent } from "./accent";

interface PageState {
  doc: PageDoc;
  loaded: boolean; // the saved page has been fetched (or found missing)
  editing: boolean;
  dirty: boolean;
  range: Range;
  setRange: (r: Range) => void;
  startEditing: () => void;
  update: (fn: (d: PageDoc) => PageDoc) => void;
  undo: () => void;
  canUndo: boolean;
  publish: () => Promise<void>;
  discard: () => void;
  accent: string; // the accent in use, with "auto" resolved
}

const Ctx = createContext<PageState | null>(null);
export const usePage = () => useContext(Ctx)!;

export function PageProvider({ children }: { children: ReactNode }) {
  const [saved, setSaved] = useState<PageDoc>(defaultDoc);
  const [doc, setDoc] = useState<PageDoc>(saved);
  const [history, setHistory] = useState<PageDoc[]>([]);
  const [editing, setEditing] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [range, setRange] = useState<Range>("week");

  useEffect(() => {
    endpoints.page().then((r) => {
      if (r.page) {
        const d = normalize(r.page);
        setSaved(d);
        setDoc(d);
      }
    }).catch(() => {}).finally(() => setLoaded(true));
  }, []);

  const update = useCallback((fn: (d: PageDoc) => PageDoc) => {
    setDoc((d) => {
      setHistory((h) => [...h.slice(-49), d]);
      return fn(d);
    });
  }, []);

  const undo = useCallback(() => {
    setHistory((h) => {
      if (!h.length) return h;
      setDoc(h[h.length - 1]);
      return h.slice(0, -1);
    });
  }, []);

  const publish = async () => {
    await endpoints.savePage(doc);
    setSaved(doc);
    setHistory([]);
    setEditing(false);
  };
  const discard = () => { setDoc(saved); setHistory([]); setEditing(false); };

  const mode = resolvedMode(doc.theme);
  const accent = useResolvedAccent(doc.theme, mode === "light");

  const value = useMemo<PageState>(() => ({
    doc, loaded, accent, editing, dirty: doc !== saved, range, setRange,
    startEditing: () => setEditing(true),
    update, undo, canUndo: history.length > 0, publish, discard,
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }), [doc, saved, loaded, editing, range, history.length, accent]);

  // Mode and accent apply app-wide.
  useEffect(() => {
    const root = document.documentElement;
    root.dataset.mode = mode;
    root.style.setProperty("--accent", accent);
  }, [mode, accent]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

// Public pages: background, tile style, font and custom CSS.
export function PublicTheme() {
  const { doc } = usePage();
  useEffect(() => {
    const root = document.documentElement;
    const vars = themeVars(doc.theme);
    Object.entries(vars).forEach(([k, v]) => root.style.setProperty(k, v));
    let el = document.getElementById("custom-css") as HTMLStyleElement | null;
    if (!el) {
      el = document.createElement("style");
      el.id = "custom-css";
      document.head.appendChild(el);
    }
    el.textContent = doc.css;
    return () => {
      Object.keys(vars).forEach((k) => root.style.removeProperty(k));
      if (el) el.textContent = "";
    };
  }, [doc.theme, doc.css]);
  return <div className="backdrop" aria-hidden="true" />;
}
