---
title: "Only the implement App pushes a workflow file to the sandbox, the second phase's probe changes the surface rather than re-running the first, and a head move re-pins a foreign-repository run by its bytes"
category: testing
tags:
  - legion-smoke
  - github-actions
  - github-app-permissions
  - workflows-permission
  - negative-control
  - simplify-pass
date: 2026-10-16
status: active
module: .github/workflows
related_issues:
  - "LEGION-779"
  - "sjawhar/legion#1975"
  - "LEGION-748"
---

# Only the implement App pushes a workflow file to the sandbox, the second phase's probe changes the surface rather than re-running the first, and a head move re-pins a foreign-repository run by its bytes

Extends docs/solutions/testing/a-throwaway-sandbox-repository-probes-workflow-semantics-with-a-control-job-and-every-production-first-step-rides-the-same-run-and-a-reimplementation-is-differentialled-against-its-oracle-first-LEGION-748.md.

- **Rule 1's push needs the `workflows` permission, which only the implement App holds on
  sjawhar/legion-smoke.** The review App (planner, tester, reviewer, architect) can create the
  branch ref (`POST git/refs` → 201) but cannot write a file under `.github/workflows/` there: the
  contents API answers `403 Resource not accessible by integration` and `git push` is refused with
  ``refusing to allow a GitHub App to create or update workflow `.github/workflows/<file>` without
  `workflows` permission``. A tester or reviewer that needs a probe run stages its files on the
  issue volume, publishes their paths and sha256 to the implementer's role topic, and the
  implementer pushes them as one commit through the git data API (two blobs → one tree on the
  branch's tree → one commit → `PATCH git/refs/heads/<branch>`), replies with the commit sha, and
  the requester verifies each pushed blob's sha256 at that commit before reading the run. The
  verification is what keeps the run the requester's own proof. Plan the hand-over before the
  round reaches it: it cost this round a 403, a refused push and an Envoy exchange mid-proof.
- **The second phase's probe changes the surface; it does not re-run the first matrix.** The
  implementer's probe tests the shapes in the abstract (synthetic jobs, one echo each); the
  tester's carries the branch's exact condition text, the `uses:` reusable-workflow job the real
  chain has, the second workflow's job pair, and the failure arm the plan accepted as a
  tightening. Every residual finding of sjawhar/legion#1975 came from the arms the first run
  lacked: a `uses:` job's result and outputs under `always()`, `needs.x.result != 'failure'`
  running on a skipped job, and the accepted tightening observed.
- **A head move after an external-surface proof re-pins the run by bytes, not by a re-run.** The
  simplify pass ran after the proofs and moved the head (the checker only). The cheap proofs (the
  checker on the real workflows, its harness, both negative controls) were re-run at the new head;
  the sandbox run was not, since its validity rests on the probed conditions being the ones the
  head's workflows carry, which a byte comparison of the probe source against the head's `if:`
  lines establishes, and which the tester checked independently by decoding the probe workflow at
  its sandbox commit through the contents API. Before the simplify pass, list which proofs the
  files it may touch can invalidate; run it before those and after the ones it cannot touch.

## Evidence

- LEGION-779 test handoff, `notes[0]`: the review App's 403 and refused push on
  sjawhar/legion-smoke branch `legion-779-tester-probe`; the implementer's push, commit
  `8bb553d0526cebde03c97878ad3231cd739fe188` holding
  `.github/workflows/legion-779-tester-probe.yaml` (sha256 `d19e0ca1…b3ca`) and
  `legion-779-tester-reusable.yaml` (sha256 `6f54925c…c698`), both verified at the commit; the
  run it started, 38836147591. The implement App had pushed `legion-779-condition-probe` (run
  38833347317) in the implementing phase with no refusal.
- Implementer probe 38833347317: 13 jobs, synthetic shapes. Tester probe 38836147591: 18 jobs plus
  a reusable workflow, the branch's conditions verbatim; its `hazard_ne_failure`, `image`/`note`
  and `build_bad_changes` arms produced the three observations the first run could not.
- sjawhar/legion#1975 `Thermo` line: `ce-simplify-code` at `3dfd84dd` → head `bfacb40e`; A1/A2
  and both negative controls re-run there with identical output; the `E2E (implementer)` line
  states the probed conditions equal the head's; the tester's `implementerProof.how` decodes the
  probe workflow at sandbox commit `f8467f1d` and compares.
