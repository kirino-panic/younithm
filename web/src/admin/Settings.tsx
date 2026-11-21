import { ArrowDown, ArrowUp, Pencil } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { endpoints, ApiError, type NamingPref, type Profile } from "../api";
import { useSession } from "../session";
import { useSearchParams } from "react-router-dom";
import { Cover, Group, InfoTip, Modal, Segmented, SettingRow, Switch, squareAvatar, useData, useToast } from "../ui";
import { AdminHead } from "./AdminLayout";
import { Debug } from "./Debug";

export function Settings() {
  const [params, setParams] = useSearchParams();
  const flags = useData(() => endpoints.visibility(), []);
  const debugOn = !!flags.data?.debug;
  const tab = debugOn && params.get("tab") === "debug" ? "debug" : "general";
  return (
    <>
      <AdminHead title="Settings">
        {debugOn && (
          <Segmented label="Settings section" value={tab} onChange={(v) => setParams(v === "general" ? {} : { tab: v })}
            options={[{ value: "general", label: "General" }, { value: "debug", label: "Debug" }]} />
        )}
      </AdminHead>
      {tab === "debug" ? <Debug /> : <General />}
    </>
  );
}

function General() {
  const { me } = useSession();
  const names = useData(() => endpoints.naming(), []);
  const langs = useData(() => endpoints.languages(), []);
  const profile = useData(() => endpoints.profile(), []);
  return (
    <>
      <div className="groups">
        <Group title="Account">
          <AccountSetting username={me?.username || ""} />
        </Group>
        <Group title="Profile">
          {profile.data && <ProfileSetting profile={profile.data} onSaved={profile.reload} />}
        </Group>
        <Group title="Metadata">
          {names.data && <NamingSetting pref={names.data} onSaved={names.reload} />}
          {langs.data && <LanguagesSetting languages={langs.data.languages} onSaved={langs.reload} />}
        </Group>
      </div>
    </>
  );
}

const ACCOUNT_ERRORS: Record<string, string> = {
  invalid_credentials: "Wrong current password.",
  username_taken: "That username is taken.",
};

function AccountSetting({ username }: { username: string }) {
  const [editing, setEditing] = useState<"username" | "password" | null>(null);
  return (
    <>
      <SettingRow label="Username">
        <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <span className="muted">{username}</span>
          <button className="btn btn-plain btn-icon" aria-label="Change username" onClick={() => setEditing("username")}><Pencil size={15} /></button>
        </span>
      </SettingRow>
      <SettingRow label="Password">
        <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <span className="muted" aria-hidden>••••••••</span>
          <button className="btn btn-plain btn-icon" aria-label="Change password" onClick={() => setEditing("password")}><Pencil size={15} /></button>
        </span>
      </SettingRow>
      {editing && <AccountDialog kind={editing} username={username} onClose={() => setEditing(null)} />}
    </>
  );
}

// Both changes ask for the current password. A new password can also sign out
// every other device, for when it may have leaked.
function AccountDialog({ kind, username, onClose }: { kind: "username" | "password"; username: string; onClose: () => void }) {
  const { refresh } = useSession();
  const toast = useToast();
  const [name, setName] = useState(username);
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [signOutOthers, setSignOutOthers] = useState(false);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const isName = kind === "username";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!isName && next !== confirm) { setErr("The new passwords don't match."); return; }
    setErr("");
    setBusy(true);
    try {
      if (isName) {
        await endpoints.changeUsername(current, name);
        await refresh();
        toast({ text: "Username changed" });
      } else {
        await endpoints.changePassword(current, next, signOutOthers);
        toast({ text: signOutOthers ? "Password changed. Other devices were signed out." : "Password changed" });
      }
      onClose();
    } catch (x) {
      setErr(x instanceof ApiError ? ACCOUNT_ERRORS[x.code] || x.message : "Couldn't save.");
      setBusy(false);
    }
  };

  const ready = current && (isName ? name.trim() && name.trim() !== username : next && confirm);
  return (
    <Modal onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 12 }}>
        <h3 style={{ fontSize: 17 }}>{isName ? "Change username" : "Change password"}</h3>
        {/* Lets password managers attach the change to the right account. */}
        {!isName && <input type="text" autoComplete="username" value={username} readOnly hidden />}
        {isName && <input className="input" aria-label="New username" autoComplete="username" maxLength={64} autoFocus value={name} onChange={(e) => setName(e.target.value)} />}
        <input className="input" type="password" aria-label="Current password" autoComplete="current-password" placeholder="Current password" autoFocus={!isName} value={current} onChange={(e) => setCurrent(e.target.value)} />
        {!isName && (
          <>
            <input className="input" type="password" aria-label="New password" autoComplete="new-password" placeholder="New password (10+ characters)" value={next} onChange={(e) => setNext(e.target.value)} />
            <input className="input" type="password" aria-label="Confirm new password" autoComplete="new-password" placeholder="Confirm new password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
            <label style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12, fontSize: 14 }}>
              <span>Sign out other devices</span>
              <Switch label="Sign out other devices" checked={signOutOthers} onChange={setSignOutOthers} />
            </label>
          </>
        )}
        {err && <p className="error-text" role="alert">{err}</p>}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 4 }}>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" disabled={!ready || busy}>Save</button>
        </div>
      </form>
    </Modal>
  );
}

const SOURCE_LABELS: Record<string, string> = { spotify: "Spotify", apple: "Apple Music", musicbrainz: "MusicBrainz", scrobbled: "As scrobbled" };
const MODES = [["default", "Default"], ["spotify", "Spotify"], ["apple", "Apple Music"], ["musicbrainz", "MusicBrainz"], ["scrobbled", "As scrobbled"], ["custom", "Custom"]];

// Which wording is shown first. A source with no name for something is
// skipped, and a name set by hand always wins.
function NamingSetting({ pref, onSaved }: { pref: NamingPref; onSaved: () => void }) {
  const toast = useToast();
  const save = async (mode: string, order?: string[]) => {
    try {
      await endpoints.setNaming(mode, order);
      onSaved();
    } catch (e) {
      toast({ text: e instanceof ApiError ? e.message : "Couldn't save." });
    }
  };
  const move = (i: number, d: number) => {
    const o = [...pref.order];
    [o[i], o[i + d]] = [o[i + d], o[i]];
    save("custom", o);
  };
  return (
    <>
      <SettingRow label={<>Preferred display metadata source<InfoTip label="About the preferred display metadata source" text="Changes what source is prioritized in the UI. This doesn't affect your library." /></>}>
        <select className="select" style={{ width: 180 }} aria-label="Preferred display metadata source" value={pref.mode}
          onChange={(e) => save(e.target.value, e.target.value === "custom" ? pref.order : undefined)}>
          {MODES.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </select>
      </SettingRow>
      {pref.mode === "custom" && (
        <div className="priority-list" aria-label="Priority">
          {pref.order.map((src, i) => (
            <div key={src} className="row-setting">
              <span className="faint num" style={{ width: 16 }}>{i + 1}</span>
              <span className="grow">{SOURCE_LABELS[src] || src}</span>
              <span style={{ display: "flex" }}>
              <button className="btn btn-icon btn-plain" aria-label={`Move ${SOURCE_LABELS[src]} up`} disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={15} /></button>
              <button className="btn btn-icon btn-plain" aria-label={`Move ${SOURCE_LABELS[src]} down`} disabled={i === pref.order.length - 1} onClick={() => move(i, 1)}><ArrowDown size={15} /></button>
              </span>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

const LANG_RE = /^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/;

// Languages names are fetched and saved in, in display priority. Saves by
// itself shortly after typing stops; a code that isn't valid is flagged and
// not sent.
function LanguagesSetting({ languages, onSaved }: { languages: string[]; onSaved: () => void }) {
  const saved = languages.join(", ");
  const [text, setText] = useState(saved);
  const [error, setError] = useState("");
  useEffect(() => setText(saved), [saved]);
  const tags = text.split(",").map((t) => t.trim()).filter(Boolean);
  const bad = tags.find((t) => !LANG_RE.test(t));

  useEffect(() => {
    if (tags.join(", ") === saved) { setError(""); return; }
    if (bad) { setError(`"${bad}" isn't a language code (try en, ja or zh-Hant)`); return; }
    if (tags.length > 5) { setError("At most 5 languages"); return; }
    setError("");
    const id = setTimeout(async () => {
      try {
        await endpoints.setLanguages(tags);
        onSaved();
      } catch (e) {
        setError(e instanceof ApiError && e.message ? e.message : "Couldn't save.");
      }
    }, 700);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text]);

  return (
    <>
      <SettingRow label={<>Preferred languages<InfoTip label="About preferred languages" text="Specifies what language(s) should be fetched and saved for any given track. Listed in order of display priority." /></>}>
        <input className="input" style={{ width: 180, ...(error ? { borderColor: "var(--danger)" } : {}) }} aria-label="Preferred languages" aria-invalid={!!error}
          autoComplete="off" spellCheck={false} placeholder="en" value={text} onChange={(e) => setText(e.target.value)} />
      </SettingRow>
      {error && <div className="row-setting"><span className="error-text" role="alert">{error}</span></div>}
    </>
  );
}


// Same pattern as the account rows: the value, then a pencil that opens a
// small dialog.
function ProfileSetting({ profile, onSaved }: { profile: Profile; onSaved: () => void }) {
  const [editing, setEditing] = useState<"name" | "avatar" | null>(null);
  return (
    <>
      <SettingRow label="Display name">
        <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <span className={profile.display_name ? "muted" : "faint"}>{profile.display_name || "Not set"}</span>
          <button className="btn btn-plain btn-icon" aria-label="Change display name" onClick={() => setEditing("name")}><Pencil size={15} /></button>
        </span>
      </SettingRow>
      <SettingRow label="Avatar">
        <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <Cover name={profile.display_name || "?"} src={profile.avatar || undefined} size={32} round />
          <button className="btn btn-plain btn-icon" aria-label="Change avatar" onClick={() => setEditing("avatar")}><Pencil size={15} /></button>
        </span>
      </SettingRow>
      {editing === "name" && <NameDialog profile={profile} onSaved={onSaved} onClose={() => setEditing(null)} />}
      {editing === "avatar" && <AvatarDialog profile={profile} onSaved={onSaved} onClose={() => setEditing(null)} />}
    </>
  );
}

function NameDialog({ profile, onSaved, onClose }: { profile: Profile; onSaved: () => void; onClose: () => void }) {
  const [name, setName] = useState(profile.display_name);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await endpoints.saveProfile({ display_name: name });
      onSaved();
      onClose();
    } catch (x) {
      setErr(x instanceof ApiError && x.message ? x.message : "Couldn't save.");
      setBusy(false);
    }
  };
  return (
    <Modal onClose={onClose}>
      <form onSubmit={submit} style={{ display: "grid", gap: 12 }}>
        <h3 style={{ fontSize: 17 }}>Display name</h3>
        <input className="input" aria-label="Display name" maxLength={40} placeholder="Your name" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
        {err && <p className="error-text" role="alert">{err}</p>}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 4 }}>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" disabled={busy || name.trim() === profile.display_name}>Save</button>
        </div>
      </form>
    </Modal>
  );
}

// Picking an image saves it straight away; Remove clears it.
function AvatarDialog({ profile, onSaved, onClose }: { profile: Profile; onSaved: () => void; onClose: () => void }) {
  const toast = useToast();
  const file = useRef<HTMLInputElement>(null);
  const save = async (avatar: string) => {
    try {
      await endpoints.saveProfile({ avatar });
      onSaved();
      onClose();
    } catch (e) {
      toast({ text: e instanceof ApiError && e.message ? e.message : "Couldn't save." });
    }
  };
  const pick = async (f?: File) => {
    if (!f) return;
    try {
      await save(await squareAvatar(f));
    } catch {
      toast({ text: "Couldn't read that image." });
    }
    if (file.current) file.current.value = "";
  };
  return (
    <Modal onClose={onClose}>
      <div style={{ display: "grid", gap: 16, justifyItems: "center" }}>
        <h3 style={{ fontSize: 17, justifySelf: "start" }}>Avatar</h3>
        <Cover name={profile.display_name || "?"} src={profile.avatar || undefined} size={96} round />
        <div style={{ display: "flex", gap: 8, justifySelf: "stretch", justifyContent: "flex-end" }}>
          {profile.avatar && <button className="btn btn-plain btn-danger" style={{ marginRight: "auto" }} onClick={() => save("")}>Remove</button>}
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" onClick={() => file.current?.click()}>{profile.avatar ? "Choose new image" : "Choose image"}</button>
        </div>
        <input ref={file} type="file" accept="image/*" hidden onChange={(e) => pick(e.target.files?.[0])} />
      </div>
    </Modal>
  );
}
