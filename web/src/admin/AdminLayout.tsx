import { useCallback, useEffect } from "react";
import { NavLink, Outlet, useNavigate, useOutletContext } from "react-router-dom";
import { CheckCheck, LogOut, Plug, Settings as SettingsIcon, Upload, X } from "lucide-react";
import { endpoints } from "../api";
import { useSession } from "../session";
import { useData, useEscape } from "../ui";

// The admin area, a window over the site. closeTo is the page behind it.
export function AdminLayout({ closeTo }: { closeTo: string }) {
  const { signOut } = useSession();
  const queue = useData(() => endpoints.enrichment(), []);
  useEffect(() => { const t = window.setInterval(queue.reload, 3000); return () => window.clearInterval(t); }, [queue.reload]);
  const pending = queue.data?.review_pending || 0;
  const navigate = useNavigate();
  const close = useCallback(() => navigate(closeTo), [navigate, closeTo]);
  // Escape closes a sheet or dialog opened inside first.
  useEscape(useCallback(() => { if (!document.querySelector(".sheet, .modal")) close(); }, [close]));
  const items = [
    { to: "settings", label: "Settings", icon: SettingsIcon },
    { to: "imports", label: "Imports", icon: Upload },
    ...(pending ? [{ to: "review", label: "Review", icon: CheckCheck, count: pending }] : []),
    { to: "connections", label: "Connections", icon: Plug },
  ];
  return (
    <>
      <div className="scrim" onClick={close} />
      <div className="overlay-wrap" onClick={close}>
        <div className="overlay admin" role="dialog" aria-label="Admin" onClick={(e) => e.stopPropagation()}>
          <nav className="admin-nav" aria-label="Admin">
            <h2>Admin</h2>
            {items.map(({ to, label, icon: Icon, count }) => (
              <NavLink key={to} to={to} className={({ isActive }) => (isActive ? "active" : "")}>
                <Icon size={17} strokeWidth={1.8} /><span>{label}</span>
                {!!count && <b className="badge badge-accent num">{count}</b>}
              </NavLink>
            ))}
            <div style={{ flex: 1 }} />
            <a href="#" onClick={(e) => { e.preventDefault(); signOut(); }}><LogOut size={17} strokeWidth={1.8} /><span>Sign out</span></a>
          </nav>
          <main className="admin-main">
            <div className="admin-content">
              <Outlet context={{ queue: queue.data || {}, reloadQueue: queue.reload } satisfies QueueContext} />
            </div>
          </main>
          <button className="btn btn-plain btn-icon admin-close" onClick={close} aria-label="Close"><X size={18} /></button>
        </div>
      </div>
    </>
  );
}

export function AdminHead({ title, children }: { title: string; children?: React.ReactNode }) {
  return <div className="admin-head"><h1>{title}</h1>{children && <div style={{ display: "flex", gap: 8 }}>{children}</div>}</div>;
}

export interface QueueContext { queue: Record<string, number>; reloadQueue: () => void }
export const useQueue = () => useOutletContext<QueueContext>();
