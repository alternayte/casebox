import { Link } from "@tanstack/react-router";
import { num } from "@/lib/format";
import { caseStatusName, caseSplitName } from "@/lib/labels";
import { cn } from "@/lib/utils";
import { Tag } from "@/ui/kit";
import { keyOf } from "@/work/api";
import { sourceOf } from "./api";

// One line each for the words a reviewer needs to read the counts, the weight and the split.
export function Glossary({ className }: { className?: string }) {
  const lines: [string, string][] = [
    ["Fail-to-pass", "Tests that fail at the base commit and pass with the merged change. They prove the work is done."],
    ["Pass-to-pass", "Tests in the touched packages that pass both times. They catch collateral damage."],
    ["Drift", "The harness names a path or command that does not exist at the base commit. Drift halves the case's weight."],
    ["Split", "Every fifth approved case of a workspace goes to the held-out set. The rest go to the dev set."],
  ];
  return (
    <dl className={cn("grid gap-x-4 gap-y-1 text-xs sm:grid-cols-[7rem_minmax(0,1fr)]", className)}>
      {lines.map(([term, text]) => (
        <div key={term} className="contents">
          <dt className="font-medium">{term}</dt>
          <dd className="text-muted-foreground">{text}</dd>
        </div>
      ))}
    </dl>
  );
}

export function StatusTag({ status }: { status: string }) {
  const tone = status === "approved" ? "ok" : status === "validation_failed" || status === "rejected" ? "signal" : "plain";
  return <Tag tone={tone}>{caseStatusName(status)}</Tag>;
}

export function SourceCell({ source }: { source: string }) {
  const s = sourceOf(source);
  return (
    <span className="flex min-w-0 flex-col">
      <span className="text-xs">{s.what}</span>
      <span className="truncate font-mono text-2xs text-muted-foreground" title={s.ref}>
        {s.ref}
      </span>
    </span>
  );
}

export function WorkItemLink({ id }: { id: string | null }) {
  if (!id) return <span className="text-muted-foreground">None</span>;
  return (
    <Link to="/work/item" search={{ id }} className="font-mono text-xs text-link hover:underline">
      {keyOf(id)}
    </Link>
  );
}

export function Count({ n }: { n: number | null }) {
  return <span className="font-mono tabular-nums">{n == null ? "—" : num(n)}</span>;
}

export function WeightCell({ weight, drift, validated }: { weight: number; drift: boolean; validated: boolean }) {
  if (!validated) return <span className="text-muted-foreground">—</span>;
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono tabular-nums">{num(weight)}</span>
      {drift && <Tag tone="signal">drift</Tag>}
    </span>
  );
}

export function SplitCell({ split }: { split: string | null }) {
  if (!split) return <span className="text-muted-foreground">—</span>;
  return <span className={cn("text-xs", split === "held_out" && "font-medium")}>{caseSplitName(split)}</span>;
}
