---
title: "Hold a pin's label to the bytes it names, never to the name those bytes once had"
category: best-practices
tags:
  - pinning
  - container-images
  - digests
  - supply-chain
  - crane
date: 2026-10-30
status: active
module: .github/scripts
related_issues:
  - "INFRA-567"
  - "sjawhar/legion#2512"
---

# Hold a pin's label to the bytes it names, never to the name those bytes once had

A pin written as `<name>@<digest>` with a human-written version label beside it has one failure
mode nothing else catches: the label and the digest disagree, and every rule that reads the label
is then wrong about what runs.

- Read the version out of the pinned artifact itself, addressed by its digest — the image config,
  the package's own declared version, whatever the artifact publishes. That read answers the same
  on every day, so it may run on every check of every row, new or years old.
- Never resolve the upstream **tag** the label mentions. A floating tag moves, so such a read
  cannot tell a row written now with a wrong label from a row written long ago with a right one;
  it fails the correct old row on every run until a person rewrites a row that was never wrong.
- Read those bytes from a copy you control wherever you hold one, and from the third party only
  where the run is about to read it anyway. A validation that fetches upstream on every run
  reintroduces the dependency the pin removed.
- Where the artifact publishes its version nowhere, move the assertion into the build that runs
  those bytes: have the build read the pin file and refuse a version that is not it.

## Evidence

sjawhar/legion#2512 reached this in three review rounds.

**The gap.** Once the copy read `<repository>@<row digest>`, nothing compared a row's digest with
the version its label claimed: relabelling both `oven/bun` rows to 1.4.5 over the 1.4.4 digests
passed every gate.

**The obvious fix is wrong.** Comparing `crane digest <upstream with its tag>` against the row's
digest refused correct rows: Docker Hub answered `sha256:7ae34c25…` for
`library/nats:2.15.1-alpine` while the correctly pinned row held `sha256:2ae57dfb…`, so the
`ci-images` job exited 1 for every retagged row and a re-run gave the same answer. That read is
deleted, and the script header carries the reason so nobody adds it back
(`.github/scripts/mirror-ci-images.sh`, `NO READ OF AN UPSTREAM TAG CAN STAND HERE`).

**What replaced it, split by what each image publishes.** `library/golang` writes `GOLANG_VERSION`
into its config, so the copy runs `crane config --platform linux/amd64` on the row's digest for
every golang row and refuses one whose bytes carry another Go than its tag names — from
`ghcr.io/sjawhar/legion/ci@<digest>`, the package's own copy, for a row the package already holds,
and from the upstream only before a copy. `oven/bun` publishes its version in no part of its
config (`PATH`, `BUN_RUNTIME_TRANSPILER_CACHE_PATH`, `BUN_INSTALL_BIN`, and nothing else), so the
build decides: `packages/envoy/docker/Dockerfile`'s web stage copies `.bun-version` and refuses a
`bun --version` that is not its content, as the worker image already did through its build script.

The harness drives both halves on a **held** row, which the deleted tag read could never reach.
