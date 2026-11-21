import { useState } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { endpoints, reauth, setCsrf, ApiError } from "../api";
import { useSession } from "../session";
import { Frame, SimBadge, StepError } from "../onboarding/Frame";
import { Group, SettingRow } from "../ui";
import { T, t, type CopyKey } from "../onboarding/copy";
import { ACCOUNT_FAILURES, failures } from "../onboarding/failures";

const MESSAGES: Record<string, string> = {
  invalid_credentials: "Wrong username or password.",
  locked: "Too many tries. Wait a moment.",
};

export function Login() {
  const { me, signIn } = useSession();
  const nav = useNavigate();
  const [u, setU] = useState("");
  const [p, setP] = useState("");
  const [err, setErr] = useState("");

  if (me?.setup_required) return <Navigate to="/setup" replace />;
  if (me?.authenticated) return <Navigate to="/admin" replace />;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      await signIn(u, p);
      nav("/admin");
    } catch (x) {
      const c = x instanceof ApiError ? x.code : "";
      setErr(MESSAGES[c] || "Couldn't sign in.");
    }
  };

  return (
    <div className="center-screen">
      <form className="auth-card" onSubmit={submit}>
        <h1>Sign in</h1>
        <input className="input" autoComplete="username" placeholder="Username" value={u} onChange={(e) => setU(e.target.value)} autoFocus />
        <input className="input" type="password" autoComplete="current-password" placeholder={t("account.password")} value={p} onChange={(e) => setP(e.target.value)} />
        {err && <p className="error-text">{err}</p>}
        <button className="btn btn-primary" style={{ height: 36 }}>Sign in</button>
      </form>
    </div>
  );
}

// First run: whoever opens the site first creates the admin account. With
// ?simulate=1 (from Debug) it is a dry run for a signed-in admin.
const MIN_PASSWORD = 10; // matches internal/auth/password.go
const MAX_PASSWORD = 256;

type AccountErr = { kind: string; detail?: string };

export function Setup() {
  const { me, refresh } = useSession();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const simulate = params.get("simulate") === "1" && !!me?.authenticated;
  const [u, setU] = useState("admin");
  const [p, setP] = useState("");
  const [err, setErr] = useState<AccountErr | null>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [startedState, setStarted] = useState(false);
  // A simulated run keeps its screen in the URL, so the debug panel can jump to it.
  const started = simulate ? params.get("page") === "account" : startedState;

  if (done) return null;
  if (me && !me.setup_required && !simulate) return <Navigate to={me.authenticated ? "/admin" : "/login"} replace />;

  const submit = async () => {
    setErr(null);
    // The same rules the server applies, checked first so the answer is instant.
    const name = u.trim();
    if (name.length < 1 || name.length > 64) return setErr({ kind: "username_length" });
    if (!simulate && p.length < MIN_PASSWORD) return setErr({ kind: "password_short" });
    if (p.length > MAX_PASSWORD) return setErr({ kind: "password_long" });
    setBusy(true);
    try {
      if (simulate) {
        await new Promise((r) => setTimeout(r, 400));
        const f = failures.get("account.create");
        if (f) return setErr({ kind: f, detail: "simulated" });
        return nav("/welcome?simulate=1");
      }
      const r = await endpoints.setup(name, p);
      setCsrf(r.csrf);
      // The password was just typed, so the welcome steps that need a fresh
      // one (saving connection secrets) don't ask for it again.
      await reauth(p).catch(() => {});
      setDone(true);
      await refresh();
      nav("/welcome");
    } catch (x) {
      if (x instanceof ApiError) setErr({ kind: (ACCOUNT_FAILURES as readonly string[]).includes(x.code) ? x.code : x.status >= 500 ? "internal" : "other", detail: x.message });
      else setErr({ kind: "network" });
    } finally {
      setBusy(false);
    }
  };

  if (!started) {
    return (
      <div className={`center-screen${simulate ? " simulating" : ""}`}>
        {simulate && <SimBadge />}
        <div className="splash">
          <h1 className="splash-title"><span className="splash-kicker"><T k="splash.kicker" /></span><T k="splash.title" /></h1>
          <button className="btn btn-primary splash-start" autoFocus onClick={() => (simulate ? nav("/setup?simulate=1&page=account") : setStarted(true))}><T k="splash.start" /></button>
        </div>
      </div>
    );
  }

  const vars = { min: MIN_PASSWORD, max: MAX_PASSWORD, origin: location.origin, detail: err?.detail || "" };
  const enter = (e: React.KeyboardEvent) => { if (e.key === "Enter") submit(); };
  return (
    <Frame simulate={simulate} at={0} title={<T k="account.title" />} sub={<T k="account.sub" />}
      onBack={() => (simulate ? nav("/setup?simulate=1") : setStarted(false))}
      onNext={submit} busy={busy} busyLabel="account.creating"
      error={err && (
        <StepError title={<T k={`account.err.${err.kind}.title` as CopyKey} />}>
          <T k={`account.err.${err.kind}` as CopyKey} vars={vars} />
        </StepError>
      )}>
      <Group>
        <SettingRow label={<T k="account.username" />}>
          <input className="input row-input" aria-label={t("account.username")} autoComplete="username" autoFocus
            aria-invalid={err?.kind === "username_length" || undefined} value={u} onChange={(e) => setU(e.target.value)} onKeyDown={enter} />
        </SettingRow>
        <SettingRow label={<T k="account.password" />}>
          <input className="input row-input" type="password" aria-label={t("account.password")} autoComplete="new-password"
            aria-invalid={err?.kind.startsWith("password") || undefined} value={p} onChange={(e) => setP(e.target.value)} onKeyDown={enter} />
        </SettingRow>
      </Group>
      <p className="faint ob-note"><T k="account.passwordHint" vars={vars} /></p>
    </Frame>
  );
}
