import { createContext, useContext } from "react";
import { endpoints, ApiError, withStepUp } from "../api";
import { PROVIDERS } from "../admin/Connections";
import { t } from "./copy";
import { failures } from "./failures";
import type { FileCheck } from "./fileCheck";

// Everything the welcome flow collects, held in the browser until the last
// button. Nothing is sent to be kept before then, so an interrupted onboarding
// leaves no half-done setup behind (and signing in brings the flow back).
// Connection details are only ever checked, never stored, until the end.
export type ProviderKey = (typeof PROVIDERS)[number]["key"];

export interface Draft {
  name: string | null; // null: unchanged
  avatar: string; // data: URL, "" for none
  ticked: ProviderKey[];
  values: Record<string, string>; // connection fields as typed
  verified: ProviderKey[]; // checked against the service with these values
  highPrecision: boolean;
  linkNames: boolean;
  imports: { id: number; files: File[]; checks: FileCheck[] }[]; // checks[i] is files[i], checked when picked
  tokenOn: boolean; // scrobblers must send a token
  token: string; // the token they send, chosen by the person
}

export const emptyDraft: Draft = {
  name: null, avatar: "", ticked: [], values: {}, verified: [],
  highPrecision: true, linkNames: true, imports: [], tokenOn: false, token: "",
};

export interface DraftApi { draft: Draft; update: (fn: (d: Draft) => Draft) => void }
export const DraftCtx = createContext<DraftApi>({ draft: emptyDraft, update: () => {} });
export const useDraft = () => useContext(DraftCtx);


// Why a piece couldn't be saved, in words that say what to do next.
export function explain(e: unknown): string {
  if (!(e instanceof ApiError)) return t("ready.err.network");
  switch (e.code) {
    case "step_up_required": return t("ready.err.password");
    case "invalid_credentials": return t("ready.err.wrong_password");
    case "too_large": return t("ready.err.too_large");
    case "unknown_format": return t("ready.err.unknown_format", { detail: e.message });
    case "bad_token": return t("ready.err.bad_token");
    case "internal": return t("ready.err.internal");
  }
  return e.status >= 500 ? t("ready.err.internal") : t("ready.err.other", { detail: e.message });
}

// What each piece fails with in a simulated run, to show a realistic message.
const SIMULATED: Record<string, ApiError> = {
  "ready.profile": new ApiError(500, "internal"),
  "ready.matching": new ApiError(500, "internal"),
  "ready.connection": new ApiError(403, "step_up_required"),
  "ready.scrobbling": new ApiError(500, "internal"),
  "ready.import": new ApiError(422, "unknown_format", "unrecognised CSV header (simulated). Supported: Spotify extended history, Apple Music, ListenBrainz, Last.fm."),
};

export interface CommitResult {
  failed: { label: string; error: string }[];
  done: Set<string>; // ids of the pieces now saved, so a retry skips them
}

// Saves the whole draft, in order. A piece that fails doesn't stop the rest;
// it's reported, and calling again with the returned done set retries only the
// failed pieces. Onboarding isn't marked finished here: the caller does that
// once everything is saved (or the person chooses to go on without it). A
// simulated run goes through the same list without sending anything, failing
// where the debug panel says to.
export async function commitDraft(d: Draft, ask: () => Promise<string | null>, simulate: boolean, done: Set<string>): Promise<CommitResult> {
  type Job = { id: string; label: string; kind: string; run: () => Promise<void> };
  const jobs: Job[] = [];
  const step = <T,>(fn: () => Promise<T>) => withStepUp(fn, ask);

  if ((d.name !== null && d.name.trim()) || d.avatar) {
    jobs.push({ id: "profile", label: t("ready.task.profile"), kind: "ready.profile", run: async () => {
      await endpoints.saveProfile({ ...(d.name !== null ? { display_name: d.name.trim() } : {}), ...(d.avatar ? { avatar: d.avatar } : {}) });
    } });
  }
  jobs.push({ id: "matching", label: t("ready.task.matching"), kind: "ready.matching", run: async () => {
    await endpoints.setMatching(d.highPrecision);
    try { localStorage.setItem("imports.link_names", d.linkNames ? "1" : "0"); } catch { /* private mode */ }
  } });
  for (const p of PROVIDERS) {
    if (!d.verified.includes(p.key)) continue;
    jobs.push({ id: `conn:${p.key}`, label: p.name, kind: "ready.connection", run: async () => {
      for (const [n] of p.fields) await step(() => endpoints.setSecret(n, d.values[n].trim()));
    } });
  }
  jobs.push({ id: "scrobbling", label: t("ready.task.scrobbling"), kind: "ready.scrobbling", run: async () => {
    await step(() => endpoints.setScrobbling(d.tokenOn, d.tokenOn ? d.token.trim() : ""));
  } });
  for (const imp of d.imports) {
    jobs.push({ id: `import:${imp.id}`, label: imp.files.map((f) => f.name).join(", "), kind: "ready.import", run: async () => {
      const [first, ...rest] = imp.files;
      let job = await endpoints.upload([first], { fuzzyConsent: d.linkNames });
      for (const f of rest) job = await endpoints.addImportFile(job.id, f);
      await step(() => endpoints.confirmImportWhenReady(job.id));
    } });
  }

  const now = new Set(done);
  const failed: CommitResult["failed"] = [];
  for (const j of jobs) {
    if (now.has(j.id)) continue;
    try {
      if (simulate) {
        await new Promise((r) => setTimeout(r, 250));
        if (failures.on(j.kind)) throw SIMULATED[j.kind];
      } else {
        await j.run();
      }
      now.add(j.id);
    } catch (e) {
      failed.push({ label: j.label, error: explain(e) });
    }
  }
  return { failed, done: now };
}
