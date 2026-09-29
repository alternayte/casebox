import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { ApiError } from "@/lib/api";
import { num, pct } from "@/lib/format";
import {
  intentName,
  intents,
  labelSourceName,
  phaseName,
  preventionName,
  preventions,
  signalName,
  wentWrongName,
  wentWrongs,
} from "@/lib/labels";
import { isMember, useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import { Button, ErrorNote, Field, Hidden, Input, Loading, Select, Tag } from "@/ui/kit";
import { useInterventions, useRelabel } from "./api";
import type { Filters, Intervention } from "./types";

// The interventions behind one theme, or the unclassified ones, with a relabel form for Members.
export function ThemePage({ wentWrong, filters, page, onPage }: { wentWrong: string; filters: Filters; page: number; onPage: (page: number) => void }) {
  const list = useInterventions(wentWrong, filters, page);
  const me = useMe();
  const canRelabel = isMember(me.data);
  const data = list.data;
  const pages = data ? Math.max(1, Math.ceil(data.total / data.pageSize)) : 1;
  const belowK = list.error instanceof ApiError && (list.error.status === 403 || list.error.status === 404);

  return (
    <div className="space-y-6">
      <div>
        <Link to="/" search={filters} className="text-xs text-link hover:underline">
          ← Steering report
        </Link>
        <h1 className="mt-1 text-2xl font-semibold tracking-tight">{wentWrongName(wentWrong)}</h1>
        <p className="text-sm text-muted-foreground">
          {wentWrong === "unclassified"
            ? "Interventions the classifier could not label with enough confidence, or that hold no text."
            : "Corrections the classifier or a person put in this class."}{" "}
          Text is masked. Each entry shows only its day.
        </p>
      </div>

      <p className="max-w-3xl border-l-2 border-link pl-3 text-sm">
        A relabel replaces the model's label for good, and the worker uses recent relabels as examples the next time it classifies. Relabel when a
        label is wrong; the report and the classifier both learn from it.
        {!canRelabel && me.data && " Viewers can read labels; a Member can change them."}
      </p>

      {list.isPending && <Loading what="interventions" />}
      {list.isError && (belowK ? <p className="text-sm"><Hidden /> <span className="text-muted-foreground">{list.error.message}</span></p> : <ErrorNote error={list.error} />)}
      {data && (
        <>
          <div className="flex items-baseline justify-between border-b-2 border-foreground pb-1 text-xs">
            <span className="font-mono tabular-nums">
              n = {num(data.total)} interventions
            </span>
            <Pager page={page} pages={pages} onPage={onPage} />
          </div>
          {data.interventions.length === 0 ? (
            <p className="text-sm text-muted-foreground">No interventions on this page.</p>
          ) : (
            <ol className={cn("divide-y", list.isPlaceholderData && "opacity-60")}>
              {data.interventions.map((i) => (
                <Row key={i.ref} i={i} canRelabel={canRelabel} />
              ))}
            </ol>
          )}
          <div className="flex justify-end border-t pt-2 text-xs">
            <Pager page={page} pages={pages} onPage={onPage} />
          </div>
        </>
      )}
    </div>
  );
}

function Pager({ page, pages, onPage }: { page: number; pages: number; onPage: (page: number) => void }) {
  return (
    <span className="flex items-center gap-2">
      <Button variant="quiet" disabled={page <= 1} onClick={() => onPage(page - 1)}>
        Previous
      </Button>
      <span className="font-mono tabular-nums">
        {page} / {pages}
      </span>
      <Button variant="quiet" disabled={page >= pages} onClick={() => onPage(page + 1)}>
        Next
      </Button>
    </span>
  );
}

function Row({ i, canRelabel }: { i: Intervention; canRelabel: boolean }) {
  const [open, setOpen] = useState(false);
  const [saved, setSaved] = useState(false);
  return (
    <li className="grid gap-3 py-3 lg:grid-cols-[11rem_minmax(0,1fr)_17rem]">
      <div className="space-y-1 text-xs">
        <div className="font-medium">{signalName(i.signal)}</div>
        <div className="text-muted-foreground">{phaseName(i.phase)}</div>
        <div className="font-mono text-muted-foreground">{i.day}</div>
      </div>
      <div>
        {i.text ? (
          <blockquote className="whitespace-pre-wrap break-words font-serif text-sm italic">“{i.text}”</blockquote>
        ) : (
          <p className="text-xs text-muted-foreground">No text. Prompt mode is off, or the signal holds none.</p>
        )}
      </div>
      <div className="space-y-2 text-xs">
        <Labels i={i} />
        {canRelabel && !open && (
          <div className="flex items-center gap-2">
            <Button onClick={() => setOpen(true)}>Relabel</Button>
            {saved && <span className="text-ok">Saved</span>}
          </div>
        )}
        {open && (
          <RelabelForm
            i={i}
            onDone={(ok) => {
              setOpen(false);
              setSaved(ok);
            }}
          />
        )}
      </div>
    </li>
  );
}

function Labels({ i }: { i: Intervention }) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
      <dt className="text-muted-foreground">Intent</dt>
      <dd className={cn(i.intent === "correction" && "text-signal")}>{intentName(i.intent)}</dd>
      {i.intent === "correction" && (
        <>
          <dt className="text-muted-foreground">Went wrong</dt>
          <dd>{wentWrongName(i.wentWrong, i.wentWrongLabel)}</dd>
          <dt className="text-muted-foreground">Prevention</dt>
          <dd>{preventionName(i.prevention)}</dd>
        </>
      )}
      <dt className="text-muted-foreground">Source</dt>
      <dd className="flex flex-wrap items-center gap-1.5">
        {i.labelSource ? <Tag tone={i.labelSource === "human" ? "ok" : "plain"}>{labelSourceName(i.labelSource)}</Tag> : "Not labelled"}
        {i.confidence != null && i.labelSource === "model" && <span className="font-mono tabular-nums text-muted-foreground">{pct(i.confidence)} confident</span>}
      </dd>
    </dl>
  );
}

function words(s: string) {
  return s.trim().split(/\s+/).filter(Boolean).length;
}

function RelabelForm({ i, onDone }: { i: Intervention; onDone: (saved: boolean) => void }) {
  const relabel = useRelabel();
  const [intent, setIntent] = useState(i.intent ?? "");
  const [wentWrong, setWentWrong] = useState(i.wentWrong ?? "");
  const [label, setLabel] = useState(i.wentWrongLabel ?? "");
  const [prevention, setPrevention] = useState(i.prevention ?? "");

  const correction = intent === "correction";
  const labelTooLong = wentWrong === "other" && words(label) > 6;
  const valid = intent !== "" && (!correction || (wentWrong !== "" && prevention !== "" && (wentWrong !== "other" || words(label) > 0) && !labelTooLong));

  function submit(e: FormEvent) {
    e.preventDefault();
    relabel.mutate(
      {
        ref: i.ref,
        intent,
        ...(correction ? { wentWrong, prevention, ...(wentWrong === "other" ? { wentWrongLabel: label.trim() } : {}) } : {}),
      },
      { onSuccess: () => onDone(true) },
    );
  }

  return (
    <form onSubmit={submit} className="space-y-2 border border-input bg-card p-2">
      <Field label="Intent">
        <Select value={intent} onChange={(e) => setIntent(e.target.value)} required>
          <option value="" disabled>
            Choose…
          </option>
          {intents.map((v) => (
            <option key={v} value={v}>
              {intentName(v)}
            </option>
          ))}
        </Select>
      </Field>
      {correction && (
        <>
          <Field label="What went wrong">
            <Select value={wentWrong} onChange={(e) => setWentWrong(e.target.value)} required>
              <option value="" disabled>
                Choose…
              </option>
              {wentWrongs.map((v) => (
                <option key={v} value={v}>
                  {wentWrongName(v)}
                </option>
              ))}
            </Select>
          </Field>
          {wentWrong === "other" && (
            <Field label="Short label, at most 6 words">
              <Input value={label} onChange={(e) => setLabel(e.target.value)} required aria-invalid={labelTooLong} />
              {labelTooLong && <span className="text-destructive">Use 6 words or fewer.</span>}
            </Field>
          )}
          <Field label="What would have prevented it">
            <Select value={prevention} onChange={(e) => setPrevention(e.target.value)} required>
              <option value="" disabled>
                Choose…
              </option>
              {preventions.map((v) => (
                <option key={v} value={v}>
                  {preventionName(v)}
                </option>
              ))}
            </Select>
          </Field>
        </>
      )}
      {!correction && intent !== "" && <p className="text-muted-foreground">Only corrections count as steering. This one leaves the theme.</p>}
      <div className="flex gap-2">
        <Button type="submit" variant="solid" disabled={!valid || relabel.isPending}>
          {relabel.isPending ? "Saving…" : "Save label"}
        </Button>
        <Button type="button" onClick={() => onDone(false)}>
          Cancel
        </Button>
      </div>
      {relabel.isError && <ErrorNote error={relabel.error} />}
    </form>
  );
}
