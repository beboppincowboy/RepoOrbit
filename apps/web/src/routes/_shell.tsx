import { Outlet, createFileRoute, redirect } from "@tanstack/react-router";
import { AppShell } from "@/features/repositories/app-shell";
import { sessionQuery } from "@/features/auth/api";

// Workspace shell: every signed-in screen renders inside the sidebar layout. Signed-out real-mode visitors go to /login.
export const Route = createFileRoute("/_shell")({
  beforeLoad: async ({ context }) => {
    const session = await context.queryClient.fetchQuery(sessionQuery);
    if (session.mode === "real" && !session.user) throw redirect({ to: "/login" });
    return { session };
  },
  errorComponent: () => <p role="alert" className="p-6 text-destructive">Could not reach the RepoOrbit API. Check that the local services are running.</p>,
  component: ShellLayout,
});

function ShellLayout() {
  const { session } = Route.useRouteContext();
  return <AppShell session={session}><Outlet /></AppShell>;
}
