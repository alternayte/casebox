import { createRootRoute, createRoute, createRouter, Outlet } from "@tanstack/react-router";

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

export const router = createRouter({ routeTree: rootRoute.addChildren([overviewRoute]) });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
