---
title: "An authority per route is a table row, and a table the check cannot decide refuses to mount"
category: best-practices
tags:
  - authorization
  - route-table
  - forward-merge
  - fail-closed
date: 2026-10-31
status: active
module: packages/envoy/internal
related_issues:
  - "sjawhar/legion#2541"
---

# An authority per route is a table row, and a table the check cannot decide refuses to mount

When a new check must cover every route, the thing that keeps it covering them is not the check:
it is a table the routes are already declared in, and a refusal at mount.

- **Declare the authority on the route's own row**, beside the row that mounts it, so adding a
  route means answering the question. Dispatch classes every mutating route in `writeClasses`
  (`packages/envoy/internal/dispatch/api/routes_table.go`) as a project's, a project the body
  names, the calling session's, or the deployment's, with
  `TestEveryMutatingRouteIsClassed` holding the two tables to each other. The broker does the same
  for caller authority: `accountPowers`
  (`packages/envoy/internal/broker/api/routes_table.go`) gives each listed caller name one bit per
  power, each route declares the power it needs, and `noPower` is held by nobody, so a route that
  declares none is reachable by no one.
- **Refuse to serve a table the check cannot decide, at mount.** `api.Register` panics naming
  every mutating row the project check has no resolver for
  (`internal/dispatch/api/server.go`), and the broker refuses its boot on a caller name
  `accountPowers` does not know. A route a merge adds then surfaces as a boot refusal in the
  branch's own tests rather than as a write nothing judged.
- **Do not let the mount-time refusal be the only guard.** It protects the deployment, not the
  reader: keep the request path fail-closed too, so an undecidable row refuses a scoped caller
  rather than depending on a panic somewhere else (`project_scope.go`'s resolver-less branch
  says exactly that in its comment).
- **This is what makes a forward merge safe for an authorization change.** A branch that adds a
  per-route check is racing every branch that adds a route; the table and its correspondence test
  are what turn that race into a loud failure in your own suite.

## Evidence

sjawhar/legion#2541 (INFRA-264). The forward merge `74797a9e` brought in `main`'s new
`POST /api/v1/projects/{key}/related`; the correspondence test and the mount refusal named it at
once, and classing it was the whole of the merge's code work. Review round 2 then asked for the
request-path guard beside the panic, which is the resolver-less branch the file now carries. The
broker's power table was round 1's answer to a listed caller — `listener`, listed only to read
tokens back — reaching the approval and revocation routes by being listed at all.
