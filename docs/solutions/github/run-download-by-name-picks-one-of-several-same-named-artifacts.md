---
title: "gh run download -n picks one of several same-named artifacts: download by artifact id, and name upload artifacts by run_attempt"
category: github
tags:
  - github-actions
  - artifacts
  - upload-artifact
  - run-attempt
  - gh
  - playwright
  - traces
date: 2026-09-14
status: active
module: .github/workflows/envoy-and-contracts.yaml
related_issues:
  - "LEGION-94"
  - "sjawhar/legion#1084"
symptoms:
  - "gh run download <run> -n <name> returns one artifact although the run's attempts uploaded several under that name"
  - "an earlier attempt's Playwright traces cannot be found after a rerun --failed"
  - "no valid artifacts found to download"
---

# `gh run download -n` Picks One of Several Same-Named Artifacts

## What happened

Run 34763436556 of `Legion Envoy and Contracts` failed its browser-test step three times in a
row, each time re-run with `legion gh -- run rerun <run> --failed`, and went green on the fourth
attempt. Each failed attempt's `actions/upload-artifact@v4` step ran and reported `success`, so
the run held three artifacts named `dispatch-e2e-test-results`: ids 10319349509 (attempt 1),
10320340092 (attempt 2), 10320705108 (attempt 3), each about 15 MB.

`legion gh -- run download 34763436556 -n dispatch-e2e-test-results` returned exactly one of them
— the latest — and said nothing about the other two. LEGION-94's spec first read that as
"re-run uploads are refused"; they were not. The uploads all succeeded; the download by name
cannot tell them apart.

## The rule

Two halves.

**Reading:** when a run has been re-run, list its artifacts before downloading and fetch by id,
never by name:

```sh
legion gh -- api repos/<owner>/<repo>/actions/runs/<run>/artifacts \
  --jq '.artifacts[] | {id, name, created_at, size_in_bytes}'
legion gh -- api repos/<owner>/<repo>/actions/artifacts/<id>/zip > attempt-N.zip
```

`created_at` orders them by attempt. The `/zip` endpoint answers with a redirect that `gh api`
follows; the body is the zip.

**Writing:** an upload step that can run more than once per run — anything under `if: failure()`
in a job that gets re-run — puts the attempt number in the artifact name:

```yaml
- uses: actions/upload-artifact@v4
  if: failure()
  with:
    name: dispatch-e2e-test-results-${{ github.run_attempt }}
    path: packages/dispatch/e2e/test-results
```

With that name in place `legion gh -- run download <run> -n dispatch-e2e-test-results-2 -D <dir>`
fetches exactly attempt 2's traces, and on a green attempt the download refuses —
`no valid artifacts found to download`, exit 1 — instead of silently handing back another
attempt's files (LEGION-94's tester used exactly that refusal as the negative control).

## Why not overwrite

What was observed is the whole mechanism this rule rests on: three attempts of one run each ran
`upload-artifact@v4` with the same `name`, each step reported `success`, and the run's artifact
list held all three side by side. `overwrite: true` would have kept only the last, which loses
the earlier attempts — the evidence a flake investigation needs most, since the first failure is
the one nobody has re-run past yet. The attempt number keeps every attempt and makes each one
addressable by name.
