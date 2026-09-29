---
title: "A container build inside go test spends the test's deadline"
category: testing
tags:
  - testcontainers
  - docker
  - go
  - ci
  - timeouts
date: 2026-09-28
status: active
module: envoy
problem_type: test_failure
component: ci
symptoms:
  - "panic: test timed out after 5m0s with the stack in testcontainers BuildImage"
  - "A container test that passes most runs fails on a slow runner with no code change"
  - "The image build's last step is in the log and the assertions never ran"
root_cause: design_error
resolution_type: workflow_improvement
severity: medium
---

# A Container Build Inside go test Spends the Test's Deadline

## Problem

`packages/envoy/internal/smoke/listener_test.go` built the listener image with
`testcontainers.WithDockerfile`, so `go test`'s `-timeout` bounded the image build as well as
the behaviour the test asserts. The bound is then a build budget nobody chose: across the ten
Release Envoy Listener runs before this was fixed, `TestSmoke` reported 256.60–289.47 s against
a 300 s timeout, and every one of its five subtests reported 0.00 s. All of the budget went to
the build and none of it to the assertions.

Run 36443745717 (`83fe7bd7`, attempt 1) crossed the line: `panic: test timed out after 5m0s`
with build step 46/46 in the log. Nothing about the listener was wrong — the runner was slow —
and the release published no image until a human re-ran the job.

## Solution

Three parts, and the third is the one that keeps the other two honest: **build the artifact in a
CI step, pass its name in the environment, and fail when the name is absent.**

```yaml
env:
  ENVOY_SMOKE_IMAGE: legion-envoy-smoke:${{ github.sha }}
steps:
  - name: Build the listener image the smoke test runs against
    run: docker buildx build --load --tag "$ENVOY_SMOKE_IMAGE" --file packages/envoy/docker/Dockerfile .
  - name: Container smoke test (testcontainers)
    run: go test -tags smoke -v -timeout 5m ./internal/smoke/
    working-directory: packages/envoy
```

```go
image := os.Getenv("ENVOY_SMOKE_IMAGE")
if image == "" {
    t.Fatal("ENVOY_SMOKE_IMAGE is unset, and this test never builds the image itself.\n…")
}
```

Keeping a build-it-yourself fallback would undo the fix in silence. A rename, a typo in the
variable, or a new lane that forgets the build step leaves the test building again inside its own
deadline, and CI stays green until the build is slow enough to cross it — which is the failure
this learning is about. The refusal turns all three into an immediate red that names the
variable and prints the build command. The assertions are untouched.

`docker buildx build --load` is the portable form: it loads into the daemon's image store both
on a runner where `docker/setup-buildx-action` has made a `docker-container` builder the default
(Release Envoy Listener) and on one still using the `docker` driver (Legion Envoy and Contracts),
where the build also reads base images the job pre-pulled.

**Name the image by content, not by a fixed tag.** The message the refusal prints is
`img=$(docker buildx build --load -q -f packages/envoy/docker/Dockerfile .)` and then
`ENVOY_SMOKE_IMAGE=$img go test …`. Go's test cache keys on the value of every environment
variable the test reads, so a rebuilt image changes `$img` and the test runs again; under a
fixed tag it does not. Measured: with `ENVOY_SMOKE_IMAGE=legion-envoy-smoke:local`, re-tagging
that name to `nats:2.10` and re-running printed `ok … (cached)` — a pass reported for an image
that could not possibly pass. With the id, the same substitution re-ran the test and failed.
A tag is also per-checkout state: two working copies building different code collide on it.

## Why This Works

A step has the job's budget; a test has its own. Moving the build moves it from a five-minute
deadline to one measured in hours, and what is left under `-timeout` is only what the test
exists to exercise. Measured in CI: `TestSmoke` 283.84 s before, 0.98 s after, with the build
155 s in its own step. On a devbox at load average 80: 2.2–20.7 s across four runs with a
prebuilt image, 521.6 s when the same test built the image itself.

The bound that remains is deliberate and stated beside the step, with the numbers it came from.

## Prevention

- A test whose slowest operation is a build, an install or a download is not bounded by its
  assertions. Check what fraction of the wall time the subtests report — `--- PASS: X (0.00s)`
  under a four-minute parent is the signal.
- When a timeout is the fix, say in a comment what it bounds and what was measured. A bare
  `-timeout 10m` is the same cliff further away.

## Related Issues

- LEGION-361. The same step runs in `.github/workflows/release-envoy-listener.yaml` and
  `.github/workflows/envoy-and-contracts.yaml`; both were changed together, because one test
  cannot have two invocation contracts.
- `docs/solutions/testing/a-loaded-devbox-stretches-every-wall-clock-budget-in-the-envoy-and-pi-envoy-suites.md`
  — the same class of failure where the slow thing is the machine rather than a build.
