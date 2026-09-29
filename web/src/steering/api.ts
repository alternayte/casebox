import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, qs } from "@/lib/api";
import { isoDay } from "@/lib/format";
import type { Filters, InterventionPage, Relabel, Report, Status } from "./types";

// The URL keeps whole days. The API takes instants: from the start of the first day to the
// start of the day after the last one.
function nextDay(day: string): string {
  const d = new Date(`${day}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() + 1);
  return `${isoDay(d)}T00:00:00Z`;
}

export function apiFilters(f: Filters) {
  return {
    from: f.from ? `${f.from}T00:00:00Z` : undefined,
    to: f.to ? nextDay(f.to) : undefined,
    workspace: f.workspace,
    repo: f.repo,
    agent: f.agent,
    model: f.model,
    harness: f.harness,
    taskType: f.taskType,
  };
}

// The last day a period covers: its end is exclusive.
export function lastDay(to: string): string {
  return isoDay(new Date(Date.parse(to) - 1));
}

export function useReport(f: Filters) {
  return useQuery({
    queryKey: ["steering", "report", f],
    queryFn: () => api<Report>("GET", `/api/v1/steering/report${qs({ ...apiFilters(f), groupBy: f.groupBy })}`),
    placeholderData: keepPreviousData,
  });
}

export function useInterventions(wentWrong: string, f: Filters, page: number) {
  return useQuery({
    queryKey: ["steering", "interventions", wentWrong, f, page],
    queryFn: () =>
      api<InterventionPage>("GET", `/api/v1/steering/interventions${qs({ wentWrong, ...apiFilters(f), page })}`),
    placeholderData: keepPreviousData,
  });
}

export function useRelabel() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: Relabel) => api<void>("POST", "/api/v1/steering/relabel", body),
    onSuccess: () => Promise.all([client.invalidateQueries({ queryKey: ["steering"] }), client.invalidateQueries({ queryKey: ["work"] })]),
  });
}

export function useRefresh() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (repo?: string) => api<Status>("POST", "/api/v1/steering/refresh", repo ? { repo } : {}),
    onSuccess: () => Promise.all([client.invalidateQueries({ queryKey: ["steering"] }), client.invalidateQueries({ queryKey: ["work"] })]),
  });
}
