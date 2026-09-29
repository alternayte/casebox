import { Link } from "@tanstack/react-router";
import { useState, type FormEvent, type ReactNode } from "react";
import { ApiError } from "@/lib/api";
import { day, num } from "@/lib/format";
import {
  agentName,
  intentName,
  labelSourceName,
  linkSourceName,
  phaseName,
  preventionName,
  signalName,
  snapshotReasonName,
  wentWrongName,
} from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Input, Loading, Section, Tag } from "@/ui/kit";
import { keyOf, useMoveSession, useTimeline, useWorkItems, type TimelineEntry, type WorkItemSummary } from "./api";

// One work item: what is known about it, its linked sessions, and its timeline of evidence.
export function WorkItemPage({ id }: { id: string }) {
  const timeline = useTimeline(id);
  const lookup = useWorkItems(keyOf(id), 50);
  const item = lookup.data?.find((w) => w.id === id);
  const me = useMe();
  const sessions = timeline.data ? linkedSessions(timeline.data) : [];

  return (
    <div className="space-y-8">
      <div>
        <Link to="/work" className="text-xs text-link hover:underline">
          ← Work
        </Link>
        <h1 className="mt-1 flex flex-wrap items-baseline gap-x-3 text-2xl font-semibold tracking-tight">
          <span className="font-mono">{keyOf(id)}</span>
          {item && <span className="font-normal">{item.title}</span>}
        </h1>
        {item && <Facts item={item} />}
      </div>

      {timeline.isPending && <Loading what="the timeline" />}
      {timeline.isError &&
        (timeline.error instanceof ApiError && timeline.error.status === 404 ? (
          <p className="text-sm">This work item does not exist.</p>
        ) : (
          <ErrorNote error={timeline.error} />
        ))}
      {timeline.data && (
        <div className="grid gap-10 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <Section title="Timeline" note="Days only. Steering entries hold no text.">
            <Timeline entries={timeline.data} />
          </Section>
          <Section title="Linked sessions">
            <Sessions workItem={id} sessions={sessions} canMove={isMember(me.data)} />
          </Section>
        </div>
      )}
    </div>
  );
}

function Facts({ item }: { item: WorkItemSummary }) {
  const facts: [string, ReactNode][] = [
    ["Source", item.provider === "jira" ? "Jira" : item.provider === "github" ? "GitHub" : item.provider],
    ["Type", item.type ?? "—"],
    ["Status", item.resolved ? <Tag tone="ok">{item.status ?? "resolved"}</Tag> : (item.status ?? "—")],
    ["Sessions", num(item.sessions)],
    ["Pull requests", num(item.pullRequests)],
    ["Merged", num(item.merged)],
  ];
  if (item.repo) facts.splice(1, 0, ["Repository", item.repo]);
  return (
    <dl className="mt-3 flex flex-wrap gap-x-8 gap-y-2 border-y py-2 text-sm">
      {facts.map(([k, v]) => (
        <div key={k}>
          <dt className="text-2xs uppercase tracking-label text-muted-foreground">{k}</dt>
          <dd className="font-mono text-xs tabular-nums">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

// The sessions linked now: links minus later reassignments, in timeline order.
function linkedSessions(entries: TimelineEntry[]): string[] {
  const linked = new Set<string>();
  for (const e of entries) {
    const sessionId = e.detail.sessionId;
    if (typeof sessionId !== "string") continue;
    if (e.kind === "session_linked") linked.add(sessionId);
    if (e.kind === "session_reassigned") linked.delete(sessionId);
  }
  return [...linked];
}

function sessionLabel(sessionId: string) {
  const at = sessionId.indexOf(":");
  if (at < 0) return <span className="font-mono">{sessionId}</span>;
  return (
    <>
      {agentName(sessionId.slice(0, at))} <span className="font-mono text-muted-foreground">{sessionId.slice(at + 1, at + 9)}</span>
    </>
  );
}

const kindNames: Record<string, string> = {
  snapshot: "Snapshot",
  session_linked: "Session linked",
  session_reassigned: "Session moved away",
  pr_linked: "Pull request linked",
  review: "Review",
  ci: "CI",
  merged: "Merged",
  revert: "Reverted",
  fix: "Fixed after merge",
  steering: "Intervention",
};

function Timeline({ entries }: { entries: TimelineEntry[] }) {
  if (entries.length === 0) return <p className="text-sm text-muted-foreground">Nothing is recorded for this work item yet.</p>;
  const days: [string, TimelineEntry[]][] = [];
  for (const e of entries) {
    const d = day(e.at);
    const last = days[days.length - 1];
    if (last && last[0] === d) last[1].push(e);
    else days.push([d, [e]]);
  }
  return (
    <ol className="space-y-4">
      {days.map(([d, list]) => (
        <li key={d} className="grid grid-cols-[6.5rem_minmax(0,1fr)] gap-3">
          <span className="pt-0.5 font-mono text-xs text-muted-foreground">{d}</span>
          <ul className="space-y-1.5 border-l pl-4">
            {list.map((e, i) => (
              <Entry key={i} e={e} />
            ))}
          </ul>
        </li>
      ))}
    </ol>
  );
}

function str(v: unknown): string | null {
  return typeof v === "string" ? v : typeof v === "number" ? String(v) : null;
}

function pr(d: Record<string, unknown>) {
  const repo = str(d.repo);
  const number = str(d.number);
  return <span className="font-mono text-xs">{repo ? `${repo}#${number}` : `#${number}`}</span>;
}

function Entry({ e }: { e: TimelineEntry }) {
  const d = e.detail;
  const correction = e.kind === "steering" && d.intent === "correction";
  const afterMerge = e.kind === "revert" || e.kind === "fix" || (e.kind === "steering" && d.phase === "after_merge");
  let body: ReactNode;
  switch (e.kind) {
    case "snapshot":
      body = (
        <>
          {snapshotReasonName(str(d.reason))}
          {str(d.status) && <span className="text-muted-foreground"> · status {str(d.status)}</span>}
          {d.resolved === true && <span className="text-muted-foreground"> · resolved</span>}
        </>
      );
      break;
    case "session_linked":
      body = (
        <>
          {sessionLabel(str(d.sessionId) ?? "")}
          <span className="text-muted-foreground">
            {" "}
            · {linkSourceName(str(d.source))}, {str(d.confidence) ?? "unknown"} confidence
          </span>
        </>
      );
      break;
    case "session_reassigned":
      body = (
        <>
          {sessionLabel(str(d.sessionId) ?? "")}
          {str(d.to) && (
            <>
              {" "}
              to{" "}
              <Link to="/work/item" search={{ id: str(d.to)! }} className="font-mono text-xs text-link hover:underline">
                {keyOf(str(d.to)!)}
              </Link>
            </>
          )}
        </>
      );
      break;
    case "pr_linked":
      body = (
        <>
          {pr(d)}
          <span className="text-muted-foreground">
            {" "}
            · {linkSourceName(str(d.source))}, {str(d.confidence) ?? "unknown"} confidence
          </span>
        </>
      );
      break;
    case "review":
      body = (
        <>
          {pr(d)} {reviewState(str(d.state))}
          {typeof d.comments === "number" && d.comments > 0 && <span className="text-muted-foreground"> · {num(d.comments)} comments</span>}
        </>
      );
      break;
    case "ci":
      body = (
        <>
          {pr(d)} {str(d.check)}{" "}
          <span className={cn(str(d.conclusion) === "success" ? "text-ok" : str(d.conclusion) === "failure" ? "text-signal" : "text-muted-foreground")}>
            {str(d.conclusion)}
          </span>
        </>
      );
      break;
    case "merged":
      body = (
        <>
          {pr(d)} {str(d.sha) && <span className="font-mono text-2xs text-muted-foreground">{str(d.sha)!.slice(0, 10)}</span>}
        </>
      );
      break;
    case "revert":
      body = (
        <>
          {pr(d)} <span className="text-muted-foreground">by {revertBy(str(d.by))}</span>
        </>
      );
      break;
    case "fix":
      body = (
        <>
          {pr(d)} <span className="text-muted-foreground">by {revertBy(str(d.by))}</span>
          {typeof d.lines === "number" && d.lines > 0 && <span className="text-muted-foreground"> · {num(d.lines)} lines</span>}
          {str(d.source) && <span className="text-muted-foreground"> · {linkSourceName(str(d.source))}</span>}
        </>
      );
      break;
    case "steering":
      body = (
        <>
          {signalName(str(d.signal))} <span className="text-muted-foreground">· {phaseName(str(d.phase))}</span>
          <span className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs">
            {d.intent ? <span className={cn(correction && "text-signal")}>{intentName(str(d.intent))}</span> : <span className="text-muted-foreground">Not classified</span>}
            {correction && d.wentWrong != null && <span>· {wentWrongName(str(d.wentWrong))}</span>}
            {correction && d.prevention != null && <span className="text-muted-foreground">· {preventionName(str(d.prevention))}</span>}
            {str(d.labelSource) && <Tag>{labelSourceName(str(d.labelSource))}</Tag>}
          </span>
        </>
      );
      break;
    default:
      body = <span className="font-mono text-xs text-muted-foreground">{JSON.stringify(d)}</span>;
  }
  return (
    <li className={cn("relative text-sm", (correction || afterMerge) && "before:absolute before:-left-[1.3rem] before:top-1.5 before:size-2 before:bg-signal")}>
      <span className={cn("mr-2 text-2xs font-medium uppercase tracking-label", correction || afterMerge ? "text-signal" : "text-muted-foreground")}>
        {kindNames[e.kind] ?? e.kind}
      </span>
      {body}
    </li>
  );
}

function reviewState(state: string | null) {
  switch (state) {
    case "APPROVED":
      return <span className="text-ok">approved</span>;
    case "CHANGES_REQUESTED":
      return <span className="text-signal">changes requested</span>;
    case "COMMENTED":
      return <span className="text-muted-foreground">commented</span>;
    case "DISMISSED":
      return <span className="text-muted-foreground">dismissed</span>;
    default:
      return <span className="text-muted-foreground">{state?.toLowerCase().replaceAll("_", " ")}</span>;
  }
}

// A revert or fix names the pull request or commit that made it, never a person.
function revertBy(by: string | null) {
  if (!by) return "an unknown change";
  return <span className="font-mono text-xs">{/^[0-9a-f]{7,40}$/.test(by) ? `commit ${by.slice(0, 10)}` : by}</span>;
}

function Sessions({ workItem, sessions, canMove }: { workItem: string; sessions: string[]; canMove: boolean }) {
  if (sessions.length === 0) return <p className="text-sm text-muted-foreground">No session is linked to this work item.</p>;
  return (
    <ul className="divide-y border-y">
      {sessions.map((s) => (
        <SessionRow key={s} sessionId={s} workItem={workItem} canMove={canMove} />
      ))}
      {!canMove && <li className="py-2 text-xs text-muted-foreground">A Member can move a session to another work item.</li>}
    </ul>
  );
}

function SessionRow({ sessionId, workItem, canMove }: { sessionId: string; workItem: string; canMove: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <li className="space-y-2 py-2 text-sm">
      <div className="flex items-center justify-between gap-2">
        <span className="truncate" title={sessionId}>
          {sessionLabel(sessionId)}
        </span>
        {canMove && !open && (
          <Button variant="quiet" onClick={() => setOpen(true)}>
            Move
          </Button>
        )}
      </div>
      {open && <MoveForm sessionId={sessionId} from={workItem} onClose={() => setOpen(false)} />}
    </li>
  );
}

// Moving a session records a reassignment on this item and an explicit link on the other. Nothing is edited.
function MoveForm({ sessionId, from, onClose }: { sessionId: string; from: string; onClose: () => void }) {
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState<string>();
  const [target, setTarget] = useState<string>();
  const results = useWorkItems(search, 20);
  const move = useMoveSession();

  function find(e: FormEvent) {
    e.preventDefault();
    setSearch(query.trim() || undefined);
    setTarget(undefined);
  }

  return (
    <div className="space-y-2 border border-input bg-card p-2 text-xs">
      <form onSubmit={find} className="flex gap-2">
        <Input type="search" aria-label="Find a work item" placeholder="Key or title" value={query} onChange={(e) => setQuery(e.target.value)} autoFocus />
        <Button type="submit">Find</Button>
      </form>
      {results.data && (
        <ul className="max-h-56 overflow-y-auto">
          {results.data
            .filter((w) => w.id !== from)
            .map((w) => (
              <li key={w.id}>
                <label className="flex cursor-pointer items-baseline gap-2 px-1 py-0.5 hover:bg-secondary">
                  <input type="radio" name={`move-${sessionId}`} checked={target === w.id} onChange={() => setTarget(w.id)} />
                  <span className="font-mono">{w.key}</span>
                  <span className="truncate text-muted-foreground">{w.title}</span>
                </label>
              </li>
            ))}
          {results.data.filter((w) => w.id !== from).length === 0 && <li className="text-muted-foreground">No other work item matches.</li>}
        </ul>
      )}
      <div className="flex gap-2">
        <Button
          variant="solid"
          disabled={!target || move.isPending}
          onClick={() => target && move.mutate({ sessionId, workItem: target }, { onSuccess: onClose })}
        >
          {move.isPending ? "Moving…" : "Move session"}
        </Button>
        <Button onClick={onClose}>Cancel</Button>
      </div>
      {move.isError && <ErrorNote error={move.error} />}
    </div>
  );
}
