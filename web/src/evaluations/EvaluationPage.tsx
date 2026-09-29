import { Link } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { ApiError } from "@/lib/api";
import { day, num, plural } from "@/lib/format";
import { agentName } from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Loading, Section, Tag } from "@/ui/kit";
import { useCancel, useCaseResults, useConfirm, useEvaluation, type Checkpoint, type EvaluationRow, type HarnessSpec } from "./api";
import {
  changeName,
  ControlledNote,
  IntervalPlot,
  level,
  minutes,
  plotDomain,
  pts,
  ptsInterval,
  purposeName,
  rate,
  ratio,
  ratioInterval,
  settingsText,
  sideValue,
  StatusTag,
  usd,
  VerdictBadge,
  verdictRule,
} from "./parts";

// One evaluation: the change, both sides, the estimate against the spend, each checkpoint, the
// verdict and the per-case results.
export function EvaluationPage({ id }: { id: string }) {
  const query = useEvaluation(id);
  const me = useMe();

  if (query.isPending) return <Loading what="the evaluation" />;
  if (query.isError)
    return query.error instanceof ApiError && query.error.status === 404 ? (
      <p className="text-sm">This evaluation does not exist.</p>
    ) : (
      <ErrorNote error={query.error} />
    );

  const { evaluation: e, checkpoints } = query.data;
  const member = isMember(me.data);
  const heldOut = e.split === "held_out";

  return (
    <div className="space-y-8">
      <div>
        <Link to="/evaluations" className="text-xs text-link hover:underline">
          ← Evaluations
        </Link>
        <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-2xl font-semibold tracking-tight">
          <span>
            {changeName(e.change)}: {sideValue(e.baseline, e.change)} → {sideValue(e.candidate, e.change)}
          </span>
          <span className="flex items-center gap-1.5 self-center text-base">
            <StatusTag status={e.status} />
            {e.verdict && <VerdictBadge verdict={e.verdict.verdict} cheaper={e.verdict.equivalentAndCheaper} />}
          </span>
        </h1>
        <p className="mt-1 text-xs text-muted-foreground">
          <span className="font-mono">{e.id}</span> · {purposeName(e.purpose)} · workspace {e.workspace} · {heldOut ? "held-out set" : "dev set"} ·{" "}
          {plural(e.cases, "case")} × 2 sides × {plural(e.repeats, "repeat")} · δ ±{(e.delta * 100).toFixed(1)} pts · requested {day(e.createdAt)}
        </p>
      </div>

      {e.mutableModel && (
        <p role="note" className="border-l-2 border-signal py-1 pl-3 text-sm">
          A model ID here ({[...new Set([e.baseline.model, e.candidate.model])].join(", ")}) names no fixed version. The provider can change the model behind it, so this
          result may not repeat.
        </p>
      )}

      {member && (e.status === "awaiting_confirmation" || e.status === "running") && <Actions e={e} />}
      {!member && e.status === "awaiting_confirmation" && (
        <p className="text-sm text-muted-foreground">This evaluation waits for a Member to confirm its estimate.</p>
      )}

      <div className="grid gap-10 lg:grid-cols-[minmax(0,1fr)_24rem]">
        <div className="space-y-10">
          <Section title="Verdict" note={e.verdict ? `${level(e.verdict.level)} interval` : undefined}>
            <VerdictBlock e={e} />
          </Section>
          <Section title="Checkpoints" note="After each full round. Interim checks use 99% intervals; the final check uses 95%.">
            <Checkpoints e={e} checkpoints={checkpoints} />
          </Section>
        </div>
        <div className="space-y-10">
          <Section title="Sides">
            <Sides e={e} />
          </Section>
          <Section title="Cost">
            <Cost e={e} />
          </Section>
        </div>
      </div>

      <Section title="Cases" note="Pass rates per side, with the count of completed runs behind each.">
        {heldOut ? (
          <p className="text-sm text-muted-foreground">Held-out results are hidden. A held-out evaluation shows only its verdict, counts and cost.</p>
        ) : (
          <Cases e={e} />
        )}
      </Section>
    </div>
  );
}

function Actions({ e }: { e: EvaluationRow }) {
  const confirm = useConfirm(e.id);
  const cancel = useCancel(e.id);
  const [cancelling, setCancelling] = useState(false);
  return (
    <div className="flex flex-wrap items-center gap-3 border border-input bg-card px-4 py-3 text-sm">
      {e.status === "awaiting_confirmation" && (
        <>
          <span>
            The estimate of {usd(e.estimate.totalUsd)} is above the confirmation threshold. No run starts until a Member confirms it.
          </span>
          <Button variant="solid" onClick={() => confirm.mutate()} disabled={confirm.isPending}>
            {confirm.isPending ? "Confirming…" : `Confirm ${usd(e.estimate.totalUsd)}`}
          </Button>
        </>
      )}
      {e.status === "running" && (
        <span>
          Running: {num(e.runsCompleted)} of {num(e.estimate.runs)} runs complete, {num(e.runsFailed)} failed to run.
        </span>
      )}
      {cancelling ? (
        <span className="flex items-center gap-2">
          <span className="text-xs">Cancel it? No new run starts; recorded costs stay.</span>
          <Button onClick={() => cancel.mutate("cancelled in the UI", { onSuccess: () => setCancelling(false) })} disabled={cancel.isPending}>
            {cancel.isPending ? "Cancelling…" : "Cancel evaluation"}
          </Button>
          <Button variant="quiet" onClick={() => setCancelling(false)}>
            Keep it
          </Button>
        </span>
      ) : (
        <Button onClick={() => setCancelling(true)}>Cancel…</Button>
      )}
      {confirm.isError && <ErrorNote error={confirm.error} />}
      {cancel.isError && <ErrorNote error={cancel.error} />}
    </div>
  );
}

function VerdictBlock({ e }: { e: EvaluationRow }) {
  const v = e.verdict;
  if (!v) {
    const text =
      e.status === "cancelled"
        ? `Cancelled before a verdict: ${e.reason ?? "no reason given"}.`
        : e.status === "awaiting_confirmation"
          ? "No verdict yet. The evaluation waits for confirmation."
          : "No verdict yet. The runner checks after each full round and stops early on better, worse or equivalent.";
    return (
      <div className="space-y-3">
        <p className="text-sm text-muted-foreground">{text}</p>
        <ControlledNote />
      </div>
    );
  }
  const domain = plotDomain([v], e.delta);
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <VerdictBadge verdict={v.verdict} cheaper={v.equivalentAndCheaper} />
        <span className="font-mono text-2xl tabular-nums">{pts(v.delta)}</span>
        <span className="font-mono text-sm tabular-nums text-muted-foreground">
          {level(v.level)} interval {ptsInterval(v.lower, v.upper)}
        </span>
      </div>
      <div className="max-w-xl">
        <IntervalPlot value={v.delta} lower={v.lower} upper={v.upper} delta={e.delta} domain={domain} />
        <Axis domain={domain} />
      </div>
      <p className="text-sm">
        <span className="font-medium">Rule met: </span>
        {verdictRule(v, e.delta)}
      </p>
      {v.equivalentAndCheaper && (
        <p className="text-sm">
          <span className="font-medium">Equivalent and cheaper: </span>the same pass rate within ±{(e.delta * 100).toFixed(1)} pts, and the cost interval lies entirely
          below 1.
        </p>
      )}
      <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-[12rem_minmax(0,1fr)]">
        <Fact term="Pass rate, baseline">
          {(v.baselineRate * 100).toFixed(1)}% <N>weighted mean over {plural(v.cases, "case")}</N>
        </Fact>
        <Fact term="Pass rate, candidate">
          {(v.candidateRate * 100).toFixed(1)}% <N>weighted mean over {plural(v.cases, "case")}</N>
        </Fact>
        <Fact term="Sample">
          {plural(v.cases, "case")} with runs on both sides, {plural(v.runs, "completed run")}
        </Fact>
        <Fact term="Cost per task, candidate ÷ baseline">
          {v.costRatio == null ? (
            <N>not available</N>
          ) : (
            <>
              <span className="font-mono tabular-nums">{ratio(v.costRatio)}</span>{" "}
              <N>
                {level(v.level)} interval {ratioInterval(v.costLower, v.costUpper)}
              </N>
            </>
          )}
        </Fact>
        <Fact term="Duration, candidate ÷ baseline">
          {v.durationRatio == null ? (
            <N>not available</N>
          ) : (
            <>
              <span className="font-mono tabular-nums">{ratio(v.durationRatio)}</span>{" "}
              <N>
                {level(v.level)} interval {ratioInterval(v.durationLower, v.durationUpper)}
              </N>
            </>
          )}
        </Fact>
      </dl>
      <p className="text-xs text-muted-foreground">
        Δ is the weighted mean over cases of the candidate's pass rate minus the baseline's. Its interval comes from a paired bootstrap over cases (10,000 resamples). The
        ratios are geometric means of per-case ratios with the same bootstrap. Cases with harness drift weigh 0.5.
      </p>
      <ControlledNote />
    </div>
  );
}

function N({ children }: { children: ReactNode }) {
  return <span className="text-xs text-muted-foreground">({children})</span>;
}

function Fact({ term, children }: { term: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted-foreground">{term}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function Axis({ domain }: { domain: number }) {
  return (
    <div className="mt-1 flex justify-between font-mono text-2xs text-muted-foreground">
      <span>{pts(-domain)}</span>
      <span>0</span>
      <span>{pts(domain)}</span>
    </div>
  );
}

function Checkpoints({ e, checkpoints }: { e: EvaluationRow; checkpoints: Checkpoint[] }) {
  if (checkpoints.length === 0)
    return <p className="text-sm text-muted-foreground">No checkpoint yet. The first comes after one repeat of every case on both sides.</p>;
  const domain = plotDomain(checkpoints, e.delta);
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[40rem] text-left text-sm">
        <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
          <tr>
            <th className="py-1.5 pr-4 font-medium">Round</th>
            <th className="py-1.5 pr-4 font-medium">Level</th>
            <th className="py-1.5 pr-4 text-right font-medium">Cases</th>
            <th className="py-1.5 pr-4 text-right font-medium">Δ</th>
            <th className="py-1.5 pr-4 font-medium">Interval</th>
            <th className="w-48 py-1.5 pr-4 font-medium">
              <span className="sr-only">Plot</span>
            </th>
            <th className="py-1.5 font-medium">Verdict</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {checkpoints.map((c) => (
            <tr key={c.round}>
              <td className="py-2 pr-4 font-mono tabular-nums">{c.round}</td>
              <td className="py-2 pr-4 font-mono text-xs tabular-nums">{level(c.level)}</td>
              <td className="py-2 pr-4 text-right font-mono tabular-nums">{num(c.cases)}</td>
              <td className="whitespace-nowrap py-2 pr-4 text-right font-mono tabular-nums">{pts(c.delta)}</td>
              <td className="whitespace-nowrap py-2 pr-4 font-mono text-xs tabular-nums">{ptsInterval(c.lower, c.upper)}</td>
              <td className="py-2 pr-4">
                <IntervalPlot value={c.delta} lower={c.lower} upper={c.upper} delta={e.delta} domain={domain} />
              </td>
              <td className="py-2">
                <VerdictBadge verdict={c.verdict} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-xs text-muted-foreground">
        The plot runs from {pts(-domain)} to {pts(domain)}. The shaded band is ±δ; the line is 0.
      </p>
    </div>
  );
}

function Sides({ e }: { e: EvaluationRow }) {
  const rows: [string, string, (s: HarnessSpec) => string][] = [
    ["Agent", "agent", (s) => agentName(s.agent)],
    ["Agent version", "agentVersion", (s) => s.agentVersion || "—"],
    ["Model", "model", (s) => s.model],
    ["Effort", "effort", (s) => s.effort ?? "default"],
    ["Harness", "harness", (s) => (s.harness === "none" ? "none (every harness file removed)" : s.harness)],
    ["Settings", "settings", settingsText],
  ];
  if (e.baseline.command || e.candidate.command) rows.push(["Command", "command", (s) => s.command?.template ?? "—"]);
  return (
    <table className="w-full text-left text-xs">
      <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
        <tr>
          <th className="py-1.5 pr-3 font-medium" />
          <th className="py-1.5 pr-3 font-medium">Baseline</th>
          <th className="py-1.5 font-medium">Candidate</th>
        </tr>
      </thead>
      <tbody className="divide-y">
        {rows.map(([label, key, value]) => {
          const changed = key === e.change;
          return (
            <tr key={key} className={cn(changed && "bg-secondary")}>
              <td className="py-1.5 pr-3 text-muted-foreground">
                {label}
                {changed && (
                  <span className="ml-1.5">
                    <Tag tone="signal">change</Tag>
                  </span>
                )}
              </td>
              <td className="break-all py-1.5 pr-3 font-mono">{value(e.baseline)}</td>
              <td className="break-all py-1.5 font-mono">{value(e.candidate)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function Cost({ e }: { e: EvaluationRow }) {
  const est = e.estimate;
  const share = est.totalUsd > 0 ? Math.min(1, e.spentUsd / est.totalUsd) : 0;
  return (
    <div className="space-y-4 text-sm">
      <div>
        <div className="flex items-baseline justify-between gap-3">
          <span className="font-mono text-lg tabular-nums">{usd(e.spentUsd)}</span>
          <span className="text-xs text-muted-foreground">of {usd(est.totalUsd)} estimated</span>
        </div>
        <div className="relative mt-1 h-2 bg-secondary" aria-hidden>
          <div className={cn("absolute inset-y-0 left-0", e.spentUsd > est.totalUsd ? "bg-signal" : "bg-foreground")} style={{ width: `${share * 100}%` }} />
        </div>
        <p className="mt-1 text-xs text-muted-foreground">
          Spend is the sum of {plural(e.runsCompleted, "recorded run")}. The cap is {usd(e.capUsd)}; no round starts that would pass it.
        </p>
      </div>
      <table className="w-full text-left text-xs">
        <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
          <tr>
            <th className="py-1.5 pr-3 font-medium">Estimate</th>
            <th className="py-1.5 pr-3 text-right font-medium">Tokens</th>
            <th className="py-1.5 text-right font-medium">Cost</th>
          </tr>
        </thead>
        <tbody className="divide-y font-mono tabular-nums">
          <tr>
            <td className="py-1.5 pr-3 font-sans">Baseline</td>
            <td className="py-1.5 pr-3 text-right">{num(est.baselineTokens)}</td>
            <td className="py-1.5 text-right">{usd(est.baselineUsd)}</td>
          </tr>
          <tr>
            <td className="py-1.5 pr-3 font-sans">Candidate</td>
            <td className="py-1.5 pr-3 text-right">{num(est.candidateTokens)}</td>
            <td className="py-1.5 text-right">{usd(est.candidateUsd)}</td>
          </tr>
          <tr className="font-medium">
            <td className="py-1.5 pr-3 font-sans">Total</td>
            <td className="py-1.5 pr-3 text-right">{num(est.baselineTokens + est.candidateTokens)}</td>
            <td className="py-1.5 text-right">{usd(est.totalUsd)}</td>
          </tr>
        </tbody>
      </table>
      <dl className="grid grid-cols-[9rem_minmax(0,1fr)] gap-y-1 text-xs">
        <Fact term="Runs">
          {num(e.runsCompleted)} completed, {num(e.runsFailed)} failed to run, of {num(est.runs)} planned
        </Fact>
        <Fact term="Per round">{usd(est.perRoundUsd)}</Fact>
        <Fact term="Sandbox time">{minutes(est.sandboxMinutes)}</Fact>
        <Fact term="Duration range">
          {minutes(est.minMinutes)} to {minutes(est.maxMinutes)}
        </Fact>
      </dl>
      <p className="text-xs text-muted-foreground">
        The estimate uses each case's past runs, else its source session, else 400,000 tokens and 12 minutes a run, priced from casebox.yml.
      </p>
    </div>
  );
}

function Cases({ e }: { e: EvaluationRow }) {
  const query = useCaseResults(e.id, true, e.status);
  if (query.isPending) return <Loading what="the per-case results" />;
  if (query.isError)
    return query.error instanceof ApiError && query.error.status === 403 ? (
      <p className="text-sm text-muted-foreground">Held-out results are hidden. A held-out evaluation shows only its verdict, counts and cost.</p>
    ) : (
      <ErrorNote error={query.error} />
    );
  const rows = query.data;
  if (rows.length === 0) return <p className="text-sm text-muted-foreground">No run has a result yet.</p>;
  const cell = "py-2 pr-4 align-top";
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[56rem] text-left text-sm">
        <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
          <tr>
            <th className="py-1.5 pr-4 font-medium">Case</th>
            <th className="py-1.5 pr-4 font-medium">Weight</th>
            <th className="py-1.5 pr-4 text-right font-medium">Baseline</th>
            <th className="py-1.5 pr-4 text-right font-medium">Candidate</th>
            <th className="py-1.5 pr-4 text-right font-medium">Failed runs</th>
            <th className="py-1.5 pr-4 text-right font-medium">Baseline cost</th>
            <th className="py-1.5 pr-4 text-right font-medium">Candidate cost</th>
            <th className="py-1.5 font-medium">Runs</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {rows.map((c) => (
            <tr key={c.caseId}>
              <td className={cn(cell, "whitespace-nowrap font-mono text-xs")}>
                <Link to="/cases/$id" params={{ id: c.caseId }} className="text-link hover:underline" title={c.caseId}>
                  {c.caseId.slice(0, 10)}
                </Link>
              </td>
              <td className={cn(cell, "text-xs")}>
                <span className="inline-flex items-center gap-1.5">
                  <span className="font-mono tabular-nums">{num(c.weight)}</span>
                  {c.drift && <Tag tone="signal">drift</Tag>}
                </span>
              </td>
              <td className={cn(cell, "whitespace-nowrap text-right font-mono tabular-nums")}>
                {rate(c.baselinePassed, c.baselineRuns)}{" "}
                <span className="text-xs text-muted-foreground">
                  {c.baselinePassed}/{c.baselineRuns}
                </span>
              </td>
              <td className={cn(cell, "whitespace-nowrap text-right font-mono tabular-nums")}>
                {rate(c.candidatePassed, c.candidateRuns)}{" "}
                <span className="text-xs text-muted-foreground">
                  {c.candidatePassed}/{c.candidateRuns}
                </span>
              </td>
              <td className={cn(cell, "text-right font-mono tabular-nums", c.failedRuns > 0 && "text-signal")}>{num(c.failedRuns)}</td>
              <td className={cn(cell, "whitespace-nowrap text-right font-mono text-xs tabular-nums")}>{usd(c.baselineCostUsd)}</td>
              <td className={cn(cell, "whitespace-nowrap text-right font-mono text-xs tabular-nums")}>{usd(c.candidateCostUsd)}</td>
              <td className={cn(cell, "text-xs")}>
                <span className="flex flex-wrap gap-x-2 gap-y-0.5">
                  {c.runs.map((r) => (
                    <Link key={r} to="/evaluations/$id/runs/$runId" params={{ id: e.id, runId: r }} className="font-mono text-link hover:underline" title={r}>
                      {r.slice(-6)}
                    </Link>
                  ))}
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-xs text-muted-foreground">
        A pass rate counts completed runs only; a failed run is an infrastructure failure and counts on neither side. Costs are the sums of recorded runs.
      </p>
    </div>
  );
}
