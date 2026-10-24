---
title: "GitHub runner's preinstalled Chrome apt source intermittently fails apt-get update"
category: envoy
tags:
  - github-actions
  - ci-cd
  - playwright
  - apt
  - flaky-ci
date: 2026-09-09
status: active
module: envoy
problem_type: build_error
component: infrastructure
symptoms:
  - "CI's dispatch job fails at the Install Playwright Chromium step, before any test runs"
  - "apt-get update reports a Hash Sum mismatch for Google's chrome-stable repository"
  - "The failure is intermittent across otherwise-identical CI runs"
root_cause: config_error
resolution_type: workflow_improvement
severity: medium
---

# GitHub Runner's Preinstalled Chrome apt Source Intermittently Fails `apt-get update`

## Problem

The `dispatch` CI job's "Install Playwright Chromium" step
(`.github/workflows/envoy-and-contracts.yaml:205-213`) runs `bunx playwright install
--with-deps chromium`, which internally runs `apt-get update` before installing Chromium's
system dependencies. GitHub's runner image preconfigures Google's `chrome-stable` apt
repository (Chrome is already installed on the image; the repo exists only to serve future
upgrades). That repository's index intermittently fails `apt-get update` with a hash-sum
mismatch, and `--with-deps` aborts entirely when `apt-get update` fails — even though the
actual dependencies it needs come from the standard Ubuntu repos, not Google's.

## What Didn't Work

The first fix attempt removed the source by an assumed filename:

```yaml
run: |
  sudo rm -f /etc/apt/sources.list.d/google-chrome*.list
  bunx playwright install --with-deps chromium
```

CI still failed at the same step on the next run — the runner keeps the Chrome apt source
under a filename the glob didn't match (this attempt predates the working fix below and is
superseded by it).

## Solution

Match the source file by its content instead of guessing its name, and drop whichever file(s)
reference Google's repository before installing (current tree,
`.github/workflows/envoy-and-contracts.yaml:205-212`):

```yaml
- name: Install Playwright Chromium
  # The runner image preconfigures Google's chrome-stable apt source, whose
  # index intermittently fails `apt-get update` with a hash-sum mismatch and
  # takes `--with-deps` down with it. Chrome is already installed there; the
  # source only serves upgrades, so drop it before installing our deps.
  run: |
    grep -rl dl.google.com /etc/apt/sources.list.d | xargs -r sudo rm -f
    bunx playwright install --with-deps chromium
  working-directory: packages/dispatch
```

## Why This Works

Chrome is already installed on the runner image; the apt source exists only to let `apt-get
upgrade` pick up a newer Chrome build later, which this job never does. Removing the source
entirely (rather than trying to fix or retry its broken index) has zero cost for this job and
eliminates the failure mode outright. Content-matching (`grep -rl dl.google.com`) is robust to
the runner image renaming the source file, unlike a filename glob that has to be kept in sync
with GitHub's image contents.

## Prevention

- When hardening a CI step against a flaky upstream apt/package index that the job does not
  actually need, prefer dropping the offending source (matched by content, not an assumed
  filename) over retrying `apt-get update` or pinning a mirror — the runner image's exact
  filenames for preinstalled sources are not a documented, stable contract.
- `xargs -r` (not bare `xargs`) so an empty grep match (source already absent, e.g. on a future
  runner image) is a no-op instead of an `rm` invocation with no arguments.

## Related Issues

- `sjawhar/legion#826`; the working content-match fix supersedes an earlier filename-glob
  attempt in the same PR.
