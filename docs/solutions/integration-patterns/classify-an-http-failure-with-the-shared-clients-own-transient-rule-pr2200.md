---
title: "Classify an HTTP failure with the shared client's own transient rule"
category: integration-patterns
tags:
  - http
  - error-categories
  - retry
  - envoy-client
date: 2026-10-28
status: active
module: packages/envoy-client
---

# Classify an HTTP failure with the shared client's own transient rule

- When code sorts an HTTP failure into categories, ask the shared client's own retry rule first
  (`transientStatus` in `packages/envoy-client/src/dispatch-http.ts`: a 5xx, 408 or 429). Do not
  write a second status table next to it. Import the function, exporting it if needed, so the two
  rules cannot drift apart.
- A category that tells the reader to change the request (`refused`, `invalid`) never covers a
  status the client would retry. Check the transient rule before any 4xx branch.
- `DispatchGatewayError` extends `DispatchServiceError`, so a branch on `DispatchServiceError`
  and its status also catches a proxy's timeout and rate limit. Test a gateway 408 and 429, not
  only Dispatch's own refusals.

## Evidence

- At 186e6b46, `errorCategory` in `scripts/dispatch/meaning-search-check.ts` mapped every 4xx
  other than 401, 403 and 404 to `refused`. That moved a gateway's 408 and 429 from `transport`
  to `refused`. The README describes `refused` as a request to change, such as a query that is
  too long, so the operator would change the query when running it again would have answered.
- 8ab64781 exports `transientStatus` and checks it first (`errorCategory`, line 269 at
  7e042950). With `DISPATCH_URL` pointed at a local server that answered 429, then 408, then
  400, the script printed `transport`, `transport`, then `refused`.
