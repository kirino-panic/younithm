import { useRef, useState } from "react";
import { Upload } from "lucide-react";
import { ActionRow, Group, Switch } from "../ui";
import { useDraft } from "./draft";
import { T, t, type CopyKey } from "./copy";
import { InfoSetting } from "./Frame";
import { checkFile, FORMAT_NAMES, importProblem, type FileCheck, type ImportProblem } from "./fileCheck";
import { useSimulated } from "./simulate";
import { failures } from "./failures";

const PROBLEM_TEXT: Record<Exclude<ImportProblem["kind"], "checking">, CopyKey> = {
  unknown: "history.err.unknown",
  network: "history.err.network",
  notMulti: "history.err.notMulti",
  appleMissing: "history.err.appleMissing",
};

// The imports to make, held in the browser: the files are uploaded and started
// only when onboarding finishes, but each one is checked as soon as it's picked
// so a file that won't work is caught here. Each pick is one import; more files
// can join it (Apple Music needs both of its CSVs in one import).
export function useHistory() {
  const simulate = useSimulated();
  const { draft, update } = useDraft();
  const pick = useRef<HTMLInputElement>(null);
  const more = useRef<HTMLInputElement>(null);
  const [adding, setAdding] = useState<number | null>(null);

  // Starts checking the files, filling in each result as it comes back.
  const check = (id: number, files: File[], from: number) => {
    files.forEach(async (f, k) => {
      const sim = simulate ? failures.get("history.file") : undefined;
      let c: FileCheck;
      if (sim === "unknown" || sim === "network") {
        await new Promise((r) => setTimeout(r, 400));
        c = { state: "error", kind: sim };
      } else {
        c = await checkFile(f);
      }
      update((d) => ({ ...d, imports: d.imports.map((i) => (i.id === id ? { ...i, checks: i.checks.map((x, j) => (j === from + k ? c : x)) } : i)) }));
    });
  };
  const add = (files: File[]) => {
    if (!files.length) return;
    const id = Date.now();
    update((d) => ({ ...d, imports: [...d.imports, { id, files, checks: files.map((): FileCheck => ({ state: "checking" })) }] }));
    check(id, files, 0);
  };
  const addTo = (id: number, files: File[]) => {
    if (!files.length) return;
    const from = draft.imports.find((i) => i.id === id)?.files.length || 0;
    update((d) => ({
      ...d,
      imports: d.imports.map((i) => (i.id === id ? { ...i, files: [...i.files, ...files], checks: [...i.checks, ...files.map((): FileCheck => ({ state: "checking" }))] } : i)),
    }));
    check(id, files, from);
  };
  const remove = (id: number) => update((d) => ({ ...d, imports: d.imports.filter((i) => i.id !== id) }));

  const problems = draft.imports.map((imp) => importProblem(imp.files, imp.checks));
  const checking = problems.some((p) => p?.kind === "checking");
  const blocked = problems.filter((p) => p && p.kind !== "checking").length;

  const view = (
    <div className="groups">
      <div>
        <Group>
          {draft.imports.map((imp, n) => {
            const problem = problems[n];
            const ok = imp.checks.find((c) => c.state === "ok") as Extract<FileCheck, { state: "ok" }> | undefined;
            return (
              <div key={imp.id} className="import-item">
                <div className="row-setting">
                  <span className="list-main">
                    <span className="list-title truncate">{imp.files.map((f) => f.name).join(", ")}</span>
                    <span className="list-sub">
                      {problem?.kind === "checking" ? <T k="history.checking" /> : <>{ok ? `${FORMAT_NAMES[ok.format] || ok.format} · ` : ""}{size(imp.files)}</>}
                    </span>
                  </span>
                  <span style={{ display: "flex", gap: 6, alignItems: "center" }}>
                    {ok?.format === "applemusic" && (
                      <button type="button" className="btn btn-plain" onClick={() => { setAdding(imp.id); more.current?.click(); }}><T k="history.addFile" /></button>
                    )}
                    <button type="button" className="btn btn-plain btn-danger" onClick={() => remove(imp.id)}><T k="common.remove" /></button>
                  </span>
                </div>
                {problem && problem.kind !== "checking" && (
                  <div className="row-setting row-note">
                    <span className="error-text"><T k={PROBLEM_TEXT[problem.kind]} vars={{ file: problem.file }} /></span>
                  </div>
                )}
              </div>
            );
          })}
          <ActionRow onClick={() => pick.current?.click()}>
            <Upload size={16} /><T k={draft.imports.length ? "history.addAnother" : "history.choose"} />
          </ActionRow>
        </Group>
        <p className="faint ob-note"><T k="history.appleNote" /></p>
      </div>
      <Group title={<T k="history.options" />}>
        <InfoSetting label="history.highPrecision" info="history.highPrecisionInfo" name={t("history.highPrecision")}>
          <Switch label={t("history.highPrecision")} checked={draft.highPrecision} onChange={(v) => update((d) => ({ ...d, highPrecision: v }))} />
        </InfoSetting>
        <InfoSetting label="history.linkNames" info="history.linkNamesInfo" name={t("history.linkNames")}>
          <Switch label={t("history.linkNames")} checked={draft.linkNames} onChange={(v) => update((d) => ({ ...d, linkNames: v }))} />
        </InfoSetting>
      </Group>
      <input ref={pick} type="file" multiple hidden onChange={(e) => { const fs = Array.from(e.target.files || []); e.target.value = ""; add(fs); }} />
      <input ref={more} type="file" multiple hidden onChange={(e) => { const fs = Array.from(e.target.files || []); e.target.value = ""; if (adding !== null) addTo(adding, fs); }} />
    </div>
  );
  return { view, checking, blocked };
}

function size(files: File[]): string {
  const b = files.reduce((n, f) => n + f.size, 0);
  const files_ = files.length === 1 ? "1 file" : `${files.length} files`;
  if (b < 1024) return `${files_}, ${b} B`;
  if (b < 1024 * 1024) return `${files_}, ${Math.round(b / 1024)} KB`;
  return `${files_}, ${(b / 1024 / 1024).toFixed(1)} MB`;
}
