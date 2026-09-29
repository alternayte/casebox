import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";

export type Role = "viewer" | "member" | "admin" | "owner";
export type Me = { accountId: string; displayName: string; role: Role; orgId: string };

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => api<Me>("GET", "/api/v1/me"), retry: false, staleTime: 60_000 });
}

export function isMember(me: Me | undefined): boolean {
  return me !== undefined && me.role !== "viewer";
}
