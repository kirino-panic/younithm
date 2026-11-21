import { StrictMode, useRef } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes, useLocation, type Location } from "react-router-dom";
import "@fontsource-variable/inter"; // Inter, served from this app, not from Google
import "react-grid-layout/css/styles.css";
import "react-resizable/css/styles.css";
import "./styles.css";
import { SessionProvider, useSession } from "./session";
import { PasswordPromptProvider, ToastProvider, useData } from "./ui";
import { basePath, endpoints } from "./api";
import { PageProvider } from "./home/PageContext";
import { PublicShell } from "./shell/Shell";
import { HomePage } from "./home/Home";
import { ArtistPage, AlbumPage, TrackPage } from "./entities/Entities";
import { AdminLayout } from "./admin/AdminLayout";
import { Imports } from "./admin/Imports";
import { Review } from "./admin/Review";
import { Library } from "./library/Library";
import { Scrobbles } from "./library/Scrobbles";
import { SearchPage } from "./library/Search";
import { Connections } from "./admin/Connections";
import { Settings } from "./admin/Settings";
import { Login, Setup } from "./admin/Auth";
import { Welcome } from "./onboarding/Welcome";

function RequireAdmin({ children }: { children: JSX.Element }) {
  const { me } = useSession();
  if (!me) return null;
  if (me.setup_required) return <Navigate to="/setup" replace />;
  return me.authenticated ? <RequireOnboarded>{children}</RequireOnboarded> : <Navigate to="/login" replace />;
}

function RequireSignedIn({ children }: { children: JSX.Element }) {
  const { me } = useSession();
  if (!me) return null;
  if (me.setup_required) return <Navigate to="/setup" replace />;
  return me.authenticated ? children : <Navigate to="/login" replace />;
}

// Until the welcome flow is finished, the admin area sends you back to it, so
// an interrupted onboarding is never lost.
function RequireOnboarded({ children }: { children: JSX.Element }) {
  const status = useData(() => endpoints.onboarding(), []);
  if (!status.data && !status.error) return null;
  return status.data && !status.data.done ? <Navigate to="/welcome" replace /> : children;
}

// The admin area opens over the site: the page it was opened from stays
// rendered behind it (the home page when /admin is opened directly).
const HOME = { pathname: "/", search: "", hash: "", state: null, key: "home" };

function AppRoutes() {
  const loc = useLocation();
  const { me } = useSession();
  const site = useRef<Location>(HOME);
  const inAdmin = loc.pathname === "/admin" || loc.pathname.startsWith("/admin/");
  if (!inAdmin && !["/login", "/setup", "/welcome"].includes(loc.pathname)) site.current = loc;
  const back = site.current.pathname + site.current.search;
  // A fresh install has nothing to show yet: send the first visitor to setup.
  if (me?.setup_required && loc.pathname !== "/setup") return <Navigate to="/setup" replace />;
  return (
    <>
      <Routes location={inAdmin ? site.current : loc}>
        <Route element={<PublicShell />}>
          <Route index element={<HomePage />} />
          <Route path="artist/:id" element={<ArtistPage />} />
          <Route path="album/:id" element={<AlbumPage />} />
          <Route path="track/:id" element={<TrackPage />} />
          <Route path="scrobbles" element={<Scrobbles />} />
          <Route path="history" element={<Navigate to="/scrobbles" replace />} />
          <Route path="library" element={<Library />} />
          <Route path="search" element={<SearchPage />} />
        </Route>
        <Route path="login" element={<Login />} />
        <Route path="setup" element={<Setup />} />
        <Route path="welcome" element={<RequireSignedIn><Welcome /></RequireSignedIn>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
      {inAdmin && (
        <Routes>
          <Route path="admin" element={<RequireAdmin><AdminLayout closeTo={back} /></RequireAdmin>}>
            <Route index element={<Navigate to="settings" replace />} />
            <Route path="settings" element={<Settings />} />
            <Route path="imports" element={<Imports />} />
            <Route path="review" element={<Review />} />
            <Route path="library" element={<Navigate to="/library" replace />} />
            <Route path="connections" element={<Connections />} />
            <Route path="*" element={<Navigate to="/admin/settings" replace />} />
          </Route>
        </Routes>
      )}
    </>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter basename={basePath || undefined}>
      <SessionProvider>
        <PageProvider>
          <ToastProvider>
            <PasswordPromptProvider>
              <AppRoutes />
            </PasswordPromptProvider>
          </ToastProvider>
        </PageProvider>
      </SessionProvider>
    </BrowserRouter>
  </StrictMode>,
);
