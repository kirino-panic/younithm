import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Upload, X } from "lucide-react";
import { api, endpoints, ApiError, type ImportJob } from "../api";
import { fmtDate, fmtNum } from "../format";
import { ActionRow, Group, InfoTip, SettingRow, Status, Switch, useData, useToast } from "../ui";
import { AdminHead, useQueue } from "./AdminLayout";
import { t } from "../onboarding/copy";

const ACTIVE = ["queued", "running"];

export function Imports() {
  const list = useData(() => endpoints.imports(), []);
  const [current, setCurrent] = useState<ImportJob | null>(null);
  const [err, setErr] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const [fuzzy, setFuzzy] = useFuzzyPref();
  // Per import, not remembered: a range left over from the last file would
  // quietly drop listens from the next one.
  const [range, setRange] = useState<DateRange>(NO_RANGE);

  // Keep the open job fresh while it is parsing or importing.
  useEffect(() => {
    if (!current || !ACTIVE.includes(current.status)) return;
    const t = window.setInterval(async () => {
      const j = await endpoints.importJob(current.id).catch(() => null);
      if (j) setCurrent(j);
      if (j && !ACTIVE.includes(j.status)) list.reload();
    }, 1500);
    return () => window.clearInterval(t);
  }, [current, list]);

  const [picked, setPicked] = useState<File[] | null>(null);
  const upload = async (files: File[]) => {
    if (!files.length) return;
    setErr("");
    try {
      const [first, ...rest] = files;
      let job = await endpoints.upload([first], { fuzzyConsent: fuzzy, ...(range.on ? { from: range.from, to: range.to } : {}) });
      for (const f of rest) {
        const fd = new FormData();
        fd.append("file", f);
        job = await api.post<ImportJob>(`/api/v1/admin/imports/${job.id}/files`, fd);
      }
      setCurrent(job);
      setPicked(null);
      setRange(NO_RANGE);
      list.reload();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Upload failed.");
    }
  };

  const confirm = async () => {
    if (!current) return;
    try {
      setCurrent(await endpoints.confirmImport(current.id));
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't start the import.");
    }
  };

  const jobs = list.data?.jobs || [];

  return (
    <>
      <AdminHead title="Imports" />
      <input ref={input} type="file" multiple hidden onChange={(e) => {
        const files = Array.from(e.target.files || []);
        e.target.value = "";
        if (files.length) setPicked(files);
      }} />
      <div className="groups">
        {err && <p className="error-text">{err}</p>}
        {current
          ? <JobGroup job={current} onConfirm={confirm} onClose={() => setCurrent(null)} onCancel={async () => { await endpoints.cancelImport(current.id).catch(() => {}); setCurrent(null); list.reload(); }} />
          : <>
            {picked ? (
              <Group title="Files to import">
                {picked.map((f, i) => (
                  <div key={`${f.name}-${i}`} className="row-setting">
                    <span className="truncate">{f.name}</span>
                    <button type="button" className="btn btn-icon" aria-label={`Remove ${f.name}`} onClick={() => { const n = picked.filter((_, j) => j !== i); setPicked(n.length ? n : null); }}><X size={16} /></button>
                  </div>
                ))}
                <ActionRow onClick={() => input.current?.click()}>Choose other files</ActionRow>
              </Group>
            ) : <Group><ActionRow onClick={() => input.current?.click()}><Upload size={16} />Upload files</ActionRow></Group>}
            <ImportOptions fuzzy={fuzzy} setFuzzy={setFuzzy} range={range} setRange={setRange} ready={!!picked} onStart={() => picked && upload(picked)} onCancel={() => setPicked(null)} />
          </>}
        <Matching />
        <Group title="Past imports">
          {!jobs.length ? <div className="row-setting muted">No imports yet.</div> : jobs.map((j) => (
            <button key={j.id} className="row-setting row-link" onClick={() => endpoints.importJob(j.id).then(setCurrent)}>
              <span className="list-main">
                <span className="list-title truncate">{j.filename}</span>
                <span className="list-sub">{[j.format, `${fmtNum(j.progress?.inserted || j.progress?.new || 0)} listens`, j.options?.debug_limit ? `newest ${fmtNum(j.options.debug_limit)}` : "", rangeText(j.options), fmtDate(j.created_at)].filter(Boolean).join(" · ")}</span>
              </span>
              <JobStatus job={j} />
            </button>
          ))}
        </Group>
      </div>
    </>
  );
}

function Matching() {
  const { queue: q, reloadQueue } = useQueue();
  const toast = useToast();
  const active = (q.queued || 0) + (q.running || 0);
  const total = active + (q.done || 0) + (q.failed || 0);
  const pct = total ? (((q.done || 0) + (q.failed || 0)) / total) * 100 : 0;
  const rescan = async () => { const r = await endpoints.requeue(); toast({ text: `${fmtNum(r.queued)} queued` }); reloadQueue(); };
  return (
    <Group title="Queue">
      {([["queued", "Queued"], ["running", "Running"], ["done", "Done"], ["failed", "Failed"]] as const).map(([k, l]) => (
        <SettingRow key={k} label={l}><span className="muted num">{fmtNum(q[k] || 0)}</span></SettingRow>
      ))}
      {!!q.review_pending && (
        <Link to="/admin/review" className="row-setting row-link">
          <span>Needs review</span><span className="num" style={{ color: "var(--accent)" }}>{fmtNum(q.review_pending)}</span>
        </Link>
      )}
      {active > 0 && <div className="row-setting"><div className="progress" style={{ flex: 1 }}><i style={{ width: `${pct}%` }} /></div></div>}
      <ActionRow onClick={rescan}>Rescan library</ActionRow>
    </Group>
  );
}

// "Link name matches without review": on unless turned off, remembered per
// browser. (A new key: the old one stored "off" for anyone who never chose.)
export function useFuzzyPref() {
  const [fuzzy, setFuzzy] = useState(() => readPref("imports.link_names", "1") === "1");
  useEffect(() => { writePref("imports.link_names", fuzzy ? "1" : "0"); }, [fuzzy]);
  return [fuzzy, setFuzzy] as const;
}

// Options for the import about to start. Each is remembered for the next one.
// Labels and (i) text are onboarding's, so an edit there shows here too.
// Start and Cancel stay greyed out until there's a file to import.
// on: "Custom date range" is switched on; the dates are sent only then.
type DateRange = { on: boolean; from: string; to: string };
const NO_RANGE: DateRange = { on: false, from: "", to: "" };

// "2020-01-01 – 2020-12-31", "from 2020-01-01", "until 2020-12-31", or "".
function rangeText(o?: { from?: string; to?: string }) {
  if (o?.from && o.to) return `${o.from} – ${o.to}`;
  if (o?.from) return `from ${o.from}`;
  if (o?.to) return `until ${o.to}`;
  return "";
}

function ImportOptions({ fuzzy, setFuzzy, range, setRange, ready, onStart, onCancel }: {
  fuzzy: boolean; setFuzzy: (v: boolean) => void; range: DateRange; setRange: (r: DateRange) => void;
  ready: boolean; onStart: () => void; onCancel: () => void;
}) {
  const backwards = range.on && !!range.from && !!range.to && range.to < range.from;
  const hpLabel = t("history.highPrecision");
  const ln = t("history.linkNames");
  const matching = useData(() => endpoints.matching(), []);
  const hp = !!matching.data?.high_precision;
  return (
    <Group title="Options">
      <SettingRow label={<>{hpLabel}<InfoTip label={`About ${hpLabel}`} text={t("history.highPrecisionInfo")} /></>}>
        <Switch label={hpLabel} checked={hp} onChange={async (v) => { await endpoints.setMatching(v); matching.reload(); }} />
      </SettingRow>
      <SettingRow label={<>{ln}<InfoTip label={`About ${ln}`} text={t("history.linkNamesInfo")} /></>}>
        <Switch label={ln} checked={fuzzy} onChange={setFuzzy} />
      </SettingRow>
      <SettingRow label="Custom date range">
        <Switch label="Custom date range" checked={range.on} onChange={(on) => setRange({ ...range, on })} />
      </SettingRow>
      {range.on && <>
      <SettingRow label="From">
        <input className="input" type="date" aria-label="From" style={{ width: "auto" }} value={range.from} max={range.to || undefined} onChange={(e) => setRange({ ...range, from: e.target.value })} />
      </SettingRow>
      <SettingRow label="To">
        <input className="input" type="date" aria-label="To" aria-invalid={backwards || undefined} style={{ width: "auto" }} value={range.to} min={range.from || undefined} onChange={(e) => setRange({ ...range, to: e.target.value })} />
      </SettingRow>
      </>}
      <ActionRow disabled={!ready || backwards} onClick={onStart}>Start import</ActionRow>
      <ActionRow danger disabled={!ready} onClick={onCancel}>Cancel</ActionRow>
    </Group>
  );
}

function JobStatus({ job }: { job: ImportJob }) {
  // Same look as Connections: the word, then a check or warning when it has finished.
  const map: Record<string, [string, boolean | null]> = {
    queued: ["Queued", null], running: ["Running", null], awaiting_confirmation: ["Ready", null],
    awaiting_files: ["Needs files", null], done: ["Done", true], failed: ["Failed", false], cancelled: ["Cancelled", null],
  };
  const [label, ok] = map[job.status] || [job.status, null];
  return <Status ok={ok} label={label} />;
}

function JobGroup({ job, onConfirm, onCancel, onClose }: { job: ImportJob; onConfirm: () => void; onCancel: () => void; onClose: () => void }) {
  const p = job.preview;
  const total = job.progress.parsed || job.progress.rows_seen || 0;
  const pct = total ? Math.min(100, ((job.progress.inserted + job.progress.duplicates) / total) * 100) : 0;
  const n = (v: number | undefined) => <span className="muted num">{fmtNum(v ?? 0)}</span>;
  return (
    <Group title={<span className="truncate">{job.filename}</span>} action={<JobStatus job={job} />}>
      {job.status === "awaiting_files" && <SettingRow label="Waiting for"><span className="muted">{job.missing?.join(", ")}</span></SettingRow>}
      {job.error && <div className="row-setting error-text">{job.error}</div>}
      {p && (
        <>
          <SettingRow label="New listens">{n(p.counts?.new ?? job.progress.new)}</SettingRow>
          <SettingRow label="Duplicates">{n(p.counts?.duplicates ?? job.progress.duplicates)}</SettingRow>
          <SettingRow label="Skipped">{n(p.counts?.skipped ?? job.progress.skipped)}</SettingRow>
          {(job.options?.from || job.options?.to) && <SettingRow label="Outside date range">{n(p.out_of_range)}</SettingRow>}
          <SettingRow label="Dates"><span className="muted num">{p.date_range?.earliest?.slice(0, 10) || "–"} – {p.date_range?.latest?.slice(0, 10) || "–"}</span></SettingRow>
          {p.sample?.slice(0, 5).map((r, i) => (
            <div key={i} className="row-setting">
              <span className="list-main">
                <span className="list-title truncate">{r.title}</span>
                <span className="list-sub truncate">{[r.artist, fmtDate(r.played_at)].join(" · ")}</span>
              </span>
              <span className="faint">{r.outcome}</span>
            </div>
          ))}
        </>
      )}
      {job.status === "running" && <div className="row-setting"><div className="progress" style={{ flex: 1 }}><i style={{ width: `${pct}%` }} /></div></div>}
      {job.status === "awaiting_confirmation" && <ActionRow onClick={onConfirm}>Import {fmtNum(p?.counts?.new ?? job.progress.new)} listens</ActionRow>}
      {job.status === "awaiting_confirmation" && <ActionRow danger onClick={onCancel}>Cancel</ActionRow>}
      {job.status !== "awaiting_confirmation" && <ActionRow onClick={onClose}>Done</ActionRow>}
    </Group>
  );
}

function readPref(k: string, d: string) {
  try { return localStorage.getItem(k) ?? d; } catch { return d; }
}
function writePref(k: string, v: string) {
  try { localStorage.setItem(k, v); } catch { /* private mode */ }
}
