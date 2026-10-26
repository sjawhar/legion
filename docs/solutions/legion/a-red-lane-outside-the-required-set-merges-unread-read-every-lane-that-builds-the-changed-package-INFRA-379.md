---
title: "A red lane outside the required set merges unread: read every lane that builds the changed package"
category: legion
tags:
  - required-checks
  - ci
  - merge-gate
  - semantic-conflict
  - envoy-go
date: 2026-10-25
status: active
module: legion-worker
related_issues:
  - "INFRA-379"
  - "sjawhar/legion#2194"
  - "sjawhar/legion#2162"
---

# A red lane outside the required set merges unread: read every lane that builds the changed package

- READY accepts a head whose required checks are still running, and the daemon's merge goes in
  the moment the required set turns green. A lane outside that set is read by nothing in that
  path: red or still running, it is merged over.
- So the required set is not "CI". On main it is `lint`, `typecheck`, `test`, `pr-title`
  (`gh api repos/sjawhar/legion/rules/branches/main`). `test` does not compile
  `packages/envoy`; `envoy-go` and `broker-multiprocess` do, and neither is required.
- Reviewer, before approving: list every check run at the code head
  (`gh api "repos/<o>/<r>/commits/<sha>/check-runs?per_page=100" --jq '.check_runs[]|"\(.name) \(.status) \(.conclusion)"'`),
  not only the required names. A red lane that builds a path the diff changes is a finding. A
  lane still running there is not yet a pass: say so in the review, never approve over it as green.
- Merger: read the same list at the READY head and quote any lane that is not success.
- A change to a shared helper (a signature, a removed function) breaks callers that other open
  pull requests add. That break shows only on the merge ref, and only in the lane that compiles
  that package. That lane's result is the one that counts, required or not.
- Whether to make those lanes required, or have READY read them, is a gate decision for the
  architect and controller. This page is how a role reads CI until that decision lands.

## Evidence

sjawhar/legion#2162 changed `startBroker` in `packages/envoy/cmd/broker/main_test.go` to
`startBroker(t, brokerOptions) *brokerProcess`. #2185, already on main, had added two callers in
the old positional form. Neither pull request changed the other's lines, so GitHub showed no
conflict. The CI of #2162 saw the break all the same:

| Check at #2162's head `ee5775bd7ae8` | Required | Conclusion | Completed |
|---|---|---|---|
| `lint` | yes | success | 11:46:39Z |
| `typecheck` | yes | success | 11:48:14Z |
| `envoy-go` (job 121158836716, step "Unit test envoy Go package") | no | failure | 11:57:07Z |
| `test` | yes | success | 11:57:26Z |
| `broker-multiprocess` | no | failure | 12:00:29Z |

`app/legion-implementer` merged it at 11:57:51Z, 25 s after the last required check went green
and 44 s after `envoy-go` went red. `broker-multiprocess` was still running at the merge. Main
then failed `go vet` in `packages/envoy` until #2194.

The INFRA-379 spec at first said the conflict was "invisible to every pre-merge check". That was
wrong: a pre-merge check saw it, and the gate did not read that check.

Related: `docs/solutions/legion/mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`
(the same shape from the branch's side) and
`docs/solutions/github/a-pull-request-github-calls-mergeable-can-still-be-unmergeable-in-substance-INFRA-66.md`
(CI judges the merge ref).
