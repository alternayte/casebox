import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent, type ReactNode } from "react";
import { ApiError } from "@/lib/api";
import { day, num } from "@/lib/format";
import {
  assertionKindName,
  assertionKinds,
  caseKindName,
  caseScopeName,
  caseSplitName,
  repoRoleName,
  retireReasonName,
  validationFailureName,
} from "@/lib/labels";
import { isAdmin, isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Field, Input, Loading, Section, Select, Tag } from "@/ui/kit";
import {
  caseKey,
  shortSha,
  sourceOf,
  useApprove,
  useApproveAssertions,
  useCase,
  useOracle,
  useValidations,
  useEditInstruction,
  useReject,
  useRetire,
  type Assertion,
  type CaseView,
} from "./api";
import { Glossary, StatusTag, WorkItemLink } from "./parts";

// One case: where it came from, its instruction, its oracle, and the review decisions.
export function CasePage({ id }: { id: string }) {
  const query = useCase(id);
  const me = useMe();

  if (query.isPending) return <Loading what="the case" />;
  if (query.isError)
    return query.error instanceof ApiError && query.error.status === 404 ? (
      <p className="text-sm">This case does not exist.</p>
    ) : (
      <ErrorNote error={query.error} />
    );

  const c = query.data;
  const s = c.summary;
  const member = isMember(me.data);
  const blocker = c.approvalBlocker;
  // The server takes an instruction only for a validated case that is not retired.
  const canEditInstruction = member && (s.status === "validated" || s.status === "approved");
  const canEditAssertions = member && s.kind === "steering" && s.status !== "retired";

  return (
    <div className="space-y-8">
      <div>
        <Link to="/cases" className="text-xs text-link hover:underline">
          ← Cases
        </Link>
        <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-2xl font-semibold tracking-tight">
          <span>{caseKindName(s.kind)} case</span>
          <span className="font-mono text-base font-normal text-muted-foreground">{s.id}</span>
          <span className="flex items-center gap-1.5 self-center text-base">
            <StatusTag status={s.status} />
            {s.split && <Tag tone={s.split === "held_out" ? "signal" : "plain"}>{caseSplitName(s.split)}</Tag>}
            {s.drift && <Tag tone="signal">drift</Tag>}
          </span>
        </h1>
        <Facts c={c} />
      </div>

      <StatusNote c={c} blocker={blocker} member={member} />

      <div className="grid gap-10 lg:grid-cols-[minmax(0,1fr)_22rem]">
        <div className="space-y-10">
          <Section title="Instruction" note="What the agent reads. Names show as [person].">
            <Instruction c={c} editable={canEditInstruction} />
          </Section>

          <Section title="Interfaces" note="Appended to the instruction under “Interfaces your solution must provide”.">
            {c.signatures.length === 0 ? (
              <p className="text-sm text-muted-foreground">No new declarations that the held-out tests use.</p>
            ) : (
              <pre className="overflow-x-auto border bg-card p-3 font-mono text-xs leading-5">{c.signatures.join("\n")}</pre>
            )}
          </Section>

          <Section title="Tests" note="The oracle: the tests that decide the case.">
            <Tests c={c} />
          </Section>

          {s.kind === "steering" && (
            <Section title="Assertions" note={c.assertionsApproved ? <Tag tone="ok">Approved</Tag> : <Tag tone="signal">Not approved</Tag>}>
              <AssertionsEditor key={JSON.stringify([c.assertions, c.judge])} c={c} editable={canEditAssertions} />
            </Section>
          )}
        </div>

        <div className="space-y-10">
          <Section title="Decision">
            <Decision c={c} blocker={blocker} member={member} admin={isAdmin(me.data)} />
          </Section>
          <Section title="Oracle">
            <Oracle c={c} />
          </Section>
          <Section title="Repositories">
            <Repos c={c} />
          </Section>
        </div>
      </div>

      <Section title="Validation runs" note="Newest first. Each run builds the base, then runs the tests 3 times with the change.">
        <Runs id={s.id} />
      </Section>
    </div>
  );
}

function Tests({ c }: { c: CaseView }) {
  const validated = c.oracle !== null;
  const oracle = useOracle(c.summary.id, validated);
  if (!validated) return <p className="text-sm text-muted-foreground">The case has no oracle yet. A passed validation records one.</p>;
  if (oracle.isPending) return <Loading what="the tests" />;
  if (oracle.isError)
    return oracle.error instanceof ApiError && oracle.error.status === 404 ? (
      <p className="text-sm text-muted-foreground">The oracle cannot be read.</p>
    ) : (
      <ErrorNote error={oracle.error} />
    );
  const o = oracle.data;
  const failToPass = o.tests?.failToPass ?? [];
  const passToPass = o.tests?.passToPass ?? [];
  const files = o.testFiles ?? [];
  const commands = o.commands ?? [];
  return (
    <div className="space-y-2">
      <TestList title="Fail-to-pass" items={failToPass} open={failToPass.length <= 20} />
      <TestList title="Pass-to-pass" items={passToPass} open={false} />
      <TestList title="Held-out test files" items={files} open={false} />
      <TestList title="Test commands" items={commands.map((x) => x.command)} open={false} />
    </div>
  );
}

function TestList({ title, items, open }: { title: string; items: string[]; open: boolean }) {
  return (
    <details open={open && items.length > 0} className="border-b pb-2">
      <summary className="cursor-pointer text-sm">
        <span className="font-medium">{title}</span> <span className="font-mono text-xs tabular-nums text-muted-foreground">{num(items.length)}</span>
      </summary>
      {items.length === 0 ? (
        <p className="mt-1 text-xs text-muted-foreground">None.</p>
      ) : (
        <ul className="mt-1 max-h-80 overflow-y-auto border bg-card p-2 font-mono text-xs leading-5">
          {items.map((t, i) => (
            <li key={i} className="break-all">
              {t}
            </li>
          ))}
        </ul>
      )}
    </details>
  );
}

// Validation is not captured work, so a run shows its minute (UTC) to tell runs of one day apart.
const minute = (iso: string) => `${iso.slice(0, 10)} ${iso.slice(11, 16)}`;

function Runs({ id }: { id: string }) {
  const runs = useValidations(id);
  if (runs.isPending) return <Loading what="validation runs" />;
  if (runs.isError) return <ErrorNote error={runs.error} />;
  if (runs.data.length === 0) return <p className="text-sm text-muted-foreground">No validation has run yet.</p>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[44rem] text-sm">
        <thead>
          <tr className="border-b-2 border-foreground text-left text-2xs uppercase tracking-label text-muted-foreground">
            <th className="py-1.5 pr-4 font-medium">At (UTC)</th>
            <th className="py-1.5 pr-4 font-medium">Result</th>
            <th className="py-1.5 pr-4 text-right font-medium">F→P</th>
            <th className="py-1.5 pr-4 text-right font-medium">P→P</th>
            <th className="py-1.5 text-right font-medium">Slowest run</th>
          </tr>
        </thead>
        <tbody>
          {runs.data.map((r, i) => (
            <tr key={i} className="border-b border-dashed align-baseline">
              <td className="whitespace-nowrap py-1.5 pr-4 font-mono text-xs text-muted-foreground">{minute(r.at)}</td>
              <td className="py-1.5 pr-4">
                {r.passed ? (
                  <Tag tone="ok">Passed</Tag>
                ) : (
                  <div className="space-y-1">
                    <span className="text-signal">{validationFailureName(r.reason)}</span>
                    {r.detail && (
                      <details>
                        <summary className="cursor-pointer text-xs text-muted-foreground">Detail</summary>
                        <pre className="mt-1 max-h-60 overflow-auto whitespace-pre-wrap font-mono text-xs text-muted-foreground">{r.detail}</pre>
                      </details>
                    )}
                  </div>
                )}
              </td>
              <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{r.failToPass == null ? "—" : num(r.failToPass)}</td>
              <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{r.passToPass == null ? "—" : num(r.passToPass)}</td>
              <td className="py-1.5 text-right font-mono tabular-nums">{r.seconds == null ? "—" : `${num(Math.round(r.seconds))} s`}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Facts({ c }: { c: CaseView }) {
  const s = c.summary;
  const source = sourceOf(s.source);
  const facts: [string, ReactNode][] = [
    ["Workspace", s.workspace],
    [source.what, <span title={source.ref}>{source.ref}</span>],
    ["Work item", <WorkItemLink id={s.workItem} />],
    ["Scope", caseScopeName(s.scope)],
    ["Mined", day(s.minedAt)],
    ["Updated", day(s.updatedAt)],
    ["Recipe", <span title={c.recipeHash}>{c.recipeHash.slice(0, 12)}</span>],
    ["Harness version", <span title={c.harnessHash}>{c.harnessHash.slice(0, 12)}</span>],
  ];
  return (
    <dl className="mt-3 flex flex-wrap gap-x-8 gap-y-2 border-y py-2 text-sm">
      {facts.map(([k, v]) => (
        <div key={k} className="min-w-0 max-w-full">
          <dt className="text-2xs uppercase tracking-label text-muted-foreground">{k}</dt>
          <dd className="truncate font-mono text-xs tabular-nums">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

// Says plainly where the case stands and, when it cannot be approved yet, why.
function StatusNote({ c, blocker, member }: { c: CaseView; blocker: string | null; member: boolean }) {
  const s = c.summary;
  let body: ReactNode;
  let tone = "border-link";
  switch (s.status) {
    case "mined":
      body = "The case is waiting for a worker to validate it at its base commit.";
      break;
    case "validation_failed":
      tone = "border-signal";
      body = (
        <>
          <p className="font-medium">Validation failed: {validationFailureName(c.failureReason)}.</p>
          {c.failureDetail && <pre className="mt-1 max-h-60 overflow-auto whitespace-pre-wrap font-mono text-xs text-muted-foreground">{c.failureDetail}</pre>}
        </>
      );
      break;
    case "rejected":
      tone = "border-signal";
      body = <>Rejected{c.rejectReason ? <>: <span className="font-serif italic">“{c.rejectReason}”</span></> : "."}</>;
      break;
    case "retired":
      body = `Retired: ${retireReasonName(c.retiredReason)}. A retired case is final.`;
      break;
    case "approved":
      tone = "border-ok";
      body = `Approved. ${s.split ? `The case is in the ${caseSplitName(s.split).toLowerCase()}. ` : ""}It counts in evaluations with weight ${num(s.weight)}.`;
      break;
    default:
      if (blocker) {
        tone = "border-signal";
        body = <>Cannot be approved yet. {blocker}</>;
      } else {
        body = member ? "Ready to approve. Read the instruction first: it must state the task without hints about the fix." : "Ready for a Member to approve.";
      }
  }
  return <div className={cn("max-w-3xl border-l-2 pl-3 text-sm", tone)}>{body}</div>;
}

function Instruction({ c, editable }: { c: CaseView; editable: boolean }) {
  const [draft, setDraft] = useState(c.instruction ?? "");
  const [saved, setSaved] = useState(false);
  const edit = useEditInstruction(c.summary.id);
  const client = useQueryClient();
  const s = c.summary;
  const titleOnly = s.kind === "capability" && s.workItem === null;
  const changed = draft.trim() !== (c.instruction ?? "").trim();

  function submit(e: FormEvent) {
    e.preventDefault();
    setSaved(false);
    // The server tokenizes and trims the text, so the draft restarts from what it stored.
    edit.mutate(draft, {
      onSuccess: () => {
        setDraft(client.getQueryData<CaseView>(caseKey(c.summary.id))?.instruction ?? draft.trim());
        setSaved(true);
      },
    });
  }

  const notes = (
    <>
      {titleOnly && <p className="text-xs text-signal">This case has no work item, so its instruction comes from the pull request title only. Add what the task needs.</p>}
      {c.instruction === null && s.hasInstruction === false && (s.status === "validated" || s.status === "approved") && (
        <p className="text-xs text-muted-foreground">No readable instruction. The worker has not drafted one, or its person was erased.</p>
      )}
    </>
  );

  if (!editable) {
    return (
      <div className="space-y-2">
        {notes}
        {c.instruction ? (
          <div className="whitespace-pre-wrap border bg-card p-3 text-sm">{c.instruction}</div>
        ) : (
          <p className="text-sm text-muted-foreground">
            {s.status === "mined" || s.status === "validation_failed" ? "A case gets an instruction after it passes validation." : "No instruction."}
          </p>
        )}
      </div>
    );
  }

  return (
    <form onSubmit={submit} className="space-y-2">
      {notes}
      <textarea
        value={draft}
        onChange={(e) => {
          setDraft(e.target.value);
          setSaved(false);
        }}
        rows={Math.min(24, Math.max(8, draft.split("\n").length + 2))}
        aria-label="Instruction"
        className="w-full rounded-sm border border-input bg-card p-3 text-sm leading-6"
      />
      <div className="flex items-center gap-3 text-xs">
        <Button type="submit" variant="solid" disabled={!changed || draft.trim() === "" || edit.isPending}>
          {edit.isPending ? "Saving…" : "Save instruction"}
        </Button>
        {changed && (
          <Button type="button" onClick={() => setDraft(c.instruction ?? "")}>
            Undo changes
          </Button>
        )}
        {saved && !changed && <span className="text-ok">Saved</span>}
        <span className="text-muted-foreground">Remove file names, function names and approaches the task did not state.</span>
      </div>
      {edit.isError && <ErrorNote error={edit.error} />}
    </form>
  );
}

type Row = { kind: string; value: string };

const usesPath = (kind: string) => kind === "forbidden_file";

function toRow(a: Assertion): Row {
  return { kind: a.kind, value: (usesPath(a.kind) ? a.path : a.pattern) ?? a.path ?? a.pattern ?? "" };
}

function AssertionsEditor({ c, editable }: { c: CaseView; editable: boolean }) {
  const [rows, setRows] = useState<Row[]>(() => c.assertions.map(toRow));
  const [judge, setJudge] = useState(c.judge[0]?.question ?? "");
  const approve = useApproveAssertions(c.summary.id);
  const original = JSON.stringify([c.assertions.map(toRow), c.judge[0]?.question ?? ""]);
  const changed = JSON.stringify([rows, judge]) !== original;
  const valid = rows.length > 0 && rows.every((r) => r.value.trim() !== "");

  const intro = (
    <p className="text-xs text-muted-foreground">
      The model proposes these from the correction. A person approves them. Each one decides the case on the trace or the diff; the judge question is
      medium evidence and never overrides a failing test.
    </p>
  );

  if (!editable) {
    return (
      <div className="space-y-3">
        {intro}
        {c.assertions.length === 0 ? (
          <p className="text-sm text-muted-foreground">No assertions yet.</p>
        ) : (
          <ul className="divide-y border-y text-sm">
            {c.assertions.map((a, i) => (
              <li key={i} className="grid grid-cols-[16rem_minmax(0,1fr)] gap-3 py-1.5">
                <span>{assertionKindName(a.kind)}</span>
                <span className="break-all font-mono text-xs">{toRow(a).value}</span>
              </li>
            ))}
          </ul>
        )}
        <div className="text-sm">
          <span className="text-2xs uppercase tracking-label text-muted-foreground">Judge question</span>
          <p>{c.judge[0]?.question ?? <span className="text-muted-foreground">None</span>}</p>
        </div>
      </div>
    );
  }

  function update(i: number, next: Partial<Row>) {
    setRows(rows.map((r, j) => (j === i ? { ...r, ...next } : r)));
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    approve.mutate({
      assertions: rows.map((r) => (usesPath(r.kind) ? { kind: r.kind, path: r.value.trim() } : { kind: r.kind, pattern: r.value.trim() })),
      judge: judge.trim() ? [{ question: judge.trim() }] : [],
    });
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      {intro}
      {rows.length === 0 && <p className="text-sm text-signal">A steering case needs at least one assertion. Add one.</p>}
      <ul className="space-y-2">
        {rows.map((r, i) => (
          <li key={i} className="grid grid-cols-[16rem_minmax(0,1fr)_auto] items-end gap-2">
            <Field label="Kind">
              <Select value={r.kind} onChange={(e) => update(i, { kind: e.target.value })}>
                {assertionKinds.map((k) => (
                  <option key={k} value={k}>
                    {assertionKindName(k)}
                  </option>
                ))}
                {!(assertionKinds as readonly string[]).includes(r.kind) && <option value={r.kind}>{r.kind}</option>}
              </Select>
            </Field>
            <Field label={usesPath(r.kind) ? "Path or glob" : "Pattern"}>
              <Input
                value={r.value}
                onChange={(e) => update(i, { value: e.target.value })}
                placeholder={usesPath(r.kind) ? "migrations/**" : r.kind === "command_before_done" ? "go test" : "gomock"}
                className="font-mono"
                required
              />
            </Field>
            <Button type="button" variant="quiet" onClick={() => setRows(rows.filter((_, j) => j !== i))}>
              Remove
            </Button>
          </li>
        ))}
      </ul>
      <Button type="button" onClick={() => setRows([...rows, { kind: "forbidden_file", value: "" }])}>
        Add assertion
      </Button>
      <Field label="Judge question (optional, one yes or no question)">
        <Input value={judge} onChange={(e) => setJudge(e.target.value)} placeholder="Did the agent run the tests before it said the work was done?" />
      </Field>
      <div className="flex flex-wrap items-center gap-3 text-xs">
        <Button type="submit" variant="solid" disabled={!valid || approve.isPending || (c.assertionsApproved && !changed)}>
          {approve.isPending ? "Approving…" : "Approve assertions"}
        </Button>
        {c.assertionsApproved && changed && <span className="text-signal">You changed approved assertions. Approve them again to keep the changes.</span>}
        {c.assertionsApproved && !changed && <span className="text-ok">Approved as shown.</span>}
      </div>
      {approve.isError && <ErrorNote error={approve.error} />}
    </form>
  );
}

function Decision({ c, blocker, member, admin }: { c: CaseView; blocker: string | null; member: boolean; admin: boolean }) {
  const s = c.summary;
  const id = s.id;
  const approve = useApprove(id);
  const reject = useReject(id);
  const retire = useRetire(id);
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const [confirmRetire, setConfirmRetire] = useState(false);

  if (!member) return <p className="text-sm text-muted-foreground">A Member approves or rejects a case. An Admin retires one.</p>;
  if (s.status === "retired") return <p className="text-sm text-muted-foreground">Retired. Nothing more happens to this case.</p>;

  const canApprove = s.status !== "approved" && s.status !== "rejected";
  const canReject = s.status !== "rejected";

  function submitReject(e: FormEvent) {
    e.preventDefault();
    reject.mutate(reason.trim(), {
      onSuccess: () => {
        setRejecting(false);
        setReason("");
      },
    });
  }

  return (
    <div className="space-y-4 text-sm">
      {canApprove && (
        <div className="space-y-1.5">
          <Button variant="solid" onClick={() => approve.mutate()} disabled={blocker !== null || approve.isPending}>
            {approve.isPending ? "Approving…" : "Approve case"}
          </Button>
          {blocker ? (
            <p className="text-xs text-signal">{blocker}</p>
          ) : (
            <p className="text-xs text-muted-foreground">Approval assigns the split: every fifth approved case of a workspace is held out.</p>
          )}
          {approve.isError && <ErrorNote error={approve.error} />}
        </div>
      )}

      {canReject &&
        (rejecting ? (
          <form onSubmit={submitReject} className="space-y-2 border border-input bg-card p-2">
            <Field label="Why reject this case">
              <textarea
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                required
                rows={3}
                autoFocus
                className="w-full rounded-sm border border-input bg-card p-2 text-xs"
              />
            </Field>
            <div className="flex gap-2">
              <Button type="submit" variant="solid" disabled={reason.trim() === "" || reject.isPending}>
                {reject.isPending ? "Rejecting…" : "Reject case"}
              </Button>
              <Button type="button" onClick={() => setRejecting(false)}>
                Cancel
              </Button>
            </div>
            {reject.isError && <ErrorNote error={reject.error} />}
          </form>
        ) : (
          <div>
            <Button onClick={() => setRejecting(true)}>Reject…</Button>
          </div>
        ))}

      {admin && (
        <div className="space-y-1.5 border-t pt-3">
          {confirmRetire ? (
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs">Retire this case for good?</span>
              <Button variant="solid" onClick={() => retire.mutate(undefined, { onSuccess: () => setConfirmRetire(false) })} disabled={retire.isPending}>
                {retire.isPending ? "Retiring…" : "Retire"}
              </Button>
              <Button onClick={() => setConfirmRetire(false)}>Cancel</Button>
            </div>
          ) : (
            <Button onClick={() => setConfirmRetire(true)}>Retire…</Button>
          )}
          <p className="text-xs text-muted-foreground">Retirement is final. The case leaves every suite.</p>
          {retire.isError && <ErrorNote error={retire.error} />}
        </div>
      )}
    </div>
  );
}

function Oracle({ c }: { c: CaseView }) {
  const s = c.summary;
  const rows: [string, ReactNode][] = [
    ["Fail-to-pass tests", s.failToPass == null ? "—" : num(s.failToPass)],
    ["Pass-to-pass tests", s.passToPass == null ? "—" : num(s.passToPass)],
    ["Slowest of 3 runs", c.seconds == null ? "—" : `${num(Math.round(c.seconds))} s`],
    ["Weight", s.failToPass == null ? "—" : num(s.weight)],
    ["Harness drift", s.failToPass == null ? "—" : s.drift ? <span className="text-signal">Yes</span> : "No"],
    ["Oracle blob", c.oracle ? <span title={c.oracle}>{c.oracle.slice(0, 12)}</span> : "—"],
  ];
  return (
    <div className="space-y-3">
      <dl className="divide-y border-y text-sm">
        {rows.map(([k, v]) => (
          <div key={k} className="flex items-baseline justify-between gap-3 py-1">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="font-mono text-xs tabular-nums">{v}</dd>
          </div>
        ))}
      </dl>
      <Glossary className="sm:grid-cols-1" />
    </div>
  );
}

function Repos({ c }: { c: CaseView }) {
  if (c.repos.length === 0) return <p className="text-sm text-muted-foreground">No repositories.</p>;
  return (
    <ul className="divide-y border-y text-sm">
      {c.repos.map((r) => (
        <li key={r.repo} className="space-y-0.5 py-2">
          <div className="break-all font-mono text-xs">{r.repo}</div>
          <div className="text-xs text-muted-foreground">{repoRoleName(r.role)}</div>
          <div className="flex gap-4 font-mono text-2xs">
            <span title={r.base}>
              <span className="text-muted-foreground">base </span>
              {shortSha(r.base)}
            </span>
            <span title={r.merged ?? undefined}>
              <span className="text-muted-foreground">merged </span>
              {shortSha(r.merged)}
            </span>
          </div>
        </li>
      ))}
    </ul>
  );
}
