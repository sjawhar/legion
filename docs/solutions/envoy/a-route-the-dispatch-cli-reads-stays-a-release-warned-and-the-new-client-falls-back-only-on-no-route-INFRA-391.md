---
title: "A route the dispatch CLI reads stays a release, warned, and the new client falls back only on no-route"
category: envoy
tags:
  - dispatch
  - version-skew
  - contract-change-census
  - route-removal
  - envoy-client
date: 2026-10-26
status: active
module: packages/envoy-client
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# A route the dispatch CLI reads stays a release, warned, and the new client falls back only on no-route

The `dispatch` CLI (`packages/envoy-client`, bundled into pi-envoy and claude-envoy) publishes to
npm when a pull request merges. A session loads it only when it restarts, and Dispatch deploys on
its own schedule. So every rollout runs a new CLI against an old Dispatch, and an old CLI against
a new one.

- **Find the out-of-process callers in the base revision before you plan a route change.** Search
  the shipped clients at the fork point, for example
  `git grep -n -P '<route>' <fork-point> -- packages/envoy-client/src packages/pi-envoy packages/claude-envoy/src packages/contracts/src skills`.
  Every hit is a caller that keeps running after your merge. Its row in the
  `## Contract change census` sets the rollout.
- **Keep a removed or renamed route for one release when an earlier CLI calls it.** It answers
  its old contract, and each call logs one warn naming the route and the project. The new route
  replaces it in the next release.
- **The new client falls back only on the older server's no-route answer.** That answer is
  `404 NOT_FOUND` whose message starts `no route for `. A `404 NOT_FOUND` that says `not found`
  (an unknown project) is not a no-route answer. If the client falls back on it too, the error
  becomes "no model".
- **"Stale clients fail visibly" is a claim. Run the old CLI before you accept it.** The CLI's
  issue-create guidance (`architectureGuidance`) turns any read error into advice text, so a
  missing route makes every create print "Could not check whether project … has an architecture
  model". That is false advice, not a failure. Quote what the old CLI actually prints in the plan.
- **Prove it as a matrix of real processes.** Run each CLI ({head, base, pre-fix}) against each
  server ({head, base}). The negative control is the pre-fix CLI against the base server.
- **A path one side cannot address has no fallback.** An older server has no per-repository
  route, so `--repo` needs a server of the new release. Say so in the CLI's CHANGELOG.

## Evidence

- The plan approved removing the singular routes because "stale clients fail visibly". Review
  round 1 of sjawhar/legion#2209 reproduced the false advice both ways (threads
  PRRT_kwDORFy7ds6wE5Qu and PRRT_kwDORFy7ds6wE5Qz). This was the defect #1522 had fixed.
- Census §1's base-revision search (`git grep -n -P 'architecture-source(?![s-])\b' 83f6320140ec
  -- packages/envoy-client/src …`) finds exactly `dispatch-http.ts:509,518`: the two calls the
  skew broke. Written at the first push, that census would have shown the problem before review.
- The fixes were 5b0ef7b1ae, a8ccde7ce2 and 80d9243296. `isNoRoute` is in
  `packages/envoy-client/src/dispatch-http.ts`. The routes kept for older clients are in
  `routes_table.go`, with warns in `architecture_source.go`.
- Proof: https://github.com/sjawhar/legion/pull/2209#issuecomment-6405051940. No fixed pairing
  gave false advice. The pre-fix CLI against the base server printed
  `Could not check whether project NOARCH has an architecture model: no route for GET …/architecture-sources`.
