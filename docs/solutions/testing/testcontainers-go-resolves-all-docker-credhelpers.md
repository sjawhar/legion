---
title: "testcontainers-go resolves every configured Docker credHelper before a Docker Hub-only build"
category: testing
tags:
  - testcontainers
  - docker
  - credhelpers
  - go
  - local-dev
date: 2026-09-09
status: active
module: envoy
problem_type: test_failure
component: tooling
symptoms:
  - "go test -tags smoke ./internal/smoke/ fails locally with a Docker credential helper error"
  - "The same smoke test passes in CI with no credential configuration at all"
  - "The failing helper is for a registry (gcr.io/Artifact Registry) the test never pulls from"
root_cause: config_error
resolution_type: environment_setup
severity: low
---

# testcontainers-go Resolves Every Configured Docker credHelper Before a Docker Hub-Only Build

## Problem

What fails is the **pull**, not the build. The smoke test pulls `nats:2.10` from Docker Hub
through testcontainers (`packages/envoy/internal/smoke/listener_test.go:46`), and testcontainers
resolves credentials for that pull against every helper in the config. The
`docker buildx build` that produces the listener image does not: it reads the local Dockerfile
through the Docker CLI's own resolution and exits 0 on a machine whose helper is broken.
Nothing in either targets a private registry. The test still fails locally on a machine whose
`~/.docker/config.json` has a `credHelpers` entry mapping an unrelated registry (here,
`us-east1-docker.pkg.dev` → `gcloud`) to a helper that is currently unable to authenticate
non-interactively:

```
ERROR: (gcloud.auth.docker-helper) There was a problem refreshing your current auth tokens:
Reauthentication failed. cannot prompt during non-interactive execution.
```

testcontainers-go resolves Docker registry auth through the same `dockercfg` config-loading
path the Docker CLI uses, which reads every `credHelpers`/`credsStore` entry in
`~/.docker/config.json` (unless overridden by `DOCKER_AUTH_CONFIG` or `DOCKER_CONFIG`) and
invokes the configured helper binary when building auth config. A helper that isn't installed,
isn't authenticated, or times out fails that resolution outright — it doesn't fall back to
anonymous/no-auth for the registries the build actually needs. The CI runner has no such
`config.json` entry at all, so the failure is invisible in CI (`.github/workflows/`, "Container
smoke test (testcontainers)": `go test -tags smoke -v -timeout 5m ./internal/smoke/`, which runs
against the image the "Build the listener image the smoke test runs against" step built) and
only reproduces on a developer machine with a stale cloud credential helper configured for
something unrelated to the test.

## Solution

Fix the credential helper, not the test:

- Re-authenticate the failing helper for real use (`gcloud auth login`), or
- Remove the stale `credHelpers`/`credsStore` entry from `~/.docker/config.json` if that
  registry access is no longer needed locally, or
- Scope Testcontainers to a clean config via `DOCKER_CONFIG` pointed at a directory with a
  minimal `config.json` (no `credHelpers`/`credsStore`), so local runs don't depend on every
  registry credential on the machine being healthy.

Do not weaken the smoke test (skip it, stub the build, mock the container) to route around this
— the test is exercising the exact thing it should: a real image in a real container against a
real NATS. CI never reproduces it because the runner has no `config.json`. A broken local
credential helper is an environment problem, not a test design problem.

## Why This Works

testcontainers' credential resolution is global-config, not per-pull-scoped: it loads the entire
`credHelpers` map and is willing to shell out to any of its entries while building the auth
context for a pull, regardless of which registry the image comes from. Fixing or removing the
one broken entry, or isolating testcontainers to a config without it, sidesteps that without
changing what the test verifies.

## Prevention

- Keep `~/.docker/config.json` credential helpers current, or scope local test runners to a
  minimal Docker config via `DOCKER_CONFIG`/`DOCKER_AUTH_CONFIG`, on any machine that runs
  testcontainers-go tests for registries it also uses for unrelated cloud work.
- When a container-based test fails locally but passes in CI, check `~/.docker/config.json`
  for `credHelpers`/`credsStore` entries before suspecting the test or the tooling — CI's
  absence of any Docker config is often the actual difference, not a code path.

## Related Issues

- Observed while landing `sjawhar/legion#826`: CI's `envoy-go` job (including the `-tags smoke`
  step) passed; the same test could not run locally on the box carrying a stale `gcloud`
  credential helper for `us-east1-docker.pkg.dev`.
