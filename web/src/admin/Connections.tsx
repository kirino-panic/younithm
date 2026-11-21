import { useRef, useState } from "react";
import { endpoints, withStepUp, ApiError, type SecretMeta } from "../api";
import { ActionRow, Group, SettingRow, Status, Switch, useData, usePasswordPrompt, useToast } from "../ui";
import { useSession } from "../session";
import { CopyField, listenBrainzUrl, tokenOk } from "../onboarding/scrobble";
import { AdminHead } from "./AdminLayout";

export const PROVIDERS = [
  { key: "musicbrainz", name: "MusicBrainz", fields: [["musicbrainz.contact", "Contact"]] },
  { key: "spotify", name: "Spotify", fields: [["spotify.client_id", "Client ID"], ["spotify.client_secret", "Client secret"]] },
  { key: "apple", name: "Apple Music", fields: [["apple.team_id", "Team ID"], ["apple.key_id", "Key ID"], ["apple.private_key", ".p8 key"]] },
] as const;

export function Connections() {
  return (
    <>
      <AdminHead title="Connections" />
      <div className="groups">
        <Scrobbling />
        <Providers />
      </div>
    </>
  );
}

// The catalogue providers, each with its own Connect.
function Providers() {
  const secrets = useData(() => endpoints.secrets(), []);
  const meta = (name: string) => secrets.data?.secrets.find((s) => s.name === name);
  return (
    <>
      {PROVIDERS.map((p) => (
        <Provider key={p.key} name={p.name} fields={p.fields.map(([n, l]) => ({ name: n, label: l, meta: meta(n) }))} onSaved={secrets.reload} />
      ))}
    </>
  );
}

function Provider({ name, fields, onSaved }: { name: string; fields: { name: string; label: string; meta?: SecretMeta }[]; onSaved: () => void }) {
  const ask = usePasswordPrompt();
  const toast = useToast();
  const [values, setValues] = useState<Record<string, string>>({});
  const connected = fields.every((f) => f.meta?.set);
  const changed = Object.values(values).some(Boolean);

  const save = async () => {
    try {
      for (const [k, v] of Object.entries(values)) if (v) await withStepUp(() => endpoints.setSecret(k, v), ask);
      setValues({});
      toast({ text: `${name} saved` });
      onSaved();
    } catch {
      toast({ text: `Couldn't save ${name}.` });
    }
  };
  const remove = async () => {
    try {
      for (const f of fields) if (f.meta?.set) await withStepUp(() => endpoints.deleteSecret(f.name), ask);
      onSaved();
    } catch { toast({ text: `Couldn't remove ${name}.` }); }
  };

  if (connected) {
    return (
      <Group title={name}>
        <SettingRow label="Status"><Status ok label="Connected" /></SettingRow>
        <ActionRow danger onClick={remove}>Remove</ActionRow>
      </Group>
    );
  }
  return (
    <Group title={name}>
      {fields.map((f) => {
        const placeholder = f.meta?.set ? `Set · ends ${f.meta.hint || "…"}` : f.name === "musicbrainz.contact" ? "Email or site URL" : "";
        const set = (v: string) => setValues({ ...values, [f.name]: v });
        return (
          <SettingRow key={f.name} label={f.label}>
            {f.name === "apple.private_key"
              ? <KeyFile label={f.label} hint={placeholder} onRead={set} />
              : <input className="input row-input" aria-label={f.label} type={f.name === "musicbrainz.contact" ? "text" : "password"} autoComplete="off" placeholder={placeholder} value={values[f.name] || ""} onChange={(e) => set(e.target.value)} />}
          </SettingRow>
        );
      })}
      <ActionRow disabled={!changed} onClick={save}>Connect</ActionRow>
    </Group>
  );
}

// Apple's private key comes as an AuthKey_XXXX.p8 file; its text is the secret.
export function KeyFile({ label, hint, onRead, onError }: { label: string; hint: string; onRead: (key: string) => void; onError?: () => void }) {
  const input = useRef<HTMLInputElement>(null);
  const toast = useToast();
  const [file, setFile] = useState("");
  const read = async (f?: File) => {
    if (input.current) input.current.value = "";
    if (!f) return;
    const text = (await f.text()).trim();
    if (!text.includes("PRIVATE KEY")) return onError ? onError() : toast({ text: "That isn't a .p8 key file." });
    setFile(f.name);
    onRead(text);
  };
  return (
    <span className="key-file">
      <span className={file ? "truncate" : "faint truncate"}>{file || hint || "No file"}</span>
      <button type="button" className="btn" aria-label={`Choose ${label} file`} onClick={() => input.current?.click()}>{file ? "Change…" : "Choose file…"}</button>
      <input ref={input} type="file" accept=".p8" hidden onChange={(e) => read(e.target.files?.[0])} />
    </span>
  );
}

// How scrobblers connect: the same three details as onboarding's scrobbler step.
// The token is optional; with it on, scrobblers must send it.
function Scrobbling() {
  const { me } = useSession();
  const cfg = useData(() => endpoints.scrobbling(), []);
  const ask = usePasswordPrompt();
  const toast = useToast();
  const [editing, setEditing] = useState(false);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const on = !!cfg.data?.require_token;

  const save = async (require: boolean, tok: string) => {
    setBusy(true);
    try {
      await withStepUp(() => endpoints.setScrobbling(require, tok), ask);
      setEditing(false);
      setToken("");
      cfg.reload();
      toast({ text: require ? "Token saved" : "Token turned off" });
    } catch (e) {
      toast({ text: e instanceof ApiError && e.message ? e.message : "Couldn't save." });
    } finally {
      setBusy(false);
    }
  };
  // Turning it on asks for a token first unless one is already stored.
  const toggle = (v: boolean) => {
    if (!v) { setEditing(false); setToken(""); if (on) save(false, ""); return; }
    if (cfg.data?.token_set) save(true, "");
    else setEditing(true);
  };
  const showInput = editing;
  const noToken = on && !cfg.data?.token_set; // an older install: required, but none chosen yet

  return (
    <div>
      <Group title="Scrobbling">
        <SettingRow label="ListenBrainz API URL"><CopyField value={listenBrainzUrl()} label="ListenBrainz API URL" /></SettingRow>
        <SettingRow label="Username"><CopyField value={cfg.data?.username || me?.username || ""} label="Username" /></SettingRow>
        <SettingRow label="Token">
          <span style={{ display: "flex", gap: 8, alignItems: "center" }}>
            {on && !showInput && <button className="btn btn-plain" onClick={() => setEditing(true)}>{noToken ? "Set a token…" : "Change…"}</button>}
            <Switch label="Token" checked={on || editing} onChange={toggle} />
          </span>
        </SettingRow>
        {showInput && (
          <div className="row-setting" style={{ gap: 8 }}>
            <input className="input token-input" aria-label="Token" placeholder="Choose a token" autoFocus autoComplete="off" spellCheck={false}
              maxLength={128} value={token} onChange={(e) => setToken(e.target.value)} onKeyDown={(e) => e.key === "Enter" && tokenOk(token) && save(true, token.trim())} />
            <button className="btn" onClick={() => { setEditing(false); setToken(""); }}>Cancel</button>
            <button className="btn btn-primary" disabled={busy || !tokenOk(token)} onClick={() => save(true, token.trim())}>Save</button>
          </div>
        )}
      </Group>
      {!(noToken && !editing) && <p className="faint ob-note">{on || editing
        ? "Scrobblers send this token. It can't be shown again, so keep a copy."
        : "With no token, anything that can reach this address can add plays."}</p>}
    </div>
  );
}

