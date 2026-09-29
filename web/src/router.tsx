import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { Device } from "@/pages/Device";
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
  routeTree: rootRoute.addChildren([appRoute.addChildren([overviewRoute, themeRoute, workRoute, workItemRoute]), loginRoute, deviceRoute]),
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
