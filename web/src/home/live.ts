import { useEffect, useState } from "react";
import { endpoints, withBase, type NowPlaying } from "../api";

// One now-playing stream per page, shared by every tile. The browser
// reconnects a dropped stream by itself; while it is down (or the server
// refuses it) the state is polled instead, and a reconnect fetches it afresh.

type State = { np: NowPlaying | null; at: number }; // at: when np arrived (ms)

let state: State = { np: null, at: 0 };
const npSubs = new Set<(s: State) => void>();
const scrobbleSubs = new Set<() => void>();
let es: EventSource | null = null;
let poll: number | undefined;

const POLL_MS = 15000;

function set(np: NowPlaying) {
  state = { np, at: Date.now() };
  npSubs.forEach((f) => f(state));
}

const fetchNow = () => endpoints.nowPlaying().then(set).catch(() => { if (!state.np) set({ playing: false }); });

function startPolling() {
  if (poll === undefined) poll = window.setInterval(fetchNow, POLL_MS);
}
function stopPolling() {
  window.clearInterval(poll);
  poll = undefined;
}

function connect() {
  fetchNow();
  if (typeof EventSource === "undefined") { startPolling(); return; }
  es = new EventSource(withBase("/api/v1/now-playing/stream"));
  es.addEventListener("now-playing", (e) => { try { set(JSON.parse((e as MessageEvent).data)); } catch { /* ignore */ } });
  es.addEventListener("scrobble", () => scrobbleSubs.forEach((f) => f()));
  es.onopen = () => { stopPolling(); fetchNow(); };
  // CONNECTING: the browser retries on its own. CLOSED: it gave up (an error
  // status such as a hidden section), so try again later from scratch.
  es.onerror = () => {
    startPolling();
    if (es?.readyState === EventSource.CLOSED) {
      es = null;
      window.setTimeout(() => { if (npSubs.size + scrobbleSubs.size > 0 && !es) connect(); }, 60000);
    }
  };
}

function retain() {
  if (!es && poll === undefined) connect();
}
function release() {
  if (npSubs.size + scrobbleSubs.size > 0) return;
  es?.close();
  es = null;
  stopPolling();
}

/** The current now-playing state, kept live. */
export function useNowPlaying(): State {
  const [s, setS] = useState(state);
  useEffect(() => {
    npSubs.add(setS);
    retain();
    setS(state);
    return () => { npSubs.delete(setS); release(); };
  }, []);
  return s;
}

/** Calls f whenever new listens are stored (a list showing them is stale). */
export function useOnScrobble(f: () => void) {
  useEffect(() => {
    scrobbleSubs.add(f);
    retain();
    return () => { scrobbleSubs.delete(f); release(); };
  }, [f]);
}

/** Milliseconds now, updated every `ms` while `on`. */
export function useNow(on: boolean, ms = 1000) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!on) return;
    const id = window.setInterval(() => setNow(Date.now()), ms);
    return () => window.clearInterval(id);
  }, [on, ms]);
  return now;
}
