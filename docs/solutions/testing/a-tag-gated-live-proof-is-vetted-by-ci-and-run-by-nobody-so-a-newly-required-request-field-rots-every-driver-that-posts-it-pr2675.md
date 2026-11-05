---
title: "A tag-gated live proof is vetted by CI and run by nobody, so a newly required request field rots every driver that posts it"
category: testing
tags:
  - live-proofs
  - e2e
  - false-green
  - api-contract
  - census
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2675"
  - "sjawhar/legion#2354"
---

# A tag-gated live proof is vetted by CI and run by nobody, so a newly required request field rots every driver that posts it

CI compiles the `-tags e2e` live proofs and runs none of them, and a Go struct literal with named
fields compiles with a new field unset. So a change that makes a request field required leaves
every live-proof driver that posts that request compiling, vetting green, and dead at its first
step whenever somebody next runs it.

- **A change that makes a request field required finds its callers by the request type, never by
  the feature's vocabulary.** Grep the type's constructor across the whole repository, the
  `-tags e2e` drivers included, and update each in that change. The drivers never name the feature
  — they name the route — so a vocabulary grep over the mechanism's own words passes them by.
- **Read the drivers as a class, not the one you were handed.** Sibling drivers in one directory
  are written by different hands at different times; the one that still works is not evidence
  about the others.
- **Which proofs are exposed is decided by the routes a proof drives, not by "live proof" as a
  class.** A proof that never calls the route a change touches needs nothing. Say which you
  checked and found clean, with the grep, so a later reader can tell "checked" from "not looked
  at".
- **Run a named proof surface before you build on it.** When an issue's acceptance names a live
  proof as its evidence, run it first: the first run tells you whether you inherited somebody
  else's breakage, and that repair is then your change's scope rather than a new issue.
- **A repair of a rotted proof must keep the refusal the proof exists to assert.** A driver that
  now fails for a new reason can be made green by a change that also hides the old assertion, so
  check that each refusal the proof names is still the refusal it observes.

## Evidence (sjawhar/legion#2675, INFRA-609)

sjawhar/legion#2354 (merge `b552205c`) moved Legion's phase handoffs onto their Dispatch issues and
made a completion of a file-backed phase name a Dispatch handoff record:
`packages/daemon/internal/api/handoff.go:275-278` refuses `400 HANDOFF_RECORD_REQUIRED` when
`HandoffID` is empty or the version is not positive.

That same commit updated one live-proof driver in the directory and missed another. `git log -S`
on the field puts `merged_root_waits_live_test.go:478`'s `HandoffID: record` in `b552205c` itself,
and `handoff_records_live_test.go` was written for the new shape; `ci_red_no_takeover_live_test.go`
kept posting `api.HandoffCompleteRequest{GrantID, Summary, Commit, Verdict}` and naming no record.
Nothing reported it. CI's only contact with these files is
`.github/workflows/ci.yaml:1540-1541`, `go vet -tags e2e ./...`, which compiles them;
`scripts/e2e/lib/live-daemon-proof.sh:122-123` builds the driver with `go test -c -tags e2e` and
the script is run by hand with `LEGION_TEST_PG_DSN`.

The rot surfaced eight days later, when INFRA-609's plan named that script as the surface of its
own acceptance criterion. Its first run died at step 1 — `ci_red_no_takeover_live_test.go:418: the
implementer's completion = 400 HANDOFF_RECORD_REQUIRED … want 200` — before any of the five steps
the proof is about. The same run at the parent commit failed identically, which is how it was told
apart from the change under test. Repairing it cost INFRA-609 a second implementing round.

`scripts/e2e/no-required-checks.sh`, in the same directory and run in the same session, was
unaffected: it drives CI settlements, pushes and base-branch reads and completes no phase, so it
never posts that request. The discriminator was the route, not the directory.

A vocabulary grep would not have found the gap. #2354's own census searched `.legion/`,
`skip-checks`, `handoff-only`, `previousHandoff` and the handoff file names; the broken driver
names none of them. `grep -rn 'HandoffCompleteRequest{' packages/daemon` returns the three files
that matter.
