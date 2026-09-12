---
title: "Baking OMP and a Plugin into a Container Image"
category: infra
tags:
  - docker
  - omp
  - plugins
  - natives
  - kubernetes
  - probes
date: 2026-09-12
status: active
module: worker-image
related_issues:
  - "sjawhar/legion#966"
symptoms:
  - "`omp plugin install <file>.tgz` fails with ENOTDIR … /package.json"
  - "an image-time `legion probe-image` fails with 'does not expose pi.agents' on a slow link"
  - "a pod starts but OMP cannot find the plugin or re-downloads native modules"
---

# Baking OMP and a Plugin into a Container Image

`packages/daemon/docker/worker.Dockerfile` (LEGION-23) ships the pinned OMP fork, the `legion`
CLI, and `@sjawhar/pi-legion-envoy` in one image and runs the daemon's two boot probes as the
last `RUN`. Four facts about OMP decided its shape; each one cost an iteration to discover.

## `omp plugin install` links a directory; it rejects a tarball

`omp plugin install <path>.tgz` fails with `ENOTDIR … /package.json`. `omp plugin install <dir>`
**symlinks** the directory into the profile's `plugins/node_modules/<name>` and records
`enabled: true` in `plugins/omp-plugins.lock.json`. So the image unpacks the `bun pm pack` tarball
into a permanent path (`/opt/legion/pi-legion-envoy`, `tar --strip-components=1`) and links that.
The directory must outlive the build step — the link is what OMP resolves at runtime.

Pack with the same steps the release workflow uses (`jq '.omp.extensions = ["dist/envoy.js",
"dist/legion.js"]'` then `bun pm pack`): `prepack.sh` refuses to pack the source manifest, and the
build must not invent a second packing recipe.

## The profile root derives from HOME

With `OMP_PROFILE=legion`, OMP's `DirResolver` puts the profile at `$HOME/.omp/profiles/legion`.
An `XDG_DATA_HOME` override applies only when that directory already exists, so a fixed path like
`/opt/omp-profile` is not reachable through the profile mechanism. Set `HOME` explicitly in the
image (`ENV HOME=/home/legion`) and create the user with that home.

Consequence for the pod spec: **everything the image-time probe proved lives under `HOME`** — the
plugin symlink and its lock at `~/.omp/profiles/legion/plugins`, the natives at `~/.omp/natives`.
A writable volume mounted **at** `/home/legion` (an `emptyDir`, or a `HOME` volume under
`readOnlyRootFilesystem`) shadows all of it silently: OMP starts, finds no plugin, re-downloads
natives. Mount writable volumes **below** the profile directory (sessions, logs, `models.db`),
never at `HOME` itself.

## The ~345 MB native-module download happens on OMP's first run

`pi_natives.linux-x64-{baseline,modern}.node` are fetched into `~/.omp/natives/<version>/` the
first time OMP runs in a fresh `HOME` — which, in the image, is the `omp plugin install` step.
That is the right place for it: the daemon's `CommandRunner` (`state/fetch.ts`, `defaultRunner`)
kills any single `omp` invocation after 30 s, so a download inside the **first probe** would read
as a definitive `does not expose pi.agents` failure on a slow link. The order

```dockerfile
RUN omp plugin install /opt/legion/pi-legion-envoy \
    && legion probe-image \
    && rm -rf /home/legion/.omp/profiles/legion/logs
```

puts the download in step 1 and probes with natives present in step 2 (~8 s). The guard is
conditional on link speed — on a fast runner the download finished in ~4 s and would have fit
inside the probe — so a green build does not prove the ordering matters; keep it anyway, it is
free. Run the step as the runtime user so the layer ships the natives and a pod never fetches them.

Proof that they are baked, not merely cached on the build host:
`docker run --rm --network none --entrypoint legion <digest> probe-image` still prints
`probe-image: OK`. Measured on the LEGION-23 image: exit 0, ~30 s wall offline versus ~8 s online —
OMP waits out some network call before answering, so an offline probe sits right at the 30 s
runner ceiling. Treat a `--network none` probe as a natives check, not a latency benchmark.

## Run the daemon's own probes as the image gate

The daemon refuses to serve unless `omp models --no-extensions --extension <probe>` prints
`LEGION_OMP_AGENTS=available` and `omp models --extension <probe>` prints `LEGION_PLUGIN_LOADED=yes`
(the marker `legion.ts` sets on load). Both live in `packages/daemon/src/daemon/boot-probes.ts`;
the hidden `legion probe-image` subcommand calls the same functions with an empty launch prefix
(an image carries no `secrets` wrapper) against `LEGION_OMP_PATH`, refusing to fall back to `PATH`.
Running it as the build's last step means a broken image never publishes, and running it against
the pulled digest is the tester's acceptance check. A manifest file on disk is never the gate: a
disabled or unregistered plugin has a manifest and still fails the load probe.

## Other pins worth knowing

- The OMP fork pin has one source, `packages/daemon/src/daemon/omp-pin.ts`; the Dockerfile prints
  it in-build (`bun omp-pin.ts > /out/omp-pin`) and the `tools` stage reads it via `COPY --from`.
  No `ARG`, nothing for a workflow to compute.
- `mise x github:<owner>/<repo>@<version>` resolves the fork inside the build exactly as the daemon
  does; mount `github_token` as a BuildKit secret (`MISE_GITHUB_TOKEN`) so a shared runner IP
  does not hit the unauthenticated API limit.
- `oven/bun:<version>-slim` owns uid 1000, so the runtime stage is `debian:bookworm-slim` with Bun
  copied from the build stage; `USER 1000:1000` (numeric) lets Kubernetes `runAsNonRoot` verify
  from the image alone.
- The OMP fork release ships linux-x64 (and darwin-arm64) only: `platforms: linux/amd64`.
- hadolint via a published container (`docker run --rm -i hadolint/hadolint hadolint -`) is a
  static check that costs nothing and caught `curl | sh`, `export $(cmd)`, and a non-numeric USER.

## Open questions the retro left for the next iteration

- The image ships both native variants (`baseline` and `modern`, ~180 MB each of a 373 MB image);
  OMP loads one at runtime. Whether the fork can be told to fetch only the variant the CPU needs is
  a question for the OMP fork, not for the Dockerfile.
- `workflow_dispatch` accepts a `cli_version` input and, on `main`, tags the image with it without
  checking that `cli` released that version in the same run. The `workflow_call` path from
  `release.yaml` is the only one that can guarantee that; treat the dispatch input as an
  operator-only override and leave it empty unless re-tagging an already-released version.
