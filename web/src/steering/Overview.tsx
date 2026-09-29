import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { day, interval, num, pct, plural, type Rate } from "@/lib/format";
import {
  agentName,
  groupByName,
  phaseName,
  preventionName,
  preventionPhrase,
  promptModeName,
  sourceName,
  taskTypeName,
  wentWrongName,
} from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Hidden, Loading, Section, ShareBar, Tag } from "@/ui/kit";
import { lastDay, useRefresh, useReport } from "./api";
import { FilterBar } from "./FilterBar";
import { scopeOf } from "./filters";
import type { Filters, Group, Quantiles, Report, Theme } from "./types";

// The steering report (SDD 6): headline and coverage, themes, prevention mix, then groups.
export function Overview({ filters, onFilters }: { filters: Filters; onFilters: (f: Filters) => void }) {
  const report = useReport(filters);
  const r = report.data;

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-end gap-x-6 gap-y-2">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Steering</h1>
          <p className="text-sm text-muted-foreground">
            How often people correct their agents' work, where, and why.
            {r && (
              <>
                {" "}
                Period <span className="font-mono text-foreground">{day(r.period.from)}</span> to{" "}
                <span className="font-mono text-foreground">{lastDay(r.period.to)}</span>.
              </>
            )}
          </p>
        </div>
        <p className="ml-auto max-w-md border-l-2 border-signal pl-3 text-xs text-muted-foreground">
          These numbers are observational. They show what happened, not why it happened. A difference between groups is not a cause.
        </p>
        <DetectNow />
      </div>

      <FilterBar key={JSON.stringify(filters)} value={filters} agents={r?.coverage.agents.map((a) => a.agent) ?? []} onApply={onFilters} />

      {report.isError && <ErrorNote error={report.error} />}
      {report.isPending && <Loading what="the steering report" />}
      {r && (
        <div className={cn("space-y-10", report.isPlaceholderData && "opacity-60")}>
          <NextSteps r={r} />
          <Headline r={r} />
          <Coverage r={r} />
          <Themes r={r} filters={filters} />
          <PreventionMix r={r} />
          {filters.groupBy && <Groups r={r} groupBy={filters.groupBy} />}
        </div>
      )}
    </div>
  );
}

// What to do next, read from coverage. Empty when the report is complete.
function NextSteps({ r }: { r: Report }) {
  const c = r.coverage;
  const steps: ReactNode[] = [];
  if (c.sessions === 0) {
    steps.push(
      <>
        No sessions in this period. Run <Cmd>casebox import --days 30</Cmd> in a repository to import past sessions, or widen the period.
        Each teammate runs <Cmd>casebox join</Cmd> to add their history.
      </>,
    );
  }
  if (!c.workerSeen) {
    steps.push(
      <>
        No worker was seen in the last 10 minutes, so new interventions stay unclassified. Start one with an analysis model:{" "}
        <Cmd>CASEBOX_ANALYSIS_PROVIDER=anthropic CASEBOX_ANALYSIS_MODEL=&lt;model id&gt; casebox worker</Cmd>. Structural numbers still show
        below.
      </>,
    );
  }
  if (c.sessions > 0 && c.interventions === 0) {
    steps.push(<>No interventions are detected yet. Detection runs after an import; a Member can run it now with “Detect now”.</>);
  }
  if (c.pending > 0) {
    steps.push(<>{plural(c.pending, "intervention")} wait for classification. The report fills in as the worker labels them.</>);
  }
  if (c.people === 0 && c.unmappedSessions > 0) {
    steps.push(
      <>
        No session belongs to a mapped person yet. Connect GitHub or Jira with <Cmd>casebox init</Cmd>: their people map each session to one person.
      </>,
    );
  } else if (c.people < r.k && c.sessions > 0) {
    steps.push(
      <>
        Fewer than {r.k} mapped people are in this period, so most numbers stay hidden. Ask teammates to run <Cmd>casebox join</Cmd>.
      </>,
    );
  }
  if (steps.length === 0) return null;
  return (
    <div className="border border-foreground bg-card p-4">
      <h2 className="mb-2 text-sm font-semibold">What to do next</h2>
      <ol className="list-decimal space-y-1.5 pl-5 text-sm">
        {steps.map((s, i) => (
          <li key={i}>{s}</li>
        ))}
      </ol>
    </div>
  );
}

function Cmd({ children }: { children: ReactNode }) {
  return <code className="rounded-sm bg-secondary px-1 font-mono text-xs">{children}</code>;
}

function Headline({ r }: { r: Report }) {
  const h = r.headline;
  return (
    <Section no="1" title="Headline" note="95% Wilson intervals. n is the sample size.">
      <div className="grid gap-px border bg-border sm:grid-cols-2 lg:grid-cols-5">
        <RateCell
          title="Correction-free rate"
          what="Finished work items with no correction in any phase."
          unit="work items"
          rate={h.correctionFreeRate}
          k={r.k}
          lead
        />
        <Cell title="Corrections per work item" what="Median and 75th percentile, by phase.">
          {h.correctionsPerWorkItem ? (
            <>
              <table className="w-full font-mono text-xs tabular-nums">
                <thead>
                  <tr className="text-2xs text-muted-foreground">
                    <th className="text-left font-normal" />
                    <th className="text-right font-normal">median</th>
                    <th className="text-right font-normal">p75</th>
                  </tr>
                </thead>
                <tbody>
                  <QuantRow label="In session" q={h.correctionsPerWorkItem.inSession} />
                  <QuantRow label="Before merge" q={h.correctionsPerWorkItem.beforeMerge} />
                  <QuantRow label="After merge" q={h.correctionsPerWorkItem.afterMerge} signal />
                </tbody>
              </table>
              <N n={h.correctionsPerWorkItem.n} unit="work items" />
            </>
          ) : (
            <Hidden k={r.k} />
          )}
        </Cell>
        <Cell title="Autonomous run" what="Agent turns and tool calls before the first correction.">
          {h.autonomousRun ? (
            <>
              <table className="w-full font-mono text-xs tabular-nums">
                <thead>
                  <tr className="text-2xs text-muted-foreground">
                    <th className="text-left font-normal" />
                    <th className="text-right font-normal">median</th>
                    <th className="text-right font-normal">p75</th>
                  </tr>
                </thead>
                <tbody>
                  <QuantRow label="Turns" q={h.autonomousRun.turns} />
                  <QuantRow label="Tool calls" q={h.autonomousRun.toolCalls} />
                </tbody>
              </table>
              <p className="mt-2 text-xs text-muted-foreground">{pct(h.autonomousRun.uncorrected)} of runs end with no correction.</p>
              <N n={h.autonomousRun.n} unit="runs" />
            </>
          ) : (
            <Hidden k={r.k} />
          )}
        </Cell>
        <RateCell title="After-merge rate" what="Agent pull requests later reverted or fixed." unit="pull requests" rate={h.afterMergeRate} k={r.k} signal />
        <RateCell title="Abandonment rate" what="Sessions dropped with no commit." unit="sessions" rate={h.abandonmentRate} k={r.k} />
      </div>
    </Section>
  );
}

function Cell({ title, what, children, lead }: { title: string; what: string; children: ReactNode; lead?: boolean }) {
  return (
    <div className={cn("flex flex-col bg-card p-4", lead && "bg-background")}>
      <h3 className="text-xs font-semibold">{title}</h3>
      <p className="mb-3 text-2xs text-muted-foreground">{what}</p>
      <div className="mt-auto">{children}</div>
    </div>
  );
}

function RateCell({ title, what, unit, rate, k, lead, signal }: { title: string; what: string; unit: string; rate: Rate | null; k: number; lead?: boolean; signal?: boolean }) {
  return (
    <Cell title={title} what={what} lead={lead}>
      {rate ? (
        <>
          <div className={cn("font-mono tabular-nums", lead ? "text-3xl" : "text-2xl", signal && "text-signal")}>{pct(rate.value)}</div>
          <div className="my-2">
            <ShareBar share={rate.value} interval={rate.interval} tone={signal ? "signal" : "ink"} />
          </div>
          <p className="font-mono text-xs tabular-nums text-muted-foreground">95% interval {interval(rate.interval)}</p>
          <N n={rate.n} unit={unit} />
        </>
      ) : (
        <Hidden k={k} />
      )}
    </Cell>
  );
}

function QuantRow({ label, q, signal }: { label: string; q: Quantiles | null; signal?: boolean }) {
  return (
    <tr className={cn(signal && "text-signal")}>
      <td className="py-0.5 font-sans">{label}</td>
      <td className="text-right">{q ? num(q.median) : "–"}</td>
      <td className="text-right">{q ? num(q.p75) : "–"}</td>
    </tr>
  );
}

function N({ n, unit }: { n: number; unit: string }) {
  return (
    <p className="mt-1 font-mono text-2xs tabular-nums text-muted-foreground">
      n = {num(n)} {unit}
    </p>
  );
}

function Coverage({ r }: { r: Report }) {
  const c = r.coverage;
  const rows: [string, ReactNode][] = [
    ["Sessions", num(c.sessions)],
    ["Mapped people", <>{num(c.people)} <span className="text-muted-foreground">(k = {r.k})</span></>],
    ["Sessions of unmapped people", <>{num(c.unmappedSessions)} <span className="text-muted-foreground">never count toward k</span></>],
    ["Prompt mode", <span>{promptModeName(c.promptMode)}</span>],
    ["Interventions", num(c.interventions)],
    ["Waiting for classification", num(c.pending)],
    ["Unclassified", num(c.unclassified)],
    ["Worker", c.workerSeen ? <span>Seen</span> : <span className="text-signal">Not seen</span>],
  ];
  return (
    <Section title="Coverage" note="What the numbers above rest on.">
      <div className="grid gap-6 lg:grid-cols-[2fr_3fr]">
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 text-sm">
          {rows.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="font-mono tabular-nums [&>span]:font-sans">{v}</dd>
            </div>
          ))}
        </dl>
        <table className="w-full self-start text-sm">
          <thead>
            <tr className="border-b text-left text-2xs uppercase tracking-label text-muted-foreground">
              <th className="py-1 font-medium">Agent</th>
              <th className="py-1 text-right font-medium">Sessions</th>
              <th className="py-1 pl-6 font-medium">Sources</th>
            </tr>
          </thead>
          <tbody>
            {c.agents.length === 0 && (
              <tr>
                <td colSpan={3} className="py-2 text-muted-foreground">
                  No agent sessions in this period.
                </td>
              </tr>
            )}
            {c.agents.map((a) => (
              <tr key={a.agent} className="border-b border-dashed">
                <td className="py-1">{agentName(a.agent)}</td>
                <td className="py-1 text-right font-mono tabular-nums">{num(a.sessions)}</td>
                <td className="py-1 pl-6 text-muted-foreground">{a.sources.map(sourceName).join(", ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Section>
  );
}

function Themes({ r, filters }: { r: Report; filters: Filters }) {
  const hidden = r.hidden.themes ?? 0;
  const max = Math.max(1, ...r.themes.map((t) => t.corrections));
  return (
    <Section
      no="2"
      title="Top correction themes"
      note={
        <>
          What went wrong, by corrections. Only corrections count as steering.{" "}
          {r.coverage.unclassified > 0 && (
            <Link to="/steering/themes/$wentWrong" params={{ wentWrong: "unclassified" }} search={scopeOf(filters)} className="text-link hover:underline">
              Review {plural(r.coverage.unclassified, "unclassified intervention")}
            </Link>
          )}
        </>
      }
    >
      {r.themes.length === 0 && hidden === 0 && <p className="text-sm text-muted-foreground">No classified corrections in this period.</p>}
      <ol className="divide-y border-y">
        {r.themes.map((t, i) => (
          <ThemeRow key={`${t.wentWrong}:${t.label ?? ""}`} rank={i + 1} t={t} max={max} filters={filters} />
        ))}
      </ol>
      {hidden > 0 && (
        <p className="mt-2 text-xs text-muted-foreground">
          {plural(hidden, "more theme")}: <Hidden k={r.k} />
        </p>
      )}
    </Section>
  );
}

function ThemeRow({ rank, t, max, filters }: { rank: number; t: Theme; max: number; filters: Filters }) {
  const phases = [
    ["in_session", t.phases.inSession],
    ["before_merge", t.phases.beforeMerge],
    ["after_merge", t.phases.afterMerge],
  ] as const;
  return (
    <li className="grid gap-4 py-4 lg:grid-cols-[2rem_minmax(0,5fr)_minmax(0,4fr)_minmax(0,6fr)]">
      <span className="font-mono text-xs text-muted-foreground">{String(rank).padStart(2, "0")}</span>
      <div className="space-y-2">
        <Link
          to="/steering/themes/$wentWrong"
          params={{ wentWrong: t.wentWrong }}
          search={scopeOf(filters)}
          className="text-base font-semibold hover:text-link hover:underline"
        >
          {wentWrongName(t.wentWrong, t.label)}
        </Link>
        <div className="flex items-baseline gap-3 font-mono text-xs tabular-nums">
          <span className="text-lg text-signal">{num(t.corrections)}</span>
          <span className="text-muted-foreground">corrections</span>
          <span>{num(t.people)}</span>
          <span className="text-muted-foreground">people</span>
        </div>
        <ShareBar share={t.corrections / max} tone="signal" />
        {t.harnessFixable ? <Tag tone="ok">harness can fix</Tag> : <Tag>outside the harness</Tag>}
        {t.patterns && t.patterns.length > 0 && (
          <ul className="space-y-0.5 text-xs">
            {t.patterns.map((p) => (
              <li key={p.id}>
                Pattern:{" "}
                <Link to="/patterns/$id" params={{ id: p.id }} className="text-link hover:underline">
                  {p.title}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="space-y-3 text-xs">
        <div>
          <h4 className="mb-1 text-2xs uppercase tracking-label text-muted-foreground">Would have prevented it</h4>
          <ul className="space-y-0.5">
            {t.prevention.map((p) => (
              <li key={p.prevention} className="flex justify-between gap-2">
                <span>{preventionName(p.prevention)}</span>
                <span className="font-mono tabular-nums">{pct(p.share)}</span>
              </li>
            ))}
          </ul>
        </div>
        <div>
          <h4 className="mb-1 text-2xs uppercase tracking-label text-muted-foreground">Phase</h4>
          <ul className="flex flex-wrap gap-x-4">
            {phases.map(([phase, count]) => (
              <li key={phase} className={cn(phase === "after_merge" && count > 0 && "text-signal")}>
                {phaseName(phase)} <span className="font-mono tabular-nums">{num(count)}</span>
              </li>
            ))}
          </ul>
        </div>
      </div>
      <div>
        {t.quotes.length === 0 ? (
          <p className="text-xs text-muted-foreground">No quotes. Prompt mode may be off, or the signals hold no text.</p>
        ) : (
          <ul className="space-y-2">
            {t.quotes.map((q) => (
              <li key={q.ref} className="border-l-2 border-border pl-3">
                <blockquote className="line-clamp-4 font-serif text-sm italic">“{q.text}”</blockquote>
                <span className="font-mono text-2xs text-muted-foreground">{q.day}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

function PreventionMix({ r }: { r: Report }) {
  const inside = r.preventionMix.filter((p) => p.harnessFixable);
  const outside = r.preventionMix.filter((p) => !p.harnessFixable);
  const lead = [...r.preventionMix].sort((a, b) => b.share - a.share).slice(0, 2);
  const total = r.preventionMix.reduce((sum, p) => sum + p.corrections, 0);
  return (
    <Section no="3" title="Prevention mix" note="What would have prevented each correction, as the classifier or a person labelled it.">
      {r.preventionMix.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No prevention shares to show. {(r.hidden.preventionMix ?? 0) > 0 && <Hidden k={r.k} />}
        </p>
      ) : (
        <>
          <p className="mb-5 max-w-3xl text-base">
            {lead.map((p, i) => (
              <span key={p.prevention}>
                {i > 0 && "; "}
                <span className="font-mono font-semibold">{pct(p.share)}</span> {i === 0 ? "of corrections " : ""}{preventionPhrase(p.prevention)}
              </span>
            ))}
            . <span className="font-mono text-xs text-muted-foreground">n = {num(total)} corrections</span>
          </p>
          <div className="grid gap-8 lg:grid-cols-2">
            <MixList title="The harness can fix" rows={inside} tone="ink" />
            <MixList title="Outside the harness" rows={outside} tone="muted" />
          </div>
          {Object.entries(r.hidden)
            .filter(([key, count]) => key.startsWith("prevention") && count > 0)
            .map(([key, count]) => (
              <p key={key} className="mt-2 text-xs text-muted-foreground">
                {plural(count, "more share")}: <Hidden k={r.k} />
              </p>
            ))}
        </>
      )}
      <AfterMerge r={r} />
    </Section>
  );
}

function MixList({ title, rows, tone }: { title: string; rows: Report["preventionMix"]; tone: "ink" | "muted" }) {
  return (
    <div>
      <h3 className="mb-2 text-2xs font-medium uppercase tracking-label text-muted-foreground">{title}</h3>
      {rows.length === 0 ? (
        <p className="text-xs text-muted-foreground">None in this period.</p>
      ) : (
        <ul className="space-y-2">
          {rows.map((p) => (
            <li key={p.prevention} className="grid grid-cols-[minmax(0,1fr)_3rem_4rem] items-center gap-3 text-sm">
              <span className="truncate">{preventionName(p.prevention)}</span>
              <span className="text-right font-mono tabular-nums">{pct(p.share)}</span>
              <span className="text-right font-mono text-2xs tabular-nums text-muted-foreground">n = {num(p.corrections)}</span>
              <div className="col-span-3">
                <ShareBar share={p.share} tone={tone} />
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// After-merge corrections cost the most, so they stand apart.
function AfterMerge({ r }: { r: Report }) {
  return (
    <div className="mt-8 border-l-4 border-signal bg-card p-4">
      <h3 className="text-sm font-semibold">After merge</h3>
      <p className="mb-2 text-xs text-muted-foreground">Reverts and fixes of merged agent pull requests. They cost the most and are the strongest evidence.</p>
      {r.afterMerge ? (
        <div className="flex flex-wrap gap-x-8 font-mono text-sm tabular-nums">
          <span>
            <span className="text-xl text-signal">{num(r.afterMerge.reverts)}</span> {r.afterMerge.reverts === 1 ? "revert" : "reverts"}
          </span>
          <span>
            <span className="text-xl text-signal">{num(r.afterMerge.fixes)}</span> {r.afterMerge.fixes === 1 ? "fix" : "fixes"}
          </span>
          <span className="text-muted-foreground">{plural(r.afterMerge.people, "person", "people")} behind them</span>
        </div>
      ) : (
        <Hidden k={r.k} />
      )}
    </div>
  );
}

function groupKeyName(groupBy: string, key: string) {
  if (groupBy === "agent") return agentName(key);
  if (groupBy === "taskType") return taskTypeName(key);
  return key;
}

function Groups({ r, groupBy }: { r: Report; groupBy: string }) {
  const hidden = r.hidden.groups ?? 0;
  return (
    <Section no="4" title={`By ${groupByName(groupBy).toLowerCase()}`} note="Each group has at least k people behind it. Groups are compared by observation only.">
      <div className="overflow-x-auto">
        <table className="w-full min-w-[48rem] text-sm">
          <thead>
            <tr className="border-b-2 border-foreground text-left text-2xs uppercase tracking-label text-muted-foreground">
              <th className="py-1.5 pr-4 font-medium">{groupByName(groupBy)}</th>
              <th className="py-1.5 pr-4 text-right font-medium">People</th>
              <th className="py-1.5 pr-4 text-right font-medium">Sessions</th>
              <th className="py-1.5 pr-4 font-medium">Correction-free</th>
              <th className="py-1.5 pr-4 font-medium">Abandonment</th>
              <th className="py-1.5 pr-4 font-medium">After merge</th>
              <th className="py-1.5 text-right font-medium">Corrections</th>
            </tr>
          </thead>
          <tbody>
            {r.groups.length === 0 && (
              <tr>
                <td colSpan={7} className="py-3 text-muted-foreground">
                  No group has at least {r.k} people behind it.
                </td>
              </tr>
            )}
            {r.groups.map((g: Group) => (
              <tr key={g.key} className="border-b border-dashed align-top">
                <td className={cn("py-1.5 pr-4", groupBy === "harness" && "font-mono text-xs")}>{groupKeyName(groupBy, g.key)}</td>
                <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{num(g.people)}</td>
                <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{num(g.sessions)}</td>
                <RateTd rate={g.correctionFreeRate} k={r.k} />
                <RateTd rate={g.abandonmentRate} k={r.k} />
                <RateTd rate={g.afterMergeRate} k={r.k} />
                <td className="py-1.5 text-right font-mono tabular-nums">{num(g.corrections)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {hidden > 0 && (
        <p className="mt-2 text-xs text-muted-foreground">
          {plural(hidden, "more group")}: <Hidden k={r.k} />
        </p>
      )}
    </Section>
  );
}

function RateTd({ rate, k }: { rate: Rate | null; k: number }) {
  return (
    <td className="py-1.5 pr-4">
      {rate ? (
        <div className="w-40">
          <div className="flex items-baseline justify-between font-mono text-xs tabular-nums">
            <span className="text-sm">{pct(rate.value)}</span>
            <span className="text-muted-foreground">{interval(rate.interval)}</span>
          </div>
          <ShareBar share={rate.value} interval={rate.interval} />
          <span className="font-mono text-2xs text-muted-foreground">n = {num(rate.n)}</span>
        </div>
      ) : (
        <Hidden k={k} />
      )}
    </td>
  );
}

// A Member can run detection now instead of waiting for the next scheduled run.
export function DetectNow() {
  const me = useMe();
  const refresh = useRefresh();
  if (!isMember(me.data)) return null;
  return (
    <div className="flex items-center gap-2">
      <Button onClick={() => refresh.mutate(undefined)} disabled={refresh.isPending}>
        {refresh.isPending ? "Detecting…" : "Detect now"}
      </Button>
      {refresh.data && (
        <span className="font-mono text-2xs text-muted-foreground">
          {num(refresh.data.interventions)} interventions, {num(refresh.data.pending)} pending, {plural(refresh.data.analysisWorkers, "analysis worker")}
        </span>
      )}
      {refresh.isError && <ErrorNote error={refresh.error} />}
    </div>
  );
}
