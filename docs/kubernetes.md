# Legion on Kubernetes

This runbook covers the worker image, the session store, the Kubernetes runtime, and the operator-launched controller.

The Go coordinator (`packages/daemon`, LEGION-208) runs each process as an Agent Sandbox; its live proof is `scripts/e2e/stage4b-sandbox-tree.sh` on the production cluster. The TypeScript daemon (`packages/daemon`) no longer runs on Kubernetes: it refuses `runtime: kubernetes` at config load and names the Go daemon (LEGION-286). Its runtime's section below is kept for reference and goes with `packages/daemon` at Stage 7 (LEGION-223).

## Worker image

Every Legion agent process under `runtime: kubernetes` — architect, planner, implementer, tester, reviewer,
merger — runs from one image, `ghcr.io/sjawhar/legion-worker` (public). It carries:

- the pinned OMP fork build the daemon's default `omp_invocation` names — resolved at build time with the
  same `mise x github:sjawhar/oh-my-pi@<pin>` mechanism a tmux host uses, from the single pin source
  `.omp-pin`; installed at `/opt/omp/bin/omp` (`LEGION_OMP_PATH`);
- `legion` (`packages/daemon`), compiled from the same commit at `go.work`'s Go version, static, at
  `/opt/legion/bin/legion` — the `legion` on `PATH` and the image's `ENTRYPOINT` — with the
  `agent-secrets` client beside it at `/opt/legion/bin/agent-secrets`. It links the commit it was built
  from: `docker run --rm ghcr.io/sjawhar/legion-worker@sha256:… version` prints
  `legion (devel) commit <sha>`. It embeds Legion's role prompts (`packages/daemon/internal/prompts/roles`:
  phase workers compose `core/<role>.md`, `mechanics/headless.md`, and the per-role residue; merger
  composes headless plus its residue; root architect, controller, and sub-architect prompts remain
  single-file). The daemon snapshots its own into its state directory before a pane can read them and
  inlines that snapshot into each pod it runs, and `legion probe-image`, given no `--role-references`,
  resolves the task agents and skills named by the prompts the image's own `legion` embeds
  (`packages/daemon/cmd/legion/probe_image.go`);
- `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` packed from that commit's `packages/pi-envoy` and
  `packages/pi-legion` (the exact `bun pm pack` steps `release.yaml`'s `pi_envoy_build` and
  `pi_legion_build` jobs run), unpacked at `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`, the two
  roots a pod names as Oh My Pi's explicit extensions — a role relaunched onto a newer plugin contract
  in a kept pod loads the generation directory's copies instead, installed from npm at
  `$XDG_STATE_HOME/legion-plugins/g<generation>/node_modules/@sjawhar/{pi-envoy,pi-legion}`
  ("Contract discipline" below) — the Envoy plugin first, with extension discovery on. Neither is linked
  into the OMP profile: a plugin both installed and named by `--extension` loads twice, and the image
  probe refuses an image whose Envoy plugin does. A human debugging inside a pod with a bare `omp` has
  the CodeGraph tool and the repository's extensions but no Legion or Envoy tools unless it passes
  `--extension /opt/legion/pi-envoy --extension /opt/legion/pi-legion`;
- `@bopstack/pi-codegraph` (from npm, pinned), the one plugin linked into the OMP profile `legion`
  (`OMP_PROFILE=legion`; plugins resolve to `/home/legion/.omp/profiles/legion/plugins/node_modules`),
  backed by the CodeGraph CLI (`@colbymchenry/codegraph`, pinned) at `/opt/codegraph/bin` (`PATH`) — the
  `codegraph` tool a tester queries for `affected` tests and a reviewer for `impact`/`callers` blast
  radius (`packages/daemon/internal/prompts/roles/core/tester.md`, `core/reviewer.md`). A pod's agent has that tool because its launch loads profile plugins (dispatch://LEGION-629), which the capability report's `codegraph` row says ([The deployment's capability report](#the-deployments-capability-report));
- OMP's native modules, pre-downloaded into `/home/legion/.omp/natives/<version>/` so a pod never fetches them;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs) and `gh` at `/usr/local/bin`, and
  `git` at `/usr/bin/git` from the `debian:trixie-slim` base — jj's git backend requires git >= 2.42
  (bookworm's 2.39.5 made every `jj git clone` in the init container fail), so the build also proves the
  image's jj accepts its git with a network-free `jj git clone` of a scratch bare repository before the
  probes run, and its last step refuses a git anywhere but `/usr/bin/git`;
- `socat` from Debian trixie's archive, at `/usr/bin/socat`: a byte pipe that carries a connection over
  `kubectl exec` to a port the pod listens on at `127.0.0.1`, such as a dev server an agent runs
  (`kubectl -n legion exec -i <pod> -c <role> -- socat - TCP:127.0.0.1:<port>`). `kubectl port-forward`
  reaches no listener in the pod under gVisor: the kubelet dials the pod's host-side network namespace,
  while the pod's sockets live in gVisor's own network stack. socat is one such pipe; `python3` or bash's
  `/dev/tcp` over the same `kubectl exec` works too. A listener bound to `0.0.0.0` is also reachable on
  the pod IP from inside the cluster, unless the deployment's NetworkPolicy denies that ingress;
- a generic toolchain for the repositories the workers work, specific to none of them: `uv` and `uvx`;
  `node`, `npm`, `npx` and `corepack` from Node 24 LTS, with `pnpm`, `pnpx`, `yarn` and `yarnpkg` linked
  to corepack's shims (the links `corepack enable` would write, which uid 1000 cannot); the Go toolchain
  at `go.work`'s version — `go` and `gofmt`, the tree at `/opt/go` (its `GOROOT`, which `go` finds by
  resolving the `/usr/local/bin/go` symlink; the image sets no `GOROOT`); the AWS CLI v2's `aws`; and
  `kubectl` 1.35, the minor version of the API server of the production cluster the workers run on,
  which Kubernetes' version skew policy requires within one minor of kubectl (a deployment whose
  cluster runs another minor layers its own `kubectl` on the image:
  [Per-deployment toolchains layer on top](#per-deployment-toolchains-layer-on-top)) — all at
  `/usr/local/bin`, which is on the image's `PATH` and every pod's. Each is a pinned release whose
  linux/amd64 download the build checks against a pinned SHA-256 before unpacking or installing it
  (the `ARG`s at the top of `worker.Dockerfile`; `kubectl`'s is the SHA-256 Kubernetes publishes beside
  the binary, taken once the binary's Sigstore signature verifies).
  The image carries the client and no credential for it: what a worker reaches with `kubectl` is
  whatever Kubernetes identity the operator's pod gives it. A corepack shim runs the version a project's
  `packageManager` names, or else the default that corepack ships (the image sets
  `COREPACK_DEFAULT_TO_LATEST=0`, so never npm's newest release), fetched on first use into the user's
  corepack cache. Node, Go and the AWS CLI keep their trees at `/opt/node`, `/opt/go` and
  `/opt/aws-cli`, outside `HOME`, so no volume a pod mounts under `HOME` shadows any of it;
- what the capability check (`packages/daemon/internal/capabilities`, which `legion probe-image` runs —
  [the probe subsection](#the-image-is-probed-before-it-publishes)) requires of the image, each outside
  `HOME` for the same reason: the language servers Oh My Pi's LSP tools start when a working directory
  carries their root markers — `gopls` (built static in the Dockerfile's `go` stage, at
  `/usr/local/bin/gopls`), `typescript-language-server` and `pyright-langserver`, the latter two
  installed under `/opt/language-servers/node_modules` by `npm ci --ignore-scripts` from
  `packages/daemon/docker/language-servers`'s own `package.json` and `package-lock.json` (the pins live
  in that `package.json`), with their launchers (and TypeScript's `tsc` and `tsserver`, and the
  `pyright` CLI) linked at `/usr/local/bin`. TypeScript stays on this repository's own major, since
  `typescript-language-server` runs `tsserver`, which TypeScript 7 no longer ships; the server resolves
  the TypeScript installed beside it unless the repository's own
  `node_modules` holds one, which it prefers. From Debian trixie's archive, at `/usr/bin`: `python3`
  (the interpreter Oh My Pi's Python eval runs — `omp setup python --check` answers `available: true`
  on it), `curl`, `wget`, `jq`, `ssh-keygen` (from `openssh-client`, which also brings `ssh`, `scp` and
  `sftp`: they reach no host that `curl`, `wget`, `git`, `python3` and `socat` on the same apt line
  could not already reach, and what a pod may reach is Legion's ruling, the withheld `network`
  capability row — dispatch://LEGION-5, the pod is the boundary — a declaration the capability report
  renders, not a network control this repository ships; the package's setuid
  `/usr/lib/openssh/ssh-keysign` and setgid `/usr/bin/ssh-agent` lose those bits in the runtime stage,
  and the build's final step refuses an image where any file under `/usr/lib/openssh` or
  `/usr/bin/ssh-agent` carries one),
  and `chromium` for the browser tools, installed without its Recommends (no
  setuid sandbox: Oh My Pi launches it with `--no-sandbox --disable-setuid-sandbox`). Oh My Pi picks
  its browser from `PUPPETEER_EXECUTABLE_PATH` first, else the first Chrome or Chromium it finds on
  `PATH`, and downloads Chrome for Testing into its cache only with none of those — so the image's
  `chromium` is the one every worker gets, with nothing downloaded, and the probe runs that same
  resolution. The image sets no `PUPPETEER_EXECUTABLE_PATH`: a variable the image's `ENV` sets is one
  `runtime.kubernetes.pod.env` is refused, and an operator's pod env naming another browser is what the
  probe checks (it runs that executable), not something to refuse. `jq` and `ssh-keygen` are real
  binaries so a worker's subprocesses — `bun test`, a script, a subagent's shell — find them, where the
  harness's `jq` builtin reaches only the bash tool. A subprocess's `jq` is therefore the image's
  `/usr/bin/jq`, trixie's, while the bash tool's own `jq` stays the harness's builtin, a different
  engine (jaq), so an expression is checked with the engine the script will run
  (`docs/solutions/legion/worker-pane-shell-gotchas.md`, "The bash tool's `jq` is jaq"). `uv` still
  installs each project's own Python, from its `.python-version` or `requires-python`, the first time
  the project runs (`uv sync`, `uv run`): in a Sandbox pod that is on the issue's volume
  ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)); elsewhere it is uv's default,
  `~/.local/share/uv/python`. The image's `python3` is for Oh My Pi's own eval, not a project's
  interpreter.

It runs as user `legion` (uid 1000, declared numerically so `runAsNonRoot` can verify it from the image
alone) with `HOME=/home/legion`, which must be writable (OMP writes sessions, logs, and `models.db` under
`~/.omp/profiles/legion`). Mount writable volumes below the profile directory, never at `/home/legion`
itself: everything the image-time probe proved lives under `HOME` — the CodeGraph plugin's link and the
lock at `~/.omp/profiles/legion/plugins`, the natives at `~/.omp/natives` — and a volume at `HOME` (an `emptyDir`,
or a `HOME` volume under `readOnlyRootFilesystem`) shadows all of it silently. It is `linux/amd64` only:
the OMP fork release has no linux/arm64 build. One commit ⇒ one image, with one exception: a
fresh pod still loads the image's own copies of `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`, from
the commit, never from an npm release, but a role relaunched onto a newer plugin contract in a
kept pod loads the release's published pair, installed from npm under the generation directory
("Contract discipline" below) — what the image does take from npm (the CodeGraph plugin and
CLI, TypeScript and the two npm language servers) is pinned by the `ARG`s at the top of
`worker.Dockerfile`.

### Image size and pull time

A node that has never run the image pulls it whole before the pod's init containers start, so the
image's compressed size is a cold launch's first cost. Before the capability tools above,
`ghcr.io/sjawhar/legion-worker:1.18.1` was 12 layers and 369 MiB compressed (386,510,448 bytes). With
them it is 22 layers and 1007 MiB compressed (1,056,280,622 bytes, the `sha-ff33c8c9fb1b` build of the
pull request that added them): the Debian layer grows from about 30 MiB to 307 MiB, almost all of it
Chromium and the library closure it depends on; the Go toolchain is 64 MiB and `gopls` 20 MiB; Node's
tree with the two npm language servers is 66 MiB. On EKS, how long a node takes to pull that is not
recorded here: the stage 4a launch below is where it is measured. The pull must fit the launch's
wait for the role's launcher before any init container runs: `worker_boot_timeout_seconds`
(`packages/daemon/internal/config/config.go`; 120 s by default), which the wait extends only once
an init container is running (`awaitRoleLauncher`, `packages/daemon/internal/runtime/sandbox/relaunch.go`;
"Liveness rules" below); a launch whose launcher has not connected by then fails and is counted. On
GKE Autopilot (measured 2026-10-16), an issue or controller pod usually waits for a node of its own
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)) and pulls the image cold there: 2m23s and
3m33s from creation to the first init container in the two launches measured (1m19s and 2m25s of
that waiting for the node, 56s and 58s pulling), and one cold pull of a new digest on an existing
node took 2m14s by itself — so a GKE deployment needs a larger `worker_boot_timeout_seconds` (for
example 300) to keep that provisioning-plus-pull time from being counted as a launch failure.
The image probe's own wait is derived from the same key: each attempt's budget is
`slow_command_timeout_seconds` (300 s by default), or, when `worker_boot_timeout_seconds` plus a
60 s margin is larger, that instead (`ProbeBudget`, `packages/daemon/internal/daemon/kubernetes.go`)
— so the capacity stop below (under "The image is probed before it publishes") always has room,
within one attempt, to see a probe pod's Unschedulable age pass the boot timeout. The boot log
names the derived bound when it applies, and so does `legion start --check-config`.
Oh My Pi's boot after it fits the registration deadline,
`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals` (360 s by default). The
stage 4a harness's `image-probe` and `root-ready` checks (`scripts/e2e/README.md`) are where a
launch of the published image on the production cluster is measured.

### The image is probed before it publishes

The daemon refuses to serve unless its OMP exposes `pi.agents` and actually loads `pi-legion` with
`pi-envoy` beside it, the two at one plugin interface version and without the pre-split package
(`packages/daemon/internal/daemon/bootgate.go`). The image build's final step runs `legion version`,
requiring the commit the workflow built, and the plugin's `dispatch` shim
(`/opt/legion/pi-envoy/bin/dispatch --help`, which needs its bundled `dist/dispatch.js` and the
image's Bun), then `legion probe-image`: the same two probes, run by the
daemon's own code, plus a third only the image runs — the session-storage probe, which prints
`session-storage=probed` on the OK line ([The image guard](#the-image-guard)) — with the Legion plugin held to
the daemon API contract (`legion.daemonApiVersion`), the Envoy plugin's interface held to the one the
Legion plugin speaks, and every task agent and skill Legion's prompts
name (`task(agent="…")`, `skill://…`) resolved by name through the same launch (the Legion plugin ships
`oracle`, `deep-worker`, `thermonuclear-deep-review` and `thermonuclear-code-quality`, and the
planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics and
`ce-simplify-code` with Legion's other skills in its `dist/skills`; the Envoy plugin ships the
`dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy` skills in its), so a build whose OMP or plugins are
broken fails instead of publishing. The build has none of the operator's model configuration, so it
leaves those agents' models unresolved (`--skip-agent-models`). It then checks every image-site row of
the capability table (`packages/daemon/internal/capabilities`) against the image, as a worker's Oh My Pi
would find each — `omp setup python --check`, the `chromium` on `PATH` (or the browser a
`PUPPETEER_EXECUTABLE_PATH` in the pod's env names) run with `--version`, the three language servers on `PATH`, the CodeGraph CLI with its plugin enabled in the
profile's lock, and `go`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq` and `ssh-keygen` on `PATH` with `go version`
running — and prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per
row, before the OK line:
`probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=skipped capabilities=checked model-fallback=on daemon-api-version=<N>`
(`model-fallback=on` is Oh My Pi's own default for `retry.modelFallback`: the build runs under no operator overlay, where a probe pod reads the operator's value; the mark reads `unread` when the probe could not read `retry.modelFallback` under the pod's Oh My Pi settings — the probe pod's log says why — and the daemon's report shows the row open).
An image row the image carries prints `present` with its evidence (`codegraph`'s: the CLI on PATH and
the plugin enabled in the profile's lock, which a pod's launch loads with extension discovery on). A live row prints
`reported` (``each session reports it at start; `legion state` renders it: <summary>``: the probe
proves nothing of it; [The deployment's capability report](#the-deployments-capability-report) says
what each session measures), a deployment row `reported` too (the daemon reports it from the
deployment's configuration), a withheld row `withheld` with its ruling, and an image row the image
lacks `missing` with why.
A build whose image lacks a capability fails, the probe naming every missing one. The daemon's Agent Sandbox runtime runs the same command in a probe
Sandbox, `legion-probe-<project>-<digest12>`, with its own contract, under the operator's pod, at every
boot, and requires `agent-models=resolved`: each agent's model resolves, with a working key, as the task
tool resolves a subagent's (`packages/daemon/internal/runtime/sandbox/probe.go`). It requires
`capabilities=checked` too: an image whose `legion` predates the capability check prints no such mark and
is refused naming that (`Succeeded without checking the capability list (its legion CLI predates the
check): build the image from this daemon's commit`), and a probe pod that exits 1 on a missing capability
is refused with its log tail, which names it (`capability <name> is missing: <detail>` under the table's
`missing` row; the stage 4a harness's `image-probe-capability-negative` check drives that with an operator
pod env `PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium`). A volume of the operator's pod block
(`runtime.kubernetes.pod.volumes`) that a `volume_mounts` entry mounts, naming a ConfigMap or Secret
that does not exist in the daemon's namespace, or asking a key it lacks, leaves the probe pod
`Pending` with the kubelet's `FailedMount` (`configmap "<name>" not found` or `configmaps "<name>" not
found`, `secret "<name>" not found` or `secrets "<name>" not found`, `configmap references
non-existent config key: <key>`, `references non-existent secret key: <key>`; the kubelet attempts no
volume that no container mounts, so an unmounted one produces no event and no refusal); the daemon
reads the pod's events once the pod is scheduled, at most
every 15 s (`mountRecheck`), and refuses on the first such event, with no retry, naming the probe pod,
the volume, the object the pod block gives it from and the kubelet's message — `the probe pod <name>
cannot mount volume "<volume>", which runtime.kubernetes.pod.volumes gives it from ConfigMap "<name>":
<kubelet message>` (or `Secret "<name>"`; a `projected` volume lists each ConfigMap and Secret source
joined with `and`; a volume the block does not name reads `the probe pod <name> cannot mount volume
"<volume>", which runtime.kubernetes.pod.volumes does not name: <kubelet message>`) — within one event
read (15 s plus the wait loop's 500 ms tick) of the kubelet's first `FailedMount`, minutes into the
boot rather than after the six attempts of the retry budget (about 71 minutes at the production
budget), so a daemon under its Deployment's startup probe refuses, exits and is restarted with its
usual back-off, each restart's probe refusing the same way in the log, until the object exists. A
container waiting `CreateContainerConfigError` whose message names such an object (one its
environment reads: `couldn't find key <key> in ConfigMap <ns>/<name>` or `… in Secret <ns>/<name>`, or
the not-found forms above) is refused the same way from the pod's status: `the probe pod <name>
cannot configure its container probe (waiting: CreateContainerConfigError), a ConfigMap or Secret it
reads being missing: <kubelet message>`. The `FailedMount` forms the kubelet retries itself and heals
(`failed to sync configmap cache: timed out waiting for the condition` and the secret form, `object
"<ns>"/"<name>" not registered`, `Unable to attach or mount volumes … timed out waiting for the
condition`) stay under the six-attempt budget, and the providers Secret, which every pod mounts, keeps
its own earlier rule and wording (`cannot mount the providers Secret …`); the stage 4a harness's
`image-probe-missing-volume-negative` check drives the refusal with a volume `operator-missing` from
a ConfigMap `<operator ConfigMap>-missing` that does not exist, mounted at
`/etc/legion-operator-missing`. The runtime keeps the line's `model-fallback`
mark for the deployment's capability report ([The deployment's capability report](#the-deployments-capability-report)). To run it yourself:
`docker run --rm ghcr.io/sjawhar/legion-worker@sha256:… probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models`
(both roots are required: the Legion and Envoy plugin roots a Sandbox pod loads the plugins from, so the probe loads them the same way; without
`--skip-agent-models` it also resolves each agent's model, which needs the operator's model roles).
A step of its own, before that final one, runs every toolchain command listed above as `legion`, from
the image `PATH` — the language servers (`gopls version`, `typescript-language-server --version`,
`tsc --version`, and `pyright --version` for `pyright-langserver`, which takes no `--version`), `go
version` with `go env GOROOT` required to be `/opt/go`, `gofmt`, `kubectl version --client`, and trixie's
`python3`, `curl`, `wget`, `jq --version`, `chromium --version` and `socat -V` included — the corepack shims fetching their shipped default pnpm and yarn
into a scratch directory the step removes. `ssh-keygen` takes no `--version`, so the step generates a
throwaway ed25519 key in that scratch directory and fingerprints it with `ssh-keygen -l`. It reruns only
when the toolchain or an earlier layer changes, so a commit that only rebuilds `legion` needs no package
registry.
To check a published image's toolchain end to end, a project Python install included:
`docker run --rm --entrypoint sh ghcr.io/sjawhar/legion-worker@sha256:… -c 'uv --version && node --version && npm --version && aws --version && kubectl version --client && go version && gopls version && python3 --version && jq --version && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.

### Pin by digest, never by tag

`legion.yaml` `runtime.kubernetes.image` accepts only `ghcr.io/sjawhar/legion-worker@sha256:…`. A tag is
mutable; the daemon must know exactly what it probed, so a tag reference is refused at startup with
`runtime.kubernetes.image must be pinned by digest (@sha256:…)` (`packages/daemon/internal/config/kubernetes.go`).

Where the digest is published:

- the job summary of every `Worker Image` run (Actions → Worker Image → the run → Summary);
- the body of the `legion-v<version>` GitHub release, under "Worker image", when `release.yaml` released
  `legion` in the same run (`gh release view legion-v<version> --json body -q .body`);
- `docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:<tag>` for a `sha-` or
  `<legion version>` worker-image tag.

Worker-image tags: `sha-<12 hex of the built commit>` on every run (on a pull request that is the
PR head, never the ephemeral merge commit); `<legion version>` only on `main` when the `legion_publish`
job released that version in the same run. Runs from any other ref publish the `sha-` worker-image tag
only and never touch a release. The package also currently holds `sha256-<image digest hex>` OCI
referrer indexes from earlier proof runs; each holds an attestation, not a worker image, so
inspecting one does not print a worker-image digest.

A tag does not say which run published it: a pull request run publishes the `sha-` tag of its head
too, when two runs build one commit the tag names whichever pushed last, and a pull request can edit
the workflow, whose token can push, to point a `sha-` tag at any digest already in the package. The
digest's GitHub artifact attestation does say. Every run except a pull request's ends
with the workflow's `attest` job, which attests the pushed digest with
`actions/attest-build-provenance` and stores the attestation with GitHub. Its Sigstore certificate
carries what the run's OIDC token says, which no workflow edit can change: its identity is the
workflow file at the ref it was read from, and its source ref and source digest are the ref and commit
the run started on. To accept a digest only when a run on `main` built it from commit `<C>`, the
commit its `sha-` tag names:

```bash
gh attestation verify oci://ghcr.io/sjawhar/legion-worker@sha256:… --repo sjawhar/legion \
  --cert-identity https://github.com/sjawhar/legion/.github/workflows/worker-image.yaml@refs/heads/main \
  --source-ref refs/heads/main --source-digest <C, all 40 hex digits> \
  --deny-self-hosted-runners
```

This human-facing command states the whole acceptance policy. The Legion release follower adds
`--format json` when it parses the verified certificate fields; that changes output only, not what
the command accepts:

- `--cert-identity` matches the identity exactly. `--signer-workflow` matches it only as a prefix, so
  it would also accept a workflow whose path merely starts with `worker-image.yaml`.
- `--source-ref` refuses a run started on another branch that calls
  `sjawhar/legion/.github/workflows/worker-image.yaml@main`: that run's identity names `refs/heads/main`,
  and its source ref names the other branch.
- `--source-digest` refuses an image a run on `main` built from another commit, such as an older
  `main` image a pull request run pointed `sha-<C>` at.
- `--deny-self-hosted-runners` refuses an otherwise matching attestation from a self-hosted runner.

A pull request run attests nothing. A pull request that edits the workflow to attest anyway gets
`refs/pull/<n>/merge` in both the identity and the source ref, and a run dispatched from another
branch gets `refs/heads/<branch>` in both, so the first two policy flags each refuse them.

The attestation is separate from the image's own BuildKit provenance, which the build keeps off
(`provenance: false`).

### How it is built — and the iteration rule

`.github/workflows/worker-image.yaml` builds on the GitHub-hosted runner with `docker/setup-buildx-action`
+ `docker/build-push-action` (the pair `release-envoy-listener.yaml` uses), layer cache in GitHub Actions
cache (`cache-from: type=gha`, `cache-to: type=gha,mode=max`), pushed with the workflow's own `GITHUB_TOKEN`
— no third-party builder, no project variable, no extra credential. It runs (1) from `release.yaml` after
the `legion_build` and `legion_publish` jobs on every `main` push that touches any file the image builds
from, (2) on every head of a pull request against `main` whose diff touches any of those files — building
the PR head and publishing `sha-` only — and (3) by `gh workflow run worker-image.yaml --ref <ref>` once the
workflow exists on `main`. The
files the image builds from are every context source `worker.Dockerfile` copies: the root manifest,
lockfile, patches and each root workspace's `package.json` (the frozen install); the packages it copies
whole — the Dockerfile (`packages/daemon/docker/**`), the two plugins and what they bundle (`packages/pi-envoy/**`,
`packages/pi-legion/**`, `packages/pi-shared/**`, `packages/envoy-client/**`, `packages/contracts/**`), the skills (`skills/**`) and the
prepack that stages them (`scripts/pi-plugin-prepack.sh`); the OMP pin
(`.omp-pin`); the whole Go module the image compiles `legion` from (`packages/daemon/**`) and its build
inputs; the context's `.dockerignore`; and the workflow itself. What a pod executes is part of the image's
behaviour — the command a Sandbox pod runs, the launch probes `legion probe-image` runs in the image's
final step and in the Go daemon's probe Sandbox, and every package they import — so a change to any of it
builds the image it is proven on, and a change that breaks the image fails on its own pull request rather
than merging and publishing nothing. The Go build inputs are `go.work`, `go.work.sum`, and `packages/envoy`
(copied whole for the `agent-secrets` binary the image also builds): the image compiles the Go `legion` at
`go.work`'s Go version, so a change that moves it past the build stage's Go fails on its own pull request
rather than in the next image build.
`.github/scripts/check-image-trigger-paths.sh` (the lint job of `pr-and-main.yaml`) parses the Dockerfile
and fails when trigger (1) or (2) misses a file it reads, so these lists cannot drift from it.
Trigger (2) is `pull_request`, not `push`: GitHub evaluates `pull_request` path filters against the whole PR
diff, so a later commit that touches none of those paths (a handoff, a docs fix) still gets the check and the
PR head never loses it; a `push` trigger filters on the pushed commits alone and would leave such a head
unguarded. The workflow's `packages`/`contents` permissions apply to same-repo pull requests (this
repository takes no fork PRs, whose token would be read-only). The `attest` job's `id-token` and
`attestations` scopes reach a pull request run only if it drops that job's `if:`, and its attestation
then names `refs/pull/<n>/merge`, which the verify command above refuses.

**The image is built only by this workflow, on the GitHub-hosted runner.** Never build it on a workstation
— no `docker build`, `docker buildx`, or `docker compose build`: an unrelated buildx job took the devbox
host to load 646 on 2026-09-12 and the Legion daemon with it (the CI runner is not a workstation). Iterate by
pushing the PR branch (trigger 2) or, once merged, dispatching (trigger 3); check the Dockerfile and workflow
statically (`hadolint`, `actionlint` where installed) and run `bun test` for the TypeScript. Pulling and
running the published image locally is fine. A failed build is retried with `gh run rerun <run-id> --failed`
(`--failed` keeps `legion_build`'s and `legion_publish`'s recorded outputs) or by pushing the branch again. A
whole-run rerun of a `release.yaml` call, or a `workflow_dispatch` of it while main's tip still carries a
`legion-v` tag, re-executes `legion_build` and `legion_publish`, which release that tag again: the image is
rebuilt, the `<legion version>` tag moves to the rebuild's digest, and the release body's "Worker image" names
that digest. `release-envoy-listener.yaml` is the exception to `--failed`: when its `release_publish` fails at
"Confirm the version" because another run took the tag first, only a whole-run re-run (`gh run rerun <run-id>`
without `--failed`) heals it, since `release_build` must compute the version again and re-stamp the binaries.

The build has no prerequisites outside this repository. After the first push there is one human action: if
the `legion-worker` GHCR package came out private, an anonymous `docker pull` fails until its visibility is
set to public — a package-settings action on GitHub with no API.

### Per-deployment toolchains layer on top

The base image carries Legion's own tools and the generic toolchain listed above, and nothing in it is
specific to one repository, so a Go, Python or Node repository runs on the published image as it is. A
deployment whose repositories need more than that (a system library, another language, a different Node
or Go line) builds its own image in **its** repo:

```dockerfile
FROM ghcr.io/sjawhar/legion-worker@sha256:…
# deployment toolchain here
```

and pins `runtime.kubernetes.image` to *that* image's digest. The runtime sets every container's `PATH`
itself (the image's `PATH` with its own directories in front), so an `ENV PATH` in the derived image never
reaches a pod: put the added commands in `/usr/local/bin` or `/usr/bin`, and outside `HOME`, where a
pod's volumes would shadow them. What the deployment adds churns on its own schedule, not Legion's
release schedule, and the deployment's repo owns its reproducibility.

### Entrypoint

The image's `ENTRYPOINT` is `["legion"]`, so `docker run --rm <image> version` and
`docker run --rm <image> probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models`
work. A pod never relies on it — the Kubernetes runtime sets every container's `command` explicitly (see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod) below). To run anything else in the image,
override it: `docker run --rm --entrypoint sh <image> -c '…'`.

## Session store

A Legion agent's conversation — its OMP session — is by default a JSONL file under `HOME`, so a pod that
loses its disk loses the conversation. The OMP fork the daemon pins (`OMP_FORK_PIN`) can store the session
in a SQL database instead, selected by two environment variables the pod (or a tmux pane) carries. Under
the Kubernetes runtime the daemon sets them for you when `legion.yaml` says
`runtime.kubernetes.session_store: postgres` ([Selecting the store](#selecting-the-store) below); the
tmux runtime has no such setting.

### The two variables

| Variable                   | Settings equivalent (`config.yml`) | Value                                                                        |
| -------------------------- | ---------------------------------- | ---------------------------------------------------------------------------- |
| `OMP_SESSION_STORAGE`      | `session.storage`                  | `sql`; unset (or `file`) is today's JSONL tree                               |
| `OMP_SESSION_SQL_DSN_FILE` | `session.sql.dsnFile`              | absolute path of a `0600` file whose trimmed contents are one `postgres://…` URL |

The environment wins over the setting, the same way `OMP_AUTH_BROKER_URL` wins over `auth.broker.url`. The
connection string is delivered as a file, never as an environment value or an argument — the same rule as
`DISPATCH_TOKEN_FILE` and every other `*_FILE` variable the daemon writes. The dialect follows the URL
scheme; Legion delivers Postgres.

### What Oh My Pi creates

On the first start with `sql`, Oh My Pi's own `SqlSessionStorage` runs `CREATE TABLE IF NOT EXISTS
omp_session_files` (columns `path TEXT PRIMARY KEY`, `content TEXT NOT NULL`, `mtime_ms BIGINT NOT NULL`,
`title TEXT`, `title_source TEXT`, `title_updated_at TEXT`) and warms its index from every row. Legion
manages no schema and no migration: the table is Oh My Pi's. One row is one session; `path` is the same
string the session would have had as a file, `content` is the JSONL transcript.

### Resume

`omp --resume=<row path>` — the value is exactly the session path the extension reports to the daemon at
`/worker/started` as `ompSessionFile`, and in Postgres it is the row's `path` key:
`<sessions root>/<encoded cwd>/<timestamp>_<session id>.jsonl`. The second child keeps that `ompSessionFile`
field and carries the row path in it unchanged — no rename, no wire or contract change. The key embeds the
home-relative sessions root, so the resuming pod must carry the same two `OMP_SESSION_*` variables and the
same `HOME` (and `OMP_PROFILE`) as the pod that wrote it. Nothing else changes: the daemon already relaunches
a dead worker with `--resume=<recorded session file>`.

### Refusals

Every refusal is exit 1 with the message on stderr, and none falls back to file storage:

| condition                                                         | behaviour                                                                                                                                                                              |
| ----------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| both variables unset, settings at their defaults                  | JSONL files exactly as before; nothing is logged                                                                                                                                       |
| `OMP_SESSION_STORAGE=sql`, no file named by the variable or the setting | refuses, naming both `OMP_SESSION_SQL_DSN_FILE` and `session.sql.dsnFile`                                                                                                        |
| the named file is missing or unreadable                           | `OMP_SESSION_SQL_DSN_FILE names <path>, which could not be read: <reason>` (or `session.sql.dsnFile names …` when the setting supplied it)                                            |
| the named file is blank after trimming                            | `… names <path>, which is empty`                                                                                                                                                       |
| the file's contents are not a connection URL the driver accepts (for example the libpq keyword form `host=… user=… password=…`) | `… names <path>, but its contents are not a connection URL the database driver accepts` — the driver's parse error is not printed, since it embeds the whole string; never the contents |
| the database is unreachable or refuses the connection             | `… names <path>, but the session database could not be opened: <driver error> (<code>)` — for a closed port `Connection closed (ERR_POSTGRES_CONNECTION_CLOSED)`; the driver's error, never the connection string |
| any other `OMP_SESSION_STORAGE` value                             | `OMP_SESSION_STORAGE is "<value>"; expected "file" or "sql"`                                                                                                                           |
| the running Oh My Pi build predates the setting                   | the variables are ignored and the session stays on files — caught before it can happen: the session-storage launch probe (`verifySessionStorageSetting`, `boot-probes.ts`) runs inside the worker image through `legion probe-image` (see [The image is probed before it publishes](#the-image-is-probed-before-it-publishes)), fails on such a build so the image never publishes, and on a passing image prints `session-storage=probed` on the command's OK line; under `session_store: postgres` the daemon requires that token in the probe pod's output — it never probes a host OMP for it |

### What stays on the pod's disk

Only the transcript moves. Tool artifacts, image blobs and `models.db` stay under the agent directory
on local disk (`~/.omp/profiles/legion/agent/…`), and OMP's logs under the role's state home
(`/home/legion/.local/state/<role>/omp/profiles/legion/logs`, [Anatomy of a Sandbox
pod](#anatomy-of-a-sandbox-pod)); `.legion/` handoffs live in the repository. A pod that dies loses
those local files as it does today — the conversation it does not. The workspace's CodeGraph index,
`.codegraph/` in the workspace on the issue's own volume (185 MB for this repository), is built by a
role's shim after its Oh My Pi starts and outlives the pod with the workspace.

### The extension under SQL storage

The Legion plugin (`@sjawhar/pi-legion`) recognises a `task` subagent inside a Legion worker without looking
for the parent's transcript on disk: its claim session records which session it bootstrapped on the interface
the two plugins share, and a later session start in the same process with a different transcript path is a
subagent (the check is `@legion/pi-shared/subagent-session`, which both plugins run;
`packages/pi-envoy/AGENTS.md`, the subagent convention, and `packages/pi-shared/AGENTS.md`).
Nothing in either plugin reads the two variables, and neither needs any other change for SQL storage.

### Selecting the store

Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping:

```yaml
runtime:
  kubernetes:
    session_store: postgres        # pvc (the default) keeps sessions on the issue's own volume
    session_dsn_secret: SESSION_DSN  # the providers-Secret key that holds the connection URL
```

`pvc` keeps each conversation a file in the `sessions` directory of the issue's volume. `postgres`
keeps every pod agent's conversation, each role's and a daemon-launched controller's, in Oh My Pi's
table. The daemon never holds the connection string: put it in the providers Secret
(`legion-<project>-providers`) under the key `session_dsn_secret` names, as one `postgres://…` URL.
Every issue pod, the controller's pod and the image probe mount that key read-only at
`/var/run/legion/providers/OMP_SESSION_SQL_DSN`, whatever the key is called, so a Secret without it
refuses boot at the image probe, naming the Secret and its keys
(`internal/runtime/sandbox/manifest.go`, `providers`).

With `postgres` on, every generation a launcher starts gets two variables in its start command
(`mainEnvironment`): `OMP_SESSION_STORAGE=sql` and
`OMP_SESSION_SQL_DSN_FILE=/var/run/legion/providers/OMP_SESSION_SQL_DSN`. The pod baseline
(`internal/podsafety`) keeps them, where it would otherwise set `OMP_SESSION_STORAGE=file`, and the
worker shim, seeing the pointer in its environment, never exports the URL file into Oh My Pi's
environment. Neither variable may be the operator's (`pod.env`) or a provider key's, and neither
may `OMP_SESSION_SQL_DSN`, the name the URL file is mounted under (`CheckPod`). Oh My Pi
creates and migrates its own two tables, `omp_session_files` and `omp_session_files_parts`, in the
`public` schema when it starts; Legion manages no schema. So the deployment's login needs to create
tables there: since Postgres 15, `CREATE` on the database is not enough, and the create is refused
with `permission denied for schema public`. Grant `USAGE, CREATE ON SCHEMA public` to the login
(measured on Postgres 16.15: `CONNECT, CREATE ON DATABASE` alone is refused, the schema grant
creates the table), or make the login the schema's owner.

Every agent in the deployment can read that URL file, as every pod mounts it, and so can read and
rewrite every conversation in the table, the controller's included, with the login's rights. That
is the trade LEGION-654's spec accepts for sessions that outlive their volumes: give the login no
rights beyond the two tables' database, and treat its URL as a credential every agent holds.

A resume under `postgres` is held to the table, not the volume. A role's launcher looks the recorded
session up in the table through the same URL file before it starts the child
(`internal/launcher`, `resumable`), and refuses the start when the table holds no such session
(`resume session <path>: the session table holds no such session`): a launch failure, never a fresh
agent. A database it cannot reach is asked again with a backoff (half a second, doubling to four
seconds) for up to 30 seconds, the URL file read afresh each time, and only then refuses the start.
Neither `workspace-init` looks for session files: an issue pod is not
told `LEGION_EXPECT_ISSUE_VOLUME` for a session, and the controller's pod is not told
`LEGION_RESUME_SESSION_FILE`. So a claim resumed on a new volume (an issue closed as `done` or its
tree drained, its volume deleted, then re-admitted) provisions its workspace from the issue's pushed
branch (`legion/<issue>`, or the repository's default branch when none was pushed) and continues
its own session; anything it had not pushed is gone with the old volume. A child issue re-admitted
as a root of its own keeps its roles' sessions under either store (`Machine.Retree`): under
`postgres` they are in the table, and under `pvc` on the issue's own volume, which its Sandbox
keeps across the move while the old tree lingers ([Volume retention](#volume-retention)).

Such an agent is told so before its next turn. Provisioning records when it created a workspace, in
the workspace's own `.jj/legion-created`, which jj never snapshots (`workspace.RecordCreated`). At
every resume the role's launcher compares that record with when the session was last written: the
row's `mtime_ms` under `postgres`, which Oh My Pi moves on every write and `legion sessions import`
sets to the copied file's, and the file's own under `pvc`. A workspace created after that write
starts the generation with `LEGION_WORKSPACE_RECREATED=true`; every other generation gets `false`,
whatever the container's environment says, and so does a resume into a workspace with no record
(one provisioned before the record was kept). A record that is empty or not one RFC 3339 instant,
which an init container killed mid-write can leave, decides only that notice: the launcher logs it,
takes the workspace as not recreated, and resumes. At session start a Legion session's plugin reads
the variable and saves one message to the session, ahead of the next turn whatever starts it (a
task, an Envoy event): `Your workspace was recreated since your last turn: it holds what was pushed
to legion/<KEY> (the repository's default branch if nothing was), and anything you had not pushed
is gone. …` (`packages/pi-legion/src/workspace-recreated.ts`, a steer that starts no turn), with an
id of its own process in the message's `details`. A prompt, the daemon's next task or a person's
direct message that pi-envoy delivers as a user turn, first recovers a failed last turn, and Oh My Pi
recovers an empty `length` stop by moving the branch back to that turn's parent, which takes the
saved message off the branch with it. The plugin's `before_agent_start`, which runs after that
recovery on either path, sends the message again when the branch no longer holds this process's
copy, a notice an earlier recreation saved not counting, and the turn saves it after the prompt.
The recovery leaves the first copy in the process's live context, so that process's requests keep
its first copy alone, and an earlier recreation's notice stays where the history put it. An Envoy
card, a custom message sent to start a turn, goes through neither the recovery nor
`before_agent_start`, so its branch keeps the saved message. Measured on Oh My Pi
`18.8.3-sami.20261009-045702`, with resumed sessions whose last turn was an ordinary reply, an
empty `length` stop, or an empty `length` stop in a session already told at an earlier recreation
(file storage for all three, and SQL storage for the first two): the notice is in the stored
session before the task arrives, the task's first model request carries this process's copy once,
just after the history and before the task, and after the task the stored branch holds it, so a
process lost in that turn resumes with the notice already there. With the plugin before the
re-send, the `length` case's stored branch had lost it; with the plugin before the id, so did the
case of a second recreation. A `task` subagent is never told.

The variables are a generation's, but the URL file's mount is the pod's. A pod whose providers
volume projects another session store than a pod created now would — one created before
`postgres` was turned on, or after it was turned off — holds a move (`movedSessionStore`,
`internal/runtime/sandbox/addresses.go`): the next relaunch of any of its roles replaces the pod,
as it does a pod that dials a moved worker stream. The image probe's container is told
`OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the providers directory as
the shim does, never exports the URL into Oh My Pi's environment.

A resumed session also restores the model it last used, and under either store the Oh My Pi
`.omp-pin` names refuses, in `--mode rpc`, to resume one whose model the profile no longer has
(`Could not restore model <provider>/<model>`, exit 1). So a change to the pods' model
route (`runtime.kubernetes.pod`, `provider_keys`) that drops a model fails the launch of every agent
resuming a session on it, `pvc` or `postgres`, until the route offers that model again. Keep the
route unchanged across the switch below in particular: there every session outlives its volume.

### Copying file sessions before turning it on

This runbook, and `scripts/sessions-import-pods.sh`, copy only from the release before issue pods,
`legion-v10.0.0`, whose every role runs in a Sandbox of its own and keeps its session on its tree's
volume; the script refuses any other release by name. A deployment on `legion-v10.1.0` to
`legion-v10.2.x`, which runs one pod per issue, has no copy path: its switch to `postgres` is a
drain at a pushed boundary, every tree closed, and the sessions on those volumes are lost. Nor is
there a copy path back: switching from `postgres` to `pvc` starts every agent fresh, as nothing
copies a row out to a file.

On `legion-v10.0.0`, a deployment whose sessions are files on its tree volumes copies each one into
the table once, while those volumes still exist, after the last write to any of them and before any
agent writes the same session under SQL storage. The order below is what keeps a copy current:
nothing checks, at a resume, that a row still matches the file it was copied from, so a session
written after its copy would resume from the older row without a word. Do not drain the deployment
by lowering the linger instead: a closed tree's cleanup deletes its volume, and with it any session
not yet copied. Steps 1 to 5 below take the place of steps 1 to 3 of [Upgrading a deployment with
running trees](#upgrading-a-deployment-with-running-trees): its steps 1 and 2 drain every tree and
so delete its volume, and its step 3 runs before step 1 here (below). Step 6 here hands back to that
section.

**A daemon-launched controller is cleared first.** Under `controller: daemon` the earlier release
resumes a suspended controller at its next look, within a minute (`keep` in
`internal/daemon/controller.go` at `legion-v10.0.0`), so the controller cannot be held still for
the copy. Clear its claim before step 1 with step 3 of the upgrade section. Its Sandbox goes, and
its volume and session go with it, so step 5 marks the claim lost and the controller this release
launches starts a fresh session.

1. **Stop every writer, keeping the linger as it is.** Tell every agent to push its work, then
   suspend every claim (`legion claims suspend`) until each tree is at a pushed boundary: no pod
   running and no claim recording a locator (`legion claims list --json`, every `locator` absent).
2. **Save the claims and stop the daemon at once.** `legion claims list --json
   --operator-token-file <file> > claims.json` (the daemon of any release prints the list this
   command reads), then scale the daemon to 0 straight away, so nothing launches, resumes, or
   cleans up a tree from here on.
3. **Copy every tree.** Where a tree's volume is mounted and the session database is reachable, run,
   for each tree the list names:

   ```sh
   legion sessions import --dsn-file <url file> --claims claims.json --tree <ROOT ISSUE> --tree-volume /legion
   ```

   `scripts/sessions-import-pods.sh <claims.json> <namespace> <worker image@sha256> <project>
   <url secret> <url key> [--context <context>]` runs it for every tree the list names: one pod
   of the worker image per tree, under gVisor, mounting the tree's volume read-only at `/legion`
   and the URL key, printing the import's lines, then deleted. It finds each tree's volume and root
   Sandbox by `legion-v10.0.0`'s labels (`legion.dev/project`, `legion.dev/role=architect`,
   `legion.dev/tree` and `legion.dev/issue`), not by name, and schedules the pod as the root
   Sandbox's pods were: its pod template's node selector, tolerations and priority class. The pod
   runs as the namespace's `default` ServiceAccount with no token
   (`automountServiceAccountToken: false`), never as the Sandbox's: it needs no identity, and a
   namespace admission policy may let only the agent-sandbox controller create pods as the worker
   ServiceAccount. Before anything it refuses a project whose Sandboxes are issue pods (a later release),
   one with a Sandbox not `Suspended`, whose pod could still start, and one with a pod that has not
   ended (any phase but `Succeeded` or `Failed`): each would be a writer the copy misses. It deletes
   a pod an interrupted run left before creating one, reports a pod that fails at once, exits 1
   when any tree's import did, names a tree whose volume or root Sandbox is gone, and names every
   claim that records a session and belongs to no tree, the cleared controller's among them, which
   step 5 marks.

   For each claim of that tree it reads the recorded session file from the volume (its path below
   the agents' sessions directory, below `<tree-volume>/sessions`; without `--tree-volume` it reads
   the recorded path itself) and writes it as one row keyed by that recorded path: the whole file
   as `content`, `byte_len` its size, `mtime_ms` the file's, no title and no parts, which is how Oh
   My Pi reads a row an older release of it wrote. It creates the two tables with Oh My Pi's own
   statements when the database has none. It prints one line per claim of the tree — `copied`,
   `copied before (identical …)`, `recorded no session`, `failed` (a file the volume does not hold)
   or `refused` (the table already holds that session with other content, which it leaves as it
   is) — then a `missing` line for every claim of the whole list, any tree's, whose recorded
   session the table does not hold, then a count of each. It exits 1 on any `failed` or `refused`,
   on a `missing` of the claims it was asked to copy (another tree's is reported only), and when
   `--tree` selects no claim of the list, which is a mistyped key.
4. **Copy every tree again.** Each run must print only `copied before` or `recorded no session`
   for its tree's claims, and its `missing` lines may name only claims step 5 is to mark: one
   whose volume was gone before step 3 (the cleared controller's, or a claim of a tree closed
   earlier), or a `failed` you cannot fix. A `refused` here means a writer was still running: the
   session grew on its volume after step 3 copied it, and the import never overwrites a row it
   already holds, so it refuses the file it now finds. Going on would resume the agent from the
   older row and lose every turn after it. Go back to step 1 instead: scale the earlier daemon back
   up, suspend whatever runs, save the list again, and scale it to 0 at once (step 2); then delete
   that row and its parts (`DELETE FROM omp_session_files_parts WHERE path = '<path>'; DELETE FROM
   omp_session_files WHERE path = '<path>'`) and copy again. Nothing writes a row under SQL storage
   before the switch, so in this runbook a `refused` has no other cause.
5. **Mark the claims whose sessions are gone.** A claim recording a session no volume holds — its
   tree's volume already deleted, or a `failed` line you cannot fix — would fail every launch
   under SQL storage, where a file store starts it fresh. Run this step even when step 4 printed
   no `missing` line: it is where the claims list is checked against the stopped daemon's own
   database (below), which no other step does. With the daemon still stopped, mark each lost in
   the daemon's own database:

   ```sh
   legion sessions mark-lost --dsn-file <url file> --claims claims.json --daemon-dsn-file <file holding the daemon's postgres_dsn>
   ```

   It copies nothing. Its statements are plain SQL against the daemon's `claims` table as
   `legion-v10.0.0` left it, never this release's store, which would migrate the database before
   the dump the rollback restores. It first reads every claim of the list's project from the daemon's
   database, the record once the daemon is stopped, and refuses, marking nothing, when one records
   a session file the list does not give it: a claim the earlier daemon launched, or relaunched
   onto a new session, after step 2 saved the list. It names each such claim; go back to step 1 as
   step 4 says (scale the earlier daemon up, suspend, save the list again, scale it to 0).
   Then, for every claim of the list whose recorded session the table lacks, it clears the
   claim's session and session file and marks its workspace lost, as the daemon marks a claim
   whose volume was lost, but only while the claim still records that session file. It prints
   `marked lost` or `failed to mark lost` per claim, then a count, and exits 1 on any failure.
   Each such claim starts a fresh session, a workflow role's in a workspace recovered from its
   issue's branch.
6. **Only then delete the old Sandboxes and their volumes**, then finish with steps 4 to 6 of
   [Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees): its
   two checks must come back empty, then the dump, then the rollout, with `session_store:
   postgres` and `session_dsn_secret` set in it.

### The image guard

An Oh My Pi built before the `session.storage` setting ignores the two variables and keeps sessions on
files without a word — the one silent fallback this setting must never allow. The check lives inside the
image: `legion probe-image`, which the image build runs before it publishes, starts the image's own Oh My
Pi with a nonsense `OMP_SESSION_STORAGE` value and passes only if it refuses, then prints
`session-storage=probed` on its OK line. The daemon's image probe passes only an OK line that carries
its daemon API contract, which a `legion probe-image` of this release prints only after that probe
passed, under either store. The daemon never probes a host Oh My Pi for this: pods run the image's
build, not the host's.

## Kubernetes runtime: the Go daemon on Agent Sandbox

Under the Go coordinator, `runtime: kubernetes` runs every issue as one Agent Sandbox: an
`agents.x-k8s.io` `Sandbox` (kubernetes-sigs/agent-sandbox, LEGION-206) named for the issue,
`legion-<project>-<issue>`, whose one pod runs under gVisor on the Legion pool. The pod holds the
two init containers and six role containers — `architect`, `planner`, `implementer`, `tester`,
`reviewer`, `merger` — each of which runs `legion launcher`, a supervisor with no workflow policy
that authenticates to the daemon's worker stream with its role's own token and starts or stops
that role's `legion worker-shim` and Oh My Pi when the daemon tells it to. Every role of the issue
shares the issue's checkout, the issue's own volume, the sessions directory on it and the pod's network;
each role keeps its own state directory, agent-secrets key, GitHub App credential and launch credentials. A role's Secret holds
its launcher token, projected into that role's container alone and bound to the pod's uid, and the
two gh files its `gh` and `git` read the role's App token from
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)); the
daemon accepts a launcher only for that role, that pod and that token. Each generation's boot token
and launch credentials (the Envoy and Dispatch bearers, a spec's secrets) travel in the authenticated
start command, and the launcher writes them owner-only and exclusively into a fresh `g<generation>`
directory of its role's memory-backed private directory, removing anything already there without
following it, and removes that directory when the generation ends: a role started in a running pod
never waits on the kubelet to refresh a Secret, and no credential is in an argv, an environment
value, a log or an error. The role's own agent runs as the same user in the same container, so it
can read its own generation's credentials, as a pane can; it cannot read another role's. A role
process is addressed by the pod's uid, its
container and its generation (the incarnation `<pod uid>/<generation>`): in a healthy running pod,
starting, suspending or recovering one role never restarts the pod or touches another role. A new generation of a role
in a running pod is a new child of its launcher. A pod is replaced after it dies, when it was not
made by this runtime, or when a closed issue is re-admitted after suspension
(`packages/daemon/internal/runtime/sandbox`).

Releasing a role ends only its process. The issue owns its Sandbox, its role Secrets, its `-boot`
Secret and its volume: the Sandbox's one `volumeClaimTemplates` entry, `issue`, from which Agent Sandbox
creates the PVC `issue-legion-<project>-<issue key lowercased>` (`IssueClaimName`,
`internal/runtime/sandbox/names.go`) with the Sandbox as its owner; each Sandbox carries the tree's
`legion.dev/tree` label, which is how the tree's cleanup finds them.
Each tree also has one durable lifecycle record (`tree_lifecycles`, keyed by the normalized project
token and the tree): an epoch that is open, cleanup-reserved, or cleanup-confirmed, and the
authority that opened it. Workflow admission opens a root's epoch in the fact that gives it its
slot; the authenticated operator spawn of a root architect opens an operator tree's. Every claim
binds the open epoch in the same short transaction that first writes it, and every spawn, resume or
retry binds again before it launches, all under the fact path's one global serializer. A reserved
epoch refuses those writes with a named wait that keeps the start's outbox row and charges no
launch failure; a confirmed epoch admits nothing until a fresh root admission opens the next one.

A workflow close or withdrawal queues a separate `issue_suspend` effect for each affected issue,
after its per-role stops. The effect checks both workflow generations, later starts, unfinished
stop effects and every stored role, including a launch not yet present in the runtime's watch.
A held turn, unfinished or uncertain launch, or Kubernetes error keeps the effect pending;
a superseded close finishes without acting. Once the roles have stopped, the daemon sets that
issue's Sandbox to `Suspended` and waits for its pod to disappear. A child issue set `done` is then
released at once: its stops were `issue_close` supervise rows, which retire each of its claims with
the session it recorded dropped, and once every claim of the issue has retired the effect
foreground-deletes its Sandbox, the volume it owns with it (`SuspendIssue` with release,
`internal/runtime/sandbox/issue_suspend.go`), logging `sandbox runtime: released the closed issue's
Sandbox and the volume it owned`; the parent's Sandbox is another issue's and is never touched, and a
later `todo` starts the child fresh on a volume of its own. For every other close — a child set
`backlog`, `icebox` or `triage`, or a root leaving the workflow with its tree — the Sandbox, the
issue's volume and the recorded sessions remain until linger cleanup. A daemon restart retries the
stored effect. Re-admission during linger reuses those resources and resumes the recorded sessions;
the issue pod's launch turn orders a concurrent resume after any suspension already in flight.

Linger expiry reserves its tree only while the root still lingers at the close's generation, so
a re-admission that committed first fences it; an operator close reserves after it authenticated
the root close. Either reservation comes before the census, which then reads every stored claim of
the tree, including one that persisted before the reservation but has not launched yet, and
deletes nothing until all of them retired. A reservation that is not confirmed resumes on the
next attempt, even after a re-admission, and stays the new start's wait until API confirmation:
every close row of the tree, however stale its generation or the linger it closed, still retires
its claim and drives that cleanup before the checks that finish a stale close apply. A launch
checks its tree's epoch again just before it records `launching` and calls the runtime, so a
reservation committed after the claim bound refuses it there, with the same uncharged wait. Every
lifecycle step takes the global serializer before it reads the lifecycle row.

Once every claim of the tree has retired, the runtime deletes the tree's Sandboxes, found by
listing them from the API by the tree's label, in any order (`CleanupTree`,
`internal/runtime/sandbox/cleanup.go`): each delete carries the listed object's UID and
resourceVersion plus `foreground` propagation and is awaited until the API no longer has it, the
listing is read again until it holds none, and a delete the API refuses because the Sandbox changed
since the listing lists again after a short wait. A Sandbox whose home is another tree
(`Store.IssueSandboxHome`: a child whose hand-over is pending, or one carried into a tree of its own
that it is in or can still be admitted into) is handed over instead of deleted: under the pod's
launch turn, one patch fenced to the listed uid and resourceVersion relabels it for the issue's tree
and suspends it, keeping its volume and the sessions on it (`handOverSandbox`), so the new tree's
first launch finds it its own whenever that launch comes. A patch the API refuses because the
Sandbox changed since the listing (its failed `test` operation, answered 409 or 422) lists again
too. Agent Sandbox v1.0.3 creates each Sandbox's PVC
with that Sandbox as its controller owner and `blockOwnerDeletion: true`; foreground deletion keeps
the owner visible until Kubernetes garbage collection deletes that blocking dependent. The restricted
daemon has no PVC API verb, so a Sandbox's absence after its foreground delete is the API's
confirmation that its volume is gone; it does not read or delete a PVC. The live runtime proof must
observe the actual PVC owner reference and its absence after foreground deletion, rather than infer
that result from labels or a generic garbage-collection rule.

An operator-created tree has no workflow record: its stored operator authority, not a zero or
sentinel generation, selects the operator cleanup entry point, which then applies the same
cleanup without manufacturing a workflow issue. A tree closed before any of
its Sandboxes was created lists none and confirms its reservation without deleting anything. A root
that left admission's waiting line before a slot opened its tree (labelled `todo`, then set to
`backlog`, `icebox` or `done`) lingers as any root does, but its tree never had a lifecycle and
holds nothing a cleanup releases, so its linger's close rows reserve nothing and finish.

A child of a closed tree re-admitted as a root of its own keeps its key, so its claims and its
Sandbox's name. Its old tree's cleanup does not wait on a claim of it that runs nothing; each of
its claims is re-pointed to its new tree before its first start there, and binds that tree's epoch;
and its root launch keeps the Sandbox its old tree left, which owns the volume holding the
issue's clone, workspace and its roles' sessions: the Sandbox is relabelled for the new tree
(`ensureSandbox`, `internal/runtime/sandbox/relaunch.go`) and the sessions resume. A role still in a
turn when the old tree closed holds that close's suspension until the turn ends, and the carry's new
generation finishes the close's own suspend row without acting. So no start of the new tree launches
anything while a claim of the issue still runs a process in another tree: the start suspends that
claim itself, as the daemon and keeping its session, and waits on its backoff, charging nothing, until
the claim has stopped (`stopElsewhere`, `internal/daemon/outbox.go`). No close of the old tree
reaches that claim, so its stop is bounded where that close would have ended the turn, at the old
root's linger expiry (`linger_hours`): a turn that ends within the linger ends on its own, and one
still running at the expiry is stopped there. Once the old root no longer lingers (it was reopened,
say), there is no close left to wait for, and the stop falls back to the stop timeout
(`worker_stop_timeout_seconds`) from then (`carriedStopDeadline`): a role still in a turn is stopped
that long after, losing only the tool call in flight. Either way the claim keeps its workspace, the
issue's branch and its session, which the new tree's start resumes with its phase's resume task. A
turn that hangs inside the linger is the operator's to stop sooner: `legion claims suspend` stops it at the
stop timeout (`worker_stop_timeout_seconds`) from then and makes the hold the operator's, so the new
tree's start of that role waits for the operator, with the claim already re-pointed to the new tree,
and `legion claims resume` relaunches its session there. A Sandbox the old tree left `Running`, none
of whose claims runs any more, is relabelled and suspended in one patch fenced to the uid and
resourceVersion it read, so the launch waits its pod out; while a claim of the old tree still runs,
the launch is refused, naming it.
That holds whether the new tree's start or the old tree's cleanup comes first: a cleanup that finds
the issue already belongs to another tree hands its Sandbox over rather than deleting it (above),
and the orphan sweep keeps a Sandbox while its home is the tree it is labelled with. A Sandbox's home
ends when its issue can no longer be admitted into that tree. For a tree admission has not opened,
a child set to `backlog`, `icebox` or `done` leaves the waiting line and lingers like any root, so
its Sandbox stays until its linger expires; one whose label is taken off leaves the line with no
linger, so its Sandbox goes at the next sweep. Taken out by its label, once it is `todo` with the
label again it waits in the line and is admitted into the phase it stood in, continuing its run on a
new volume from the issue's branch if the sweep has taken the old one: under `session_store:
postgres` its roles' sessions continue from the table, and under `pvc` they went with the volume and
start fresh ([Volume retention](#volume-retention)). Taken out by a status, it comes back as any
root does after its linger: a new generation at `admitted`, its design gate, handoffs and pending
READY cleared (`ClearTreeGeneration`), its architect starting over from the issue's branch. A
hand-over fence counts only while the child's record still names the tree the fence was written
for, and a hand-over Dispatch refuses permanently drops its fence with its row (`finishRefused`,
`internal/daemon/outbox.go`). Past that the Sandbox is an orphan, which the sweep deletes with its
volume, fenced and logged. Only a child re-admitted after its old tree's cleanup, one that had left
the workflow before the close (a parked child the close does not hand over), finds its Sandbox and
volume gone; its re-pointed claim still names its session (`Retree`,
`internal/supervise/lifecycle.go`, keeps it: the supervisor knows nothing of Sandboxes, and a check
there would race the cleanup). The launch then expects the volume to hold the clone
(`LEGION_EXPECT_ISSUE_VOLUME`), `workspace-init` exits 3 on the fresh volume, the
daemon logs `the issue's volume was lost` once and relaunches the role as a fresh session on the new
volume ([Volume retention](#volume-retention)): the re-admission runs, from the issue's branch, with
the old sessions gone — the same path a root re-admitted after its own tree's close takes.

Before the daemon opens its store, so before any schema write, image probe or reconcile, it checks
that Agent Sandbox is installed and refuses a namespace that holds a Sandbox of its project whose pod
is not exactly the launchers its labels name — six for an issue's (`legion.dev/issue`), the
controller's alone for the project controller's (`legion.dev/role=controller` with no tree) — or that
owns no volume claim template, or one not named `issue`, naming every such Sandbox, each with its
reason, and how many there are. That catches every per-claim Sandbox of the layout before issue
pods, running or suspended, the controller Sandbox a `controller: daemon` daemon made before the
controller ran in a launcher pod (`role=controller`, one `worker` container), and every Sandbox of
the tree-volume layout before per-issue volumes (an issue Sandbox that mounted its tree root's
volume, that root's Sandbox with its `tree` template, or that layout's controller Sandbox). Once the
store opens and before it migrates, it refuses a claim that still records a Sandbox locator of the
layout before issue pods (no pod uid, container or generation), that earlier controller's claim
included. A deployment of an earlier layout clears all of it before it upgrades
([Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees)).
The daemon runs on a host its pods can reach and serves the worker stream they dial. The controller
is `legion controller start` on the operator's machine, or, under `controller: daemon`, a pod of its
own with one launcher that the daemon launches ([The controller](#the-controller)).

### Upgrading a deployment with running trees

This section is for a deployment that runs the release before issue pods (`legion-v10.0.0`), whose
every role runs in a Sandbox of its own. Issue pods (LEGION-462, sjawhar/legion#1752) and per-issue
volumes (LEGION-632) land in this one release, so one drained cutover takes a deployment from
per-claim Sandboxes to independent issue pods; a deployment that ran an issue-pod build on one tree
volume (a build of #1752's branch before this release) drains the same way, and its Sandboxes are
refused the same way. A daemon at this release refuses to boot while either layout's work is still
in place:

- **A per-claim Sandbox** in its namespace, running or suspended, is refused by the census before
  the store opens (`rejectLegacyIssueSandboxes` in `internal/runtime/sandbox/sandbox.go`, run by
  `run` in `internal/daemon/daemon.go` before `store.Open`).
- **A Sandbox of the tree-volume layout** — one that owns no volume claim template (an issue
  Sandbox that mounted its tree root's volume) or one whose template is named `tree`, not `issue`
  (that root's own Sandbox, or that layout's controller Sandbox) — is refused by the same census
  (`treeVolumeLayout` in `sandbox.go`), each named with its reason: `Sandbox <name> owns no volume;
  it is of the tree-volume layout before per-issue volumes, so drain and remove it as
  docs/kubernetes.md's "Upgrading a deployment with running trees" says`, or `Sandbox <name> owns a
  volume claim template named "tree", not "issue"; it is of the tree-volume layout …`. Such a
  Sandbox is never adopted: a child's would fit no launch, and a root's volume holds every clone and
  workspace of its tree, which only this drain may take apart.
- **A per-claim locator** on any claim is refused once the store has connected and before it
  migrates (`HasLegacySandboxClaims` in `internal/store/claims.go`).

Nothing is written before either refusal: `store.Open` only connects and pings, and the first
write is `Migrate`, after both checks. So a refused boot leaves the database as the earlier release
left it, and re-pinning the earlier release's image recovers. In the cluster a refused boot is a
crash-looping Deployment: the earlier release's pods keep running, but nothing supervises them.

**A rollout that also turns on `session_store: postgres` does not drain.** It replaces steps 1 to 3
below with steps 1 to 5 of [Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on),
which keep every tree's volume until its sessions are in the table. When `controller: daemon` ran,
step 3 below runs before copy step 1: the earlier release resumes a suspended controller within a
minute, so it cannot be held still for the copy, and copy step 5 marks its claim lost. Copy step 6
deletes the per-claim Sandboxes and hands back to steps 4, 5 and 6 below: the two checks, the dump,
and the rollout, with `session_store: postgres` set.

No migration turns a per-claim Sandbox or locator into an issue pod's, and nothing on a tree volume
of either earlier layout carries over (below), so the upgrade drains the deployment while it still
runs the earlier release:

1. **Drain every tree.** Move each tree's root issue out of the workflow on Dispatch (`done`,
   `backlog`, `icebox` or `triage`) and let its linger expire, which closes the tree. A tree no
   workflow issue backs, one an operator spawned, is closed with `legion claims close` on its root
   claim; the earlier release refuses that for a workflow issue's tree, which the workflow closes.
   Each claim of a closed tree is released and retires, and a retired claim records no locator.
2. **Let the cleanup finish.** On the earlier release, releasing a claim deletes its Sandbox by
   name, and the root's Sandbox takes the tree volume with it (`Release` in
   `internal/runtime/sandbox/sandbox.go` at `legion-v10.0.0`; on an issue-pod build on a tree
   volume, the tree's close deletes each child's Sandbox and then the root's, the volume with it).
   Wait until every Sandbox is gone, or delete one left behind once no claim of it runs.
3. **Clear the controller's claim, if `controller: daemon` ran.** Set `controller: operator` and
   boot the build that ran it once. It stops the controller its earlier boot launched, logging
   `controller: stopping the controller an earlier boot under controller: daemon launched; this
   daemon leaves the controller to its operator`: the stop deletes the claim's Sandbox, and with it
   the controller's volume and session, and retires the claim, so `legion claims list` shows
   `legion-<project>-controller` `retired`. That removes the controller Sandbox of either earlier
   layout, the per-claim one and the one owning a `tree` template, both of which the census refuses.
   In the cluster each of these boots is a configuration change of its own through the deploy path.
4. **Check that nothing is left.** `<project>` below is `legion.yaml`'s `project` lowercased with
   every non-alphanumeric removed (`claim.ProjectToken`), the value of every Sandbox's
   `legion.dev/project` label and of the `claims.project` column. Both of these must come back
   empty:

   ```sh
   kubectl -n <namespace> get sandboxes -l 'legion.dev/project=<project>,!legion.dev/probe'
   ```

   ```sql
   -- connected as the daemon is, with its postgres_dsn
   select token, state from claims where project = '<project>' and locator->>'runtime' = 'sandbox';
   ```

   The query lists every claim that records a Sandbox locator at all, which is stricter than the
   boot check: on the earlier release every such locator is a per-claim one.
5. **Dump the database** (`pg_dump` with the daemon's `postgres_dsn`). It is the only way back
   ([Rolling back](#rolling-back-the-upgrade)).
6. **Roll this release out in one step:**
   - the daemon at this release, with its `legion.yaml` at this release's shape:
     `runtime.kubernetes.issue_volume` where the file set `tree_volume`, and `resources.<role>` as
     `{cpu, memory}` where it set `requests` and `limits` ([Configuration](#configuration) names each
     refusal);
   - `runtime.kubernetes.image` pinned to a worker image built from it, since the image probe
     refuses a worker image whose `pi-legion` declares another daemon API contract (`ProbeImage` in
     `internal/runtime/sandbox/probe.go`); and
   - for an operator-launched controller, every operator's `legion` and `pi-legion` at this
     release's daemon API contract (`DaemonAPIVersion` in `internal/api/version.go`).
     `legion controller start` refuses a `pi-legion` speaking another contract (`probeAndMint` in
     `cmd/legion/controller.go`), and the daemon refuses a `legion` or `pi-legion` of another
     contract with 409 (`internal/api/controller.go`). That `pi-legion` release must be published
     before the rollout. Contract 20 adds the controller's credential route
     (`POST /legion/v1/controller/github-credential`) and the controller pod's `GH_*` environment
     variables (LEGION-668).

**Why deleting a live tree's Sandboxes loses its work.** On the earlier release the tree volume is
the root Sandbox's `volumeClaimTemplates` entry, so Agent Sandbox creates the volume's claim with
that Sandbox as its owner, and deleting the root Sandbox deletes the volume (`sandboxManifest` in
`internal/runtime/sandbox/manifest.go` and `Release` in `sandbox.go`, both at `legion-v10.0.0`).
That loses every change in its working copies that was not pushed and, under `session_store: pvc`,
every session recorded on the volume. Under `postgres` a session copied into the table first
([Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on)) is
kept, and its agent resumes it in a workspace recovered from its issue's pushed branch. A volume
kept anyway is never mounted again: the earlier release names a tree's claim
`tree-legion-<project>-<root issue>-architect`, from the root architect's own Sandbox
(`SandboxName` and `TreeClaimName` in `names.go` at `legion-v10.0.0`), an issue-pod build on a tree
volume `tree-legion-<project>-<root issue>`, from the root issue's Sandbox, and this release names
each issue's own claim `issue-legion-<project>-<issue>`, from the issue's Sandbox (`SandboxName` and
`IssueClaimName` in `internal/runtime/sandbox/names.go`), so no pod of this release ever mounts a
claim of either earlier layout. Suspending the tree's claims on the earlier release instead
(`legion claims suspend`) keeps their Sandboxes, `Suspended`, and the census refuses a suspended
per-claim or tree-volume Sandbox all the same.

#### Rolling back the upgrade

Restore the dump from step 5 and re-pin the earlier release; work done under this release since the
upgrade is not in the dump. Re-pinning without the restore does not work. Once this release has
booted it has applied migrations 0033 to 0035 (`internal/store/migrations`), and the earlier
release cannot read what they and this release write:

- it refuses an `issue_suspend` outbox row as an unknown kind (`decodeOutboxJSON` in
  `internal/record/outbox.go` at `legion-v10.0.0`);
- it refuses an issue pod's locator, whose Sandbox name carries no role (`checkLocator` in
  `internal/runtime/sandbox/sandbox.go` at `legion-v10.0.0`);
- nothing stops it booting on the newer schema: `Migrate` applies the migrations it has not
  recorded and compares nothing against the version the database is at
  (`internal/store/store.go`, the same at `legion-v10.0.0`), so it would boot and then fail row by
  row.

### Pinning a release that drops what a running pod still uses

A pod is built whole from the image pinned when it was launched, and nothing relaunches it for an
image change alone: a release whose daemon stops serving a route, or stops setting a variable,
that the previous image's agents call or read leaves every pod still on the earlier image calling
this daemon as the older release. LEGION-631 is one: it deletes the `/legion/v1/gh-token` and
`/legion/v1/git-credential` routes, which a pod's `legion gh` and `legion credential` from the
earlier image still call. Such a release is pinned only after every tree is drained (step 1 of the
runbook above) or every issue pod is relaunched onto the new image. The daemon's boot log, and
every orphan sweep after it, names each pod still on another image —
`sandbox runtime: <sandbox> runs image <image>, the configured image is <image>; its agents call
this daemon as the older release until the pod is relaunched` (`warnImageDrift` in
`internal/runtime/sandbox/sandbox.go`) — and the daemon replaces such a pod at the next launch of
any of its roles (`imageDrift` beside the moved-stream trigger in `relaunch.go`), which interrupts
the roles live in it once: they end with the pod and resume in the new one, as a moved worker
stream already makes them (below). The daemon-launched controller's pod is in scope for both the
warning and the replacement, which happens only when the controller's process is being started
anyway. The boot action is the log line alone: refusing to adopt a drifted pod would stop
observing a live pod and trigger nothing.

A launch replaces a pod other roles of its issue still run in when the pod cannot run the launching
role as a pod created now would: it runs another image; it holds a worker stream, a secrets broker
enrollment or a session database URL key that a pod created now is not handed; or a role's
launcher Secret no longer binds it. The replacement ends those roles' processes wherever their
turns stood, and that end is the daemon's act, not theirs (LEGION-729). The patch that suspends
the pod also names it on the Sandbox, as the `legion.dev/displaced-pod` annotation: one merge
patch, so a suspension the API server refuses names nothing and every other annotation of the
Sandbox stays. Once the named pod is being deleted or is gone, the runtime reports each role
recorded in it as displaced. When its cached copy of the Sandbox does not name the pod yet, it
reads the Sandbox from the API server before it reports the end, and reports it uncertain, to be
read again, if that read fails. The supervisor then logs `supervise: the process ended with a pod
the daemon replaced; relaunching it uncharged` and resumes the role's session in the new pod at
once. It charges neither a launch failure nor a death. A turn the role was in is lost: its task
goes back to waiting and is sent again once the session resumes, as a moved address's relaunch
does. The annotation lives on the Sandbox, so a daemon restarted mid-replacement reads it as
well.

A route change the plugin calls is a contract bump by `version.go`'s rule, and since LEGION-164 a
contract bump is brought in place at the daemon's first boot on it, with no pod replacement and
no runbook step: the boot pass ("Contract discipline" below) relaunches every registered claim
whose recorded contract differs from the daemon's through the moved-address relaunch path, logging
`supervise: the process speaks another plugin contract; relaunching it onto the current plugin`
once per claim, and the relaunched role loads the release's published plugins installed in its
pod. What the text above keeps is the release that changes what the pod's own `legion` binary
speaks (the launcher, the shim, `workspace-init`): an install refreshes no binary, so only a new
image carries that change, and such a release is still pinned only after every tree is drained or
every issue pod is relaunched onto the new image.

### Contract discipline

`DaemonAPIVersion` (`packages/daemon/internal/api/version.go`) is the Go daemon's own contract
number; each claim records the contract its process registered with as `pluginContract`. A role's
`pi-legion` declares the contract it was built against
(`legion.daemonApiVersion`, `packages/pi-legion/AGENTS.md`), and the daemon's boot pass
(`refreshStalePlugins` in `packages/daemon/internal/daemon/daemon.go`) and its registration path
(`internal/supervise/plugin.go`) agree on one rule: a registered claim whose recorded contract
differs from the daemon's own, or is unrecorded — every claim is unrecorded the first time a
daemon boots on a release that records contracts at all — is relaunched once, uncharged, through
the moved-address relaunch path (`interrupted` → `launch`; the task of a turn in flight goes back
to waiting and is delivered again; a claim whose suspension is held for its turn is suspended
instead of relaunched, and its next resume launches on the current plugin; a claim still launching
is left to its registration, which decides it the moment it arrives). Each relaunch logs one line, exactly
`supervise: the process speaks another plugin contract; relaunching it onto the current plugin`,
naming `claim`, `incarnation`, `pluginContract` (0 when the claim recorded none), `daemonApiVersion`
and `trigger` (`boot`, `registration`, or `sweep`: a relaunch the tree lifecycle or the store
refuses leaves the claim live and stale, so the orphan sweep — the one periodic pass that already
reads every claim — posts it again until one attempt starts).

Under Kubernetes that relaunch keeps the pod — even one built from another image — rather than
replacing it: the role container's shim runs an install script,
`/bin/sh -c '<install script>' legion-plugins <omp argv…>`, which runs
`bun add --exact @sjawhar/pi-legion@<version> @sjawhar/pi-envoy@<version>` (a tarball path or
URL source is handed to `bun add` as written) into a fresh generation directory,
`$XDG_STATE_HOME/legion-plugins/g<generation>/` (`/home/legion/.local/state/<role>/legion-plugins/g<N>/`),
checks with a `node -e` snippet that the installed `pi-legion`'s `legion.daemonApiVersion` equals
the daemon's own (exiting 1 before `exec`, printing
`legion plugins: @sjawhar/pi-legion@<v> declares daemon API contract <got>; this daemon requires <want>`
when it does not), and then `exec "$@"`s into Oh My Pi, which starts with
`--extension <gen dir>/node_modules/@sjawhar/pi-envoy --extension <gen dir>/node_modules/@sjawhar/pi-legion`
in place of `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`. A pod replaced for any other reason
in the same relaunch — a suspended Sandbox, a lost launcher binding, a failed init container, a
moved address — installs nothing: it comes up on the configured image, whose copies the boot gate
already holds to the daemon's contract; and a relaunch that is not a stale-plugin one (a death, a
moved address) still replaces a pod on another image as the section above says. An install failure ends the
child before Oh My Pi starts; the launcher reports it gone, the daemon charges it as a launch
failure, and the claim is retried until the budget ends it in a `worker-died`.

The pair the install uses is `runtime.kubernetes.plugins: { pi_legion: <exact version | absolute
tarball path | http(s) URL>, pi_envoy: <same> }` (both keys together; a dist-tag or version range
is refused), or, when that key is absent, the pair a release build links into the binary — a
binary that links one prints it as `legion version`'s second line, `plugins @sjawhar/pi-legion <v>
@sjawhar/pi-envoy <v>` (the worker image's own `legion` links one too). An exact version is pinned
by npm's own immutable version; a tarball path or URL source carries no integrity pin beyond the
installed `pi-legion`'s declared-contract check above — it is operator trust in what that path or
URL serves. `legion start --check-config` prints, under
`runtime: kubernetes`, after its OK line and its capability lines, either
`plugins: @sjawhar/pi-legion <v> @sjawhar/pi-envoy <v> (from legion.yaml|from the binary)` or
`plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none
relaunches no process on another plugin contract`. The boot logs
`daemon: stale-plugin relaunches install` (`piLegion`, `piEnvoy`, `source`) when it knows a pair, or
the Warn `daemon: this daemon knows no plugin versions to relaunch a stale process onto; set
runtime.kubernetes.plugins or run a release build` when it does not. A Kubernetes daemon that
knows no pair is in a degraded mode: it still records every claim's contract and still writes one
Error per stale claim, `supervise: the process speaks another plugin contract, and this daemon
knows no plugin versions to relaunch it onto; set runtime.kubernetes.plugins`, but relaunches
nothing and refuses nothing. Under `runtime: tmux` a relaunch is always possible regardless: the
pane resumes on the host's own `pi-legion`, which the boot gate already holds to the daemon's
contract, so there is no install to source a pair for.

A stale caller's registration is refused 409 with
`pi-legion speaks daemon API contract <got>; this daemon requires <want>: the daemon is relaunching
this process onto the current plugin` (or, when it registered no contract at all,
`pi-legion registered no daemon API contract; this daemon requires <want>: …`). Its other calls get
the same sentence — a claim route answers `{"error": …}`, a credential or workflow route
`{"code":"STALE_PLUGIN_CONTRACT","error": …}` — logged
`api: refused a call from a process on another plugin contract`; the operator's own controller
instead gets `…: run legion controller start from the release built with this daemon`, since there
is no pod for the daemon to relaunch it in. Every other undecodable request body is logged
`api: refused an undecodable request body` naming the route and the decoder's error. A relaunched
process that registers still on the old contract is charged as a launch failure (the claim
remembers `PluginRelaunchFor`), ending, like any other exhausted budget, in a `worker-died`. The
contract shows on every claim view — `GET /legion/v1/state`, `legion state --json`,
`read_record`'s `pluginContract` field — on `legion claims list`'s line (its last column), and on
the state's controller locator.

What a contract bump never refreshes is the pod's own `legion` binary: the launcher, the shim and
`workspace-init` keep running the generation they were started with, so a release that changes the
launcher's or the shim's own wire to the daemon is a pod replacement as it always was ("Pinning a
release that drops what a running pod still uses" above), never something the contract relaunch
fixes in place.

### Configuration

```yaml
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:<64 hex>   # digest only
    storage_class: gp2          # required: the issue volumes' class (the cluster has no default)
    issue_volume: 20Gi          # default 20Gi; one volume per issue, and one for the controller's pod
    kubeconfig: /home/ubuntu/.kube/legion-daemon-production   # relative to legion.yaml's directory
    context: legion-daemon@example   # required when the kubeconfig sets no current context
    scheduling:                 # optional, beyond the Legion pool the runtime always selects
      node_selector: {}         # merged over legion.dev/pool=legion, which it may not name
      tolerations: []
      priority_class: legion
    resources:                  # optional; a role's reservation: cpu and memory limits, each requested in full
                                # unless cpu_request or memory_request sets less (Issue sizing: Guaranteed or
                                # Burstable), so every role is covered for the resource-limits capability with or
                                # without this block, and an ephemeral-storage limit over a small request (the
                                # role's bound on the node's disk)
      tester: { cpu: 1, memory: 6Gi, ephemeral_storage: 40Gi }   # requests 1 and 6Gi; the disk request stays the default 1Gi
      reviewer: { memory: 2Gi }            # cpu stays the default, 750m; disk bound the default, 10Gi over 1Gi
      implementer: { memory: 32Gi, memory_request: 8Gi }   # Burstable: reserves 8Gi, may use 32Gi
      controller: { cpu: 2, memory: 8Gi }  # controller: daemon only
    pod:                        # the operator's: env, volumes, mounts, ServiceAccount (below)
      service_account: legion-worker
      env: { PI_CONFIG_FILES: /etc/legion-operator/overlay.yml }
      volumes: [...]
      volume_mounts: [...]
    repository_cache:           # optional: each mapped repository's cache (The repository cache, below)
      volume: 20Gi              # default 20Gi; the cache's own volume, which holds the bare clone
      refresh_minutes: 10       # default 10; how long between the cache's fetches from GitHub
      retain_hours: 168         # default 168; how long a cache outlives its repository leaving the mapping
      resources: { cpu: 1, memory: 4Gi, ephemeral_storage: 2Gi, ephemeral_storage_request: 1Gi }   # the defaults
bind: <the daemon host's own address, or 0.0.0.0 once advertise_host names one>
advertise_host: <optional: a stable Service name, e.g. legion-daemon-<project>.<namespace>.svc, every pod dials instead of bind, at worker_stream_port>
worker_stream_port: <the port advertise_host or bind serves it on>
daemon_url: http://<the address pods reach the daemon at>:<the daemon's own port>
capabilities:                   # optional: the deployment capabilities decided by name, with the reason (The deployment's capability report, below); a gap is reported, never refused
  decided: { secrets: "<reason>", model-fallback: "<reason>", resource-limits: "<reason>", pool-capacity: "<reason>" }
```

Every container of every Legion pod carries a cpu and memory limit and a request no greater than it,
and an ephemeral-storage limit, its bound on the node's disk, over a smaller ephemeral-storage
request. `runtime.kubernetes.resources.<role>` sets a role's reservation: `cpu` and `memory` (the
limits), `cpu_request` and `memory_request`, `ephemeral_storage` (the limit) and
`ephemeral_storage_request`, any of them. A limit the file leaves out takes the daemon's default
(`config.DefaultResources()`, `packages/daemon/internal/config/kubernetes.go`); a `cpu_request` or
`memory_request` it leaves out takes the file's limit where it set one, else the default's request
(`settledRequest`), so a file that raises `memory` alone keeps the role's request equal to it.
With no request set below a limit, as at the defaults, every
Legion pod is `Guaranteed`; a role with a lower request makes every pod holding its container
`Burstable` ([Issue sizing](#issue-sizing-one-reservation-per-pod), "Guaranteed or Burstable").

| role | cpu | memory | ephemeral-storage (limit / request) |
| :--- | :--- | :--- | :--- |
| `implementer`, `tester` | 750m | 6Gi | 20Gi / 1Gi |
| `reviewer` | 750m | 4Gi | 10Gi / 1Gi |
| `architect`, `planner`, `merger` | 250m | 1Gi | 10Gi / 1Gi |
| `controller` (`controller: daemon`) | 1 | 4Gi | 10Gi / 1Gi |
| the image probe (fixed, not a `resources` key) | 250m | 1Gi | 5Gi / 1Gi |

Each default cpu and memory is both the request and the limit.

The image probe pod's reservation is the daemon's own and no role's share (`probeReservation`,
`packages/daemon/internal/daemon/kubernetes.go`): `legion probe-image` starts one Oh My Pi at a
time and runs no lane or browser, so 250m and 1Gi hold it, and the probe runs at every boot before
anything is served — a probe that reserved a role's share, as it did the controller's 1 CPU and 4Gi
until LEGION-632, could leave the daemon unable to boot on a pool with no room for a pod that
large ([Issue sizing](#issue-sizing-one-reservation-per-pod), "The bound").

At the defaults a six-role issue pod sums to 3 CPU and 19 GiB, its ephemeral-storage limits to 80Gi
and its requests to 6Gi. `issue_volume` sizes each issue's own volume, the clone, the workspace,
uv's Pythons and packages, and the roles' sessions on it; the controller's pod owns one of the same
size, holding its sessions alone. Ephemeral storage is what a role container writes outside every
volume: its root filesystem, the node's disk — the role's `$HOME`, Oh My Pi's state home with its
Chromium profiles and logs, the Go and Bun caches a build fills (a role container mounts no `/tmp`;
its private and state directories and the config home are in-memory `emptyDir`s, whose pages the
bound does not count). Pods of unrelated trees share a node, so one role filling the node's disk
would put every pod on it under `DiskPressure`; the limit has the kubelet evict the pod of the
container that passed it, the offending issue's alone, and the request is what the scheduler fits
to the node's allocatable ephemeral storage, its root volume. The request is small by default so
that scheduling binds to almost nothing; an operator
who knows the root volume raises `ephemeral_storage_request` so the scheduler reserves disk
([Issue sizing](#issue-sizing-one-reservation-per-pod)).

`packages/daemon/internal/config/kubernetes.go` reads the block and refuses, naming the key:
- `tree_volume`, the tree-volume layout's key: `runtime.kubernetes.tree_volume is now issue_volume:
  one volume per issue`;
- `role_profiles`: `unknown key runtime.kubernetes.role_profiles: a role's reservation is
  runtime.kubernetes.resources.<role>, its cpu and memory limits with optional lower cpu_request and
  memory_request, and a role absent there takes the daemon's default`;
- a `resources` key that is none of the seven roles: `runtime.kubernetes.resources key "small" must
  be a role (architect, planner, implementer, tester, reviewer, merger, controller)`; and
  `resources.controller` unless `controller: daemon`, the one setting under which the daemon
  launches the controller's pod (`runtime.kubernetes.resources.controller sizes the pod of the
  controller the daemon launches, and controller: operator launches none: set controller: daemon or
  drop the key`);
- the earlier shape inside a role's entry: `runtime.kubernetes.resources.<role>.requests is gone: a
  role's reservation is its cpu and memory limits, with cpu_request and memory_request below them
  where set` (`limits` likewise); any other member of a role's entry but `cpu`, `cpu_request`,
  `memory`, `memory_request`, `ephemeral_storage` and `ephemeral_storage_request` is an unknown key,
  and any of those six or `issue_volume` that is not a positive Kubernetes quantity is refused as
  such (`… must be a positive Kubernetes quantity (e.g. 20Gi or 500m)`);
- a request past its limit, once both are settled — the file set one side or both, since the
  defaults never exceed: `runtime.kubernetes.resources.<role>.memory_request <request> exceeds
  memory <limit>`, and likewise `cpu_request … exceeds cpu …` and `ephemeral_storage_request …
  exceeds ephemeral_storage …`, and `runtime.kubernetes.repository_cache.resources.…` likewise;
- a `repository_cache` that is not a mapping, or names a member but `volume`, `refresh_minutes`,
  `retain_hours` and `resources`; a `volume` that is not a positive Kubernetes quantity; a
  `refresh_minutes` or `retain_hours` that is not a positive integer; its `resources` take a role
  entry's six members and nothing else;
- `gateway`, removed with LEGION-270: a pod's model route is the operator's `pod`;
- `agent_secrets.operator`, removed with LEGION-664: the daemon logs in with its own ServiceAccount
  token (`token_file`), which no person approves, so it names no approver;
- an image that is not pinned by digest;
- `session_store` other than `pvc` or `postgres`; `postgres` without `session_dsn_secret`, or with
  one that is empty or not a Secret data key (`[-._a-zA-Z0-9]+`); and a `session_dsn_secret` under
  `pvc` (an inert key is refused, never ignored). At boot the daemon also refuses a
  `session_dsn_secret` that is the providers Secret's `NATS_NKEY_SEED`, a `provider_keys` entry
  that reads the same key (the shim would export the URL into Oh My Pi's environment), and an
  operator's `pod.env` or `provider_keys` naming `OMP_SESSION_STORAGE` or
  `OMP_SESSION_SQL_DSN_FILE` ([Selecting the store](#selecting-the-store)).

`legion start --check-config` shows each of these before anything starts: a file carrying
`tree_volume: 20Gi` is refused naming `issue_volume`; one carrying `resources: {tester: {limits:
{memory: 8Gi}}}` naming `limits`; one carrying `resources: {small: {cpu: 500m}}` naming the seven
roles; one carrying `resources: {implementer: {memory: 32Gi, memory_request: 40Gi}}` naming
`memory_request`; and a file with no `resources` block passes, as does one with `resources:
{tester: {cpu: 2, memory: 8Gi}}` or `resources: {tester: {ephemeral_storage: 40Gi}}`, the daemon
filling every field the file leaves unset. A file that gives a role a request below its limit
passes and prints, after the capability gaps, which pods that makes `Burstable`:

```text
resources: Burstable roles (a request below the limit): implementer, so every issue pod is Burstable; every other pod is Guaranteed
```

Legion holds no model route. `pod` is the operator's: `env`, `volumes` (each a `secret`,
`config_map` or `projected` source), `volume_mounts` and `service_account`, added to every pod, the
image probe's included, and refused where they name a path or variable of Legion's own or the
worker image's. A ConfigMap or Secret a volume names must exist in the daemon's namespace, with every
key the volume asks of it: the image probe refuses the boot at once, naming it, when it does not, for
every volume a `volume_mounts` entry mounts (the kubelet attempts no other)
([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)).
`provider_keys` names keys of the providers Secret, which every pod mounts, those
keys alone, for the shim to export. `deploy/kubernetes/operator-route/` is an example of one: the
Hawk model gateway, keyed by a projected ServiceAccount token, with a `models.yml`, a settings
overlay, and the pod that mounts them; the operator keeps their own copies of the two files in a
directory of their own ([Operator configuration](#operator-configuration)), and the Stage 4a and 4b
proofs run on the example, each with its own copy of its ConfigMap.

Under `runtime: kubernetes` it also requires `daemon_url`, `envoy_url`, `nats_urls`,
`envoy_token_file`, `operator_token_file`, `dispatch_url`, `github_apps` and `projects`. It refuses
`omp_invocation` and `omp_launch_prefix`: every pod runs the worker image's Oh My Pi. Every
address a pod is handed must be one a pod can reach, so `daemon_url`, `envoy_url`, `dispatch_url`
and each `nats_urls` entry may be neither loopback nor the unspecified address, and neither may the
worker-stream host a pod dials: `advertise_host` when the file sets one, `bind` otherwise.
`envoy_url` and each `nats_urls` entry are endpoints, not request URLs: they refuse a query string
or fragment. This is a breaking configuration change for a file that has either; put credentials in
URL userinfo or in a Secret, never in a query.
`advertise_host` is an IP address or a DNS name, the host alone, and only `runtime: kubernetes`
accepts it. A pod's own IP changes on every restart, so a daemon running inside the cluster binds
`0.0.0.0` and lets `advertise_host` name the Service DNS name that reaches whichever pod is live.
Pods dial it at the port the worker stream listens on, so the Service exposes `worker_stream_port`
as that same port number. A daemon on a fixed host (a devbox or a VM) leaves `advertise_host` unset
and binds that host's own address, which pods then dial. A loopback `bind` is refused either way: a
listener bound only to loopback answers no Service and no pod. `legion start --check-config` runs
all of it without starting the daemon, writing a file or running a key command, and then every
refusal boot makes from the files and the environment before its first write, in boot's words: the
operator, Envoy and Dispatch bearers' files, the NATS nkey seed, the instructions file, and the
runtime's own reads (the kubeconfig and every value's translation; under tmux, the OMP invocation,
through `mise where` when it names a `mise` tool, and the host's `gh`, `git` and `jj`). After its
`Config OK: project=…` line it prints one line per deployment capability the file alone leaves open
with no decision — `resource-limits` under tmux, where a pane reserves no CPU and memory, and
`secrets` when no broker is configured — in boot's words (`capability <name> is open: <detail>; to record a decision,
add to legion.yaml: capabilities.decided.<name>: "<reason>"`), and still exits 0: a report, not a
refusal ([The deployment's capability report](#the-deployments-capability-report)). What it
does not do is what boot writes or runs: the state directory, secretsd's provider keys, the plugin
gate and the image probe.

The NATS nkey seed is optional, as on tmux: `nats_nkey_seed_file` (relative to `legion.yaml`'s
directory), else `NATS_NKEY_SEED_FILE`, else `NATS_NKEY_SEED` in the daemon's environment, is the
`legion-pane` user every pod connects as, and the user the daemon's own NATS connection
authenticates as when it has no seed of its own (below). A set source that is empty,
missing, unreadable, blank, readable by more than its owner, or not an nkey user seed refuses boot,
naming the key and the path. One exception: when root owns the file and the daemon is not root,
its group may read it, since a daemon running as a non-root uid in a pod reads a mounted Secret —
always root's — only through the pod's `fsGroup`. Mount it with `defaultMode: 0440`, never the
kubelet's default `0644`, which others can read; any other seed file, the daemon's own or another
user's, stays 0600. A daemon running as root in a pod has no such exception: under `fsGroup` its
mount is `root:<fsGroup>` 0440, which the reader refuses for a root reader, so give that pod no
`fsGroup` and mount the Secret with `defaultMode: 0400`, or pass the seed as `NATS_NKEY_SEED` (and
the daemon's own as `NATS_DAEMON_NKEY_SEED`) from a `secretKeyRef`. `legion start --check-config`
reads the seed as boot does, and its OK line then names the seed's user by public key
(`Config OK: project=<project> nats-nkey-user=U…`), never the seed. With one, every pod's
`NATS_NKEY_SEED_FILE` names `/var/run/legion/providers/NATS_NKEY_SEED`, the providers Secret's own
`NATS_NKEY_SEED` key, which every pod and the image probe mount beside the `provider_keys`,
whatever a launch carries: the daemon never copies the seed into a claim's Secret. Put the same
seed in the providers Secret under that key. The image probe refuses boot when the kubelet cannot
mount the key, when it holds no user seed, and when its user is not the pane seed's (the
probe reports the user's public key, never the seed); it reads the key by the daemon's rule above.
A probe that reports no user at all is an image whose `legion probe-image` predates the report, and
the refusal says to build the image from the daemon's commit: a current one exits 1 on a blank or
invalid key. The shim skips the file, since the pointer
names it, so the seed is never a variable of Oh My Pi or the tools it runs. With none, a pod
carries no pointer and mounts no such key. While the daemon has a seed, `provider_keys` may neither
name `NATS_NKEY_SEED` (on either runtime) nor read the Secret's `NATS_NKEY_SEED` key under another
name, and `pod.env` may not set `NATS_NKEY_SEED_FILE`.

The daemon's own NATS seed is optional too, and no pod ever gets it: `nats_daemon_nkey_seed_file`
(relative to `legion.yaml`'s directory), else `NATS_DAEMON_NKEY_SEED_FILE`, else
`NATS_DAEMON_NKEY_SEED` in the daemon's environment, is the `legion-daemon` user the daemon's
connection authenticates as. That user's grants, which `legion-pane` does not hold, cover what
the connection uses: publish `$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS`,
`$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>`, `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>`,
`$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>` and `$JS.ACK.ENVOY_NOTIFICATIONS.>` (which its
two durable consumers, `legion-go-<project>-dispatch` and `legion-go-<project>-github`, use, though
the trailing `>` reaches every consumer of the stream, each host's listener consumer included, and
lets the seed create a push consumer that delivers stream messages to any subject), and
subscribe `notifications.envoy.exceptions.notifications.role.>` and `_INBOX.>` (which receives
every reply any client on the server gets, key-value reads and watch updates included, such as
the listener's `envoy_roles` and `envoy_sessions` watchers). It publishes on no core
subject: role and controller notices go to the Envoy listener over HTTP, and control directives
over the worker stream, so the Go daemon needs no `legion.ctl` grant. These subjects were verified
live: a server granting exactly them logged no refusal across a boot creating both durables, a
restart onto the existing ones, Dispatch and GitHub events acknowledged and a poison message
terminated, and its trace showed no other API subject (no `$JS.API.INFO`, no
`CONSUMER.DURABLE.CREATE`, which the TypeScript daemon uses instead). It is read by the same rule
and refusals as the pane seed, the
group-readable mount included, and neither seed falls back to the other: an unusable
daemon seed refuses boot even beside a good pane seed. Unset, the daemon connects as the pane
seed, and with neither, with no credential. Keep it out of the providers Secret, which every pod
mounts: in-cluster, put it in a Secret of its own, mounted into the daemon's pod alone with
`defaultMode: 0440` (0400 with no `fsGroup` for a daemon running as root, as above; on a host, a
0600 file the daemon's uid owns), and name that file in
`nats_daemon_nkey_seed_file`. No launch carries it, and a tmux pane's environment drops its
variables with every other credential-shaped name. That keeps the seed out of what the daemon
hands a pane, not out of a pane's reach: on the tmux runtime every pane runs as the daemon's own
uid, so a pane process can read the daemon's seed file, or `/proc/<daemon pid>/environ` when the
raw `NATS_DAEMON_NKEY_SEED` is used. The daemon/pane split limits what a leaked pane seed can do,
not what a pane process can do; the process boundary holds only when the daemon runs apart from
its agents, as a user or in a pod of its own (the Kubernetes runtime). Every command the daemon itself starts (git and
jj in a managed repository's checkout, `mise where`, a key command) runs without
`NATS_NKEY_SEED` and `NATS_DAEMON_NKEY_SEED`, so a seed passed by value never reaches a repository's
tooling; still, prefer the file forms (`nats_nkey_seed_file`, `nats_daemon_nkey_seed_file`, or the
`_FILE` variables), since a value stays in the daemon's own process environment. At boot the
daemon logs, once, `legion daemon connects to NATS` with `user=U…` (the public key, never the
seed), `paneUser=true|false`, and `seed=daemon|pane|none`. `legion start --check-config` reads it
as boot does and adds
`nats-daemon-nkey-user=U…` to its OK line. Every permission the server refuses the daemon's
connection, a subscription or a publish (its JetStream consumers' API requests included), is logged
at error as `NATS refused the daemon a permission: its NATS user lacks that grant` with its
`operation` and `subject`; the server reports a refusal asynchronously, and nats.go's default
handler would only write it to stderr, outside the daemon's log. Every other asynchronous error is
logged at warn with the `subject` of the subscription it names, a dropped connection at warn as
`NATS connection lost` with its `error`, the reconnect at info as `NATS connection restored`
with its `server`, and a terminal close (a fatal server `-ERR`) at error, once, as `NATS
connection closed` with its `error`. Reconnects never run out: the connection never drops a
server from its pool for having failed too many times, so an outage mid-run shows as `NATS
connection lost` and then, once NATS answers again, `NATS connection restored` — never a close —
at nats.go's own default 2 s reconnect wait. A server that keeps refusing to reconnect for any
reason short of the two shapes that do close it — an unrecognized server `-ERR`, or the same
authorization error twice in a row (nats.go's own terminal-close rules) — never closes the
connection and so never logs either of those two lines again: a repeating handshake failure, say
a server that accepts the TCP connection but never completes the protocol handshake, retries
silently behind the one `NATS connection lost` line. A separate warn, `NATS has not
reconnected`, covers that gap: first after 3 minutes down, then every 3 minutes after that while
the connection stays down, naming the downtime so far and the connection's own last-seen error —
usually empty during a plain refused dial, since nats.go clears it on every failed attempt, and naming the failure's
own cause while a handshake keeps failing, since nats.go leaves that one in place until the
connection succeeds. At boot an unreachable NATS or Dispatch delays the boot instead of exiting,
retried one second doubling to one minute, forever, logged at warn as `boot probe failed
transiently; waiting to run it again` with its `probe` (naming which), `attempt`, `retryIn` and
`detail`.

A malformed seed refuses the boot immediately at startup, before any network connection is
attempted. A NATS authorization violation at connect time is the same: the server said no
synchronously, and the boot refuses it at once. An EOF during the NATS handshake also refuses at
once — the connection simply closed, and nats.go returns that synchronously too — though no crash
in the audited journal took this shape, so refusing rather than waiting here is a judgment call.

A NATS permission violation on a JetStream call (a refused consumer or stream grant) is reported
asynchronously: the server tells the connection of it well before the blocked call's own attempt
bound runs out, but the blocked call itself does not return early on that report — it waits out
its own bound exactly as a call that will never get an answer does. Only once that bound runs
out, at 30 seconds, does the boot learn of the violation at all, by folding the connection's own
last-reported error into the one the blocked call returns, so it recognizes the refusal by name
and refuses outright. A clustered JetStream that is itself unavailable — every server reachable,
none of them answering the API request — surfaces the exact same way at the exact same
30-second bound: a bare request timeout, nothing in its own text to say why, nothing to fold in.
The boot cannot tell that shape from a permission violation whose report never arrived — the two
take the same 30 seconds either way, so timing offers no way to distinguish them — and refuses
both rather than waiting on either.

A Postgres failure while reconciling admission is not covered by either wait: an error Postgres
itself returns exits as soon as it comes back, and only Postgres accepting a connection and then
never answering waits out the same 30-second bound before the boot gives up.

Several shapes an operator should know wait forever rather than exit, none of them obviously
"network trouble" on their face: a Dispatch 401 or 403 (a bad or revoked bearer token); a NATS or
Dispatch host that does not resolve (a typo in `nats_urls` or `dispatch_url`'s hostname); a
non-Dispatch 4xx, such as an HTML 404 from a `dispatch_url` whose path is wrong but whose host
answers; and a TLS failure that is not certificate verification (a protocol mismatch, a stalled
handshake). Each of these waits silently: no line is logged beyond the generic `boot probe failed
transiently` warn above, though its own `detail` carries the error's own text (`UNAUTHORIZED:
...`, `no such host`), not a separate line calling out the credential or configuration problem by
name.

A Dispatch 401 or 403 is the one with an operational consequence worth naming plainly: the daemon
does not exit on one, so nothing re-reads `dispatch_token_file` until an operator restarts it by
hand to pick up a corrected or renewed token.

With several `nats_urls`, or a clustered NATS whose advertised addresses this daemon cannot reach,
an authorization violation on one server can surface as a dial failure, or the generic "nats: no
servers available for connection" answer, from a different server nats.go tries next, so the boot
waits and the logged `detail` may never name the authorization error at all (LEGION-580).

While the readiness gate waits, callers outside the daemon see it as still booting, not as down:
the API port is already bound by this point (the same as during the image probe, both before this
gate), so it accepts a connection but serves nothing until the gate passes, and `legion status`
reports the daemon's PID alive but not yet answering. A pane's `legion` tool calls (`read_record`
and `handoff_complete`, the latter minting its grant in-process first) and `controller start` all
wait on that same API, so none of them succeeds until the daemon actually serves; a pane's plain
`gh` and `git` do not, since they read the role's credential from its gh files and call the daemon
for nothing.

Rollout order for the server's `legion-daemon` user: the server admits
`legion-daemon` (its public key applied) with the daemon's grants first; then its seed is stored,
every daemon gets it and restarts, and its `legion daemon connects to NATS` line must read
`paneUser=false` (#1494). Only then is the `legion-pane` seed written. A clean boot line
proves the user, not every grant: the check before the pane seed is written also has each daemon
consume a Dispatch and a GitHub event with no error
line, and searches each daemon's log for `NATS refused the daemon`, since a missing grant on the
exceptions lane (`notifications.envoy.exceptions.notifications.role.>`) still boots healthy and
consumes both events, and that error line is its only sign. `legion-pane` is never granted the
daemon's subjects above. Reversed, a daemon holding only the `legion-pane` seed connects as
`legion-pane`, and each refused subject logs the error line above (a refused consumer or
subscription never delivers).

### The deployment's capability report

`packages/daemon/internal/capabilities` is the one list of what a Legion worker can do (LEGION-578:
every worker is a full agent), 20 rows, each checked at one site. The image rows (`eval-js`,
`eval-python`, `browser`, `lsp`, `codegraph`, `skills`, `toolchain`) are `legion probe-image`'s
([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)), `present` once
the probe passed; the live rows (`subagents`, `web-search`, `mcp`, `repository-extensions`,
`dispatch-envoy-tools`, `github`) are proved by each session at its start, which measures them
inside its own Oh My Pi and reports them with its ready, and the daemon renders each from what its
live sessions reported (LEGION-663; the paragraphs after the deployment rows); the withheld rows
carry the ruling that keeps them from
every worker (`network`: dispatch://LEGION-5, the pod is the boundary; `operator-setup`:
dispatch://LEGION-200, Legion owns its dependencies; `production-identities`: dispatch://LEGION-551
and dispatch://LEGION-205); and the four deployment rows are the daemon's to measure from its own
configuration and its own boot, since no image or pod can show them:

- `secrets`: `runtime.kubernetes.agent_secrets` is configured and the daemon's broker token login
  is `issued`. Under tmux it is open unless decided: no process is enrolled with the broker.
- `model-fallback`: the pod's Oh My Pi has `retry.modelFallback` true, as the probe's OK line's
  `model-fallback` mark reports it; under tmux the plugin gate reads the host's Oh My Pi once the gate
  has passed. The example overlay turns it on ([Operator configuration](#operator-configuration)).
  The mark reads `unread` when the probe could not read `retry.modelFallback` under the pod's Oh My
  Pi settings (the probe pod's log says why), and the daemon's report shows the row open.
- `resource-limits`: every workflow role — and the controller's, under `controller: daemon` —
  reserves CPU and memory (`config.RoleResources.Reserved`: `runtime.kubernetes.resources.<role>`
  or the daemon's default, a limit with a request no greater than it,
  [Issue sizing](#issue-sizing-one-reservation-per-pod)), which a loaded Kubernetes configuration
  always satisfies; the row would name each role that did not. Its detail also says each pod's QoS
  class: `each request equal to its limit, so every pod is Guaranteed`, or `Burstable roles (a
  request below the limit): <roles>, so every issue pod is Burstable; every other pod is
  Guaranteed` (the controller's pod in place of the issue pods when the role is the controller), so
  `legion state --json` shows which roles reserve less than they may use. Under tmux it is open
  unless decided: a pane has no requests or limits.
- `pool-capacity`: the Legion pool had room for the image probe pod's own 250m / 1Gi reservation at
  boot — the probe pod scheduled at its first attempt. While it did not (`stuck`,
  `internal/runtime/sandbox/probe.go`: the pod `Pending` with `PodScheduled=False Unschedulable`
  for longer than `worker_boot_timeout_seconds`), the daemon stays up and retries without exiting
  ([Issue sizing](#issue-sizing-one-reservation-per-pod), "The bound"), and once the probe passes
  the row is open, saying how many attempts waited and the scheduler's last reason (`the image
  probe pod was Unschedulable <n> time(s) at boot before it scheduled: <reason>; the pool had no
  room for its 250m / 1Gi reservation`), unless `capabilities.decided.pool-capacity` names a
  reason. The daemon measures it from the probe's own run (`bootprobe.ImageReport.CapacityWaits`),
  so `legion start --check-config` never prints it. Under tmux it is present: no probe pod runs.

A deployment row is `present` when the deployment satisfies it, `decided` when `legion.yaml`'s
`capabilities.decided.<name>: "<reason>"` records a decision on it (the report shows the reason in
the gap's place; a decision on a satisfied row is moot and the row reads present), and `open`
otherwise, carrying the line that records one. A name that is not one of the four is refused at
load naming them; a blank reason too.

A live row is proved by each session at its start. The Legion extension
(`packages/pi-legion/src/capability-report.ts`) measures the six inside Oh My Pi once
`claims/register` has answered — concurrently, while the jj attribution and the Envoy role claim
run — and sends the report with `claims/ready` (`capabilities`: `measuredAt`, `elapsedMs` and
`rows[{name, ok, detail}]`), each check within 8 s and the whole report within 10 s. A check that
fails, throws or runs past its budget is a failing row carrying the reason, never a stopped
session, and the daemon normalises what arrives (a live row the session left out reads `not
reported by this session`, a name the table has no live row for is dropped, a detail is one line
with every credential it quotes redacted, cut at 1024 bytes) and never refuses a ready for what it
reports. The state route answers without authentication, so the extension cleans each detail the
same way before it sends it, and the daemon cleans a report it reads back from the store at boot
too. The ready after a regained Envoy role re-sends the same report; the controller's ready
carries none. Each row's check, and the fact a passing one reports:

- `subagents`: `pi.agents` is exposed, the `task` tool is registered, and every agent the
  registration's `promptAgents` names (the task agents the role's prompts dispatch) is resolved by
  `discoverAgents` in the workspace and none is in `task.disabledAgents` — `task resolves <n>
  agents, every one the prompts dispatch`.
- `web-search`: `web_search` is registered and one `runSearchQuery` for `jujutsu version control`
  is answered by a provider — `web_search registered; provider <name> answered in <n> ms`.
- `mcp`: every server the workspace's `.mcp.json` or `.omp/mcp.json` or the Oh My Pi profile
  configures is connected, measured through a second, short-lived client per server that the check
  disconnects (the session's own clients are the host's), so a server that admits one client reads
  open with its error — `connected: <name> (<config file>), …`, or `no MCP server configured`.
- `repository-extensions`: every `.omp/extensions/` module, `.omp/skills/*/SKILL.md` and
  `.claude/skills/*/SKILL.md` the workspace carries is among what Oh My Pi discovers —
  `discovered: <paths>`, or `the repository carries no .omp/extensions, .omp/skills or
  .claude/skills`.
- `dispatch-envoy-tools`: the eleven Envoy tools (the ten `envoy_*` tools and `slack_post`) are
  registered and `dispatch read --issue <issue>` answers, which is why the row's summary reads
  "reaches Dispatch through the `dispatch` command and Envoy through the pi-envoy tools" — `11
  Envoy tools registered; dispatch read <issue> answered from <path>`.
- `github`: `gh api graphql -f query='{ viewer { login } }'` answers the role's App login under the
  role's token file (REST `GET /user` is not a call an App installation token may make, so the row
  never reads it) — `gh viewer login: <login>`.

The daemon renders a live row from its live sessions' reports. A live session is a claim that is
ready, working or idle whose report came from its current process (the report's `incarnation` is
the claim's locator's), so a claim relaunched, suspended or retired since its session reported
keeps no row open until the new process reports. The row is `present` when no live session fails
it and at least one proved it, its detail `proved by <N> live session(s); latest pod
<sandbox>/<container> (<role>, <issue>) measured <RFC 3339 time>: <fact>` (the latest
measurement; `pane <window>:<pane>` in place of the pod under tmux); `open` when any live session
fails it, its detail naming each failing session as `pod <sandbox>/<container> (<role>, <issue>):
<fact>`, `; `-joined, the first three then ` +N more`, and carrying no `configLine` — no
`capabilities.decided` line exists for a live row, since a session's failing check is a fact to
fix (the operator's, or the issue's architect's), not a deployment decision, and `legion start
--check-config` says nothing of live rows; and `unchecked` before any live session has reported,
its detail `no session has reported yet (<ruling>): <summary>`, where the ruling is the issue whose
check proves the row (`repository-extensions`: dispatch://LEGION-629; `github`:
dispatch://LEGION-631; the other four: dispatch://LEGION-663). Nothing measures again
mid-session, so a session that booted during a Dispatch, GitHub or search-provider outage reads
open until its next boot.

The report appears in four places. The daemon's log: at each ready that carries a report, one
`capabilities: session reported` line (`claim`, `role`, `issue`, `locator`, `incarnation`,
`elapsedMs` and the `rows` as JSON) and one `capability <name> is open: <fact>` warning per row
the session's check failed, the session in the same attributes; and one warning per open row at
boot and again at the first controller tick after the set of open rows changed (the tick asks
only when it wakes the controller — one registered, no tick pending — and the broker login
reaching `issued` and a live session's report of a row its check failed are the changes a running
daemon sees), as `capability <name> is open: <detail>; to record a decision, add to legion.yaml:
capabilities.decided.<name>: "<reason>"` on a deployment row and `capability <name> is open: pod
<sandbox>/<container> (<role>, <issue>): <fact>` — the row's rendered detail — on a live one.
`legion state --json`: under `capabilities` every row as `{name, status, detail, decision?,
configLine?, source?}`, `status` one of `present`, `unchecked`, `withheld`, `decided` or `open`
(daemon API contract 16 added the list, contract 21 dropped `live` from it and contract 25 dropped
`installed`, which nothing has emitted since LEGION-629; the image rows are `present` once the
probe passed and `unchecked` before it or under tmux), `source` (`configuration` or `boot`, contract
25) on a deployment row alone — what legion.yaml alone decides or what boot measured — never on a
live row, and under each
claim's `capabilities` (`issues.<KEY>.architect` and `issues.<KEY>.workers.<role>.claim`) that
claim's latest report — `measuredAt`, `incarnation`, `ok` (the names its checks passed) and
`open[{name, detail}]` — whatever process sent it; compare its `incarnation` with the claim's
`locator.incarnation` to tell the running process's report from a predecessor's. The controller's
`tick` notice, whose `openCapabilities` names the open rows, live rows beside the deployment gaps,
so the day's report names each (`skills/legion-controller/SKILL.md`). And `legion start
--check-config`, which prints after its OK line the rows the file alone leaves open. Nothing
refuses to start over a gap: a daemon that would not run its pods would itself keep workers from
working, so a deployment gap is the operator's to close or to decide, by name and with the reason,
and a live gap is the session's environment to fix. The example `legion.yaml`
(`deploy/kubernetes/daemon/legion.yaml.example`) leaves its `resources` block commented out, so the
daemon's defaults reserve CPU and memory for every role, and decides `secrets`.

### Anatomy of a Sandbox pod

Each Sandbox carries `legion.dev/project`, `legion.dev/tree` and `legion.dev/issue` (`names.go`):
one Sandbox per issue, shared by every role that works it, owning the issue's volume. The pod
template (`manifest.go`) has two init containers and one container per role (`claim.Roles`:
architect, planner, implementer, tester, reviewer, merger), named for its role:

1. `workspace-fetch` fills the pod's feed, the bare repository at `/legion/feed/<owner>/<repo>.git`
   on the issue's own volume. It is told its repository's cache (`LEGION_REPOSITORY_CACHE_URL`,
   the cache's Service, and `LEGION_REPOSITORY_CACHE_TOKEN_FILE`, the cache's own token), and a
   fresh feed comes from that cache alone: the fetch waits for it to be ready (`waiting for the
   repository cache <url> to be ready: <its answer>`, every 10 s, for at most the cache's own first
   clone's 30 minutes and 10 more), unpacks its copy of the repository, checks it as git checks
   what it takes from a remote, then fetches from GitHub only what the cache lacks: its log's last
   line is `workspace-init fetch: <n> bytes from the repository cache <url> in <d>, its history
   verified in <d>; <m> bytes from GitHub for the delta (<k> refs)`. A cache that is never ready,
   refuses the pod's token, names another repository, or streams history that does not verify
   fails the fetch, naming why, and nothing is cloned from GitHub instead, so the cache's own first
   clone is the one full download of the repository ([The repository cache](#the-repository-cache)).
   A relaunch, whose volume already holds the feed, fetches the delta into it alone (`the feed
   holds <owner>/<repo> already: …`), after removing every git lock file a fetch killed mid-update
   left in the feed, each logged with its age. It takes an existing feed only in the shape the
   fetch writes: no symlink anywhere, its owner directory included; no gitfile; no hook but git's
   samples; a configuration holding only the keys git's bare init and clone and the fetch write,
   each line in git's own form (a `[section]` header alone on its line, or a tab-indented `key =
   value` with no quote, escape or comment), since git also reads a key written on its header's
   line; nothing in `info/` but git's `exclude` and `refs`, and the temporary file
   update-server-info writes `info/refs` or `objects/info/packs` through, which git never reads;
   and no alternates. Before this release any role could
   write the issue volume's `/legion/feed`, so a volume carried over may hold a repository there
   that would run a hook or follow a setting in the token-holding fetch: such a feed is removed,
   logged (`the feed's <owner>/<repo> is not one this fetch wrote (<why>): removed; the feed is
   filled afresh`), and filled from the repository cache. Every fetch from GitHub then points the
   feed's `HEAD` at the branch GitHub names as the default now (`git ls-remote --symref`, under the
   same credential and bound), since a fetch never moves `HEAD` and prunes the branch a rename
   left behind; a move is logged (`the default branch of <owner>/<repo> is now <name> (HEAD named
   <ref>)`). A branch GitHub renamed between the fetch and that read is not yet in the feed, so
   `HEAD` stays until a fetch brings it, logged (`GitHub's default branch of <owner>/<repo> is now
   <name>, which this fetch did not bring: HEAD stays at <ref> until a fetch brings it`). Once the
   fetch may start — at once, or once the cache is ready — the cache's stream,
   its verification and the delta from GitHub share one 30-minute bound
   (`workspace.FetchTimeout`), each given what remains of it, so the fetch's waits, transfers and
   checks end within 70 minutes (`workspace.FetchBudget`) however long it waited, and the few local
   git commands around them take moments; a step with nothing left fails the fetch, naming the
   bound. Each transfer from GitHub is tried again up to four times after a transient failure,
   within that bound.
   It is the only process that holds the provisioning token
   ([Trust model](#trust-model-the-provisioning-token)).
2. `workspace-init` provisions the issue's clone and its jj workspace on the issue's own volume from
   the read-only feed, taking no lock and waiting on no other pod: no other pod mounts the volume.
   The clone is a `git clone --shared` of the feed, so its objects stay in the feed
   (`.git/objects/info/alternates` names `/legion/feed/<owner>/<repo>.git/objects`) and only what
   the issue's own commits add is written into the clone; its `origin` names GitHub, and
   `jj git init --colocate` makes it jj's, tracking the default branch as a `jj git clone` would.
   Its environment is the image's `PATH`, `LEGION_EXPECT_ISSUE_VOLUME=true` when the volume must
   already hold the clone or a retained session (the launching claim resumes a session, or any
   stored claim of the issue recorded one), `LEGION_WORKSPACE_RECOVERED_FROM` on a relaunch after
   the volume was lost, and the XDG base directories (`initEnvironment`, `manifest.go`); nothing
   names a role, a generation, a lock wait or another issue's workspace. The workspace starts at the
   issue's branch, `legion/<KEY>`, which the daemon created on GitHub at the repository's default
   branch, as GitHub names it (`default_branch`, `release/v7.1` as readily as `main`), before the
   issue's architect or planner started; with no such branch it starts at that default branch,
   which the pod's command names (`--default-branch`), and its log says which (`workspace-init:
   <dir> on legion/<KEY> from <default branch>`). Before any git or jj runs it removes every git
   lock file (`<file>.lock`) in the clone's git directory and in the issue's own worktree entry of
   it, and logs each with its path and age (`RemoveStaleGitLocks`,
   `internal/workspace/stalelock.go`): a git or jj interrupted mid-write leaves its lock, and every
   later write of that file fails on it,
   but no process can hold one now, since no role container of the pod has started and no other pod
   mounts the volume. Another worktree entry of the clone keeps its locks; a lock it cannot remove,
   and a clone whose `.git` or `.git/worktrees` is a symlink (git never makes either one, and it
   would take the removal outside the clone), fail the container naming the path. jj's own locks
   are `flock(2)` locks, which end with their holder, so it leaves them. Under a running pod nothing
   removes a lock, since another role's git, which a role's container cannot see, may hold it: the
   `jj` the shim's working-copy adoption and a `handoff_complete` run name each lock still on disk
   that they failed on, with its age, in their error, say not to remove it while the pod runs, and
   that replacing the issue's pod removes it.
3. Each role container runs `legion launcher --connect tcp://<advertise_host, or bind with none
   set>:<worker_stream_port> --token-file … --sandbox <name> --role <role> --private-dir …`: PID 1
   of that role, starting and stopping the role's `legion worker-shim` (Oh My Pi) child on the
   daemon's command, never a worker process of its own. Its log has one line for each connection
   the daemon accepted, `launcher: connected to the daemon at <address> as launcher <id> (sandbox
   <name>, role <role>, pod <uid>)`, and one for each connection that ended, `launcher: daemon
   connection: <why>; reconnecting`, so a connected launcher reads apart from one still dialing.

The issue's volume is its Sandbox's one `volumeClaimTemplates` entry, `issue`: `ReadWriteOnce`, of
`runtime.kubernetes.issue_volume` (20Gi by default) and `storage_class`, labelled
`legion.dev/project` and `legion.dev/issue` and never `legion.dev/tree` (`claimLabels`,
`manifest.go`; the controller's by project and `legion.dev/role`): the volume is the issue's, not
the tree's — its tree changes when a child is re-admitted as a root, and the claim is never
relabelled. From it Agent Sandbox creates the PVC `issue-legion-<project>-<issue key lowercased>`
(`IssueClaimName`, `names.go`) with the Sandbox as its owner, so `kubectl -n legion get pvc -l
legion.dev/issue=<KEY>` finds it and the Sandbox's deletion takes it. `workspace-init` and every
role container mount it at `/legion`, and each role container again at Oh My Pi's sessions directory
through a `subPath`, so a session survives its pod. The feed is the volume's `feed` directory:
`workspace-fetch` mounts that `subPath` alone, read-write, at `/legion/feed`, and `workspace-init`
and every role container mount the same `subPath` again, read-only, at `/legion/feed` over their
whole-volume mount, so the path the clone's alternates names is one in every container and no
agent writes what the token-holding container reads. The kubelet makes the directory on first use,
so a volume that has none is relaunched as it is: nothing on it moves. Each role's Secret,
`<sandbox>-<role>-boot`, holds that role's launcher token and its gh bundle: for every GitHub owner
of the project, `github-hosts-<owner>`, gh's `hosts.yml` carrying a GitHub App installation token
under user `x-access-token`, and `github-config-<owner>`, a `config.yml` of `version: "1"`, and
`github-default`, the owner of the issue's repository. The token for that owner is the role's App's
(the implement App's for the implementer and the merger, the review App's for every other role);
every other owner's is the same App's narrowed to reading contents, pull requests and metadata.
All of it is projected read-only into the role's own container: the token where the launcher
reads it, the gh files through a per-role `gh-<role>` volume mounted at `/var/run/legion/gh` in that
role's container alone, each owner's two keys as `<owner>/hosts.yml` and `<owner>/config.yml`. A
pod's projection names the owners of its creation, so an owner added to the project reaches the
issue's next pod. The issue's `-boot`
Secret holds the provisioning token, projected into `workspace-fetch` alone. The operator's volumes and mounts
join every role container's, and the providers Secret's configured keys when there are any, with its
`NATS_NKEY_SEED` key when the daemon has a NATS nkey seed. Each role's private and state directories
and the XDG config home are in-memory, one set per role so no role's launcher or state collides
with a sibling's; a role container mounts no `/tmp` (the `tmp` memory volume is `workspace-fetch`'s
alone, `podkind.go`), so its `/tmp`, its `$HOME` and the state home below are its root filesystem,
the node's disk, under the role's ephemeral-storage limit. Each role's agent is also told a state
home of its own,
`XDG_STATE_HOME=/home/legion/.local/state/<role>` (`roleStateHome`, `manifest.go`), a path on the
container's own filesystem mounted from no volume, and its shim makes Oh My Pi's profile directory
under it, `omp/profiles/legion`, before Oh My Pi starts (`podsafety.EnsureStateHome`; Oh My Pi
reads the variable only where that directory already exists, falling back to the profile's config
root otherwise). The role containers share the pod's network namespace, the workspace path and the
Oh My Pi profile path, and Oh My Pi's browser broker lock is an abstract unix socket named from the
lock path under its state root — with one state root that name would be the same in every
container, the first role's lock would block every other role's broker, and their `browser.open`
would fail (`Shared browser daemon unavailable`) while `broker.sock` sat on the first container's
own filesystem. `workspace-init`, which runs alone, keeps the plain `/home/legion/.local/state`.

Every container carries a reservation, a cpu and memory limit with a request no greater than it,
and an ephemeral-storage limit over a smaller request: each role container its role's
(`runtime.kubernetes.resources.<role>`, or the daemon's default), and both init containers the
reservation of the role whose launch created the pod (`issuePod.initContainers`, `podkind.go`). At
the default reservations every request equals its limit and the pod is `Guaranteed`; a role whose
`cpu_request` or `memory_request` is below its limit makes the pod `Burstable`. Each container is
bounded on the node's disk, and Legion sets no affinity on the pod (GKE Autopilot adds its own,
below; [Issue sizing](#issue-sizing-one-reservation-per-pod)).

Every role container is told `LEGION_GH_OWNERS_DIR=/var/run/legion/gh`, the bundle's root,
`GH_CONFIG_DIR=/var/run/legion/gh/<owner>`, the issue's repository's owner's directory, and
`LEGION_GH_PATH=/usr/local/bin/gh`, the image's real gh. The `gh` first on its PATH is
`/opt/legion/bin/gh`, which runs `legion gh`: it runs the real gh with `GH_CONFIG_DIR` at the
directory of the owner the command targets, read as gh reads it from the real gh's own help (see
docs/deployment.md, "What a pane's gh, git and jj are"), and at the issue owner's when the command
names no repository at all; a command it cannot read for certain, one naming two owners, and one
naming an owner the bundle holds no directory for are refused with exit 2 and send nothing. `git`
reads the same files through the shared clone's
`credential.helper`, `!legion git-credential`, with `credential.useHttpPath` on, so the helper
answers from the `hosts.yml` of the owner in the path git asks about and from nothing else;
`workspace-init provision` is passed it as `--credential-helper '!legion git-credential'`, and the
`legion` in it resolves on the PATH of whichever process runs git. `GH_TOKEN`, `GITHUB_TOKEN` and
`GH_HOST` are set to the empty string, which gh ignores, so no value the image or an operator's rc
file leaves in the environment outranks the files. No container is told `LEGION_GIT_PATH`,
`LEGION_JJ_PATH`, `LEGION_CREDENTIAL_HELPER` or a grant file (the `legion` tool mints the grants for
its own daemon calls in-process), and nothing is installed under the issue's volume for a pod to
run: a pod's `git` and `jj` are the image's,
resolved on its PATH, which puts only the `legion` directory first. The daemon renders the gh keys
when it writes the role Secret at the pod's start, logging `sandbox runtime: github credential
written` with the sandbox, role, App, `expiresAt` and owners, and a refresher walks every live role
each minute, re-rendering its bundle for the owner `github-default` names from the daemon's cached
leases and updating the Secret only for the owners whose `hosts.yml` changed (`sandbox runtime:
github credential refreshed`, the same fields); the kubelet rewrites the projection in place, so
the next `gh` runs with the new token. The `appauth` lease re-mints 20 minutes before its token
expires, so a new token lands at least 15 minutes before the old one lapses, and GitHub keeps the
old token valid until its own expiry. `gh config set`, `gh alias set` and `gh auth login` cannot
write the read-only directory, and `gh auth status` reports the account as `x-access-token`. A
`task` subagent inherits the container's environment, so its `gh` acts as the same App.

Every role container is told `UV_PYTHON_INSTALL_DIR=/legion/uv/python/<issue>` (the issue key as a
DNS label), `UV_CACHE_DIR=/legion/uv/cache` and `UV_LINK_MODE=copy`. uv keeps the Pythons it installs
and its cache on the issue's volume beside the workspace, so a project's `.venv`, which links to its
interpreter there, runs as it is in every later pod of the issue, which reuses the packages an earlier
pod downloaded. The volume is the issue's alone, so no other issue's pod ever installs into either
directory, at the cost of one download per issue. uv copies each package from the cache into a
`.venv`: with the cache and the `.venv` on one filesystem it would otherwise hardlink them, and an
edit made in place inside the `.venv` would change the cache and every later install from it. So each
`.venv` is a full copy of its packages on the issue's volume, beside the cache, and a deployment
sizes `issue_volume` for the clone, the workspace, the issue's Python and one copy of its packages,
and for the workspace's CodeGraph index (`.codegraph/`, 185 MB for this repository). `uv cache clean`
and `uv cache prune` remove cache entries under a lock that stops at the pod; on an issue's own
volume no other pod uses uv, so either may run whenever the issue's pod is the only one.

Each role's shim is started with `--warm-codegraph`: once its Oh My Pi has written its first frame —
its extensions loaded, its RPC loop serving — the shim builds the workspace's CodeGraph index in the
background (`codegraph init`, or `codegraph index` to repair one an earlier build left partial, as
`codegraph status --json` decides). Nothing waits on it: not the launch, not the role's registration,
not a prompt. Six role shims share one workspace, so the warm-up holds a lease at
`.codegraph/legion-warm.lock` for its run — an exclusive create whose holder refreshes its mtime
every 10 s, taken over once it is 60 s stale, the takeover itself under a second exclusive create
beside it so two shims that both read one stale lease never both build — and a shim that finds the
lease held leaves the build to its holder. When a build finishes, the shim's warm-up logs one line in
the role container's log, `codegraph warm-up for <workspace> built the index: codegraph init took
<duration>, done <duration> after launch` — the build's own duration and the time since the role's
launch (`codegraph index` in place of `init` for a repair) — which stage 4b's `repository-tools`
checkpoint reads (`kubectl logs <pod> --all-containers`). A stop mid-build — the launcher's one
SIGTERM to the role's process group — ends Oh My Pi and the `codegraph` child together, and the shim
exits only once the warm-up has released its lease: it waits for that release at least 3 s and at
most what the pod's `terminationGracePeriodSeconds` (`worker_stop_timeout_seconds`, which the
launcher passes the shim as `--stop-grace`) leaves once the agent's 5 s grace and the 1 s stdout
drain are spent (4 s with the default 10 s). The whole stop fits inside the grace from 9 s up, so the
daemon refuses a smaller `worker_stop_timeout_seconds` under `runtime: kubernetes` at configuration
load (`legion start --check-config` says so, quoting the key and the floor), and the relaunch is
never told a live build holds the workspace; it runs the warm-up again, and `status` on the volume's
existing index answers complete, so nothing runs, or reports the index the stop left partial, which
`codegraph index` repairs. The tester's `affected` and the reviewer's `impact`/`callers` queries
answer once `codegraph status --json` reports `index.state: "complete"`; before that a role falls
back to grep, as its prompt says. The init containers never call `codegraph`.

Every pod runs:
- with `runtimeClassName: gvisor`;
- with `serviceAccountName` set to the operator's `pod.service_account` (the namespace's default
  when it names none) and `automountServiceAccountToken: false`;
- under Pod Security "restricted": non-root user 1000 on the pod, and on each container no
  privilege escalation, ALL capabilities dropped and the RuntimeDefault seccomp profile;
- on the Legion pool, with its node selector and toleration, and nothing else of its placement that
  Legion itself sets: no affinity (GKE Autopilot adds its own to an annotated pod, below);
- `Guaranteed` at the default reservations: every container's cpu and memory request equal to its
  limit (the two quantities the kubelet's QoS class reads); `Burstable` when a role the pod holds
  sets a lower `cpu_request` or `memory_request`; and its ephemeral storage a 1Gi request under a
  per-role limit ([Issue sizing](#issue-sizing-one-reservation-per-pod), "The node-disk bound");
- annotated against autoscaler eviction:
  - `karpenter.sh/do-not-disrupt: "true"`, Karpenter's own, which GKE's cluster-autoscaler ignores;
  - the issue pod and the project controller's additionally carry
    `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`, the upstream Cluster Autoscaler's own
    annotation, which GKE Autopilot honours as a request for up to seven days of extended-duration
    protection from its own scale-down and node-upgrade eviction — not indefinitely, and not from
    node pressure or an OOM, a higher-priority Pod, Compute Engine maintenance or node repair, or an
    operator-initiated drain;
  - on GKE Autopilot this second annotation also adds a required node affinity
    (`cloud.google.com/extended-duration-pods`) and gives the pod a node of its own whenever
    possible, which usually costs each pod it creates (an issue's first launch, or a relaunch that
    replaces the pod) a fresh node and a cold image pull ([Image size and pull
    time](#image-size-and-pull-time));
  - a pod carrying it is refused outright under the `cloud.google.com/gke-spot` node selector or
    toleration (GKE offers no extended duration to a Spot Pod requested any other way, such as the
    `autopilot-spot` compute class); the boot probe (below) carries only Karpenter's, so it does
    not catch a `gke-spot` scheduling config before every issue and controller launch is refused;
  - the image probe pod (below) carries only the first: a dedicated node per boot would cost more
    than the eviction window buys a probe that is already short-lived and retried on failure.

The project controller's Sandbox under `controller: daemon`, `legion-<project>-controller`, is the
same pod with a one-role list: it carries `legion.dev/project` and `legion.dev/role=controller` and
no tree or issue label, one init container (`workspace-init controller`) and one launcher container,
`controller`, both carrying the controller's reservation, and owns a volume of its own the same way,
`issue-legion-<project>-controller` ([Daemon-launched controller](#daemon-launched-controller)).

The image probe runs as a Sandbox of its own, `legion-probe-<project>-<digest12>`, with
`shutdownPolicy: Delete`, its one container carrying the probe's own fixed reservation — 250m and
1Gi, over a 5Gi disk bound and a 1Gi disk request (`probeReservation`,
`internal/daemon/kubernetes.go`), no role's share, since `legion probe-image` starts one Oh My Pi
at a time and runs no lane or browser, and a probe sized as a role could keep the daemon from
booting on a full pool ([The image is probed before it publishes](#the-image-is-probed-before-it-publishes),
[Issue sizing](#issue-sizing-one-reservation-per-pod)).

### The repository cache

Each repository the project serves has a cache of its own: one Sandbox, `legion-<project>-cache-<owner>-<repo>-<8 hex>`
(`CacheSandboxName`, `names.go`; the hex is of the repository's name, so two repositories whose
names slug alike get two caches), labelled `legion.dev/project` and `legion.dev/cache=true` and
annotated `legion.dev/cache-repo: <owner>/<repo>`, which is the one place its repository is read
from (`kubectl -n legion get sandbox -l legion.dev/cache`). It is no issue's and no claim's: no
tree's cleanup lists it, and nothing of it registers on the worker stream
(`internal/runtime/sandbox/repocache.go`). Its pod runs one container, `cache`, the image's
`legion workspace-init cache --repo <owner>/<repo> --root /legion/cache --listen :8418 --refresh
10m0s` (`refresh_minutes`, as a duration), on the Legion pool under gVisor with the operator's
ServiceAccount, and nothing of an agent's: no model route, provider key or Dispatch variable. It
owns one volume, `cache-<sandbox>`, of `runtime.kubernetes.repository_cache.volume` (20Gi by
default), holding a bare clone of the repository at `/legion/cache/<owner>/<repo>.git`, and a
headless Service of its own name, whose DNS name resolves to the pod only while it is Ready. Its
Secret, `<sandbox>-token`, holds the implement App's installation token for the repository's
owner, read again as every transfer from GitHub starts and brought to the current lease on the gh refresher's
minute, and the cache's own bearer token, 32 random bytes made with the Secret and kept while it
stands; the issue pods of that repository are given the same bearer token in their provisioning
Secret, and no other pod has it.

The process clones the repository once, through the same one-shot credential and bounds as an issue
pod's fetch, then fetches every branch from GitHub every `refresh_minutes` (10 by default), pruning
the branches GitHub deleted and keeping what each fetch brings as a pack, and repacks the clone
once a day. Restarted after a fetch was killed, it removes the git lock files that fetch left
before it fetches again. It serves, on port 8418:

- `GET /readyz`: 503 until the first clone is whole, then 200, with nothing else; the pod's
  readiness probe, which carries no token.
- `GET /healthz`: `503 {"ready":false,…}` until the first clone is whole, then `200 {"ready":true,
  "repo":"<owner>/<repo>","head":"<default branch's commit>","lastFetchAt":"…","bytes":<n>}`; it
  waits on no fetch, and answers any request, since it holds nothing secret and nothing of the
  repository's content.
- `GET /tar`: the clone's refs and packs as a tar stream — `HEAD`, `packed-refs`, `refs/` and
  `objects/pack/pack-<id>.pack`, no pack index, loose object or configuration — which waits for a
  fetch under way to finish, so it never streams a half-written ref or pack.

`/tar` answers only a request whose `Authorization: Bearer` is the cache's own token; any other is
401 and names nothing. `/healthz` and `/tar` name their repository in a `Legion-Repository` header
on every answer. Each fetch also points the clone's `HEAD` at the branch GitHub names as the
default now, once the fetch has brought that branch, so a default branch renamed on GitHub is the
one the cache publishes and its tar's `HEAD` names from the next tick. A fetch that fails is logged
and tried at the next tick, and the
cache stays ready on what it last fetched; a restarted cache whose volume holds its clone fetches
into it, and serves it as it is when GitHub cannot be reached; a first clone that fails ends the
process, and the kubelet restarts it.

An issue pod's `workspace-fetch` fills a fresh feed from its repository's cache alone
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)). It asks the cache's health with no
credential, takes nothing from a cache whose answer names another repository, so a cache pod
replaced and its address reused by another repository's cache can put nothing into the feed, and
presents the cache's token with its request for the tar alone. It indexes each pack
it received afresh (`git index-pack`, which checks the pack's checksum and every object in it) and
checks every object the refs reach is there (`git fsck --connectivity-only`) before the feed is
used, and a damaged or incomplete history fails the fetch. A cache that is not ready is waited
for, and one that never becomes ready fails the fetch, naming its last answer; the issue pod's
failure then names the cache's state (`the repository cache of <owner>/<repo>, which a fresh feed
comes from alone, is failing: container cache CrashLoopBackOff: …`).

The issue pods reach the cache on port 8418 at its Service's name, `<cache>.<namespace>.svc`,
the headless Service the Agent Sandbox controller makes for the cache's Sandbox. Since the fetch
presents the cache's token to whatever that name reaches, the daemon hands an issue pod the
cache's address and token only once the Service of that name is the cache's own: controlled by
the cache's Sandbox, by uid, selecting exactly the pods the controller stamps with the hash of the
Sandbox's name (`agents.x-k8s.io/sandbox-name-hash`), and headless (`clusterIP: None`) with no
`externalName`, the shape the controller makes. Anyone who may create a Service in the namespace
could make one of the cache's name first, or write a controller reference into one itself, which
the API server accepts. The controller refuses to adopt one without its tracking label, or one
with a cluster IP, and leaves it standing; one with the label and no cluster IP, an `ExternalName`
Service among them, it adopts, writing its own controller reference and the exact selector but
keeping the planted `type` and `externalName`, so it would send the token to its maker's host.
Either
way the launch waits, logging `the repository cache's Service is not the cache's` with the Service
and why, and fails at the boot timeout naming them, with no pod made and no token written, and
`legion state` shows the cache `refused` with the same reason. The controller never resets a
Service's type: delete such a Service, and the controller makes the cache's own. While the
daemon's Services list or watch is failing, its store may hold a Service that has since changed,
so it trusts none: launches wait the same way, and the cache shows `refused` with `its Service …
cannot be checked: the Service store may be stale: …`. The daemon reads each Ready cache's
`/healthz` every minute at its pod's address, as the store
holds it, with no credential, so a daemon outside the cluster that reaches pod addresses but
resolves no cluster name reads it too; an answer from a pod that took the address once the cache's
pod was gone names another repository, and is that read's failure. A NetworkPolicy that isolates
the namespace's pods must admit both, or every fresh feed waits on a cache it cannot reach and
`legion state` shows the read's failure.

`legion state --json | jq .repositories` shows them: each `mapped[]` repository carries `cache`
(`repo`, `sandbox`, `state` — `starting`, `cloning`, `ready`, `failing` with the kubelet's
`reason`, or `refused` with the Service's — `ready`, `address`, the cache's Service, while Ready
behind its own Service, and from the last health read of the pod it runs now `head`,
`lastFetchAt` and `bytes`, or `healthError`) once the cache exists, and `caches` lists the caches
of repositories the mapping no longer names, each with `retainedUntil`.

An issue pod's launch makes its repository's cache when there is none yet, and the daemon makes
the cache of each repository it serves at boot and on every orphan sweep after it, once a minute:
a missing cache is created (its Sandbox `Suspended`, then its Secret owned by that Sandbox, then
its pod template and `Running`), and a cache whose pod runs another image than the daemon's, has
ended, or was left `Suspended` gets a new pod while its Sandbox and volume stay, so a new release
costs a pod, not a clone. A launch whose cache cannot be made fails, naming why. The cache of a
repository the mapping no longer names is annotated `legion.dev/cache-unmapped-at` at the sweep
that finds it so, kept for `runtime.kubernetes.repository_cache.retain_hours` (168 by default), and
then deleted with its volume and Secret; mapped again before that, it loses the annotation and
stays. A cache the sweep cannot make or repair — its owner's token cannot be minted, say — is a
warning naming it, tried again at the next sweep. The boot census refuses a Sandbox labelled
`legion.dev/cache` that is not one this runtime builds (no repository annotation, or another
container or claim template), by name.

### A pod whose address moved

A role process's addresses are fixed when it is launched, and nothing changes a running process's
argv or environment. The daemon hands every role process, the daemon-launched controller's
included, six from its configuration. The worker stream listener,
`tcp://<advertise_host, or bind with none set>:<worker_stream_port>`, is in every launcher
container's command (item 3 of the anatomy list above), so it is fixed for the pod's life: a
launcher dials it and never redials elsewhere. The other five reach a generation's Oh
My Pi in the start command its launcher receives, never in the pod spec, so they are fixed for that
generation's life: `LEGION_DAEMON_URL` (`daemon_url`), `ENVOY_NATS_URL` (`nats_urls`), `ENVOY_URL`
(`envoy_url`), `DISPATCH_URL` (`dispatch_url`) and, when the runtime enrolls pods,
`AGENT_SECRETS_URL` (`runtime.kubernetes.agent_secrets.url`); the daemon-launched controller's
generation also carries `LEGION_SLACK_CHANNELS` (the project's `slack_channels`), recorded and
compared like these. Each moves when the daemon restarts with its key changed,
and with no `advertise_host` set the stream also moves when the daemon restarts on another host. A
daemon that restarts re-adopts each live claim by its recorded locator (the boot orphan sweep), and
re-adoption alone would leave the role holding the old addresses: a launcher dialling a stale
stream never reaches the new daemon, and a stale `LEGION_DAEMON_URL` fails every call the agent
makes to the daemon's API (its phase completion, its record read) while its stream still works.

So the runtime compares a role's addresses with what it hands now on every evaluation of the role
(each watch event, the probe-interval sweep, each probe):

- **The stream** is read from the role container's launcher command in the pod spec, before the
  launcher's own state, since a launcher on a stale stream would only ever read as disconnected.
- **The secrets broker's enrollment**, for a role that enrolls, is read from the pod spec at the
  same point: the `agent-secrets-token` volume's projection of the broker's token (its audience,
  expiry and path) and the role's own key directory, which a generation of the role started now
  runs against. Like the stream, both are fixed for the pod's life, so turning
  `runtime.kubernetes.agent_secrets` on, or changing its `audience` or `token_expiry_seconds`,
  leaves every running workflow role holding a pod that lacks them.
- **The other five** are read from a record on the issue Sandbox (`addresses.go`). Before it sends a
  generation's start command, the daemon merge-patches the role's own annotation,
  `legion.dev/addresses-<role>`, with the generation and, for each of the five, its variable, its
  name and the sha256 of its whole value. The patch carries the Sandbox's uid, so a Sandbox deleted
  and recreated meanwhile is never written, and it leaves every other role's annotation as it was.
  A later daemon reads the record from its informer cache, with no API read of its own, and compares
  the digests once the role's launcher reports it runs that generation. A role with no record, or a
  record of another generation, is never stale, so a generation started before records existed is
  not relaunched for want of one; a record that cannot be read is reported stale, and the relaunch
  writes a fresh one. The record holds no raw value: a URL's credentials travel only in the start
  command, never into a cluster object.

A role holding any other value is reported `stale_address` rather than `alive` (`ObservationKind`,
`internal/runtime/runtime.go`), and the observation's detail names each address that moved, with
the name it holds and the one a generation started now is handed (`--connect
tcp://192.0.2.5:13371, now tcp://192.0.2.7:13371`, `ENVOY_URL https://envoy.internal.example, now
https://ENVOY.INTERNAL.EXAMPLE`, or `agent-secrets-token audience=agent-secrets expiry=3600s
path=token, now audience=agent-secrets-next expiry=3600s path=token`). The supervisor logs that
detail, so every printed endpoint is constructed from its scheme, host and port alone, adding
`xxxxx@` when userinfo is present. Path, query and fragment never appear, which also protects a
value an earlier daemon accepted before the current endpoint grammar refused queries and fragments,
and a value that yields no scheme and host is named only `xxxxx`. `ENVOY_NATS_URL` is named entry by
entry from the URLs the loader accepted, never by splitting the joined value, since raw commas are
valid in userinfo. A role this daemon started always compares equal, so only the roles a daemon
under another configuration started are ever reported, from the boot that re-adopts them; nobody
runs a command for it. The operator's own variables (`runtime.kubernetes.pod.env`) are not
compared: a change there reaches the pods created after it.

The supervisor relaunches each reported claim at once, through the launch path a death uses: a
`Resume` of its recorded session, or a `Spawn` when it has not registered yet. What the relaunch
replaces follows from what moved. A pod whose launchers dial a stale stream, or which lacks the
broker enrollment its roles are started against now, cannot run them as a pod created now would,
and every role of it is reported at once, from the boot that re-adopts them: the first of them
relaunched replaces the pod, under the pod's launch turn, and the others, whose relaunches wait on
that turn, resume into the new pod, whose six launchers dial the current stream and mount the
current enrollment. None of them is charged, and none is found gone when the pod is replaced under
it. The controller's pod, whose one launcher dials a stale stream, or which lacks the broker
enrollment under `runtime.kubernetes.agent_secrets`, is replaced the same way at its relaunch. A
role whose environment alone moved, the broker's URL, `LEGION_SLACK_CHANNELS` for the controller
and the other addresses its agent is told among them, starts its next generation in the same pod,
handed the current values, and the pod and its other roles are left as they are. The stale
observation is never charged, since the process did nothing wrong; a relaunch the runtime refuses
is charged as any launch failure is.
A turn the stale process was in used the addresses it holds and ends with the relaunch, so the turn
is lost: its task goes back to waiting and is sent again once the relaunched agent is ready, the
recovery a death in a turn gets. A claim whose suspension was held for that turn's end is suspended
instead, as it is when its process dies.

The record costs one API write per generation started, the annotation's patch (844 bytes in the
runtime's tests, with Dispatch and the broker unset), where a start in a running issue pod made none
before. Stage 4b drives both cases with real agents on the cluster: `address-moved-env-same-pod`
respells `envoy_url`'s host, and `address-moved-stream-new-pod` swaps the daemon's API and
worker-stream ports ([`scripts/e2e/README.md`](../scripts/e2e/README.md#stage4b-sandbox-treesh)).

### A shell on an issue's volume

Some of provisioning's refusals name `jj` commands against the issue's clone, the repository its
workspace is a `jj workspace` of, `-R /legion/repos/github.com/<owner>/<repo>`
(`packages/daemon/internal/workspace/bookmark.go`):
- a local bookmark deleted and never pushed: restore it, cancel the deletion, or start from the
  repository's default branch;
- a conflicted local bookmark: keep an added commit, or start from the repository's default branch.

The refusals for an origin row that racing fetches conflicted or moved name no command, because
provisioning again settles them.

Each refusal is in the failing pod's `workspace-init` log
(`kubectl -n legion logs <pod> -c workspace-init`), and the daemon's log quotes its tail. Nothing
is registered before a refusal, so the pod's next attempt refuses again until the operator acts.
The clone is only on the issue's volume, so the commands run in a pod that mounts it, from the
operator's context: the daemon's identity creates no pod and has no exec
([RBAC](#rbac-the-go-daemon-needs)).

**The issue's pod is running.** Each of its role containers mounts the volume at `/legion`:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY> --field-selector=status.phase=Running
kubectl -n legion exec -it <pod> -c architect -- sh
export XDG_STATE_HOME=/home/legion/.local/state/architect   # the role's state home (below)
jj bookmark list --all-remotes legion/<KEY> -R /legion/repos/github.com/<owner>/<repo>
```

The shell runs as the issue's agents do, user 1000 under gVisor, but it carries the container's
environment, not the agent's: a role container's spec holds only what the kubelet resolves, which
is `POD_UID` from the downward API, and every other variable the agent is told — the operator's
`runtime.kubernetes.pod.env` included, and among them
`XDG_STATE_HOME=/home/legion/.local/state/<role>`, the role's state home — arrives in its
launcher's start command (`launchEnvironment`, `manifest.go`). The `export` above sets that one, so
an `omp` started in the shell roots its state where the role's agent does rather than under the
profile's config root; for the agent's whole environment, read `/proc/<agent pid>/environ`, the
process whose `argv[0]` is `omp` under the role's `legion worker-shim`, as stage 4a's `pod-baseline`
does (`xargs -0 sh -c 'exec env -i "$@" omp …' agent-env </proc/<pid>/environ`). The clone is at
`/legion/repos/github.com/<owner>/<repo>` and the issue's workspace at
`/legion/workspaces/<owner>/<repo>/<issue key lowercased>` (`workspace.Location`,
`internal/workspace/provision.go`); no other issue's workspace is on this volume.

**No pod of the issue is running**, as when the only pod is the one whose `workspace-init`
refuses. That pod is still there: the kubelet retries its `workspace-init` (`restartPolicy:
Always`) and the daemon's next launch replaces it, and every attempt removes the git locks in the
issue's clone, one a git of yours holds included. So make it go first, and keep the issue from
launching again while you work:

1. Suspend every claim of the issue that is in `registered`, `ready`, `idle`, `working`,
   `launching`, `shim_connected`, or `suspended` with a `suspendedBy` other than `operator` (`legion
   claims list --json --operator-token-file <file>`): `legion claims suspend --claim <token>
   --operator-token-file <file>` on each. An operator's suspend takes over one the daemon recorded,
   so the workflow resumes it no more until you do. A `working` claim stops at its turn's end (the
   command waits `--wait`, a minute by default), and a `launching` or `shim_connected` one once its
   agent is ready. For a child issue, suspend its tree's architect too: it runs in the root's pod,
   and its retry of a held phase relaunches the child's failed worker.
2. Once each suspend has printed its claim `suspended` (run one that exits 1 again, with a longer
   `--wait`: it exits 1 when the wait runs out with the suspension still held for the agent's turn,
   and the patch below would cut that turn off mid-write, leaving a stale lock of its own), set the
   Sandbox `Suspended` from the operator's context. `legion claims suspend` stops a role's process
   but leaves the pod, and its Sandbox `Running` (`Suspend`, `internal/runtime/sandbox/sandbox.go`).
3. Run git or jj writes only once the issue lists no pod. A claim the suspend does not act on —
   `queued`, `launch_uncertain`, `failed`, `retired` — the workflow can still start: a start row
   spawns a queued claim and retries a failed or retired one (`internal/daemon/outbox.go`), which
   sets the Sandbox `Running` again. So list the pods again right before each write.

```sh
kubectl -n legion patch sandbox legion-<project>-<issue key lowercased> --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n legion get pods -l legion.dev/issue=<KEY>
```

Then mount the issue's claim in a pod of your own, `issue-<Sandbox>`, i.e.
`issue-legion-<project>-<issue key lowercased>` (`kubectl -n legion get pvc -l legion.dev/issue=<KEY>`).
The claim is `ReadWriteOnce`, and the pod carries no affinity, so it lands wherever the pool has
room and the volume follows it. The issue's next launch (the operator's `resume` or `retry`, or the
workflow's start of one of its roles, such as its tree's architect retrying a held phase) sets its
Sandbox `Running` again (`relaunch`, `relaunch.go`); delete your pod before it, and for a child
issue suspend the tree's architect first. Once you are done and your pod is deleted, resume each
claim you suspended (`legion claims resume --claim <token> --operator-token-file <file>`).
`ReadWriteOnce` binds the volume to one node, not one pod: on another node the new pod waits on the
detach, with `FailedAttachVolume` or `Multi-Attach` events, until yours is gone, but on the same
node it mounts the volume beside yours, and its `workspace-init` removes the clone's git locks, one
a git still running in yours holds included:

```yaml
apiVersion: v1
kind: Pod
metadata: { name: legion-issue-shell, namespace: legion }   # no legion.dev/* labels
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  nodeSelector: { legion.dev/pool: legion }
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000             # the issue pod's own: its files on the volume are group 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: shell
      image: <runtime.kubernetes.image>      # the daemon's worker image, by digest
      command: [sleep, "3600"]
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: issue, mountPath: /legion }]
  volumes: [{ name: issue, persistentVolumeClaim: { claimName: issue-legion-<project>-<issue key lowercased> } }]
```

```sh
kubectl -n legion apply -f legion-issue-shell.yaml
kubectl -n legion wait --for=condition=Ready pod/legion-issue-shell --timeout=300s
kubectl -n legion exec -it legion-issue-shell -- sh
kubectl -n legion delete pod legion-issue-shell
```

The pod carries no credential. An agent of the issue can plant git and jj configuration in the clone
([Trust model](#trust-model-the-provisioning-token)), which this shell's `jj` obeys, and the pod
gives that configuration nothing to take. Run no command in it that holds a token.

### Issue sizing: one reservation per pod

Every container of a Legion pod carries a cpu and memory limit with a request no greater than it
(`roleRequirements`, `internal/daemon/kubernetes.go`), and an ephemeral-storage limit over a smaller
ephemeral-storage request (the node-disk bound, below): each role container its role's reservation
(`runtime.kubernetes.resources.<role>`, the daemon's default where the file sets none —
[Configuration](#configuration)), both init containers the reservation of the role whose launch
created the pod (`issuePod.initContainers`, `internal/runtime/sandbox/podkind.go`), the controller's
launcher and init container the controller's, and the image probe's one container the probe's own
fixed 250m / 1Gi, request equal to limit (`probeReservation`, [Configuration](#configuration)), no
role's share. Inside a pod, under gVisor, one role may use what its idle siblings reserved (below).
At the defaults a six-role issue pod sums to 3 CPU and 19 GiB, the init containers adding nothing: a
pod's effective request is the larger of its containers' sum and its largest init container, and no
one role's reservation exceeds the sum of the six. Every role reserved is what the `resource-limits`
row of [The deployment's capability report](#the-deployments-capability-report) measures, so on a
Kubernetes deployment it is `present` with or without a `resources` block; its detail names the
roles, if any, that make a pod `Burstable`.

**Guaranteed or Burstable.** The kubelet's QoS class reads cpu and memory alone (the disk bound's
request under its limit changes no pod's class). A deployment that sets no `cpu_request` or
`memory_request` below its limit runs only `Guaranteed` pods: each reserves its peak for its whole
life and bursts past nothing, and it is never the class the kubelet evicts first under node memory
pressure. A role given a lower request — `implementer: {memory: 32Gi, memory_request: 8Gi}`, for an
agent whose `lean` processes each map a few GiB at import and can pass 20 GiB on a large proof, but
which uses little between builds — makes every pod holding its container `Burstable`: every issue
pod for a workflow role, since each holds all six, and the controller's pod for the controller; the
image probe pod stays `Guaranteed`. The init containers take the launching role's reservation as
they always do, so they never raise the pod's effective request above its containers' sum. The
scheduler and the pool's limits count the requests, so more pods fit a node and a request-billed
cluster bills less. The cost is that the burst is not reserved: a node whose pods together use more
memory than it has is under memory pressure, and the kubelet evicts `Burstable` pods using more than
their request before any `Guaranteed` pod, or the kernel OOM-kills a container. Either stalls the
tree as an OOM kill of a `Guaranteed` pod does — the pod is relaunched and its roles resume their
sessions, but the turn in flight is lost. Request at least what a role needs to boot and to idle,
and set `Burstable` roles only where losing a turn under pressure is cheaper than reserving the
peak.

On GKE Autopilot, which bills a pod by its requests, a pod on the default general-purpose platform
or the `Balanced` or `Scale-Out` class is honoured a request below its limit only in a cluster that
supports bursting: GKE 1.30.2-gke.1394000 or later on cgroup v2, with the GKE-managed
`efficiency-daemon` DaemonSet running (`kubectl get daemonset -n kube-system efficiency-daemon`); a
cluster that does not support it sets each limit to the request, so the implementer above would run
`Guaranteed` at an 8Gi limit
([Resource requests in Autopilot](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests#resource-limits),
"Setting resource limits in Autopilot"). A pod that requests an accelerator or a specific machine
series (the `Performance` class) can burst on any GKE version that supports that hardware. Where it
can burst, a general-purpose, `Balanced` or `Scale-Out` pod bursts only into the node's burstable
capacity — the sum of the requests of the pods on that node, plus a small buffer Autopilot adds for
its system pods — and GKE terminates containers in lower-QoS pods first when memory use passes it;
a `Performance`-class or accelerator pod bursts into the node's allocatable instead
([Configure Pod bursting in GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/pod-bursting-gke#burstable-capacity),
"Bursting availability in GKE" and "Burstable capacity in GKE"). That node is itself sized from the
requests: GKE adds up the cpu, memory and ephemeral-storage requests of the pods and DaemonSets that
will run on the new node and rounds them up to the nearest machine type in the series that holds
them, and Autopilot bills that VM
([Optimize Autopilot Pod performance by choosing a machine series](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/performance-pods#how-gke-selects-machine-type),
"How GKE selects a machine type" and "Pricing"; all three pages read 2026-10-18). So on Autopilot
every class sets the burst ceiling from the requests. Legion's issue and controller pods carry
`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`, which on Autopilot gives each a node of
its own whenever possible ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)), so on the
general-purpose platform an issue pod's ceiling is about its own summed requests, whatever its
limits say: with the implementer at `memory_request: 8Gi` and every other role at its default, the
pod requests 1 + 1 + 8 + 6 + 4 + 1 = 21Gi, so the implementer peaks near 21Gi even while every
sibling idles, and never reaches its 32Gi limit. On a `Performance` node the ceiling is those
requests rounded up to a machine type, and a lower request still picks a smaller, cheaper VM. To
give a role real headroom on Autopilot, raise the pod's summed requests — the role's own request,
or that of a sibling that sits idle while it works — rather than its limit alone.

The defaults were sized from an issue pod on the production cluster (measured 2026-10-09, under
`GOMAXPROCS=3`): `go test ./...` of `packages/daemon` peaks at about 1.45 GiB of summed RSS, a cold
`go build ./...` at about 0.97 GiB, `bun test` of a plugin package at about 1.2 GiB, Biome and
`bun install` at about 0.8 GiB each and `tsc` at about 0.75 GiB; a role's Oh My Pi process is about
1 GiB after half an hour of work, and a headless Chromium's largest process about 0.45 GiB. A
lane-running role's Go test lane, its agent and a browser sum to about 3.9 GiB, at the limit of the
4Gi they were first given, and a `Guaranteed` pod OOM-killed mid-turn stalls its tree; so the
implementer's and tester's 6Gi holds its agent, one lane and a browser with room over that sum,
the reviewer's 4Gi holds its agent, a browser and its review pair, and the pod's 19 GiB holds six
agents beside them.

Legion sets no affinity itself (GKE Autopilot adds its own to an annotated pod, [Anatomy of a
Sandbox pod](#anatomy-of-a-sandbox-pod)): each pod owns its volume and shares nothing with another
pod, so the scheduler bin-packs it wherever the `legion` pool has room for its reservation, and
Karpenter adds a
node under the pool's limits when none has, of the smallest type the pool's requirements allow. In
production (read 2026-10-08) those are `instance-cpu Gt 3` and `instance-memory Gt 65535`, so the
floor is an 8-vCPU, 64 GiB type whose allocatable is about 7.9 CPU and 60.8 GiB: a 3 CPU / 19 GiB
issue pod fits, two per node (7.9 / 3 by cpu, where 60.8 / 19 would place three), cpu the binding
dimension. On GKE Autopilot each issue and controller pod tends to get a node of its own instead
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)), so node count there tracks pod count rather
than this two-per-node packing. A pod relaunched onto another node waits for its `ReadWriteOnce`
volume to detach from the old one, which shows as transient `FailedAttachVolume` or `Multi-Attach`
events until the old pod is gone; then it runs on.

Under gVisor the per-container cgroup changes nothing inside the sandbox: `runsc` sizes the sandbox
from the pod's cgroup, so inside a pod `nproc` is max(2, ⌈Σ cpu limits⌉) and `/proc/meminfo`'s
`MemTotal` is about Σ memory limits of the pod's containers, and an active role may use the pod's
whole reservation while its siblings idle. The reservation is the pod's working set, not one
container's; each container's own request and limit are what the API shows, and its requests are
what the scheduler counts.

**The bound.** Concurrently running issue pods are bounded by what the pool's `limits.cpu` and
`limits.memory` leave for pods of the per-pod sum of requests: at the defaults, two pods fit a floor
node and `limits.cpu: 256` places about 80 three-CPU pods (less what the nodes' daemonsets hold),
and `limits.memory` divided by 19 GiB bounds them too when that is the smaller (a `Burstable` role's
lower request lowers the sum, and so places more) — where the
tree-volume layout ran one tree per node, each pinned to a node of its own. `admission_cap` bounds
roots alone: a tree of N children runs N+1 pods at once, and nothing caps concurrent child pods. A
pod the pool cannot place stays `Pending`, unscheduled; once it has been for longer than
`worker_boot_timeout_seconds` the boot watchdog reads it dead ([Liveness rules](#liveness-rules)),
one launch failure, its relaunch meets the same pool, and the claim fails once its launch failures
run out — no later than its registration deadline. Two pods beside the issue pods count against
the same limits: the image probe pod, its own 250m / 1Gi, present at every boot and daemon restart
for as long as the probe runs; and, under `controller: daemon`, the controller's pod, 1 CPU / 4Gi
at the default, for as long as the daemon runs. Size the pool's limits for
`admission_cap × (1 + the children a tree runs at once)` pods of the per-pod sum plus those two,
or keep `admission_cap` within what the limits place.

A pool with no room for the probe pod itself does not refuse the boot. The probe pod stays
`Pending` with `PodScheduled=False Unschedulable`; once it has been so for longer than
`worker_boot_timeout_seconds` — the same bound the liveness rules give an issue pod, long enough
for Karpenter to add a node when the pool's limits allow one, so a cold pool scaling up is not
read as full — the attempt ends waiting on capacity (`stuck`, `internal/runtime/sandbox/probe.go`).
That stop can always fire within the attempt: the attempt's own budget is derived from
`worker_boot_timeout_seconds` whenever `slow_command_timeout_seconds` would otherwise leave it less
room (`ProbeBudget`, "Image size and pull time" above), so the attempt outlives the boot timeout by
construction. Ending the attempt, its Sandbox deleted, the daemon logs `boot probe is waiting on capacity; running it again` with
`probe=worker image`, the pod's name and the scheduler's own reason in `detail` (`probe pod
legion-probe-<project>-<digest12> is Unschedulable: 0/83 nodes are available: 25 Insufficient cpu …`)
and `retryIn`, the probe's longest interval, 5 min. It then creates the probe Sandbox again, as many
times as it takes: such a wait is counted against none of the probe's six attempts
(`bootprobe.Run`, `Outcome.Waiting`), since a full pool says nothing about the image and a daemon
that exited would not make room. The daemon stays up through it, but serves nothing yet: the probe
runs before the API is served (`plan.probe` precedes `serve` in `internal/daemon/daemon.go`), so
`legion state` answers nothing while the probe waits, and the log is where the wait shows. Once a
probe pod is placed and passes, the `pool-capacity` row of
[The deployment's capability report](#the-deployments-capability-report) records how many attempts
waited and the scheduler's last reason, open until `capabilities.decided.pool-capacity` names a
reason; a boot whose probe pod scheduled at its first attempt reads it `present`.

**The node-disk bound.** Pods of unrelated trees share a node, and what a role writes outside every
volume — its root filesystem: `$HOME`, Oh My Pi's state home with its Chromium profiles and logs,
the Go and Bun caches a build fills — is the node's disk, the node's allocatable ephemeral storage
(its root volume; read it with `kubectl get node <n> -o
jsonpath='{.status.allocatable.ephemeral-storage}'`). That disk must hold the sum of the pods'
actual disk use, which Σ of their containers' `ephemeral_storage` limits bounds above: a six-role
issue pod's limits sum to 80Gi at the defaults (20Gi for the implementer and tester, 10Gi for each
other role), its requests to 6Gi. A container past its own limit has its pod evicted — the
offending issue's pod alone — rather than the node reaching `DiskPressure`, where the kubelet
evicts by its own ranking and another tree's pod can go. The request is what the
scheduler counts against the node's allocatable ephemeral storage; at the default 1Gi it binds
almost nothing, so a node's disk can be oversubscribed by the pods' limits. In production (read
2026-10-09) the Legion node class carries one 700Gi gp3 root device, and a node's allocatable
ephemeral storage is about 629 GiB of it; with CPU and memory placing about two issue pods per node,
their limits sum to about 160Gi of those 629 GiB, so the bound has room before any request is
raised. An operator with a known root volume raises `ephemeral_storage_request` toward the role's
expected use so the scheduler reserves disk and places no pod a full node could not hold.

### Volume retention

Node loss reattaches the EBS volume to a replacement node and the pod resumes where it left off (on
another node it first waits for the detach, which shows as transient `FailedAttachVolume` or
`Multi-Attach` events). What follows is the file store's (`session_store: pvc`, the default), where
the issue's sessions are on the volume with its clone; under `session_store: postgres` no pod is
told `LEGION_EXPECT_ISSUE_VOLUME` (`issuePod.prepare`, `issuePod.provision`), so none of it runs: a
lost volume is provisioned again from the issue's pushed branch and each role's session continues
from the table ([Selecting the store](#selecting-the-store)). Volume loss — a PVC that lost the
issue's clone and every retained session of the issue, as a fresh EBS volume after node loss can —
is detected through the `workspace-init` init container, which every role's launcher waits behind:
it expects the issue's clone (`LEGION_EXPECT_ISSUE_VOLUME`) once the launching claim resumes a
session or any stored claim of the issue recorded one (`IssueHasSessions`), and exits 3 only once
both the clone and every retained session are gone. Under `restartPolicy: Always` a failed init
container never turns the pod `Failed`; the kubelet leaves it `Pending` in `Init:Error` or
`Init:CrashLoopBackOff` and keeps retrying it forever on its own. The runtime does not wait for a
phase that will not come: `evaluate` reads the init container's current or last-terminated state as
**Gone**, with `WorkspaceLost` true for workspace-init's exit 3 (`the issue's volume was lost: …`,
`internal/runtime/sandbox/observe.go`), and `relaunch` replaces the init-failed pod outright (delete,
then recreate) instead of waiting on its launchers. The loss is one issue's: the supervisor logs
`supervise: the issue's volume was lost with the session; relaunching a fresh session`
(`internal/supervise/machine.go`) and stamps every other claim of that issue `workspaceLost`,
dropping the session each recorded on the volume, and touches no other issue of the tree, whose
volumes are their own; each stamped claim's replacement starts fresh from the committed issue
bookmark until its own fresh session registers, even after the new pod rebuilt the clone, and a role
first created later uses the ordinary fresh-worker path. A pre-loss worker is never downgraded to
an ordinary missing-session failure. Its prompt says nothing of the loss: the relaunch names the
ref the workspace is recovered from in the init container's
`LEGION_WORKSPACE_RECOVERED_FROM` (`initEnvironment`, `manifest.go`), and `workspace-init provision`
records it, with the commit it recreated the workspace at and the reason `volume-missing`, in the
workspace's `.legion/<KEY>/workspace-recovered.json` (`writeRecoveryMarker`,
`cmd/legion/workspace_init.go`), the directory the issue's committed handoffs live in.

One PVC per issue, `issue-legion-<project>-<issue key lowercased>` (`ReadWriteOnce`, `issue_volume`,
`storage_class`), created by Agent Sandbox from the issue Sandbox's one claim template when the
Sandbox is first created, owned by it and labelled by project and issue, never by tree, so
`kubectl -n legion get pvc -l legion.dev/issue=<KEY>` finds it; the controller's Sandbox owns one the
same way, `issue-legion-<project>-controller`, labelled by project and role. Nothing annotates or
ages a volume; it goes with its Sandbox, and the Sandbox goes:

- at a child issue's close as `done`: its claims are retired with their sessions dropped
  (`issue_close`), and the close's durable effect foreground-deletes the Sandbox, the PVC with it
  (`SuspendIssue` with release, `internal/runtime/sandbox/issue_suspend.go`), logging `sandbox
  runtime: released the closed issue's Sandbox and the volume it owned`; a later `todo` starts the
  child fresh. A child set `backlog`, `icebox` or `triage` instead keeps its Sandbox, `Suspended`,
  and its volume until the tree closes;
- at the tree's close, linger expiry: the tree's cleanup foreground-deletes every remaining issue
  Sandbox of the tree, in any order, each taking the volume it owns (`CleanupTree`,
  `internal/runtime/sandbox/cleanup.go`), except one whose home is another tree (a child whose
  hand-over is pending, or one carried into a tree of its own that it is in or can still be admitted
  into), which it hands over to that tree, relabelled and `Suspended`, keeping its volume;
- by the orphan sweep, for a Sandbox of a tree whose cleanup confirmed or that has no lifecycle:
  the sweep keeps every Sandbox of a live tree, and one whose home is the tree it is labelled with (a
  carried child waiting in admission's line, or inside its own linger), and, deleting any other,
  takes its volume with it through the owner reference (`ReconcileOrphans`,
  `internal/runtime/sandbox/sandbox.go`). A carried child that leaves the line before admission
  opens its tree loses its Sandbox there: once its own linger ends when it is set to `backlog`,
  `icebox` or `done`, and at the next sweep when its label is taken off.

A child of a closed tree carried into a tree of its own keeps its Sandbox, its volume and its roles'
sessions: the Sandbox is relabelled for the new tree, by the new tree's launch (`ensureSandbox`,
`internal/runtime/sandbox/relaunch.go`) or by the old tree's cleanup when that comes first
(`handOverSandbox`), and the Sandbox alone — the daemon's identity has no PVC verb, which is why the
claim carries no tree label to go stale. A child re-admitted only after its old tree's cleanup took
its Sandbox and volume (one that had left the workflow before the close) starts fresh on a new
volume with `the issue's volume was lost` logged once, its re-pointed claims' sessions gone with the
volume ([Kubernetes runtime: the Go daemon on Agent Sandbox](#kubernetes-runtime-the-go-daemon-on-agent-sandbox)
says why `Retree` keeps them). A near-full volume slows jj's
working-copy snapshot before a push, which is why `legion push`'s grant lives `credential.pushTTL`
(5 minutes) in place of the usual 60 seconds (`internal/credential/grants.go`;
`docs/solutions/legion/worker-pane-shell-gotchas.md` has the mechanics).

### Liveness rules

The daemon probes a pod by reading it and consulting the worker stream's live registrations:

- the Sandbox itself not found (deleted, or never created) → **dead (gone)**, distinct from a
  present Sandbox with no pod;
- pod not found (the Sandbox is present, with no pod of its own) → **dead (gone)**, naming the
  Sandbox's operating mode, its `Suspended` condition if any, and any same-named pod that is not
  this Sandbox's (a stranger holding the name);
- pod present but its uid is not the recorded one → **dead (not the recorded process)**; the stop that
  follows refuses to delete it (the delete carries the recorded uid as a precondition, and Kubernetes
  answers 409), so a stranger wearing a reused name is never destroyed;
- pod carrying a `deletionTimestamp`, or in phase `Succeeded` or `Failed` → **dead (gone)**; for a
  `Failed` pod the last 20 log lines of the failing container (the init container when it exited
  non-zero, else the main one) are quoted in the daemon log;
- `Pending` with the `workspace-fetch` or `workspace-init` init container **running** → **alive**,
  whatever the pod's age: the pod is provisioning its working copy (`workspace-fetch`'s wait for
  the repository cache, then its stream, verification and delta under one
  `workspace.FetchTimeout` rather than `workspace.CommandTimeout`, all within
  `workspace.FetchBudget` ([Trust model](#trust-model-the-provisioning-token)), then
  `workspace-init`'s shared clone of the feed, whose steps that follow the repository's size share
  one `workspace.CloneTimeout`, and its other commands, each up to `workspace.CommandTimeout`;
  it waits on no other pod
  and takes no lock, since the clone it provisions is the issue's own, on the issue's own volume),
  and a live initialiser is a live process — as the tmux runtime's own in-process provisioning is.
  The launch that created the pod waits for the role's launcher, which starts only once both init
  containers have finished. Until it first sees an init container running it waits
  `worker_boot_timeout_seconds`, the image pull's bound; from then on, `worker_boot_timeout_seconds`
  counted from the last moment it saw one running, and never longer than that plus
  `workspace.FetchBudget` and `workspace.CloneTimeout` together (100 min;
  `sandbox.Runtime.ProvisionBound`) from the first (`awaitRoleLauncher`,
  `internal/runtime/sandbox/relaunch.go`): each term is the real bound of a distinct phase, run one
  after the other, `workspace-fetch`'s whole fetch and then `workspace-init`'s shared clone of the
  feed. The fetch started no later than that first sighting and the shared clone after the fetch
  ended, so both end inside the bound, each failing on its own, naming why, when it runs out of
  its own — the fetch's failure naming the repository cache's state — and `workspace-init`'s other
  commands then have at least the boot timeout. A launch that gives up while `workspace-fetch`
  still runs says so, with the cache's state. A pod that provisions for twenty minutes is one
  launch, not a launch failure per boot timeout, and a launcher that has not connected the boot
  timeout after provisioning ended still fails the launch. The wait runs inside whatever asked for
  the launch, and a start row asks from the outbox, on the issue's own lifecycle lane: only that
  issue's later lifecycle rows — its stops, suspensions and later starts — wait behind it, while
  other issues' rows and the issue's own board writes and notices run meanwhile. During a GitHub
  outage that is the fetch's five attempts and four waits, about 2 minutes when GitHub refuses at
  once and about 12½ minutes when every connect times out (127 s each), after which the fetch fails
  and the launch ends; each relaunch of the claim then holds the issue's lifecycle lane as long
  again, and the issue's later lifecycle rows wait on it. A repository cache that is slow to become
  ready and a transfer that keeps making progress can hold the launch for the fetch's whole 70
  minutes, and `workspace-init`'s shared clone of the feed, for a large repository, up to 30 more.
  Each launch try is bounded inside the runtime (`sandbox.Runtime.LaunchBound`,
  `internal/runtime/sandbox/relaunch.go`) at the sum of the try's waits and bounded calls — three
  boot timeouts; the repository cache's waits, five more boot timeouts and the calls of one other
  ensure of the cache that may hold its turn ahead of the try; the launcher wait's backstop of
  twice the boot timeout plus `sandbox.Runtime.ProvisionBound`; the termination grace; and the
  try's client calls at their 30 s timeout — about 2 h 50 min at the defaults; a try that reaches
  it is charged a launch failure and the next try runs, while a try the daemon's own stop ends is
  still charged nothing.
  Once the launch returns, the boot watchdog re-arms on the claim, bounded by its registration
  deadline
  (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, default 360 s,
  6 min): under Kubernetes, the deadline carries an added bound of `sandbox.Runtime.ProvisionBound`
  until the shim's first hello, and from there the daemon re-arms the base deadline alone, the same
  one a tmux pane runs under throughout. A pod that never says hello is retired at the launch's return
  plus the base deadline plus the bound, armed as one (106 min at the defaults); one that says hello
  and never registers is retired at hello plus the base deadline alone (6 min from the hello); and
  one whose agent registers and never says it is ready is retired at its registration plus the base
  deadline alone (6 min from the registration, again from a daemon restart that finds it
  registered), then resumed as the same session one generation later and counted as a launch
  failure. A tmux pane carries no bound to begin with, since it starts the agent at once with no
  init phase. Nothing serializes the pods of one tree against each other: each issue pod is created
  as its launch comes, and only the relaunches, suspension, release and orphan deletion of one pod
  take turns, keyed by its Sandbox's name (`lockPod`);
- the init container **terminated non-zero** (its current state, or `LastTerminationState` once the
  kubelet has already restarted it) → **dead (gone)**, its log tail quoted, `WorkspaceLost` set when
  it is `workspace-init` exiting 3; under `restartPolicy: Always` the pod never turns `Failed` for
  this — the kubelet leaves it `Pending` in `Init:Error`/`Init:CrashLoopBackOff` and keeps retrying
  the container itself — so the daemon reads the failed attempt directly instead of waiting for a
  phase that will not come, and `relaunch` replaces the pod outright rather than waiting on its
  launchers;
- `Pending`, unscheduled (`PodScheduled=False`), for longer than `worker_boot_timeout_seconds` →
  **dead (gone)**, with the pod's events quoted; the boot watchdog's existing path retires it and
  its stop deletes the pod. A pod already scheduled but stuck before either init container starts
  — an image pull or a volume mount that never finishes — is not caught here: it stays **alive**
  under the `Pending` rule below, bounded only by the registration deadline above, the same as any
  other pod still provisioning;
- the Sandbox's `Ready` condition reports `MultiplePods` or `ReconcilerError` → **unknown**: the
  Sandbox controller itself cannot resolve the pod it owns, so nothing here can either;
- `Pending` otherwise → **alive**;
- `Running`: judged through the recorded role's own container, never the whole pod — a neighbour
  role's container restart changes nothing here. No status at all for that container → **unknown**.
  A connected role `legion launcher` reporting a child of the recorded generation is **alive**,
  even when Kubernetes still shows the container's previous instance terminated for a moment after
  a restart: that one answer is asked first, and it alone outranks the terminated status. Otherwise
  a terminated role container → **dead (gone)**, its log tail quoted. Otherwise, with no launcher
  connected → **unknown** (a booting or redialing launcher is not death; the boot watchdog
  decides). Otherwise the connected launcher's remaining answers: a child of another generation is
  **dead (not the recorded process)**, a last-reported exit matching the recorded generation is
  **dead (gone)**, a last exit of another generation is again **dead (not the recorded process)**,
  and no child ever reported is **dead (gone)**;
- phase `Unknown` → **unknown**;
- the API read failed → **alive** if the pod's stream is registered (live proof), else **unknown**.

`unknown` never marks anything dead by itself. A graceful stop sends the RPC `shutdown` frame over the
registered stream, waits up to the stop timeout, then deletes the pod with that many seconds of grace
(0 when the caller skips the graceful step) and the recorded uid as precondition, then deletes the
per-pod Secret. Before a replacement generation is created, the previous generation's pod is deleted
the same way and awaited until it is gone (force-deleted at grace 0 if it outlives the stop timeout):
two generations never share a working copy.

A phase worker or sub-architect pod that dies mid-task — its container crashed, or the pod was
deleted — is relaunched by the daemon itself, as the same agent one generation later
(`legion-<issue>-<role>-g<n+1>`, its command carrying `--resume=<the recorded session>`), and
prompted with the daemon's catch-up rather than a replay of the interrupted task (LEGION-179). The
death is seen twice over: at once, when the pod's worker stream closes and the daemon's one
reconnect (`connect` awaiting a fresh registration for `worker_rpc_timeout_seconds`) finds none;
and, for a death the stream never reported, on the next resync tick, which probes every located,
ready-confirmed worker claim with the rules above. Each death counts one `launchFailures`, so a
pod that keeps dying before its `/worker/ready` reaches `worker-died` at `MAX_LAUNCH_FAILURES`
exactly like a boot that never confirms; a confirmed ready resets the count. A finished worker whose
pod dies while idle — no longer its issue's active phase, nothing queued for it — is retired, not
relaunched (`… after finishing: <issue>'s active phase is <role> …; retired, not relaunched`); the
architect's next `spawn_worker` resumes it. To exercise this on a kind cluster, crash the process
from the node rather than deleting the pod gracefully (a graceful stop lets the shim shut OMP down
cleanly): `node=$(kind get nodes --name <cluster>)`, `cid=$(docker exec "$node" crictl ps -q --name
worker --label io.kubernetes.pod.name=<pod>)`, `pid=$(docker exec "$node" crictl inspect --output
go-template --template '{{.info.pid}}' "$cid")`, `docker exec "$node" kill -9 "$pid"` — a
`kill -9 1` from inside the pod's own pid namespace is dropped by the kernel. Expect the daemon log
line `<role token>: worker process died (its stream closed and the one reconnect was refused);
launch failure 1/3; relaunching the same agent with --resume and its catch-up`, then
`respawning <issue> by resuming OMP session <path>`, within seconds.

### Trust model: the provisioning token

The provisioning token, the implement App's installation token, is a credential for the whole
repository, and every agent of an issue can write the issue's volume — its own issue's alone, since
no other issue's pod mounts it: the clone's hooks, its git and jj configuration (a legacy
`.jj/workspace-config.toml`, which jj would migrate into what it reads, included), its remote URL,
its `http.proxy`. git and jj obey all of it — they run hooks, the git jj is told to run,
working-copy filters and `ext::` transports, and send credentials through the proxy the
configuration names — so the provisioning clone, which runs with that token before any role of the
issue exists, happens in no process whose git or jj an agent's plant can reach. The Go
coordinator's pods (`packages/daemon`) keep to that with two init containers:

- **`workspace-fetch`** mounts the provisioning Secret, an in-memory `TMPDIR` of its own, and the
  issue volume's `feed` directory alone (a `subPath`, at `/legion/feed`), which every other
  container of the pod mounts read-only, and runs `legion workspace-init fetch --repo
  <owner>/<repo> --feed /legion/feed`: a fresh feed is the repository cache's tar unpacked into a
  feed of its own making (`git init --bare`) and a `git fetch` of the delta from
  `https://github.com/<owner>/<repo>`; a feed the volume already holds gets that fetch alone —
  reading no git configuration but the feed's own, which only this container writes
  (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_PARAMETERS` unset), with a
  one-shot credential git asks for `https://github.com` alone. The cache is asked with the
  cache's own token, from the same Secret, and an answer naming another repository is refused.
  Its tar may hold only the repository's refs and packs (`HEAD`, `packed-refs`, `refs/`,
  `objects/pack/pack-<id>.pack`), each a regular file or a directory inside the feed, never a
  configuration, hook, index or alternates file; the first entry that is not is refused by name
  and the fetch fails (`FeedEntry`, `internal/workspace/clone.go`), as it does when a pack does
  not index or the history is not whole. A feed is filled beside its final path and renamed into
  place once whole, with git's automatic garbage collection off and every fetch's objects kept as
  a pack, since the issue's clone keeps its objects there. It mounts no other part of the issue's
  volume and no config home, and it takes an existing feed repository only in the shape it writes
  (no symlink, gitfile, hook, alternates, configuration key of anyone else's, or configuration line
  outside git's own form; see "Anatomy of a Sandbox pod"), so nothing an earlier release's agents
  wrote at `/legion/feed` runs in it. That check is also what guards a carried-over volume whose
  `feed` an earlier agent made a link to another directory of the volume: the kubelet mounts the
  link's target as the feed, and agents may still change what the next fetch reads there, through
  that directory's own path or the link itself, which is no mount point in their containers, so the
  check, run before any git in an init container no agent runs beside, is what keeps the fetch
  from running what they wrote. Every other provisioning command is
  bounded by `workspace.CommandTimeout` (5 minutes, fixed), but a transfer's duration follows the
  repository's size and the network's speed, not a fixed step in provisioning: once the fetch may
  start — at once, or once the repository cache is ready, after a wait of at most 40 minutes — the
  cache's stream, its verification, every attempt of the transfer from GitHub and its read of
  GitHub's `HEAD` run under one `workspace.FetchTimeout` (30 minutes) instead, each given what
  remains of it, so the fetch's waits, transfers and checks end within `workspace.FetchBudget` (70
  minutes). A transfer
  that fails transiently is run again: git's fatal exit (128) with a cause line — `RPC failed; …` for
  a request of the transfer, `unable to access '…': …` for the first request — naming 408, 429 or a
  5xx, or a connection that could not be made or broke (connect, DNS, reset, timeout, empty reply,
  a transfer cut short, an HTTP/2 stream reset, a TLS session closed); or, with no cause line, a
  pack stream that ended early. Each retry logs the attempt's error on stdout as one line, the
  token replaced by `***`, removes whatever a failed clone left beside the feed, and waits 5 s,
  15 s, 30 s, then a minute: four retries at most, so a transfer still failing after the fourth
  fails the fetch. No retry starts past the bound. Any other failure — a token refused, a repository not found or not
  granted, a certificate that does not verify, a curl error the list does not name — fails the
  fetch at once (`transientCloneFailure`, `internal/workspace/fetch.go`). The launch waits for the
  pod's launchers while either init container runs, up to `sandbox.Runtime.ProvisionBound` (100
  minutes) plus its boot timeout from when it first sees one running (above, "Liveness rules"), so
  the fetch, its wait and retries included, spends no launch failure, and `workspace-init` has its
  shared clone's 30 minutes and at least the boot timeout after a fetch that ran its whole budget.
  The fetch reads the token from its file anew as every transfer
  attempt from GitHub starts, taking the file as it stands then, and while the pod's
  `workspace-fetch` has yet to finish, the gh refresher's minute brings its provisioning Secret to
  the implement App's current lease (`refreshProvisioningToken`,
  `internal/runtime/sandbox/ghcredential.go`), the cache's token beside it kept, logging `sandbox
  runtime: provisioning token refreshed`; the daemon re-mints a lease once less than 20 minutes of
  it remain (`internal/appauth/appauth.go`) and the kubelet rewrites the projected file within its
  sync period, so a delta after a long wait on the repository cache, or a retry late in a long
  fetch, presents a token that is still good. A pod past its fetch keeps its Secret as it is.
- **`workspace-init`** mounts the issue's volume, the feed read-only over it, and the config home —
  never the Secret — and runs `legion workspace-init provision --issue <KEY> --repo <owner>/<repo>
  --default-branch <name> --root /legion --credential-helper '!legion git-credential' --feed
  /legion/feed`: the clone is a `git clone --shared` of the feed, its origin set to
  `https://github.com/<owner>/<repo>`, and its fetch reaches that remote at the feed over git's
  file transport, bringing the default branch and the issue's branch alone, each by its exact
  name, then the workspace add, `update-stale`, and the configuration writes, once it has removed
  the clone's stale git locks ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)). The shared
  clone copies no object, but its checkout of the working tree and jj's import of every commit
  follow the repository's size, so the two share one bound of their own, `workspace.CloneTimeout`
  (30 minutes), the import given what the checkout left, rather than `workspace.CommandTimeout`;
  every other command here keeps the 5 minutes.
  It takes no lock and waits on no other pod: the clone is the issue's own, on the issue's own
  volume, and no other provision shares the repository. What an agent of the issue planted can run
  there, with nothing to take that the agent does not already hold.

`packages/daemon/internal/runtime/sandbox/boundary_test.go` runs both containers exactly as the
manifest states them against nine such plants, with every one of provisioning's git and jj pins made
ineffective.

No jj configuration an agent writes ahead of a command reaches provisioning's jj either: jj 0.38 and
later keep a repository's and a workspace's configuration in the config home (the pod's own,
in-memory and empty at every start), and the one way a file on the volume becomes jj configuration,
jj migrating a legacy `.jj/workspace-config.toml` or `.jj/repo/config.toml` that has no id file
beside it, is closed by removing that file before each jj command provisioning runs
(`disarmLegacyConfig`, `internal/workspace/config.go`). Every jj command run in a workspace names it
with `-R`, so jj never walks up to an ancestor's `.jj`, and a jj command is refused when the `.jj` it
would open, or the clone's `.jj` or `.jj/repo`, is a symlink or anything but a real directory; when
any directory of the layout between the root (`/legion` in a pod, the state directory under tmux,
which itself may be one) and a workspace or the clone (`repos/<host>/<owner>/<name>`,
`workspaces/<owner>/<name>/<issue>`) is a symlink, which the refusal
names; when it runs in a workspace that has no `.jj`; and when the workspace's `.jj/repo` names any
other directory, followed through symlinks and `..` as jj itself follows it, or is neither a
directory nor a regular file (`guardWorkspace`, the same file). The worker image's jj is 0.45; on
the tmux runtime, which runs the host's jj through the same code, the daemon refuses to start with a
jj older than 0.38 (`resolveTools`, naming `LEGION_JJ_PATH`), since before 0.38
`.jj/repo/config.toml` is the repository's live configuration.

What the boundary protects is identity. Since LEGION-631 each role container holds its own App's
tokens, read-only, in the gh bundle its `LEGION_GH_OWNERS_DIR` names
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)): an implement-role container (the
implementer's, the merger's) holds the implement App's tokens — for the issue's owner the same App,
from the same lease, as the provisioning token — and a review-role container (the architect's,
planner's, tester's, reviewer's) the review App's, every owner but the issue's narrowed to reading;
and each role's `gh` and `git` run in a container that mounts
the issue's volume, with whatever an agent of the issue planted in the clone's configuration. A plant in the
implementer's container, then, reaches a token the implementer already holds for its push and its
pull request. What no plant reaches is the other App: no review-role container ever holds the
implement App's token (the review App cannot resolve a review thread on the implementer's pull
request, so the reviewer names the bot threads it accepted to the implementer, which resolves them
with its own `gh`; the daemon serves no thread route), and no implement-role container the review
App's, so an approval posted as the review App is a review-role container's, and a push or a pull
request as the implement App an implement-role container's.

On the **tmux** runtime there is no such boundary: panes run under the daemon's uid and can read its
0600 credential files, and the daemon's credentialed clone and fetch run in the host's shared clone
itself, the one every issue workspace of the daemon is a `jj workspace` of. Provisioning's pins
there — no git hook (`core.hooksPath=/dev/null`), the git the daemon resolved at boot as jj's
`git.executable-path`, `GIT_ALLOW_PROTOCOL=https`, no working-copy snapshot in the credentialed
fetch, the one-shot credential scoped to `https://github.com` with no askpass, and
`GIT_CONFIG_PARAMETERS` unset — are defence, not a boundary. They hold the settings they name; they
do not stop every program the shared clone's own git or jj configuration can name. One example of
what they leave open: a tree-written `http.proxy` with `http.sslVerify=false` still sees the token
on its way to github.com.

### The daemon's Kubernetes request rate

The runtime's clients share one client-side rate limiter, 50 requests a second with a burst of 100
(`clientQPS`, `clientBurst` in `internal/runtime/sandbox/clients.go`), over every Sandbox, pod,
event and Secret request it makes; a watch is never paced. Each boot check (the Agent Sandbox
install check and the layout census) runs before the runtime exists and paces itself at the same
rate. The rate is sized for a few hundred roles. A restart has every live role's launcher say hello
again, and each hello reads its role's `-boot` Secret: at 50 a second three hundred launchers'
reads clear in about 4 s. A launch, about eight requests into a running pod and about thirty when
it replaces the pod, waits a fraction of a second more for each launch ahead of it.

A request waits on the limiter as long as its caller allows, a launch as long as the launch's own
deadlines allow, and only then does its own 30 s bound start: client-go starts a client's timeout
after its wait on the limiter (`rest/request.go`), and the runtime's informers use clients of their
own with no timeout, since one would end a watch. A launch the limiter holds back waits its turn
and is charged no launch failure. A pod log read is the exception: it is bounded whole at 30 s,
its wait on the limiter included, since client-go bounds a log stream per attempt and not across
its retries.

An API server under load answers some requests overloaded or failed instead of acting on them: API
Priority and Fairness's 429, once client-go's own Retry-After retries ran out, and any 5xx, such as
the 500 etcd's throttle reaches a client as (`rpc error: code = ResourceExhausted desc =
etcdserver: throttle: too many requests`). Such an answer to one of a launch's requests refused
nothing the launch did, so the runtime sends that request again after a jittered backoff (250 ms,
doubling to 8 s) rather than failing the launch, and the launch goes on from that request once the
server answers it. Each request waits so for up to a boot timeout from its first overloaded
answer, or until the launch step it belongs to ends when that comes first, so a try under a long
throttle can take several boot timeouts, one per request it sends. Two of a launch's waits send a
request of their own, and that request ends with the wait's budget: the wait for a Sandbox being
deleted, and the read confirming the old pod is gone. A failed launch's suspension of
its Sandbox waits the server out for at most 30 s. Each throttled request logs `sandbox runtime:
the API server answered a launch's request overloaded; sending it again after a backoff` once,
naming it, and `… it had answered overloaded` once the server answers, with how many overloaded
answers it waited out. No launch failure is charged while it waits. A request the server still
answers overloaded at its deadline fails the launch with that answer, which says how often and
how long it was waited out, and the failure is charged as any other is.

The rule an operator sees: a 429, a 5xx, or a timeout the server itself reports is waited out; a
refusal of the request (a 403, a 409, a 422, an admission webhook's denial) and no answer at all
(the client's own timeout, a connection that never reached the server) fail the launch, charged.
`serverBusy` in `internal/runtime/sandbox/busy.go` is the one place that decides it. A write sent
again may have been applied the first time: a Secret create refused as already existing, or an
update refused as a conflict, is read and written once more, and a fenced relabel of the Sandbox
refused as invalid is read again, failing the try when the Sandbox has not changed since. While
the API server refuses the informers' own lists or watches as overloaded, a launch waiting for its
role launcher waits on, within its deadline, until the informers list and watch again. Only a
launch's requests wait the server out; a sweep, a claim's suspension or a hello's credential read
fails on the answer and is tried again by its own caller. A charged launch failure is followed by
the supervisor's launch backoff ([Supervision](site/src/content/docs/legion/concepts.md#supervision)).

A role's launcher credential is read once however often its launcher says hello. The read is
single-flight per Secret of a Sandbox and outlives the hello that began it: the hello waits for it
up to 30 s and is then refused, and the launcher's next hello, a second later, joins the same read
or finds what it stored. A read that fails is logged (`sandbox runtime: a launcher Secret could not
be read; …`, naming the Secret and the error) and is not repeated for that Sandbox for 10 s; a
hello meanwhile is refused without a request, so three hundred launchers whose reads fail ask for
at most thirty a second. A launch into a kept pod reads its roles' Secrets to check their binding,
and holds each bound one, so its launcher is not refused for an earlier failed read. The daemon
reads each Secret by name because its RBAC grants `get` and never `list` on Secrets (next section).

### RBAC the Go daemon needs

The daemon runs as a restricted identity (in production, an IAM role mapped to the `legion-daemon`
group). It needs:
- `sandboxes`: create, get, list, watch, patch, delete; `sandboxes/status`: get;
- `secrets`: create, delete, update, get, but never list;
- `pods`: get, list, watch, which the incarnation fence's pod informer reads; `pods/log`: get;
- `services`: get, list, watch, which the Service informer reads, so a launch hands an issue pod
  its repository cache's address only once the Service of that name is the cache's own
  ([The repository cache](#the-repository-cache)); without them the daemon refuses at boot,
  naming the refused list;
- `events`: list;
- a cluster-scoped get on `customresourcedefinitions` named `sandboxes.agents.x-k8s.io`, and a get
  on the `agent-sandbox-controller` Deployment in `agent-sandbox-system`, for the boot refusal when
  either is missing.

It has no pod create or delete and no PVC verb (LEGION-206 Requirement 8).

### The live proof

`scripts/e2e/stage4b-sandbox-tree.sh` drives three real issue trees through the Go daemon on this
runtime, in the production cluster's namespace `legion`, against production Dispatch, the Envoy
listener and NATS, with real agents. What it holds, and what it touches in production, is in
[`scripts/e2e/README.md`](../scripts/e2e/README.md#stage4b-sandbox-treesh).

### Probing a configuration before the daemon boots on it

`legion probe-boot --config <legion.yaml>` runs the part of a Kubernetes daemon's boot that the
operator's worker pod can refuse, from that configuration (`packages/daemon/internal/daemon/probeboot.go`):
boot's reads of the configuration, the caller's environment and the files they name (the loader
that runs neither App's `private_key_command`, then `readBoot`), then the cluster check (Agent
Sandbox installed, no Sandbox of an older layout) and the image probe a booting daemon runs, one
value for both (`sandboxBootOf`). The probe Sandbox is built by the same code from the operator's
pod — its ServiceAccount, volumes, mounts and environment — and judged the same way
(`sandbox.NewProber`). It skips every boot stage that reads nothing of the worker pod: the store,
its legacy-claims fence, the migration and the GitHub App mint, which a booting daemon runs before
the probe, and the NATS connection and the Dispatch listing, which it runs after. On a pass it
prints `probe-boot: OK project=<project> image=<image> model-fallback=<mark> capacity-waits=<n>` and
exits 0; a refusal is boot's own, on stderr, and exits 1.

- **Release.** Run the `legion` of the release the daemon will restart on. The probe's daemon API
  contract, the role prompts' references, the probe command, its reservation and its verdicts are
  compiled into the binary, so a CLI older than the daemon can pass a probe the daemon refuses.
- **Where to run it.** As a one-shot pod of the image of the release the daemon will restart on,
  under the daemon's ServiceAccount, with the daemon pod's environment and mounts: that matches the
  release above and the identity, the environment and the files below at once, and no credential
  leaves the cluster. Minting a token for the daemon's ServiceAccount onto a CI runner or any other
  host hands that host every grant the daemon holds, Secret writes and Sandbox patches included, not
  only the subset probe-boot uses, and the host must also hold every bearer file the configuration
  names.
- **Identity.** It acts as whoever `runtime.kubernetes` resolves to in its own process: the daemon's
  identity only with the daemon's kubeconfig and context, or in a pod under the daemon's
  ServiceAccount. It uses a subset of the daemon's grants above: `sandboxes` create, get, list,
  watch and delete; `pods` list and watch; `services` list and watch; `pods/log` get; `events` list; the get on the CRD and on
  the controller's Deployment. It reads no `secrets` and patches no Sandbox. The probe pod itself is
  created by the agent-sandbox controller with the operator's ServiceAccount, so its result is the
  daemon's boot's own when it runs as the daemon's identity; under another, an RBAC or admission
  rule that depends on who creates the Sandbox can answer differently.
- **Environment.** Run it with the daemon's environment. With `nats_nkey_seed_file` unset, whether
  the caller's environment holds `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` decides both the probe
  container's providers pointer to the seed and the probe's check of the providers Secret's seed
  against the pane NATS user, so a caller without them runs another probe than the daemon would.
  It also reads every secret file the configuration names, as `legion start --check-config` does,
  so the host it runs on holds all of them. Of their contents, only the pane NATS seed's enters the
  probe: it decides the pane NATS user the probe checks the providers Secret's seed against. Every
  other file only has to pass boot's reads.
- **Its own probe Sandbox.** A booting daemon names its probe Sandbox for the project and the image;
  probe-boot names its own for its run as well (`legion-probe-<project>-<digest>-<run>`). So it may
  run while that project's daemon boots, or beside another probe-boot, and none takes another's
  Sandbox for a leftover. It deletes its Sandbox when it ends, a SIGTERM or SIGINT included; after a
  SIGKILL, or one cut off mid-create, the Sandbox ends at its `shutdownTime`.
- **Capacity.** It waits on room in the Legion pool without limit, as the daemon's probe does (a
  waiting attempt counts against no retry), so the caller bounds it: a CI job's timeout, or
  `timeout <duration> legion probe-boot …`. The daemon's own probe runs at boot, before it launches
  any pod; probe-boot runs while the daemon serves, so its pod (`probeReservation`) competes with
  live issue pods for the pool's capacity until it finishes, and on a pool with a hard cap it can
  delay an issue pod's launch or relaunch.

A change to the worker pod (a new projected token, a ConfigMap mount, a variable) is proven with it,
run as the daemon's identity, on a cluster that admits Legion's pods before any daemon restarts on
it: a pod the cluster would not admit, mount or start fails here as it would fail the daemon's boot.
A pass holds only against the cluster's objects as they are at the run, since the probe mounts the
ConfigMaps, Secrets, ServiceAccount and providers Secret that exist then. A change that adds one of
them applies it first, and fails here at once until it does, naming the object — the missing
ConfigMap or Secret and the volume the pod block gives it to (`the probe pod <name> cannot mount
volume "<volume>", which runtime.kubernetes.pod.volumes gives it from ConfigMap "<name>": …`), as
the daemon's boot would; a change that removes or renames one the pod still names passes here and
fails at the restart.

## Kubernetes runtime (TypeScript daemon)

This section is the TypeScript daemon's runtime, which it no longer runs: it refuses `runtime:
kubernetes` at config load (LEGION-286), and the section goes with `packages/daemon` at Stage 7. With `runtime: kubernetes`, the daemon runs the tree's root architects and phase workers as one
Kubernetes pod per process, on one disk volume per issue tree. The controller always remains an
interactive tmux pane on the daemon host: `runtime: kubernetes` selects where roots and workers run,
never the controller. Attach with `tmux -L legion-<project> attach -t legion-<project>`. LEGION-25
Part B (`legion controller start`, "Operator-launched controller" below) is unaffected and optional.

The daemon keeps owning retries, generations, the same-agent `--resume`, and the running-worker cap
exactly as it does on a single machine; Kubernetes only supplies the root or worker process, volume,
and resource allowance. Nothing in this mode uses a Job, a StatefulSet, or `activeDeadlineSeconds`.

### Configuration

This section is the TypeScript daemon's, which now refuses `runtime: kubernetes` outright, naming the
Go daemon (LEGION-286); what follows is the block it read before. The Go coordinator (`packages/daemon`, LEGION-208)
reads the same `runtime.kubernetes` key with different rules, and refuses the examples below as
written: its runtime selects the Legion pool itself, so `scheduling.node_selector` may not set
`legion.dev/pool`; `resources` is keyed by role, with no `role_profiles`; `storage_class` is
required, and a `gateway` block is refused as removed (LEGION-270: a pod's model route is the
operator's `pod` below); every pod dials the worker stream at `tcp://<advertise_host, or bind with
none set>:<worker_stream_port>`, so that address must be one pods reach, never `0.0.0.0` or
loopback — `bind` itself may be `0.0.0.0` only once `advertise_host` names the address instead, so
a daemon whose own pod restarts onto a new IP can still bind every interface and be reached through
its Service; and no Legion URL a pod is handed (`daemon_url`, `envoy_url`, `dispatch_url`, each
`nats_urls` entry) may name a loopback or
unspecified host (`packages/daemon/internal/config/kubernetes.go`). It also reads
`runtime.kubernetes.pod` — `env`, `volumes` (each one `secret`, `config_map`, or `projected`
source), `volume_mounts` (read-only unless `read_only: false`), and `service_account` — which it
adds to every pod, the image probe's included, refusing any name or path of Legion's own or the
worker image's; and the top-level `provider_keys` maps each variable Oh My Pi reads to a key of the
providers Secret, of which every pod then mounts those keys alone, for the shim to export.
Legion holds no model route: everything a pod's Oh My Pi needs to reach a model — a `models.yml`,
a settings overlay in `PI_CONFIG_FILES`, a token — is the operator's, through `pod` and
`provider_keys`. `deploy/kubernetes/operator-route/` is an example of one (the Go live
harnesses': the Hawk model gateway, keyed by a projected ServiceAccount token). [Operator
configuration](#operator-configuration) is what an operator gives it.

`runtime` is either the scalar `tmux` (the default) or a mapping whose single key selects the
Kubernetes runtime and carries its settings:

```yaml
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:<64 hex>   # digest only
    storage_class: gp3           # optional; the cluster default when omitted
    tree_volume: 20Gi            # default 20Gi; one volume per issue tree
    kubeconfig: ./kind.kubeconfig  # optional; relative paths resolve against legion.yaml's directory.
                                   # Omitted: the in-cluster service account
    session_store: pvc           # default; postgres stores each agent's conversation in Postgres (see Session store)
    session_dsn_secret: SESSION_DSN  # required with postgres: the key of legion-<project>-providers holding one postgres:// URL
    resources:                   # optional overrides of the profile table below, per quantity
      large:
        limits:
          memory: 24Gi
    role_profiles:               # optional overrides of the role -> profile map below
      planner: medium
    scheduling:
      node_selector: { legion.dev/pool: legion }
      tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
      priority_class: legion
daemon_url: http://<address pods reach the daemon at>:<the daemon's own port>   # required under kubernetes
bind: 0.0.0.0
envoy_token_file: /var/run/legion/providers/ENVOY_TOKEN       # required under kubernetes
```

A user block with `exec:` (the shape `aws eks update-kubeconfig` writes) is honoured: the command
runs with the daemon's environment plus `exec.env`, its `status.token` is cached until one minute
before `status.expirationTimestamp`, and one 401 triggers one refresh-and-retry. uid `legion` on
the devbox receives the instance role through IMDS, so no credential file exists.

Every Legion pod is annotated `karpenter.sh/do-not-disrupt: "true"`, which stops consolidation and
drift from evicting it; a NodePool's `expireAfter` is forceful in Karpenter v1 and must be `Never`
for Legion's pool, provisioned by your infrastructure-as-code.

The block is file-only: there are no `LEGION_KUBERNETES_*` environment keys, and `LEGION_RUNTIME`
never outranks the file (a disagreement is logged once and ignored). Defaults (root spec §3):

| profile | requests (cpu / memory / ephemeral-storage) | limits (cpu / memory / ephemeral-storage) |
| :--- | :--- | :--- |
| `small` | 500m / 1Gi / 2Gi | 2 / 3Gi / 8Gi |
| `medium` | 1 / 2Gi / 10Gi | 4 / 6Gi / 30Gi |
| `large` | 2 / 4Gi / 20Gi | 6 / 12Gi / 60Gi |

| role | profile |
| :--- | :--- |
| architect, planner, reviewer, merger | `small` |
| implementer | `medium` |
| tester | `large` |

Startup refuses, naming the field, when: `runtime: kubernetes` is given without the block
(`runtime.kubernetes is required when runtime is kubernetes: …`); `namespace` or `image` is missing;
`image` is not pinned by digest; `daemon_url` is missing; `envoy_token_file` is missing
(`envoy_token_file is required when runtime is kubernetes (or set ENVOY_TOKEN_FILE)` — a listener
bound off loopback requires a bearer); `omp_launch_prefix` (or
`LEGION_OMP_LAUNCH_PREFIX`) is set — provider keys come from the mounted Secret, so the prefix has no
process to wrap; `session_store` is anything but `pvc` or `postgres`; `session_store: postgres` comes
without `session_dsn_secret`, or with one that is empty, is not a Secret data key
(`[-._a-zA-Z0-9]+`), or is named `OMP_SESSION_STORAGE` or `OMP_SESSION_SQL_DSN_FILE` (see
[Selecting the store](#selecting-the-store)); `session_dsn_secret` is set under `pvc` (an inert key
is refused, never ignored); or the daemon runs neither with a `kubeconfig` nor inside a pod
(`runtime.kubernetes.kubeconfig is not set and /var/run/secrets/kubernetes.io/serviceaccount/token
does not exist`). Every one of those is a boot refusal before anything is spawned. `session_store`
outside the `runtime.kubernetes` mapping — under `runtime: tmux` — is an unknown key
(`Unknown config key "session_store"`): the setting exists only for pods.

Four prerequisites and caveats the configuration cannot check for you:

- **The `instructions` file must hold no secret.** Under tmux it is read by the pane through `$(cat …)`
  on the daemon host; under Kubernetes the daemon inlines its text into the pod's `--append-system-prompt`
  argument, so it travels in the pod spec's argv — visible to anyone who can `get pods -o yaml` in the
  namespace, and recorded in the API server's audit log. Provider keys and tokens belong in the providers
  Secret only.
- **The daemon host still needs the tmux runtime's toolchain.** Until the daemon itself runs in the
  cluster (LEGION-25), `runtime: kubernetes` changes where the *agents* run, not what the daemon boots
  with: it still resolves OMP and the `pi-legion` and `pi-envoy` plugins through mise for its two boot probes, and
  still needs `jj`, `git`, `gh`, and `tmux` on its PATH (the environment resolver and the plugin
  contract check run before the runtime is chosen). A host missing any of them refuses to start
  exactly as a tmux daemon would.
- **A runtime switch needs a fresh `state_dir`.** A `state.json` written under tmux records tmux
  locators (windows, panes, sockets) for its trees and worker claims; a Kubernetes daemon reading them
  probes each as `not-recorded-process`/gone and retires or resurrects them onto pods, and the reverse
  switch does the same with pod locators — a working copy on the daemon host and one on a PVC are
  never the same files. Start the other runtime on a new `state_dir` (or after every tree has closed),
  and never point the two at one state directory.

### Operator configuration

The Go coordinator's. Legion holds no model, provider or route. Everything a pod's Oh My Pi needs to
reach a model is the operator's, and the daemon hands the same pieces to every pod it runs: each
issue's pod (every role container of it alike) and the image probe's.

- **The project's repositories** are Dispatch's mapping (Settings → Repositories → Projects), one of
  them the project's default; `legion.yaml`'s `projects.<KEY>` names none and refuses `repo`. The
  daemon reads the mapping at boot and on Dispatch's `settings.repo_project.updated` of its
  project, and checks, per repository, that both GitHub Apps are installed on it (`GET
  /repos/{owner}/{repo}/installation` under each App's JWT): a repository either App is missing
  from is left out, not refused, and `legion state --json` names it under `repositories.refused`
  with the App it lacks; a project with no repository mapped does not boot, and one whose every
  repository lacks an App boots serving none, admitting nothing until an App is installed. Every
  role's gh bundle holds a directory for each served repository's owner. An
  issue is recorded on the repository Dispatch resolves for it, with that repository's default
  branch as GitHub names it, when the daemon first records it, and keeps both. A claim on an issue
  the workflow does not record, such as an operator's tree, works the project's default
  repository as it is at each launch, and its launch is refused while the project serves no
  default. **Upgrade order:**
  Dispatch first (it resolves each issue's repository), then map the project's repositories there
  with the old `projects.<KEY>.repo` as the project's default, then delete the key and start this
  daemon; its first boot fills every record written before the record carried its repository from
  Dispatch's resolution, and refuses to boot, naming each record, while Dispatch resolves none, or
  resolves one other than the repository of the issue's pull request.
- **`runtime.kubernetes.pod`** has four keys. `env` is variables set in the agent's container.
  `volumes` are each one `secret`, `config_map`, or `projected` source; a projected
  `service_account_token` must last at least 600 s, the least the API server issues, and its
  `audience` may not still hold a `${…}` placeholder, which nothing expands. `volume_mounts`
  are read-only unless `read_only: false`, and may use `sub_path`. `service_account` is the pods'
  ServiceAccount; unset, pods run as the namespace's `default` ServiceAccount. A name or path that
  collides with Legion's own is refused at load, naming both: a variable the runtime, the worker
  image's `ENV` or every launch sets, a volume name Legion uses, or a mount at, under or above a path
  Legion mounts, the image owns, or a tool runs from, or the agents' state home
  (`/home/legion/.local/state`, under which every role's agent keeps its Oh My Pi state in a
  directory of its own: the role's shim makes Oh My Pi's profile directory there before Oh My Pi
  starts, so a mount there would have every role refuse to start at launch rather than here).
  `legion start --check-config` runs the same check.
  [`deploy/kubernetes/operator-route/`](../deploy/kubernetes/operator-route/README.md) is an
  example of one, the one the Go live harnesses run on: a `models.yml` and a settings overlay from a
  ConfigMap, and a mounted token its key command reads. Its README lists what an operator supplies
  and how `pod` and `provider_keys` compose.
- **Dispatch's public dashboard.** Every pod reaches Dispatch at `dispatch_url`, usually an
  in-cluster address nobody outside the cluster can open. Set `DISPATCH_PUBLIC_URL` in
  `runtime.kubernetes.pod.env` to Dispatch's dashboard as people open it, for example
  `https://dispatch.example.org`: every link an agent's `dispatch` command prints, which agents
  repeat in Slack replies, pull requests and comments, then points there, and a pasted dashboard
  link on either address resolves. Load refuses, naming it, a value that is not an http or https
  URL with no userinfo, a host that is a DNS name of letters, digits and hyphens (no `xn--` label,
  no trailing dot, a last label that is not a number), a dotted-quad IPv4 address or a bracketed
  IPv6 address, and any port from 1 to 65535: a rule narrower than the agents' URL parser, so a
  value load accepts never turns Dispatch off in an agent. The controller's `slack_post` refuses a
  reply linking an address inside the cluster.
  Like every `pod.env` variable, it reaches an agent with the start command of its next generation,
  so set it in the same restart that installs this release, which relaunches the controller once.
  To hand a later change to a running agent, start its next generation with `legion claims suspend`
  and then `legion claims resume`, which resumes its session; `legion claims stop` starts it fresh.
- **The Legion machine-user sign-in.** `legion model-token` is a model `apiKey` command that signs a
  pod in to Cognito with its projected service-account token and prints the access token; the
  [operator route README](../deploy/kubernetes/operator-route/README.md) shows the line and what the
  command does.
- **Each role's model** is the operator's: the `models.yml` and `overlay.yml` they keep in a
  directory of their own (for example `~/.local/state/legion-model-config`), which
  `deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>` writes into the
  ConfigMap `legion-operator-route` in namespace `legion`, with a server-side apply under its own
  field manager that forces conflicts: it takes the two keys over from any tool that wrote them
  before and names that tool's field manager on stderr. A tool that still declares the ConfigMap
  acts at its own next apply: a forced one writes its copy back, and one without force fails on any
  key whose value differs and leaves the operator's copy; either way, stop it declaring the
  ConfigMap first.
  To change a role's model, edit its line under `modelRoles` in that `overlay.yml` and run the same
  command again. Pods started after that use it; a running pod keeps the files it started with until
  it restarts, since both are mounted by `subPath`, which the kubelet never refreshes. The example
  overlay also turns `retry.modelFallback` on (LEGION-578's ruling: a failed model call is retried
  on another model; which model is the repository's `retry.fallbackChains` to name): the probe pod
  reads the effective value under the operator's configuration
  (`omp config get retry.modelFallback`) onto its OK line's `model-fallback` mark (`unread` when it
  could not read it: the probe pod's log says
  why), and the daemon reports the `model-fallback` capability open while a deployment's copy
  turns it off or the probe could not read it — a report, not a refusal; a
  `capabilities.decided.model-fallback: "<reason>"` line in `legion.yaml` records a decision to
  keep it off ([The deployment's capability report](#the-deployments-capability-report)).
- **`runtime.kubernetes.agent_secrets`** enrolls every pod the daemon runs with the secrets broker
  (the broker design's pod-enrollment plan), so an agent in a pod runs
  `agent-secrets <SECRET> -- <command>` and gets only
  that pod generation's grants. `url` is the broker's base URL (https, or http to a loopback
  address), and `token_file` (required, absolute) is where the daemon's own pod mounts its projected
  ServiceAccount token for the broker's audience, `agent-secrets`, with an `expirationSeconds` of
  at most 3600: the broker refuses a token that lives longer than an hour from its issue
  (`TOKEN_TOO_LONG_LIVED`). The daemon logs in with that token at boot, on a background context,
  and again halfway through each credential's life and whenever the broker refuses it, reading the
  file each time so it presents the kubelet's current token; no person approves any of it, a
  restart included, so the block names no approver and a file that sets `operator` is refused at
  load naming the key. The broker logs the daemon in only while its `BROKER_SERVICE_LOGINS` names
  the daemon's ServiceAccount for `legion-daemon` (the broker's operate page, "What to run"), and
  keeps each account's one launcher credential across logins. The daemon keeps one key for its
  process's life, in memory only, and proves it again at each renewal, so a renewal changes only the
  credential's expiry and an enrollment in flight never waits for one; a restart brings a new key,
  which the next login moves the credential to, so the pods the daemon enrolled before the restart
  stay its own. Each login is logged (`agent-secrets: logged in with the daemon's ServiceAccount
  token`, or `agent-secrets: the token login failed; retrying` with the broker's answer), and
  `GET /legion/v1/state`'s `agentSecretsLogin` reads its `state`: `none` before the first login
  finishes, `issued` while a credential that has not expired is held (a renewal that failed beside
  it included), or `failed` (daemon API contract 22). Pod enrollment fails closed and retries until
  a login succeeds. After a failed login the daemon waits 30 s before the next, doubling each time
  to at most 5 minutes, whether or not it still holds a credential, and a successful login resets
  the wait. Running the daemon off the cluster (the Stage 4a live proof), `token_file` holds a
  token the operator mints, at most an hour long, for a ServiceAccount of the proof's own that
  `BROKER_SERVICE_LOGINS` lists for `legion-daemon` and no running daemon uses, with
  `kubectl create token <account> -n <namespace> --audience agent-secrets --duration 1h`: each
  account holds one credential, so a proof logging in as a running daemon's account would take
  that daemon's credential.
  A held enrollment can come long after the shim's hello, which carries the pod's projected token
  as the shim read it then, and that token lives `token_expiry_seconds` from when the kubelet wrote
  it; the broker refuses an expired one as `POD_TOKEN_EXPIRED`, naming its expiry. So when the
  token the daemon holds would expire within the enrollment's RPC bound plus a minute, the daemon
  first asks the shim to read the file again (the `agent-secrets-token` frame), which the kubelet
  rewrites before the token expires. A token the broker refuses as expired anyway, its clock
  further ahead than that minute, is dropped with the pod's key kept (the supervisor logs
  `the broker found the pod token expired; keeping the key for a fresh token`), and the next
  observation reads a fresh one the same way. A shim from a worker image older than that frame
  forwards it to Oh My Pi,
  which refuses it; the daemon then logs
  `supervise: agent-secrets: the shim did not read its projected token again; waiting for its next hello`
  and enrolls on the shim's next hello: one its dropped connection redials with, or the claim's
  next generation, which an operator starts with `legion claims suspend` and then
  `legion claims resume` once the login reads `issued`.
  `provider_keys` may not name an `AGENT_SECRETS_*` variable; `audience` (default
  `agent-secrets`) and `token_expiry_seconds` (default 3600, at most 3600, the cluster's admission cap)
  shape the one projected token every pod carries for the broker, alone in its volume beside the
  operator's middleman token. With the block, the worker container mounts that token read-only at
  `/var/run/legion/agent-secrets-token/token`, a memory-backed key directory at
  `/var/run/legion/agent-secrets`, and is told `AGENT_SECRETS_URL` and `AGENT_SECRETS_KEY_DIR`; the
  shim generates the pod's key there before it dials, its `hello2` carries the key's thumbprint and
  the token, and the daemon enrolls the pod under its own won credential once the agent has
  registered — naming the pod UID it recorded at spawn (the enrollment carries no issue or
  approver at all; each secret's owner and tier tags pick the approver for the pod's later
  credential requests at request time) — hands the enrollment id back to the shim, and revokes it
  wherever it lets the pod go (a death, the registration deadline, a suspension, a stop, the
  tree's close). The shim's renewer, `agent-secrets renew --pod-token-file <the projected token>`,
  sends the pod's current token with every renewal, and the broker refuses a renewal whose token
  names another pod UID or service account than the enrollment's (`POD_IDENTITY_MISMATCH`, which the
  renewer retries until the lease ends, since the broker also answers it while its token issuer is
  unreachable). The token is not yet required: once any enrollment naming a pod UID has renewed
  with that pod's token, the broker refuses a renewal without one (`POD_TOKEN_REQUIRED`, on which
  the renewer stops) from every enrollment naming that pod, whichever machine login made it, so
  the copied-token gap closes for every pod UID a current-image pod has renewed under. A pod on an
  older worker image never sends one and renews as before, each renewal logged at WARN, so the gap
  stays open for old-image UIDs until the release that requires the token (the broker's operate
  page, "The renew token check"). A renewer that cannot read its token file sends nothing and prints
  `ERROR: enrollment <id> cannot read its pod token`, which the shim relays into the pod's log. Every
  deploy order keeps every pod renewing: an older broker ignores the token, and this one accepts a
  renewal without it from a pod no enrollment of which has sent one. That is liveness, not
  exposure: a grant lasts until its session ends, so replace every old-image issue pod before or
  right after the broker deploys (the operate page names the steps). The broker ends a pod
  enrollment only for the launcher credential that made it. The daemon's token login keeps its
  account's one credential across restarts, so it ends every pod it enrolled; an enrollment another
  credential made (a service login a person approved through a code before token logins) is
  refused `NOT_ENROLLING_CREDENTIAL`, and the daemon logs
  `supervise: agent-secrets: enrollment revocation refused; its lease ends it once its pod stops renewing`:
  it ends one lease after its pod's renewer stops, or at once when someone revokes that login on
  Dispatch's machine-login page by its **Credential** id (the broker's operate page, "The renew
  token check", gives the query that finds it), never by its machine name. The daemon logs in as the
  service `legion-daemon`, and its pods run as the ServiceAccount
  `runtime.kubernetes.pod.service_account` names (`default` when unset, above). A broker configured
  with that account for `legion-daemon`, such as
  `BROKER_SERVICES=legion-daemon=system:serviceaccount:legion:legion-worker` for pods running as
  `legion-worker`, gives a secret tagged `owner=legion-daemon` to every such pod at once and to no
  other session (the broker's concepts page, "Owner and tier"). The broker refuses to start with a
  namespace's `default` account in `BROKER_SERVICES`, since every pod there that names no account
  runs as it, so pods that receive service-owned secrets name a dedicated account. The daemon's own
  ServiceAccount, which logs in, is a different account from its pods', and it is named in
  `BROKER_SERVICE_LOGINS`, not `BROKER_SERVICES`; the broker refuses to start with an account in
  `BROKER_SERVICE_LOGINS` that is a namespace's `default` or one `BROKER_SERVICES` binds, since
  every pod running as it could then log in as the daemon.
  Without the block, pods carry none of this. An older worker image is refused at
  the image probe: the block's pod variables are daemon API contract 8. A pod's agent-secrets
  volumes are fixed when it is created, so turning the block on, or changing its `audience` or
  `token_expiry_seconds`, reports every running workflow role stale at the restart that brings the
  change, and each issue pod is replaced with its roles moving into the new one together
  ([A pod whose address moved](#a-pod-whose-address-moved)). Turning the block off revokes no
  enrollment. It moves the environment alone (`AGENT_SECRETS_URL`, now unset), so each role's next
  generation starts in the pod it ran in, which still mounts the broker's token, which the kubelet
  keeps refreshing, and the role's key directory, holding its last key and enrollment id. The pod
  keeps both until it is replaced, at the latest when its issue closes. Its launcher starts the
  shim without the broker's flags, so nothing Legion runs renews that enrollment, and the daemon,
  with no broker configured, cannot revoke it (it logs
  `supervise: agent-secrets: enrollment recorded with no broker configured; its lease ends it`).
  Anything in that role's container that knows the broker's URL can renew it, though: the role's
  key directory and the broker's projected token are mounted into that container and no other, the
  broker accepts a renewal on the key's proof and that pod's current token, and
  `agent-secrets renew`, which the image ships, needs only that URL, the key directory and the
  token file. So the enrollment can stay live until the pod is replaced, at the latest when its
  issue closes, plus one lease (`BROKER_LEASE_SECONDS`). To end it at once, anyone signed in to
  Dispatch revokes the daemon's login on Dispatch's machine-login page (`/credentials/machine`),
  the row `legion-daemon on <namespace>/<account>` whose **Approved by** reads `No one:` and the
  daemon's ServiceAccount, which ends every session it enrolled; the daemon logs in again with its
  token under a new credential, which enrolls the pods it launches next, and a pod already running
  stays without secrets until it is replaced
  (`docs/site/src/content/docs/broker/guides/revoke-a-session.md`, "End a machine's login").
- **`provider_keys`** (top-level) maps each variable Oh My Pi reads to a key of the providers
  Secret, `legion-<project>-providers`, which the operator creates. Every pod mounts the keys
  `provider_keys` names and no other key of the Secret, each at a file named for its variable, and
  the shim exports each into Oh My Pi's environment, never its own. The one other key a pod mounts
  is `NATS_NKEY_SEED`, when the daemon has a NATS nkey seed: the operator puts the `legion-pane`
  seed there, and every pod reads it through `NATS_NKEY_SEED_FILE`, which the shim does not export
  ([Configuration](#configuration)). (The TypeScript daemon's
  [providers Secret](#the-providers-secret) mounts every key; the Go runtime does not.) A Secret or key the kubelet cannot mount is refused at boot by the image probe,
  naming the Secret and keys. A `provider_keys` variable that anything else in the pod sets is
  refused at load.
- **Settings order.** Oh My Pi reads `PI_CONFIG_FILES` in order, each overlay outranking the ones
  before it and all of them outranking a repository's `.omp/config.yml`. Legion writes one overlay,
  the turn-scoping one (`bash.autoBackground.enabled` and `async.enabled` off, the two keys a
  takeover's abort depends on, `packages/daemon/internal/podsafety/turnscope.yml`), and names it
  first, ahead of the operator's, so the operator's overlay outranks it. Nothing of a repository's
  settings is held off: they reach a pod's agent as they reach any agent session, under the
  operator's overlay. The pod sets `PI_CONFIG_DIR=.omp` and `OMP_SESSION_STORAGE=file` on the agent
  where the pod leaves them unset, which an operator's pod may not set, since they decide where Oh
  My Pi keeps the session a resume reads.
- **Model roles.** Legion's shipped agents dispatch by role alias: `oracle` and the planner's
  `plan-gap-analyst` as `@oracle`; both review agents and the planner's `plan-reviewer` as
  `@review`; and `deep-worker`, which writes the implementer's code, as `@deep`. The boot gate
  refuses, by agent, any whose role the operator's settings (`modelRoles`, or
  `task.agentModelOverrides`) leave unconfigured, or whose model's key does not work, because Oh My
  Pi's task tool would quietly run it on the parent session's model.
  The bundled agents Legion's prompts also dispatch use Oh My Pi's built-in roles: `scout` is
  `@smol` and `reviewer` is `@slow`. `smol` and `slow`, left unset in every layer, inherit the
  default role's model, which the gate accepts. Settings records merge
  key by key across layers, though, so a role the operator's overlay does not name can be named by a
  repository's `.omp/config.yml`. Name each role those agents use (`review`, `oracle`, `deep`,
  `smol`, `slow`) to keep the choice the operator's.
- **The repository `.env`.** Oh My Pi's runtime loads the working directory's `.env` into its
  environment at start, filling every variable the pod left unset or empty. A repository can therefore set
  anything Oh My Pi reads from its environment: a provider's API key, `PI_SMOL_MODEL`,
  `PI_SLOW_MODEL` and `PI_PLAN_MODEL` (which override those roles), and `CLAUDE_CODE_USE_FOUNDRY`
  with `FOUNDRY_BASE_URL` (Anthropic Foundry, which takes a model's endpoint). Set every such
  variable your route depends on in `pod.env` to a non-empty value, which outranks `.env`. The fixture
  sets `CLAUDE_CODE_USE_FOUNDRY: "0"` for this reason.
- **Providers the route does not use.** Oh My Pi falls back from a failing provider to any other
  enabled provider it holds a key for, without a word. Legion names no provider, so the operator's
  overlay disables every provider its route does not use (`disabledProviders`), and can hold every
  session to the route's models (`enabledModels`). The fixture's overlay disables Bedrock twice (a
  node's instance role is its key), Google, and the local servers that need no key. A provider a key
  could turn on, such as `cursor` through a repository's `CURSOR_ACCESS_TOKEN`, belongs on the list
  when the route does not use it.
- **A key that fails at boot refuses the boot.** The image probe runs every provider key command the
  agents' models need, at every boot. A command that fails is "no working credentials", whether the
  cause is a revoked token or a network blip: the gate cannot tell the two apart, and passing on a
  failed key is the fallback the gate exists to stop. Nothing restarts the Go daemon on its own: it
  runs off the cluster, and whoever started it starts it again.

### Cutting over an instance from tmux to pods

Drain every tree while it is still on tmux, stop the daemon, choose a new `state_dir`, and start
with `runtime: kubernetes`. The state file intentionally holds the controller's tmux locator beside
root and worker pod locators; the daemon routes them by process kind.

The block below is the TypeScript daemon's. The Go daemon's cutover is the same drain and fresh
`state_dir`; its [Configuration](#configuration) says where its rules differ, and its
[live proof](#the-live-proof) is what to run at the head you deploy.

```yaml
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:<digest>
    kubeconfig: /home/legion/.kube/config
    storage_class: gp2
    tree_volume: 20Gi
    scheduling:
      node_selector: { legion.dev/pool: legion }
      tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
      priority_class: legion
nats_urls: [nats://nats.internal.example.com:4222]
envoy_url: http://envoy-listener.internal.example.com:9020
envoy_token_file: /home/legion/.config/legion/sjawhar-legion/envoy-api-token
dispatch_url: https://dispatch.internal.example.com
daemon_url: http://<devbox VPC IP>:<daemon port>
bind: <devbox VPC IP>
worker_stream_port: <worker stream port>
```

**`--resume` keeps the same-agent invariant through the init container.** The tmux runtime `stat`s the
recorded session file on the daemon host and refuses to spawn when it is missing (a silent fresh agent
would lose the session). Under Kubernetes the file lives on the pod's volume (`/legion/sessions/…`, the
OMP sessions `subPath`), which the daemon cannot see — and OMP itself does not refuse: verified against
the pinned OMP build (`omp --resume=<missing path> --mode rpc`), it exits 0, answers `ready`, and runs
as a fresh agent, with no error frame and no mention of the file. So when a spawn resumes a session, the
runtime hands the init container the file's volume path (`LEGION_RESUME_SESSION_FILE`, the recorded
`/home/legion/.omp/profiles/legion/agent/sessions/<file>` translated to `/legion/sessions/<file>`; a
recorded path anywhere else is refused before any API call), and `workspace-init` exits non-zero naming
the path if it is not there after provisioning — the pod goes `Failed`, the daemon counts a launch
failure, exactly as tmux does. A volume replaced or a `sessions/` directory removed by hand therefore
fails the respawn loudly instead of quietly starting a new agent under the old role token. Under
`session_store: postgres` the transcript is a database row, not a file on the volume, so the init
container omits that check; `--resume=<row path>` still reaches OMP, which opens the row — and when the
row is gone, starts a fresh session with a new id that the daemon then refuses to register (`409 Worker
respawn must resume the same agent session`), so the pod exits and the launch failure is counted (see
[Selecting the store](#selecting-the-store)).

**On kind, let the node pull the image from GHCR by digest** (the package is public; a fresh node
pulled the 377 MB image in about 10 s). Do not `kind load docker-image` a digest-only reference: kind
imports it as an anonymous `docker.io/library/import-<date>` image, containerd then answers every pod's
pull for that digest with `image "docker.io/library/import-…@sha256:…": not found`, and neither
`crictl rmi` nor `ctr images rm` clears it — the cluster has to be recreated.

### Anatomy of a pod

One bare Pod per process generation, named `legion-<issue-slug>-<role>-g<generation>` (issue keys are
lowercased; an over-long key is truncated with an 8-hex hash so the name fits 63 characters),
labelled `legion.dev/project`, `legion.dev/tree`, `legion.dev/issue`, `legion.dev/role`, and
`legion.dev/generation`, with `restartPolicy: Never`: a pod that exits is dead, and the daemon — not
the kubelet — decides whether the next generation starts (`--resume` of the same agent).

Every pod of a tree is *required* to schedule on the node that already runs another pod of that tree
(`podAffinity` on `legion.dev/tree` at `kubernetes.io/hostname`), because the tree's volume is one
`ReadWriteOnce` PVC and the shared working copies on it must be reachable from every pod that mounts
it. The tree's first pod satisfies its own affinity term.

Containers, in order:

1. **Init container `workspace-init`** runs `legion workspace-init --issue <KEY> --repo <owner>/<repo>
   --root /legion --credential-helper '!/opt/legion/bin/legion credential'`: the same
   `provisionIssueWorkspace` a tmux daemon runs in-process, rooted at the volume — a shared clone under
   `/legion/repos/github.com/<owner>/<repo>` plus one jj workspace per issue under
   `/legion/workspaces/<owner>/<repo>/<key-lower>`, cloning or fetching with the repository token
   (`LEGION_PROVISION_TOKEN_FILE`, the GitHub App installation token the daemon clones with on a
   single machine, mounted from the per-pod Secret into this container **only**). It adopts nothing
   for any identity: the working copy is adopted for the assigned role at each assignment, through
   the main container's shim (below). The shared clone is written under an exclusive `flock(2)` on
   `/legion/repos/github.com/<owner>/<repo>.lock`, held for the init container's lifetime, so two pods of
   one tree provisioning together never write it at once; a contended pod logs that it is waiting and
   waits up to `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS`, which the daemon sets from its boot deadline (see
   *Liveness rules*). It then writes `credential.helper` so `git` inside the
   pod redeems the agent's per-command grant through `legion credential` at `daemon_url`, installs
   the `gh` shim at `/legion/worker-bin/gh`, and creates `/legion/sessions` and `/legion/gh`.
2. **Main container `worker`** runs `legion worker-shim --connect tcp://<host of daemon_url>:<worker_stream_port>
   --boot-token-file /var/run/legion/boot/LEGION_BOOT_TOKEN --provider-env-dir /var/run/legion/providers
   -- omp [--resume=<session file>] --mode rpc --append-system-prompt <role prompt, addressing text,
   and deployment instructions joined by blank lines>`. The prompt texts are passed inline (the daemon
   has them; the image may be built from another commit) and, exactly as on tmux, in **one**
   `--append-system-prompt` argument — OMP's flag is last-wins, so several flags would hand the
   model only the last text. A joined value over 128 KiB (Linux `MAX_ARG_STRLEN`) is refused at spawn.
   The shim dials the daemon, sends its boot token in one `hello` line, and only after the daemon's
   `hello_ack` spawns OMP. Every assignment to this pod -- the first, and a live idle worker
   re-prompted after another role's pod has touched the shared working copy -- first adopts `@` for
   the assigned role:
   the daemon sends the shim an `adopt-working-copy` frame (`jjUser`, `jjEmail`, `timeoutMs` — never a
   command or a path), the shim runs the same `jj metaedit --update-author` in its own workspace
   (`LEGION_WORKSPACE`/`LEGION_ROOT_WORKSPACE`) and answers `adopt-working-copy-result`; `ok: false`
   fails the delivery exactly as a failed `metaedit` does on tmux.

Volumes and mounts:

| volume | source | mounted at | in |
| :--- | :--- | :--- | :--- |
| `tree` | the tree PVC `legion-<tree-slug>` | `/legion` | both containers |
| `tree` (subPath `sessions`) | same PVC | `/home/legion/.omp/profiles/legion/agent/sessions` | main — OMP's session directory, so a replacement pod finds the file it resumes |
| `providers` | Secret `legion-<project>-providers` (read-only) | `/var/run/legion/providers` | main |
| `boot` | the per-pod Secret, one key per secret the daemon delivers: `LEGION_BOOT_TOKEN`, and `ENVOY_TOKEN` when the daemon has an Envoy bearer (read-only) | `/var/run/legion/boot` | main |
| `provision` | the per-pod Secret, key `LEGION_PROVISION_TOKEN` (read-only) | `/var/run/legion/provision` | init only |
| `grant` | memory-backed `emptyDir` (1Mi) | `/var/run/legion/grant` | main — where the extension writes the grant it mints before each bash command and each tool call Oh My Pi serves with `gh` |

The main container's environment is the same set of variables a tmux pane carries, with every value
that was a daemon-machine path re-pointed at its pod location:

| variable | pod value | read by |
| :--- | :--- | :--- |
| `PATH` | `/legion/worker-bin:` + the image's PATH | the pane's `gh` shim first, as on tmux |
| `GH_CONFIG_DIR` | `/legion/gh` | `gh` |
| `LEGION_GRANT_FILE` | `/var/run/legion/grant/<role token>-grant` | the pi-legion extension (writes), `legion credential`/`gh`/`handoff complete` (read) |
| `LEGION_STATE_DIR` | `/legion` | the pi-legion extension's jj attribution overlay |
| `LEGION_CREDENTIAL_HELPER` | `!/opt/legion/bin/legion credential` | what `workspace-init` wrote into the clone's git config |
| `DISPATCH_TOKEN_FILE` | `/var/run/legion/providers/DISPATCH_TOKEN` | `dispatchSettings` (when the deployment sets `dispatch_url`) |
| `LEGION_ROOT_WORKSPACE` / `LEGION_WORKSPACE` | `/legion/workspaces/<owner>/<repo>/<key-lower>` | the extension; also the container's `workingDir` |
| `LEGION_BOOT_TOKEN_FILE` | `/var/run/legion/boot/LEGION_BOOT_TOKEN` | the extension's boot handshake |
| `LEGION_TERMINATION_GRACE_SECONDS` | the pod's `terminationGracePeriodSeconds` (`worker_stop_timeout_seconds`) | the shim, PID 1: on SIGTERM it gives OMP half of it to exit on its closed stdin before the fallback SIGTERM |
| `ENVOY_TOKEN_FILE` (when the daemon has an Envoy bearer) | `/var/run/legion/providers/ENVOY_TOKEN` — the providers Secret's key, one copy like `DISPATCH_TOKEN_FILE`; never a per-pod Secret key | `@legion/envoy-client`'s transport: the bearer on every listener call |

Everything else (`LEGION_TREE`/`ISSUE`/`ROLE`/`GENERATION`/`PROJECT`, `LEGION_DAEMON_URL`, `ENVOY_URL`,
`ENVOY_NATS_URL`, `DISPATCH_URL`, the `GIT_*` settings, the emptied `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST`)
passes through unchanged. Requests and limits come from the role's profile.

Two things keep secrets out of `kubectl get pod -o yaml`: every token is a **file** (the Secret mounts
above; no `env`, `command`, or `args` ever carries a value), and provider keys enter the OMP child's
environment **only**, through the shim's `--provider-env-dir` — the shim reads each regular file in the
directory as `NAME=contents` into the environment of the `omp` process it spawns — skipping any `NAME`
the pod already consumes through a `NAME_FILE` pointer in the shim's own environment (`DISPATCH_TOKEN`,
which `DISPATCH_TOKEN_FILE` points at), so a file-pointed token is never also an environment variable
of every tool the agent runs; refusing to start at all, naming the key and its file, when a `NAME` is
already a variable of the shim's own environment (see the providers Secret, below) — and never into
its own (`/proc/1/environ` inside the pod carries no key; the OMP child's does, by design).

### The providers Secret

`legion-<project>-providers` (where `<project>` is `legion.yaml`'s `project` lowercased with
non-alphanumerics removed) is created per deployment, never by the daemon, with one key per variable
OMP or the extension reads: `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`, `DISPATCH_TOKEN`,
and `ENVOY_TOKEN` when the Envoy listener requires a bearer. It is mounted read-only into every main
container, and every pod's `ENVOY_TOKEN_FILE` points at its `ENVOY_TOKEN` key — so a daemon that runs
outside the cluster over a kubeconfig and has an `envoy_token_file` must put that same token in this
Secret, or its pods' listener calls are refused. The daemon cannot `get` or `list` Secrets through the API (its Role grants neither). The orphan sweep never touches it: the
sweep deletes only the per-pod Secret named after an orphan pod. Nothing else belongs in it — every
worker pod mounts the volume and the shim exports each file that has no `<NAME>_FILE` pointer in the
pod into the OMP child's environment, so a GitHub App private key here would reach every worker. And a key must not be named like
a variable the pod already carries — `OMP_SESSION_STORAGE`, `OMP_SESSION_SQL_DSN_FILE`, any
`LEGION_*`, `DISPATCH_URL`, the deliberately empty `GH_TOKEN`: that export lands over the pod's own
environment, so such a key would replace the daemon's value without anything saying so (a stale
`OMP_SESSION_STORAGE=file` would quietly move a Postgres deployment back to files). The shim refuses
to start instead, before Oh My Pi is spawned, with a message naming the key and its file
(`legion worker-shim: --provider-env-dir key OMP_SESSION_STORAGE
(/var/run/legion/providers/OMP_SESSION_STORAGE) is already a variable of this pod's environment; …`);
the pod is `Failed` and the daemon counts the launch failure as it does any other. A key the pod reads
through a `<NAME>_FILE` pointer (`DISPATCH_TOKEN`, `ENVOY_TOKEN`) is skipped, not refused.

### Contract discipline

For a daemon contract change, first merge the worker image and the two plugin releases, then set
`runtime.kubernetes.image` to its digest, install both plugin releases (`@sjawhar/pi-envoy`,
`@sjawhar/pi-legion`) in the Legion profile, and restart the daemon. The Go daemon's own contract
discipline — which processes a boot pass relaunches, how, and what it logs — is "Contract
discipline" under "Kubernetes runtime: the Go daemon on Agent Sandbox" above; this runtime does
none of it.

### RBAC the daemon needs

For the daemon's Role (LEGION-25), the verbs this runtime uses on core/v1 in its namespace:

| resource | verbs |
| :--- | :--- |
| `pods` | `get`, `list`, `create`, `delete` |
| `pods/log` | `get` |
| `persistentvolumeclaims` | `get`, `list`, `create`, `delete`, `patch` |
| `secrets` | `create`, `delete` |
| `events` | `list` |

No `get` or `list` on Secrets: either returns Secret data, and a list would hand the daemon every Secret in the namespace, `legion-<project>-providers` and its provider keys included, which the design says the daemon never holds. The orphan sweep names each per-pod Secret from the pod it belongs to (they share the name) and deletes by name.

A 403 fails the spawn (or stop) naming the verb and resource, e.g. `create secrets/legion-…`.

## The controller

The controller is the one Legion session a person talks to. `controller` in `legion.yaml` says who
launches it: `operator` (the default), a person running `legion controller start` on a machine of
theirs ([Operator-launched controller](#operator-launched-controller)), or `daemon`, the daemon
itself, as an Agent Sandbox pod it supervises ([Daemon-launched controller](#daemon-launched-controller)).
`controller: daemon` needs `runtime: kubernetes`; the loader refuses it under tmux (`controller:
daemon needs runtime: kubernetes, …`) and refuses any value but the two.

### Controller liveness

Under either `controller` mode the daemon checks the project's controller every minute: it reads
the controller's record and, while a session holds it, that session's liveness from the Envoy role
registry. While no session holds the current capability, or the registry says the session is gone,
it logs `controller not registered; run legion controller start` at most once per
`worker_boot_timeout_seconds`. Under `controller: daemon` the line goes on with that mode's remedy,
`… only under controller: operator; this daemon launches its own controller and relaunches it`,
and names the state of the controller's claim as the daemon's store holds it (`claimState`):
`launching` while a launch is in flight, `failed` or `retired` while the daemon waits to retry it,
so a launch or a backoff reads apart from a death. A boot under `controller: daemon` with no live
controller logs the line from its first check until its first launch registers, and again each
`worker_boot_timeout_seconds` while scheduling and the image pull take longer than that. The check
runs beside the daemon's keeping of its controller and never waits on a launch, however long one
takes. While the registry holds the controller role for nobody, the daemon also logs `controller
liveness: the controller role has no live holder; the controller is gone` at every check. Both
modes log both lines on the same schedule, and every line names the mode (`mode`; the
not-registered line also says whether a session holds the record, `registered`), so one log query
on either line's text counts either mode. Stage 4b's `daemon-controller-liveness` checkpoint shows
both lines for a daemon-launched controller whose pod is gone and whose relaunches cannot schedule,
and neither while it runs ([`scripts/e2e/README.md`](../scripts/e2e/README.md#stage4b-sandbox-treesh)).

### Daemon-launched controller

Under `controller: daemon` the daemon launches the project's controller at boot and keeps it
running, as it does a root architect. Nobody runs `legion controller start`, and
`POST /legion/v1/controller/secret` and `POST /legion/v1/controller/github-credential` both answer
409 (`this daemon launches the project's controller itself (controller: daemon), so legion
controller start has none to start`): one controller runs per project.

**The claim.** The controller is one claim, `legion-<project>-controller`, on the role
`controller` with no issue and no tree. It is supervised like any claim: a pod that dies is
relaunched and resumes the same Oh My Pi session, within `launch_failure_limit`; a daemon that
restarts re-adopts the running pod and launches no second one. A claim whose budget ran out fails,
as any claim does, and the daemon retries it with fresh budgets after a minute, doubling the wait
at each retry that fails again up to thirty minutes, and resetting it once the controller is ready.
`legion claims list` shows the claim; the workflow never sees it.

**The credential.** The daemon mints the controller's credential at every launch: the launch's
boot token, which reaches the pod's launcher in the daemon's authenticated start command and which
the launcher writes owner-only into that generation's private directory, as for every role. The
session's pi-legion extension sees `LEGION_BOOT_TOKEN_FILE` beside `LEGION_CONTROLLER=1` and registers on
`POST /legion/v1/claims/register` with that token. The daemon records the registration on the claim
and records the session as the project's controller, under a fresh capability nobody holds (never
the boot token), which ends every earlier controller grant; the answer is the operator's
controller's registration (`{claimToken, role: "controller", generation, secret}`, its generation
the launch's). The session then takes the controller role, subscribes to the controller topic, and
calls `POST /legion/v1/claims/ready`; the daemon then delivers the start message `legion controller
start` passes, so every launch runs the skill's start procedure. A claim step that fails exits Oh
My Pi, and the daemon relaunches it. No operator token reaches the pod. Under `controller: daemon`
the claim route registers the controller only from a launch of this claim: a token no launch
resolves is refused as an invalid boot token, the operator's capability and the token of a launch
the claim has since replaced among them, so a replaced pod never registers outside its claim's
generation fence. `/legion-claim-controller` registers the boot token only in the controller's own
session (`LEGION_CONTROLLER=1`); in a root architect's or phase worker's session it needs the
operator's capability like any other takeover, and stops before any daemon call without one.

**The pod.** Sandbox `legion-<project>-controller` in `runtime.kubernetes.namespace` runs the pod an
issue's Sandbox runs ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)) with a one-role list,
on the Legion pool under gVisor, with the operator's pod (`runtime.kubernetes.pod`) and the
providers Secret, so it reaches models by the route every worker does. Its one container,
`controller`, runs `legion launcher --role controller`: it authenticates to the worker stream with
its own launcher token and starts and stops the controller's `legion worker-shim` and Oh My Pi, on
the pod baseline (`--pod-safety`: the turn-scoping overlay first in `PI_CONFIG_FILES`, the two
session-placing variables, [Settings order](#operator-configuration), and Oh My Pi's profile
directory made under its state home, `/home/legion/.local/state/controller`), on the daemon's
command, as an issue pod's role containers do theirs, so a relaunch in a healthy pod is a new
generation of that child rather than a new pod. Its role Secret,
`legion-<project>-controller-controller-boot`, holds the launcher's token and its gh bundle (each
owner's `github-hosts-<owner>` and `github-config-<owner>`, the review App's read-only `hosts.yml`
and `config.yml` for every owner of the project, and `github-default`, the first owner), refreshed
every minute as every role's are, and is the only Secret its launch writes: there is no
provisioning `-boot` Secret.
Its Sandbox owns a volume of its own, as an issue's does (`issue-legion-<project>-controller`, from
its one `issue` claim template, of `runtime.kubernetes.issue_volume` and `storage_class`), mounted
at `/legion` with its `sessions` directory at Oh My Pi's sessions directory, so a relaunch resumes
the session. It provisions no workspace: no `workspace-fetch`, no provisioning token. Its `gh`
bundle, projected read-only at `/var/run/legion/gh` by a `gh-controller` volume, acts as the review
App, refreshed every minute as every role's is; a failed review-App mint fails the launch naming
the role, and the keeper relaunches it. A project with no GitHub owner to mint for (none of its
repositories has an App) starts the controller anyway with `gh` acting as nobody, as `legion
controller start` does there: its role Secret records an empty `github-default`, its `gh-controller`
volume is an empty directory, and it holds owners from its next pod on. Its one init container,
`legion workspace-init controller --root
/legion`, makes the sessions directory; in a pod created to resume a session whose file is gone it
exits 3, which the daemon reads as a lost volume and answers with a fresh controller. The pod
carries `legion.dev/project` and `legion.dev/role=controller` and no tree or issue label, so no
tree's cleanup lists it, and the boot census holds it to exactly the one launcher those labels
name and to the volume it owns. Under `runtime.kubernetes.agent_secrets` it is enrolled with the
secrets broker as an issue pod's roles are: it projects the broker's token, has a key directory of
its own, and its shim takes the three agent-secrets flags and its agent `AGENT_SECRETS_URL` and
`AGENT_SECRETS_KEY_DIR`. It runs as the operator's ServiceAccount (`runtime.kubernetes.pod`), the
same account as every issue pod. The broker binds one account to each service (`BROKER_SERVICES`),
so this shared account is what makes the controller the `legion-daemon` service's own pod: a
controller given an account of its own would no longer be granted that service's secrets. The
broker grants it, with no person asked, an agent secret whose owner is `legion-daemon` and whose
tier is `agent`, which is how the operator stores `LEGION_SLACK_BOT_TOKEN`, the Slack app's bot
token its `slack_post` replies with. Legion sets no affinity on it, so under
Karpenter it lands wherever the pool has room for it (on GKE Autopilot it usually gets a node of its own,
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)), and it is annotated `karpenter.sh/do-not-disrupt:
"true"` and `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`: Karpenter never consolidates
or replaces for drift the node it runs on, though issue pods can still be scheduled onto that node;
GKE Autopilot holds off a scale-down or node-upgrade eviction of the pod for up to seven days
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod) has what it does not stop), after which
Autopilot may evict it like any other pod, and it is relaunched and resumes the same session like
any claim's pod that dies ("The claim", above). Its agent is told `LEGION_CONTROLLER=1`,
`LEGION_ROLE=controller`, `LEGION_PROJECT`, `LEGION_DAEMON_URL`, the Envoy, NATS and Dispatch
settings, `LEGION_SLACK_CHANNELS`, the project's Slack channels comma-separated and empty for none
(the runtime's, always set, and refused in `runtime.kubernetes.pod.env` and `provider_keys`, so no
list but the project's reaches it; its plugin subscribes to each channel's mention topic while the
session holds the controller role, registered with no listener, and a resume first drops every
Slack topic an earlier session registered (a restored one would outlive the role), so a resume or
relaunch hears the channels the launch names and no others; the list is one of the addresses the
runtime records per generation, so a daemon restarted with another `slack_channels` re-adopts the
controller as an address that moved and relaunches it, resuming its session;
`legion controller start` drops an inherited value, and any controller but this one ignores it),
the launch secrets' `<NAME>_FILE` pointers, and the six gh variables exactly as an issue pod's role
container is told them (`LEGION_GH_OWNERS_DIR=/var/run/legion/gh`, `GH_CONFIG_DIR` its first owner's
directory there, `LEGION_GH_PATH=/usr/local/bin/gh`, `GH_TOKEN=""`, `GITHUB_TOKEN=""`, `GH_HOST=""`),
and nothing of
a tree, an issue or a checkout. Its system prompt is the controller's role
prompt, a part saying it runs headless in a pod, the daemon's `Design gate policy:` line, the
project's repositories sentence, the project's `Slack channels:` line after it when `slack_channels` names any, and the deployment
instructions. Both its containers, the init container and the launcher, carry the
controller's reservation — `runtime.kubernetes.resources.controller` where the file sets a field,
the daemon's default of 1 CPU and 4Gi otherwise — as request and limit alike unless the file sets a
lower `cpu_request` or `memory_request`, so at the default the pod is `Guaranteed`, never the class
the kubelet evicts first under node memory pressure (a lower request makes it `Burstable`,
[Issue sizing](#issue-sizing-one-reservation-per-pod), "Guaranteed or Burstable"); the image probe
pod carries its own, smaller reservation, not the controller's, so a controller reservation the
pool cannot place leaves the probe, and the boot, unaffected
([Issue sizing](#issue-sizing-one-reservation-per-pod)).

The Sandbox is the claim's alone, where an issue's is its tree's. A release of the claim (a switch
back, or `legion claims stop`) deletes it, and its role Secret and volume with it. The orphan sweep
keeps it while the daemon knows the claim, running or suspended, so a suspended controller's
session survives, and deletes it once the claim is retired. Either delete runs under the pod's
launch turn and only at the version of the Sandbox it was decided on: a controller relaunched into
the Sandbox since has written it, so the delete is refused, the daemon logs `sandbox runtime: kept a
sandbox written since its delete was decided; the orphan sweep decides it again` naming it, and the
next sweep decides again on the claims known then. The controller Sandbox a daemon made before the
controller ran in a launcher pod (`role=controller`, one `worker` container), and the one a daemon
of the tree-volume layout made (a claim template named `tree`), are refused at boot like every
Sandbox of a layout before this one, each named: remove it before enabling issue pods
([Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees), step 3).

**Reaching it.** Nobody types into the pod. A person reaches the controller through Dispatch (a
message to its session on the Agents page, a reply to its ask, a mention) or Envoy, and reads its
session with `kubectl -n <namespace> logs legion-<project>-controller -c controller` (its launcher's
log, which carries the shim's and Oh My Pi's) or its transcript on the volume. A Send from the
Agents page is answered in the conversation that page shows. Wakes reach it as they reach the
operator's controller: it subscribes to `notifications.legion.<project>.controller` once it holds
the role.

**Switching back.** To hand the controller back to a person, set `controller: operator` (or drop
the key), drop `runtime.kubernetes.resources.controller`, which the daemon refuses at boot unless
`controller: daemon`, and restart the daemon. At boot, before it re-adopts or relaunches anything,
that daemon stops the controller's claim the earlier boot left, unless it is retired already: its
Sandbox is released and the claim retires (`legion claims list` shows it `retired`), logged as
`controller: stopping the controller an earlier boot under controller: daemon launched; this daemon
leaves the controller to its operator`. Then, whether it stopped the claim at this boot or found it
retired, it ends the controller record's registration while the record still names that claim's
session, minting it a capability nobody holds, logged as `controller: ending the registration of the
controller an earlier boot under controller: daemon launched; this daemon leaves the controller to
its operator`; a record naming another session, the operator's controller's once it has
registered, or none, is left as it is. One known residual: a controller whose session a lost volume
ended after it registered, switched back before its fresh launch registers, leaves the record naming
that dead session, which the switch back does not end. Nobody holds its secret, which lived only in
the dead agent, so it shows as a stale `controllerLocator` with admission wakes queued for nobody
until the operator's `legion controller start` replaces it. A daemon that cannot stop the claim
refuses to boot, naming
it (`stop legion-<project>-controller, …`), with the record untouched; one that stopped it but
cannot end its registration refuses naming it too (`end the registration of
legion-<project>-controller, …`), and the next boot, finding the claim retired, ends it. The
claim route refuses a launch of that claim on such a daemon (409, `legion-<project>-controller is a
launch of the daemon's own controller, and this daemon leaves the controller to its operator
(controller: operator)`), and the token of an earlier launch is an invalid boot token, so no pod of
the daemon's can replace the operator's registration. Then start the controller with `legion
controller start` ([Operator-launched controller](#operator-launched-controller)). Releasing the
Sandbox deletes the controller's volume with it, and the session on it, so switching to
`controller: daemon` again starts a fresh controller after the keeper's first one-minute wait, on
fresh budgets: a resume of the recorded session finds it gone (`workspace-init controller` exits 3)
and the daemon relaunches the claim fresh. A `legion claims stop` of the controller under
`controller: daemon` releases it the same way, so the keeper's retry after it starts a fresh
controller too.

**Rolling back the image.** A daemon built before LEGION-592 cannot run against a database a
`controller: daemon` daemon used: it lists the controller's claim as an issue keyed `""`, which every
agent's plugin refuses. Nothing deletes a claim row, and a switch back only retires this one, while
that release lists every claim of its project whatever its state or role. With the row present,
every agent's `read_record` fails, and the operator's controller cannot claim, since it reads the
state before it registers. So the fallback from `controller: daemon` is `controller: operator` on
this release, never the previous image. A revert that cannot be avoided goes in this order:

1. Remove `controller` and `runtime.kubernetes.resources.controller` from `legion.yaml`: the earlier
   release refuses both as unknown keys, and without them this release runs `controller: operator`.
   Restart this release with that file and let it boot once. Do not skip this boot: only this
   release ends the stopped controller's registration. It also releases the controller's Sandbox and
   volume itself, which the earlier release's orphan sweep would otherwise do at its first boot: that
   sweep counts no retired claim as known, so it takes a retired claim's Sandbox as readily as one
   whose row is gone. The boot is done when `legion claims list` shows
   `legion-<project>-controller` `retired`. If the boot is refused, stop here and follow the
   refusal's entry in troubleshooting.
2. Stop that daemon: the earlier release must not run beside it for the same project, and while it
   runs it still holds the claim in memory, so any write of that claim would put the row back.
3. Read the row, then delete it. `<project>` is the project's token: `project` in `legion.yaml`
   lowercased, with every character outside `a-z0-9` dropped, so `project: LEGION` gives
   `legion-legion-controller`; `legion claims list` shows it. Run
   `select token, state from claims where token = 'legion-<project>-controller';` and check it
   returns exactly one row, in state `retired`. Then run
   `delete from claims where token = 'legion-<project>-controller';` and check it answers
   `DELETE 1`; its pending delivery, if any, goes with it (`on delete cascade`). Delete by token,
   never by role: several projects' daemons can share the database. If either check shows
   anything else, do not start the earlier release.
4. Start the earlier release.

### Operator-launched controller

Under `controller: operator` the daemon launches no controller, under either runtime: it has no
process of the controller to start or resume, and it stops at boot the claim of one an earlier boot
under `controller: daemon` launched ([Switching back](#daemon-launched-controller)). It holds the
controller's record and reads the session's liveness from the Envoy role registry, and says when no
controller is registered ([Controller liveness](#controller-liveness)).

**The operator token.** `operator_token_file` in `legion.yaml` names a file holding one long random
string (`openssl rand -hex 32`). The Go daemon requires it under every runtime, because the operator
routes that spawn and drive claims authenticate against it as well as the controller's
(`operator_token_file is required: the operator routes that spawn and drive claims authenticate
against the bearer it names`). It is read once at boot, and is never an environment variable or a
flag.

**The operator-side file.** `legion controller start` reads a small file of its own, never the
daemon's `legion.yaml`, whose loader would run both GitHub Apps' `private_key_command` on your
machine and demand keys the controller never uses. `deploy/kubernetes/daemon/controller.yaml.example`
is the complete shape: the same key names as `legion.yaml`, only the thirteen the controller needs
(`project`, `daemon_url`, `operator_token_file`, `envoy_url`, `envoy_token_file`, `nats_urls`,
`nats_nkey_seed_file`, `dispatch_url`, `dispatch_token_file`, `instructions`, `omp_invocation`,
`omp_launch_prefix`, `state_dir`; `nats_nkey_seed_file` is the Go CLI's alone, which the TypeScript CLI refuses as unknown). Any other key is refused naming it and the example (`unknown key "runtime" in the
controller configuration; legion controller start reads only … — see
deploy/kubernetes/daemon/controller.yaml.example`); `nats_urls` is required; `dispatch_url` and
`dispatch_token_file` go together; and relative paths resolve against the file's own directory, with
no `~`. The operator token and the NATS nkey seed each sit in a file only you can read (mode 0600):
a group- or world-readable one is refused naming the path and mode (`… is readable by its group or
others (mode 0640); chmod 0600 it`). The daemon's read of either of its seeds holds a file it owns
to the same rule, and lets the group read only a root-owned file, as a pod's kubelet-mounted Secret is
([Configuration](#configuration)).

**Starting it.** Run `legion controller start --config controller.yaml`, where `daemon_url` (or
`--daemon-url <url>`, which replaces it) is the daemon's API as your machine reaches it. In order, and
keeping nothing until the daemon has answered, the command:

1. reads the file and refuses as above, and refuses a blank or unreadable Envoy or Dispatch token
   file, a `nats_nkey_seed_file` that is blank, unreadable, readable by its group or others, or
   holds no nkey user seed, a missing or blank instructions file, and an Oh My Pi invocation that
   does not resolve;
2. reads the project's repositories from the daemon's unauthenticated `GET /legion/v1/state`
   (`repositories`), since the controller's addressing names them, and refuses a daemon that does
   not answer, a body with no `repositories` object, and a mapped entry that is not `owner/name`;
   an empty mapping, or one whose every repository the daemon leaves out, launches the controller
   all the same, its addressing saying `The daemon maps no repository to this project now` with each
   repository left out and why. Then it probes that Oh My Pi as the controller will run it, with
   `omp models`, which starts no session, and refuses a pi-legion it does not load, or one speaking
   another daemon API contract, loaded without pi-envoy or with a pi-envoy at another plugin
   interface version, or loaded beside the pre-split package;
3. asks `POST /legion/v1/controller/secret` with the operator token as `Authorization: Bearer` and
   the contract step 2 held the plugin to (`{"pluginContract": <N>}`). The daemon compares the token
   in constant time, then refuses a contract that is not its own with 409, naming both, before it
   mints anything, so a `legion` and a daemon from different releases never cut the running
   controller off. Otherwise it mints a fresh controller capability, which replaces the previous
   one and its registration and ends every controller grant: the last start wins. The answer also
   carries the daemon's `gates.design`, and an answer without `root-issues` or `off` is refused
   with a request to upgrade the daemon;
4. writes the secret 0600 under the local state directory (`state_dir`, by default
   `$XDG_STATE_HOME/legion/<project>-controller`), beside the `legion` launcher and the
   deployment instructions;
5. asks `POST /legion/v1/controller/github-credential` with the capability step 3 minted
   (`{"secret": <capability>}`). The daemon compares the capability's hash in constant time under
   its controller lock and answers the review App's read-only token for every owner of the project,
   each rendered as gh's two files (`owners`, each owner's `hosts` and `config`, `default`, the
   first owner, `app`, and `expiresAt`, the soonest expiry), which the command writes as a gh
   bundle under `<state_dir>/gh`, one 0700 directory per owner (`hosts.yml` 0600 through a temp
   file and rename), printing `[legion] the controller's gh acts as the review App for owners
   <owners> (read-only) from <dir>; this process refreshes the tokens before they expire
   (<expiresAt>)`. While Oh My Pi runs, a goroutine re-fetches every minute, asking for the
   default the first answer named (`"default"` in the request), and rewrites only the owners
   whose `hosts.yml` changed, logging each change, failure and stop to
   `<state_dir>/github-credential.log` (never to the terminal, never a token); a 403
   `INVALID_CONTROLLER_CAPABILITY` (a later start replaced the capability) or 409
   `CONTROLLER_LAUNCHED` ends the loop, and the files last until the tokens' expiry — the loop's
   lifetime is Oh My Pi's, not the command's signal context. Against a daemon with no GitHub App or
   no project owner (500 `GITHUB_TOKEN_SOURCE_UNAVAILABLE` / `GITHUB_OWNER_UNCONFIGURED`), the
   command empties the 0700 `gh` directory, prints `[legion] the daemon has no GitHub App to act
   as; the controller's gh acts as nobody`, and runs no loop. Any other failure of this fetch
   (unreachable, 403, 409, 502) exits 1 naming the route and the daemon's sentence, and Oh My Pi
   never starts: the mint in step 3 has already replaced the incumbent, as every post-mint failure
   does. Before step 3 the command finds the real gh its session's `gh` wrapper runs: the
   operator's `LEGION_GH_PATH`, or the first `gh` on its PATH that is not the state directory's own
   wrapper, and refuses to start with neither (`gh is not on PATH`) or when that first `gh` is a
   script, a `gh` wrapper of the operator's machine that would answer in place of the review App;
6. runs Oh My Pi interactive in the foreground (`omp_launch_prefix` and `omp_invocation`, one joined
   `--append-system-prompt` holding the controller prompt, the daemon's `Design gate policy:` line,
   the project's repositories sentence and the deployment instructions, no `--resume`, no
   `--mode rpc`) with the controller's
   environment (`LEGION_CONTROLLER=1`, `LEGION_ROLE=controller`, `LEGION_DAEMON_URL`,
   `LEGION_PROJECT`, `LEGION_STATE_DIR`, `LEGION_CONTROLLER_START_MESSAGE` (the pi-legion
   extension sends it as the session's first turn right after its role claim succeeds and before
   it opens the live wake subscription, so the controller's first turn runs its start procedure
   deterministically, with nothing typed, rather than racing a wake for the session's one
   first-turn slot — LEGION-392), its grant and secret files, `LEGION_GH_OWNERS_DIR` naming
   `<state_dir>/gh` (the bundle step 5 wrote), `GH_CONFIG_DIR` its default owner's directory (the
   bundle's root, empty, when the daemon has no GitHub App), `LEGION_GH_PATH` the real gh, with
   `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` emptied, so nothing inherited outranks the files, the
   Envoy and Dispatch
   endpoints, and `NATS_NKEY_SEED_FILE` naming `nats_nkey_seed_file` when the file sets it) on top
   of the operator's own environment, less `NATS_DAEMON_NKEY_SEED` and `NATS_DAEMON_NKEY_SEED_FILE`
   (the controller is pane-side, and never gets the daemon's seed) and less `LEGION_BOOT_TOKEN` and
   `LEGION_BOOT_TOKEN_FILE` (a start from inside a Legion pane inherits that pane's, and the plugin
   takes a controller carrying one for a daemon-launched controller), and exits with Oh My Pi's exit
   code.

A refusal before the secret is written removes the directories made for the probe, so the state
directory is as it was.

**How the daemon sees it.** The session's pi-legion extension registers on
`POST /legion/v1/claims/register` with the capability and takes the controller role. The daemon
records `controllerLocator: {runtime, external: true, sessionId, registeredAt}` (`legion state
--json`, `GET /legion/v1/state`), `runtime` being the daemon's own. On every orphan sweep it reads
`GET /v1/roles/legion-<project>-controller` from the Envoy listener with its bearer. The controller is
alive while the holder is the recorded session and its `last_seen` is within
`max(worker_boot_timeout_seconds, 2 × 120 s)`, which is 240 s at the defaults. It is gone when the role
is unheld or expired, held by another session, or last seen that long ago. It is unknown, never gone,
when the listener is unreachable, refuses with anything but a 404, or answers a body the probe cannot
read.

**Re-running and failing.** Running the command again mints a new capability and takes the role.
Closing the terminal leaves the project without a controller until you run it again. Failures name
what to fix:

- a wrong token: `<daemon_url>/legion/v1/controller/secret: the daemon answered 403 Forbidden: Invalid
  operator token — the operator token does not match the daemon's operator_token_file`;
- an unreachable daemon: `could not reach the Legion daemon at <daemon_url>: …; is the port-forward
  running? (never falls back to another address)`.
