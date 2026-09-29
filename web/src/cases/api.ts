import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, qs } from "@/lib/api";

// The JSON of server/Casebox.Server/Features/Cases/CaseEndpoints.cs. Enums are snake_case.
export type CaseSummary = {
  id: string;
  kind: string;
  workspace: string;
  status: string;
  scope: string;
  source: string;
  workItem: string | null;
  rank: number;
  failToPass: number | null;
  passToPass: number | null;
  drift: boolean;
  weight: number;
  split: string | null;
  hasInstruction: boolean;
  minedAt: string;
  updatedAt: string;
};

export type CaseRepo = { repo: string; base: string; merged: string | null; role: string };
export type Assertion = { kind: string; path?: string | null; pattern?: string | null };
export type JudgeQuestion = { question: string };

export type CaseView = {
  summary: CaseSummary;
  repos: CaseRepo[];
  instruction: string | null;
  signatures: string[];
  assertions: Assertion[];
  judge: JudgeQuestion[];
  assertionsApproved: boolean;
  oracle: string | null;
  seconds: number | null;
  failureReason: string | null;
  failureDetail: string | null;
  rejectReason: string | null;
  retiredReason: string | null;
  recipeHash: string;
  harnessHash: string;
  // The server's reason the case cannot be approved now, or null.
  approvalBlocker: string | null;
};

export type ValidationRun = {
  at: string;
  passed: boolean;
  reason: string | null;
  detail: string | null;
  failToPass: number | null;
  passToPass: number | null;
  seconds: number | null;
};

export type OracleTests = {
  tests: { failToPass?: string[] | null; passToPass?: string[] | null } | null;
  testFiles: string[] | null;
  commands: { command: string; results?: string | null }[] | null;
};

export type CatalogFilters = { status?: string; kind?: string; workspace?: string; split?: string };

export type BulkResult = { approved: string[]; refused: { id: string; reason: string }[] };

export const catalogPageSize = 200;

export function useQueue(workspace: string | undefined) {
  return useQuery({
    queryKey: ["cases", "queue", workspace ?? ""],
    queryFn: () => api<CaseView[]>("GET", `/api/v1/cases/queue${qs({ workspace })}`),
    placeholderData: keepPreviousData,
  });
}

// One page of the catalog. It asks for one row more than a page to learn whether a next page exists.
export function useCatalog(f: CatalogFilters, page: number) {
  return useQuery({
    queryKey: ["cases", "catalog", f, page],
    queryFn: async () => {
      const rows = await api<CaseSummary[]>("GET", `/api/v1/cases${qs({ ...f, limit: catalogPageSize + 1, offset: (page - 1) * catalogPageSize })}`);
      return { cases: rows.slice(0, catalogPageSize), more: rows.length > catalogPageSize };
    },
    placeholderData: keepPreviousData,
  });
}

export const caseKey = (id: string) => ["cases", "one", id];

export function useValidations(id: string) {
  return useQuery({
    queryKey: ["cases", "validations", id],
    queryFn: () => api<ValidationRun[]>("GET", `/api/v1/cases/${encodeURIComponent(id)}/validations`),
  });
}

// The oracle exists only after a passed validation; the server answers 404 before that.
export function useOracle(id: string, enabled: boolean) {
  return useQuery({
    queryKey: ["cases", "oracle", id],
    queryFn: () => api<OracleTests>("GET", `/api/v1/cases/${encodeURIComponent(id)}/oracle`),
    enabled,
  });
}

export function useCase(id: string) {
  return useQuery({
    queryKey: caseKey(id),
    queryFn: () => api<CaseView>("GET", `/api/v1/cases/${encodeURIComponent(id)}`),
  });
}

// Every write changes the queue, the catalog and the case, so each one refreshes all three.
function useCaseMutation<T, R = void>(call: (body: T) => Promise<R>) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: () => client.invalidateQueries({ queryKey: ["cases"] }),
  });
}

const path = (id: string, rest: string) => `/api/v1/cases/${encodeURIComponent(id)}/${rest}`;

export const useEditInstruction = (id: string) => useCaseMutation((text: string) => api<void>("PUT", path(id, "instruction"), { text }));
export const useApproveAssertions = (id: string) =>
  useCaseMutation((body: { assertions: Assertion[]; judge: JudgeQuestion[] }) => api<void>("POST", path(id, "assertions"), body));
export const useApprove = (id: string) => useCaseMutation<void>(() => api<void>("POST", path(id, "approval")));
export const useReject = (id: string) => useCaseMutation((reason: string) => api<void>("POST", path(id, "rejection"), { reason }));
export const useRetire = (id: string) => useCaseMutation<void>(() => api<void>("POST", path(id, "retirement"), {}));
export const useBulkApprove = () => useCaseMutation((ids: string[]) => api<BulkResult>("POST", "/api/v1/cases/approvals", { ids }));

// The source key names what the case came from: pr:<repo>#<n>, fix:<repo>#<n> or steering:<ref>.
export function sourceOf(source: string): { what: string; ref: string } {
  const at = source.indexOf(":");
  const prefix = at < 0 ? "" : source.slice(0, at);
  const ref = at < 0 ? source : source.slice(at + 1);
  switch (prefix) {
    case "pr":
      return { what: "Pull request", ref };
    case "fix":
      return { what: "Fix pull request", ref };
    case "revert":
      return { what: "Revert", ref };
    case "steering":
      return { what: "Correction", ref };
    default:
      return { what: "Source", ref: source };
  }
}

export const shortSha = (sha: string | null | undefined) => (sha ? sha.slice(0, 10) : "—");
