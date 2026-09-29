import { Link } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { day, num, plural } from "@/lib/format";
import { caseKindName, caseKinds, caseScopeShortName, caseSplitName, caseSplits, caseStatusName, caseStatuses } from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { useWorkspaces } from "@/lib/workspaces";
import { Button, ErrorNote, Field, Loading, Select } from "@/ui/kit";
import { catalogPageSize, useBulkApprove, useCatalog, useQueue, type BulkResult, type CaseSummary, type CaseView, type CatalogFilters } from "./api";
import { Count, Glossary, SourceCell, SplitCell, StatusTag, WeightCell, WorkItemLink } from "./parts";

export type CasesSearch = CatalogFilters & { view?: "catalog"; page?: number };

// The Cases page: the review queue (validated cases with an instruction) and the catalog of every case.
export function CaseList({ search, onSearch }: { search: CasesSearch; onSearch: (s: CasesSearch) => void }) {
  const catalog = search.view === "catalog";
  const { view: _view, page, ...filters } = search;
  const queue = useQueue(search.workspace);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Cases</h1>
        <p className="text-sm text-muted-foreground">Replayable tasks from your history. A person approves each case before it counts in an evaluation.</p>
      </div>

      <div className="flex flex-wrap items-end gap-x-6 gap-y-3 border-b">
        <nav className="flex gap-1 text-sm" aria-label="Cases views">
          <ViewTab active={!catalog} onClick={() => onSearch({ workspace: search.workspace })}>
            Review queue
            {queue.data && <span className="ml-1.5 font-mono text-xs tabular-nums text-muted-foreground">{num(queue.data.length)}</span>}
          </ViewTab>
          <ViewTab active={catalog} onClick={() => onSearch({ ...filters, view: "catalog" })}>
            Catalog
          </ViewTab>
        </nav>
        <Filters catalog={catalog} value={filters} onChange={(f) => onSearch(catalog ? { ...f, view: "catalog" } : { workspace: f.workspace })} />
      </div>

      {catalog ? (
        <Catalog filters={filters} page={page ?? 1} onPage={(p) => onSearch({ ...filters, view: "catalog", page: p > 1 ? p : undefined })} />
      ) : (
        <Queue query={queue} />
      )}

      <Glossary className="max-w-3xl border-t pt-3" />
    </div>
  );
}

function ViewTab({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-current={active ? "page" : undefined}
      className={cn("-mb-px border-b-2 px-2 pb-2 text-muted-foreground hover:text-foreground", active ? "border-foreground font-medium text-foreground" : "border-transparent")}
    >
      {children}
    </button>
  );
}

// Filters apply at once and live in the URL, so a view is a link.
function Filters({ catalog, value, onChange }: { catalog: boolean; value: CatalogFilters; onChange: (f: CatalogFilters) => void }) {
  const workspaces = useWorkspaces();
  const set = (key: keyof CatalogFilters) => (e: { target: { value: string } }) => onChange({ ...value, [key]: e.target.value || undefined });
  const names = [...new Set([...(workspaces.data ?? []).map((w) => w.name), ...(value.workspace ? [value.workspace] : [])])];
  return (
    <div className="ml-auto flex flex-wrap items-end gap-3 pb-2">
      <Field label="Workspace" className="w-40">
        <Select value={value.workspace ?? ""} onChange={set("workspace")}>
          <option value="">All</option>
          {names.map((w) => (
            <option key={w} value={w}>
              {w}
            </option>
          ))}
        </Select>
      </Field>
      {catalog && (
        <>
          <Field label="Status" className="w-44">
            <Select value={value.status ?? ""} onChange={set("status")}>
              <option value="">All</option>
              {caseStatuses.map((s) => (
                <option key={s} value={s}>
                  {caseStatusName(s)}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Kind" className="w-32">
            <Select value={value.kind ?? ""} onChange={set("kind")}>
              <option value="">All</option>
              {caseKinds.map((k) => (
                <option key={k} value={k}>
                  {caseKindName(k)}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Split" className="w-32">
            <Select value={value.split ?? ""} onChange={set("split")}>
              <option value="">All</option>
              {caseSplits.map((s) => (
                <option key={s} value={s}>
                  {caseSplitName(s)}
                </option>
              ))}
            </Select>
          </Field>
        </>
      )}
    </div>
  );
}

const th = "py-1.5 pr-4 font-medium";
const td = "py-1.5 pr-4";

function HeadCells() {
  return (
    <>
      <th className={th}>Case</th>
      <th className={th}>Kind</th>
      <th className={th}>Source</th>
      <th className={th}>Work item</th>
      <th className={th}>Scope</th>
      <th className={th}>Status</th>
      <th className={cn(th, "text-right")} title="Fail-to-pass tests">
        F→P
      </th>
      <th className={cn(th, "text-right")} title="Pass-to-pass tests">
        P→P
      </th>
      <th className={th}>Weight</th>
      <th className={th}>Split</th>
    </>
  );
}

function RowCells({ c }: { c: CaseSummary }) {
  return (
    <>
      <td className={cn(td, "whitespace-nowrap font-mono text-xs")}>
        <Link to="/cases/$id" params={{ id: c.id }} className="text-link hover:underline" title={c.id}>
          {c.id.slice(0, 10)}
        </Link>
      </td>
      <td className={cn(td, "text-xs")}>{caseKindName(c.kind)}</td>
      <td className={cn(td, "max-w-56")}>
        <SourceCell source={c.source} />
      </td>
      <td className={cn(td, "whitespace-nowrap text-xs")}>
        <WorkItemLink id={c.workItem} />
      </td>
      <td className={cn(td, "text-xs")}>{caseScopeShortName(c.scope)}</td>
      <td className={td}>
        <StatusTag status={c.status} />
      </td>
      <td className={cn(td, "text-right")}>
        <Count n={c.failToPass} />
      </td>
      <td className={cn(td, "text-right")}>
        <Count n={c.passToPass} />
      </td>
      <td className={cn(td, "text-xs")}>
        <WeightCell weight={c.weight} drift={c.drift} validated={c.failToPass != null} />
      </td>
      <td className={td}>
        <SplitCell split={c.split} />
      </td>
    </>
  );
}

function Queue({ query }: { query: ReturnType<typeof useQueue> }) {
  const me = useMe();
  const canApprove = isMember(me.data);
  const bulk = useBulkApprove();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [result, setResult] = useState<BulkResult>();

  if (query.isPending) return <Loading what="the review queue" />;
  if (query.isError) return <ErrorNote error={query.error} />;

  const cases = query.data;
  const ready = cases.filter((c) => c.approvalBlocker === null).map((c) => c.summary.id);
  const chosen = [...selected].filter((id) => cases.some((c) => c.summary.id === id));

  function toggle(id: string) {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setSelected(next);
  }

  function approve() {
    setResult(undefined);
    bulk.mutate(chosen, {
      onSuccess: (r) => {
        setResult(r);
        setSelected(new Set(r.refused.map((x) => x.id)));
      },
    });
  }

  return (
    <div className="space-y-3">
      <p className="max-w-3xl border-l-2 border-link pl-3 text-sm">
        The queue holds validated cases with a drafted instruction. Read each instruction: it must state the task without hints about the fix.
        {canApprove ? " Select cases to approve several at once." : " A Member can approve, edit or reject a case."}
      </p>

      {canApprove && cases.length > 0 && (
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <Button onClick={() => setSelected(new Set(ready))} disabled={ready.length === 0}>
            Select all ready ({num(ready.length)})
          </Button>
          <Button onClick={() => setSelected(new Set())} disabled={chosen.length === 0}>
            Clear
          </Button>
          <Button variant="solid" onClick={approve} disabled={chosen.length === 0 || bulk.isPending}>
            {bulk.isPending ? "Approving…" : `Approve ${plural(chosen.length, "case")}`}
          </Button>
        </div>
      )}
      {bulk.isError && <ErrorNote error={bulk.error} />}
      {result && <BulkOutcome result={result} />}

      <div className={cn("overflow-x-auto", query.isPlaceholderData && "opacity-60")}>
        <table className="w-full min-w-[62rem] text-sm">
          <thead>
            <tr className="border-b-2 border-foreground text-left text-2xs uppercase tracking-label text-muted-foreground">
              {canApprove && (
                <th className="w-6 py-1.5 pr-2">
                  <span className="sr-only">Select</span>
                </th>
              )}
              <HeadCells />
            </tr>
          </thead>
          <tbody>
            {cases.length === 0 && (
              <tr>
                <td colSpan={11} className="py-4 text-muted-foreground">
                  The queue is empty. Run casebox mine to find cases; they arrive here after validation and a drafted instruction.
                </td>
              </tr>
            )}
            {cases.map((c) => (
              <QueueRow key={c.summary.id} c={c} canApprove={canApprove} checked={selected.has(c.summary.id)} onToggle={() => toggle(c.summary.id)} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function QueueRow({ c, canApprove, checked, onToggle }: { c: CaseView; canApprove: boolean; checked: boolean; onToggle: () => void }) {
  const blocker = c.approvalBlocker;
  return (
    <>
      <tr className="align-baseline">
        {canApprove && (
          <td className="py-1.5 pr-2">
            <input
              type="checkbox"
              checked={checked}
              onChange={onToggle}
              disabled={blocker !== null && !checked}
              aria-label={`Select case ${c.summary.id}`}
              title={blocker ?? undefined}
            />
          </td>
        )}
        <RowCells c={c.summary} />
      </tr>
      <tr className="border-b border-dashed">
        {canApprove && <td />}
        <td colSpan={10} className="pb-2 pr-4 text-xs">
          {c.instruction ? (
            <p className="line-clamp-2 whitespace-pre-line text-muted-foreground">{c.instruction}</p>
          ) : (
            <p className="text-muted-foreground">No readable instruction.</p>
          )}
          {blocker && <p className="mt-0.5 text-signal">Cannot be approved yet: {blocker}</p>}
        </td>
      </tr>
    </>
  );
}

function BulkOutcome({ result }: { result: BulkResult }) {
  return (
    <div role="status" className="space-y-1 border-l-2 border-foreground pl-3 text-sm">
      <p>
        Approved {plural(result.approved.length, "case")}.
        {result.refused.length > 0 ? ` The server refused ${plural(result.refused.length, "case")}:` : " The server refused none."}
      </p>
      {result.refused.length > 0 && (
        <ul className="space-y-0.5 text-xs">
          {result.refused.map((r) => (
            <li key={r.id}>
              <Link to="/cases/$id" params={{ id: r.id }} className="font-mono text-link hover:underline">
                {r.id.slice(0, 10)}
              </Link>{" "}
              <span className="text-signal">{r.reason}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Catalog({ filters, page, onPage }: { filters: CatalogFilters; page: number; onPage: (page: number) => void }) {
  const catalog = useCatalog(filters, page);
  if (catalog.isPending) return <Loading what="the catalog" />;
  if (catalog.isError) return <ErrorNote error={catalog.error} />;
  const { cases, more } = catalog.data;
  const filtered = Object.values(filters).some(Boolean);
  const first = (page - 1) * catalogPageSize;
  return (
    <div className="space-y-2">
      <div className={cn("overflow-x-auto", catalog.isPlaceholderData && "opacity-60")}>
        <table className="w-full min-w-[66rem] text-sm">
          <thead>
            <tr className="border-b-2 border-foreground text-left text-2xs uppercase tracking-label text-muted-foreground">
              <HeadCells />
              <th className="py-1.5 font-medium">Mined</th>
            </tr>
          </thead>
          <tbody>
            {cases.length === 0 && (
              <tr>
                <td colSpan={11} className="py-4 text-muted-foreground">
                  {page > 1
                    ? "No cases on this page."
                    : filtered
                      ? "No case matches these filters."
                      : "No cases yet. Confirm a workspace recipe, then run casebox mine."}
                </td>
              </tr>
            )}
            {cases.map((c) => (
              <tr key={c.id} className="border-b border-dashed align-baseline hover:bg-card">
                <RowCells c={c} />
                <td className="py-1.5 font-mono text-xs text-muted-foreground">{day(c.minedAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {(page > 1 || more) && (
        <div className="flex items-center justify-end gap-2 text-xs">
          <span className="font-mono tabular-nums text-muted-foreground">
            {cases.length > 0 ? `${num(first + 1)}–${num(first + cases.length)}` : "—"} by rank
          </span>
          <Button variant="quiet" disabled={page <= 1} onClick={() => onPage(page - 1)}>
            Previous
          </Button>
          <span className="font-mono tabular-nums">page {num(page)}</span>
          <Button variant="quiet" disabled={!more} onClick={() => onPage(page + 1)}>
            Next
          </Button>
        </div>
      )}
    </div>
  );
}
