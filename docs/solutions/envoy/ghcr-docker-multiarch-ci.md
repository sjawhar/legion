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

In a monorepo the two paths follow different conventions, and both are relative to the repo
root: `context` is the tree the daemon receives and `COPY` resolves against, `file` is where the
Dockerfile itself lives. They are not the same directory here. Every call site builds
`packages/envoy/docker/Dockerfile` from the **repository root**, because the Dockerfile's web
stage copies the whole Bun workspace — the root `package.json`, `bun.lock`, `patches/`, and each
workspace package's manifest — none of which is under `packages/envoy/`:

```yaml
- uses: docker/build-push-action@v6
  with:
    context: .                              # the repo root, where COPY resolves from
    file: packages/envoy/docker/Dockerfile  # also relative to the repo root
```

The same holds for the `docker buildx build --load --file packages/envoy/docker/Dockerfile .`
steps that build the smoke image in `envoy-and-contracts.yaml` and `release-envoy-listener.yaml`,
and for a local build by hand. Narrowing the context to `packages/envoy` fails the build at the
first workspace `COPY`.

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
