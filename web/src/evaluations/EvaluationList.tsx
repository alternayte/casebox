import { Link } from "@tanstack/react-router";
import { day, num } from "@/lib/format";
import { cn } from "@/lib/utils";
import { useWorkspaces } from "@/lib/workspaces";
import { ErrorNote, Field, Loading, Select, Tag } from "@/ui/kit";
import { useEvaluations, useOffers, type EvaluationRow } from "./api";
import { ChangeText, ControlledNote, level, pts, ptsInterval, purposeName, StatusTag, usd, VerdictBadge } from "./parts";

export type EvaluationsSearch = { workspace?: string };

// The Evaluations page: every evaluation with its verdict, interval, sample size and cost.
export function EvaluationList({ search, onSearch }: { search: EvaluationsSearch; onSearch: (s: EvaluationsSearch) => void }) {
  const workspaces = useWorkspaces();
  const names = [...new Set([...(workspaces.data ?? []).map((w) => w.name), ...(search.workspace ? [search.workspace] : [])])];
  const list = useEvaluations(search.workspace);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Evaluations</h1>
        <p className="text-sm text-muted-foreground">
          Each evaluation runs a baseline and a candidate that differ in one thing on the same approved cases, and gives a verdict with an interval.
        </p>
      </div>

      <Offers workspaces={search.workspace ? [search.workspace] : (workspaces.data ?? []).map((w) => w.name)} />

      <div className="flex flex-wrap items-end gap-3 border-b pb-2">
        <Field label="Workspace" className="ml-auto w-40">
          <Select value={search.workspace ?? ""} onChange={(e) => onSearch({ workspace: e.target.value || undefined })}>
            <option value="">All</option>
            {names.map((w) => (
              <option key={w} value={w}>
                {w}
              </option>
            ))}
          </Select>
        </Field>
      </div>

      {list.isPending ? <Loading what="evaluations" /> : list.isError ? <ErrorNote error={list.error} /> : <Table rows={list.data} />}

      <ControlledNote className="max-w-3xl border-t pt-3" />
    </div>
  );
}

// "Your harness vs no harness", offered once a workspace has 10 approved dev cases and no such evaluation.
function Offers({ workspaces }: { workspaces: string[] }) {
  const offers = useOffers(workspaces).filter((o) => o.offer?.offer);
  if (offers.length === 0) return null;
  return (
    <div className="space-y-3">
      {offers.map(({ workspace, offer }) => (
        <div key={workspace} className="border-l-2 border-foreground bg-card px-4 py-3">
          <p className="text-sm font-medium">Does the harness of {workspace} earn its tokens?</p>
          <p className="mt-1 text-sm text-muted-foreground">
            {workspace} has {num(offer!.approvedCases)} approved dev cases. Compare the current harness with the same agent and model with every harness file removed. Run
            this in a repository of the workspace; it prints the cost estimate and asks before it starts:
          </p>
          <pre className="mt-2 overflow-x-auto border bg-background px-3 py-2 font-mono text-xs">casebox compare --candidate harness=none</pre>
        </div>
      ))}
    </div>
  );
}

const th = "py-1.5 pr-4 font-medium";
const td = "py-2 pr-4 align-top";

function Table({ rows }: { rows: EvaluationRow[] }) {
  if (rows.length === 0)
    return (
      <p className="text-sm text-muted-foreground">
        No evaluation yet. Start one from a repository with <code className="font-mono text-xs">casebox compare --candidate model=&lt;id&gt;</code>.
      </p>
    );
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[60rem] text-left text-sm">
        <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
          <tr>
            <th className={th}>Evaluation</th>
            <th className={th}>Change</th>
            <th className={th}>Verdict</th>
            <th className={cn(th, "text-right")}>Δ pass rate</th>
            <th className={th}>Interval</th>
            <th className={cn(th, "text-right")}>Sample</th>
            <th className={cn(th, "text-right")}>Spent / estimate</th>
            <th className={th}>Status</th>
            <th className={th}>Purpose</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {rows.map((e) => (
            <Row key={e.id} e={e} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Row({ e }: { e: EvaluationRow }) {
  const v = e.verdict;
  return (
    <tr>
      <td className={cn(td, "whitespace-nowrap")}>
        <Link to="/evaluations/$id" params={{ id: e.id }} className="font-mono text-xs text-link hover:underline" title={e.id}>
          {e.id.slice(-10)}
        </Link>
        <div className="text-2xs text-muted-foreground">
          {e.workspace} · {day(e.createdAt)}
        </div>
      </td>
      <td className={cn(td, "max-w-72")}>
        <ChangeText e={e} />
        {e.mutableModel && (
          <div className="mt-0.5">
            <Tag tone="signal">mutable model</Tag>
          </div>
        )}
      </td>
      <td className={td}>{v ? <VerdictBadge verdict={v.verdict} cheaper={v.equivalentAndCheaper} reason={v.reason} /> : <span className="text-xs text-muted-foreground">None yet</span>}</td>
      <td className={cn(td, "whitespace-nowrap text-right font-mono tabular-nums")}>{v ? pts(v.delta) : "—"}</td>
      <td className={cn(td, "whitespace-nowrap font-mono text-xs tabular-nums")}>
        {v ? (
          <>
            {ptsInterval(v.lower, v.upper)} <span className="text-muted-foreground">({level(v.level)})</span>
          </>
        ) : (
          "—"
        )}
      </td>
      <td className={cn(td, "whitespace-nowrap text-right text-xs tabular-nums")}>
        {v ? (
          <>
            {num(v.cases)} cases
            <div className="text-muted-foreground">{num(v.runs)} runs</div>
          </>
        ) : (
          <>
            {num(e.cases)} cases
            <div className="text-muted-foreground">
              {num(e.runsCompleted)} of {num(e.estimate.runs)} runs
            </div>
          </>
        )}
      </td>
      <td className={cn(td, "whitespace-nowrap text-right text-xs tabular-nums")}>
        {usd(e.spentUsd)}
        <div className="text-muted-foreground">of {usd(e.estimate.totalUsd)}</div>
      </td>
      <td className={td}>
        <StatusTag status={e.status} />
      </td>
      <td className={cn(td, "text-xs")}>
        {purposeName(e.purpose)}
        {e.split === "held_out" && <div className="text-muted-foreground">held-out set</div>}
      </td>
    </tr>
  );
}
