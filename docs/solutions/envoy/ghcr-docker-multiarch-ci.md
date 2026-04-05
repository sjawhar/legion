---
title: "GHCR Docker Multi-Arch Build in GitHub Actions"
category: envoy
tags:
  - docker
  - github-actions
  - ci-cd
  - ghcr
  - multi-arch
  - buildx
date: 2026-04-05
status: active
module: envoy
related_issues:
  - "#243"
symptoms:
  - "docker push to ghcr.io fails silently"
  - "packages: write permission missing"
  - "gh release create not a git repository"
  - "arm64 build fails on amd64 runner"
---

# GHCR Docker Multi-Arch Build in GitHub Actions

## Context

The envoy release workflow builds Go binaries and creates GitHub releases. Adding Docker image build+push to GHCR required understanding several non-obvious conventions.

## Learnings

### 1. GHCR Requires `packages: write` Permission

The `GITHUB_TOKEN` can authenticate to GHCR via `docker/login-action`, but push will fail unless the workflow declares `packages: write`:

```yaml
permissions:
  contents: write   # for gh release create
  packages: write   # for GHCR push — easy to forget
```

Without it, login succeeds but push fails. Set this at the workflow level so all jobs inherit it.

### 2. Docker Build Context vs Dockerfile Path

In a monorepo, the Dockerfile lives at `packages/envoy/docker/Dockerfile` but the build context must be the package root (`packages/envoy/`) so that `COPY` commands resolve correctly:

```yaml
- uses: docker/build-push-action@v6
  with:
    context: packages/envoy          # where COPY commands resolve from
    file: packages/envoy/docker/Dockerfile  # path relative to repo root, not context
```

The `context` and `file` paths follow different conventions — `context` is the Docker daemon's working directory, `file` is relative to the repo root.

### 3. Multi-Arch Requires QEMU + Buildx

Standard Docker on GitHub runners only builds for the runner's native architecture. For multi-arch (amd64 + arm64):

```yaml
- uses: docker/setup-qemu-action@v3      # ARM64 emulation on amd64 runner
- uses: docker/setup-buildx-action@v3    # multi-platform builder (replaces default)
```

Both are required. Missing QEMU causes arm64 builds to fail; missing buildx means `platforms` is ignored.

### 4. Contracts Are Committed — No Generation Step Needed in Docker

The Go contracts at `packages/envoy/internal/contracts/generated.go` are committed to version control. The Docker build doesn't need a `gen:go` step — it picks up the committed file via `COPY internal ./internal`. The CI envoy job validates contracts are up-to-date by running `gen:go` before its binary build, and the Docker job depends on `[envoy]` for this gating.

### 5. `gh release create` Needs Git Checkout

The `gh` CLI's `release create` command requires a `.git` directory to resolve the repository. Jobs that only use `actions/download-artifact` have no git context. Adding `actions/checkout@v5` before artifact download fixes the "not a git repository" error without interfering with downloaded artifact directories.
