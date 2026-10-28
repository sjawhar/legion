---
title: "A job if: on a workflow_call input holds on every run that leaves the input out only as == '' on a string input with an empty default"
category: github
tags:
  - github-actions
  - workflow_call
  - inputs
  - job-conditions
  - expression-coercion
  - check-image-trigger-paths
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "INFRA-576"
  - "sjawhar/legion#2358"
---

# A job if: on a workflow_call input holds on every run that leaves the input out only as == '' on a string input with an empty default

## The rule

1. Before you gate a job on `inputs.<name>`, write down the input's value on each kind of run:
   - a `workflow_call` that sets it: the caller's value;
   - a `workflow_call` that leaves it out: its declared `default`, typed (a string, `false`, a
     number), not `''`;
   - the workflow's own `push` or `pull_request` run: null. Against a literal GitHub coerces both
     sides to numbers, so null equals `''` and also equals `'0'`;
   - a `workflow_dispatch` run: the `workflow_dispatch` trigger's own default for that name, when
     it declares one.
2. A gate that must hold whenever the input is left out (a mode that skips work only when a caller
   asks) is `inputs.<name> == ''` on a `type: string` input with no `default` or `default: ""`,
   and the same name not declared with a non-empty default under `workflow_dispatch`. Every other
   shape is false on some run that leaves the input out: `== ''` on a non-empty default or on a
   boolean or number input, `!=` the declared default, `!= '0'`, `== '<non-empty>'`, `!= ''`.
3. A check that admits such a gate reads the input's `type` and `default`, not only its name, and
   models no coercion it has not measured: admit the one known-safe shape and refuse the rest.

## Evidence

`.github/scripts/check-image-trigger-paths.sh` (lines 29-46, `call_inputs` at 155-168) admits
`worker-image.yaml`'s build-job gate `inputs.attest_digest == ''` (`worker-image.yaml:159`, the
input declared `type: string`, `default: ""` at lines 33-37). Two review rounds on
sjawhar/legion#2358 widened what it refused:

- Round 1 (fixed in 4507cb60): `inputs.mode == 'attest'` and `inputs.mode != ''` were admitted,
  though both are false on every `pull_request` run.
- Round 2 (fixed in 8f1d9f49): the check still modelled a left-out input as `''`. With the test
  file's `fixture` and `call_modes` helpers, `inputs.mode == ''` with `default: "build"`,
  `inputs.mode != 'attest'` with `default: "attest"`, and `inputs.mode != '0'` each exited 0, so a
  gate that turned the build off on every call or every pull request passed the required lint job.
  `check-image-trigger-paths.test.sh` now refuses each, and a boolean input.

The `workflow_dispatch` case in rule 1 is GitHub's documented behaviour and was not probed here;
the check does not yet read `workflow_dispatch` inputs.
