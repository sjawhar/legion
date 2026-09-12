# Legion on Kubernetes

This runbook grows with the LEGION-19 tree. This page currently covers the worker image (LEGION-23);
the in-cluster daemon (LEGION-25) and the kind smoke (LEGION-26) add their sections beside it.

## Worker image

Every Legion agent process under `runtime: kubernetes` — architect, planner, implementer, tester, reviewer,
merger — runs from one image, `ghcr.io/sjawhar/legion-worker` (public). It carries:

- the pinned OMP fork build the daemon's default `omp_invocation` names — resolved at build time with the
  same `mise x github:sjawhar/oh-my-pi@<pin>` mechanism a tmux host uses, from the single pin source
  `packages/daemon/src/daemon/omp-pin.ts`; installed at `/opt/omp/bin/omp` (`LEGION_OMP_PATH`);
- the `legion` CLI compiled from the same commit (`legion`, `worker-shim`, `credential`, `gh`, `handoff`, and
  the hidden `probe-image`), at `/opt/legion/bin/legion`;
- `@sjawhar/pi-legion-envoy` packed from that commit's `packages/pi-envoy` (the exact `bun pm pack` steps
  `release.yaml`'s `pi_envoy` job runs) and linked into the isolated OMP profile `legion`
  (`OMP_PROFILE=legion`; plugins resolve to `/home/legion/.omp/profiles/legion/plugins/node_modules`);
- OMP's native modules, pre-downloaded into `/home/legion/.omp/natives/<version>/` so a pod never fetches them;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs), `git`, `gh`.

It runs as user `legion` (uid 1000, declared numerically so `runAsNonRoot` can verify it from the image
alone) with `HOME=/home/legion`, which must be writable (OMP writes sessions, logs, and `models.db` under
`~/.omp/profiles/legion`). Mount writable volumes below the profile directory, never at `/home/legion`
itself: everything the image-time probe proved lives under `HOME` — the plugin link and its lock at
`~/.omp/profiles/legion/plugins`, the natives at `~/.omp/natives` — and a volume at `HOME` (an `emptyDir`,
or a `HOME` volume under `readOnlyRootFilesystem`) shadows all of it silently. It is `linux/amd64` only:
the OMP fork release has no linux/arm64 build. One commit ⇒ one image: nothing in it is pinned to an npm
version.

### The image is probed before it publishes

The daemon refuses to serve unless its OMP exposes `pi.agents` and actually loads `pi-legion-envoy`
(`packages/daemon/src/daemon/boot-probes.ts`). The image build's last step runs the same two probes through
`legion probe-image`, so a build whose OMP or plugin is broken fails instead of publishing. The in-cluster
daemon (LEGION-25, planned) is to run `legion probe-image` in a one-shot pod against the configured digest.
To run it yourself: `docker run --rm --entrypoint legion ghcr.io/sjawhar/legion-worker@sha256:… probe-image`.

### Pin by digest, never by tag

`legion.yaml` `runtime.kubernetes.image` accepts only `ghcr.io/sjawhar/legion-worker@sha256:…`. A tag is
mutable; the daemon must know exactly what it probed, so a tag reference is refused at startup with
`runtime.kubernetes.image must be pinned by digest (@sha256:…)` (`packages/daemon/src/daemon/image-ref.ts`).

Where the digest is published:

- the job summary of every `Worker Image` run (Actions → Worker Image → the run → Summary);
- the body of the `cli-v<version>` GitHub release, under "Worker image", when `release.yaml` released the
  CLI in the same run (`gh release view cli-v<version> --json body -q .body`);
- `docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:<tag>` for any published tag.

Tags: `sha-<12 hex of the built commit>` on every run (on a pull request that is the PR head, never the
ephemeral merge commit); `<cli version>` only on `main` when `cli` released that version in the same run.
Runs from any other ref publish the `sha-` tag only and never touch a release.

### How it is built — and the iteration rule

`.github/workflows/worker-image.yaml` builds on the GitHub-hosted runner with `docker/setup-buildx-action`
+ `docker/build-push-action` (the pair `release-envoy-listener.yaml` uses), layer cache in GitHub Actions
cache (`cache-from: type=gha`, `cache-to: type=gha,mode=max`), pushed with the workflow's own `GITHUB_TOKEN`
— no third-party builder, no project variable, no extra credential. It runs (1) from
`release.yaml` after the `cli` job on every `main` push that touches the daemon or plugin, (2) on every head
of a pull request against `main` whose diff touches `packages/daemon/docker/**`, the OMP pin
(`packages/daemon/src/daemon/omp-pin.ts`), or the workflow itself — building the PR head and publishing
`sha-` only — and (3) by `gh workflow run worker-image.yaml --ref <ref>` once the workflow exists on `main`.
Trigger (2) is `pull_request`, not `push`: GitHub evaluates `pull_request` path filters against the whole PR
diff, so a later commit that touches none of those paths (a handoff, a docs fix) still gets the check and the
PR head never loses it; a `push` trigger filters on the pushed commits alone and would leave such a head
unguarded. The workflow's `id-token`/`packages`/`contents` permissions apply to same-repo pull requests (this
repository takes no fork PRs, whose token would be read-only).

**The image is built only by this workflow, on the GitHub-hosted runner.** Never build it on a workstation
— no `docker build`, `docker buildx`, or `docker compose build`: an unrelated buildx job took the sami-agents
host to load 646 on 2026-09-12 and the Legion daemon with it (the CI runner is not a workstation). Iterate by
pushing the PR branch (trigger 2) or, once merged, dispatching (trigger 3); check the Dockerfile and workflow
statically (`hadolint`, `actionlint` where installed) and run `bun test` for the TypeScript. Pulling and
running the published image locally is fine. A failed build is retried with `gh run rerun <run-id> --failed`
(`--failed` keeps the `cli` job's recorded outputs; a whole-run rerun of a `release.yaml` call re-executes
`cli` against its own tag and empties `cli_version`) or by pushing the branch again.

The build has no prerequisites outside this repository. After the first push there is one human action: if
the `legion-worker` GHCR package came out private, an anonymous `docker pull` fails until its visibility is
set to public — a package-settings action on GitHub with no API.

### ECR mirror

Node instance roles pull from the account's ECR without pull secrets, so the workflow mirrors the image
(by digest, same tags) on `main` runs only, and only when repository variables `AWS_ECR_PUBLISH_ROLE_ARN`,
`AWS_ECR_REGISTRY`, and `AWS_REGION` are all set. On any other ref the step is skipped and the summary says
`ECR mirror skipped: not a main run` (the publish role trusts `refs/heads/main` alone, and a PR's `sha-`
image is nothing anyone pins); with a variable unset it is skipped and the summary says
`ECR mirror skipped: <variable> unset`. The GHCR digest is authoritative; a failed mirror never changes it
(the summary says so and names the `gh run rerun <run-id> --failed` retry). The AWS side (publish role
trusting `repo:sjawhar/legion:ref:refs/heads/main`, ECR repository `legion-worker`) lives in agent-c's
`meta/infra` Pulumi, not here.

### Per-deployment toolchains layer on top

The base image carries Legion's own tools only. A deployment whose repositories need more (agent-c: `uv`,
Python, Node) builds its own image in **its** repo:

```dockerfile
FROM ghcr.io/sjawhar/legion-worker@sha256:…
# deployment toolchain here
```

and pins `runtime.kubernetes.image` to *that* image's digest. The toolchain churns on the deployment's
schedule, not Legion's release schedule, and the deployment's repo owns its own reproducibility — one Legion
release therefore never has to know about anyone's Python version.

### Entrypoint

The image's `ENTRYPOINT` is `["legion", "worker-shim"]` — a placeholder so the image has a sensible
default command. The real entrypoint (how provider keys reach the OMP child from `*_FILE` secrets, and how
the shim dials the daemon) is the Kubernetes runtime's (LEGION-24), which sets the pod's `command`. To run
anything else in the image, override it: `docker run --rm --entrypoint sh <image> -c '…'`,
`docker run --rm --entrypoint legion <image> probe-image`.
