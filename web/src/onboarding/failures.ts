import { useSyncExternalStore } from "react";

// Failures the debug panel can switch on for a simulated run, so every error
// state can be seen without breaking anything real. Kept for the tab's life.
export const CONNECTION_FAILURES = ["missing", "rejected", "rate_limited", "timeout", "network"] as const;

// Every way account creation can fail, as the server (or the browser) reports it.
export const ACCOUNT_FAILURES = ["username_length", "password_short", "password_long", "setup_done", "network", "internal"] as const;

export const FAILURES = {
  profile: [{ key: "profile.photo", label: "Photo can't be read" }],
  ready: [
    { key: "ready.profile", label: "Profile doesn't save" },
    { key: "ready.matching", label: "Matching options don't save" },
    { key: "ready.connection", label: "Connections don't save" },
    { key: "ready.scrobbling", label: "Scrobbler token doesn't save" },
    { key: "ready.import", label: "Imports can't start" },
  ],
} as const;

const KEY = "onboarding.failures";
let state: Record<string, string> = read();
let version = 0;
const subs = new Set<() => void>();

function read(): Record<string, string> {
  try { return JSON.parse(sessionStorage.getItem(KEY) || "{}"); } catch { return {}; }
}
function set(next: Record<string, string>) {
  state = next;
  version++;
  try { sessionStorage.setItem(KEY, JSON.stringify(state)); } catch { /* private mode */ }
  subs.forEach((f) => f());
}

export const failures = {
  on: (k: string) => !!state[k],
  get: (k: string): string | undefined => state[k],
  toggle: (k: string, v: boolean) => { const n = { ...state }; if (v) n[k] = "1"; else delete n[k]; set(n); },
  setValue: (k: string, v: string) => { const n = { ...state }; if (v) n[k] = v; else delete n[k]; set(n); },
  count: () => Object.keys(state).length,
  clear: () => set({}),
};

export const useFailures = () =>
  useSyncExternalStore((f) => { subs.add(f); return () => { subs.delete(f); }; }, () => version);
