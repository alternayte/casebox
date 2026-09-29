import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { CaseList, type CasesSearch } from "@/cases/CaseList";
import { CasePage } from "@/cases/CasePage";
import { caseKinds, caseSplits, caseStatuses } from "@/lib/labels";
import { EvaluationList, type EvaluationsSearch } from "@/evaluations/EvaluationList";
import { EvaluationPage } from "@/evaluations/EvaluationPage";
import { RunPage } from "@/evaluations/RunPage";
import { Device } from "@/pages/Device";
import { PatternList, PatternPage, type PatternsSearch } from "@/proposer/Patterns";
import { ProposalList, ProposalPage, type ProposalsSearch } from "@/proposer/Proposals";
import { Login } from "@/pages/Login";
import { AppShell } from "@/shell/AppShell";
import { validateFilters } from "@/steering/filters";
import { Overview } from "@/steering/Overview";
import { ThemePage } from "@/steering/ThemePage";
import type { Filters } from "@/steering/types";
import { WorkItemPage } from "@/work/WorkItemPage";
import { WorkList } from "@/work/WorkList";

const rootRoute = createRootRoute({ component: () => <Outlet /> });

// Pages that need a signed-in account. Login and device approval stay outside.
const appRoute = createRoute({ getParentRoute: () => rootRoute, id: "app", component: AppShell });

const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  validateSearch: validateFilters,
  component: function OverviewRoute() {
    const navigate = overviewRoute.useNavigate();
    return <Overview filters={overviewRoute.useSearch()} onFilters={(f: Filters) => navigate({ search: f })} />;
  },
});

const themeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/steering/themes/$wentWrong",
  validateSearch: (search: Record<string, unknown>): Filters & { page?: number } => {
    const { groupBy: _groupBy, ...filters } = validateFilters(search);
    const page = Number(search.page);
    return { ...filters, page: Number.isInteger(page) && page > 1 ? page : undefined };
  },
  component: function ThemeRoute() {
    const { wentWrong } = themeRoute.useParams();
    const { page, ...filters } = themeRoute.useSearch();
    const navigate = themeRoute.useNavigate();
    return (
      <ThemePage
        key={wentWrong}
        wentWrong={wentWrong}
        filters={filters}
        page={page ?? 1}
        onPage={(p) => navigate({ search: (prev) => ({ ...prev, page: p > 1 ? p : undefined }) })}
      />
    );
  },
});

const workRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/work",
  validateSearch: (search: Record<string, unknown>): { q?: string } => ({
    q: typeof search.q === "string" && search.q !== "" ? search.q : undefined,
  }),
  component: function WorkRoute() {
    const { q } = workRoute.useSearch();
    const navigate = workRoute.useNavigate();
    return <WorkList key={q ?? ""} query={q} onQuery={(next) => navigate({ search: { q: next } })} />;
  },
});

// Work item IDs hold '/' and '#', so the ID is a search parameter, as in the API.
const workItemRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/work/item",
  validateSearch: (search: Record<string, unknown>): { id: string } => ({ id: typeof search.id === "string" ? search.id : "" }),
  component: function WorkItemRoute() {
    const { id } = workItemRoute.useSearch();
    return id ? <WorkItemPage key={id} id={id} /> : <p className="text-sm">No work item is named. Choose one from Work.</p>;
  },
});

// Reads the Cases view and filters from the URL; unknown values are dropped.
function validateCases(search: Record<string, unknown>): CasesSearch {
  const page = Number(search.page);
  const pick = (value: unknown, allowed: readonly string[]) => (typeof value === "string" && allowed.includes(value) ? value : undefined);
  return {
    view: search.view === "catalog" ? "catalog" : undefined,
    status: pick(search.status, caseStatuses),
    kind: pick(search.kind, caseKinds),
    split: pick(search.split, caseSplits),
    workspace: typeof search.workspace === "string" && search.workspace !== "" ? search.workspace : undefined,
    page: search.view === "catalog" && Number.isInteger(page) && page > 1 ? page : undefined,
  };
}

const casesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/cases",
  validateSearch: validateCases,
  component: function CasesRoute() {
    const navigate = casesRoute.useNavigate();
    return <CaseList search={casesRoute.useSearch()} onSearch={(search) => navigate({ search })} />;
  },
});

const caseRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/cases/$id",
  component: function CaseRoute() {
    const { id } = caseRoute.useParams();
    return <CasePage key={id} id={id} />;
  },
});

const evaluationsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/evaluations",
  validateSearch: (search: Record<string, unknown>): EvaluationsSearch => ({
    workspace: typeof search.workspace === "string" && search.workspace !== "" ? search.workspace : undefined,
  }),
  component: function EvaluationsRoute() {
    const navigate = evaluationsRoute.useNavigate();
    return <EvaluationList search={evaluationsRoute.useSearch()} onSearch={(search) => navigate({ search })} />;
  },
});

const evaluationRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/evaluations/$id",
  component: function EvaluationRoute() {
    const { id } = evaluationRoute.useParams();
    return <EvaluationPage key={id} id={id} />;
  },
});

const runRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/evaluations/$id/runs/$runId",
  component: function RunRoute() {
    const { id, runId } = runRoute.useParams();
    return <RunPage key={runId} id={id} runId={runId} />;
  },
});

const workspaceSearch = (search: Record<string, unknown>): PatternsSearch & ProposalsSearch => ({
  workspace: typeof search.workspace === "string" && search.workspace !== "" ? search.workspace : undefined,
});

const patternsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/patterns",
  validateSearch: workspaceSearch,
  component: function PatternsRoute() {
    const navigate = patternsRoute.useNavigate();
    return <PatternList search={patternsRoute.useSearch()} onSearch={(search) => navigate({ search })} />;
  },
});

const patternRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/patterns/$id",
  component: function PatternRoute() {
    const { id } = patternRoute.useParams();
    return <PatternPage key={id} id={id} />;
  },
});

const proposalsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/proposals",
  validateSearch: workspaceSearch,
  component: function ProposalsRoute() {
    const navigate = proposalsRoute.useNavigate();
    return <ProposalList search={proposalsRoute.useSearch()} onSearch={(search) => navigate({ search })} />;
  },
});

const proposalRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/proposals/$id",
  component: function ProposalRoute() {
    const { id } = proposalRoute.useParams();
    return <ProposalPage key={id} id={id} />;
  },
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  validateSearch: (search: Record<string, unknown>): { returnUrl?: string } => ({
    returnUrl: typeof search.returnUrl === "string" ? search.returnUrl : undefined,
  }),
  component: function LoginRoute() {
    return <Login returnUrl={loginRoute.useSearch().returnUrl} />;
  },
});

const deviceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/device",
  validateSearch: (search: Record<string, unknown>): { code?: string } => ({
    code: typeof search.code === "string" ? search.code : undefined,
  }),
  component: function DeviceRoute() {
    return <Device code={deviceRoute.useSearch().code} />;
  },
});

export const router = createRouter({
  routeTree: rootRoute.addChildren([appRoute.addChildren([overviewRoute, themeRoute, workRoute, workItemRoute, patternsRoute, patternRoute, proposalsRoute, proposalRoute, casesRoute, caseRoute, evaluationsRoute, evaluationRoute, runRoute]), loginRoute, deviceRoute]),
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
