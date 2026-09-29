import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { ApiError } from "@/lib/api";
import { num } from "@/lib/format";
import { assertionKindName } from "@/lib/labels";
import { cn } from "@/lib/utils";
import { ErrorNote, Loading, Section, Tag } from "@/ui/kit";
import { useRun, type RunView, type TestCount } from "./api";
import { usd } from "./parts";

// One run: its tests, assertions, judge answer, process checks, usage and trace.
export function RunPage({ id, runId }: { id: string; runId: string }) {
  const query = useRun(id, runId);
  const back = (
    <Link to="/evaluations/$id" params={{ id }} className="text-xs text-link hover:underline">
      ← Evaluation
    </Link>
  );

  if (query.isPending) return <Loading what="the run" />;
  if (query.isError) {
    const status = query.error instanceof ApiError ? query.error.status : 0;
    return (
      <div className="space-y-2">
        {back}
        {status === 403 ? (
          <p className="text-sm">Held-out results are hidden. A held-out evaluation shows only its verdict, counts and cost.</p>
        ) : status === 404 ? (
          <p className="text-sm">This run does not exist.</p>
        ) : (
          <ErrorNote error={query.error} />
        )}
      </div>
    );
  }

  const r = query.data;
  return (
    <div className="space-y-8">
      <div>
        {back}
        <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-2xl font-semibold tracking-tight">
          <span>{r.side === "baseline" ? "Baseline" : "Candidate"} run</span>
          <span className="font-mono text-base font-normal text-muted-foreground">{r.runId}</span>
          <span className="self-center text-base">
            <Outcome r={r} />
          </span>
        </h1>
        <p className="mt-1 text-xs text-muted-foreground">
          Case{" "}
          <Link to="/cases/$id" params={{ id: r.caseId }} className="font-mono text-link hover:underline">
            {r.caseId}
          </Link>{" "}
          · repeat {r.repeat} · model {r.model ?? "not reported"}
        </p>
        {r.reason && <p className="mt-2 border-l-2 border-signal py-1 pl-3 text-sm">{r.reason}</p>}
      </div>

      <div className="grid gap-10 lg:grid-cols-[minmax(0,1fr)_22rem]">
        <div className="space-y-10">
          <Section title="Tests" note="Run once in a separate verifier sandbox, with the held-out tests added.">
            <Tests r={r} />
          </Section>
          <Section title="Assertions">
            <Assertions r={r} />
          </Section>
          <Section title="Judge" note={<Tag>medium evidence</Tag>}>
            <Judge r={r} />
          </Section>
        </div>
        <div className="space-y-10">
          <Section title="Process checks">
            <Process r={r} />
          </Section>
          <Section title="Usage and cost">
            <Usage r={r} />
          </Section>
        </div>
      </div>

      <Section title="Trace" note="The agent's session as canonical events, in order.">
        <Trace trace={r.trace} />
      </Section>
    </div>
  );
}

function Outcome({ r }: { r: RunView }) {
  if (r.status === "failed") return <Tag tone="signal">failed to run</Tag>;
  if (r.passed === true) return <Tag tone="ok">passed</Tag>;
  if (r.passed === false) return <Tag tone="signal">did not pass</Tag>;
  return <Tag>{r.status.replaceAll("_", " ")}</Tag>;
}

function Line({ term, children }: { term: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted-foreground">{term}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function Count({ c }: { c: TestCount }) {
  const ok = c.passed === c.total;
  return (
    <span className={cn("font-mono tabular-nums", !ok && "text-signal")}>
      {num(c.passed)} of {num(c.total)} passed
    </span>
  );
}

function Tests({ r }: { r: RunView }) {
  if (r.applied === false) return <p className="text-sm text-signal">The agent's diff did not apply to a pristine base. No test ran.</p>;
  if (!r.tests) return <p className="text-sm text-muted-foreground">No verification result yet.</p>;
  const failed = r.failedTests ?? [];
  return (
    <div className="space-y-3 text-sm">
      <dl className="grid grid-cols-[9rem_minmax(0,1fr)] gap-y-1">
        <Line term="Fail-to-pass">
          <Count c={r.tests.failToPass} />
        </Line>
        <Line term="Pass-to-pass">
          <Count c={r.tests.passToPass} />
        </Line>
      </dl>
      {failed.length > 0 && (
        <div>
          <p className="text-2xs font-medium uppercase tracking-label text-muted-foreground">Failed tests ({num(failed.length)})</p>
          <ul className="mt-1 space-y-0.5 font-mono text-xs">
            {failed.map((t) => (
              <li key={t} className="break-all">
                {t}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

function Assertions({ r }: { r: RunView }) {
  const list = r.assertions ?? [];
  if (list.length === 0) return <p className="text-sm text-muted-foreground">The case has no steering assertions.</p>;
  return (
    <ul className="space-y-1 text-sm">
      {list.map((a, i) => (
        <li key={i} className="flex items-center gap-2">
          {a.passed ? <Tag tone="ok">held</Tag> : <Tag tone="signal">broken</Tag>}
          {assertionKindName(a.kind)}
        </li>
      ))}
    </ul>
  );
}

function Judge({ r }: { r: RunView }) {
  if (!r.judge) return <p className="text-sm text-muted-foreground">The case asks no judge question.</p>;
  return (
    <div className="space-y-2 text-sm">
      <p className="whitespace-pre-wrap">{r.judge.answer}</p>
      <p className="text-xs text-muted-foreground">
        The judge is a model's answer to a narrow question: {r.judge.evidence} evidence. It can fail a run whose tests passed; it never passes a run whose tests or
        assertions failed.
      </p>
    </div>
  );
}

function Process({ r }: { r: RunView }) {
  const p = r.processChecks;
  if (!p) return <p className="text-sm text-muted-foreground">No process checks were recorded.</p>;
  return (
    <ul className="space-y-1.5 text-sm">
      <li className="flex items-center gap-2">
        {p.ranTestsBeforeDone ? <Tag tone="ok">yes</Tag> : <Tag tone="signal">no</Tag>}
        Ran the tests before saying done
      </li>
      <li className="flex items-center gap-2">
        {p.editedTestAfterFailure ? <Tag tone="signal">yes</Tag> : <Tag tone="ok">no</Tag>}
        Edited a test file after a failure
      </li>
    </ul>
  );
}

function Usage({ r }: { r: RunView }) {
  const u = r.usage;
  const tokens = (v: number | null | undefined) => (v == null ? "—" : num(v));
  return (
    <dl className="grid grid-cols-[9rem_minmax(0,1fr)] gap-y-1 text-xs">
      <Line term="Input tokens">
        <span className="font-mono tabular-nums">{tokens(u?.inputTokens)}</span>
      </Line>
      <Line term="Output tokens">
        <span className="font-mono tabular-nums">{tokens(u?.outputTokens)}</span>
      </Line>
      <Line term="Cache read">
        <span className="font-mono tabular-nums">{tokens(u?.cacheReadTokens)}</span>
      </Line>
      <Line term="Cache write">
        <span className="font-mono tabular-nums">{tokens(u?.cacheWriteTokens)}</span>
      </Line>
      <Line term="Cost">
        <span className="font-mono tabular-nums">{usd(r.costUsd)}</span>{" "}
        <span className="text-muted-foreground">{u?.costUsd != null ? "(reported by the agent)" : "(tokens × the price table)"}</span>
      </Line>
      <Line term="Duration">
        <span className="font-mono tabular-nums">{r.seconds == null ? "—" : `${num(Math.round(r.seconds))} s`}</span>
      </Line>
      <Line term="Turns">
        <span className="font-mono tabular-nums">{r.turns == null ? "—" : num(r.turns)}</span>
      </Line>
      <Line term="Tool calls">
        <span className="font-mono tabular-nums">{r.toolCalls == null ? "—" : num(r.toolCalls)}</span>
      </Line>
      <Line term="Limits">
        {r.timedOut || r.tokenCapExceeded ? (
          <span className="text-signal">{[r.timedOut && "timed out", r.tokenCapExceeded && "token cap exceeded"].filter(Boolean).join(", ")}</span>
        ) : (
          "within the timeout and the token cap"
        )}
      </Line>
    </dl>
  );
}

// A canonical session event (cli/internal/capture Event).
type TraceEvent = {
  seq?: number;
  at?: string;
  kind?: string;
  text?: string;
  tool?: { name?: string; status?: string; files?: string[] };
  usage?: { inputTokens?: number; outputTokens?: number; cacheReadTokens?: number; cacheWriteTokens?: number };
};

const eventNames: Record<string, string> = {
  session_start: "Session start",
  prompt: "Prompt",
  response: "Response",
  tool_call: "Tool call",
  tool_result: "Tool result",
  interruption: "Interruption",
  denial: "Denied tool call",
  rewind: "Rewind",
  human_edit: "Human edit",
  compaction: "Compaction",
  session_end: "Session end",
  unknown: "Other",
};

function traceEvents(trace: unknown): TraceEvent[] | null {
  if (Array.isArray(trace)) return trace as TraceEvent[];
  if (trace && typeof trace === "object" && Array.isArray((trace as { events?: unknown }).events)) return (trace as { events: TraceEvent[] }).events;
  return null;
}

function Trace({ trace }: { trace: unknown }) {
  if (trace == null) return <p className="text-sm text-muted-foreground">This run has no trace.</p>;
  const events = traceEvents(trace);
  if (!events) return <pre className="max-h-[32rem] overflow-auto border bg-card p-3 font-mono text-xs">{JSON.stringify(trace, null, 2)}</pre>;
  if (events.length === 0) return <p className="text-sm text-muted-foreground">The trace has no events.</p>;
  const start = events[0]?.at ? Date.parse(events[0].at) : NaN;
  return (
    <div>
      <p className="mb-2 text-xs text-muted-foreground">{num(events.length)} events.</p>
      <ol className="divide-y border-y text-sm">
        {events.map((ev, i) => {
          const offset = ev.at && !Number.isNaN(start) ? Math.max(0, Math.round((Date.parse(ev.at) - start) / 1000)) : null;
          const tokens = ev.usage ? (ev.usage.inputTokens ?? 0) + (ev.usage.outputTokens ?? 0) + (ev.usage.cacheReadTokens ?? 0) + (ev.usage.cacheWriteTokens ?? 0) : 0;
          return (
            <li key={ev.seq ?? i} className="grid grid-cols-[4rem_8rem_minmax(0,1fr)] gap-3 py-1.5">
              <span className="font-mono text-2xs tabular-nums text-muted-foreground">{offset == null ? "" : clock(offset)}</span>
              <span className={cn("text-xs", ev.kind === "tool_call" || ev.kind === "tool_result" ? "text-muted-foreground" : "font-medium")}>
                {eventNames[ev.kind ?? "unknown"] ?? ev.kind}
              </span>
              <span className="min-w-0 space-y-0.5">
                {ev.tool && (
                  <span className="block font-mono text-xs">
                    {ev.tool.name}
                    {ev.tool.status && <span className="text-muted-foreground"> · {ev.tool.status}</span>}
                    {ev.tool.files && ev.tool.files.length > 0 && <span className="text-muted-foreground"> · {ev.tool.files.join(", ")}</span>}
                  </span>
                )}
                {ev.text && <EventText text={ev.text} />}
                {tokens > 0 && <span className="block text-2xs text-muted-foreground">{num(tokens)} tokens</span>}
              </span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

function EventText({ text }: { text: string }) {
  if (text.length <= 400) return <span className="block whitespace-pre-wrap break-words text-xs">{text}</span>;
  return (
    <details className="text-xs">
      <summary className="cursor-pointer whitespace-pre-wrap break-words">
        {text.slice(0, 400)}… <span className="text-link">show all {num(text.length)} characters</span>
      </summary>
      <span className="block whitespace-pre-wrap break-words">{text}</span>
    </details>
  );
}

function clock(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}
