---
title: "A refusal keyed on a new field is proven by swapping only the side whose rule changed"
category: testing
tags:
  - negative-control
  - smoke
  - multi-process
  - protocol-change
  - pi-envoy
date: 2026-10-31
status: active
module: packages/pi-envoy/scripts
related_issues:
  - "sjawhar/legion#2592"
---

# A refusal keyed on a new field is proven by swapping only the side whose rule changed

A change that makes a server refuse what it used to allow is proven by a surface that fails
against the base and passes on the branch; that much is already written
(`docs/solutions/testing/a-real-binary-refusal-proof-seeds-past-every-earlier-branch-and-the-proof-is-the-pair-INFRA-797.md`,
`docs/solutions/github/prove-a-path-filter-change-with-the-pinned-actions-own-code-and-a-skip-and-run-pair-of-scratch-pull-requests-based-on-the-branch-INFRA-583.md`, and the `git archive`
mechanics in
`docs/solutions/testing/prove-a-go-acceptance-test-fails-on-base-by-running-the-unchanged-file-in-a-git-archive-export-INFRA-487.md`).
What that advice does not say is which side to swap, and for a rule keyed on a field the clients
only now send, swapping the wrong one proves nothing.

- **Swap only the component whose rule changed; keep every other component at the branch.** The
  control builds the listener from the base and leaves every client as the branch's
  (`packages/pi-envoy/scripts/smoke-role-takeover.sh:40-44`, `--base-rev`). Swapping both sides
  would have passed on the base as well, because a client that sends no instance id falls into the
  compatibility case the rule itself keeps
  (`packages/envoy/internal/session/instance.go:16`). A control that proves nothing is worse than
  none, because it is quoted.
- **Name which rows must still pass.** A control that fails everything is a broken rig. This one
  reports per row — `3 of 10 rows did not hold` against the base, with the four rows the change
  does not touch green on both sides — so the failure is attributable to the rule rather than to
  the swap.
- **An identity minted per process can only be proven across processes.** One child process per
  client instance, each loading the real extension, and the real channel-server process for the
  other client; `env -i` on each, so the operator's own environment cannot reach them
  (`smoke-role-takeover.sh:266`, `:274`). An in-process double shares whatever the module mints
  and cannot show two instances at all.
- **Assert inside the row that the two identities differ.** Otherwise a run where both sides
  happen to share an id satisfies the granting case and the row passes for the wrong reason: each
  row that turns on two instances reads both ids off the wire and requires them different.
- **Give each client its own recording proxy, and hold a request at it to order the race.** What
  each client sent and what the listener answered is read from a log rather than inferred, and the
  race the rule exists for is ordered by holding one client's claim at its proxy until the other
  has registered (`smoke-role-takeover.sh:18-22`, `POST /__smoke/hold?path=/v1/roles/set`). Force
  the losing order; never start two clients and hope one wins.
- **A proof that needs no credential and no deployment becomes a regression lock.** This one
  builds its own server and nats-server with `go build` and drives bun children, so it runs as a
  step of an existing CI job (`.github/workflows/ci.yaml:3066`), while the sibling smokes in the
  same directory need a model provider and stay manual. That criterion — what the proof needs,
  not whether it is called a smoke — is what decides whether it guards the rule from now on.

## Evidence

sjawhar/legion#2592 (dispatch://INFRA-404) made the Envoy listener refuse a soft role claim that
names a live predecessor belonging to another client instance, where it had granted it. The
acceptance criterion asked for a listener built from the branch, on a real NATS server, with real
client processes, showing each row of the issue's table and failing two named scenarios against
`main`.

The run on the branch: `SMOKE ROLE TAKEOVER PASS: all 10 rows held`. The same script with
`--base-rev main@origin`: `SMOKE ROLE TAKEOVER FAIL: 3 of 10 rows did not hold`, the fork claim
answering 200 where the branch answers 409, and the handoff's deregistration deleting the other
process's live entry with no refusal body at all.
