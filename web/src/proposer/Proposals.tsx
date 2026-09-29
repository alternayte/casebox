import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { ApiError } from "@/lib/api";
import { day } from "@/lib/format";
import { isMember, useMe } from "@/lib/session";
import { useWorkspaces } from "@/lib/workspaces";
import { Button, ErrorNote, Field, Input, Loading, Section, Select, Tag } from "@/ui/kit";
import { opName, statusName, useProposal, useProposals, useReject, type Candidate, type Proposal } from "./api";

export type ProposalsSearch = { workspace?: string };

const pts = (v: number) => `${v >= 0 ? "+" : ""}${(v * 100).toFixed(1)} pts`;

// Proposals: candidates and their dev-batch scores, the held-out gate, the pull request and the outcome.
export function ProposalList({ search, onSearch }: { search: ProposalsSearch; onSearch: (s: ProposalsSearch) => void }) {
  const proposals = useProposals(search.workspace);
  const workspaces = useWorkspaces();
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Proposals</h1>
        <p className="max-w-3xl text-sm text-muted-foreground">
          Small harness edits drafted for a pattern, or removals from the harness diet. Each is scored on dev cases, then tested on held-out cases the proposer never
          sees. A pull request opens only when that gate passes, and a person merges it.
        </p>
      </div>
      <div className="flex flex-wrap items-end gap-4 border-b pb-3">
        <Field label="Workspace">
          <Select value={search.workspace ?? ""} onChange={(e) => onSearch({ workspace: e.target.value || undefined })}>
            <option value="">All workspaces</option>
            {workspaces.data?.map((w) => (
              <option key={w.name} value={w.name}>
                {w.name}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {proposals.isPending ? (
        <Loading what="proposals" />
      ) : proposals.isError ? (
        <ErrorNote error={proposals.error} />
      ) : proposals.data.length === 0 ? (
        <p className="text-sm text-muted-foreground">No proposal yet. Run casebox propose --scheduled, or add the weekly schedule to the harness CI workflow.</p>
      ) : (
        <ul className="divide-y border-y">
          {proposals.data.map((p) => (
            <li key={p.id} className="flex flex-wrap items-baseline justify-between gap-2 py-2.5 text-sm">
              <span className="min-w-0">
                <Link to="/proposals/$id" params={{ id: p.id }} className="text-link hover:underline">
                  {p.kind === "removal" ? "Harness diet: removal" : (p.patternTitle ?? "Harness edit")}
                </Link>
                <span className="ml-2 text-xs text-muted-foreground">
                  {p.workspace} · {day(p.createdAt)}
                </span>
              </span>
              <StatusBadge p={p} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function StatusBadge({ p }: { p: Proposal }) {
  const tone = p.status === "gate_passed" || p.status === "pr_opened" || p.status === "merged" ? "ok" : p.status === "gate_failed" || p.status === "rejected" ? "signal" : "plain";
  return <Tag tone={tone}>{statusName(p.status)}</Tag>;
}

export function ProposalPage({ id }: { id: string }) {
  const proposal = useProposal(id);
  const me = useMe();
  if (proposal.isPending) return <Loading what="the proposal" />;
  if (proposal.isError)
    return proposal.error instanceof ApiError && proposal.error.status === 404 ? <p className="text-sm">This proposal does not exist.</p> : <ErrorNote error={proposal.error} />;
  const { proposal: p, candidates } = proposal.data;
  return (
    <div className="space-y-8">
      <div>
        <Link to="/proposals" className="text-xs text-link hover:underline">
          ← Proposals
        </Link>
        <h1 className="mt-1 flex flex-wrap items-baseline gap-3 text-2xl font-semibold tracking-tight">
          {p.kind === "removal" ? "Harness diet: removal" : (p.patternTitle ?? "Harness edit")}
          <StatusBadge p={p} />
        </h1>
        <p className="mt-1 text-xs text-muted-foreground">
          <span className="font-mono">{p.id}</span> · workspace {p.workspace} · {p.repo} at <span className="font-mono">{p.baseCommit.slice(0, 12)}</span> · drafted{" "}
          {day(p.createdAt)}
          {p.pattern && (
            <>
              {" "}
              ·{" "}
              <Link to="/patterns/$id" params={{ id: p.pattern }} className="text-link hover:underline">
                its pattern
              </Link>
            </>
          )}
        </p>
        {p.reason && <p className="mt-2 border-l-2 border-signal py-1 pl-3 text-sm">{p.reason}</p>}
      </div>

      {isMember(me.data) && p.status !== "merged" && p.status !== "rejected" && <Reject id={p.id} />}

      <Section title="Candidates" note="Scored on the dev batch against the cached baseline; a smoke-sized score, not a verdict">
        <ol className="space-y-4">
          {candidates.map((c) => (
            <CandidateRow key={c.index} c={c} gated={p.gateIndex === c.index} />
          ))}
        </ol>
      </Section>

      <Section title="Gate" note="Held-out cases, both sides run; the proposer sees only this outcome">
        {p.checks && p.checks.length > 0 ? (
          <table className="w-full text-left text-sm">
            <thead className="border-b text-2xs uppercase tracking-label text-muted-foreground">
              <tr>
                <th className="py-1.5 pr-3 font-medium">Check</th>
                <th className="py-1.5 pr-3 font-medium">Passed</th>
                <th className="py-1.5 pr-3 font-medium">Evidence</th>
                <th className="py-1.5 font-medium">Detail</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {p.checks.map((c) => (
                <tr key={c.name}>
                  <td className="py-1.5 pr-3">{c.name}</td>
                  <td className="py-1.5 pr-3">{c.passed ? "yes" : "no"}</td>
                  <td className="py-1.5 pr-3 text-xs">{c.evidence}</td>
                  <td className="py-1.5 text-xs">{c.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p className="text-sm text-muted-foreground">{p.status === "gating" ? "The gate is running." : "No gate ran."}</p>
        )}
        {p.gateEvaluation && (
          <p className="mt-2 text-xs">
            <Link to="/evaluations/$id" params={{ id: p.gateEvaluation }} className="text-link hover:underline">
              The gate's evaluation
            </Link>{" "}
            <span className="text-muted-foreground">(held-out: verdict, counts and cost only)</span>
          </p>
        )}
      </Section>

      <Section title="Pull request and outcome">
        {p.prUrl ? (
          <p className="text-sm">
            <a href={p.prUrl} className="text-link hover:underline">
              Pull request #{p.prNumber}
            </a>
            {p.mergedAt && <> · merged {day(p.mergedAt)}</>}
          </p>
        ) : (
          <p className="text-sm text-muted-foreground">No pull request: one opens only after the gate passes.</p>
        )}
        {p.outcome ? (
          <p className="mt-2 text-sm">
            Corrections of this pattern per 100 sessions: {p.outcome.before.toFixed(2)} in the 30 days before the merge ({p.outcome.beforeN} sessions),{" "}
            {p.outcome.after.toFixed(2)} in the 30 days after ({p.outcome.afterN} sessions). {p.outcome.fell ? "The rate fell." : "The rate did not fall; the pattern stays open."}{" "}
            <span className="text-muted-foreground">Observational: other changes in the same weeks affect it too.</span>
          </p>
        ) : (
          p.mergedAt && <p className="mt-2 text-sm text-muted-foreground">The outcome is measured 30 days after the merge.</p>
        )}
      </Section>
    </div>
  );
}

function CandidateRow({ c, gated }: { c: Candidate; gated: boolean }) {
  return (
    <li className="border-l-2 border-border pl-3">
      <div className="flex flex-wrap items-baseline gap-2 text-sm">
        <span className="font-medium">Candidate {c.index}</span>
        {c.mergedFrom && <Tag>merged from {c.mergedFrom.join(" and ")}</Tag>}
        {gated && <Tag tone="ok">sent to the gate</Tag>}
        {c.score ? (
          <span className="font-mono text-xs tabular-nums">
            Δ {pts(c.score.delta)}, interval {pts(c.score.lower)} to {pts(c.score.upper)}, {c.score.runs} runs, wins on {c.score.wins.length}
            {c.score.costRatio != null && <>, cost ×{c.score.costRatio.toFixed(2)}</>}
          </span>
        ) : (
          <span className="text-xs text-muted-foreground">not scored yet</span>
        )}
      </div>
      <p className="text-sm text-muted-foreground">
        {c.rationale} <span className="text-2xs">(model-generated)</span>
      </p>
      <ul className="mt-1 space-y-1 text-xs">
        {c.edits.map((e, i) => (
          <li key={i}>
            <span className="font-medium">{opName(e.op)}</span> in <span className="font-mono">{e.file}</span>
            {e.heading && <> under “{e.heading}”</>}
            {e.old && <div className="font-mono text-muted-foreground line-through">{e.old}</div>}
            {e.new && <div className="whitespace-pre-wrap font-mono">{e.new}</div>}
          </li>
        ))}
      </ul>
    </li>
  );
}

function Reject({ id }: { id: string }) {
  const reject = useReject(id);
  const [reason, setReason] = useState("");
  return (
    <div className="flex flex-wrap items-end gap-3 border border-input bg-card px-4 py-3 text-sm">
      <Field label="Reject, with a reason" className="min-w-64 flex-1">
        <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="The proposer reads it and does not propose this change again for 90 days" />
      </Field>
      <Button onClick={() => reject.mutate(reason)} disabled={reject.isPending || reason.trim() === ""}>
        Reject
      </Button>
      {reject.isError && <ErrorNote error={reject.error} />}
    </div>
  );
}
