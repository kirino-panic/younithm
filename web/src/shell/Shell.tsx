import { useEffect } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { Home, Library, List, LogIn, Pencil, Search, Settings, type LucideIcon } from "lucide-react";
import { useSession } from "../session";
import { endpoints } from "../api";
import { useData } from "../ui";
import { usePage, PublicTheme } from "../home/PageContext";


const main: { to: string; label: string; icon: LucideIcon }[] = [
  { to: "/", label: "Home", icon: Home },
  { to: "/search", label: "Search", icon: Search },
  { to: "/library", label: "Library", icon: Library },
  { to: "/scrobbles", label: "Scrobbles", icon: List },
];

export function PublicShell() {
  const { admin } = useSession();
  const page = usePage();
  const nav = useNavigate();
  const loc = useLocation();
  // Search reads the library, so it goes where the library does.
  const site = useData(() => (admin ? Promise.resolve(null) : endpoints.site()), [admin]);
  const canSearch = admin || (!site.loading && site.data?.sections.library !== false);

  // ⌘K / Ctrl-K opens search from anywhere on the site.
  useEffect(() => {
    if (!canSearch) return;
    const h = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== "k") return;
      e.preventDefault();
      const field = document.querySelector<HTMLInputElement>(".search-page .lib-search input");
      if (field) field.focus();
      else nav("/search");
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [canSearch, nav]);
  const links = main.filter((m) => m.to !== "/search" || canSearch);

  const edit = () => {
    if (loc.pathname !== "/") nav("/");
    page.startEditing();
  };

  return (
    <div className="app">
      <PublicTheme />
      <nav className="rail" aria-label="Main">
        {links.map(({ to, label, icon: Icon }) => (
          <NavLink key={to} to={to} end className={({ isActive }) => `rail-item${isActive ? " active" : ""}`} data-tip={label} aria-label={label}>
            <Icon size={20} strokeWidth={1.8} />
          </NavLink>
        ))}
        <div className="rail-spacer" />
        {admin && (
          <button className={`rail-item${page.editing ? " active" : ""}`} data-tip="Edit page" aria-label="Edit page" onClick={edit}>
            <Pencil size={19} strokeWidth={1.8} />
          </button>
        )}
        <NavLink to={admin ? "/admin" : "/login"} className="rail-item" data-tip={admin ? "Admin" : "Sign in"} aria-label={admin ? "Admin" : "Sign in"}>
          {admin ? <Settings size={20} strokeWidth={1.8} /> : <LogIn size={20} strokeWidth={1.8} />}
        </NavLink>
      </nav>
      <main className="main"><Outlet /></main>
      <nav className="tabbar" aria-label="Main">
        {links.map(({ to, label, icon: Icon }) => (
          <NavLink key={to} to={to} end className={({ isActive }) => (isActive ? "active" : "")}>
            <Icon size={22} strokeWidth={1.8} />{label}
          </NavLink>
        ))}
      </nav>
    </div>
  );
}
