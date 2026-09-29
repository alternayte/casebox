import { num } from "@/lib/format";
import { agentName } from "@/lib/labels";
import { cn } from "@/lib/utils";
import { Tag } from "@/ui/kit";
import type { EvaluationRow, HarnessSpec, Verdict, VerdictReached } from "./api";

// Numbers of an evaluation: differences in percentage points, ratios, money and time.

const minus = (s: string) => s.replace("-", "−");

// A share as signed percentage points: 0.12 is "+12.0 pts".
export function pts(share: number): string {
  return `${minus(share >= 0 ? `+${(share * 100).toFixed(1)}` : (share * 100).toFixed(1))} pts`;
}

export function ptsInterval(lower: number, upper: number): string {
  const one = (v: number) => minus(v >= 0 ? `+${(v * 100).toFixed(1)}` : (v * 100).toFixed(1));
  return `${one(lower)} to ${one(upper)} pts`;
}

export function level(l: number): string {
  return `${Math.round(l * 100)}%`;
}

export function usd(v: number): string {
  return `${v.toLocaleString("en-GB", { minimumFractionDigits: 2, maximumFractionDigits: 2 })} USD`;
}

export function ratio(r: number): string {
  return `${r.toFixed(2)}×`;
}

export function ratioInterval(lower: number | null, upper: number | null): string {
  return lower == null || upper == null ? "no interval" : `${lower.toFixed(2)} to ${upper.toFixed(2)}`;
}

export function minutes(m: number): string {
  return m < 90 ? `${num(Math.round(m))} min` : `${(m / 60).toFixed(1)} h`;
}

export function rate(passed: number, runs: number): string {
  return runs === 0 ? "—" : `${Math.round((passed / runs) * 100)}%`;
}

const verdictNames: Record<string, string> = {
  better: "Better",
  worse: "Worse",
  equivalent: "Equivalent",
  inconclusive: "Inconclusive",
};

const statusNames: Record<string, string> = {
  awaiting_confirmation: "Awaiting confirmation",
  running: "Running",
  done: "Done",
  cancelled: "Cancelled",
};

const purposeNames: Record<string, string> = {
  compare: "Comparison",
  harness_vs_none: "Harness vs no harness",
  harness_ci: "Harness CI",
  gate: "Proposal gate",
};

export const statusName = (s: string) => statusNames[s] ?? s.replaceAll("_", " ");
export const purposeName = (p: string) => purposeNames[p] ?? p.replaceAll("_", " ");

export function VerdictBadge({ verdict, cheaper }: { verdict: Verdict | string; cheaper?: boolean }) {
  return (
    <span
      className={cn(
        "inline-block whitespace-nowrap rounded-sm px-1.5 text-2xs font-medium leading-4",
        verdict === "better" && "bg-ok text-background",
        verdict === "worse" && "bg-signal text-background",
        verdict === "equivalent" && "bg-foreground text-background",
        verdict === "inconclusive" && "border border-input text-muted-foreground",
      )}
    >
      {verdictNames[verdict] ?? verdict}
      {cheaper ? " and cheaper" : ""}
    </span>
  );
}

export function StatusTag({ status }: { status: string }) {
  const tone = status === "awaiting_confirmation" ? "signal" : status === "done" ? "ok" : "plain";
  return <Tag tone={tone}>{statusName(status)}</Tag>;
}

// The server names what differs: agent, agentVersion, model, effort, harness, settings or command.
export function changeName(change: string): string {
  return (
    { agent: "Agent", agentVersion: "Agent version", model: "Model", effort: "Effort", harness: "Harness", settings: "Settings", command: "Command" }[change] ??
    change
  );
}

export function sideValue(s: HarnessSpec, change: string): string {
  switch (change) {
    case "agent":
    case "agentVersion":
      return `${agentName(s.agent)} ${s.agentVersion}`.trim();
    case "model":
      return s.model;
    case "effort":
      return s.effort ?? "default";
    case "harness":
      return s.harness === "none" ? "no harness" : s.harness;
    case "settings":
      return settingsText(s);
    case "command":
      return s.command?.template ?? "none";
    default:
      return "";
  }
}

export function settingsText(s: HarnessSpec): string {
  const parts = [
    s.settings.maxTurns != null ? `${num(s.settings.maxTurns)} turns` : null,
    `${num(s.settings.timeoutMinutes ?? 30)} min timeout`,
    s.settings.tokenCap != null ? `${num(s.settings.tokenCap)} token cap` : null,
  ];
  return parts.filter(Boolean).join(", ");
}

export function ChangeText({ e }: { e: EvaluationRow }) {
  return (
    <span className="min-w-0">
      <span className="text-muted-foreground">{changeName(e.change)}:</span>{" "}
      <span className="font-mono text-xs">{sideValue(e.baseline, e.change)}</span> → <span className="font-mono text-xs">{sideValue(e.candidate, e.change)}</span>
    </span>
  );
}

// The rule a verdict met, or why it is inconclusive (docs/specs/evaluations.md, Statistics).
export function verdictRule(v: VerdictReached, delta: number): string {
  const l = level(v.level);
  const margin = `±${(delta * 100).toFixed(1)} pts`;
  switch (v.verdict) {
    case "better":
      return `The ${l} interval lies entirely above 0.`;
    case "worse":
      return `The ${l} interval lies entirely below 0.`;
    case "equivalent":
      return `The ${l} interval lies within ${margin} (δ).`;
  }
  if (v.reason === "budget") return "Inconclusive: the budget ran out. The next round would have passed the cap.";
  if (v.reason) return `Inconclusive: ${v.reason}.`;
  if (v.cases < minimumCases) return `Inconclusive: only ${num(v.cases)} cases have completed runs on both sides. A verdict needs at least ${minimumCases}.`;
  return `Inconclusive: the ${l} interval neither lies on one side of 0 nor within ${margin}.`;
}

// The server's EvaluationDecider.MinimumCases: no verdict below it.
export const minimumCases = 10;

// A difference with its interval on a fixed axis: zero, the ±δ band, the interval and the estimate.
export function IntervalPlot({ value, lower, upper, delta, domain }: { value: number; lower: number; upper: number; delta: number; domain: number }) {
  const x = (v: number) => `${((Math.max(-domain, Math.min(domain, v)) + domain) / (2 * domain)) * 100}%`;
  return (
    <div className="relative h-4 w-full" aria-hidden>
      <div className="absolute inset-y-0 bg-secondary" style={{ left: x(-delta), width: `calc(${x(delta)} - ${x(-delta)})` }} />
      <div className="absolute inset-y-0 w-px bg-muted-foreground" style={{ left: x(0) }} />
      <div className="absolute top-1/2 h-px bg-foreground" style={{ left: x(lower), width: `calc(${x(upper)} - ${x(lower)})` }} />
      <div className="absolute top-1 bottom-1 w-px bg-foreground" style={{ left: x(lower) }} />
      <div className="absolute top-1 bottom-1 w-px bg-foreground" style={{ left: x(upper) }} />
      <div className="absolute top-1/2 size-2 -translate-x-1/2 -translate-y-1/2 rounded-full bg-foreground" style={{ left: x(value) }} />
    </div>
  );
}

// The axis half-width that holds every interval and the ±δ band, rounded up to 5 points.
export function plotDomain(intervals: { lower: number; upper: number }[], delta: number): number {
  const widest = Math.max(delta * 2, 0.1, ...intervals.flatMap((i) => [Math.abs(i.lower), Math.abs(i.upper)]));
  return Math.min(1, Math.ceil(widest * 20) / 20);
}

export function ControlledNote({ className }: { className?: string }) {
  return (
    <p className={cn("text-xs text-muted-foreground", className)}>
      A verdict is a controlled comparison: both sides run the same cases, in random interleaved order, and differ in one thing. Steering metrics only observe
      work as it happened, so they show association, not cause.
    </p>
  );
}
