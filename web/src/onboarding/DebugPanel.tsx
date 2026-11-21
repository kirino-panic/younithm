import { useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { ChevronDown, ChevronUp, LogOut } from "lucide-react";
import { PROVIDERS } from "../admin/Connections";
import { Segmented, Switch } from "../ui";
import { ACCOUNT_FAILURES, CONNECTION_FAILURES, FAILURES, failures, useFailures } from "./failures";

// Floats over every screen of a simulated onboarding: jump to any screen,
// and switch on failures.
const PAGES = [
  { id: "splash", label: "Welcome", to: "/setup?simulate=1" },
  { id: "account", label: "Account", to: "/setup?simulate=1&page=account" },
  { id: "profile", label: "Profile", to: "/welcome?simulate=1&step=profile" },
  { id: "connections", label: "Connections", to: "/welcome?simulate=1&step=connections" },
  { id: "history", label: "History", to: "/welcome?simulate=1&step=history" },
  { id: "scrobbler", label: "Scrobbler", to: "/welcome?simulate=1&step=scrobbler" },
  { id: "ready", label: "All set", to: "/welcome?simulate=1&step=ready" },
] as const;
type PageId = (typeof PAGES)[number]["id"];

const FAIL_LABEL: Record<string, string> = {
  missing: "Missing field", rejected: "Refused", rate_limited: "Busy", timeout: "No answer", network: "Network error",
  username_length: "Bad username", password_short: "Password too short", password_long: "Password too long",
  setup_done: "Already set up", unknown: "Not an export", internal: "Server error",
};

function currentPage(path: string, q: URLSearchParams): PageId {
  if (path.startsWith("/setup")) return q.get("page") === "account" ? "account" : "splash";
  return (PAGES.find((p) => p.id === q.get("step"))?.id as PageId) || "profile";
}

const OPEN_KEY = "onboarding.debug.open";

export function DebugPanel() {
  useFailures();
  const loc = useLocation();
  const nav = useNavigate();
  const page = currentPage(loc.pathname, new URLSearchParams(loc.search));
  const [open, setOpenState] = useState(() => { try { return sessionStorage.getItem(OPEN_KEY) !== "0"; } catch { return true; } });
  const setOpen = (v: boolean) => { setOpenState(v); try { sessionStorage.setItem(OPEN_KEY, v ? "1" : "0"); } catch { /* private mode */ } };
  const [tab, setTab] = useState<"pages" | "failures">("pages");

  const exit = () => {
    failures.clear();
    nav("/admin/settings?tab=debug");
  };

  if (!open) {
    return (
      <button className="debug-pill" onClick={() => setOpen(true)} aria-label="Open debug panel">
        Debug{failures.count() > 0 && ` · ${failures.count()} failing`}<ChevronUp size={14} />
      </button>
    );
  }

  return (
    <aside className="debug-panel" aria-label="Onboarding debug panel">
      <header className="debug-head">
        <strong>Debug</strong>
        <span style={{ flex: 1 }} />
        <button className="btn btn-plain" onClick={exit}><LogOut size={14} />Exit</button>
        <button className="btn btn-icon" aria-label="Hide debug panel" onClick={() => setOpen(false)}><ChevronDown size={16} /></button>
      </header>
      <Segmented full label="Debug section" value={tab} onChange={setTab} options={[
        { value: "pages", label: "Pages" },
        { value: "failures", label: failures.count() ? `Failures (${failures.count()})` : "Failures" },
      ]} />

      <div className="debug-body">
        {tab === "pages" && (
          <div className="debug-pages">
            {PAGES.map((p) => (
              <button key={p.id} className={`btn${p.id === page ? " btn-primary" : ""}`} onClick={() => nav(p.to)}>{p.label}</button>
            ))}
          </div>
        )}

        {tab === "failures" && (
          <>
            <DebugGroup title="Account">
              <FailSelect k="account.create" label="Creating it" options={ACCOUNT_FAILURES} />
            </DebugGroup>
            <DebugGroup title="Profile">
              {FAILURES.profile.map((f) => <FailSwitch key={f.key} k={f.key} label={f.label} />)}
            </DebugGroup>
            <DebugGroup title="Connections check">
              {PROVIDERS.map((p) => <FailSelect key={p.key} k={`connections.${p.key}`} label={p.name} options={CONNECTION_FAILURES} />)}
            </DebugGroup>
            <DebugGroup title="Import files">
              <FailSelect k="history.file" label="Checking a file" options={["unknown", "network"]} />
            </DebugGroup>
            <DebugGroup title="Saving (All set page)">
              {FAILURES.ready.map((f) => <FailSwitch key={f.key} k={f.key} label={f.label} />)}
            </DebugGroup>
            {failures.count() > 0 && <button className="btn" onClick={failures.clear}>Clear all failures</button>}
          </>
        )}

      </div>
    </aside>
  );
}

function DebugGroup({ title, children }: { title: string; children: React.ReactNode }) {
  return <section className="debug-group"><h4>{title}</h4>{children}</section>;
}

function FailSelect({ k, label, options }: { k: string; label: string; options: readonly string[] }) {
  return (
    <label className="debug-row">
      <span>{label}</span>
      <select className="input debug-select" value={failures.get(k) || ""} onChange={(e) => failures.setValue(k, e.target.value)}>
        <option value="">Works</option>
        {options.map((o) => <option key={o} value={o}>{FAIL_LABEL[o] || o}</option>)}
      </select>
    </label>
  );
}

function FailSwitch({ k, label }: { k: string; label: string }) {
  return (
    <label className="debug-row">
      <span>{label}</span>
      <Switch label={label} checked={failures.on(k)} onChange={(v) => failures.toggle(k, v)} />
    </label>
  );
}
