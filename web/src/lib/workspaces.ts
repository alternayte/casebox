import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";

export type Workspace = { name: string; repos: string[] };

export function useWorkspaces() {
  return useQuery({ queryKey: ["workspaces"], queryFn: () => api<Workspace[]>("GET", "/api/v1/workspaces") });
}
