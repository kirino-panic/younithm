import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { endpoints, setCsrf, type Me } from "./api";

interface Session {
  me: Me | null;
  admin: boolean;
  refresh: () => Promise<void>;
  signIn: (u: string, p: string) => Promise<void>;
  signOut: () => Promise<void>;
}

const Ctx = createContext<Session>({ me: null, admin: false, refresh: async () => {}, signIn: async () => {}, signOut: async () => {} });
export const useSession = () => useContext(Ctx);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const refresh = useCallback(async () => {
    try {
      const m = await endpoints.me();
      if (m.csrf) setCsrf(m.csrf);
      setMe(m);
    } catch {
      setMe({ authenticated: false });
    }
  }, []);
  useEffect(() => { refresh(); }, [refresh]);

  const signIn = async (u: string, p: string) => {
    const r = await endpoints.login(u, p);
    setCsrf(r.csrf);
    await refresh();
  };
  const signOut = async () => {
    await endpoints.logout().catch(() => {});
    setCsrf("");
    await refresh();
  };

  return <Ctx.Provider value={{ me, admin: !!me?.authenticated, refresh, signIn, signOut }}>{children}</Ctx.Provider>;
}
