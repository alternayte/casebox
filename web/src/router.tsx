import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";
import { Device } from "@/pages/Device";
import { Login } from "@/pages/Login";

const rootRoute = createRootRoute({
  component: () => (
    <div className="min-h-screen">
      <header className="border-b px-6 py-3 font-semibold">Casebox</header>
      <main className="px-6 py-6">
        <Outlet />
      </main>
    </div>
  ),
});

const overviewRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: () => <h1 className="text-xl font-semibold">Overview</h1>,
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

export const router = createRouter({ routeTree: rootRoute.addChildren([overviewRoute, loginRoute, deviceRoute]) });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
