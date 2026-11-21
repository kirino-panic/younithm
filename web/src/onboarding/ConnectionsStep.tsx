import { useState } from "react";
import { endpoints, type SecretMeta } from "../api";
import { KeyFile, PROVIDERS } from "../admin/Connections";
import { Group, SettingRow, Status, Switch, useData } from "../ui";
import { useDraft, type ProviderKey } from "./draft";
import { useSimulated } from "./simulate";
import { hasCopy, T, t, type CopyKey } from "./copy";
import { failures } from "./failures";

type Check = { state: "checking" } | { state: "error"; kind: string; detail?: string; fields?: string };

// The message for a failed check: refusals get advice for that service.
function errKey(kind: string, provider: string): CopyKey {
  if (kind === "rejected" || kind === "not_configured") {
    const k = `connections.err.rejected.${provider}`;
    return hasCopy(k) ? k : "connections.err.rejected";
  }
  return (["missing", "rate_limited", "timeout", "network", "badKey"].includes(kind) ? `connections.err.${kind}` : "connections.err.rejected") as CopyKey;
}

const list = (xs: string[]) => (xs.length <= 1 ? xs.join("") : `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`);

// Each service is a checkbox that opens its fields. Continue tries every ticked
// one against the real service and moves on only once all of them work. The
// details stay in the draft: they're stored when onboarding finishes.
export function useConnections() {
  const simulate = useSimulated();
  const { draft, update } = useDraft();
  const secrets = useData(() => (simulate ? Promise.resolve({ secrets: [] as SecretMeta[] }) : endpoints.secrets()), [simulate]);
  const [checks, setChecks] = useState<Partial<Record<ProviderKey, Check>>>({});
  const [busy, setBusy] = useState(false);

  // Already stored (set up in Admin before): shown as connected, left alone.
  const saved = (p: (typeof PROVIDERS)[number]) => p.fields.every(([n]) => !!secrets.data?.secrets.find((s) => s.name === n)?.set);
  const verified = (k: ProviderKey) => draft.verified.includes(k);
  const setCheck = (k: ProviderKey, c?: Check) => setChecks((cs) => ({ ...cs, [k]: c }));
  const setValue = (n: string, v: string, k: ProviderKey) =>
    update((d) => ({ ...d, values: { ...d.values, [n]: v }, verified: d.verified.filter((x) => x !== k) }));

  const checkOne = async (p: (typeof PROVIDERS)[number]): Promise<boolean> => {
    const vals = Object.fromEntries(p.fields.map(([n]) => [n, draft.values[n]?.trim() || ""]));
    // A simulated run needs no real values; the debug panel picks the outcome.
    const fail = simulate ? failures.get(`connections.${p.key}`) : undefined;
    const empty = p.fields.filter(([n]) => !vals[n]).map(([, label]) => label);
    if ((!simulate && empty.length) || fail === "missing") {
      setCheck(p.key, { state: "error", kind: "missing", fields: list(empty.length ? empty : p.fields.map(([, l]) => l)) });
      return false;
    }
    setCheck(p.key, { state: "checking" });
    if (simulate) {
      await new Promise((r) => setTimeout(r, 700));
      if (fail) {
        setCheck(p.key, { state: "error", kind: fail, detail: fail === "rejected" ? "invalid_client (simulated)" : undefined });
        return false;
      }
    } else {
      try {
        const r = await endpoints.checkConnectionWith(p.key, vals);
        if (!r.ok) {
          const kind = r.error_kind || "rejected";
          setCheck(p.key, { state: "error", kind, detail: kind === "rejected" ? r.detail : undefined });
          return false;
        }
      } catch {
        setCheck(p.key, { state: "error", kind: "network" });
        return false;
      }
    }
    setCheck(p.key);
    update((d) => ({ ...d, verified: [...d.verified.filter((x) => x !== p.key), p.key] }));
    return true;
  };

  // One at a time: MusicBrainz allows a request a second. Returns the names
  // of the services that don't work yet.
  const checkAll = async (): Promise<string[]> => {
    setBusy(true);
    const failed: string[] = [];
    for (const p of PROVIDERS) if (draft.ticked.includes(p.key) && !saved(p) && !verified(p.key) && !(await checkOne(p))) failed.push(p.name);
    setBusy(false);
    return failed;
  };

  const view = (
    <div className="groups groups-tight">
      {PROVIDERS.map((p) => {
        const stored = saved(p);
        const ok = stored || verified(p.key);
        const open = !stored && draft.ticked.includes(p.key);
        const check = checks[p.key];
        const toggle = (v: boolean) => {
          update((d) => ({
            ...d,
            ticked: v ? [...d.ticked, p.key] : d.ticked.filter((x) => x !== p.key),
            verified: v ? d.verified : d.verified.filter((x) => x !== p.key),
          }));
          if (!v) setCheck(p.key);
        };
        return (
          <Group key={p.key}>
            <div className="row-setting">
              <span>{p.name}</span>
              <span className="row-trail">
                {ok ? <Status ok label={t(stored ? "connections.connected" : "connections.works")} />
                  : check?.state === "checking" ? <span className="muted" style={{ fontSize: "var(--t-footnote)" }}><T k="connections.checking" /></span>
                  : check?.state === "error" && open ? <Status ok={false} label={t("connections.notWorking")} /> : null}
                {/* Already stored (set up in Admin): stays on here; Admin is where to remove it. */}
                <Switch label={p.name} checked={stored || open} disabled={busy || stored} onChange={toggle} />
              </span>
            </div>
            {open && p.fields.map(([n, label]) => (
              <SettingRow key={n} label={label}>
                {n === "apple.private_key"
                  ? <KeyFile label={label} hint={draft.values[n] ? t("connections.keyChosen") : ""} onRead={(v) => { setValue(n, v, p.key); setCheck(p.key); }}
                      onError={() => setCheck(p.key, { state: "error", kind: "badKey" })} />
                  : <input className="input row-input" aria-label={label} type={n === "musicbrainz.contact" ? "text" : "password"} autoComplete="off" disabled={busy}
                      placeholder={n === "musicbrainz.contact" ? t("connections.contact") : ""} value={draft.values[n] || ""}
                      onChange={(e) => setValue(n, e.target.value, p.key)} />}
              </SettingRow>
            ))}
            {open && check?.state === "error" && (
              <div className="row-setting row-note">
                <span className="error-text"><T k={errKey(check.kind, p.key)} vars={{ service: p.name, fields: check.fields || "" }} /></span>
                {check.detail && <span className="faint truncate" title={check.detail}>{check.detail}</span>}
              </div>
            )}
          </Group>
        );
      })}
    </div>
  );
  return { view, checkAll, busy };
}
