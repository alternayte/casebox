import { Link, Outlet } from "@tanstack/react-router";
import { useState } from "react";
import { api, resetCsrf } from "@/lib/api";
import { roleName } from "@/lib/labels";
import { useMe } from "@/lib/session";
import { ErrorNote } from "@/ui/kit";

const nav = [
  { to: "/", label: "Overview", exact: true },
  { to: "/work", label: "Work", exact: false },
] as const;

// Every page but login and device approval lives in this shell and needs a signed-in account.
export function AppShell() {
  const me = useMe();
  const [leaving, setLeaving] = useState(false);

  async function logOut() {
    setLeaving(true);
    try {
      await api("POST", "/api/v1/auth/logout");
    } finally {
      resetCsrf();
      window.location.assign("/login");
    }
  }

  return (
    <div className="min-h-screen">
      <header className="border-b border-foreground bg-card">
        <div className="mx-auto flex h-11 max-w-7xl items-center gap-6 px-6">
          <Link to="/" className="font-mono text-sm font-semibold tracking-tight">
            casebox
          </Link>
          <nav className="flex h-full items-stretch gap-1">
            {nav.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                activeOptions={{ exact: item.exact, includeSearch: false }}
                className="flex items-center border-b-2 border-transparent px-2 text-sm text-muted-foreground hover:text-foreground"
                activeProps={{ className: "!border-foreground !text-foreground font-medium" }}
              >
                {item.label}
              </Link>
            ))}
          </nav>
          {me.data && (
            <div className="ml-auto flex items-center gap-3 text-xs">
              <span>
                {me.data.displayName} <span className="text-muted-foreground">· {roleName(me.data.role)}</span>
              </span>
              <button type="button" onClick={logOut} disabled={leaving} className="text-link hover:underline disabled:opacity-50">
                Log out
              </button>
            </div>
          )}
        </div>
      </header>
      <main className="mx-auto max-w-7xl px-6 py-6">
        {me.isError ? <ErrorNote error={me.error} /> : me.data ? <Outlet /> : null}
      </main>
    </div>
  );
}
