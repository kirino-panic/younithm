import { useEffect, useRef, useState, type ReactNode } from "react";
import { AlertCircle, Info } from "lucide-react";
import { Badge, SettingRow } from "../ui";
import { T, type CopyKey } from "./copy";
import { DebugPanel } from "./DebugPanel";

// The steps after account creation. Account creation is step 0.
export const STEPS = ["profile", "connections", "history", "scrobbler", "ready"] as const;
export type Step = (typeof STEPS)[number];

export function StepDots({ at }: { at: number }) {
  return (
    <div className="step-dots" role="img" aria-label={`Step ${at + 1} of ${STEPS.length + 1}`}>
      {Array.from({ length: STEPS.length + 1 }, (_, i) => <span key={i} className={i <= at ? "on" : ""} />)}
    </div>
  );
}

// Pinned to the top of every screen of a simulated run, with the debug panel.
export function SimBadge() {
  return (
    <>
      <div className="sim-badge"><Badge tone="warning"><T k="common.sim" /></Badge></div>
      <DebugPanel />
    </>
  );
}

// Every onboarding screen after the splash uses this one frame, so the dots,
// title and buttons always sit in the same place: the body grows between them.
export function Frame({ simulate, at, onSkip, title, sub, children, error, onBack, onNext, next = "common.next", busy, busyLabel, nextDisabled, secondary }: {
  simulate: boolean; at: number; onSkip?: () => void;
  title: ReactNode; sub: ReactNode; children?: ReactNode; error?: ReactNode;
  onBack?: () => void; onNext?: () => void; next?: CopyKey; busy?: boolean; busyLabel?: CopyKey; nextDisabled?: boolean;
  secondary?: { label: CopyKey; onClick: () => void }; // a second choice, beside the main button
}) {
  return (
    <div className={`ob-screen${simulate ? " simulating" : ""}`}>
      {simulate && <SimBadge />}
      <div className="ob-frame">
        <div className="ob-top">
          <StepDots at={at} />
          {onSkip && <button type="button" className="btn btn-plain" onClick={onSkip}><T k="common.skip" /></button>}
        </div>
        <header className="ob-head">
          <h1>{title}</h1>
          <p className="muted">{sub}</p>
        </header>
        <div className="ob-body">{children}</div>
        {error && <div className="ob-error-slot">{error}</div>}
        <footer className="ob-foot">
          {onBack && <button type="button" className="btn" disabled={busy} onClick={onBack}><T k="common.back" /></button>}
          {secondary && <button type="button" className="btn" onClick={secondary.onClick}><T k={secondary.label} /></button>}
          {onNext && (
            <button type="button" className="btn btn-primary" disabled={busy || nextDisabled} onClick={onNext}>
              <T k={busy && busyLabel ? busyLabel : next} />
            </button>
          )}
        </footer>
      </div>
    </div>
  );
}

// A problem the person has to act on: what went wrong, then what to do.
export function StepError({ title, children }: { title: ReactNode; children: ReactNode }) {
  // Long steps can push it below the fold: bring it into view when it appears.
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => { ref.current?.scrollIntoView({ block: "nearest", behavior: "smooth" }); }, []);
  return (
    <div ref={ref} className="ob-error" role="alert">
      <AlertCircle size={16} />
      <div><strong>{title}</strong><div>{children}</div></div>
    </div>
  );
}

// A setting with an (i) that opens an explanation under it.
export function InfoSetting({ label, info, name, children }: { label: CopyKey; info: CopyKey; name: string; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <SettingRow label={
        <span className="info-label">
          <T k={label} />
          <button type="button" className="info-btn" aria-expanded={open} aria-label={`About ${name}`} onClick={() => setOpen(!open)}>
            <Info size={15} />
          </button>
        </span>
      }>{children}</SettingRow>
      {open && <div className="row-setting row-note info-note"><span className="muted"><T k={info} /></span></div>}
    </>
  );
}
