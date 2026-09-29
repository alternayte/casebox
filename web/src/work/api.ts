import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, qs } from "@/lib/api";

export type WorkItemSummary = {
  id: string;
  provider: string;
  key: string;
  repo: string | null;
  title: string;
  type: string | null;
  status: string | null;
  resolved: boolean;
  sessions: number;
  pullRequests: number;
  merged: number;
  updatedAt: string;
};

export type TimelineEntry = { at: string; kind: string; detail: Record<string, unknown> };

export function useWorkItems(query: string | undefined, limit = 100) {
  return useQuery({
    queryKey: ["work", "list", query ?? "", limit],
    queryFn: () => api<WorkItemSummary[]>("GET", `/api/v1/work-items${qs({ query, limit })}`),
    placeholderData: keepPreviousData,
  });
}

export function useTimeline(id: string) {
  return useQuery({
    queryKey: ["work", "timeline", id],
    queryFn: () => api<TimelineEntry[]>("GET", `/api/v1/work-items/timeline${qs({ id })}`),
  });
}

export function useMoveSession() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ sessionId, workItem }: { sessionId: string; workItem: string }) =>
      api<void>("PUT", `/api/v1/sessions/${encodeURIComponent(sessionId)}/work-item`, { workItem }),
    onSuccess: () => Promise.all([client.invalidateQueries({ queryKey: ["work"] }), client.invalidateQueries({ queryKey: ["steering"] })]),
  });
}

// A work item's key is its stream ID without the provider prefix.
export function keyOf(id: string): string {
  return id.replace(/^wi:(jira|github):/, "");
}
