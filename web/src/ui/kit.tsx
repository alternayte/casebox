import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode, SelectHTMLAttributes } from "react";
import { ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";

// Small building blocks for the report pages, styled from the tokens in index.css only.

export function Button({ className, variant = "outline", ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "outline" | "solid" | "quiet" }) {
  return (
    <button
      {...props}
      className={cn(
        "inline-flex h-7 items-center gap-1 rounded-sm px-2.5 text-xs font-medium disabled:cursor-not-allowed disabled:opacity-50",
        variant === "outline" && "border border-input bg-card hover:bg-secondary",
        variant === "solid" && "bg-primary text-primary-foreground hover:opacity-90",
        variant === "quiet" && "text-link hover:underline",
        className,
      )}
    />
  );
}

const control = "h-7 w-full rounded-sm border border-input bg-card px-2 text-xs text-foreground";

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={cn(control, className)} />;
}

export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select {...props} className={cn(control, "pr-6", className)}>
      {children}
    </select>
  );
}

export function Field({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <label className={cn("flex min-w-0 flex-col gap-1", className)}>
      <span className="text-2xs font-medium uppercase tracking-label text-muted-foreground">{label}</span>
      {children}
    </label>
  );
}

// A numbered section, as in a printed abstract: rule above, number, title, a note on the right.
export function Section({ no, title, note, children }: { no?: string; title: string; note?: ReactNode; children: ReactNode }) {
  return (
    <section className="border-t-2 border-foreground pt-3">
      <header className="mb-4 flex flex-wrap items-baseline gap-x-3 gap-y-1">
        {no && <span className="font-mono text-xs text-muted-foreground">{no}</span>}
        <h2 className="text-lg font-semibold tracking-tight">{title}</h2>
        {note && <p className="ml-auto text-xs text-muted-foreground">{note}</p>}
      </header>
      {children}
    </section>
  );
}

// In a solo organisation nothing is hidden, so an empty number has no data behind it yet.
export function Hidden({ k, what, solo }: { k?: number; what?: string; solo?: boolean }) {
  if (solo) return <span className="inline-block rounded-sm bg-hidden px-1.5 py-0.5 text-2xs text-muted-foreground">no data yet{what ? ` (${what})` : ""}</span>;
  return (
    <span className="inline-block rounded-sm bg-hidden px-1.5 py-0.5 text-2xs text-muted-foreground" title={`Casebox shows a number only when at least ${k ?? "k"} people are behind it.`}>
      hidden: fewer than {k ?? "k"} people{what ? ` (${what})` : ""}
    </span>
  );
}

// A horizontal bar for a share, with an optional interval whisker.
export function ShareBar({ share, interval, tone = "ink" }: { share: number; interval?: [number, number]; tone?: "ink" | "signal" | "muted" }) {
  return (
    <div className="relative h-2 w-full bg-secondary" aria-hidden>
      <div
        className={cn("absolute inset-y-0 left-0", tone === "ink" && "bg-foreground", tone === "signal" && "bg-signal", tone === "muted" && "bg-muted-foreground")}
        style={{ width: `${Math.max(0, Math.min(1, share)) * 100}%` }}
      />
      {interval && (
        <div
          className="absolute -inset-y-1 border-x border-foreground"
          style={{ left: `${interval[0] * 100}%`, width: `${Math.max(0, interval[1] - interval[0]) * 100}%` }}
        >
          <div className="absolute inset-x-0 top-1/2 h-px bg-foreground" />
        </div>
      )}
    </div>
  );
}

export function ErrorNote({ error }: { error: unknown }) {
  const message = error instanceof ApiError ? error.message : "The server cannot be reached.";
  const code = error instanceof ApiError ? error.code : undefined;
  const docs = error instanceof ApiError ? error.docs : undefined;
  return (
    <p role="alert" className="border-l-2 border-destructive py-1 pl-3 text-sm text-destructive">
      {message}
      {code && (
        <>
          {" "}
          {docs ? (
            <a href={docs} className="font-mono text-xs underline" target="_blank" rel="noreferrer">
              {code}
            </a>
          ) : (
            <span className="font-mono text-xs">{code}</span>
          )}
        </>
      )}
    </p>
  );
}

export function Loading({ what }: { what: string }) {
  return <p className="py-8 text-sm text-muted-foreground">Loading {what}…</p>;
}

export function Tag({ children, tone = "plain" }: { children: ReactNode; tone?: "plain" | "signal" | "ok" }) {
  return (
    <span
      className={cn(
        "inline-block whitespace-nowrap rounded-sm border px-1.5 text-2xs leading-4",
        tone === "plain" && "border-border text-muted-foreground",
        tone === "signal" && "border-signal text-signal",
        tone === "ok" && "border-ok text-ok",
      )}
    >
      {children}
    </span>
  );
}
