import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api } from "@/lib/api";
import { agentName, groupByName, groupBys, taskTypeName, taskTypes, type GroupBy } from "@/lib/labels";
import { Button, Field, Input, Select } from "@/ui/kit";
import type { Filters } from "./types";

type Workspace = { name: string; repos: string[] };

// The period and filters of the report. Applying writes them to the URL, so a view is a link.
export function FilterBar({
  value,
  agents,
  onApply,
  showGroupBy = true,
}: {
  value: Filters;
  agents: string[];
  onApply: (f: Filters) => void;
  showGroupBy?: boolean;
}) {
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: () => api<Workspace[]>("GET", "/api/v1/workspaces") });
  // The parent keys this component on the applied filters, so the draft restarts when they change.
  const [draft, setDraft] = useState<Filters>(value);

  const repos = [...new Set((workspaces.data ?? []).flatMap((w) => w.repos))].sort();
  const set = (key: keyof Filters) => (e: { target: { value: string } }) => setDraft({ ...draft, [key]: e.target.value || undefined });

  function submit(e: FormEvent) {
    e.preventDefault();
    const clean: Filters = {};
    for (const [key, v] of Object.entries(draft)) if (v) (clean as Record<string, string>)[key] = v.trim();
    onApply(clean);
  }

  return (
    <form onSubmit={submit} className="grid grid-cols-2 items-end gap-x-3 gap-y-2 border-y bg-card px-3 py-3 sm:grid-cols-4 lg:grid-cols-10">
      <Field label="From">
        <Input type="date" value={draft.from ?? ""} onChange={set("from")} />
      </Field>
      <Field label="To">
        <Input type="date" value={draft.to ?? ""} onChange={set("to")} />
      </Field>
      <Field label="Workspace">
        <Select value={draft.workspace ?? ""} onChange={set("workspace")}>
          <option value="">All</option>
          {(workspaces.data ?? []).map((w) => (
            <option key={w.name} value={w.name}>
              {w.name}
            </option>
          ))}
        </Select>
      </Field>
      <Field label="Repository">
        <Input list="cbx-repos" value={draft.repo ?? ""} onChange={set("repo")} placeholder="All" />
        <datalist id="cbx-repos">
          {repos.map((r) => (
            <option key={r} value={r} />
          ))}
        </datalist>
      </Field>
      <Field label="Agent">
        <Select value={draft.agent ?? ""} onChange={set("agent")}>
          <option value="">All</option>
          {[...new Set([...agents, ...(draft.agent ? [draft.agent] : [])])].map((a) => (
            <option key={a} value={a}>
              {agentName(a)}
            </option>
          ))}
        </Select>
      </Field>
      <Field label="Model">
        <Input value={draft.model ?? ""} onChange={set("model")} placeholder="All" />
      </Field>
      <Field label="Harness version">
        <Input value={draft.harness ?? ""} onChange={set("harness")} placeholder="All" className="font-mono" />
      </Field>
      <Field label="Task type">
        <Select value={draft.taskType ?? ""} onChange={set("taskType")}>
          <option value="">All</option>
          {taskTypes.map((t) => (
            <option key={t} value={t}>
              {taskTypeName(t)}
            </option>
          ))}
        </Select>
      </Field>
      {showGroupBy ? (
        <Field label="Group by">
          <Select value={draft.groupBy ?? ""} onChange={(e) => setDraft({ ...draft, groupBy: (e.target.value || undefined) as GroupBy | undefined })}>
            <option value="">None</option>
            {groupBys.map((g) => (
              <option key={g} value={g}>
                {groupByName(g)}
              </option>
            ))}
          </Select>
        </Field>
      ) : (
        <div className="hidden lg:block" />
      )}
      <div className="flex gap-2">
        <Button type="submit" variant="solid">
          Apply
        </Button>
        <Button type="button" onClick={() => onApply({})}>
          Reset
        </Button>
      </div>
    </form>
  );
}
