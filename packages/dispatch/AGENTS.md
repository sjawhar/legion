# Dispatch Package

Dispatch is the React single-page application for coordinating native Dispatch
issues. The Go server lives in `packages/envoy/cmd/dispatch` and serves the
production build from `web/dist`.

## Layout

- `web/src/app.tsx` owns authentication, the React Router shell, and the
  responsive sidebar / main content / margin shell. The margin is a bottom
  sheet below the `md` breakpoint.
- `web/src/api/types.ts` mirrors the Dispatch JSON entities.
- `web/src/api/client.ts` is the typed same-origin HTTP client. It is the only
  browser API boundary.
- `web/src/api/sse.ts` opens the issue event stream and invalidates TanStack
  Query cache entries for the affected issue.
- `web/src/main.tsx` installs React Router and the shared Query client.

`AuthGate` resolves `GET /auth/whoami`; unauthenticated visitors see the
GitHub sign-in link at `/auth/start`. All application requests use the same
origin so the browser sends the signed-in cookie.

## Commands

Run these from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun test
```

## End-to-end tests

`bun run e2e` drives Playwright against the real Go Dispatch server and
Postgres. The harness runs `e2e/run-server.sh` unless
`PLAYWRIGHT_BASE_URL` selects an already deployed server. The harness defaults
to `DISPATCH_E2E_PORT=8777`, which keeps its temporary server separate from
the production listener on port 8766. It defaults `DATABASE_URL` to
`postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable` and
uses trusted `X-Dispatch-User` identity for `alice` and `bob`; do not replace it
with a fixture server.

`e2e/seed.ts` truncates the test database before each scenario. For a deployed
server, set `PLAYWRIGHT_DATABASE_URL` for the same database and
`E2E_AGENT_TOKEN` for bearer-seeded API calls.

## Phone acceptance

The `iphone` Playwright project uses Chromium with the iPhone 13 viewport,
touch input, and user agent. It verifies the responsive drawer, bottom-sheet
margin, and compact Inbox layout. It does not replace the manual phone check:
run the server on a tailnet-reachable address, open it from a phone, and answer
an open ask from the Inbox.
