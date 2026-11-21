import { useRef, useState } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { Camera } from "lucide-react";
import { useSession } from "../session";
import { endpoints } from "../api";
import { Cover, Group, SettingRow, Switch, squareAvatar, useData, usePasswordPrompt } from "../ui";
import { useConnections } from "./ConnectionsStep";
import { useHistory } from "./HistoryStep";
import { importProblem } from "./fileCheck";
import { commitDraft, DraftCtx, emptyDraft, useDraft, type CommitResult, type Draft } from "./draft";
import { SimulateCtx, useSimulated } from "./simulate";
import { T, t } from "./copy";
import { failures } from "./failures";
import { Frame, STEPS, StepError, type Step } from "./Frame";
import { CopyField, listenBrainzUrl, tokenOk } from "./scrobble";

// First run, after the account exists. Every step only fills in a draft held
// here; the last step saves it all at once. The step lives in the URL so a
// reload stays put (the draft starts over). ?simulate=1 makes it a dry run.

// What every step passes to its Frame: where it is and how to move.
interface Nav { simulate: boolean; at: number; onSkip?: () => void; onBack?: () => void; onNext: () => void }

export function Welcome() {
  const [params, setParams] = useSearchParams();
  const simulate = params.get("simulate") === "1";
  const status = useData(() => (simulate ? Promise.resolve({ done: false }) : endpoints.onboarding()), [simulate]);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const step: Step = STEPS.find((s) => s === params.get("step")) || "profile";
  const i = STEPS.indexOf(step);
  const sim: Record<string, string> = simulate ? { simulate: "1" } : {};
  const go = (n: number) => setParams({ step: STEPS[Math.max(0, Math.min(n, STEPS.length - 1))], ...sim });

  if (!status.data) return null;
  if (status.data.done) return <Navigate to="/admin" replace />; // finished once already: Admin has it all

  const nav: Nav = {
    simulate,
    at: i + 1,
    // Skipping goes to the end, the one place where anything is saved.
    onSkip: i < STEPS.length - 1 ? () => go(STEPS.length - 1) : undefined,
    // The account exists by the profile step, so there's no going back past it.
    onBack: i > 0 ? () => go(i - 1) : undefined,
    onNext: () => go(i + 1),
  };

  return (
    <SimulateCtx.Provider value={simulate}>
    <DraftCtx.Provider value={{ draft, update: (fn) => setDraft((d) => fn(d)) }}>
      {step === "profile" && <ProfileStep nav={nav} />}
      {step === "connections" && <ConnectionsStep nav={nav} />}
      {step === "history" && <HistoryFrame nav={nav} />}
      {step === "scrobbler" && <ScrobblerFrame nav={nav} />}
      {step === "ready" && <ReadyStep nav={nav} />}
    </DraftCtx.Provider>
    </SimulateCtx.Provider>
  );
}

// Next waits for the file checks and moves on only when every import is usable.
function HistoryFrame({ nav }: { nav: Nav }) {
  const { view, checking, blocked } = useHistory();
  const [tried, setTried] = useState(false);
  return (
    <Frame {...nav} title={<T k="history.title" />} sub={<T k="history.sub" />}
      onNext={() => { setTried(true); if (!checking && !blocked) nav.onNext(); }}
      busy={checking} busyLabel="history.checkingNext"
      error={tried && blocked > 0 && (
        <StepError title={<T k="history.err.summary.title" />}><T k="history.err.summary" /></StepError>
      )}>
      {view}
    </Frame>
  );
}

// Next checks every ticked service and moves on only when all of them work.
function ConnectionsStep({ nav }: { nav: Nav }) {
  const { view, checkAll, busy } = useConnections();
  const [failed, setFailed] = useState<string[]>([]);
  return (
    <Frame {...nav} title={<T k="connections.title" />} sub={<T k="connections.sub" />}
      onNext={async () => { const f = await checkAll(); setFailed(f); if (!f.length) nav.onNext(); }}
      busy={busy} busyLabel="connections.checking"
      error={failed.length > 0 && !busy && (
        <StepError title={<T k="connections.err.summary.title" />}>
          <T k={failed.length === 1 ? "connections.err.summary.one" : "connections.err.summary.many"}
            vars={{ names: failed.length === 1 ? failed[0] : `${failed.slice(0, -1).join(", ")} and ${failed[failed.length - 1]}` }} />
        </StepError>
      )}>
      {view}
    </Frame>
  );
}

// The default Profile tile, editable in place: tap the photo or the name.
function ProfileStep({ nav }: { nav: Nav }) {
  const simulate = useSimulated();
  const { draft, update } = useDraft();
  const { me } = useSession();
  const file = useRef<HTMLInputElement>(null);
  const [bad, setBad] = useState("");
  // What's stored now (empty on a first run; a dry run always starts blank).
  const stored = useData(() => (simulate ? Promise.resolve({ display_name: "", avatar: "" }) : endpoints.profile()), [simulate]);
  const name = draft.name ?? stored.data?.display_name ?? "";
  const avatar = draft.avatar || stored.data?.avatar || "";

  const pick = async (f?: File) => {
    if (file.current) file.current.value = "";
    if (!f) return;
    setBad("");
    try {
      if (simulate && failures.on("profile.photo")) throw new Error("simulated");
      const a = await squareAvatar(f);
      update((d) => ({ ...d, avatar: a }));
    } catch {
      setBad(f.name);
    }
  };

  return (
    <Frame {...nav} title={<T k="profile.title" />} sub={<T k="profile.sub" />}
      error={bad && <StepError title={<T k="profile.badPhoto.title" />}><T k="profile.badPhoto" vars={{ file: bad }} /></StepError>}>
      <div className="tile tile-profile welcome-tile">
        <div className="profile">
          <button type="button" className="profile-avatar avatar-pick" aria-label={avatar ? "Change photo" : "Add photo"} onClick={() => file.current?.click()}>
            <Cover name={name || me?.username || "?"} src={avatar || undefined} size="100%" round />
            <span className="avatar-pick-badge"><Camera size={14} strokeWidth={2} /></span>
          </button>
          <div className="profile-text">
            <input className="profile-name profile-name-input" aria-label="Display name" maxLength={40} placeholder={me?.username || t("profile.name")} autoFocus
              value={name} onChange={(e) => update((d) => ({ ...d, name: e.target.value }))} onKeyDown={(e) => e.key === "Enter" && nav.onNext()} />
            <div className="muted num" style={{ fontSize: "var(--t-footnote)" }}><T k="profile.est" vars={{ year: new Date().getFullYear() }} /></div>
          </div>
        </div>
        <input ref={file} type="file" accept="image/*" hidden onChange={(e) => pick(e.target.files?.[0])} />
      </div>
    </Frame>
  );
}

// The three things a ListenBrainz scrobbler asks for. The token is off unless
// turned on, and then it's whatever the person types.
function ScrobblerFrame({ nav }: { nav: Nav }) {
  const { draft, update } = useDraft();
  const { me } = useSession();
  const [tried, setTried] = useState(false);
  const bad = draft.tokenOn && !tokenOk(draft.token);
  return (
    <Frame {...nav} title={<T k="scrobbler.title" />} sub={<T k="scrobbler.sub" />}
      onNext={() => { setTried(true); if (!bad) nav.onNext(); }}
      error={tried && bad && <StepError title={<T k="scrobbler.err.token.title" />}><T k="scrobbler.err.token" /></StepError>}>
      <div>
        <Group>
          <SettingRow label={<T k="scrobbler.url" />}><CopyField value={listenBrainzUrl()} label={t("scrobbler.url")} /></SettingRow>
          <SettingRow label={<T k="scrobbler.username" />}><CopyField value={me?.username || ""} label={t("scrobbler.username")} /></SettingRow>
          <SettingRow label={<T k="scrobbler.token" />}>
            <Switch label={t("scrobbler.token")} checked={draft.tokenOn} onChange={(v) => update((d) => ({ ...d, tokenOn: v }))} />
          </SettingRow>
          {draft.tokenOn && (
            <div className="row-setting">
              <input className="input token-input" aria-label={t("scrobbler.token")} placeholder={t("scrobbler.tokenPlaceholder")} autoFocus autoComplete="off" spellCheck={false}
                aria-invalid={(tried && bad) || undefined} value={draft.token} maxLength={128} onChange={(e) => update((d) => ({ ...d, token: e.target.value }))} />
            </div>
          )}
        </Group>
        <p className="faint ob-note"><T k={draft.tokenOn ? "scrobbler.tokenNote" : "scrobbler.openNote"} /></p>
      </div>
    </Frame>
  );
}

// The last page. Every step has already checked its own part, so this is
// only "you're all set" and the button that saves it all. If saving still
// fails (the server went away, say), the page says what and why, and offers
// to retry just those pieces or go on without them; nobody is trapped here.
function ReadyStep({ nav }: { nav: Nav }) {
  const simulate = useSimulated();
  const { draft } = useDraft();
  const ask = usePasswordPrompt();
  const go = useNavigate();
  const [saving, setSaving] = useState(false);
  const [done, setDone] = useState<Set<string>>(new Set());
  const [failed, setFailed] = useState<CommitResult["failed"]>([]);

  // Skipping ahead can pass the import checks; don't save an import that won't work.
  const badImports = draft.imports.some((imp) => importProblem(imp.files, imp.checks) !== null);

  const leave = () => go(simulate ? "/admin/settings?tab=debug" : "/");
  const complete = async () => {
    if (!simulate) await endpoints.finishOnboarding().catch(() => {});
    leave();
  };
  const save = async () => {
    setSaving(true);
    const r = await commitDraft(draft, ask, simulate, done);
    setDone(r.done);
    setFailed(r.failed);
    setSaving(false);
    if (!r.failed.length) await complete();
  };

  return (
    <Frame simulate={nav.simulate} at={nav.at} title={<T k="ready.title" />} sub={<T k="ready.sub" />}
      onBack={failed.length || saving ? undefined : nav.onBack}
      onNext={save} next={failed.length ? "ready.retry" : "ready.start"} busy={saving} busyLabel="ready.saving" nextDisabled={badImports}
      secondary={failed.length > 0 && !saving ? { label: "ready.skipFailed", onClick: complete } : undefined}
      error={badImports ? (
        <StepError title={<T k="history.err.summary.title" />}><T k="history.err.summary" /></StepError>
      ) : failed.length > 0 && !saving && (
        <StepError title={<T k="ready.err.title" />}>
          <T k="ready.err.intro" />
          <ul className="ob-error-list">
            {failed.map((f, k) => <li key={k}><strong>{f.label}</strong> {f.error}</li>)}
          </ul>
        </StepError>
      )} />
  );
}
