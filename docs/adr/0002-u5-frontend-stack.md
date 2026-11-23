# Use TanStack Start for the U5 frontend

U5 migrates the React frontend from Next.js to TanStack Start for typed routes and shareable repository filters. We chose Start over a Vite app with TanStack Router for its option to enable server rendering by route if a later requirement warrants it. We chose shadcn/ui with Tailwind CSS to reuse existing styles and adopt shadcn components directly. Panda CSS would require restyling the existing screens and adapting or replacing those components.

Start serves the app shell. The browser restores filters from the URL and uses TanStack Query for analytics reads. The deployment forwards `/auth`, `/api`, and `/graphql` to Go on the same origin. Repository deep links can show a loading state while analytics load. Go owns GitHub OAuth, sessions, CSRF, repository authorization, and metric calculations. TanStack Table renders tabular views, and ECharts plots the metrics supplied by Go.

Before building the U5 analytics workspace, port the existing screens and verify demo mode, GitHub sign-in and logout, repository selection, activity refresh, direct links, and private repository access through production routing.
