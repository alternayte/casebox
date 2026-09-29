import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api, ApiError } from "@/lib/api";

type Me = { accountId: string; displayName: string; role: string; orgId: string };

// Approves a CLI's device login, so casebox init or join receives a token for this account.
export function Device({ code }: { code?: string }) {
  const me = useQuery({ queryKey: ["me"], queryFn: () => api<Me>("GET", "/api/v1/me"), retry: false });
  const [userCode, setUserCode] = useState(code ?? "");
  const [state, setState] = useState<"idle" | "busy" | "approved">("idle");
  const [error, setError] = useState<string>();

  if (me.error instanceof ApiError && me.error.status === 401) {
    const here = `/device${userCode ? `?code=${encodeURIComponent(userCode)}` : ""}`;
    window.location.assign(`/login?returnUrl=${encodeURIComponent(here)}`);
    return null;
  }

  async function approve(e: FormEvent) {
    e.preventDefault();
    setState("busy");
    setError(undefined);
    try {
      await api("POST", "/api/v1/auth/device/approve", { userCode });
      setState("approved");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "The server cannot be reached.");
      setState("idle");
    }
  }

  if (state === "approved") {
    return (
      <div className="mx-auto mt-16 max-w-sm space-y-2">
        <h1 className="text-xl font-semibold">The CLI is connected</h1>
        <p className="text-muted-foreground">Go back to your terminal. You can close this page.</p>
      </div>
    );
  }

  return (
    <form onSubmit={approve} className="mx-auto mt-16 max-w-sm space-y-4">
      <h1 className="text-xl font-semibold">Connect the Casebox CLI</h1>
      <p className="text-sm text-muted-foreground">
        Check that this code matches the one in your terminal. The CLI then acts as {me.data?.displayName ?? "you"}.
      </p>
      <input
        aria-label="Code"
        className="w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-lg tracking-widest"
        value={userCode}
        onChange={(e) => setUserCode(e.target.value.toUpperCase())}
        placeholder="XXXX-XXXX"
      />
      <button
        type="submit"
        disabled={state === "busy" || userCode.replace(/[^A-Z]/g, "").length !== 8 || !me.data}
        className="w-full rounded-md bg-primary px-4 py-2 text-primary-foreground disabled:opacity-50"
      >
        Approve
      </button>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </form>
  );
}
