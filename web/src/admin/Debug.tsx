import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { endpoints, withStepUp, ApiError } from "../api";
import { fmtNum } from "../format";
import { normalize } from "../home/doc";
import { ActionRow, Group, Modal, SettingRow, Switch, useData, usePasswordPrompt, useToast } from "../ui";

// Tools for testing and cleanup. Nothing here is needed day to day.
export function Debug() {
  const toast = useToast();
  const ask = usePasswordPrompt();
  const nav = useNavigate();
  const dbg = useData(() => endpoints.debug(), []);
  const d = dbg.data;
  const [limit, setLimit] = useState(100);
  const [confirm, setConfirm] = useState(false);
  const file = useRef<HTMLInputElement>(null);
  const [incoming, setIncoming] = useState<unknown>(null); // a layout file waiting for the go-ahead
  useEffect(() => { if (d?.import_limit) setLimit(d.import_limit); }, [d?.import_limit]);

  const save = async (p: { fake_now_playing?: boolean; import_limit?: number }) => {
    try {
      await endpoints.setDebug(p);
      dbg.reload();
    } catch (e) {
      toast({ text: e instanceof ApiError && e.message ? e.message : "Couldn't save." });
    }
  };
  const requeue = async () => {
    try {
      const r = await endpoints.requeue();
      toast({ text: `Queued ${fmtNum(r.queued)} tracks` });
    } catch {
      toast({ text: "Couldn't queue." });
    }
  };

  const exportLayout = async () => {
    try {
      const { page } = await endpoints.page();
      if (!page) { toast({ text: "No saved layout yet. Publish one first." }); return; }
      const a = document.createElement("a");
      a.href = URL.createObjectURL(new Blob([JSON.stringify(page, null, 2)], { type: "application/json" }));
      a.download = "younithm-layout.json";
      a.click();
      URL.revokeObjectURL(a.href);
    } catch {
      toast({ text: "Couldn't export." });
    }
  };
  const readLayout = async (f?: File) => {
    if (!f) return;
    try {
      const raw = JSON.parse(await f.text());
      if (!raw || typeof raw !== "object" || !Array.isArray(raw.tiles)) throw new Error("not a layout");
      setIncoming(raw);
    } catch {
      toast({ text: "That file isn't a layout." });
    }
    if (file.current) file.current.value = "";
  };
  const importLayout = async () => {
    try {
      await endpoints.savePage(normalize(incoming)); // normalize fills anything the file leaves out
      window.location.reload(); // the page reads its layout once, on load
    } catch (e) {
      setIncoming(null);
      toast({ text: e instanceof ApiError && e.message ? e.message : "Couldn't import." });
    }
  };

  if (!d) return null;
  const limited = d.import_limit > 0;
  return (
    <>
      <div className="groups">
        <Group title="Imports">
          <SettingRow label={<span style={limited ? { color: "var(--warning)" } : undefined}>Limit import size</span>}>
            <span style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <input className="input num" type="number" min={1} style={{ width: 90 }} value={limit} disabled={!limited} aria-label="Newest listens to import"
                onChange={(e) => setLimit(Math.max(1, Number(e.target.value) || 1))}
                onBlur={() => { if (limited && limit !== d.import_limit) save({ import_limit: limit }); }} />
              <Switch label="Limit import size" checked={limited} onChange={(v) => save({ import_limit: v ? limit : 0 })} />
            </span>
          </SettingRow>
          <p className="faint" style={{ fontSize: 12, padding: "0 var(--s4) var(--s3)" }}>When on, each import uses only the newest listens in the file.</p>
        </Group>

        <Group title="Onboarding">
          <ActionRow onClick={() => nav("/setup?simulate=1")}>Simulate onboarding</ActionRow>
          <p className="faint" style={{ fontSize: 12, padding: "0 var(--s4) var(--s3)" }}>Walks through first run as a new install would. Nothing is saved.</p>
        </Group>

        <Group title="Now playing">
          <SettingRow label="Fake now playing">
            <Switch label="Fake now playing" checked={d.fake_now_playing} onChange={(v) => save({ fake_now_playing: v })} />
          </SettingRow>
          <p className="faint" style={{ fontSize: 12, padding: "0 var(--s4) var(--s3)" }}>Shows a made-up track in the Now playing tile whenever nothing is really playing.</p>
        </Group>

        <Group title="Layout">
          <ActionRow onClick={exportLayout}>Export home layout…</ActionRow>
          <ActionRow onClick={() => file.current?.click()}>Import home layout…</ActionRow>
          <input ref={file} type="file" accept="application/json,.json" hidden onChange={(e) => readLayout(e.target.files?.[0])} />
          <p className="faint" style={{ fontSize: 12, padding: "0 var(--s4) var(--s3)" }}>The saved layout, theme, greeting and custom CSS as a JSON file. Your profile isn't included.</p>
        </Group>

        <Group title="Library">
          <SettingRow label="Scrobbles"><span className="muted num">{fmtNum(d.counts.scrobbles)}</span></SettingRow>
          <SettingRow label="Tracks"><span className="muted num">{fmtNum(d.counts.recordings)}</span></SettingRow>
          <SettingRow label="Albums"><span className="muted num">{fmtNum(d.counts.releases)}</span></SettingRow>
          <SettingRow label="Artists"><span className="muted num">{fmtNum(d.counts.artists)}</span></SettingRow>
          <SettingRow label="Imports"><span className="muted num">{fmtNum(d.counts.imports)}</span></SettingRow>
          <ActionRow onClick={requeue}>Re-run matching for every track</ActionRow>
          <ActionRow danger onClick={() => setConfirm(true)}>Clear all library data…</ActionRow>
        </Group>
      </div>

      {incoming !== null && (
        <Modal onClose={() => setIncoming(null)}>
          <h3 style={{ fontSize: 17 }}>Replace your home layout?</h3>
          <p className="muted" style={{ fontSize: 13 }}>The imported layout becomes your published page, and the current one is overwritten. Export it first if you want to keep it.</p>
          <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
            <button className="btn" onClick={() => setIncoming(null)}>Cancel</button>
            <button className="btn btn-primary" onClick={importLayout}>Replace</button>
          </div>
        </Modal>
      )}
      {confirm && (
        <ClearDialog onClose={() => setConfirm(false)} counts={d.counts.scrobbles} onConfirm={async () => {
          try {
            await withStepUp(() => endpoints.clearLibrary(), ask);
            setConfirm(false);
            dbg.reload();
            toast({ text: "Library cleared" });
          } catch (e) {
            toast({ text: e instanceof ApiError && e.message ? e.message : "Couldn't clear." });
          }
        }} />
      )}
    </>
  );
}

function ClearDialog({ onClose, onConfirm, counts }: { onClose: () => void; onConfirm: () => void; counts: number }) {
  const [text, setText] = useState("");
  return (
    <Modal onClose={onClose}>
      <h3 style={{ fontSize: 17 }}>Clear all library data?</h3>
      <p className="muted" style={{ fontSize: 13 }}>
        This deletes {fmtNum(counts)} scrobbles, every artist, album and track, their links and edits, import history, uploaded import files and downloaded artwork. It can't be undone.
        Your account, connections, page layout and settings are kept.
      </p>
      <input className="input" autoFocus placeholder="Type DELETE to confirm" value={text} onChange={(e) => setText(e.target.value)} />
      <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn btn-primary btn-danger" disabled={text !== "DELETE"} onClick={onConfirm}>Clear everything</button>
      </div>
    </Modal>
  );
}
