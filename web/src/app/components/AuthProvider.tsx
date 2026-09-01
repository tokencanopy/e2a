"use client";

import { createContext, useContext, useState, useEffect, useCallback } from "react";
import type { UserInfo } from "./types";

type AuthContextType = {
  user: UserInfo | null;
  loading: boolean;
  signOut: () => Promise<void>;
  // setUser lets other components push fresh profile data into the
  // shared cache after a PATCH /api/auth/me edit, so the sidebar's
  // identity card updates without an extra round-trip.
  setUser: (u: UserInfo | null) => void;
};

const AuthContext = createContext<AuthContextType>({
  user: null,
  loading: true,
  signOut: async () => {},
  setUser: () => {},
});

export function useAuth() {
  return useContext(AuthContext);
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [user, setUser] = useState<UserInfo | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    fetch("/api/auth/me", { credentials: "include" })
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => setUser(data))
      .catch(() => setUser(null))
      .finally(() => setLoading(false));
  }, []);

  const signOut = useCallback(async () => {
    setUser(null);

    // Use a top-level native navigation instead of fetch(). The server may
    // redirect to an OIDC provider on another origin, and fetch follows that
    // redirect without navigating the browser or reliably applying the
    // provider's Set-Cookie headers.
    const form = document.createElement("form");
    form.method = "POST";
    form.action = "/api/auth/logout";
    form.hidden = true;
    document.body.appendChild(form);
    form.submit();
  }, []);

  return (
    <AuthContext.Provider value={{ user, loading, signOut, setUser }}>
      {children}
    </AuthContext.Provider>
  );
}
