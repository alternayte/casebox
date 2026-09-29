import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api, ApiError, resetCsrf } from "@/lib/api";

type Methods = { local: boolean; oidc: boolean };

// A return URL is always a path on this site, never another origin.
function safeReturn(url: string | undefined) {
  return url && url.startsWith("/") && !url.startsWith("//") ? url : "/";
}

export function Login({ returnUrl }: { returnUrl?: string }) {
  const methods = useQuery({ queryKey: ["auth-methods"], queryFn: () => api<Methods>("GET", "/api/v1/auth/methods") });
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const target = safeReturn(returnUrl);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api("POST", "/api/v1/auth/local", { password });
      resetCsrf();
      window.location.assign(target);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "The server cannot be reached.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mx-auto mt-16 max-w-sm space-y-6">
      <h1 className="text-xl font-semibold">Log in to Casebox</h1>
      {methods.data?.oidc && (
        <a
          className="block rounded-md bg-primary px-4 py-2 text-center text-primary-foreground"
          href={`/api/v1/auth/oidc/login?returnUrl=${encodeURIComponent(target)}`}
        >
          Log in with your identity provider
        </a>
      )}
      {methods.data?.local && (
        <form onSubmit={submit} className="space-y-3">
          <label className="block text-sm font-medium" htmlFor="password">
            Local admin password
          </label>
          <input
            id="password"
            type="password"
            autoComplete="current-password"
            className="w-full rounded-md border border-input bg-background px-3 py-2"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          <p className="text-sm text-muted-foreground">casebox up printed this password.</p>
          <button
            type="submit"
            disabled={busy || password.length === 0}
            className="w-full rounded-md border px-4 py-2 disabled:opacity-50"
          >
            Log in
          </button>
        </form>
      )}
      {methods.data && !methods.data.local && !methods.data.oidc && (
        <p className="text-sm">No sign-in method is configured on this server.</p>
      )}
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
