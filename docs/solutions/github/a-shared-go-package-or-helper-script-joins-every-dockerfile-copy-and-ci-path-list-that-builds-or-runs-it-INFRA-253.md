---
title: "A shared Go package or helper script joins every Dockerfile COPY and CI path list that builds or runs it"
category: github
tags:
  - path-filters
  - dockerfile
  - go-workspace
  - ci-triggers
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-253"
  - "sjawhar/legion#2229"
---

# A shared Go package or helper script joins every Dockerfile COPY and CI path list that builds or runs it

Extends docs/solutions/github/a-worker-image-path-filter-covers-the-code-the-init-container-runs.md.

## Rule

- A Go package another module imports lives outside `internal/`: Go refuses an import of another
  module's `internal/`.
- A new top-level package of `packages/envoy` goes into every Dockerfile that builds a binary
  importing it. `packages/envoy/docker/Dockerfile` copies `cmd/` and `internal/` only, so a new
  sibling directory is missing from the image build even though `go build` passes locally.
- A CI job's path filter lists the packages its binary compiles in. Derive the list from
  `go list -deps ./cmd/<binary>` and add every repository package it names, not just the new one.
- A script a job runs (a helper the harness execs) goes into the job's filter and into the
  workflow's own `on.push.paths` and `on.pull_request.paths`: a filter never fires for a workflow
  its trigger never started.
- A Bun test that builds a Go binary runs after the job's `setup-go` step.
- An image the live tests need is built by a workflow step with `docker/build-push-action` from a
  Dockerfile that compiles its own sources: `.github/scripts/check-image-trigger-paths.sh` refuses
  a plain `docker build` step and a `COPY` of a file CI built in an earlier step.

## Evidence

- `packages/envoy/listenfd` broke the envoy-smoke image build at 7e791a61 (`no required module
  provides package github.com/sjawhar/envoy/listenfd`); 44f8baeb added the `COPY` to
  `packages/envoy/docker/Dockerfile`.
- Review of #2229 found the `dispatch` filter in `envoy-and-contracts.yaml` lacked
  `packages/envoy/listenfd/**` and the workflow's triggers lacked `scripts/e2e/lib/held-port.py`;
  `go list -deps ./cmd/dispatch` also named `internal/githubappjwt` and `internal/id`, which the
  filter lacked before #2229 (fixed in 9729362306a9).
- `scripts/e2e/lib/held-port.test.ts` builds envoy-listener, so pr-and-main's
  `bun test scripts/e2e/lib` step moved after `setup-go`.
- The held-port wrapper image (`packages/envoy/infra/host/heldportwrapper/Dockerfile`) compiles the
  wrapper in a Go stage for the image-trigger check.
