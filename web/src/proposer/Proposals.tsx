import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { ApiError } from "@/lib/api";
import { day } from "@/lib/format";
import { isMember, useMe } from "@/lib/session";
import { useWorkspaces } from "@/lib/workspaces";
import { Button, ErrorNote, Field, Input, Loading, Section, Select, Tag } from "@/ui/kit";
import { kindName, statusName, useApprove, useProposal, useProposals, useReject, type FilePreview, type Proposal } from "./api";

export type ProposalsSearch = { workspace?: string };

// Proposals: one small change per pattern of corrections. A person approves or rejects each one, and
// `casebox apply` writes it into the repository (docs/specs/simple-evolution.md).
export function ProposalList({ search, onSearch }: { search: ProposalsSearch; onSearch: (s: ProposalsSearch) => void }) {
  const proposals = useProposals(search.workspace);
  const workspaces = useWorkspaces();
  const waiting = proposals.data?.proposals.filter((p) => p.status === "open" || p.status === "approved") ?? [];
  const done = proposals.data?.proposals.filter((p) => p.status !== "open" && p.status !== "approved") ?? [];
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Proposals</h1>
        <p className="max-w-3xl text-sm text-muted-foreground">
          Small changes that would have prevented a pattern of corrections: an instruction, a skill, an MCP server, or a code note for your agent. Approve or reject each
          one. At most three wait at once. An approved change lands with <code className="font-mono">casebox apply</code>; 30 days later Casebox shows whether the
          corrections fell.
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
      ) : (
        <>
          <Section title="Waiting for you">
            {waiting.length === 0 ? (
              <p className="text-sm text-muted-foreground">Nothing waits. Casebox drafts a proposal when a pattern of corrections appears.</p>
            ) : (
              <Rows proposals={waiting} />
            )}
          </Section>
          {done.length > 0 && (
            <Section title="Applied and rejected">
              <Rows proposals={done} />
            </Section>
          )}
          {proposals.data.hidden > 0 && (
            <p className="text-xs text-muted-foreground">{proposals.data.hidden} more have fewer than k people behind their pattern now, so they are hidden.</p>
          )}
        </>
      )}
    </div>
  );
}

function Rows({ proposals }: { proposals: Proposal[] }) {
  return (
    <ul className="divide-y border-y">
      {proposals.map((p) => (
        <li key={p.id} className="flex flex-wrap items-baseline justify-between gap-2 py-2.5 text-sm">
          <span className="min-w-0">
            <Link to="/proposals/$id" params={{ id: p.id }} className="text-link hover:underline">
              {p.title}
            </Link>
            <span className="ml-2 text-xs text-muted-foreground">
              {kindName(p.kind)} · {p.patternTitle} · {day(p.createdAt)}
            </span>
          </span>
          <StatusBadge p={p} />
        </li>
      ))}
    </ul>
  );
}

function StatusBadge({ p }: { p: Proposal }) {
  const tone = p.status === "applied" ? "ok" : p.status === "open" ? "signal" : "plain";
  return (
    <Tag tone={tone}>
      {statusName(p.status)}
      {p.appliedMode ? ` (${p.appliedMode})` : ""}
    </Tag>
  );
}

export function ProposalPage({ id }: { id: string }) {
  const proposal = useProposal(id);
  const me = useMe();
  if (proposal.isPending) return <Loading what="the proposal" />;
  if (proposal.isError)
    return proposal.error instanceof ApiError && proposal.error.status === 403 ? (
      <p className="text-sm">Fewer than k people are behind this proposal's pattern now, so it is not shown.</p>
    ) : proposal.error instanceof ApiError && proposal.error.status === 404 ? (
      <p className="text-sm">This proposal does not exist.</p>
    ) : (
      <ErrorNote error={proposal.error} />
    );
  const { proposal: p, rationale, reason, preview, note, evidence, baseCommit } = proposal.data;
  return (
    <div className="space-y-8">
      <div>
        <Link to="/proposals" className="text-xs text-link hover:underline">
          ← Proposals
        </Link>
        <h1 className="mt-1 flex flex-wrap items-baseline gap-3 text-2xl font-semibold tracking-tight">
          {p.title}
          <StatusBadge p={p} />
        </h1>
        <p className="mt-1 text-xs text-muted-foreground">
          {kindName(p.kind)} · workspace {p.workspace} · {p.repo} at <span className="font-mono">{baseCommit.slice(0, 12)}</span> · drafted {day(p.createdAt)} ·{" "}
          <Link to="/patterns/$id" params={{ id: p.pattern }} className="text-link hover:underline">
            {p.patternTitle}
          </Link>
        </p>
        <p className="mt-3 max-w-3xl text-sm">
          {rationale} <span className="text-2xs text-muted-foreground">(model-generated)</span>
        </p>
        {reason && <p className="mt-2 border-l-2 border-signal py-1 pl-3 text-sm">Rejected: {reason}</p>}
      </div>

      {isMember(me.data) && <Decide p={p} />}

      <Section title="The corrections behind it" note={`${evidence.corrections} corrections from ${evidence.people} people (observational)`}>
        {evidence.quotes.length === 0 ? (
          <p className="text-xs text-muted-foreground">No quotes: prompt mode may be off.</p>
        ) : (
          <ul className="space-y-2">
            {evidence.quotes.map((q) => (
              <li key={q.ref} className="border-l-2 border-border pl-3 text-sm">
                “{q.text}” <span className="text-2xs text-muted-foreground">{q.day}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section title="The change">
        {note ? (
          <div className="space-y-3 text-sm">
            <p>
              <span className="font-medium">What:</span> {note.what}
            </p>
            <p>
              <span className="font-medium">Why:</span> {note.why}
            </p>
            <div>
              <p className="text-2xs uppercase tracking-label text-muted-foreground">The prompt casebox apply gives your agent</p>
              <pre className="mt-1 whitespace-pre-wrap border bg-card p-3 font-mono text-xs">{note.prompt}</pre>
            </div>
          </div>
        ) : (
          <div className="space-y-4">
            {preview.map((f) => (
              <Diff key={f.path} file={f} />
            ))}
          </div>
        )}
      </Section>

      <Section title="Outcome">
        {p.outcome ? (
          <p className="text-sm">
            Corrections of this pattern per 100 sessions: {p.outcome.before.toFixed(2)} in the 30 days before it was applied ({p.outcome.beforeN} sessions),{" "}
            {p.outcome.after.toFixed(2)} in the 30 days after ({p.outcome.afterN} sessions). {p.outcome.fell ? "The rate fell." : "The rate did not fall; the pattern stays open."}{" "}
            <span className="text-muted-foreground">Observational: other changes in the same weeks affect it too.</span>
          </p>
        ) : p.appliedAt ? (
          <p className="text-sm text-muted-foreground">
            Applied {p.appliedMode === "private" ? "privately" : "to the shared files"} on {day(p.appliedAt)}. Casebox compares this pattern's corrections 30 days
            before and after that day.
          </p>
        ) : (
          <p className="text-sm text-muted-foreground">Measured 30 days after the change is applied.</p>
        )}
      </Section>
    </div>
  );
}

// The before and after of one file: added lines marked "+", removed ones "−", with two lines of
// context around each change and the unchanged rest folded.
function Diff({ file }: { file: FilePreview }) {
  const before = file.before?.split("\n") ?? [];
  const after = file.after.split("\n");
  const had = new Set(before);
  const has = new Set(after);
  const removed = before.filter((l) => !has.has(l));
  const changed = after.map((l) => !had.has(l));
  const near = (i: number) => changed.slice(Math.max(0, i - 2), i + 3).some(Boolean);
  const rows: { kind: "same" | "add" | "fold"; text: string }[] = [];
  let folded = 0;
  after.forEach((l, i) => {
    if (changed[i] || near(i)) {
      if (folded > 0) rows.push({ kind: "fold", text: `${folded} unchanged lines` });
      folded = 0;
      rows.push({ kind: changed[i] ? "add" : "same", text: l });
    } else folded++;
  });
  if (folded > 0) rows.push({ kind: "fold", text: `${folded} unchanged lines` });
  return (
    <div>
      <p className="font-mono text-xs">
        {file.path} {file.before === null && <span className="text-muted-foreground">(new file)</span>}
      </p>
      <pre className="mt-1 overflow-x-auto border bg-card p-3 font-mono text-xs leading-5">
        {removed.map((l, i) => (
          <div key={`-${i}`} className="text-signal">
            − {l}
          </div>
        ))}
        {rows.map((r, i) =>
          r.kind === "fold" ? (
            <div key={i} className="text-2xs italic text-muted-foreground">
              ⋯ {r.text}
            </div>
          ) : r.kind === "add" ? (
            <div key={i} className="bg-ok/10 font-medium">
              + {r.text}
            </div>
          ) : (
            <div key={i} className="text-muted-foreground">
              {"  "}
              {r.text}
            </div>
          ),
        )}
      </pre>
    </div>
  );
}

function Decide({ p }: { p: Proposal }) {
  const approve = useApprove(p.id);
  const reject = useReject(p.id);
  const [reason, setReason] = useState("");
  if (p.status === "approved")
    return (
      <p className="border border-input bg-card px-4 py-3 text-sm">
        Approved. In the repository, run <code className="font-mono">casebox apply {p.id}</code>
        {p.kind === "code_note" ? " to start your agent with the prompt." : " to write it privately, or add --commit to share it with your team."}
      </p>
    );
  if (p.status !== "open") return null;
  return (
    <div className="flex flex-wrap items-end gap-3 border border-input bg-card px-4 py-3 text-sm">
      <Button variant="solid" onClick={() => approve.mutate()} disabled={approve.isPending}>
        Approve
      </Button>
      <Field label="Or reject, with a reason" className="min-w-64 flex-1">
        <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="The next draft reads it; this change does not come back for 90 days" />
      </Field>
      <Button onClick={() => reject.mutate(reason)} disabled={reject.isPending || reason.trim() === ""}>
        Reject
      </Button>
      {approve.isError && <ErrorNote error={approve.error} />}
      {reject.isError && <ErrorNote error={reject.error} />}
    </div>
  );
}
