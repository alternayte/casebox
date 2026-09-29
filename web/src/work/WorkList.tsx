import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { day, num } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Input, Loading, Tag } from "@/ui/kit";
import { useWorkItems } from "./api";

// Work items, newest activity first. Every report groups by them.
export function WorkList({ query, onQuery }: { query?: string; onQuery: (q: string | undefined) => void }) {
  const items = useWorkItems(query);
  const [draft, setDraft] = useState(query ?? "");

  function submit(e: FormEvent) {
    e.preventDefault();
    onQuery(draft.trim() || undefined);
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-end gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Work</h1>
          <p className="text-sm text-muted-foreground">Jira and GitHub issues, with the sessions and pull requests linked to them.</p>
        </div>
        <form onSubmit={submit} className="ml-auto flex w-full max-w-sm gap-2">
          <Input type="search" aria-label="Search work items" placeholder="Key or title" value={draft} onChange={(e) => setDraft(e.target.value)} />
          <Button type="submit" variant="solid">
            Search
          </Button>
        </form>
      </div>

      {items.isPending && <Loading what="work items" />}
      {items.isError && <ErrorNote error={items.error} />}
      {items.data && (
        <div className={cn("overflow-x-auto", items.isPlaceholderData && "opacity-60")}>
          <table className="w-full min-w-[52rem] text-sm">
            <thead>
              <tr className="border-b-2 border-foreground text-left text-2xs uppercase tracking-label text-muted-foreground">
                <th className="py-1.5 pr-4 font-medium">Key</th>
                <th className="py-1.5 pr-4 font-medium">Title</th>
                <th className="py-1.5 pr-4 font-medium">Type</th>
                <th className="py-1.5 pr-4 font-medium">Status</th>
                <th className="py-1.5 pr-4 text-right font-medium">Sessions</th>
                <th className="py-1.5 pr-4 text-right font-medium">Pull requests</th>
                <th className="py-1.5 pr-4 text-right font-medium">Merged</th>
                <th className="py-1.5 font-medium">Updated</th>
              </tr>
            </thead>
            <tbody>
              {items.data.length === 0 && (
                <tr>
                  <td colSpan={8} className="py-4 text-muted-foreground">
                    {query
                      ? "No work item matches this search."
                      : "No work items yet. Connect Jira or GitHub with casebox init; items appear when the next poll runs."}
                  </td>
                </tr>
              )}
              {items.data.map((w) => (
                <tr key={w.id} className="border-b border-dashed align-baseline hover:bg-card">
                  <td className="whitespace-nowrap py-1.5 pr-4 font-mono text-xs">
                    <Link to="/work/item" search={{ id: w.id }} className="text-link hover:underline">
                      {w.key}
                    </Link>
                  </td>
                  <td className="max-w-md truncate py-1.5 pr-4" title={w.title}>
                    {w.title || <span className="text-muted-foreground">No title</span>}
                  </td>
                  <td className="py-1.5 pr-4 text-xs text-muted-foreground">{w.type ?? "—"}</td>
                  <td className="py-1.5 pr-4 text-xs">
                    {w.resolved ? <Tag tone="ok">{w.status ?? "resolved"}</Tag> : (w.status ?? "—")}
                  </td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{num(w.sessions)}</td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{num(w.pullRequests)}</td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{num(w.merged)}</td>
                  <td className="py-1.5 font-mono text-xs text-muted-foreground">{day(w.updatedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {items.data.length === 100 && <p className="mt-2 text-xs text-muted-foreground">Showing the 100 most recently updated. Search to find others.</p>}
        </div>
      )}
    </div>
  );
}
