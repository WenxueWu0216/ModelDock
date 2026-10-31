import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { setToken, setUnauthorizedHandler } from "./api";
import type { Role } from "./types";

export interface Session {
  token: string;
  sub: string;
  name?: string;
  role: Role;
  exp?: number;
}

const KEY = "modeldock.token";

/** Decodes (does not verify) a JWT payload. The API verifies every request. */
export function decodeToken(token: string): Session {
  const parts = token.trim().split(".");
  if (parts.length !== 3) throw new Error("not a JWT (expected three dot-separated parts)");
  const b64 = parts[1]!.replace(/-/g, "+").replace(/_/g, "/");
  const json = JSON.parse(atob(b64.padEnd(b64.length + ((4 - (b64.length % 4)) % 4), "="))) as {
    sub?: string;
    name?: string;
    role?: Role;
    exp?: number;
  };
  if (!json.sub || !json.role) throw new Error("token has no sub or role claim");
  if (json.exp && json.exp * 1000 < Date.now()) throw new Error("token has expired");
  return { token: token.trim(), sub: json.sub, name: json.name, role: json.role, exp: json.exp };
}

export const can = {
  approve: (r: Role) => r === "approver" || r === "admin",
  deploy: (r: Role) => r === "deployer" || r === "admin",
};

interface AuthState {
  session: Session | null;
  login: (token: string) => void;
  logout: () => void;
}

const AuthContext = createContext<AuthState | null>(null);

function initial(): Session | null {
  try {
    const t = sessionStorage.getItem(KEY);
    return t ? decodeToken(t) : null;
  } catch {
    return null;
  }
}

export function AuthProvider({ children, initialSession }: { children: ReactNode; initialSession?: Session | null }) {
  const [session, setSession] = useState<Session | null>(() => initialSession ?? initial());
  setToken(session?.token ?? null);

  const logout = useCallback(() => {
    try {
      sessionStorage.removeItem(KEY);
    } catch {
      /* storage unavailable */
    }
    setToken(null);
    setSession(null);
  }, []);

  const login = useCallback((token: string) => {
    const s = decodeToken(token);
    try {
      sessionStorage.setItem(KEY, s.token);
    } catch {
      /* keep in memory only */
    }
    setToken(s.token);
    setSession(s);
  }, []);

  useEffect(() => setUnauthorizedHandler(logout), [logout]);

  const value = useMemo(() => ({ session, login, logout }), [session, login, logout]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth outside AuthProvider");
  return ctx;
}

export function useSession(): Session {
  const s = useAuth().session;
  if (!s) throw new Error("not logged in");
  return s;
}
