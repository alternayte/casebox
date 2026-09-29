// The web UI talks only to /api/v1. State-changing calls carry the CSRF token.
let csrf: string | undefined;

export class ApiError extends Error {
  constructor(
    public status: number,
    title: string,
  ) {
    super(title);
  }
}

async function csrfToken(): Promise<string> {
  if (!csrf) {
    const res = await fetch("/api/v1/auth/csrf", { credentials: "same-origin" });
    if (!res.ok) throw new ApiError(res.status, "Log in first.");
    csrf = ((await res.json()) as { token: string }).token;
  }
  return csrf;
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && !path.startsWith("/api/v1/auth/local")) headers["X-CSRF-TOKEN"] = await csrfToken();
  const res = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const problem = (await res.json().catch(() => ({}))) as { title?: string };
    throw new ApiError(res.status, problem.title ?? `The server answered ${res.status}.`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export function resetCsrf() {
  csrf = undefined;
}
