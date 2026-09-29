import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { ApiError } from "@/lib/api";
import { day, num, plural } from "@/lib/format";
import { preventionName, wentWrongName } from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { useWorkspaces } from "@/lib/workspaces";
import { Button, ErrorNote, Field, Hidden, Input, Loading, Section, Select, Tag } from "@/ui/kit";
import { statusName, useAcknowledge, useDismiss, usePattern, usePatterns, type Pattern } from "./api";

export type PatternsSearch = { workspace?: string };

// The pattern board: clusters of corrections that share a cause, each with at least k people behind it.
export function PatternList({ search, onSearch }: { search: PatternsSearch; onSearch: (s: PatternsSearch) => void }) {
  const patterns = usePatterns(search.workspace);
  const workspaces = useWorkspaces();
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Patterns</h1>
        <p className="max-w-3xl text-sm text-muted-foreground">
          Corrections that share what went wrong, what would have prevented it and the area of the code, split by cause by the analysis model. A pattern needs 3
          corrections from at least k people in 60 days. Titles and summaries are model-generated; the counts and quotes are observed.
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
      {patterns.isPending ? (
        <Loading what="patterns" />
      ) : patterns.isError ? (
        <ErrorNote error={patterns.error} />
      ) : patterns.data.patterns.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No pattern yet. Patterns appear once 3 classified corrections from {patterns.data.k} people share a cause.
          {patterns.data.hidden > 0 && <> {plural(patterns.data.hidden, "pattern")} has fewer than k people behind it now and is hidden.</>}
        </p>
      ) : (
        <>
          <ul className="divide-y border-y">
            {patterns.data.patterns.map((p) => (
              <PatternRow key={p.id} p={p} />
            ))}
          </ul>
          {patterns.data.hidden > 0 && <Hidden k={patterns.data.k} what={plural(patterns.data.hidden, "pattern")} />}
        </>
      )}
    </div>
  );
}

function PatternRow({ p }: { p: Pattern }) {
  return (
    <li className="grid gap-2 py-3 sm:grid-cols-[minmax(0,1fr)_14rem]">
      <div className="min-w-0">
        <Link to="/patterns/$id" params={{ id: p.id }} className="font-medium text-link hover:underline">
          {p.title}
        </Link>
        <p className="text-sm text-muted-foreground">{p.summary}</p>
        <p className="mt-1 flex flex-wrap gap-1.5 text-2xs">
          <Tag>{wentWrongName(p.wentWrong, p.label)}</Tag>
          <Tag>{preventionName(p.prevention)}</Tag>
          <Tag>{p.path === "*" ? "no file named" : p.path === "." ? "repository root" : `${p.path}/`}</Tag>
          {p.advisory && <Tag tone="signal">advisory</Tag>}
        </p>
      </div>
      <div className="text-xs sm:text-right">
        <div className="font-mono tabular-nums">
          {num(p.corrections)} corrections · {num(p.people)} people
        </div>
        <div className="text-muted-foreground">
          {statusName(p.status)} · {p.workspace}
        </div>
        {p.latestProposal && (
          <div>
            <Link to="/proposals/$id" params={{ id: p.latestProposal.id }} className="text-link hover:underline">
              Proposal: {statusName(p.latestProposal.status)}
            </Link>
          </div>
        )}
      </div>
    </li>
  );
}

export function PatternPage({ id }: { id: string }) {
  const pattern = usePattern(id);
  const me = useMe();
  if (pattern.isPending) return <Loading what="the pattern" />;
  if (pattern.isError)
    return pattern.error instanceof ApiError && pattern.error.status === 403 ? (
      <p className="text-sm">Fewer than k people are behind this pattern now, so it is not shown.</p>
    ) : pattern.error instanceof ApiError && pattern.error.status === 404 ? (
      <p className="text-sm">This pattern does not exist.</p>
    ) : (
      <ErrorNote error={pattern.error} />
    );
  const { pattern: p, quotes, cases, proposals } = pattern.data;
  return (
    <div className="space-y-8">
      <div>
        <Link to="/patterns" className="text-xs text-link hover:underline">
          ← Patterns
        </Link>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight">{p.title}</h1>
        <p className="text-sm text-muted-foreground">
          {p.summary} <span className="text-2xs">(model-generated)</span>
        </p>
        <p className="mt-1 text-xs text-muted-foreground">
          {wentWrongName(p.wentWrong, p.label)} · {preventionName(p.prevention)} · {p.path === "*" ? "no file named" : p.path} · workspace {p.workspace} · detected{" "}
          {day(p.detectedAt)} · {statusName(p.status)}
          {p.reason && <> ({p.reason})</>}
        </p>
      </div>
      {p.advisory && (
        <p role="note" className="border-l-2 border-signal py-1 pl-3 text-sm">
          Advisory: this would be prevented by {preventionName(p.prevention).toLowerCase()}. Casebox does not change tickets, models or permissions by pull request, so the
          proposer does not take it.
        </p>
      )}
      {isMember(me.data) && (p.status === "open" || p.status === "acknowledged") && <Actions p={p} />}

      <Section title="Evidence" note={`${num(p.corrections)} corrections from ${num(p.people)} people (observational)`}>
        <dl className="mb-4 grid max-w-md grid-cols-3 gap-2 text-sm">
          <Phase term="In session" n={p.phases.inSession} />
          <Phase term="Before merge" n={p.phases.beforeMerge} />
          <Phase term="After merge" n={p.phases.afterMerge} />
        </dl>
        {quotes.length === 0 ? (
          <p className="text-xs text-muted-foreground">No quotes: prompt mode may be off, or the signals hold no text.</p>
        ) : (
          <ul className="space-y-2">
            {quotes.map((q) => (
              <li key={q.ref} className="border-l-2 border-border pl-3 text-sm">
                “{q.text}” <span className="text-2xs text-muted-foreground">{q.day}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section title="Cases" note="Steering cases mined from these corrections">
        {cases.length === 0 ? (
          <p className="text-sm text-muted-foreground">None yet. Mining turns corrections whose fix can be checked into steering cases.</p>
        ) : (
          <ul className="flex flex-wrap gap-2 text-xs">
            {cases.map((c) => (
              <li key={c.id}>
                <Link to="/cases/$id" params={{ id: c.id }} className="font-mono text-link hover:underline">
                  {c.id}
                </Link>{" "}
                <span className="text-muted-foreground">
                  {c.status}
                  {c.split ? `, ${c.split === "held_out" ? "held-out" : c.split}` : ""}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <Section title="Proposals">
        {proposals.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            None yet. The weekly proposer run (casebox propose --scheduled) or casebox propose --pattern {p.id} drafts and tests edits.
          </p>
        ) : (
          <ul className="space-y-1 text-sm">
            {proposals.map((x) => (
              <li key={x.id}>
                <Link to="/proposals/$id" params={{ id: x.id }} className="font-mono text-xs text-link hover:underline">
                  {x.id}
                </Link>{" "}
                {statusName(x.status)}
                {x.prUrl && (
                  <>
                    {" "}
                    ·{" "}
                    <a href={x.prUrl} className="text-link hover:underline">
                      pull request
                    </a>
                  </>
                )}
              </li>
            ))}
          </ul>
        )}
      </Section>
    </div>
  );
}

function Phase({ term, n }: { term: string; n: number }) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-label text-muted-foreground">{term}</dt>
      <dd className="font-mono tabular-nums">{num(n)}</dd>
    </div>
  );
}

function Actions({ p }: { p: Pattern }) {
  const acknowledge = useAcknowledge(p.id);
  const dismiss = useDismiss(p.id);
  const [reason, setReason] = useState("");
  return (
    <div className="flex flex-wrap items-end gap-3 border border-input bg-card px-4 py-3 text-sm">
      {p.status === "open" && (
        <Button onClick={() => acknowledge.mutate()} disabled={acknowledge.isPending}>
          Acknowledge
        </Button>
      )}
      <Field label="Dismiss, with a reason" className="min-w-64 flex-1">
        <Input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why this is not worth fixing" />
      </Field>
      <Button onClick={() => dismiss.mutate(reason)} disabled={dismiss.isPending || reason.trim() === ""}>
        Dismiss
      </Button>
      {acknowledge.isError && <ErrorNote error={acknowledge.error} />}
      {dismiss.isError && <ErrorNote error={dismiss.error} />}
    </div>
  );
}
