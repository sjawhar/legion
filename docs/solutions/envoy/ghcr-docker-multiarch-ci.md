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
  - native-runners
date: 2026-10-27
status: active
module: envoy
related_issues:
  - "#243"
  - "INFRA-522"
symptoms:
  - "docker push to ghcr.io fails silently"
  - "packages: write permission missing"
  - "gh release create not a git repository"
  - "arm64 build fails on amd64 runner"
  - "arm64 image build under QEMU takes over 30 minutes"
---

# GHCR Docker Multi-Arch Build in GitHub Actions

## Context

The envoy release workflow builds Go binaries and creates GitHub releases. Adding Docker image build+push to GHCR required understanding several non-obvious conventions.

## Learnings

### 1. GHCR Requires `packages: write` Permission

The `GITHUB_TOKEN` can authenticate to GHCR via `docker/login-action`, but push will fail unless the job that pushes holds `packages: write`:

```yaml
docker:
  permissions:
    contents: read    # for actions/checkout
    packages: write   # for GHCR push — easy to forget
```

Without it, login succeeds but push fails. `release-envoy-listener.yaml` sets `permissions: {}`
at the workflow level and every job declares its own `permissions` block, so no job inherits a
grant: the `docker` legs hold `contents: read` and `packages: write`, `image_index` holds
`packages: write` alone, and `release_publish` holds `packages: write` (to move `latest`) beside
`contents: write` (for the GitHub release) and its other scopes. A job added later with no block
of its own gets no scope rather than an unreviewed grant.

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

The same holds for the steps that build the smoke image in `envoy-and-contracts.yaml` and
`release-envoy-listener.yaml`, and for a local build by hand. Narrowing the context to
`packages/envoy` fails the build at the first workspace `COPY`.

Those two smoke steps are `docker/build-push-action@v6` with `load: true`, not a `run:` step:
`.github/scripts/check-image-trigger-paths.sh` reads the action's `context` and `file` to check
that the workflow's path triggers cover every file the build reads, and refuses a build written
as a shell command, which it cannot read reliably.

### 3. Build Each Platform on Its Own Native Runner, Not Under QEMU

A GitHub-hosted runner builds natively only for its own architecture. This workflow first built
both platforms in one `docker` job on an amd64 runner: `docker/setup-qemu-action` emulated arm64
behind one `platforms: linux/amd64,linux/arm64` build. It worked, and it was slow. Over 94 `main`
runs from 2026-10-19 to 2026-10-27 the `docker` job's median was 40.17 minutes, 34.6 of them
(median) the two-platform build-push step that ran the emulated arm64 build. Its smoke test
covered amd64 alone, while the push was amd64 and arm64.

The fix is Docker's "Distribute build across multiple runners": one matrix leg per platform, each
on a runner of that architecture (`ubuntu-24.04-arm` for arm64), each pushing its platform by
digest, and a job after them that joins the digests into the tagged index. No QEMU step is left,
and each leg smoke-tests the platform it pushes, natively, before it pushes it:

```yaml
docker:
  name: docker (${{ matrix.arch }})
  strategy:
    matrix:
      include:
        - arch: amd64
          runner: ubuntu-24.04
        - arch: arm64
          runner: ubuntu-24.04-arm
  runs-on: ${{ matrix.runner }}
  steps:
    - uses: docker/setup-buildx-action@v4
    - id: build
      uses: docker/build-push-action@v7
      with:
        platforms: linux/${{ matrix.arch }}
        provenance: mode=min
        outputs: type=image,name=ghcr.io/sjawhar/legion/envoy,push-by-digest=true,name-canonical=true,push=true
        cache-from: type=registry,ref=ghcr.io/sjawhar/legion/envoy:buildcache-${{ matrix.arch }}
        cache-to: ${{ github.ref == 'refs/heads/main' && format('type=registry,ref=ghcr.io/sjawhar/legion/envoy:buildcache-{0},mode=max', matrix.arch) || '' }}
```

- `outputs` replaces `push` and `tags`: the leg pushes an untagged image, and only the join tags
  `envoy:<sha>`, once both legs have pushed. `push: true` beside `outputs` would request a second
  exporter.
- The layer cache is a registry cache in the image's own package
  (`cache-from: type=registry,ref=<image>:buildcache-<arch>`): a cache manifest, not an image.
  Every build reads it; `cache-to` is set only on `main`, through the
  `github.ref == 'refs/heads/main' && format(...) || ''` expression, so a dispatch from another
  branch writes none: the empty string leaves the leg with no cache export.
- Each platform needs a cache tag of its own (`buildcache-<arch>`). Legs sharing one ref
  overwrite each other's cache manifest, so each run would find only the other platform's cache.
- Not `type=gha`: its `mode=max` export added about 1.6 GB of new Actions-cache entries per main
  run, at 27-29 runs a day (measured 2026-10-27), and evicted the entries pull requests read.
  A writer on a branch or pull request can also be cancelled mid-export. With writes on `main`
  only, the cache tracks the tree every build branches from, and no branch replaces it with its
  own layers.
- `docker/setup-buildx-action` stays on every leg: the registry cache exporter (and `mode=max`)
  needs the `docker-container` builder it creates, not the default `docker` driver.
- The digests travel as artifacts, not job outputs: a matrix job's outputs keep only the last
  leg's value. Each leg writes `steps.build.outputs.digest` as the name of an empty file and
  uploads it as `envoy-image-digest-<arch>`; the join downloads them with `pattern:
  envoy-image-digest-*` and `merge-multiple: true`.

The join is a job of its own (`image_index`) that runs `docker buildx imagetools create
--metadata-file <file> --tag ghcr.io/sjawhar/legion/envoy:<sha> <image>@sha256:<amd64>
<image>@sha256:<arm64>` with the runner's own docker buildx, so it checks out nothing and sets up
no builder. It reads the index's digest from the metadata file
(`jq -r '."containerimage.descriptor".digest'`), never back through the `envoy:<sha>` tag, which
another run may move, and hands that digest to the attestation and the release job. With
`provenance: mode=min` on each leg, each leg's push is itself an index of the platform manifest
and its attestation manifest, and `imagetools create` carries both legs' attestation manifests
into the joined index; the job's closing `docker buildx imagetools inspect` prints the index's
platforms and attestation manifests in the run log.

### 4. Contracts Are Committed — No Generation Step Needed in Docker

The Go contracts at `packages/envoy/internal/contracts/generated.go` are committed to version control. The Docker build doesn't need a `gen:go` step — it picks up the committed file via `COPY internal ./internal`. The CI envoy job validates contracts are up-to-date by running `gen:go` before its binary build, and the Docker job depends on `[envoy]` for this gating.

### 5. `gh release create` Needs Git Checkout

The `gh` CLI's `release create` command requires a `.git` directory to resolve the repository. Jobs that only use `actions/download-artifact` have no git context. Adding `actions/checkout@v5` before artifact download fixes the "not a git repository" error without interfering with downloaded artifact directories.
