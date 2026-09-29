import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, qs } from "@/lib/api";

// The JSON of server/Casebox.Server/Features/Patterns and Features/Proposals (docs/specs/self-evolution.md).
export type ProposalRef = { id: string; kind: string; status: string; prUrl: string | null };

export type Pattern = {
  id: string;
  workspace: string;
  wentWrong: string;
  label: string | null;
  prevention: string;
  path: string;
  title: string;
  summary: string;
  advisory: boolean;
  status: string;
  reason: string | null;
  corrections: number;
  people: number;
  phases: { inSession: number; beforeMerge: number; afterMerge: number };
  detectedAt: string;
  updatedAt: string;
  latestProposal: ProposalRef | null;
};

export type PatternList = { patterns: Pattern[]; hidden: number; k: number };

export type PatternDetail = {
  pattern: Pattern;
  quotes: { ref: string; text: string; day: string }[];
  cases: { id: string; status: string; split: string | null }[];
  proposals: ProposalRef[];
};

export type Edit = { op: string; file: string; heading?: string | null; old?: string | null; new?: string | null };

export type CandidateScore = {
  index: number;
  evaluation: string;
  delta: number;
  lower: number;
  upper: number;
  wins: string[];
  costRatio: number | null;
  runs: number;
};

export type Candidate = { index: number; edits: Edit[]; files: string[]; rationale: string; mergedFrom: number[] | null; score: CandidateScore | null };

export type GateCheck = { name: string; passed: boolean; evidence: string; detail: string };

export type Outcome = { before: number; after: number; beforeN: number; afterN: number; fell: boolean };

export type Proposal = {
  id: string;
  workspace: string;
  pattern: string | null;
  patternTitle: string | null;
  kind: string;
  status: string;
  repo: string;
  baseCommit: string;
  gateIndex: number | null;
  gateEvaluation: string | null;
  checks: GateCheck[] | null;
  reason: string | null;
  prNumber: number | null;
  prUrl: string | null;
  mergedAt: string | null;
  outcome: Outcome | null;
  createdAt: string;
  updatedAt: string;
};

export type ProposalDetail = { proposal: Proposal; candidates: Candidate[] };

export const usePatterns = (workspace?: string) =>
  useQuery({ queryKey: ["patterns", workspace ?? ""], queryFn: () => api<PatternList>("GET", `/api/v1/patterns/${qs({ workspace })}`) });

export const usePattern = (id: string) =>
  useQuery({ queryKey: ["patterns", "one", id], queryFn: () => api<PatternDetail>("GET", `/api/v1/patterns/${encodeURIComponent(id)}`), retry: false });

export const useProposals = (workspace?: string) =>
  useQuery({ queryKey: ["proposals", workspace ?? ""], queryFn: () => api<Proposal[]>("GET", `/api/v1/proposals/${qs({ workspace })}`) });

export const useProposal = (id: string) =>
  useQuery({ queryKey: ["proposals", "one", id], queryFn: () => api<ProposalDetail>("GET", `/api/v1/proposals/${encodeURIComponent(id)}`), retry: false });

function useInvalidating<T>(call: (arg: T) => Promise<void>, keys: string[]) {
  const client = useQueryClient();
  return useMutation({ mutationFn: call, onSuccess: () => keys.forEach((k) => client.invalidateQueries({ queryKey: [k] })) });
}

export const useAcknowledge = (id: string) =>
  useInvalidating(() => api<void>("POST", `/api/v1/patterns/${encodeURIComponent(id)}/acknowledgement`), ["patterns"]);

export const useDismiss = (id: string) =>
  useInvalidating((reason: string) => api<void>("POST", `/api/v1/patterns/${encodeURIComponent(id)}/dismissal`, { reason }), ["patterns"]);

export const useReject = (id: string) =>
  useInvalidating((reason: string) => api<void>("POST", `/api/v1/proposals/${encodeURIComponent(id)}/rejection`, { reason }), ["proposals", "patterns"]);

const statusNames: Record<string, string> = {
  open: "Open",
  acknowledged: "Acknowledged",
  dismissed: "Dismissed",
  resolved: "Resolved",
  searching: "Searching",
  gating: "At the gate",
  gate_passed: "Gate passed",
  gate_failed: "Gate failed",
  gate_inconclusive: "Parked: gate inconclusive",
  pr_opened: "Pull request open",
  merged: "Merged",
  rejected: "Rejected",
};

export const statusName = (s: string) => statusNames[s] ?? s.replaceAll("_", " ");

const opNames: Record<string, string> = {
  add_bullet: "Add a bullet",
  replace_bullet: "Replace a bullet",
  delete_bullet: "Delete a bullet",
  delete_section: "Delete a section",
  write_skill: "Write a skill",
  delete_skill: "Delete a skill",
};

export const opName = (op: string) => opNames[op] ?? op;
