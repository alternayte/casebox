import { keepPreviousData, useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, qs } from "@/lib/api";

// The JSON of server/Casebox.Server/Features/Evaluations/EvaluationEndpoints.cs. Enums are snake_case.
export type AgentSettings = { maxTurns: number | null; timeoutMinutes: number | null; tokenCap: number | null };
export type CommandTemplate = { template: string; logGlob: string | null; logFormat: string | null };

export type HarnessSpec = {
  agent: string;
  agentVersion: string;
  model: string;
  effort: string | null;
  harness: string;
  settings: AgentSettings;
  command: CommandTemplate | null;
  // A shared harness repository at a ref (casebox.yml harness.shared).
  shared?: { repo: string; ref: string } | null;
  // A proposal candidate's files over the harness at the ref (docs/specs/self-evolution.md).
  overrides?: string | null;
};

export type Estimate = {
  cases: number;
  runs: number;
  baselineTokens: number;
  candidateTokens: number;
  baselineUsd: number;
  candidateUsd: number;
  totalUsd: number;
  sandboxMinutes: number;
  minMinutes: number;
  maxMinutes: number;
  perRoundUsd: number;
  // The smallest pass-rate difference this size detects with power 0.8; absent on older evaluations.
  detectableEffect?: number | null;
};

export type Verdict = "better" | "worse" | "equivalent" | "inconclusive";

export type VerdictReached = {
  verdict: Verdict;
  delta: number;
  lower: number;
  upper: number;
  level: number;
  cases: number;
  runs: number;
  costRatio: number | null;
  costLower: number | null;
  costUpper: number | null;
  durationRatio: number | null;
  durationLower: number | null;
  durationUpper: number | null;
  equivalentAndCheaper: boolean;
  baselineRate: number;
  candidateRate: number;
  reason: string | null;
  // Harness CI: cases the baseline passed in every run and the candidate failed in every run.
  regressions?: string[] | null;
};

// A baseline evaluation's end: it compares nothing, so it has no verdict.
export type Scored = { cases: number; runs: number; passRate: number };

// The pull request or nightly run behind a harness CI or baseline evaluation.
export type CiRef = { kind: "baseline" | "pull_request"; repo: string; number: number | null; headSha: string | null };

export type EvaluationStatus = "awaiting_confirmation" | "running" | "done" | "cancelled";

export type EvaluationRow = {
  id: string;
  workspace: string;
  split: string;
  purpose: string;
  status: EvaluationStatus;
  change: string;
  baseline: HarnessSpec;
  candidate: HarnessSpec;
  repeats: number;
  delta: number;
  capUsd: number;
  estimate: Estimate;
  mutableModel: boolean;
  cases: number;
  spentUsd: number;
  runsCompleted: number;
  runsFailed: number;
  verdict: VerdictReached | null;
  reason: string | null;
  createdAt: string;
  updatedAt: string;
  scored: Scored | null;
  ci: CiRef | null;
};

export type Checkpoint = {
  round: number;
  level: number;
  cases: number;
  delta: number;
  lower: number;
  upper: number;
  verdict: Verdict;
  at: string;
};

export type EvaluationDetail = { evaluation: EvaluationRow; checkpoints: Checkpoint[] };

export type CaseResult = {
  caseId: string;
  weight: number;
  drift: boolean;
  baselineRuns: number;
  baselinePassed: number;
  candidateRuns: number;
  candidatePassed: number;
  failedRuns: number;
  baselineCostUsd: number;
  candidateCostUsd: number;
  runs: string[];
};

export type TestCount = { passed: number; total: number };

export type RunUsage = {
  inputTokens: number | null;
  outputTokens: number | null;
  cacheReadTokens: number | null;
  cacheWriteTokens: number | null;
  costUsd: number | null;
};

export type RunView = {
  runId: string;
  caseId: string;
  side: "baseline" | "candidate";
  repeat: number;
  status: string;
  passed: boolean | null;
  applied: boolean | null;
  usage: RunUsage | null;
  costUsd: number;
  seconds: number | null;
  turns: number | null;
  toolCalls: number | null;
  model: string | null;
  timedOut: boolean;
  tokenCapExceeded: boolean;
  processChecks: { ranTestsBeforeDone: boolean; editedTestAfterFailure: boolean } | null;
  tests: { failToPass: TestCount; passToPass: TestCount } | null;
  failedTests: string[] | null;
  assertions: { kind: string; passed: boolean }[] | null;
  judge: { answer: string; evidence: string } | null;
  reason: string | null;
  trace: unknown;
};

export type Offer = { offer: boolean; approvedCases: number; done: boolean };

// A running evaluation changes as its runs complete; the pages read it again every 10 seconds.
const live = (status: EvaluationStatus | undefined) => (status === "running" || status === "awaiting_confirmation" ? 10_000 : false);

export function useEvaluations(workspace: string | undefined) {
  return useQuery({
    queryKey: ["evaluations", "list", workspace ?? ""],
    queryFn: () => api<EvaluationRow[]>("GET", `/api/v1/evaluations${qs({ workspace })}`),
    placeholderData: keepPreviousData,
    refetchInterval: (q) => (q.state.data?.some((e) => live(e.status)) ? 10_000 : false),
  });
}

export function useEvaluation(id: string) {
  return useQuery({
    queryKey: ["evaluations", "one", id],
    queryFn: () => api<EvaluationDetail>("GET", `/api/v1/evaluations/${encodeURIComponent(id)}`),
    refetchInterval: (q) => live(q.state.data?.evaluation.status),
  });
}

// Held-out evaluations answer 403 here; the caller does not ask for them.
export function useCaseResults(id: string, enabled: boolean, status: EvaluationStatus | undefined) {
  return useQuery({
    queryKey: ["evaluations", "cases", id],
    queryFn: () => api<CaseResult[]>("GET", `/api/v1/evaluations/${encodeURIComponent(id)}/cases`),
    enabled,
    retry: (count, error) => !(error instanceof ApiError && error.status === 403) && count < 2,
    refetchInterval: live(status),
  });
}

export function useRun(id: string, runId: string) {
  return useQuery({
    queryKey: ["evaluations", "run", id, runId],
    queryFn: () => api<RunView>("GET", `/api/v1/evaluations/${encodeURIComponent(id)}/runs/${encodeURIComponent(runId)}`),
    retry: (count, error) => !(error instanceof ApiError && error.status === 403) && count < 2,
  });
}

// One offer question per workspace: the route answers for a single workspace.
export function useOffers(workspaces: string[]) {
  return useQueries({
    queries: workspaces.map((w) => ({
      queryKey: ["evaluations", "offer", w],
      queryFn: () => api<Offer>("GET", `/api/v1/evaluations/offer${qs({ workspace: w })}`),
    })),
    combine: (results) => workspaces.map((w, i) => ({ workspace: w, offer: results[i]?.data })),
  });
}

function useEvaluationMutation<T>(call: (body: T) => Promise<void>) {
  const client = useQueryClient();
  return useMutation({ mutationFn: call, onSuccess: () => client.invalidateQueries({ queryKey: ["evaluations"] }) });
}

export const useConfirm = (id: string) =>
  useEvaluationMutation<void>(() => api<void>("POST", `/api/v1/evaluations/${encodeURIComponent(id)}/confirmation`));
export const useCancel = (id: string) =>
  useEvaluationMutation((reason: string) => api<void>("POST", `/api/v1/evaluations/${encodeURIComponent(id)}/cancellation`, { reason }));
