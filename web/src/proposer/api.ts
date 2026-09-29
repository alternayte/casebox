import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, qs } from "@/lib/api";

// The JSON of server/Casebox.Server/Features/Patterns and Features/Proposals (docs/specs/simple-evolution.md).
export type ProposalRef = { id: string; kind: string; title: string; status: string };

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
  advisoryNote: string | null;
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

export type Quote = { ref: string; text: string; day: string };

export type PatternDetail = { pattern: Pattern; quotes: Quote[]; proposals: ProposalRef[] };

export type Edit = { op: string; file: string; heading?: string | null; old?: string | null; new?: string | null };

export type FilePreview = { path: string; before: string | null; after: string };

export type CodeNote = { what: string; why: string; prompt: string };

export type Outcome = { before: number; after: number; beforeN: number; afterN: number; fell: boolean };

export type Proposal = {
  id: string;
  workspace: string;
  pattern: string;
  patternTitle: string;
  repo: string;
  kind: string;
  title: string;
  status: string;
  createdAt: string;
  appliedAt: string | null;
  appliedMode: string | null;
  outcome: Outcome | null;
};

export type ProposalList = { proposals: Proposal[]; hidden: number };

export type ProposalDetail = {
  proposal: Proposal;
  rationale: string;
  reason: string | null;
  edits: Edit[];
  preview: FilePreview[];
  note: CodeNote | null;
  baseCommit: string;
  evidence: { title: string; summary: string; corrections: number; people: number; meetsK: boolean; quotes: Quote[] };
};

export const usePatterns = (workspace?: string) =>
  useQuery({ queryKey: ["patterns", workspace ?? ""], queryFn: () => api<PatternList>("GET", `/api/v1/patterns/${qs({ workspace })}`) });

export const usePattern = (id: string) =>
  useQuery({ queryKey: ["patterns", "one", id], queryFn: () => api<PatternDetail>("GET", `/api/v1/patterns/${encodeURIComponent(id)}`), retry: false });

export const useProposals = (workspace?: string) =>
  useQuery({ queryKey: ["proposals", workspace ?? ""], queryFn: () => api<ProposalList>("GET", `/api/v1/proposals/${qs({ workspace })}`) });

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

export const useApprove = (id: string) =>
  useInvalidating(() => api<void>("POST", `/api/v1/proposals/${encodeURIComponent(id)}/approval`), ["proposals", "patterns"]);

export const useReject = (id: string) =>
  useInvalidating((reason: string) => api<void>("POST", `/api/v1/proposals/${encodeURIComponent(id)}/rejection`, { reason }), ["proposals", "patterns"]);

const statusNames: Record<string, string> = {
  open: "Waiting for you",
  approved: "Approved, not applied",
  applied: "Applied",
  rejected: "Rejected",
};

export const statusName = (s: string) => statusNames[s] ?? s.replaceAll("_", " ");

const patternStatusNames: Record<string, string> = {
  open: "Open",
  acknowledged: "Acknowledged",
  dismissed: "Dismissed",
  resolved: "Resolved",
};

export const patternStatusName = (s: string) => patternStatusNames[s] ?? s.replaceAll("_", " ");

const kindNames: Record<string, string> = {
  harness_edit: "Instruction edit",
  skill: "Skill",
  mcp: "MCP server",
  code_note: "Code note",
};

export const kindName = (k: string) => kindNames[k] ?? k.replaceAll("_", " ");
