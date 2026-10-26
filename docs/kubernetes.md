# Legion on Kubernetes

This runbook covers the worker image, the session store, the Kubernetes runtime, the
operator-launched controller, and the one thing in it a role does for itself: running a workload
in the cluster from a role ([Running a workload in the cluster from a
role](#running-a-workload-in-the-cluster-from-a-role)).

The Go coordinator (`packages/daemon`, LEGION-208) runs each process as an Agent Sandbox; its live proof is `scripts/e2e/stage4b-sandbox-tree.sh` on the production cluster. The TypeScript daemon no longer runs on Kubernetes: it refuses `runtime: kubernetes` at config load and names the Go daemon (LEGION-286), and its sources were deleted on 2026-10-03. Its runtime's section below is kept for reference until someone removes it (LEGION-223).

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
- OMP's native module, extracted at build into `/home/legion/.omp/natives/<version>/`: from 18.11.0 OMP writes only the variant the CPU selects (AVX2 or not), so this is the build runner's variant, and a pod whose node selects the same one extracts nothing, while a node that selects the other has each role write that one (111 MiB) into its own home overlay, on the pod's `role-homes` volume, at its first OMP run;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs) and `gh` at `/usr/local/bin`, and
  `git` at `/usr/bin/git` from the `debian:trixie-slim` base — jj's git backend requires git >= 2.42
  (bookworm's 2.39.5 made every `jj git clone` in the init container fail), so the build also proves the
  image's jj accepts its git with a network-free `jj git clone` of a scratch bare repository before the
  probes run, and its last step refuses a git anywhere but `/usr/bin/git`;
- `socat` from Debian trixie's archive, at `/usr/bin/socat`: a byte pipe that carries a connection over
  `kubectl exec` to a port the pod listens on at `127.0.0.1`, such as a dev server an agent runs
  (`kubectl -n legion exec -i <pod> -c roles -- socat - TCP:127.0.0.1:<port>`). `kubectl port-forward`
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
  on it), `curl`, `wget`, `jq`, `flock` (from `util-linux`: the install Legion renders for an
  installing launch serialises on the issue's shared bun install cache with `/usr/bin/flock`, so a
  `roles` container whose image lacks it fails every installing launch; the apt line names the package
  even though Debian's `priority:required` set already brings it into the base, so the day a base
  drops it the build fails instead of every launch in the fleet), `ssh-keygen` (from
  `openssh-client`, which also brings `ssh`, `scp` and
  `sftp`: they reach no host that `curl`, `wget`, `git`, `python3` and `socat` on the same apt line
  could not already reach, and what a pod may reach is Legion's ruling, the withheld `network`
  capability row — dispatch://LEGION-5, the pod is the boundary, except a workload a role starts
  where its deployment's cluster admits it and the Service fronting it
  ([Running a workload in the cluster from a role](#running-a-workload-in-the-cluster-from-a-role))
  — a declaration the capability report
  renders, not a network control this repository ships; the package's setuid
  `/usr/lib/openssh/ssh-keysign` and setgid `/usr/bin/ssh-agent` lose those bits in the `runtime-base` stage,
  and the build's final step refuses an image where any file under `/usr/lib/openssh` or
  `/usr/bin/ssh-agent` carries one),
  `psql` (from `postgresql-client`: the Dispatch browser suite `bun run e2e`, which a
  reader-facing Dispatch change must pass before it merges, reaches `psql` through
  `packages/dispatch/e2e/psql.ts` — `packages/dispatch/e2e/seed.ts` calls it for every spec's
  database reset — and the local stack `bun run dispatch:local`
  (`packages/dispatch/e2e/local-instance.ts`) refuses to start without `go`, `psql`, `flock` and
  `openssl` on `PATH`, and without `docker` too unless a `DATABASE_URL` is given; the image
  carries `go`, `flock` and `openssl`, a `DATABASE_URL` stands in for `docker`, and `psql` was the
  one a role had no way to supply, so a role on a digest without it cannot run the suite its own
  change owes — Debian's `psql` is a Perl wrapper around the versioned client, and it resolves
  because `perl-base` is in the image),
  `redis-cli` (from `redis-tools`: no
  repository of the project speaks Redis today, and it is carried anyway, at about a megabyte,
  because a second image roll costs more than a megabyte), `sqlite3` (a repository's own SQLite
  fixture or database has no other way to open from a shell, and Oh My Pi's own profile databases
  under `~/.omp/profiles/legion` are SQLite as well; a role's session is not one of them — it is
  `results.jsonl`, `state.json` and `title` under `XDG_STATE_HOME`, and the session store these
  pods otherwise use is Postgres), `nc` (from `netcat-openbsd`, for the reachability check a role would
  otherwise write as an inline Python loop: the package ships `/usr/bin/nc.openbsd` and the `nc`
  name comes from the alternatives system rather than being a file of the package, so every gate
  asks for `nc` and never `nc.openbsd`), `dig` (from `bind9-dnsutils`, the package trixie's virtual
  `dnsutils` resolves to: how a role whose connection to a service name fails tells "the name does
  not resolve" from "the packets are dropped"), `gcc` (with `libc6-dev`, the libc headers and startup
  files it links with: the C compiler Go's cgo builds with, and Go refuses `-race` without cgo, so
  they are what lets a role run the race detector; the paragraph after this list says how cgo's
  default is kept),
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

`gcc` on `PATH` would turn cgo on by default (stock Go does whenever it finds a C compiler), so the
image keeps it off and a role opts in per command, `CGO_ENABLED=1 go test -race …`, with no `CC`,
zig or linker override. Two places hold the default. The image's `ENV CGO_ENABLED=0` reaches every
process that inherits the image's environment: the bash tool, subagents, `go test`'s test binaries,
a `go/build` reader such as `go/types`' source importer (which reads only the process environment),
and a toolchain Go switches to for `GOTOOLCHAIN` or a `go.mod` `toolchain` line. A `CGO_ENABLED=0`
line the build appends to `/opt/go/go.env`, the file Go reads for a distributor's defaults, holds
where that environment is scrubbed: Oh My Pi's Python eval kernel, which starts from an allowlisted
environment without `CGO_ENABLED`, and a test that runs `go` under an environment it builds. The
build's toolchain step reads `go env CGO_ENABLED` with the variable removed from the environment
and fails unless it is `0`, so the build fails if the `go.env` line is ever lost.
`go env -w CGO_ENABLED=1` does not override the image's `ENV`, because Go ranks the process
environment above its `go env -w` file; and `runtime.kubernetes.pod.env` refuses `CGO_ENABLED`, as
it refuses every variable the image's `ENV` sets. Two cases get Go's own default, cgo on: a scrubbed
environment that also switches toolchain (the switched toolchain's `go.env` lacks the line), and a
`go/build` reader under a scrubbed environment (it never reads `go.env`). With `gcc` present both
work: the switched toolchain's builds succeed, linked dynamically, and the reader's cgo processing
runs. A deployment still pinning an older image digest has no `gcc`
and needs zig as `CC`
(`docs/solutions/testing/the-race-detector-runs-in-a-worker-pod-with-zig-as-the-c-compiler-and-a-library-callback-from-an-earlier-test-writes-your-log-sink-INFRA-182.md`).

It runs as user `legion` (uid 1000, declared numerically so `runAsNonRoot` can verify it from the image
alone) with `HOME=/home/legion`, which must be writable (OMP writes logs and `models.db` under
`~/.omp/profiles/legion`; every session is a row in the session database). Mount writable volumes
below the profile directory, never at `/home/legion`
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

### The tools a role's own work needs

The image carries the five clients above — `psql`, `redis-cli`, `sqlite3`, `nc` and `dig` — and for
everything else the one documented step is the pinned form of the three package runners the image
already has: `uvx`, `bunx` and `go run`, plus `uv run --with` where what a role needs is a library
rather than a command. The two answer different halves of the question. The image
answers the tools a role may not arrange for itself — `apt` cannot write a system prefix in an
unprivileged container, unpacking a package by hand works for some tools and not for others, and a
client a role unpacks onto its own `PATH` is the withheld `operator-setup` capability — while the
runners answer the tools no image can anticipate.

What decided it is the container, not the cost. Measured in a role container of a deployment: the
role is an unprivileged user, `/usr/local/bin` and `/opt` are read-only, `/home` is read-only while
`$HOME` (`/home/legion`) under it is writable, and `apt-get` cannot take dpkg's lock even to
download.

```text
$ id
uid=1000(legion) gid=1000(legion) groups=1000(legion)
$ apt-get install -d -y postgresql-client
E: Could not open lock file /var/lib/dpkg/lock-frontend - open (13: Permission denied)
E: Unable to acquire the dpkg frontend lock (/var/lib/dpkg/lock-frontend), are you root?
```

So `apt` cannot write a system prefix in an unprivileged container. Unpacking a package's files by
hand is a separate question — it works for some tools and not for others (the extraction row below)
— and what rules it out is not the unpacking but the test the role's own skill states, in
`skills/legion-worker/references/tools.md` under "What the withheld capability is, and what it is
not": an extraction under that writable `$HOME` is outside the sanctioned form whatever a role
scopes it to, and so is a client left where the role's later commands and its subagents resolve it,
which is the withheld `operator-setup` capability
(`packages/daemon/internal/capabilities/capabilities.go`). A test harness's own prefix on the
issue's disk, reached by an absolute path or through a `PATH` its own script exports around its own
commands, passes that test and is the role's own business — which is how the Dispatch browser
suite's rootless Postgres with pgvector is stood up in a pod today. So a role wanting a client of
its own on its `PATH` has the image, a pinned runner, or `uv run --with` for a library. The five
priced options and their verdicts:

| option | the measurement | verdict |
| :--- | :--- | :--- |
| the image carries the five clients | 20 Debian packages and 5.9 MiB of downloads (6,216,928 bytes), from `apt-get install -y --no-install-recommends --print-uris bind9-dnsutils netcat-openbsd postgresql-client redis-tools sqlite3` against this image's own installed package set, so it is the true increment. In the unit a cold launch pays — compressed image size, each read from that image's own manifest in the registry, and each image's `org.opencontainers.image.revision` read from its config blob — this branch's head, `ghcr.io/sjawhar/legion-worker:sha-afa29128061f` (revision `afa29128061fc10dd6c6aa3755ba051b98b84e02`), is 24 layers and 1026.9 MiB (1,076,764,060 bytes) against the 24 layers and 1019.0 MiB (1,068,477,764 bytes) of the base it forks from, `:sha-92dbeea3d391` (revision `92dbeea3d3911b5fb08e279fb201634a84ffe72e`): 8,286,296 bytes, 7.9 MiB on an image of a gigabyte, of which the runtime stage's Debian layer is 8,285,191 bytes (327,156,512 → 335,441,703), the other 23 layers carrying 1,105 bytes between them and none of them differing by more than 3,164 bytes | **chosen** |
| a per-issue install onto the issue's disk | the issue's disk is 49 GiB, and every cache already sits on it (`XDG_CACHE_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `GOCACHE`, `GOMODCACHE`, `BUN_INSTALL_CACHE_DIR`, `UV_CACHE_DIR` and `UV_PYTHON_INSTALL_DIR`, the nine `skills/legion-worker/references/tools.md` lists), so disk is not what rules it out | **rejected for a compiled client on a role's `PATH`**: `apt` refuses above, and leaving one unpacked where a role's later commands resolve it is the withheld `operator-setup` capability (ruling dispatch://LEGION-200), the extraction row below. Still right for a language's own package set — which is the runner row — and for a test harness's own prefix, which only its own commands resolve through |
| a runner the image already provides, pinned and failing closed | `uvx ruff@0.14.0 --version` takes 1.7 s with the uv cache empty and 0.15 s warm; `bunx --package @biomejs/biome@2.6.3 biome --version` 0.3 s with bun's cache warm; `go run github.com/nats-io/natscli/nats@v0.1.5 --version` 7.2 s with the module cache warm, 103 s on a cold one | **chosen, as documentation rather than new code**: the image already carries all three |
| extracting Debian packages onto the issue's disk | 30 packages, 11 MiB of downloads, 105 MiB extracted; `redis-cli`, `sqlite3` and `dig` ran from the prefix with `LD_LIBRARY_PATH` set and `psql` did not, Debian's `/usr/bin/psql` being a Perl wrapper that loads `PgCommon.pm` through Perl's own search path, which no extraction prefix is on | **rejected as the way to get these five clients onto a role's `PATH`**: that is the withheld `operator-setup` capability (`packages/daemon/internal/capabilities/capabilities.go`, ruling dispatch://LEGION-200) under the test in `skills/legion-worker/references/tools.md`, "What the withheld capability is, and what it is not", and it is fragile for exactly the client this change exists for. The same unpacking inside a test harness's own environment passes that test and is a different verdict — and it would not have got a role `psql` either way |
| `runtime.kubernetes.setup`, the operator-set command run before each role's agent starts | it runs on every start of every role, six times per issue | **rejected**: it buys nothing the image does not, since it is still an unprivileged container |

The forms a role types, the rule that a pin names a coordinate as well as a version, and the line
between a sanctioned pinned run and the withheld `operator-setup` capability are a role's own
documentation rather than an operator's, and they live in one place:
`skills/legion-worker/references/tools.md`. The worker image ships that skill, so a role reads it
where it works, and a tool no runner reaches is the image change that reference spells out, with
the grep that finds its sites.

### Image size and pull time

A node that has never run the image pulls it whole before the pod's init containers start, so the
image's compressed size is a cold launch's first cost. Before the capability tools above,
`ghcr.io/sjawhar/legion-worker:1.18.1` was 12 layers and 369 MiB compressed (386,510,448 bytes). With
them it is 22 layers and 1007 MiB compressed (1,056,280,622 bytes, the `sha-ff33c8c9fb1b` build of the
pull request that added them): the Debian layer grows from about 30 MiB to 307 MiB, almost all of it
Chromium and the library closure it depends on; the Go toolchain is 64 MiB and `gopls` 20 MiB; Node's
tree with the two npm language servers is 66 MiB. The five client tools
([The tools a role's own work needs](#the-tools-a-roles-own-work-needs)) cost 7.9 MiB — not a slice
of that 22-layer figure, but a difference measured like for like between a later pair of builds,
this branch's head and the base it forks from: the head
`ghcr.io/sjawhar/legion-worker:sha-afa29128061f` (revision
`afa29128061fc10dd6c6aa3755ba051b98b84e02`) is 24 layers and 1026.9 MiB (1,076,764,060 bytes)
where the base it forks from, `:sha-92dbeea3d391` (revision
`92dbeea3d3911b5fb08e279fb201634a84ffe72e`), is 24 layers and 1019.0 MiB (1,068,477,764 bytes) —
8,286,296 bytes, of which the runtime stage's Debian layer is 8,285,191 bytes (327,156,512 →
335,441,703); the other 23 layers carry the remaining 1,105 bytes between them, no single one of
them differing by more than 3,164 bytes in either direction. Each size is read from that image's
own manifest in the registry and each revision from its config blob; a layer rebuilt from the same
content still compresses to a slightly different size, so read the figure as 7.9 MiB rather
than as its last digits. On EKS, how long a node takes
to pull that is not recorded here: the stage 4a launch below is where it is measured. The pull
must fit the launch's wait for the role's launcher before any init container runs:
`worker_boot_timeout_seconds` (`packages/daemon/internal/config/config.go`; 120 s by default),
which the wait extends only once
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
`asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-writing-specs` and `envoy` skills in its), so a build whose OMP or plugins are
broken fails instead of publishing. The build has none of the operator's model configuration, so it
leaves those agents' models unresolved (`--skip-agent-models`). It then checks every image-site row of
the capability table (`packages/daemon/internal/capabilities`) against the image, as a worker's Oh My Pi
would find each — `omp setup python --check`, the `chromium` on `PATH` (or the browser a
`PUPPETEER_EXECUTABLE_PATH` in the pod's env names) run with `--version`, the three language servers on `PATH`, the CodeGraph CLI with its plugin enabled in the
profile's lock, and `go`, `gcc`, `curl`, `wget`, `python3`, `node`, `bun`, `uv`, `kubectl`, `jq`,
`ssh-keygen`, `flock`, `psql`, `redis-cli`, `sqlite3`, `nc` and `dig` on `PATH` with `go version`
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
tool resolves a subagent's (`packages/daemon/internal/runtime/sandbox/probe.go`). Before those
probes the probe Sandbox's command adds `--role-namespaces /var/run/legion/role-probe`, a disk
`emptyDir` (`role-probe`) the probe pod mounts there as an issue pod mounts `role-homes`: the probe
first runs one role generation through `legion launcher role-init` as an issue pod's launcher
runs every role, over a layout of its own in that directory and with its own `legion version` as
the shim, and exits 1 with `legion probe-image: role namespaces: <why>` when the runtime or node
refuses what that generation makes (`launcher.CheckRoleNamespaces`): user, mount, PID and IPC
namespaces of its own; its gh, private and state directories bound at the fixed paths; an overlay
whose lower layer is an empty home directory on the probe's own `emptyDir`; its own `/tmp` and
`/var/tmp`, memory filesystems at `/run/lock` and `/dev/shm`, a devpts at `/dev/pts` and an mqueue
at `/dev/mqueue`; the covers and its own `/proc`; and `legion version` started as uid 1000 in a
nested user namespace, which must exit 0. So such a cluster fails the daemon's boot rather than
every role's start. The probe does not test the overlay over the image's own `/home/legion`, whose
lower layer is on the container's root filesystem, the home's volumes bound back on top of it, or
the copy of the role's own config home: those run first when a role starts in an issue pod. The
image build's own run passes no `--role-namespaces`, since a build's container may not make the
namespaces. The runtime requires
`capabilities=checked` too: an image whose `legion` predates the capability check prints no such mark and
is refused naming that (`Succeeded without checking the capability list (its legion CLI predates the
check): build the image from this daemon's commit`), and a probe pod that exits 1 on a missing capability
is refused with its log tail, which names it (`capability <name> is missing: <detail>` under the table's
`missing` row; the stage 4a harness's `image-probe-capability-negative` check drives that with an operator
pod env `PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium`). An image the kubelet cannot use refuses
the boot at once, from the probe pod's status: a container waiting `InvalidImageName` or
`ErrImageNeverPull`. A pull the registry refused because it does not have the image refuses it once
the kubelet has met that answer on two pulls: a container waiting `ErrImagePull` or
`ImagePullBackOff` whose whole message, after the back-off's `Back-off pulling image "<ref>":
ErrImagePull: ` when it carries one, is containerd's answer that the registry had no manifest for
the reference, naming that one reference throughout (`rpc error: code = NotFound desc = failed to
pull and unpack image "<ref>": failed to resolve reference "<ref>": <ref>: not found` on a local
pull, or `… failed to resolve image: <ref>: not found` through containerd's transfer service, the
default pull from containerd 2.1), with the pod's `Failed` events (`Failed to pull image "<ref>":
<that answer>`) counting two pulls or more.
One answer is no proof: containerd 2 resolves a digest as a manifest and then as a blob, and
reports the blob request's 404 — which a registry such as GHCR answers for a manifest it holds —
over a manifest request the network cut. The kubelet pulls again about ten seconds after the first
failure, and the daemon reads the events at most every 15 s, so the refusal comes within about half
a minute of the first pull (on kubelet 1.32 and later, whose `ImagePullBackOff` message carries the
failed pull's error; an older kubelet's carries none, so the refusal waits for a read that finds the
container waiting `ErrImagePull`, later but within the attempt): `worker image sha256:<digest>
failed its probe: the registry does not have image <image>: …`, quoting the kubelet. Every other
failed pull — credentials the registry
refused (`pull access denied`, `401`, `403`; Docker Hub answers a repository it does not have this
way), a registry the node cannot reach (`i/o timeout`, `connection refused`, `connection reset by
peer`), a rate limit (`429`, `toomanyrequests`), a server error, containerd's other not-found
answers (`no resolve hosts`, a manifest past its size limit, a manifest or layer the registry named
and then did not serve), and a manifest-404 met on one pull — may pass on a later attempt, and
stays under the six-attempt budget. One exception follows from containerd 2 reporting a 404 over a
failed request: on a node whose containerd tries a mirror before the registry (`hosts.toml`), a
mirror without the image is refused as a missing image while the registry behind it stays
unreachable or keeps refusing the node's credentials. A volume of the operator's pod block
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
<kubelet message>` (or `Secret "<name>"`, `PersistentVolumeClaim "<name>"`, `NFS export
<server>:<path>`; a `projected` volume lists each ConfigMap and Secret source joined with `and`; a
volume the block does not name reads `the probe pod <name> cannot mount volume
"<volume>", which runtime.kubernetes.pod.volumes does not name: <kubelet message>`; a share's own
refusals, a claim the scheduler cannot find, a mount the node ran and that failed, and gVisor's
failed start over a share it cannot read, are under [Network shares](#network-shares)) — within one event
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
version` with `go env GOROOT` required to be `/opt/go`, `gcc --version` (Go's cgo, and so its race
detector, needs it), `go env CGO_ENABLED` with `CGO_ENABLED` removed from the environment required
to be `0` (so the build fails if `/opt/go/go.env`'s `CGO_ENABLED=0` line, the default a scrubbed
environment depends on, is ever lost), `gofmt`, `kubectl version --client`, and trixie's
`python3`, `curl`, `wget`, `jq --version`, `flock --version`, `psql --version`, `redis-cli --version`,
`sqlite3 --version`, `nc -h`, `dig -v`, `chromium --version` and `socat -V` included — the corepack shims fetching their shipped default pnpm and yarn
into a scratch directory the step removes. `ssh-keygen` takes no `--version`, so the step generates a
throwaway ed25519 key in that scratch directory and fingerprints it with `ssh-keygen -l`. Two of the
five clients are odd the same way: `nc` takes no `--version` either, so the step runs `nc -h`, which
prints its usage and exits 0, under the `nc` name the alternatives system installs and never
`nc.openbsd`; and `dig -v` prints its version on stdout, measured in a pod of the image (`dig -v
2>/dev/null` prints `DiG 9.20.30-1~deb13u1-Debian`, `dig -v 2>&1 >/dev/null` prints nothing). It reruns only
when the toolchain or an earlier layer changes, so a commit that only rebuilds `legion` needs no package
registry.
To check a published image's toolchain end to end, a project Python install included:
`docker run --rm --entrypoint sh ghcr.io/sjawhar/legion-worker@sha256:… -c 'uv --version && node --version && npm --version && aws --version && kubectl version --client && go version && gcc --version && gopls version && python3 --version && jq --version && flock --version && psql --version && redis-cli --version && sqlite3 --version && nc -h && dig -v && ssh-keygen -t ed25519 -N "" -f /tmp/k && chromium --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.

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
that digest. `release-envoy-listener.yaml` needs no re-run when its `release_publish` finds, at "Confirm the
version", that another run took the tag first: it publishes nothing and, when its commit still needs a release
and no later run on main still running holds it, starts a new run on main itself, which decides the version again
and re-stamps the binaries.

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
pod's volumes would shadow them. Oh My Pi's Python tool also starts its kernel with an allowlisted
environment (`PATH`, `HOME`, `USER`, `TMPDIR`, the `LC_`, `XDG_` and `PI_` prefixes and a few more), so
any other `ENV` of the derived image (a toolchain home such as `ELAN_HOME`, a cache directory) is missing
there and in everything it spawns, while the bash tool keeps it: when a command needs such a variable,
install it as a wrapper in `/usr/local/bin` that sets the variable when it is unset. What the deployment
adds churns on its own schedule, not Legion's release schedule, and the deployment's repo owns its
reproducibility.

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
`runtime.kubernetes.session_store: postgres` or `cloudsql-iam`, its two database stores
([Selecting the store](#selecting-the-store) below); the tmux runtime has no such setting.

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
| the running Oh My Pi build predates the setting                   | the variables are ignored and the session stays on files — caught before it can happen: the session-storage launch probe (`verifySessionStorageSetting`, `boot-probes.ts`) runs inside the worker image through `legion probe-image` (see [The image is probed before it publishes](#the-image-is-probed-before-it-publishes)), fails on such a build so the image never publishes, and on a passing image prints `session-storage=probed` on the command's OK line, which a `legion probe-image` of this release prints only once that probe has passed, under every store ([The image guard](#the-image-guard)) — the daemon never probes a host OMP for it |

### What stays on the pod's disk

Only the transcript moves, and only the transcript survives the pod. Tool artifacts, image blobs
and `models.db` stay under the agent directory
on local disk (`~/.omp/profiles/legion/agent/…`), and `.legion/` handoffs live in the repository. A
pod that dies loses those local files — the conversation it does not. OMP's logs
are under the role's state home (`/legion/scratch/state/<role>/omp/profiles/legion/logs`, [Anatomy
of a Sandbox pod](#anatomy-of-a-sandbox-pod)), which is on the pod's own disk, the one directory
of the issue's scratch named for the role (every role of the issue can still write it): those
logs, Chromium's `browser-profiles`, the plugin generation a stale-plugin relaunch installs and
the workspace's CodeGraph index (`.codegraph/` in the workspace, 185 MB for this repository, built
by a role's shim after its Oh My Pi starts) all live as long as the pod and no longer ([The disk
dies with its pod](#the-disk-dies-with-its-pod)). Read a role's log from its pod's container while
its pod is there; the pod that replaces it starts with none of it.

### The extension under SQL storage

The Legion plugin (`@sjawhar/pi-legion`) recognises a `task` subagent inside a Legion worker without looking
for the parent's transcript on disk: its claim session records which session it bootstrapped on the interface
the two plugins share, and a later session start in the same process with a different transcript path is a
subagent (the check is `@legion/pi-shared/subagent-session`, which both plugins run;
`packages/pi-envoy/AGENTS.md`, the subagent convention, and `packages/pi-shared/AGENTS.md`).
Nothing in either plugin reads the two variables, and neither needs any other change for SQL storage.

### Selecting the store

Three keys in `legion.yaml`, all inside the `runtime.kubernetes` mapping: `session_store` chooses
the kind, and each kind carries the one key that opens its store. The store is required, and must
be one that keeps sessions in a database:

```yaml
runtime:
  kubernetes:
    session_store: postgres          # or cloudsql-iam; pvc and an omitted key are both refused
    session_dsn_secret: SESSION_DSN  # the providers-Secret key that holds the connection URL
```

```yaml
runtime:
  kubernetes:
    session_store: cloudsql-iam    # every pod signs in to Cloud SQL as itself; no URL is held anywhere
    session_database:              # the database each pod signs in to; all three members are required
      instance: example-project:us-central1:example-instance  # the connection name, project:region:instance
      name: legion_sessions                                   # the database in that instance
      user: legion-sessions@example-project.iam               # the IAM login a pod signs in as
```

`postgres` and `cloudsql-iam` both keep every pod agent's conversation, each role's and a
daemon-launched controller's, in Oh My Pi's table; what differs is how a pod reaches that database
and what it must hold to do so (`Store` and `Database`, `internal/ompsessions/store.go`), which for
the second of them is [What `cloudsql-iam` puts in a pod](#what-cloudsql-iam-puts-in-a-pod), below.
`pvc`, which kept each conversation a file in the `sessions` directory of the issue's disk, is
refused, and so is an omitted `session_store`, which settles to it: the loader gives both the same
line and its reason (`readSessionStore`, `internal/config/kubernetes.go`):

```
runtime.kubernetes.session_store must name a store that keeps sessions in a database, and is
required (INFRA-80): an issue pod's disk is the pod's own ephemeral volume, deleted with the pod,
so a session kept as a file on it ('pvc', also what an omitted session_store means) dies with it,
the next launch's resume fails on the missing file (internal/launcher's sessionWritten), and three
such failures retire the claim
```

There is no silent default, because a default would be a deployment that boots and then loses
every conversation at the first pod replacement. **So the deployment names its store and that
store's key before this release boots**, and the database is reachable from the pods: an upgrade
that applies the image first has a daemon that refuses its own configuration at load ([Cutting
over to the pod-owned disk](#cutting-over-to-the-pod-owned-disk)).

Each kind names its own key and refuses the other's, an inert key being refused and never ignored:
`runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is
cloudsql-iam; remove it`, and `runtime.kubernetes.session_database is not used when
runtime.kubernetes.session_store is postgres; remove it`. `postgres` without its key is refused
(`runtime.kubernetes.session_dsn_secret is required with runtime.kubernetes.session_store postgres:
the key of the providers Secret that holds the session database's postgres:// URL`), and so is a
value that is no Secret data key (`[-._a-zA-Z0-9]+`), naming it. All three members of
`session_database` are required, since no pod reaches the database without them; `instance` must
be a connection name the Cloud SQL connector uses whole — `project:region:instance`, or
`domain:project:region:instance` for a legacy domain-scoped project, with no whitespace anywhere,
which its own parser (`instance.ParseConnName`) accepts and reads back as exactly that string, so a
longer string it would take only part of is refused too (`ValidInstance`,
`internal/ompsessions/store.go`, which the pod's proxy holds to as well); and a member the mapping
does not model is refused by name (`readSessionStore` and `readSessionDatabase`,
`internal/config/kubernetes.go`).

**No deployment runs `cloudsql-iam` yet, and the sign-in it needs cannot be granted from here.**
Every Legion pod of every project runs as the one Kubernetes ServiceAccount the operator's
`runtime.kubernetes.pod.service_account` names (`ServiceAccountName`, `manifest.go`), so every pod
presents the same cloud identity: a federated principal with no email address, where Cloud SQL
accepts only a user, a service account or a group as a database login. Giving that shared account
an email-bearing identity of its own would hand every project's pods one sign-in, so it is not done
here. `dispatch://INFRA-321` gives each project's pods an account of their own,
`dispatch://INFRA-311` declares the login, its grant and the instance's identity flag where the
deployment is declared, and `dispatch://INFRA-320` points a deployment's session store at that
login once its pods can use it. Everything written below is implemented and configurable on this
branch; no deployment has it turned on.

`postgres` holds the whole connection URL, password and all, in one providers-Secret key. The
daemon never holds the connection string: put it in the providers Secret
(`legion-<project>-providers`) under the key `session_dsn_secret` names, as one `postgres://…` URL.
Every issue pod, the controller's pod and the image probe mount that key read-only at
`/var/run/legion/providers/OMP_SESSION_SQL_DSN`, whatever the key is called, so a Secret without it
refuses boot at the image probe, naming the Secret and its keys
(`internal/runtime/sandbox/manifest.go`, `providers`).

With the store settled, every generation a launcher starts gets two variables in its start command
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

**One login, every conversation: what that costs.** Under `postgres` every role container mounts
the providers Secret with the session database's URL among its keys, and the agent is told where
that file is (`OMP_SESSION_SQL_DSN_FILE`, above), so every agent in the deployment holds the login
the whole table sits behind. Oh My Pi's two tables are keyed by the session's recorded path and
carry no tenant, project or issue column, so rights over the table are rights over every
conversation in it: the controller's, and every role's of every issue of every tree. An agent that
obeys text it was handed can therefore read another issue's transcript and rewrite it — and these
agents read issue bodies, pull request comments and fetched pages by design, so text written to be
obeyed reaches them as a matter of course. A rewritten row is not an inert file: it is what that
role's next resume replays into its own agent, so the write lands in another agent's head. The
party reachable this way is any other issue, role or tree **of this deployment**, never one outside
it: the database holds this deployment's sessions, and no pod of it mounts another's.

Under `cloudsql-iam` there is no such credential to hold: what the agent reads carries a login and
no password at all (`Endpoint.AgentURL`, `internal/ompsessions/store.go`), and what it names is a
unix socket in the generation's own directory on the role's own memory-backed private volume,
which the pod's one `roles` container mounts under that role's directory alone and which no other
role's namespace is shown (`legion launcher role-init`). Any process of the role's own generation
can still dial that socket, as any process in a pod can read the URL file under `postgres`: this
store removes the password, not that trade, and the rights the login has in the database are still
every conversation's.

Under `pvc` a session was a file on a disk only its own issue's pod could mount, so no issue
could reach another's at all, and this release widens that boundary rather than keeping it. That
is the trade LEGION-654's spec accepts for sessions that outlive their pods, and it is a trade to
weigh before the rollout rather than after: a deployment that cannot accept one agent reaching
another's transcript has no setting in this release that stops it.
Where it is closed properly is `dispatch://INFRA-228` — a credential per issue, or a row-level
policy the database itself enforces — and nothing in this release anticipates that work.

The login is not narrowed here, and narrowing it would buy nothing. A login restricted to
`omp_session_files` and `omp_session_files_parts` still reads and writes every row of both, which
is the whole of the exposure above; isolation has to be per issue — a credential of its own, or a
policy the rows are checked against — and no table-level grant is that. The project also declined
the same narrowing on this same store on `dispatch://INFRA-244`, so deciding the other way here
would put two contradictory postures on one credential in one deployment. What this release does
ask of the login is unchanged: give it no rights beyond the database of those two tables, and
treat its URL as a credential every agent holds.

A resume is held to the table, and to nothing on a disk. A role's launcher looks the recorded
session up in the table before it starts the child (`internal/launcher`, `resumable`,
`sessionWritten`) — under `postgres` through the same URL file the child will read, and under
`cloudsql-iam` through its own pgx client over the socket its proxy has just bound and proven,
which is that generation's own (`Endpoint.ClientURL`) — and refuses the start when the table holds
no such session (`resume session <path>: the session table holds no such session`): a launch
failure, never a fresh agent, and three of them retire the claim. A database it cannot reach is
asked again with a backoff (half a second, doubling to four seconds) for up to 30 seconds — under
`postgres` the URL file read afresh each time, since that file is a projection something else may
replace — and only then refuses the start.
Neither `workspace-init` looks for a session file, and neither is told to expect one: there is no
disk on which a session could be waiting. A launch that creates the issue's pod provisions its
workspace from the issue's pushed branch and continues its own session from the table; a role
launched into the pod already running provisions nothing and continues its session in the
workspace the roles before it left. Which launch is which, and what goes with a pod, is stated
once in [The disk dies with its pod](#the-disk-dies-with-its-pod). A child issue re-admitted as a
root of its own keeps its roles' sessions the same way (`Machine.Retree`): they are rows, and no
disk is carried into the new tree.

An agent whose workspace is newer than its session is told so before its next turn. Provisioning
records when it created a workspace, in the workspace's own `.jj/legion-created`, which jj never
snapshots (`workspace.RecordCreated`). At
every resume the role's launcher compares that record with when the session was last written: the
row's `mtime_ms`, which Oh My Pi moves on every write and `legion sessions import`
sets to the copied file's. A workspace created after that write
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

The variables are a generation's, but the store a pod was created under is the pod's: the providers
volume's session item under `postgres`, and the three session-proxy flags the pod's launcher
container carries under `cloudsql-iam` (`sessionStoreInPod`). A pod that holds another
session store than a pod created now would holds a move (`movedSessionStore`,
`internal/runtime/sandbox/addresses.go`) — one created before this release, under a deployment
whose sessions were still files, one created under the other kind or another key, or, under
`cloudsql-iam`, a changed instance, database or login — and the next relaunch of any of its roles
replaces the pod, as it does a pod that dials a moved worker stream. Under `postgres` the image
probe's container is told `OMP_SESSION_SQL_DSN_FILE` too, so `legion probe-image`, which reads the
providers directory as the shim does, never exports the URL into Oh My Pi's environment; under
`cloudsql-iam` the probe's pod mounts no session key and is told no pointer at all
(`sessionDSNPointer`, `manifest.go`).

A resumed session also restores the model it last used, and the Oh My Pi
`.omp-pin` names refuses, in `--mode rpc`, to resume one whose model the profile no longer has
(`Could not restore model <provider>/<model>`, exit 1). So a change to the pods' model
route (`runtime.kubernetes.pod`, `provider_keys`) that drops a model fails the launch of every agent
resuming a session on it until the route offers that model again. Keep the
route unchanged across the copy below in particular: there every session outlives the volume it
was written on.

### What `cloudsql-iam` puts in a pod

No session key, anywhere. No issue pod, no controller pod and no image-probe pod mounts one
(`sessionSecretKey`, `internal/runtime/sandbox/manifest.go`, which answers for `postgres` alone),
the daemon holds no connection string, and no URL sits in a Secret, a volume, an argv, an
environment value or a log. What the pod's `legion launcher` is told instead is where
the database is — `--session-instance <project:region:instance>`, `--session-database <name>` and
`--session-user <login>`, in that order, the three values of `session_database`
(`sessionProxyFlags` over `sessionProxyFlagNames`, `manifest.go`), once for every role it serves.
There is no address among them: the launcher binds each generation's socket in that generation's
own directory, under the role's own private directory, which it already knows from `--roles-dir`
(`--private-dir` in the controller's pod).

Every generation of that role then gets its own connection, in this order
(`manager.start`, `packages/daemon/internal/launcher/launcher.go`):

1. The launcher makes the generation's own directory,
   `/var/run/legion/private/g<generation>` as the role's namespace sees it, on the role's own
   memory-backed private volume, owner-only, and writes the connection URL into it as
   `OMP_SESSION_SQL_DSN`, mode `0600`, created exclusively and without following a link — the way
   it writes that generation's boot token (`writeFiles`, then `startSession`). The URL names the
   socket at the path the role's own namespaces show it, which role-init binds back under
   `/var/run/legion/private`; the launcher, outside those namespaces, binds it at the role's
   directory of the roles directory.
2. It binds a unix socket beside that file, `.s.PGSQL.5432`, which is libpq's own name for a
   Postgres server on port 5432 (`ompsessions.SocketName`), and proves the credential path with
   one upstream connection before its start returns (`sessionproxy.Start`,
   `packages/daemon/internal/sessionproxy`). The socket is bound before anything is dialed, so a
   start that loses the address to something else costs no credential work.
3. Only then, when the start carries a resume, it looks the recorded session up in the table
   through that socket.
4. Only then does it start the worker shim, and so Oh My Pi, whose `OMP_SESSION_SQL_DSN_FILE`
   names the file written in step 1 (`sessionDSNFile`, `manifest.go`).

A failure in any of the first three refuses the generation. The start answers with the error,
which names what failed: clearing the private directory names it, or the entry it could not remove;
making the generation's directory or writing one of its files (a credential, the connection URL)
names the generation and the file; the proxy's start names the generation and the instance; the
resume lookup names the recorded session file. No child is started; the proxy, if it had started,
is closed and the generation's directory removed, so nothing is left bound and nothing is left on
disk (`endSession`). An agent never runs against a proxy that cannot reach the database, because
the proof dial happens before the proxy's start returns. That dial is bounded at half the
launcher's `--stop-grace` (5 s at the default `worker_stop_timeout_seconds` of 10), so an instance
that never answers — an unreachable private address, a handshake that stalls — is refused, as
`no answer within <bound>`, while the daemon still waits for the start's answer: the daemon waits
on a start until the launcher's connection deadline plus that same grace, so at least the whole
grace remains however late the launcher connected (`proofTimeout`, `launcher.go`). The launcher
reads one command at a time, so the bound also keeps it free to read the stop the daemon sends
after a start it gave up on.

The connection belongs to the generation, not to the pod. The proxy keeps serving through the
child's stop grace however the stop arrives — the daemon's stop or the container's own SIGTERM — so
the session writes the agent makes while it shuts down are served: the context the launcher starts
the proxy with bounds only its start, and once started the proxy ends only when it is closed
(`sessionproxy.Start`). It is closed once the child, and in the pod the generation's leftover
processes (`cleanExitedGeneration`), have exited, and before the generation's directory goes, so
the launcher exits with nothing bound, nothing is left listening over a socket that has been
removed, and between generations nothing is bound and no connection file exists (`manager.wait`).
The next generation binds a socket and writes a file of its own.

How long the proxy outlives the moment the generation is told to end, in a pod, where the launcher
is PID 1 of its role's PID namespace (`signalGeneration`, `process.go`):

- **A stop** — the daemon's stop, with its grace, or the container's SIGTERM, with `--stop-grace`
  (`worker_stop_timeout_seconds`, which is also the pod's `terminationGracePeriodSeconds`): at most
  that one grace. The stop's SIGTERM and, at the grace, its SIGKILL reach every process in the
  namespace, the child and whatever it left behind alike, so the leftovers' cleanup ends inside the
  same grace, and no process is left to hold a connection open.
- **A child that exits on its own** and leaves processes behind: the leftovers get up to
  `--stop-grace` more, SIGTERM then SIGKILL (`cleanExitedGeneration`), with the proxy still serving
  them, so at most one grace after the child's exit.

When the proxy is closed, it lets connections still open finish for up to one second
(`sessionDrain`, `launcher.go`; `Config.Drain`, `sessionproxy.go`), then closes both ends of those
left. In a pod that drain costs nothing, since every process that could hold a connection is gone
by then; it bounds the case where the launcher is not PID 1, where signals reach only the child's
process group and a holder outside it could otherwise keep the stop, the next generation and the
launcher's exit waiting.

That URL carries a login and no password, because the credential is the pod's own cloud identity:
the Cloud SQL Go connector puts it in the instance's ephemeral-certificate exchange (automatic IAM
database authentication, `cloudsqlconn.WithIAMAuthN`), never in the Postgres startup packet, and
mints one as it dials, per connection, from a certificate cache it keeps refreshed
(`newConnectorDialer` and `serve`, `internal/sessionproxy`). That is why the proxy reads no
Postgres protocol at all: it accepts a connection, dials the instance over its private IP, and
copies bytes both ways — which is what Google's own `cloud-sql-proxy` does over the same
connector. Google runs that connector as a container beside the application; a Legion pod admits
no further container, since `runtime.kubernetes.pod` takes only env, volumes, mounts and a service
account, so the same library runs inside the pod's own first process, the launcher
itself.

The two readers of that socket spell it differently, so `ompsessions.Endpoint` composes both
(`internal/ompsessions/store.go`):

| reader | spelling | why |
| :--- | :--- | :--- |
| the agent's Oh My Pi, through `OMP_SESSION_SQL_DSN_FILE` | `AgentURL`: `postgres://<login>@localhost/<database>?path=<the socket>&sslmode=disable` | Oh My Pi hands the text to Bun's SQL client, which takes a unix socket as the URL's `path` parameter and ignores the host |
| the launcher's own resume lookup | `ClientURL`: `postgres:///<database>?host=<the socket's directory>&sslmode=disable&user=<login>` | pgx takes libpq's `host` as the socket's *directory* and appends `.s.PGSQL.5432` itself |

An IAM login holds an `@`, so both spellings are built with `net/url`, which percent-encodes it,
and the socket path, wherever it lands. Neither carries a password. The file is the agent's to
read through its pointer and nothing else's to export: it is in the generation's private
directory, not in the providers mount the worker shim exports from.

What the operator supplies is the three values of `session_database`, the pods' cloud identity —
the ServiceAccount `runtime.kubernetes.pod.service_account` names, which is what the connector
signs in as — and the login's rights in the database. Those rights are the ones `postgres` needs
too: Oh My Pi creates and migrates `omp_session_files` and `omp_session_files_parts` in the
`public` schema when it starts, so the login needs `USAGE, CREATE ON SCHEMA public` beside the
database, or it must own the schema. `legion sessions import` is unchanged and keeps `--dsn-file`:
it runs on an operator's machine, where there is no launcher and no proxy, so the operator supplies
a connection URL that machine can reach. `legion sessions mark-lost` is gone with the lost disk it
marked (INFRA-80), and nothing replaces it: a pod's disk dies with the pod, so no session of an
earlier pod is ever waiting to be marked.

### Copying file sessions before turning it on

This runbook, and `scripts/sessions-import-pods.sh`, copy only from the release before issue pods,
`legion-v10.0.0`, whose every role runs in a Sandbox of its own and keeps its session on its tree's
volume; the script refuses any other release by name. A deployment on `legion-v10.1.0` to
`legion-v10.2.x`, which runs one pod per issue, has no copy path: its switch to `postgres` is a
drain at a pushed boundary, every tree closed, and the sessions on those volumes are lost. There
is no way back either, and nothing to go back to: this release has no second store, and nothing
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
its volume and session go with it, so step 5 clears the session its claim still records and the
controller this release launches starts a fresh session.

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
5. **Clear the claims whose sessions are gone.** A claim recording a session the table does not
   hold fails every launch under this release — the role launcher asks the table, refuses the
   start naming the session, and the third such failure retires the claim ([Selecting the
   store](#selecting-the-store)) — where the file store it is leaving would have started it
   fresh. Run this step even when step 4 printed
   no `missing` line: it is where the claims list is checked against the stopped daemon's own
   database, which no other step does. There is no command for it any more: `legion sessions
   mark-lost` went with the lost-disk machinery this release deleted, since nothing carries a
   disk from one pod to the next for a launch to find lost. So read step 4's `missing` lines
   against `claims.json` — each names a claim whose session no volume held: one whose tree's
   volume was already deleted, the cleared controller's, or a `failed` you could not fix — and,
   with the daemon still stopped, clear what each records in the daemon's own database. These are
   plain statements against the `claims` table as `legion-v10.0.0` left it, never this release's
   store, which would migrate the database before the dump the rollback restores:

   ```sql
   -- connected as the daemon is, with its postgres_dsn, the daemon stopped
   select token, session, session_file from claims where project = '<project>' and session_file <> '';
   update claims set session = '', session_file = ''
     where project = '<project>' and token in ('<token>', '<token>', …);
   ```

   Check the select's rows against `claims.json` before the update, as the removed command did:
   a claim recording a session file the list does not give it is one the earlier daemon launched,
   or relaunched onto a new session, after step 2 saved the list, so the list is stale. Go back to
   step 1 then, as step 4 says (scale the earlier daemon up, suspend, save the list again, scale
   it to 0). Update only the claims step 4 reported `missing`. Each of them starts a fresh
   session under this release, a workflow role's in a workspace provisioned from its issue's
   branch. The claims table's `workspace_lost` column is left as it is: this release neither
   writes nor reads it ([Rolling back this release](#rolling-back-this-release)).
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
passed, under every store. The daemon never probes a host Oh My Pi for this: pods run the image's
build, not the host's.

## Kubernetes runtime: the Go daemon on Agent Sandbox

Under the Go coordinator, `runtime: kubernetes` runs every issue as one Agent Sandbox: an
`agents.x-k8s.io` `Sandbox` (kubernetes-sigs/agent-sandbox, LEGION-206) named for the issue,
`legion-<project>-<issue>`, whose one pod runs under gVisor on the Legion pool. The pod holds two
init containers and one container, `roles`, whose PID 1 is one `legion launcher`, a supervisor
with no workflow policy that runs every workflow role — `architect`, `planner`, `implementer`,
`tester`, `reviewer`, `merger`: it holds one connection to the daemon's worker stream per role,
each authenticated with that role's own token, and starts or stops that role's
`legion worker-shim` and Oh My Pi when the daemon tells it to, each role's generation in Linux
namespaces of its own. Every role of the issue
shares the issue's checkout, the pod's own disk it sits on, the temporary directory every role's
agent is told (`TMPDIR=/legion/scratch/tmp`, one directory of the issue's scratch, below) and the
pod's network, and every one of their sessions is a row in the session database rather than a
file any of them holds;
each role keeps its own home, config home, `/tmp`, `/var/tmp`, `/run/lock` and `/dev/shm` mounts,
state directory, agent-secrets key, GitHub App credential and launch credentials. A role's Secret holds
its launcher token, projected under that role's own directory of the container and bound to the
pod's uid, which no role's process sees, and the
two gh files its `gh` and `git` read the role's App token from
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)); the
daemon accepts a launcher connection only for that role, that pod and that token. Each generation's boot token
and launch credentials (the Envoy and Dispatch bearers, a spec's secrets) travel in the authenticated
start command, and the launcher writes them owner-only and exclusively into a fresh `g<generation>`
directory of its role's memory-backed private directory, removing anything already there without
following it, and removes that directory when the generation ends: a role started in a running pod
never waits on the kubelet to refresh a Secret, and no credential is in an argv, an environment
value, a log or an error. The role's own agent sees only its own private directory, inside its
namespaces, so it can read its own generation's credentials, as a pane can; it cannot read another
role's. What the container mounts for the pod rather than for a role is every role's alike: the
operator's providers Secret (`provider_keys`) and, under `runtime.kubernetes.agent_secrets`, the
projected broker token. A role process is addressed by the pod's uid, its role and its generation (the
incarnation `<pod uid>/<generation>`): in a healthy running pod,
starting, suspending or recovering one role never restarts the pod or touches another role. A new generation of a role
in a running pod is a new child of its launcher. A pod is replaced after it dies, when it was not
made by this runtime, or when a closed issue is re-admitted after suspension
(`packages/daemon/internal/runtime/sandbox`).

Releasing a role ends only its process. The issue owns its Sandbox, its role Secrets and its
`-boot` Secret, and nothing else: its disk belongs to its pod, as a generic ephemeral volume the
pod's own template declares, so the Sandbox owns no `volumeClaimTemplates` entry and the disk is
created and deleted with the pod ([The disk dies with its
pod](#the-disk-dies-with-its-pod)). Each Sandbox carries the tree's
`legion.dev/tree` label, which is how the tree's cleanup finds them.
Each tree also has one durable lifecycle record (`tree_lifecycles`, keyed by the normalized project
token and the tree): an epoch that is open, cleanup-reserved, or cleanup-confirmed, and the
authority that opened it. Workflow admission opens a root's epoch in the fact that gives it its
slot; the authenticated operator spawn of a root architect opens an operator tree's. Every claim
binds the open epoch in the same short transaction that first writes it, and every spawn, resume or
retry binds again before it launches, all under the fact path's one global serializer. A reserved
epoch refuses those writes with a named wait that keeps the start's outbox row and charges no
launch failure; a confirmed epoch admits nothing until a fresh root admission opens the next one.
A workflow root whose key names an open operator tree waits for its slot behind that tree's epoch,
and when it leaves the workflow while it waits (a person parks it, or someone moves it to a running
tree's status) its record goes to `done` and nothing more: the operator's claims and pod carry the
root's tokens and names, so the workflow queues no stop, no `issue_suspend` and no linger close for
them (`leaveUnderOperator`, `internal/workflow/linger.go`). `todo` with the label queues it again.

A workflow close or withdrawal queues a separate `issue_suspend` effect for each affected issue,
after its per-role stops. The effect checks both workflow generations, later starts, unfinished
stop effects and every stored role, including a launch not yet present in the runtime's watch.
A held turn, unfinished or uncertain launch, or Kubernetes error keeps the effect pending;
a superseded close finishes without acting. Once the roles have stopped, the daemon sets that
issue's Sandbox to `Suspended` and waits for its pod to disappear, which takes the disk that pod
owns. Then it deletes the Sandbox, whatever the close was: a child set `done`, one set `backlog`,
`icebox` or `triage`, and a root leaving the workflow with its tree are one case now, since a
Sandbox kept behind would hold nothing (`SuspendIssue`,
`internal/runtime/sandbox/issue_suspend.go`), logging `sandbox runtime: deleted the closed issue's
Sandbox and the role Secrets it owned` — the six role Secrets go with it by owner reference. A
`done` child's stops were `issue_close` supervise rows, which retire each of its claims with the
session it recorded dropped, so a later `todo` starts it fresh; every other close keeps the
claims' sessions in the table for a re-admission to resume. The parent's Sandbox is another
issue's and is never touched. A daemon restart retries the
stored effect. Re-admission during linger makes the Sandbox, the pod and its disk afresh and
resumes the recorded sessions;
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
since the listing lists again after a short wait. Nothing is handed over any more: the cleanup
deletes every Sandbox the listing names, a carried child's included, because a child carried into
a tree of its own has nothing left to be handed — the work was on its pod's disk, which went with
the pod — and that child's own tree creates a Sandbox for it at its first launch
(`ensureSandbox`). A Sandbox the new tree's launch relabelled before the cleanup reached it fails
the cleanup's fence and is no longer in the next listing, so it is left to that tree. The orphan
sweep still keeps a Sandbox whose home is another tree (`Store.IssueSandboxHome`: a child whose
hand-over is pending, or one carried into a tree of its own that it is in or can still be
admitted into), which is how a carried child's Sandbox survives a sweep between its launch and
its old tree's cleanup. Agent Sandbox v1.0.3 creates each Sandbox's PVC
with that Sandbox as its controller owner and `blockOwnerDeletion: true`; foreground deletion keeps
the owner visible until Kubernetes garbage collection deletes every blocking dependent — an issue
Sandbox's six role Secrets, and the claim a controller's or a cache's Sandbox owns. The restricted
daemon has no PVC API verb, so a Sandbox's absence after its foreground delete is the API's
confirmation that what it owned is gone; it does not read or delete a PVC. An issue's disk is not
among those dependents: it is its pod's own, and it goes when the delete takes the pod.

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
and its root launch takes over the Sandbox its old tree left, if the cleanup has not reached it
yet: the Sandbox is relabelled for the new tree (`ensureSandbox`,
`internal/runtime/sandbox/relaunch.go`), and whether it is that Sandbox or one made afresh, the
launch brings a new pod with a disk of its own and the roles resume from the session table.
A role still in a
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
that long after, losing only the tool call in flight. Either way the claim keeps the
issue's branch and its session, which the new tree's start resumes with its phase's resume task;
the workspace is provisioned again from that branch in the new pod. A
turn that hangs inside the linger is the operator's to stop sooner: `legion claims suspend` stops it at the
stop timeout (`worker_stop_timeout_seconds`) from then and makes the hold the operator's, so the new
tree's start of that role waits for the operator, with the claim already re-pointed to the new tree,
and `legion claims resume` relaunches its session there. A Sandbox the old tree left `Running`, none
of whose claims runs any more, is relabelled and suspended in one patch fenced to the uid and
resourceVersion it read, so the launch waits its pod out; while a claim of the old tree still runs,
the launch is refused, naming it.
That holds whether the new tree's start or the old tree's cleanup comes first: a cleanup that
reaches the issue first deletes its Sandbox, and the new tree's first launch makes one (above),
while the orphan sweep keeps a Sandbox whose home is the tree it is labelled with. A Sandbox's home
ends when its issue can no longer be admitted into that tree. For a tree admission has not opened,
a child set to `backlog`, `icebox` or `done`, or to a status only a running tree has (`in_progress`,
`testing`, `needs_review`, `retro`), leaves the waiting line and lingers like any root, so its
Sandbox stays until its linger expires; one whose label is taken off leaves the line with no
linger, so its Sandbox goes at the next sweep. Taken out by its label, once it is `todo` with the
label again it waits in the line and is admitted into the phase it stood in, continuing its run
in a pod of its own from the issue's branch: its roles' sessions continue from the session table,
and whatever the pod it had was working on went with that pod's disk ([The disk dies with its
pod](#the-disk-dies-with-its-pod)). Taken out by a status, it comes back as any
root does after its linger: a new generation at `admitted`, its design gate, handoffs and pending
READY cleared (`ClearTreeGeneration`), its architect starting over from the issue's branch. A
hand-over fence counts only while the child's record still names the tree the fence was written
for, and a hand-over Dispatch refuses permanently drops its fence with its row (`finishRefused`,
`internal/daemon/outbox.go`). Past that the Sandbox is an orphan, which the sweep deletes, fenced
and logged, taking its pod and the disk that pod owns. A child re-admitted after its old tree's
cleanup took its Sandbox, one that had left the workflow before the close (a parked child the
close does not hand over), finds no Sandbox and makes one at its next launch; its re-pointed
claim still names its session (`Retree`, `internal/supervise/lifecycle.go`, keeps it: the
supervisor knows nothing of Sandboxes, and a check there would race the cleanup), which the
session table holds and the role resumes. That launch creates the issue's pod, so it provisions
its workspace from the issue's branch, as every new pod does ([The disk dies with its
pod](#the-disk-dies-with-its-pod)), and nothing is reported lost — the same path a root
re-admitted after its own tree's close takes.

Before the daemon opens its store, so before any schema write, image probe or reconcile, it checks
that Agent Sandbox is installed and refuses a namespace that holds a Sandbox of its project whose pod
template is neither of the containers its labels allow — for an issue's (`legion.dev/issue`), the one
`roles` container or, from the layout before contract 39, exactly the six role containers; for the
project controller's (`legion.dev/role=controller` with no tree), the one `controller` container — or
that does not own exactly the volumes a Sandbox of that kind owns. That rule is the kind's, not
one rule for both: an issue's Sandbox owns no volume claim template at all, its pod's disk being
the pod's own, and the project controller's owns the one named `issue`, so a controller Sandbox
owning none and any Sandbox owning a template named something else, `tree` above all, are refused
(`treeVolumeLayout` in `internal/runtime/sandbox/install.go`). The refusal names every such
Sandbox, each with its reason, and how many there are. That catches every per-claim Sandbox of
the layout before issue
pods, running or suspended, the controller Sandbox a `controller: daemon` daemon made before the
controller ran in a launcher pod (`role=controller`, one `worker` container), and the tree-volume
layout's own Sandboxes that own a `tree` template — the tree root's and that layout's controller.
One shape the census accepts rather than refuses: an issue Sandbox still owning the single `issue`
template the release before the pod-owned disk gave it. `spec.volumeClaimTemplates` is immutable
in the CRD, so the only way out of that shape is the delete-and-create of the issue's next launch,
and refusing it would stop the daemon until an operator had deleted one Sandbox per open issue by
hand; so the census logs it by name instead — `sandbox runtime: issue Sandbox <name> owns an
"issue" claim template, the disk shape before the pod-owned disk; its next launch replaces it` —
and the launch replaces it ([Cutting over to the pod-owned
disk](#cutting-over-to-the-pod-owned-disk)). A repository cache's Sandbox
(`legion.dev/cache`) is judged by its own shape alone (`cacheSandboxShape`), never as a pod of a
kind. Once the
store opens and before it migrates, the daemon refuses a claim that still records a Sandbox locator of the
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
- **A Sandbox of the tree-volume layout that owns a volume** — one whose claim template is named
  `tree`, not `issue` (the tree root's own Sandbox, or that layout's controller Sandbox), or a
  controller Sandbox of that layout owning no template at all — is refused by the same census
  (`treeVolumeLayout` in `install.go`), each named with its reason: `Sandbox <name> owns no volume;
  it is of the tree-volume layout before per-issue volumes, so drain and remove it as
  docs/kubernetes.md's "Upgrading a deployment with running trees" says`, or `Sandbox <name> owns a
  volume claim template named "tree", not "issue"; it is of the tree-volume layout …`. Such a
  Sandbox is never adopted: a root's volume holds every clone and workspace of its tree, which
  only this drain may take apart. That layout's *issue* Sandboxes own no template, which is this
  release's own shape, so the census passes over them by their volumes; the root's Sandbox is
  refused all the same, so no daemon of this release boots on that layout, and the drain below
  deletes every Sandbox of a tree, children first.
- **A per-claim locator** on any claim is refused once the store has connected and before it
  migrates (`HasLegacySandboxClaims` in `internal/store/claims.go`).

Nothing is written before either refusal: `store.Open` only connects and pings, and the first
write is `Migrate`, after both checks. So a refused boot leaves the database as the earlier release
left it, and re-pinning the earlier release's image recovers. In the cluster a refused boot is a
crash-looping Deployment: the earlier release's pods keep running, but nothing supervises them.

**Every rollout of this release turns on a store that keeps sessions in a database**, since the
loader accepts no other ([Selecting the store](#selecting-the-store)), so the copy is part of this
upgrade rather than an option beside it: steps 1 to 3 below are replaced by steps 1 to 5 of
[Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on),
which keep every tree's volume until its sessions are in the table. When `controller: daemon` ran,
step 3 below runs before copy step 1: the earlier release resumes a suspended controller within a
minute, so it cannot be held still for the copy, and copy step 5 accounts for its claim. Copy step 6
deletes the per-claim Sandboxes and hands back to steps 4, 5 and 6 below: the two checks, the dump,
and the rollout, whose `legion.yaml` names the store and that store's own key:
`session_dsn_secret` under `postgres`, and `session_database` under `cloudsql-iam`. The copy
itself runs the same way under either, since `legion sessions import` takes the database as a
`--dsn-file` URL the machine running it can reach, which under `cloudsql-iam` is the operator's to
supply: no pod holds one.

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
5. **Dump the database**, `pg_dump -Fc -T omp_session_files -T omp_session_files_parts -f <dump
   file> '<postgres_dsn>'` with the daemon's `postgres_dsn`. It is the only way back ([Rolling
   back](#rolling-back-the-upgrade)). The two `-T` options leave out Oh My Pi's session tables:
   where they share the daemon's database under a login of their own, the daemon's login cannot
   read them and a `pg_dump` that names them exits 1 with `permission denied for table
   omp_session_files`. A rollback restores the dump into a new database, so the session tables in
   this one are left as they stand.
6. **Roll this release out in one step:**
   - the daemon at this release, with its `legion.yaml` at this release's shape:
     `runtime.kubernetes.issue_volume` where the file set `tree_volume`, one `resources.issue`
     entry where it set a `resources.<role>` entry per workflow role, and `resources.controller`
     as `{cpu, memory}` (with `cpu_request` and `memory_request` below them where the requests
     were lower) where it set that entry's `requests` and `limits`, and a store that keeps sessions
     in a database with that store's key, both required — for a deployment today `session_store:
     postgres` with the `session_dsn_secret` key the providers Secret holds the database's URL
     under ([Configuration](#configuration) and [Selecting the store](#selecting-the-store) name
     each refusal); keep a copy of the whole file as it was, since the earlier release refuses
     this release's shape key by key and a rollback puts that copy back ([Rolling
     back](#rolling-back-the-upgrade) names every refused key);
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
That loses every change in its working copies that was not pushed and, where that release kept
sessions as files on the volume (its own `session_store: pvc`, a value this release's loader
refuses),
every session recorded on it. Where it kept them in the table (`postgres`), a session copied there first
([Copying file sessions before turning it on](#copying-file-sessions-before-turning-it-on)) is
kept, and its agent resumes it in a workspace provisioned from its issue's pushed branch. A volume
kept anyway is never mounted again: the earlier release names a tree's claim
`tree-legion-<project>-<root issue>-architect`, from the root architect's own Sandbox
(`SandboxName` and `TreeClaimName` in `names.go` at `legion-v10.0.0`), and an issue-pod build on a
tree volume `tree-legion-<project>-<root issue>`, from the root issue's Sandbox, where this
release gives an issue no claim of its own at all: its pod's disk is the pod's own ephemeral
volume, named `<pod>-issue` and created with the pod ([The disk dies with its
pod](#the-disk-dies-with-its-pod)), and `issue-legion-<project>-<issue>` (`IssueClaimName` in
`internal/runtime/sandbox/names.go`) is now the project controller's claim alone. So no pod of
this release ever mounts a
claim of either earlier layout. Suspending the tree's claims on the earlier release instead
(`legion claims suspend`) keeps their Sandboxes, `Suspended`, and the census refuses a suspended
per-claim or tree-volume Sandbox all the same.

#### Rolling back the upgrade

Roll back by restoring the dump from step 5 and re-pinning the earlier release; work done under
this release since the upgrade is not in the dump. Re-pinning without the restore does not work.
Once this release has booted it has applied migrations 0033 to 0054 (`internal/store/migrations`),
and the earlier release cannot read what they and this release write:

- it refuses an `issue_suspend` outbox row as an unknown kind (`decodeOutboxJSON` in
  `internal/record/outbox.go` at `legion-v10.0.0`);
- it refuses an issue pod's locator, whose Sandbox name carries no role (`checkLocator` in
  `internal/runtime/sandbox/sandbox.go` at `legion-v10.0.0`);
- nothing stops it booting on the newer schema: `Migrate` applies the migrations it has not
  recorded and compares nothing against the version the database is at
  (`internal/store/store.go`, the same at `legion-v10.0.0`), so it would boot and then fail row by
  row.

**Nothing of an issue's disk survives the rollback; push first.** An issue's disk is its pod's own
and goes when the pod ends ([The disk dies with its pod](#the-disk-dies-with-its-pod)), and the
earlier release ends every one of them: the dump was taken after the drain, so every claim in it
is retired, and at boot the earlier release sweeps orphans with a grace of 0
(`reconcileBootOrphans` in `internal/daemon/daemon.go` at `legion-v10.0.0`), deleting every
Sandbox labelled `legion.dev/project=<project>`, but the image probe's, whose name is not that of a
claim it has not retired (`ReconcileOrphans` in `internal/runtime/sandbox/sandbox.go` at
`legion-v10.0.0`). That is every Sandbox this release made: each issue's
`legion-<project>-<issue key lowercased>`, the controller's and each repository cache's, each pod
with it and each issue pod's disk with that pod. No step keeps a disk instead: deleting a Sandbox
with `--cascade=orphan`, which kept a Sandbox-owned volume before the pod-owned disk, keeps nothing
an issue now has, since the disk belongs to the pod and not to the Sandbox; and the earlier release
could not use one anyway, since it never mounts a claim of this release's (below). This release
does the same if it boots once on the restored database, which holds no issue record and only
retired claims: its boot sweep deletes every issue Sandbox with a grace of 0 too. Do these steps in
this order:

1. **Have every running issue's work pushed, and confirm it, while this release still runs.** List
   the issue pods (`kubectl -n <namespace> get pods -l 'legion.dev/project=<project>,legion.dev/issue'
   -L legion.dev/issue`), ask the role with work in hand on each, through Dispatch, to push it to
   `legion/<KEY>`, and confirm each push as the cutover does ([Cutting over to the pod-owned
   disk](#cutting-over-to-the-pod-owned-disk), step 1). The pushed branch is the only copy of an
   issue's work the rollback keeps.
2. **Stop this release's daemon and keep it stopped:**

   ```sh
   kubectl -n <daemon namespace> scale deployment <daemon Deployment> --replicas=0
   kubectl -n <daemon namespace> wait --for=delete pod -l <daemon pod labels> --timeout=5m
   ```

   (`legion stop` for a daemon outside a Deployment). It stays at zero replicas until step 5
   re-pins it: a crash restart, or a replica left running, after the restore boots this release on
   the restored database, and that one boot sweeps every issue Sandbox with a grace of 0. Stopping
   the daemon ends no pod: an issue pod keeps running, unsupervised, until the earlier release's
   boot deletes its Sandbox in step 5. That window is the last in which anything on an issue's disk
   can be read. For an issue whose push step 1 could not confirm, open a shell in its still-running
   pod ([A shell on an issue's disk](#a-shell-on-an-issues-disk)) and push what the workspace at
   `/legion/workspaces/<owner>/<repo>/<issue key lowercased>` holds that its branch on GitHub does
   not, with a credential of your own: the `roles` container's shell holds no role's.
3. **Restore the dump into a new database the daemon's login owns**, and point the earlier
   release's `postgres_dsn` at it. Create the database as a login that may create one (an admin),
   owned by the daemon's login, then restore as the daemon's login:

   ```sh
   createdb <admin connection options> -O <daemon login> <new database>
   pg_restore <daemon login's connection options> --exit-on-error --no-owner -d <new database> <dump file>
   ```

   Both halves matter where the daemon's login is not a superuser. A database an admin owns
   refuses the daemon's login a restore (`pg_restore` exits 1, `permission denied for schema
   public`), and a restore an admin runs leaves every table the admin's, so the earlier release's
   `Migrate` fails on its first read (`permission denied for table schema_version`) before it
   writes anything. The admin must be a superuser or a member of the daemon's login: PostgreSQL 16
   refuses `createdb -O` otherwise (`must be able to SET ROLE "<daemon login>"`), which is the
   case for a managed instance's admin login (Cloud SQL's `postgres`, RDS's master user). There,
   run `GRANT <daemon login> TO <admin login>;` first, as a role that holds ADMIN OPTION on the
   daemon's login: the role that created it, or a superuser. Any other role is refused (`permission
   denied to grant role "<daemon login>"`, with the detail `Only roles with the ADMIN option on
   role "<daemon login>" may grant this role.`), even an admin with `CREATEROLE`, where the daemon's
   login was created by another role. Every one of these refusals comes before any write.

   The database this release ran on is left untouched, and with it the shared session tables
   under a login of their own. Restoring into that database does not work:
   `pg_restore --clean --if-exists` exits 1 there, since this release's foreign keys onto `claims`
   and `issues` keep it from dropping either table (`cannot drop table public.claims because other
   objects depend on it`) and the restore then meets the tables still there (`relation "claims"
   already exists`), leaving `schema_version` at 32 with this release's tables and claims in place.
   Run 2026-10-26 07:31Z on postgres:16.15 (Docker), at head `5511b8ad7`: a database at
   `legion-v10.0.0`'s migrations 0001 to 0032 with claims, dumped as step 5 says, then taken
   through every migration of that head, the one-container migration then numbered 0051;
   `pg_restore --clean --if-exists` into it exited 1 as quoted, and
   a restore into a new database exited 0 with `schema_version` at 32, both claims, and no session
   table, while the old database's session row stayed. Run again 2026-10-26 08:18Z on
   postgres:16.15 (Docker) with a daemon login that is not a superuser, which ran the migrations,
   the dump and the restore: `createdb -O` and `pg_restore` as above exited 0, and the daemon's
   login read `schema_version` 32 and its claim from the new database; the admin-owned database
   and the admin's restore failed as quoted. The `GRANT` was run on postgres:16.15 the same day:
   by an admin with `CREATEROLE` that is not a superuser, on a daemon login a superuser created, it
   failed as quoted; by an admin that created the daemon login itself, it and the `createdb -O`
   both exited 0.
4. **Put `legion.yaml` back**, the copy of the whole file step 6 said to keep, and run the earlier
   release's `legion start --check-config` on it. The earlier release reads the file before it
   opens the database or the cluster, and refuses this release's shape key by key, so a daemon
   re-pinned on the file as this release left it never starts:
   - the top-level keys this release added, each `unknown key <key>` (`classify` in
     `internal/config/config.go` at `legion-v10.0.0`): `capabilities`, `production_check_hours`
     and `worker_backends`;
   - under `projects.<KEY>`, the `repo` the earlier release requires (`projects.<KEY>.repo must
     be "owner/name" (got "undefined")`, `readProject` in `internal/config/stage3.go` at that tag)
     and every key this release added there, `deploys`, `merge` and `slack_channels`, each
     `Unknown key "projects.<KEY>.<key>"`;
   - `tree_volume` where this release's file sets `issue_volume` (`unknown key
     runtime.kubernetes.issue_volume`, `members` in `internal/config/config.go` at that tag), one
     `resources.<role>` entry per workflow role where it sets `resources.issue`
     (`runtime.kubernetes.resources key "issue" must be a role (architect, planner, implementer,
     tester, reviewer, merger, controller)`, `readResources` in `internal/config/kubernetes.go` at
     that tag), and `resources.controller` as that entry's `requests` and `limits` where it sets
     `cpu`, `memory`, `cpu_request` and `memory_request` (`unknown key
     runtime.kubernetes.resources.controller.cpu`, the same `members`);
   - every other key of the `runtime.kubernetes` block this release added, each refused by name
     the same way: `session_database`, `plugins`, `repository_cache`, `setup`,
     `agent_secrets.token_file`, and a pod volume of kind `persistent_volume_claim` or `nfs`;
   - a database session store: the file puts `session_store` back to `pvc`, or leaves it unset,
     and removes `session_dsn_secret` with it. The earlier release refuses `postgres`
     (`runtime.kubernetes.session_store postgres is not supported until Stage 6: a pod's session
     lives on the tree volume (pvc)`), refuses `cloudsql-iam` as `runtime.kubernetes.session_store
     must be 'pvc' or 'postgres'`, and refuses the secret beside `pvc`
     (`runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is
     pvc; remove it`), all three in `checkSessionStore` of `internal/config/kubernetes.go`; it
     reads no session of that table in any case, since the claims the dump restores are the
     drained ones (below).

   No data is lost to any of these refusals — each comes before the daemon's first write and
   names its key — but the daemon stays down until the file is back.
5. **Re-pin the earlier release** and scale its Deployment back to one replica.

What the rollback loses, then: every issue's disk, with whatever of its workspace was not pushed
(step 1 and step 2's window are the only ways to keep it); the record of every tree this release
ran (the restored claims are the drained ones, so the earlier release resumes no role and knows no
issue of those trees); every session this release's agents had, which stay as rows in the session
tables of the database this release ran on, untouched, where the earlier release, which reads
sessions only as files on its tree volumes, never looks; and the controller's session. Dispatch
keeps the statuses this release wrote, and each issue's branch on GitHub keeps what was pushed.
Hand each tree back to the earlier release as new work, its root `todo` with the `legion` label,
once its branch holds what you kept.

The earlier release never mounts a claim of this release's in any case: it names its claims
`tree-legion-<project>-<root issue>-architect` (`TreeClaimName` in `names.go` at
`legion-v10.0.0`), where this release gives an issue pod a disk of its own named `<pod>-issue`, and
the controller's Sandbox and the repository caches' are left to the earlier release's sweep, which
deletes them with their volumes: the controller's working root and the caches' clones, which hold
no issue's work.

### Cutting over to the pod-owned disk

This section is for a deployment already running issue pods of the release before this one
(`legion-v10.1.0` to `legion-v10.2.x`), where each issue's Sandbox owned an `issue` volume claim
that outlived its pods. Nothing is migrated in place: `spec.volumeClaimTemplates` is immutable in
the CRD, so an issue's Sandbox is deleted and made afresh, and the claim it owned is reaped with
it ([The disk dies with its pod](#the-disk-dies-with-its-pod)).

**The database and its secret come first, and are not part of the cutover.** This release's
loader refuses a `runtime.kubernetes` block that does not name a store that keeps sessions in a
database, with that store's key — for a deployment today `session_store: postgres` and
`session_dsn_secret` — so the deployment's `legion.yaml` names both, and the providers Secret
holds that key, before this release boots at all ([Selecting the store](#selecting-the-store)). A
deployment still on the file store copies its sessions into the table first — but only one on
`legion-v10.0.0` can: a deployment on `legion-v10.1.0` to `legion-v10.2.x` has **no copy path at
all**, and its switch is a drain at a pushed boundary with every tree closed and the sessions on
those volumes lost ([Copying file sessions before turning it
on](#copying-file-sessions-before-turning-it-on)). A sessions table behind is a deployment every
issue's next launch fails on, not something the cutover repairs.

**Every running issue is pushed and stopped while the earlier daemon still runs, because the first
boot of this release cuts over every live claim by itself.** This release raises the daemon's
plugin contract to 37, and every claim of a deployment on the release before it recorded 36. The
boot posts `StalePlugin` to each restored claim whose process has registered and whose recorded
contract is not the daemon's (`refreshStalePlugins`, `internal/daemon/daemon.go`), and the
supervisor answers it by replacing that process, mid-turn if a turn is running (`refreshPlugin`,
`internal/supervise/plugin.go`). That relaunch finds the issue's Sandbox of the previous disk shape
and replaces it (`replaceSupersededSandbox`, `internal/runtime/sandbox/relaunch.go`), deleting it
together with the volume that holds the issue's whole workspace. A role keeps its process until
its issue leaves the workflow, idle between phases or not, so every issue still in the workflow
when this release boots is cut over by that boot, all at once, and whatever it had not pushed is
gone before the operator can read a line of the log. The last moment an agent can be asked to push
is while the earlier daemon still supervises it, and the steps below are ordered to take it.

The order is the runbook's to keep, not a hold in the daemon. Holding the migrating relaunch until
an operator chose to start the issue would give the daemon a new kind of control over work, and
this project keeps the daemon to moving work through its phases and keeping its pods running,
adding to that only when the person who owns the daemon says so. A hold would not save anything
this change has not already given up either: its approved specification is that work nobody pushed
dies with its pod, and the cutover's loss is that rule applied once, to every issue at the same
moment. Nor does the order ask the operator for anything new: the earlier daemon has to be stopped
before the image is applied regardless (step 4), and the push and the stop the runbook already
asked for simply come before it.

The cutover, in this order:

1. **Have every running issue's work pushed, and confirm it.** Under the earlier daemon, still
   running, list the issues it has pods for: `kubectl -n legion get pods -l
   'legion.dev/project=<project>,legion.dev/issue' -L legion.dev/issue`. Each one holds a live
   claim, so each is one the boot would cut over. Ask the role that has work in hand on each,
   through Dispatch, to push it to `legion/<KEY>`, letting a role mid-turn finish first; a role
   between phases pushed when it completed. Then confirm rather than trust the answer: take the
   commit id the role reports, and in any clone of the repository `git fetch
   https://github.com/<owner>/<repo> legion/<KEY> && git merge-base --is-ancestor <commit>
   FETCH_HEAD` exits 0 only when the branch carries it. The pushed branch is the one copy of the
   issue's work that survives the cutover.
2. **Stop those issues.** A person moves each out of the workflow (`backlog`, from the Dispatch
   dashboard or with `legion status <KEY> backlog --operator-token-file <file>`); for a root that
   takes its tree out, whose cleanup deletes every issue Sandbox of the tree, volumes and all,
   which is why its children's pushes are confirmed first. The earlier daemon ends the issue's
   processes and leaves its Sandbox `Suspended`, owning its volume, with no pod. A claim with no
   running process is not one the boot replaces, so the issue waits, disk intact, for the launch
   the operator chooses in step 7.
3. **Leave none running.** Step 1's listing prints no pod before the next step, unless it is the
   pod of an issue whose loss you have chosen to accept. An issue nobody stops still cuts over, and
   when depends on its claims. One whose claims are suspended, stopped in step 2 or before the
   upgrade, cuts over at its next launch, whenever that launch comes, since any launch replaces a
   Sandbox of the previous shape. One whose claims are live has no such grace: its launch is the
   boot itself, which replaces its Sandbox and deletes the volume, taking whatever was unpushed,
   before step 6's census can be read. The census logs before anything is relaunched, so it still
   names that Sandbox, as work to do, when the Sandbox and its disk are already gone; an operator
   who waits for that list to learn what is at risk learns it from a census of disks that no
   longer exist. Steps 1 and 2 are how the loss is chosen rather than met.
4. **Stop the earlier daemon.** Scale its Deployment to 0 and wait for its pod to go, as the
   session copy does ([Copying file sessions before turning it
   on](#copying-file-sessions-before-turning-it-on), step 2), rather than rolling one image onto
   the other: nothing makes a daemon a singleton, and a daemon that is restarting or refusing its
   configuration leaves the earlier release's issue pods running with nobody supervising them
   ([Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees)).
   Migration 0051 strips the `release` key from every `issue_suspend` row still queued, because
   this release decodes that payload strictly and no longer has the field; an earlier daemon
   still running after the strip, closing a child as done during the overlap, queues the key
   again, and the new daemon's decode then stalls that issue's lifecycle lane on a row no retry
   can finish — the very failure the migration exists to prevent.
5. **Apply this release.** Pin the daemon's image and `runtime.kubernetes.image` to this release
   through the deploy path, with the `legion.yaml` that names the database and its secret (above).
6. **Read the boot log for the issues still to cut over.** The census accepts an issue Sandbox
   that still owns the `issue` template and logs one line for each, `sandbox runtime: the issue
   Sandbox owns a claim template, the disk shape before the pod-owned disk; its next launch
   replaces it`, which names the Sandbox in the line's `sandbox` attribute, not in its text (the
   template's name is its `claim_template`), so grep the message and read the names off the
   attribute. With every issue stopped first, each name is a `Suspended` Sandbox whose volume
   still exists, and the list is the work.
7. **Start each stopped issue again.** A person moves it back into the workflow (`todo`, the same
   two ways). Its launch deletes the Sandbox of the previous shape and the volume it owns, creates
   a Sandbox of the new shape, and its pod brings a disk of its own, provisioned from the pushed
   branch step 1 confirmed.
8. **Delete the old-shape Sandboxes no issue will start again.** A `Suspended` Sandbox whose issue
   nobody launches keeps its volume indefinitely, so the operator deletes each by name once its
   issue is one no person intends to resume: `kubectl -n legion delete sandbox <name>`, which
   takes the volume it owns with it. Step 6's list names them.
9. **Audit the namespace.** The cutover is done when every claim in the namespace is a pod's own
   or one of the three exceptions. No label selector can answer that: an issue's disk carries no
   label at all, and the only claims Legion labels are exceptions, so read every claim and judge it
   by its owner:

   ```sh
   kubectl -n legion get pvc -o json | jq -r --arg controller 'issue-legion-<project>-controller' --arg state '<the daemon state claim>' '
     .items[]
     | select(.metadata.name | startswith("cache-legion-") | not)
     | select(.metadata.name != $controller and .metadata.name != $state)
     | [.metadata.name,
        ((.metadata.ownerReferences // [] | map(select(.controller == true)) | first | .kind + "/" + .name) // "none")]
     | @tsv' |
     awk -F'\t' '$2 !~ /^Pod\// { print }'
   ```

   It prints nothing when the cutover is complete. Every line it does print is a claim no pod
   owns, which on this release is either an issue's volume of the previous shape (step 8) or
   something that is not Legion's at all. The three names it passes over are the exceptions, each
   owned by a Sandbox or by nothing and each right as it is: every repository cache's claim,
   `cache-legion-<project>-cache-<owner>-<repo>-<hash>` ([The repository
   cache](#the-repository-cache)); the project controller's,
   `issue-legion-<project>-controller` ([Daemon-launched
   controller](#daemon-launched-controller)), which is there only under `controller: daemon`; and
   the claim the daemon's own Deployment mounts for its `state_dir`, whose name is the
   deployment's own. Name no other exception: a claim that answers to none of them and has no pod
   for an owner is the finding this audit exists for.

#### Rolling back this release

Rolling the image back is not a matter of re-pinning it. The previous release's boot census
refuses an issue Sandbox that owns no volume claim template — the shape this release makes for
every issue — reading it as the tree-volume layout, so its daemon crash-loops until the namespace
holds none. **Every issue Sandbox of the project is deleted before the earlier image is pinned**
(`kubectl -n legion delete sandbox <name>` for each, or a tree at a time by its
`legion.dev/tree` label), which takes each pod and each pod's disk, so every issue's unpushed
work is gone and the earlier release provisions its volumes afresh at the next launch. Push
first, as the cutover does.

The database needs no restore for this step. This release's own migration, 0051, strips the
`release` key from queued `issue_suspend` rows and changes no column, and the claims table keeps
`workspace_lost` — this release writes and reads it nowhere (`claimSelect`,
`internal/store/claims.go`), and it stays rather than being dropped exactly so the earlier
release's daemon still finds the claims table it knows. Migrations here are forward-only, so a
dropped column would have made this rollback a restore.

From a release that also carries the one `roles` container (daemon API contract 39, [Moving from
one container per role to one `roles` container](#pinning-a-release-that-drops-what-a-running-pod-still-uses)),
run that rollback's steps 1, 2, 3 and 5 first — push, stop the daemon, the locators and their
`schema_version` row, and `legion.yaml`'s `resources` block — since the release before this one
refuses a `roles` locator and an `issue` resources entry as this one does; its step 4 is not
needed, since every issue Sandbox is deleted here.

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

**Moving from one container per role to one `roles` container (daemon API contract 39).** A
release at contract 39 builds an issue pod with one container, `roles`, whose launcher runs every
role in multi-role mode, which only an image of that release's `legion` has: pin the worker image
with the daemon release, since the image probe refuses an image whose `pi-legion` declares another
contract. The release before it is contract 38, the blocked issues (INFRA-225), whose issue pods
run the six role containers of contract 37, the pod-owned disk ([Cutting over to the pod-owned
disk](#cutting-over-to-the-pod-owned-disk)), and this section is written against it: a deployment
still on a release before contract 37 cuts over to the pod-owned disk first. A rollback rewrites
the database in place and needs no dump (below); a safety copy taken
with the daemon stopped does no harm, as `pg_dump -Fc -T omp_session_files -T
omp_session_files_parts -f <dump file> '<postgres_dsn>'` with the daemon's `postgres_dsn`: where
Oh My Pi's two session tables share that database under a login of their own, the daemon's login
cannot read them and a `pg_dump` that does not leave them out exits 1 with `permission denied for
table omp_session_files`. At boot, store
migration 0054 rewrites every stored claim locator naming a per-role container to `roles`, and the
boot census passes a pod of either layout and refuses everything else as before. A running issue
pod whose own containers are exactly the six role names (a container another controller injected
into the running pod, a mesh's sidecar say, is not counted) is reported moved for every role
recorded in it, with the detail `the pod's containers architect, planner, implementer, tester,
reviewer, merger, now roles`, and the supervisor relaunches each uncharged. The first relaunch
replaces the pod — even an installing (stale-plugin) launch — on the configured image, and its
siblings resume into the new pod; a turn a role was in is sent again once its session resumes, as a
moved address's relaunch does. The replaced pod takes its disk with it, so the new pod provisions
its workspace from the issue's pushed branch ([The disk dies with its
pod](#the-disk-dies-with-its-pod)): before the rollout, have each running issue's work pushed, stop
those issues and stop the earlier daemon, as the cutover's steps 1, 2 and 4 say ([Cutting over to
the pod-owned disk](#cutting-over-to-the-pod-owned-disk)), and start them again as its step 7 does,
since the boot replaces every issue pod at once and work done after a push is lost with its pod.
`legion.yaml`'s `resources` takes one `issue` entry in place of the per-role entries
([Configuration](#configuration)); keep a copy of the block as it was, which a rollback puts back.
`runtime.kubernetes.pod` loses names it could use before, each refused at load
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)): the volume names `role-homes` and
`role-probe`; a mount at, under or above `/var/run/legion/roles`, `/var/run/legion/home`,
`/var/run/legion/role-probe`, `/dev/mqueue`, `/dev/ptmx`, `/dev/pts` or `/proc`; and a mount at
or above `/tmp`, `/var/tmp`, `/run/lock`, `/var/lock` or `/dev/shm`, or the image's other
spellings of them. Run
this release's `legion start --check-config` on the file before the rollout.

To roll this release back to contract 38, every stored locator and every issue Sandbox both go back
to what the earlier release reads. Re-pinning alone boots neither:

- Migration 0054 has no down migration, and the earlier release cannot read what it wrote: it
  refuses an issue pod's locator naming the container `roles` (`sandbox container "roles" does not
  match claim role`, `Locator.Validate` in `internal/runtime/locator.go` at contract 38), which
  it reads as it loads its claims. Nothing refuses the newer schema first: its `Migrate` applies
  each migration it has not recorded and compares nothing against the version the database is at
  (`internal/store/store.go`).
- Before it opens the database at all, its boot census refuses every issue Sandbox whose template
  is not exactly the six role containers (`legacyIssueSandbox` in
  `internal/runtime/sandbox/install.go` at contract 38): `legacy issue Sandbox <name> runs
  containers [roles], not exactly the launchers its labels name [architect implementer merger
  planner reviewer tester]`. Every Sandbox this release launched a pod into has the one-container
  template, Suspended or not.

**Every issue pod ends in this rollback, and its disk with it; push first.** An issue's disk is a
generic ephemeral volume its pod owns, not its Sandbox ([The disk dies with its
pod](#the-disk-dies-with-its-pod)), so step 4 below, which suspends each issue Sandbox to rewrite
its template, ends each pod and deletes each disk, and whatever a workspace held that was not
pushed is gone. Nothing else is lost: every session is a row in the session database, which the
rollback does not touch and both releases read, and the earlier release's next launch of each
issue provisions its workspace from the issue's pushed branch, as every new pod does.

Do not restore a dump taken before the upgrade. The restored record would be behind Dispatch and
behind the Sandboxes: a root admitted under this release is restored waiting, the earlier release
reads Dispatch's newer status at boot and releases it as unslotted, and once its linger ends the
orphan sweep deletes its Sandbox; a pending hand-over the dump holds is dropped or cancelled, and
the source tree's cleanup or the sweep deletes that Sandbox too, each with the record of the work
it carried. Deleting each issue Sandbox instead of rewriting it would also pass the earlier
release's census, now that no volume hangs on a Sandbox, and loses no more than the rewrite does:
the disks, which go with the pods either way. It drops the `legion.dev/displaced-pod` record a
Sandbox carries, though, so a claim this release displaced would be read as dead and charged.
Reverse 0054 in the live database and rewrite each Sandbox's template in place, which keeps the
record, every Sandbox, its uid, its annotations and its tree label. In this order:

1. **Have every running issue's work pushed, and confirm it**, while this release still runs, as
   the cutover's step 1 does ([Cutting over to the pod-owned
   disk](#cutting-over-to-the-pod-owned-disk)). The pushed branch is the only copy of an issue's
   workspace the rollback keeps.
2. **Stop this release's daemon** (scale its Deployment to zero, or `legion stop`), so nothing
   launches into a Sandbox or writes a locator while both are rewritten.
3. **Give every issue pod's locator back its role's container.** Before 0054 an issue pod's role
   ran in a container named for the claim's role, so the `role` column says what each locator named;
   the controller's locators still name `controller` and are not touched, and a tmux locator, or one
   with no `sandbox` member, matches neither statement:

   ```sh
   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
   begin;
   update claims set locator = jsonb_set(locator, '{sandbox,container}', to_jsonb(role))
   where locator->>'runtime' = 'sandbox'
     and locator->'sandbox'->>'container' = 'roles'
     and role <> 'controller';
   update claim_capabilities cc set locator = jsonb_set(cc.locator, '{sandbox,container}', to_jsonb(c.role))
   from claims c
   where c.token = cc.claim_token
     and cc.locator->>'runtime' = 'sandbox'
     and cc.locator->'sandbox'->>'container' = 'roles'
     and c.role <> 'controller';
   delete from schema_version where version = 54;
   commit;
   SQL
   ```

   It prints `UPDATE <n>` twice and `DELETE 1`; a rerun prints `UPDATE 0` twice and `DELETE 0`. A
   claim this release launched first gets its role's container the same way, so the earlier
   release loads it too. The `delete` removes the row `schema_version` records for 0054, so a later
   upgrade back to this release applies 0054 again and rewrites every claim the earlier release
   launched in the meantime; with the row left, that upgrade would skip 0054 (its `Migrate` applies
   only the migrations `schema_version` does not record, `versionRecorded` in
   `internal/store/store.go`) and look for each role's own container in a pod that has only
   `roles`. The earlier release's `Migrate` checks each migration it has by its own number and reads
   no highest row; only its `legion state` reports one, the `schemaVersion` it reads as
   `max(version)` (`schemaVersion` in `internal/store/store.go`, the state read in
   `internal/daemon/daemon.go`), which reads 53 once the row is gone, the earlier release's own
   last migration (`0053_issue_taken_back.up.sql`, which this rollback leaves applied with
   `0052_dispatch_issues.up.sql`). The
   SQL changes only these locators and that row, so Oh My Pi's session tables, wherever they are,
   keep every turn taken under this release. The `54` in the `delete` is the number of the
   migration that renames the container, `0054_issue_pod_roles_container.up.sql`; where that file
   carries another number in the release you roll back from, use that number.
4. **Return every one-container issue Sandbox to the earlier layout's template.** With
   `<namespace>` and `<project>` as in [Upgrading a deployment with running
   trees](#upgrading-a-deployment-with-running-trees) step 4, each patch suspends the Sandbox, which
   ends its pod and with it the pod's disk, and replaces its containers with the six role names
   alone; its fence (`test`) makes a rerun refuse a Sandbox it already rewrote:

   ```sh
   kubectl -n <namespace> get sandboxes -l 'legion.dev/project=<project>,legion.dev/issue' \
     -o jsonpath='{range .items[?(@.spec.podTemplate.spec.containers[0].name=="roles")]}{.metadata.name}{"\n"}{end}' |
   while read -r name; do
     kubectl -n <namespace> patch sandbox "$name" --type json -p '[
       {"op": "test", "path": "/spec/podTemplate/spec/containers/0/name", "value": "roles"},
       {"op": "replace", "path": "/spec/operatingMode", "value": "Suspended"},
       {"op": "replace", "path": "/spec/podTemplate/spec/containers", "value": [
         {"name": "architect"}, {"name": "planner"}, {"name": "implementer"},
         {"name": "tester"}, {"name": "reviewer"}, {"name": "merger"}]}]'
     kubectl -n <namespace> wait --for=delete "pod/$name" --timeout=5m
   done
   ```

   Each patch prints `sandbox.agents.x-k8s.io/<name> patched`. Its wait prints `pod/<name>
   condition met` when the pod was still there, and nothing when it was already gone (a Sandbox
   that was Suspended before the patch); it exits 0 either way. The template is only what the
   census reads: the earlier release's next launch into the Sandbox writes its whole pod template
   before it sets it running (`relaunch` in `internal/runtime/sandbox/relaunch.go`), as for any
   Suspended Sandbox. The patch leaves `volumeClaimTemplates` as it is, and an issue Sandbox of
   either release owns none, which is the shape the earlier release's census passes
   (`treeVolumeLayout` in `install.go` at contract 38). The controller's Sandbox (one `controller`
   container in both releases), a repository cache's and the image probe's carry no
   `legion.dev/issue` label and are left as they are. The same `get sandboxes` query, without the
   loop, then prints nothing.
5. **Put `legion.yaml`'s `runtime.kubernetes.resources` block back as it was before the upgrade**
   (the copy kept then: one entry per role, or no block when the file had none). The earlier
   release refuses this release's `issue` entry as it reads the file:
   `runtime.kubernetes.resources key "issue" must be a role (architect, planner, implementer,
   tester, reviewer, merger, controller)` (`readResources` in `internal/config/kubernetes.go`).
   Leave `session_store` and its key as they are: both releases require a store that keeps
   sessions in a database. Run the earlier release's `legion start --check-config` on the file
   before re-pinning it.
6. **Re-pin the earlier daemon and its worker image together**, and start the daemon.

The earlier release then boots: its census passes, and it loads every claim as this release left
it. Each claim recorded as running names a pod that is gone, so the daemon logs `supervise:
process died` with `no pod of sandbox <name> (operatingMode Suspended; Suspended=True
PodTerminated)` (or `the process ended with a pod the daemon replaced; relaunching it uncharged`,
for a claim whose old pod this release replaced, which the Sandbox's `legion.dev/displaced-pod`
still names) and resumes the claim's recorded session, from the session database, in a new pod of
six containers on a disk of that pod's own, its workspace provisioned from the issue's pushed
branch. A Gone charges one launch failure, which the agent's becoming ready clears (`ready` in
`internal/supervise/table.go`), and a death only when the claim had a task outstanding.

The rollback rewinds nothing but the unpushed work of every issue's disk: the workflow's progress
since the upgrade stays in the record, and every session stays where it is, in the session
tables, which neither step 3 nor step 4 touches. The record, the Sandboxes' tree labels and
Dispatch stay in step, so the earlier release's sweep and tree cleanup read the same trees this
release left.

Step 4 was run against Agent Sandbox v1.0.3 on a scratch cluster, with the earlier release's census
itself as the check: before step 4 it refused the one-container Sandbox as above; after it, it
passed, and the Sandbox kept its uid. That run predates the pod-owned disk: its Sandbox still owned
a volume, which it kept; from contract 37 the pod's disk goes with the pod at the patch, as above.
Step 3's SQL was run verbatim on 2026-10-26 12:48Z against PostgreSQL 16.2 holding contract 38's
migrations 0001 to 0053 (main at `bc307fb56`), with sandbox locators for every
workflow role of an issue pod, a controller's sandbox locator, a tmux locator, a null locator, one
with no `container` and one with a null `sandbox`, in both tables: after this release's 0054 and
step 3, both tables equalled a snapshot taken before 0054, row for row, with `schema_version` back
at 53, and a second run changed nothing. Applying 0054 again afterwards, as the next upgrade's
`Migrate` does, rewrote the same six claims and six capabilities to `roles`. The safety copy's
`pg_dump`, run as a login that cannot read the session tables, exited 1 without the two `-T`
options and 0 with them.

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
replacing it: the role's shim runs the start wrapper,
`/bin/sh -c '<start script>' legion-start <n> <setup command…> <omp argv…>` (`startArgv`,
`internal/runtime/sandbox/plugins.go`; `<n>` is the setup command's word count, 0 with none),
whose install step runs
`bun add --backend=copyfile --exact @sjawhar/pi-legion@<version> @sjawhar/pi-envoy@<version>` (a
tarball path or URL source is handed to `bun add` as written; `--backend=copyfile` because the
issue's bun install cache and the generation directory now share the issue's volume, where bun would
otherwise hardlink the installed files out of that cache and an in-place edit of either would
corrupt the other for the life of the volume) into a fresh generation directory,
`$XDG_STATE_HOME/legion-plugins/g<generation>/` (`/legion/scratch/state/<role>/legion-plugins/g<N>/`),
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
    issue_volume: 50Gi          # default 50Gi; one volume per issue, and one for the controller's pod
    kubeconfig: /home/ubuntu/.kube/legion-daemon-production   # relative to legion.yaml's directory
    context: legion-daemon@example   # required when the kubeconfig sets no current context
    scheduling:                 # optional, beyond the Legion pool the runtime always selects
      node_selector: {}         # merged over legion.dev/pool=legion, which it may not name
      tolerations: []
      priority_class: legion
    resources:                  # optional; two keys, each a pod's reservation: cpu and memory limits, each requested
                                # in full unless cpu_request or memory_request sets less (Issue sizing: Guaranteed
                                # or Burstable), so every pod is covered for the resource-limits capability with or
                                # without this block, and an ephemeral-storage limit over a smaller request (the
                                # pod's bound on the node's disk)
      issue: { memory: 32Gi, memory_request: 19Gi }   # every issue pod: Burstable, reserves 19Gi, may use 32Gi
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
    setup: [/bin/sh, -c, '<command>']   # optional: run in every role's namespace before each start of its Oh My Pi (below)
bind: <the daemon host's own address, or 0.0.0.0 once advertise_host names one>
advertise_host: <optional: a stable Service name, e.g. legion-daemon-<project>.<namespace>.svc, every pod dials instead of bind, at worker_stream_port>
worker_stream_port: <the port advertise_host or bind serves it on>
daemon_url: http://<the address pods reach the daemon at>:<the daemon's own port>
capabilities:                   # optional: the deployment capabilities decided by name, with the reason (The deployment's capability report, below); a gap is reported, never refused
  decided: { secrets: "<reason>", model-fallback: "<reason>", resource-limits: "<reason>", pool-capacity: "<reason>" }
```

Every container of every Legion pod carries a cpu and memory limit and a request no greater than it,
and an ephemeral-storage limit, its bound on the node's disk, over a smaller ephemeral-storage
request. `runtime.kubernetes.resources` takes exactly two keys: `issue`, the one reservation every
issue pod's `roles` container and both its init containers carry, and `controller`, the
daemon-launched controller's pod's, only under `controller: daemon`. Each takes the six fields
`cpu` and `memory` (the limits), `cpu_request` and `memory_request`, `ephemeral_storage` (the
limit) and `ephemeral_storage_request`, any of them. A limit the file leaves out takes the
daemon's default (`config.DefaultResources()`, `packages/daemon/internal/config/kubernetes.go`);
a `cpu_request` or `memory_request` it leaves out takes the file's limit where it set one, else the
default's request (`settledRequest`), so a file that raises `memory` alone is requested in full.
With no request set below a limit, as at the defaults, every Legion pod is `Guaranteed`; a lower
request makes that pod `Burstable` ([Issue sizing](#issue-sizing-one-reservation-per-pod),
"Guaranteed or Burstable").

| key | cpu (limit / request) | memory (limit / request) | ephemeral-storage (limit / request) |
| :--- | :--- | :--- | :--- |
| `issue` | 3 / 3 | 19Gi / 19Gi | 80Gi / 6Gi |
| `controller` (`controller: daemon`) | 1 / 1 | 4Gi / 4Gi | 10Gi / 1Gi |
| the image probe (fixed, not a `resources` key) | 250m / 250m | 1Gi / 1Gi | 5Gi / 1Gi |

The `issue` default is the sum of the six per-role defaults of the layout that ran one container
per role. One number frees nothing at run time: under gVisor every process of a pod already used
whatever the pod had free, so the pod's peak must fit its reservation — an issue pod measured at
1-minute resolution used its whole 19Gi for minutes while its reviewer ran a review (2026-10-21).
On GKE Autopilot the cap is 10Gi of ephemeral storage for the main containers' sum and 10Gi for
each init container (server dry runs, 2026-10-24), and GKE clamps a container's limit to its
request, so a GKE deployment sets
`issue: {ephemeral_storage: 10Gi, ephemeral_storage_request: 10Gi}`.

The image probe pod's reservation is the daemon's own and no issue pod's share (`probeReservation`,
`packages/daemon/internal/daemon/kubernetes.go`): `legion probe-image` starts one Oh My Pi at a
time and runs no lane or browser, so 250m and 1Gi hold it, and the probe runs at every boot before
anything is served — a probe that reserved a larger share, as it did the controller's 1 CPU and 4Gi
until LEGION-632, could leave the daemon unable to boot on a pool with no room for a pod that
large ([Issue sizing](#issue-sizing-one-reservation-per-pod), "The bound").

At the defaults an issue pod reserves 3 CPU and 19 GiB, an ephemeral-storage limit of 80Gi and a
request of 6Gi. `issue_volume` sizes each issue pod's own disk: the clone, the workspace,
uv's Pythons and packages, and everything a role writes without
being told where to put it. Four of those directories are the issue's, one set its roles share —
`/legion/scratch/tmp`, `/legion/scratch/cache`, `/legion/scratch/data` and
`/legion/scratch/bun/install/cache`, the cache home holding the issue's one Playwright browser
registry, a Chromium about 400 MB, and the bun install cache's cold fill for this repository
1.4 GB — beside the Go pair this change adds, the build and module caches at
`/legion/go/build` and `/legion/go/mod`, 1.3 GiB and 931 MiB for this repository after
`go test -count=1 ./...` over `packages/daemon` alone — the figure an operator sizes from,
because roles run tests — where a cold `go build ./...` of that same module with nothing tested
leaves 751 MiB and 375 MiB, the reproducible cold-build measurement that
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod) records below beside it. One directory is
per role, its state home at `/legion/scratch/state/<role>`, holding Oh My Pi's logs,
Chromium's `browser-profiles` and the plugin generation an installing launch puts there; it is
per role for a mechanism, not isolation, since every role of the issue can write it: Oh My Pi
names its browser broker's lock from the state
root, the name is an abstract unix socket, an abstract socket name is one name across a network
namespace, and every role of a pod shares the pod's, so one state home would block every role's
broker but the first. An
operator's arithmetic is therefore the issue's: one set of four, one Go pair, and one state home
per role the issue runs. The project controller's pod has a volume of the same size — its
Sandbox's own, not its pod's — holding the scratch of its one role.
Nothing prunes `/legion/scratch`: it grows for the life of the pod, which is the life of the disk
it sits on, and this change raises `issue_volume`'s default from `20Gi` to `50Gi`
(`defaultIssueVolume`,
`packages/daemon/internal/config/kubernetes.go`) — a default every project takes rather than a
line of one deployment, so the apply this change needs is the image digest alone, with no
deployment edit. The size rides in the pod's own ephemeral claim template, evaluated when the
kubelet creates the claim, so every pod created after that apply gets 50 GiB and a pod already
running keeps the size it started with until a launch replaces it.
That fits by the arithmetic alone: the shared layout's worst case for an issue that works all six
roles is about 8.5 GiB — one temporary directory, one cache home, one data home, one bun install
cache and one Go pair — against live volumes measured at 2.1 to 2.7 GiB of their 20 (01:45Z on
2026-10-21). A larger figure is on record and is not a production one: the volume this change was
written on reached **17 GiB of its 20, 3.0 GiB free** (01:32Z on 2026-10-21) with 14 GiB of that
in six per-role copies of the Go build and module caches — an intermediate layout no shipped image
ever produced, since a role at the merge base is told no `GOCACHE`, no `GOMODCACHE` and
no `BUN_INSTALL_CACHE_DIR`, and the Go pair is new on the volume here. A disk that dies with its
pod fills no slower than one that outlived it: the same clone, the same caches, the same size,
and a single pod's life is long enough to fill 50 GiB, since a pod lives as long as the issue
runs without being stopped or replaced. There are two remedies, and the quick one destroys work:
restart the issue, and its next pod brings an empty disk of its own, taking the turn's unpushed
changes and every cache the pod had built with the old one ([The disk dies with its
pod](#the-disk-dies-with-its-pod)). The other leaves the turn where it is, and is the one that
follows. The remedy for a volume
near full is manual, because nothing prunes, and it accounts for every directory this layout
creates: each is on the delete list or among the exemptions under it, and a test holds
the two lists to what the runtime renders
(`TestTheReclaimParagraphNamesEveryDirectoryTheRendererPutsOnTheIssueVolume`).

Delete list — `chmod -R u+w` first, then remove, these being the paths the issue's roles share
and the next build or install rebuilds:

- `/legion/scratch/cache`
- `/legion/scratch/tmp`
- `/legion/scratch/bun/install/cache`
- `/legion/go`
- `/legion/uv/cache`

Exempt, each for a reason the list above does not hold:

- `/legion/scratch/state/<role>` — per-role, and Oh My Pi's logs, Chromium's profiles and the
  installed plugin generation rather than a cache; small. Oh My Pi's state-class directories
  (`run/daemons`, `logs`, `browser-profiles`, `reports`, `terminal-sessions`) hang under
  `$XDG_STATE_HOME/omp`, so they are this path's and not the shared data home's.
- `/legion/scratch/data` — the shared `XDG_DATA_HOME`, the data-class home of the tools that
  honour it. Nothing re-derives it: the caches above are rebuilt by the next build or install,
  where what a tool stores in its data home is gone when it is deleted.
- `/legion/uv/python/<issue>` — the other half of uv's directory, exempt for that same reason
  sharpened by a dependency: it holds the interpreters uv installed, and a project's `.venv` in
  the issue's workspace, which this remedy does not delete, links to one of them, so removing it
  leaves a virtual environment pointing at an interpreter that is gone. The cache half costs only
  a download.

Measure either of the last two before you touch it, and delete it only when it is the reason the
volume is full.

`/legion/scratch/tmp` is on the list but takes a different window, and it is the one path here
whose only creator is a role's **next shim start**: `podsafety.EnsureScratch` makes the told
directories once, as the shim starts Oh My Pi (`worker_shim.go`), and a role's shim lives across
turns, so a `tmp` deleted under a live role stays gone until that role is relaunched. What a
live role then does with it splits two ways, and the quiet half is the dangerous one: `mktemp -d`
fails with exit 1, where Python's `tempfile` silently falls back to `/tmp` — which in a role's
namespace is the role's own directory on the `role-homes` volume, on the node's disk and charged to
the ephemeral-storage limit this whole layout exists to get out from under. So delete `tmp` with the role stopped, not merely between
its turns; the rest of the list is reclaimable between turns, when no role is mid-build or
mid-install. Do not leave it out of the list either, because a temporary directory sounds
transient: a role at the merge base is told no `TMPDIR` at all and its temporaries died with the
container, where here a killed build's residue lives as long as the issue does — one interrupted
`go build` left **851 MB in a single `go-build*` tree** on this branch's volume, more than
everything else the reclaim found. As for the rest: a
role mid-build rebuilds what it loses, but deleting `$BUN_INSTALL_CACHE_DIR` under a role
installing its plugin pair takes the `flock` inode with it, so a second role enters the critical
section while the first is inside it and both fill one cold cache entry (measured: with no delete
the second waits the full bound; with the directory deleted and remade mid-install it does not).
The install block cannot prevent that and does not claim to — a lock whose inode is unlinked
protects nothing — what it does is keep the damage to that one install: each attempt remakes the
cache directory before taking its lock and clears a non-directory left at that path, so the
launch retries and succeeds instead of the issue being left with a regular file there that fails
every later installing launch until a person removes it.
(The 5.5 GiB a reclaim returned on this branch's own volume came out of six per-role copies under
the intermediate layout above, not out of these paths.) The `chmod` is not caution — the go
command writes every extracted
module directory read-only (mode 555), so a plain `rm -rf` of a module cache exits 1 with a
`Permission denied` line per file and leaves bytes behind, and what it leaves is the extracted
source while what it removes is the re-downloadable `cache/download`: run without the `chmod` the
reclaim frees the cheap half and keeps the expensive one. The non-zero exit is the one mercy in
that shape, since a scripted remedy fails loudly rather than silently. Size it
knowing that moving these writes onto
the volume removes the alarm they used to ring: an ephemeral-storage overrun was a kubelet
eviction with a pod event naming the container, where ENOSPC on the PVC is no event at all and
surfaces as unexplained failures in jj, in the session transcripts, in `workspace-init` and in
the CodeGraph build — and nothing in this repository reads a PVC's usage.

Ephemeral storage is what the `roles` container writes outside every volume: its root filesystem,
the node's disk — the image's own content and whatever a recipe writes outside the told
directories — and the `role-homes` disk `emptyDir`, which holds each role's home overlay and its
`/tmp` and `/var/tmp` ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)). Each role's
private and state directories and the pod's shared config home are in-memory `emptyDir`s, whose pages the bound
does not count, and the issue's temporary directory, cache and data homes and bun install cache,
each role's state home and the issue's shared Go caches are on the issue's volume, which the
bound does not count either.
Pods of unrelated trees share a node, so one issue filling the node's disk would put every pod on
it under `DiskPressure`; the limit has the kubelet evict the pod of the container that passed it,
the offending issue's alone, and the request is what the scheduler fits to the node's allocatable
ephemeral storage, its root volume. The request is small by default so
that scheduling binds to almost nothing; an operator
who knows the root volume raises `ephemeral_storage_request` so the scheduler reserves disk
([Issue sizing](#issue-sizing-one-reservation-per-pod)).

`packages/daemon/internal/config/kubernetes.go` reads the block and refuses, naming the key:
- `tree_volume`, the tree-volume layout's key: `runtime.kubernetes.tree_volume is now issue_volume:
  one volume per issue`;
- `role_profiles`: `unknown key runtime.kubernetes.role_profiles: an issue pod's reservation is
  runtime.kubernetes.resources.issue, its cpu and memory limits with optional lower cpu_request and
  memory_request, and a field absent there takes the daemon's default`;
- a workflow role's key under `resources`: `runtime.kubernetes.resources.<role> is gone: an issue
  pod has one reservation, runtime.kubernetes.resources.issue`; any other key but `issue` and
  `controller`: `runtime.kubernetes.resources key "<k>" must be issue or controller`; and
  `resources.controller` unless `controller: daemon`, the one setting under which the daemon
  launches the controller's pod (`runtime.kubernetes.resources.controller sizes the pod of the
  controller the daemon launches, and controller: operator launches none: set controller: daemon or
  drop the key`);
- the earlier shape inside an entry: `runtime.kubernetes.resources.issue.requests is gone: a
  reservation is its cpu and memory limits, with cpu_request and memory_request below them
  where set` (`limits` likewise); any other member of an entry but `cpu`, `cpu_request`,
  `memory`, `memory_request`, `ephemeral_storage` and `ephemeral_storage_request` is an unknown key,
  and any of those six or `issue_volume` that is not a positive Kubernetes quantity is refused as
  such (`… must be a positive Kubernetes quantity (e.g. 20Gi or 500m)`);
- a request past its limit, once both are settled — the file set one side or both, since the
  defaults never exceed: `runtime.kubernetes.resources.issue.memory_request 20Gi exceeds memory
  19Gi`, and likewise `cpu_request … exceeds cpu …` and `ephemeral_storage_request …
  exceeds ephemeral_storage …`, for `controller` too, and `runtime.kubernetes.repository_cache.resources.…` likewise;
- a `repository_cache` that is not a mapping, or names a member but `volume`, `refresh_minutes`,
  `retain_hours` and `resources`; a `volume` that is not a positive Kubernetes quantity; a
  `refresh_minutes` or `retain_hours` that is not a positive integer; its `resources` take a
  reservation's six members and nothing else;
- `gateway`, removed with LEGION-270: a pod's model route is the operator's `pod`;
- `agent_secrets.operator`, removed with LEGION-664: the daemon logs in with its own ServiceAccount
  token (`token_file`), which no person approves, so it names no approver;
- an image that is not pinned by digest;
- a `setup` that is not a list of non-empty strings (`runtime.kubernetes.setup must be an array
  of non-empty strings`), or an empty list (`runtime.kubernetes.setup must name a command: leave it
  out to run none`);
- a `session_store` that keeps sessions as files (`pvc`), and an omitted `session_store`, which
  settles to it and is the same refusal rather than a default; a `session_store` other than `pvc`,
  `postgres` or `cloudsql-iam`; `postgres` without `session_dsn_secret`, or with one that is empty
  or not a Secret data key (`[-._a-zA-Z0-9]+`); `cloudsql-iam` without `session_database`, or with
  one that is not a mapping, names a member but `instance`, `name` and `user`, leaves one of the
  three empty, or whose `instance` is not a connection name the Cloud SQL connector uses whole
  (`project:region:instance`, or `domain:project:region:instance` for a domain-scoped project, with
  no whitespace); a `session_dsn_secret` under `cloudsql-iam`; and a `session_database` under
  `postgres` (an inert key is refused, never ignored). At boot the daemon also refuses, under
  `postgres`, a `session_dsn_secret` that is the providers Secret's `NATS_NKEY_SEED` and a
  `provider_keys` entry that reads the same key (the shim would export the URL into Oh My Pi's
  environment) — neither can arise under `cloudsql-iam`, where no pod mounts a session key — and,
  under every store, an
  operator's `pod.env` or `provider_keys` naming `OMP_SESSION_STORAGE` or
  `OMP_SESSION_SQL_DSN_FILE`, or a `provider_keys` entry named `OMP_SESSION_SQL_DSN`
  ([Selecting the store](#selecting-the-store)).

`legion start --check-config` shows each of these before anything starts: a file carrying
`tree_volume: 20Gi` is refused naming `issue_volume`; one carrying `resources: {issue: {limits:
{memory: 8Gi}}}` naming `limits`; one carrying `resources: {tester: {cpu: 2}}` naming
`resources.issue`; one carrying `resources: {small: {cpu: 500m}}` naming `issue or controller`;
one carrying `resources: {issue: {memory_request: 20Gi}}` naming `memory_request`; and a file with
no `resources` block passes, as does one with `resources: {issue: {cpu: 4, memory: 24Gi}}` or
`resources: {issue: {ephemeral_storage: 10Gi, ephemeral_storage_request: 10Gi}}`, the daemon
filling every field the file leaves unset. A file that gives a pod a request below its limit
passes and prints, after the capability gaps, which pods that makes `Burstable` — `the issue pod`,
`the controller's pod`, or `the issue pod and the controller's pod`:

```text
resources: Burstable (a request below the limit): the issue pod; every other pod is Guaranteed
```

Legion holds no model route. `pod` is the operator's: `env`, `volumes` (each a `secret`,
`config_map`, `projected`, `persistent_volume_claim` or `nfs` source), `volume_mounts` and
`service_account`, added to every pod, the image probe's included, and refused where they name a
path or variable of Legion's own or the worker image's. A ConfigMap or Secret a volume names must
exist in the daemon's namespace, with every key the volume asks of it: the image probe refuses the
boot at once, naming it, when it does not, for every volume a `volume_mounts` entry mounts (the
kubelet attempts no other)
([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)).

A volume may also be an existing network share: [Network shares](#network-shares) lists the sources
and what each runtime supports.

`provider_keys` names keys of the providers Secret, which every pod mounts, those
keys alone, for the shim to export. `deploy/kubernetes/operator-route/` is an example of one: the
Hawk model gateway, keyed by a projected ServiceAccount token, with a `models.yml`, a settings
overlay, and the pod that mounts them; the operator keeps their own copies of the two files in a
directory of their own ([Operator configuration](#operator-configuration)), and the Stage 4a and 4b
proofs run on the example, each with its own copy of its ConfigMap.

`setup` is the deployment's setup command, an argv run for every role — each workflow role in its
own namespaces of an issue pod, and the controller in its container under `controller: daemon` — before each start of
that role's Oh My Pi: a fresh role's, a resumed one's and a replaced pod's alike, since a relaunch
may come up in a new pod, whose filesystem outside its volumes is new. It is one command for every
role; a shell command is `[/bin/sh, -c, '<command>']`. It runs as the role's
user (uid 1000), in the container's working directory (the issue's workspace, or the controller's
`/legion`), with the environment Oh My Pi gets: the role's gh files and the git credential
entries ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)), so a `git clone` of a repository
one of the project's owners holds, which the role's App is installed on, is answered with the
role's own token. It runs after the install a stale-plugin relaunch makes
([Contract discipline](#contract-discipline)), with stdin from `/dev/null` and its stdout on
stderr, so neither touches the RPC stream the shim and Oh My Pi share; its output is in the role
container's log. Oh My Pi starts only once it exits 0. A non-zero exit ends the start with
`legion setup: runtime.kubernetes.setup exited <status>; not starting Oh My Pi` on stderr and the
command's own exit status, which the launcher reports as the generation's exit: the daemon reads
the role as gone, charges a launch failure, and retries until the budget ends the claim in a
`worker-died`, as it does an install that fails. Its run counts against the launch's registration
deadline (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, 360 s by
default) and adds to every start's time. The container's filesystem keeps what an earlier start
in the same pod wrote, so a command that installs something should succeed when it is already
there. An Oh My Pi plugin from a marketplace repository, for example:

```yaml
runtime:
  kubernetes:
    setup:
      - /bin/sh
      - -c
      - '{ omp plugin marketplace update acme-skills || omp plugin marketplace add acme/agent-skills; } && omp plugin install agent-skills@acme-skills --force'
```

`acme-skills` is the `name` the repository's marketplace file gives; `omp plugin marketplace add`
refuses a name already added, and `omp plugin install` an installed plugin without `--force`. On a
container's first start the `update` finds no such marketplace, so the role's log shows one
`Failed to update marketplace … not found` line before the `add` succeeds; that line is expected.

Under `runtime: kubernetes` it also requires `daemon_url`, `envoy_url`, `nats_urls`,
`envoy_token_file`, `operator_token_file`, `dispatch_url`, `github_apps` and `projects`. It refuses
`omp_invocation` and `omp_launch_prefix`: every pod runs the worker image's Oh My Pi. It refuses a
`worker_backends` entry that puts a role on `claude` too, since the worker image carries no Claude
Code (`worker_backends.<role> is claude, which runs only under runtime: tmux`). Every
address a pod is handed must be one a pod can reach, so `daemon_url`, `envoy_url`, `dispatch_url`
and each `nats_urls` entry may be neither loopback nor the unspecified address, and neither may the
worker-stream host a pod dials: `advertise_host` when the file sets one, `bind` otherwise. `bind`
and the host of each of those URLs and of `agent_secrets.url` must be ASCII. This is a breaking
configuration change for a file that names a host in Unicode; write it in its `xn--` form.
`envoy_url` and each `nats_urls` entry are endpoints, not request URLs: they refuse a query string
or fragment. This is a breaking configuration change for a file that has either; put credentials in
URL userinfo or in a Secret, never in a query.
`advertise_host` is an IP address or an ASCII DNS name, the host alone, and only
`runtime: kubernetes` accepts it. A pod's own IP changes on every restart, so a daemon running
inside the cluster binds `0.0.0.0` and lets `advertise_host` name the Service DNS name that
reaches whichever pod is live.
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
every worker (`network`: dispatch://LEGION-5, the pod is the boundary, except a workload a role
starts where its deployment's cluster admits it and the Service fronting it
([Running a workload in the cluster from a role](#running-a-workload-in-the-cluster-from-a-role));
`operator-setup`:
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
- `resource-limits`: the issue pod — and the controller's pod, under `controller: daemon` —
  reserves CPU and memory (`config.Reservation.Reserved`: `runtime.kubernetes.resources.issue` and
  `.controller` or the daemon's default, a limit with a request no greater than it,
  [Issue sizing](#issue-sizing-one-reservation-per-pod)), which a loaded Kubernetes configuration
  always satisfies; open, the detail would read `no CPU and memory requests and limits under
  runtime.kubernetes.resources for <pods>`. Present, it says each pod's QoS class: `every pod has
  CPU and memory requests and limits under runtime.kubernetes.resources, each request equal to its
  limit, so every pod is Guaranteed`, or `every pod has CPU and memory requests and limits under
  runtime.kubernetes.resources; Burstable (a request below the limit): <pods>; every other pod is
  Guaranteed`, `<pods>` being `the issue pod`, `the controller's pod`, or `the issue pod and the
  controller's pod`, so `legion state --json` shows which pods reserve less than they may use.
  Under tmux it is open unless decided: a pane has no requests or limits.
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
one Sandbox per issue, shared by every role that works it, owning its pod and the role Secrets
and no volume at all. The pod
template (`manifest.go`) has two init containers and one container, `roles`, which runs every
workflow role (`claim.Roles`: architect, planner, implementer, tester, reviewer, merger):

1. `workspace-fetch` fills the pod's feed, the bare repository at `/legion/feed/<owner>/<repo>.git`
   on the issue's own volume. It is told its repository's cache (`LEGION_REPOSITORY_CACHE_URL`,
   the cache's Service, and `LEGION_REPOSITORY_CACHE_TOKEN_FILE`, the cache's own token), and a
   fresh feed comes from that cache alone: the fetch waits for it to be ready (`waiting for the
   repository cache <url> to be ready: <its answer>`, every 10 s, for at most the cache's own first
   clone's 2 hours and 10 more, `workspace.CacheWaitTimeout`; a pod waiting holds its node
   reservation, and its launch the issue's lifecycle lane, all the while, both while a first clone
   runs and when the cache never becomes ready — a crash loop, a cache pod that cannot be scheduled
   or whose volume does not bind, a first clone past its 2 hours — so each such launch spends 130
   minutes, not 40, before its fetch fails), unpacks its
   copy of the repository, checks each pack and its index against the checksums git wrote into them
   and that every object the refs reach is there, then fetches from GitHub the default branch GitHub
   names, the issue's recorded default while GitHub has it, and the Legion issue branches: its log's last
   line is `workspace-init fetch: <n> bytes from the repository cache <url> in <d>, its history
   verified in <d>; <m> bytes from GitHub for the delta (<k> refs)`. A cache that is never ready,
   refuses the pod's token, names another repository, or streams history that does not verify
   fails the fetch, naming why, and nothing is cloned from GitHub instead, so the cache's own first
   clone is the one full download of the repository ([The repository cache](#the-repository-cache)).
   A fresh feed is filled beside its final path and renamed into place only once the stream, its
   verification and the delta have all succeeded, so the disk holds a whole and current feed or
   none. A retry of the fetch in the same pod that does find a feed there fetches the delta into it
   alone (`the feed holds <owner>/<repo> already: …`), after removing every git lock file a fetch
   killed mid-update left in the feed and every partial pack (`objects/pack/tmp_*`) a transfer
   killed mid-pack left there, each logged with its age, and after deleting every branch outside
   the delta that GitHub no longer lists or has moved, each logged with its reason, as a fresh
   feed's fetch does too (below, [The repository cache](#the-repository-cache)). Such a retry is
   rare: the kubelet retries a failed init container on the pod it failed in, but the daemon reads
   that first failure (`failedInit`, `observe.go`) and replaces the pod within seconds, and a pod
   created anew starts on an empty disk and fills its feed whole.
   It takes an existing feed only in the shape the
   fetch and Git maintenance write: no symlink anywhere, its owner directory included; no gitfile;
   no hook but git's samples; a configuration holding only the keys git's bare init and clone and
   the fetch write, each line in git's own form (a `[section]` header alone on its line, or a
   tab-indented `key = value` with no quote, escape or comment), since git also reads a key written
   on its header's line; Git's `gc.pid`, `gc.log`, packed refs and reflogs; `info/exclude`,
   `info/refs`, and the temporary files `update-server-info` leaves; and
   `objects/info/packs` plus commit-graph files. An alternate object store is refused. A feed in
   any other shape is removed, logged, and filled from the repository cache: within one pod the
   feed is written by `workspace-fetch` alone and every other container mounts it read-only, so
   this check holds the fetch's own retry to what the fetch left, and a disk from before this
   release, which any role of its issue could write, no longer reaches a pod at all.
   Every fetch from GitHub then
   points the feed's `HEAD` at the branch GitHub names as the default now (`git
   ls-remote --symref`, under the same credential and bound), since a fetch never moves `HEAD`; a move is logged (`the default branch of
   <owner>/<repo> is now <name> (HEAD named <ref>)`). A branch GitHub renamed between the fetch and
   that read is not yet in the feed, so `HEAD` stays until a fetch brings it, logged (`GitHub's
   default branch of <owner>/<repo> is now <name>, which this fetch did not bring: HEAD stays at
   <ref> until a fetch brings it`). Once the fetch may start — at once, or once the cache is ready
   — the cache's stream and its verification share one 30-minute bound (`workspace.FetchTimeout`),
   each given what remains of it, and the delta from GitHub has a 30-minute bound of its own, so a
   slow stream never leaves the delta minutes to run in. The fetch's waits, transfers and checks end
   within 190 minutes (`workspace.FetchBudget`) however long it waited, and the few local
   git commands around them take moments; a step with nothing left fails the fetch, naming the
   bound. Each transfer from GitHub is tried again up to four times after a transient failure,
   within that bound. It is the only process that holds the provisioning token
   ([Trust model](#trust-model-the-provisioning-token)).
2. `workspace-init` provisions the issue's clone and its jj workspace on the pod's own disk from
   the read-only feed, taking no lock and waiting on no other pod: no other pod mounts that disk.
   The clone is a local `git clone` of the feed. Git hard-links the feed's object files when the
   filesystem allows, but the clone owns those links, so replacing the feed cannot remove history
   its refs need; it has no `.git/objects/info/alternates`. Its `origin` names GitHub, and
   `jj git init --colocate` makes it jj's, tracking the default branch as a `jj git clone` would.
   Its environment is the image's `PATH` and the XDG base directories (`initEnvironment`,
   `manifest.go`); nothing names a role, a generation, a lock wait, another issue's workspace, or
   anything the disk is expected to hold already — the disk is created with the pod, so every
   provision is the issue's first. The workspace starts at the
   issue's branch, `legion/<KEY>`, which the daemon created on GitHub at the repository's default
   branch, as GitHub names it (`default_branch`, `release/v7.1` as readily as `main`), before the
   issue's architect or planner started; with no such branch it starts at that default branch,
   which the pod's command names (`--default-branch`), and its log says which (`workspace-init:
   <dir> on legion/<KEY> from <default branch>`). Before any git or jj runs it removes every git
   lock file (`<file>.lock`) in the clone's git directory and in the issue's own worktree entry of
   it, and logs each with its path and age (`RemoveStaleGitLocks`,
   `internal/workspace/stalelock.go`): the kubelet retries a failed init container on the pod it
   failed in, so a first attempt killed mid-fetch leaves its locks on the same disk the retry
   works on, and every later write of a locked file fails on it — when the retry runs before the
   daemon replaces the pod, which it does within seconds of reading the failure (see
   `workspace-fetch` above). No process can hold one when the
   retry runs, since the pod's `roles` container has not started and no other pod mounts the disk.
   Another worktree entry of the clone keeps its locks; a lock it cannot remove,
   and a clone whose `.git` or `.git/worktrees` is a symlink (git never makes either one, and it
   would take the removal outside the clone), fail the container naming the path. jj's own locks
   are `flock(2)` locks, which end with their holder, so it leaves them. Under a running pod nothing
   removes a lock, since another role's git, which a role's namespace cannot see, may hold it: the
   `jj` the shim's working-copy adoption and a `handoff_complete` run name each lock still on disk
   that they failed on, with its age, in their error, say not to remove it while the pod runs, and
   that replacing the issue's pod removes it.
3. The `roles` container's PID 1 is one `legion launcher` in multi-role mode:
   `/opt/legion/bin/legion launcher --connect tcp://<advertise_host, or bind with none
   set>:<worker_stream_port> --sandbox <name> --roles-dir /var/run/legion/roles --role architect
   --role planner --role implementer --role tester --role reviewer --role merger --stop-grace <d>`,
   the pod's uid from `POD_UID` (the downward API). It holds one authenticated worker-stream
   connection per role, each with that role's own launcher token, read from
   `/var/run/legion/roles/<role>/launcher/LAUNCHER_TOKEN`, and starts and stops each role's
   `legion worker-shim` (Oh My Pi) child on the daemon's command, never a worker process of its
   own. Its log has one line for each connection the daemon accepted, `launcher: connected to the
   daemon at <address> as launcher <id> (sandbox <name>, role <role>, pod <uid>)`, and one for each
   connection that ended, `launcher: daemon connection: <why>; reconnecting`, so a connected
   launcher reads apart from one still dialing. The launcher's own lines for a role, and each role
   child's stdout and stderr lines, are prefixed `<role>: `, so the container's one log interleaves
   every role's lines and `kubectl logs <pod> -c roles | grep '^<role>: '` reads one role's. A
   prefix only ever starts a line: the launcher's lines and each generation of a role write through
   writers of their own, each holding back a partial line until its newline arrives, and a
   generation's last line without a newline is written out as a line of its own once the generation
   ends. A line longer than 64 KiB (`maxLogLine` in `internal/launcher/launcher.go`) is cut there:
   its first 64 KiB is written as a line of its own and the rest follows on a new line behind the
   prefix again. A line the launcher writes for no role (a panic, a failed reap) has no prefix, and
   when the container ends the daemon's detail for a role quotes that role's last 20 lines, its
   prefix dropped, then the container's last 20 unprefixed lines under `the container's own last
   lines, no role's:` (`roleLog` in `internal/runtime/sandbox/observe.go`), all read from the log's
   last 2,000 lines and at most 1 MiB. As the container's PID 1 the launcher reaps every orphan its PID namespace
   adopts (what a `kubectl exec` leaves behind), never a role's own generation, whose exit its role
   reads. A generation whose role-init fails before it starts the shim, setting up the role's
   namespaces, exits 71 (`launcher.RoleInitSetupFailedExit`, a status role-init uses for nothing
   else) and is reported with the exit code `-2` (`shimwire.LauncherExitSetupFailed`), which no
   shim exits with, and the daemon's detail for it quotes the role's last lines, role-init's reason
   among them; a role-init that ends any other way without reporting its shim's status (a panic's
   2) is reported with that exit as it is.

The pod's disk is declared in the pod template itself, as a generic ephemeral volume
(`issuePod.disk`, `podkind.go`): `ReadWriteOnce`, of `runtime.kubernetes.issue_volume`
(50Gi by default) and `storage_class`. Kubernetes creates its claim when the pod starts, names it
`<pod>-issue` after the pod and the volume, makes the pod its owner, and deletes it when the pod
goes; the claim carries no label, since the Sandbox CRD prunes whatever an ephemeral template's
`metadata` sets, so an operator finds it by that name and owner and by no selector ([The disk
dies with its pod](#the-disk-dies-with-its-pod)). An issue's Sandbox owns no
`volumeClaimTemplates` entry at all (`issuePod.disk`), and no role's session is on the
disk: every session is a row in the session database ([Selecting the store](#selecting-the-store)),
which is what a role carries from one pod of its issue to the next. `workspace-init` and the
`roles` container mount the disk at `/legion`. The feed is its `feed` directory:
`workspace-fetch` mounts that `subPath` alone, read-write, at `/legion/feed`, and `workspace-init`
and `roles` mount the same `subPath` again, read-only, at `/legion/feed` over their
whole-disk mount. The clone's object store owns its links to that feed, and no agent writes what
the token-holding container reads. The kubelet makes the directory on first use. Each role's Secret,
`<sandbox>-<role>-boot`, holds that role's launcher token and its gh bundle: for every GitHub owner
of the project, `github-hosts-<owner>`, gh's `hosts.yml` carrying a GitHub App installation token
under user `x-access-token`, and `github-config-<owner>`, a `config.yml` of `version: "1"`, and
`github-default`, the owner of the issue's repository. The token for that owner is the role's App's
(the implement App's for the implementer and the merger, the review App's for every other role);
every other owner's is the same App's narrowed to reading contents, pull requests and metadata.

The `roles` container mounts each role `<r>`'s own volumes under its directory of
`/var/run/legion/roles`: `/var/run/legion/roles/<r>/launcher` (volume `launcher-<r>`, read-only:
the launcher token, as `LAUNCHER_TOKEN`), `/var/run/legion/roles/<r>/gh` (`gh-<r>`, read-only: the role's gh bundle,
each owner's two keys as `<owner>/hosts.yml` and `<owner>/config.yml`),
`/var/run/legion/roles/<r>/private` (`private-<r>`, memory), `/var/run/legion/roles/<r>/state`
(`state-<r>`, memory) and, only when the pod enrolls with the secrets broker,
`/var/run/legion/roles/<r>/agent-secrets` (`agent-secrets-key-<r>`, memory). One disk `emptyDir`,
`role-homes`, is at `/var/run/legion/roles/.homes`. The pod's shared mounts appear once: the issue
volume at `/legion`, the config home, the feed read-only, the providers
Secret's configured keys when there are any (with its `NATS_NKEY_SEED` key when the daemon has a
NATS nkey seed), the broker's token when the pod enrolls, and the operator's volumes and mounts.
Every one of them that belongs at or under `/home/legion` (the config home at `.config`, an
operator's mount there) is
mounted under `/var/run/legion/home` at the same relative path instead, so `/home/legion` itself
carries no mount in the `roles` container; each role's namespace binds it back at its own path,
except the config home. That one is `workspace-init`'s, and no role sees or writes it: each role
gets a config home of its own at `~/.config`, a copy of the shared one made the first time a
generation of the role starts in the pod and kept on `role-homes` for the pod's life
(`/var/run/legion/roles/.homes/<role>/config` in the container's view; `ownConfigHome` in
`internal/launcher/roleinit_linux.go`; the copy holds a `.legion-config-home-complete` file once
whole, and one without it is made again), with any mount under the shared config home bound back
on top. A git `core.fsmonitor` or a jj alias one role writes in its own `~/.config` so never runs in
a sibling's git or jj; a jj setting it plants in the shared clone's `.jj/repo/config.toml`, or a git
setting in the clone's `.git/config` other than the two the git pins below hold, does.
A pod's gh projection names the owners of its creation, so an owner added to the project reaches
the issue's next pod. The issue's `-boot` Secret holds the provisioning token, projected into
`workspace-fetch` alone.

**Each role in namespaces of its own.** The launcher starts each role generation through the hidden
`legion launcher role-init`, in its own Linux user, mount, PID and IPC namespaces. Inside them
`/var/run/legion/roles` is covered by an empty memory filesystem, and the role's own gh, private,
state and agent-secrets directories are bound back at the fixed paths `/var/run/legion/gh`,
`/var/run/legion/private`, `/var/run/legion/state` and `/var/run/legion/agent-secrets`; its
launcher token is bound nowhere, so no role's process reads it, and no role sees a sibling's
directories. `/home/legion` is an overlay whose lower is the image's and whose upper and work
directories are the role's on `role-homes` (a work directory of each generation's own), with
every mount the container holds under `/var/run/legion/home` bound back on top at its own path
under `/home/legion`, and `/var/run/legion/home` itself then covered. The lower layer has no
mount under it because Linux refuses an overlay over one with mounts locked under it from a more
privileged namespace, which every container mount is inside a role's user namespace; gVisor
allows it today, and the layout does not lean on that. `/tmp` and
`/var/tmp` are binds of the role's own directories on `role-homes`; `/run/lock`, `/var/lock` and
`/dev/shm` are private small memory filesystems; `/dev/pts` is a new devpts instance holding only
the terminals the role opens (`newinstance`, `ptmxmode=0666`, with `/dev/ptmx` bound to it where
the image's is a node), and `/dev/mqueue` is mounted from the role's own IPC namespace, so it
shows only that namespace's message queues (`privateDevices`, `roleinit_linux.go`); `/proc` is the
role's own. role-init follows no link on the way to a mount point in the role's own layers: a link
an earlier generation of the role left there (a `~/.cache` linked elsewhere, say) is removed and
the directory or empty file the mount needs is made in its place, logged as `role-init: <path> was
a link, which role-init does not follow on the way to a mount point: replaced it with the mount
point's place`, and anything else in the way that is neither a link nor a directory fails the
generation. role-init then starts
the shim's argv unchanged as its child, cloned into a nested user namespace that maps uid and gid
1000 with no capabilities, and stays the PID namespace's init, so when a generation ends every
process it started ends with it and a role's `kill -9 -1` reaches none of its siblings'. The
isolation rests on ownership and private mounts, not on a read-only root, which a nested user
namespace could undo
(`docs/solutions/legion/namespace-isolation-under-gvisor-holds-on-locked-mounts-and-ownership-not-a-read-only-root-INFRA-208.md`). An operator mount
(`runtime.kubernetes.pod.volume_mounts`) at or above `/tmp`, `/var/tmp`, `/run/lock`, `/var/lock`
or `/dev/shm`, or at or above the image's other spellings of them (`/var/run/lock`, `/run/shm`,
`/var/run/shm`), is refused: `runtime.kubernetes.pod.volume_mounts[i].mount_path <p> is at or above
<dir>, which every role's namespace in an issue pod covers with its own: mount under it instead`.
A mount under `/tmp` and the like is allowed, and role-init carries it back. A mount at, under or
above `/dev/mqueue`, `/dev/ptmx`, `/dev/pts` or `/proc` is refused (`roleReplacedPaths` in
`internal/runtime/sandbox/manifest.go`), since role-init mounts a filesystem of the role's own
there (a new devpts instance at `/dev/pts`, its `ptmx` bound over `/dev/ptmx`, the role's own IPC
namespace's mqueue at `/dev/mqueue`, the role's own `/proc`) and carries nothing back onto it:
`runtime.kubernetes.pod.volume_mounts[i].mount_path <p>
overlaps /dev/pts, which every role's namespace in an issue pod replaces with a filesystem of its
own and carries no mount back onto: a mount may be neither at, under, nor above /dev/pts`. A mount
at, under or
above `/var/run/legion/roles`, `/var/run/legion/home` or `/var/run/legion/role-probe` (the image
probe's volume, [The image is probed before it publishes](#the-image-is-probed-before-it-publishes))
is refused as any other path Legion mounts
is: `runtime.kubernetes.pod.volume_mounts[i].mount_path <p> overlaps /var/run/legion/roles, which
Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's`. Every one
of these rules, and the check of the paths Legion mounts and the image owns, compares a mount
through the worker image's links to directories (`/var/run` to `/run`, `/var/lock` to
`/run/lock`, `/run/shm` to `/dev/shm`, and the merged `/bin`, `/sbin`, `/lib` and `/lib64`), since
the kubelet mounts a path spelled through one where the link points: `/run/legion/roles` is
refused as `/var/run/legion/roles` is, with the refusal naming the spelling it reached, and
`/run/operator` is allowed as `/var/run/operator` is. A mount under `/home/legion` is allowed, and
reaches the role at its own path.

What a role writes without being told where is not on the node's disk: the issue's temporary
directory, its cache and data homes and bun's install cache are one set on the issue's volume,
every role of the pod told the same four, and the two Go caches are one pair on that volume shared
by the issue's roles (below). The one directory of that scratch named for a role is its state
home, `XDG_STATE_HOME=/legion/scratch/state/<role>` (`roleStateHome`, `manifest.go`), which every
role of the issue can still write (below), and its shim
makes Oh My Pi's profile directory under it, `omp/profiles/legion`, before Oh My Pi starts
(`podsafety.EnsureStateHome`; Oh My Pi reads the variable only where that directory already exists,
falling back to the profile's config root otherwise). The roles share the pod's network namespace,
the workspace path and the Oh My Pi profile path, and Oh My Pi's browser broker lock is an abstract
unix socket named from the lock path under its state root — with one state root that name would be
the same for every role, the first role's lock would block every other role's broker, and their
`browser.open` would fail (`Shared browser daemon unavailable`). `workspace-init`, which runs
alone, keeps the plain `/home/legion/.local/state` and the image's own cache and data homes;
`workspace-fetch` is told no XDG home at all, only a `TMPDIR` of its own on the pod's `tmp` memory
volume.

**One container, one fate.** Each role is judged by its own launcher connection's state: one role's
process exiting is that role's `gone` alone. The `roles` container ending — its PID 1 exiting,
`OOMKilled`, an eviction — ends every role of the issue: each is `gone`, its detail naming the
container's reason and exit code and quoting that role's own last 20 prefixed lines, then the
container's last 20 unprefixed lines, which are no role's (`roleLog`, `observe.go`), and the
supervisor relaunches each as it relaunches any `gone` role.

Every container carries a reservation, a cpu and memory limit with a request no greater than it,
and an ephemeral-storage limit over a smaller request: the `roles` container and both init
containers the issue pod's one reservation (`runtime.kubernetes.resources.issue`, or the daemon's
default). At the default every request equals its limit and the pod is `Guaranteed`; a
`cpu_request` or `memory_request` below its limit makes the pod `Burstable`. The pod is bounded on
the node's disk, and Legion sets no affinity on it (GKE Autopilot adds its own, below;
[Issue sizing](#issue-sizing-one-reservation-per-pod)).

The variables below are the ones a role's agent process carries, not the ones its container's pod
spec lists: the `roles` container's own spec carries a single variable, `POD_UID` from the downward
API (`packages/daemon/internal/runtime/sandbox/testdata/golden/worker.json` gives the `roles`
container an `env` of `POD_UID` alone). `launchEnvironment` (`manifest.go`) splits the rendered
environment in two — the kubelet-resolved entries go on the container, and the plain `NAME=value`
pairs travel in the daemon's start command for that role to `legion launcher`, which hands them to
that role's `legion worker-shim` and so to its agent (`launcherCommand`, `launcher.go`). So a
`printenv` run by `kubectl exec` into the container finds none of them but `PATH`, which the
worker image's own `ENV` sets (`packages/daemon/docker/worker.Dockerfile`) to the value the row
below documents, so every process of the container carries it however it was started; one the
role's own agent runs prints each of them the runtime sets, and exits 1 for the four it does not.
They are an issue pod's role's; `<generation>` is the role's own generation counter, which differs
between the roles of one pod and rises on each relaunch of that role in it, and every path under
`/var/run/legion` is the role's own directory, bound there in its namespaces.
`DISPATCH_TOKEN_FILE` and `ENVOY_TOKEN_FILE` are stated without
condition because configuration load refuses a `runtime: kubernetes` daemon that sets no
`envoy_token_file` and one that sets no `dispatch_url`
(`packages/daemon/internal/config/kubernetes.go`), and refuses a configured `dispatch_url` with no
`dispatch_token_file` (`missingDispatchTokenFile`, `packages/daemon/internal/config/stage3.go`),
so every pod carries both.

| variable | value | renderer |
| :--- | :--- | :--- |
| `PATH` | `/opt/legion/bin` first, then the image's own | `podPath`, `manifest.go` |
| `GH_CONFIG_DIR` | `/var/run/legion/gh/<owner>`, the issue's repository's owner's directory in the role's gh bundle | `ghAgentEnv`, `podkind.go` |
| `LEGION_GRANT_FILE` | the runtime sets it in no container: the `legion` tool mints the grants for its own daemon calls in-process | — |
| `LEGION_STATE_DIR` | `/var/run/legion/state` | `StateDir`, `names.go`; `manifest.go` |
| `LEGION_CREDENTIAL_HELPER` | the runtime sets it in no container | — |
| `DISPATCH_TOKEN_FILE` | `/var/run/legion/private/g<generation>/DISPATCH_TOKEN` | `manifest.go`, over `generationDir`, `names.go` |
| `LEGION_ROOT_WORKSPACE` | the runtime sets it in no container | — |
| `LEGION_WORKSPACE` | `/legion/workspaces/<owner>/<repo>/<key-lower>` | `podkind.go`, over `workspace.Location`, `internal/workspace/provision.go` |
| `LEGION_BOOT_TOKEN_FILE` | `/var/run/legion/private/g<generation>/LEGION_BOOT_TOKEN` | `launcherCommand`, `launcher.go` |
| `LEGION_TERMINATION_GRACE_SECONDS` | the runtime sets it in no container; the pod's grace reaches the shim as its `--stop-grace` argument | `launcherCommand`, `launcher.go` |
| `ENVOY_TOKEN_FILE` | `/var/run/legion/private/g<generation>/ENVOY_TOKEN` | `manifest.go` |

`<owner>`, `<repo>`, `<key-lower>` and `<generation>` are the pod's own: the owner and repository
of the issue's repository, the issue key in lower case, and the role's generation counter. The
daemon-launched controller's container differs: of these eleven names its agent carries no
`LEGION_WORKSPACE`, because it is told no tree, issue or checkout (`controllerPod.agentEnv`,
`podkind.go`), and its `GH_CONFIG_DIR` is its bundle's first owner's directory, because its launch
names no repository and the default is then the project's owners sorted and the first taken
(`appauth.GitHubCredential`, `credential.go`);
[Daemon-launched controller](#daemon-launched-controller) has the rest of it.

Every role is told `LEGION_GH_OWNERS_DIR=/var/run/legion/gh`, the bundle's root (its own, bound
there in its namespaces),
`GH_CONFIG_DIR` at the issue's repository's owner's directory under it (the table above), and
`LEGION_GH_PATH=/usr/local/bin/gh`, the image's real gh. The `gh` first on its PATH is
`/opt/legion/bin/gh`, which runs `legion gh`: it runs the real gh with `GH_CONFIG_DIR` at the
directory of the owner the command targets, read as gh reads it from the real gh's own help (see
docs/deployment.md, "What a pane's gh, git and jj are"), and at the issue owner's when the command
names no repository at all; a command it cannot read for certain, one naming two owners, and one
naming an owner the bundle holds no directory for are refused with exit 2 and send nothing. `git`
reads the same files through `!legion git-credential`
(`workspace.GitHubCredentialHelper`) with `credential.useHttpPath` on, so the helper answers from
the `hosts.yml` of the owner in the path git asks about and from nothing else. The shared clone
names it as its `credential.helper`: `workspace-init provision` is passed it as
`--credential-helper '!legion git-credential'`. Every role is also told it outside any
clone, as five git configuration entries git reads after every configuration file, the clone's
own `.git/config` among them (`gitConfigPairs`, `manifest.go`): `GIT_CONFIG_COUNT=5`,
`credential.https://github.com.helper` reset to empty and then set to `!legion git-credential`,
`credential.https://github.com.useHttpPath=true`, `core.fsmonitor` set to empty and
`core.hooksPath=/dev/null`. The issue's roles share the clone and each can write its
`.git/config` and hooks, so the last two keep a role's `git status` or `git commit` from running an
fsmonitor program or a hook another role planted, with the credentials of the role that runs git.
They pin those two settings and no others, and they do not make the roles of one issue a boundary
against each other. Every role can write the whole issue volume, and the roles share the pod's
network namespace, so code one role plants runs in a sibling, with the sibling's credentials,
through any of these:

- the checkout, which the roles share by design: a test the tester runs, a script or build step the
  reviewer runs;
- the clone's other git settings that run a program or route a request: a filter driver
  (`filter.<name>.clean` with `.git/info/attributes`), `diff.external`, a `textconv`, `core.pager`,
  `gpg.program`, an `ext::` transport, and `http.proxy`, which sends the sibling's GitHub token
  through a proxy the planting role runs, with no program of its own run in the sibling at all;
- the clone's `.jj/repo/config.toml`, which a sibling's jj moves into its own config home and obeys
  (`ui.pager`, an alias);
- the workspace's `.omp/` directory, whose extensions a sibling's Oh My Pi loads when it starts;
- a role's installed plugins: an installing launch loads the plugin pair from
  `/legion/scratch/state/<role>/legion-plugins/g<generation>` on the issue volume, which every role
  of the issue can write;
- the caches every role shares on the issue volume, which `go run`, `uvx` and `bunx` run code
  from: `GOMODCACHE` and `GOCACHE` (`/legion/go`), uv's cache and Pythons (`/legion/uv`), bun's
  install cache, and `XDG_CACHE_HOME` and `XDG_DATA_HOME` (`/legion/scratch`);
- the one temporary directory every role's agent is told, `TMPDIR=/legion/scratch/tmp` (below):
  a script, an archive or a patch a role writes there to run or read a moment later is a file
  every sibling can replace first, and a tool that names a file in it by a fixed name reads
  whatever is there;
- the session transcripts: under `postgres` they are rows, and the database's URL is in the
  providers mount the container holds for the pod rather than for a role, so every role of the
  issue reaches them there. Under `cloudsql-iam` each generation's connection is private to its
  role (the socket is in that role's private directory), but every role signs in as the pod's one
  IAM login, whose rights reach every session row, so any role that opens a connection of its
  own reads and writes a sibling's transcript as well ([Session store](#session-store)).

The issue's roles trust one another's writes to the volume as they did when each ran in a container
of its own. The namespaces keep a role's tokens and private files out of a sibling's own reads, and
no further. Through the helper, every git a role runs, the setup
command's ([Configuration](#configuration)) and an agent's own clone of another repository
included, answers `https://github.com/<owner>/…` from the role's own files when `<owner>` is an
owner the bundle holds a directory for (one of the project's) and the role's App is installed on
that repository; git in the shared clone asks the same helper it asked before, and every other
host is left to the configuration files. The `legion` in the helper resolves on the PATH of
whichever process runs git. `GH_TOKEN`, `GITHUB_TOKEN` and
`GH_HOST` are set to the empty string, which gh ignores, so no value the image or an operator's rc
file leaves in the environment outranks the files. No container is told `LEGION_GIT_PATH` or
`LEGION_JJ_PATH` (for the credential helper and the grant file, see the table above), and nothing
is installed under the issue's volume for a pod to run: a pod's `git` and `jj` are the image's,
resolved on its PATH. The daemon renders the gh keys
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

Every role is told where to put what it writes without being told where, and every one
of those values names a path on the issue's volume. Four of them are the issue's, one directory
each that every role of it is told the same, under the issue's scratch `/legion/scratch`
(`scratchRoot`, `manifest.go`): `TMPDIR=/legion/scratch/tmp`,
`XDG_CACHE_HOME=/legion/scratch/cache`, `XDG_DATA_HOME=/legion/scratch/data` and
`BUN_INSTALL_CACHE_DIR=/legion/scratch/bun/install/cache` (`roleEnvironment`, `manifest.go`). A
directory per role is what those four replace, by the human's decision of 2026-10-20 (specification
version 20): the issue's roles already share the pod, the pod's disk whole, the one workspace
they all work in, uv's Pythons and cache and the two Go caches, and each
can read and write what the others write, so a cache per role bought an isolation the pod does not
have and cost a copy of every cache per role. `XDG_STATE_HOME=/legion/scratch/state/<role>` is the
one directory of the scratch named for a role (the state home above), and a sibling can write it
too. The two Go caches
are the issue's as they already were, one pair every role of the issue is told:
`GOCACHE=/legion/go/build` and `GOMODCACHE=/legion/go/mod` (`goBuildCache` and `goModCache`,
`manifest.go`), beside the workspaces rather than under the scratch, on the same volume beside
uv's cache. Both hold content addressed
by its hash, and the roles of one issue may fill them at once: of the build cache `go help cache`
says "The cache is safe for concurrent invocations of the go command", and what reaches the
module cache is a module zip or `go.mod` the go command has verified against a known hash
(`go help module-auth`) into a directory it then makes read-only, which only
`go build -modcacherw` leaves writable. A pair is 751 MiB of build cache and 375 MiB of module
cache for this repository, so six roles with a pair each would hold five redundant copies, about
5.5 GiB of a 50 GiB volume spent on nothing. It was proven live in an issue pod: two
`go build ./...` at once against one empty shared pair both exited 0 in 227 s, filling 751 MiB of
build cache and 375 MiB of module cache — two concurrent builds of one module leave one build's
worth of content in the shared pair, which is why sharing is both safe and cheap — and the cache
that race left rebuilt and tested clean. What those figures measure is one build: a cold
`cd packages/daemon && go build ./...` into an empty pair — `packages/daemon` alone, one of the
two modules `go.work` names, with nothing of `packages/envoy` built and no test run. A second,
separate run of it measured 752 MiB and 375 MiB in 114 s, exit 0. A role that runs tests leaves
more: after `go test -count=1 ./...` over that same one module the pair holds 1.3 GiB of build
cache and 931 MiB of module cache, measured in this issue's own container, and that after-tests
pair is the one the sizing above does its arithmetic with, because roles run tests. Bun's install
cache is one of the four the issue's roles share, and sharing it is what bun's own behaviour
asks for: bun hardlinks out of its install cache into whatever
it installs for, and the issue's roles share one checkout (`workspace.Location`,
`packages/daemon/internal/workspace/provision.go`, names the repository and the issue, never a
role), so a file edited in place inside `node_modules` is already in the packages every role
runs from, whatever we do about caches; what a cache per role confined was afterwards — only that
role's later installs would put the changed copy back (LEGION-198, where bun's hardlinking was
first met). The state home is the one that stays the role's, and for a mechanism rather than
caution: Oh My Pi names its browser broker's lock from the state root, the name is an abstract
unix socket, and an abstract socket name is one name across every process that shares a network
namespace, which every role of a pod does, so one state home would block every role's browser
broker but the first (above).

Sharing the cache home gives the issue's roles one Playwright browser registry, since
playwright-core resolves it to `$XDG_CACHE_HOME/ms-playwright`, and `Registry.install()` takes an
exclusive lock on that directory with about ten minutes of back-off, so a second role installing
at the same time waits rather than races. Two things follow and are worth knowing before meeting
them: an install still waiting when the back-off is spent fails with a lock error, and
`playwright install --force`, which removes a current installation before writing the new one, can
remove a browser tree another role of the issue is running from.

Sharing `BUN_INSTALL_CACHE_DIR` gives them one bun install cache, which no lock of bun's own
guards: filling a cold entry of it from several roles at once races. Six concurrent
`bun add --backend=copyfile --exact chalk@5.3.0 lodash@4.17.21` into six separate directories
against one empty cache left one exit 0 and five exit 1 with
`ENOENT: failed copying files from cache to destination`, measured twice in this issue's own
container on bun 1.3.14, where the same six against the cache they had just filled all exited 0 —
the race is the cold fill of an entry, not the install out of it, and `--backend=copyfile` governs
only how files leave the cache. The install Legion renders for an installing launch takes
`/usr/bin/flock` on the cache directory, after a `mkdir -p` of it so the lock's inode exists before
anything fills the cache, and retries the install once (`installBlock`,
`packages/daemon/internal/runtime/sandbox/plugins.go`), so a plugin-version bump, which makes the
pair cold for every role of a kept pod at once, does not fail their launches. That wait is
bounded (`installLockWait`), since `flock` otherwise waits forever and one stalled install
would be charged to every role of the pod: the bound is a third of the registration deadline that
configuration gives — `worker_boot_timeout_seconds` times
`worker_boot_registration_deadline_intervals`, so 120 seconds at the defaults — floored at one
second, since a wait that rounded down to zero would render `-w 0`, which is `-n`: no wait at all,
and the cold-fill race back. A proportion rather than a fixed number because it has to hold at
whatever the operator sets: the script runs the install twice, so two waits take two thirds of the
deadline and the remaining third is left for the install and the agent's start, where a constant
sized for the defaults would be two waits of 240 seconds against
the 180 second deadline of a halved boot timeout and could never fire before the deadline retired
the role. That remainder holds at every configuration but the smallest, where the floor takes
over: under a three-second deadline two floored waits of a second consume it, deliberately, since
being serialised and then retired is no worse than racing — an unserialised cold fill fails the
launch anyway. Above that floor the bound is also long enough for the last of six roles to queue
behind five serialised
installs of the pair, 24 seconds each at the defaults — the half of the bound a smaller divisor
would break, which `TestTheInstallLockWaitIsABoundInProportionToTheRegistrationDeadline` holds
against the installs rather than against the derivation. A wait that
times out exits 1, so it takes the retry and then the script's named failure — that one bump's
launch fails the way it did before the lock existed, rather than hanging. `flock` is advisory
and per-inode, which holds exactly where this needs it — every role of one pod shares a
kernel and that inode on the issue's volume — and nowhere wider, so two pods would not be covered.
Two agents of one issue running `bun install` at once against a cold shared cache can fail the
same way, nothing Legion renders wraps those, and the failure is a failed install with a clear
error rather than a corrupted one — bun stops, it does not half-write the workspace — so a second
run succeeds against the cache the first filled. That is why an agent's own install is a
documented consequence and the install Legion renders was a blocker: nothing is there to retry
that one.

`XDG_CONFIG_HOME` is the one XDG home not on the volume: every role is told
`/home/legion/.config`, where its namespace binds its own config home, the copy on `role-homes` of
the pod's shared in-memory one that holds the jj configuration `workspace-init` writes (above), and
`workspace-init` keeps every value it had.
The other paths are all under the issue volume's own whole-volume mount at `/legion`: the
scratch adds no volume and no mount, and no directory of it is a `sub_path` mount of its own. The
roles of an issue share one scratch, the state homes under its `state` directory the only part of
it named for a role, and the role's shim makes six directories — the four the issue's roles share,
plus the shared Go pair — before Oh My Pi starts (`podsafety.EnsureScratch`), since a role told a
temporary directory nothing created cannot run; `podsafety.EnsureStateHome` makes Oh My Pi's
profile directory under the state home.
What puts them off the container's own filesystem is the ephemeral-storage bound: on GKE Autopilot
an issue pod is admitted with at most 10Gi for all six roles together (Warden caps a pod's main
containers' summed ephemeral-storage requests at 10Gi and clamps each container's limit to its
request; [Issue sizing](#issue-sizing-one-reservation-per-pod)), while
ordinary work measurably needs more: this repository's Go build cache 751 MiB and its Go module
cache 375 MiB (the measurement above), a Playwright Chromium about 400 MB, a Postgres with a data
directory about 300 MB, and a cold bun install cache 1.4 GB. A pod past its
ephemeral-storage limit is evicted whole by the kubelet, and all six roles of an issue
run in its one container, so one role's overrun takes every role of that issue down with it — which is
what those caches did on GKE Autopilot. The disk is the pod's alone and dies with it, so what a
pod downloads and builds serves that pod's roles and no later one. The role's `$HOME`
stays off the disk: it is an overlay of the image's own content (`~/.omp`, its profile's
plugins, natives and agent directory), which every pod must keep reading, with the role's writes
on the pod's `role-homes` disk `emptyDir`, so a recipe that writes somewhere else under the home
directory is still bounded by the pod's ephemeral-storage limit.

Every role is told `UV_PYTHON_INSTALL_DIR=/legion/uv/python/<issue>` (the issue key as a
DNS label), `UV_CACHE_DIR=/legion/uv/cache` and `UV_LINK_MODE=copy`. uv keeps the Pythons it installs
and its cache on the pod's own disk beside the workspace, so a project's `.venv`, which links to
its interpreter there, runs as it is for every role of the pod that made it; a later pod of the
issue installs its own. The disk is that pod's alone, so no other issue's pod ever installs into
either
directory, at the cost of one download per issue. uv copies each package from the cache into a
`.venv`: with the cache and the `.venv` on one filesystem it would otherwise hardlink them, and an
edit made in place inside the `.venv` would change the cache and every later install from it. So each
`.venv` is a full copy of its packages on the pod's disk, beside the cache, and a deployment
sizes `issue_volume` for the clone, the workspace, the issue's Python and one copy of its packages,
and for the workspace's CodeGraph index (`.codegraph/`, 185 MB for this repository). `uv cache clean`
and `uv cache prune` remove cache entries under a lock that stops at the pod; the disk is the
pod's own and no other pod uses uv on it, so either may run whenever the issue's roles are idle.

Each role's shim is started with `--warm-codegraph`: once its Oh My Pi has written its first frame —
its extensions loaded, its RPC loop serving — the shim builds the workspace's CodeGraph index in the
background (`codegraph init`, or `codegraph index` to repair one an earlier build left partial, as
`codegraph status --json` decides). Nothing waits on it: not the launch, not the role's registration,
not a prompt. Six role shims share one workspace, so the warm-up holds a lease at
`.codegraph/legion-warm.lock` for its run — an exclusive create whose holder refreshes its mtime
every 10 s, taken over once it is 60 s stale, the takeover itself under a second exclusive create
beside it so two shims that both read one stale lease never both build — and a shim that finds the
lease held leaves the build to its holder. When a build finishes, the shim's warm-up logs one line in
the `roles` container's log, prefixed `<role>: `, `codegraph warm-up for <workspace> built the index: codegraph init took
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
never told a live build holds the workspace; it runs the warm-up again, and `status` on the index
that stop left on the pod's disk answers complete, so nothing runs, or reports it partial, which
`codegraph index` repairs. A relaunch in a new pod finds no index and builds one, as the issue's
first pod did. The tester's `affected` and the reviewer's `impact`/`callers` queries
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
  limit (the two quantities the kubelet's QoS class reads); `Burstable` when its `resources` key
  (`issue` or `controller`) sets a lower `cpu_request` or `memory_request`; and its ephemeral
  storage a request under a larger limit ([Issue sizing](#issue-sizing-one-reservation-per-pod),
  "The node-disk bound");
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
no tree or issue label, runs no init container and one launcher container, `controller`, carrying
the controller's reservation, and owns a volume of its own the same way,
`issue-legion-<project>-controller` ([Daemon-launched controller](#daemon-launched-controller)).

The image probe runs as a Sandbox of its own, `legion-probe-<project>-<digest12>`, with
`shutdownPolicy: Delete`, its one container carrying the probe's own fixed reservation — 250m and
1Gi, over a 5Gi disk bound and a 1Gi disk request (`probeReservation`,
`internal/daemon/kubernetes.go`), none of an issue pod's, since `legion probe-image` starts one Oh
My Pi at a time and runs no lane or browser, and a probe sized as an issue pod could keep the
daemon from
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

The process clones the repository once, through the same one-shot credential as an issue pod's
fetch but, the first time, within `workspace.CacheFetchTimeout` (2 hours) rather than an issue pod's 30 minutes —
its first clone is the repository's one full download, and on 2026-10-26 one deployment's cache with one CPU
under gVisor was killed at 30 minutes 289 times running, never ready — then fetches every branch
from GitHub every `refresh_minutes` (10 by default), pruning the branches GitHub deleted and keeping
what each fetch brings as a pack. Once a day, after a fetch, it repacks the clone
(`git repack -a -d`, within 2 hours, `repackTimeout`, and with git's memory bounded, `repackMemory`: a full repack of a 3.2 GB clone peaked at 4.35 GiB unbounded, past the cache's 4Gi limit, and at 1.78 GiB bounded), holding no lock a tar takes: a tar is
served throughout and is taken again if the repack swaps the packs under it, and only the next
fetch waits. A repack that fails is logged and tried again a day later, and the partial files it
left in `objects/pack` (every `tmp_*` — a pack, its index, reverse index, bitmap, mtimes — and
`.tmp-*`) are removed at once, each logged, as they are after a fetch that fails; a process
starting on a volume where a killed one left any removes them before its first fetch. A fetch into
the clone keeps an issue pod's 30-minute bound, since every tar waits for it; git's transfer sets no
stall limit, so a stalled fetch holds every tar for up to those 30 minutes. Restarted after a fetch
was killed, it also removes the git lock files that fetch left before it fetches again. As its
container's PID 1 it reaps, after each refresh, the processes git's helpers leave it when a git
dies (`git remote-https`, `git index-pack`). It serves, on port 8418:

- `GET /readyz`: 503 until the first clone is whole, then 200, with nothing else; the pod's
  readiness probe, which carries no token.
- `GET /healthz`: `503 {"ready":false,…}` until the first clone is whole, then `200 {"ready":true,
  "repo":"<owner>/<repo>","head":"<default branch's commit>","lastFetchAt":"…","bytes":<n>,
  "streams":<s>}`, `streams` being the `/tar` requests it is sending or holding in its queue at the
  answer; it waits on no fetch, and answers any request, since it holds nothing secret and nothing
  of the repository's content.
- `GET /tar`: the clone's refs and packs as a tar stream — `HEAD`, `packed-refs`, `refs/`,
  `objects/pack/pack-<id>.pack` and the `objects/pack/pack-<id>.idx` git reads each pack through,
  no loose object or configuration — which waits for a fetch under way to finish, so it never
  streams a half-written ref or pack, and waits for no repack. At most 32 streams are sent at once
  (`maxStreams`, `internal/repocache/server.go`); a request past them waits, first come first
  served, for one to end, within its own deadline. Streams share the cache pod's sending rate
  evenly, so without the bound a large launch slows every stream at once until none finishes: on
  2026-10-26 one cache sent a 4.0 GB clone (51 packs) at about 400 MB/s in all over 290 streams,
  about 48 minutes before any reached its last byte, past the fetch's 30-minute `FetchTimeout`, and every
  fetch failed and started over. Bounded, each stream takes its share among 32 and the queue
  drains at the same total rate.

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
presents the cache's token with its request for the tar alone. It holds every pack and index it
received to the checksums git wrote into them — each file's own trailing hash over its bytes, and
the pack hash the index records, which must be that pack's — and checks every object the refs
reach is there (`git fsck --connectivity-only`) before the feed is used, and a damaged or
incomplete history fails the fetch. A pack that arrived without its index is indexed here instead
(`git index-pack`). Re-indexing every pack the cache wrote is what the feed no longer does: it inflates
and rehashes every object, 8 to 12 minutes per issue pod on a 3.2 GB cache. The feed takes the index's
names on the cache's word instead, which trusts every writer of the cache's indexes: `git index-pack`,
which named every object by its content when `git fetch` received the pack from GitHub; the daily
`git repack -a -d`, whose `pack-objects` writes new indexes from names it already holds without
hashing the objects again; the cache pod's volume; and whoever answers at the cache's address, since
the bearer token proves the pod to the cache and nothing proves the cache to the pod. An index whose
checksums hold but which maps an object's name to another object passes the fetch's checks, and a
checkout of a blob it misnames writes the other blob's content without an error, which jj in the
issue's workspace does not record as a change in a repository of realistic size. The daemon checks
that the cache's Service is the cache's own once, before it tells a pod the address (below).

From GitHub the fetch then takes, by exact name, the default branch GitHub names now and the
default branch the daemon recorded for the issue while GitHub has it (`LEGION_DEFAULT_BRANCH` on the
fetch container), and `refs/heads/legion/*`, pruned within those. Before that delta it reads every
branch GitHub has with its tip (one `git ls-remote --symref <url> HEAD refs/heads/*`) and deletes
every other branch of the feed that is not exactly GitHub's tip, on a fresh feed and on one a retry of
the init container finds on the pod's disk alike, logging each with its reason (`removed <branch> from the feed: GitHub no longer has
it`, or `…: GitHub has moved it, and this fetch brings only the default branch and the issue
branches`). So after a fetch every branch of the feed is GitHub's as it stood then, or absent: the
issue's clone is made from the recorded default branch and the issue's own branch, both current, an
issue branch GitHub deleted is gone, and an agent that wants another branch as GitHub has it now
fetches it from GitHub through its clone's `origin`. A fresh feed streamed from a cache that is
behind loses, at that point, every branch outside the delta that moved since the cache's last
refresh; nothing provisioning reads is among them. A large branch pushed between the cache's refresh
and a wave of launches is never pulled into every issue pod at once. A recorded default GitHub no
longer has leaves the feed too, and provisioning refuses an issue with no branch whose recorded
default is gone rather than starting it from an old tip.

A cache that is not ready is waited
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
ended, or was left `Suspended`, or whose Sandbox's pod template reserves other than
`runtime.kubernetes.repository_cache.resources` now says, gets a new pod while its Sandbox and
volume stay, so a new release or a new reservation costs a pod, not a clone. The reservation is
read from the Sandbox's pod template, never from the pod, since GKE Autopilot rewrites a pod's
requirements at admission. A pod still running is replaced only once it serves no stream: the
daemon asks its `/healthz` first, whose `streams` counts the tars it is sending or holding in its
queue, and while that is above 0 it logs `repository cache pod is replaced once it serves no
stream` and leaves the pod to a later sweep, because the replacement would end every one of those
tars, failing each fetch reading one and making its issue pod again. A cache of an earlier release
answers no `streams`, and one whose `/healthz` cannot be read is replaced at once, as before. A
launch whose cache cannot be made fails, naming why. The cache of a
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

- **The stream** is read from the `roles` container's launcher command in the pod spec, before the
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
replaces follows from what moved. A pod whose launcher dials a stale stream, or which lacks the
broker enrollment its roles are started against now, cannot run them as a pod created now would,
and every role of it is reported at once, from the boot that re-adopts them: the first of them
relaunched replaces the pod, under the pod's launch turn, and the others, whose relaunches wait on
that turn, resume into the new pod, whose launcher dials the current stream for every role and mounts the
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

### A shell on an issue's disk

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
The clone is only on the pod's own disk, so the commands run in that pod, from the operator's
context: the daemon's identity creates no pod and has no exec
([RBAC](#rbac-the-go-daemon-needs)).

**The issue's pod is running.** Its `roles` container mounts the disk at `/legion`:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY> --field-selector=status.phase=Running
kubectl -n legion exec -it <pod> -c roles -- sh
export XDG_CONFIG_HOME=/var/run/legion/home/.config   # the pod's config home (below)
export XDG_STATE_HOME=/legion/scratch/state/architect   # the role's state home (below)
jj bookmark list --all-remotes legion/<KEY> -R /legion/repos/github.com/<owner>/<repo>
```

The shell runs as the issue's agents do, user 1000 under gVisor, but it carries the container's
environment, not any agent's: the `roles` container's spec holds only what the kubelet resolves, which
is `POD_UID` from the downward API, and every other variable the agent is told — the operator's
`runtime.kubernetes.pod.env` included, and among them
`XDG_STATE_HOME=/legion/scratch/state/<role>`, the role's state home — arrives in its
launcher's start command (`launchEnvironment`, `manifest.go`). The first `export` points jj at the
pod's shared config home, where `workspace-init` wrote the clone's per-repository configuration
(`git.abandon-unreachable-commits = false` and no repository identity), and which no role writes:
the `roles` container mounts it at `/var/run/legion/home/.config`. A role's own `~/.config` is a
copy of it made in its namespace (`/var/run/legion/roles/.homes/<role>/config` here, holding what
that role wrote since). The shell's own `~/.config` is the image's, empty, so without the `export`
the shell's jj reads it, says `Per-repo config not found`, and reads
jj's default `git.abandon-unreachable-commits = true`, the setting Legion turns off. jj needs only
that variable, so the shell's `HOME` stays the image's `/home/legion`, which carries no mount in the
`roles` container: an `omp` started there reads the image's own `~/.omp` profile, its plugins and
natives, as a role's does through its overlay, where `/var/run/legion/home` holds only the
container's mounts and no profile. The second `export` sets the state home, so
an `omp` started in the shell roots its state where the role's agent does rather than under the
profile's config root; for the agent's whole environment, read `/proc/<agent pid>/environ`, the
process whose `argv[0]` is `omp` under the role's `legion worker-shim`, as stage 4a's `pod-baseline`
does (`xargs -0 sh -c 'exec env -i "$@" omp …' agent-env </proc/<pid>/environ`). The clone is at
`/legion/repos/github.com/<owner>/<repo>` and the issue's workspace at
`/legion/workspaces/<owner>/<repo>/<issue key lowercased>` (`workspace.Location`,
`internal/workspace/provision.go`); no other issue's workspace is on this volume.

**No pod of the issue is running.** Then there is nothing to reach. The issue's disk was its
pod's own and went with the pod ([The disk dies with its pod](#the-disk-dies-with-its-pod)), so
no claim is left to mount, and the clone, the workspace and everything the issue's roles wrote
are gone with it. A refusal above is worked in the pod that raised it or not at all: a pod whose
`workspace-init` is failing runs no role container to exec into, and the remedy there is to let
the pod be replaced. The runtime reads a failed init container as the end of every role of that
pod (`InitFailed`, `internal/runtime/sandbox/observe.go`) and the next launch replaces the pod
outright instead of waiting on its launchers (`relaunch.go`); the replacement provisions a fresh
clone and workspace from the issue's pushed branch, on a disk of its own. Nothing is saved first,
because a shell on the old disk could carry nothing over.

**The one stopped disk still worth mounting: a tree volume of the release before issue pods.**
That release's tree volume is its root Sandbox's, not a pod's, so it outlives every pod of the
tree, and copying the sessions off it is the one procedure that still needs a stopped disk
reachable: step 3 of [Copying file sessions before turning it
on](#copying-file-sessions-before-turning-it-on).
`scripts/sessions-import-pods.sh` makes one such pod per tree itself; the pod below is that step
run by hand for a single tree, with `legion sessions import --dsn-file <url file> --claims
claims.json --tree <ROOT ISSUE> --tree-volume /legion` run inside it. The tree's claim is the one
its labels name (`kubectl -n legion get pvc -l
legion.dev/project=<project>,legion.dev/tree=<ROOT ISSUE>`). By that step of the runbook the
tree's root Sandbox is `Suspended` with no pod of its own and the earlier release's daemon is
stopped, so nothing launches a pod that would want the volume while yours holds it; the claim is
`ReadWriteOnce` and this pod mounts it read-only, which the import is:

```yaml
apiVersion: v1
kind: Pod
metadata: { name: legion-tree-shell, namespace: legion }   # no legion.dev/* labels
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  nodeSelector: { legion.dev/pool: legion }
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000             # the tree's own pods ran as it: its files are group 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: shell
      image: <runtime.kubernetes.image>      # the daemon's worker image, by digest
      command: [sleep, "3600"]
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: tree, mountPath: /legion, readOnly: true }]
  volumes: [{ name: tree, persistentVolumeClaim: { claimName: <the tree's claim above> } }]
```

```sh
kubectl -n legion apply -f legion-tree-shell.yaml
kubectl -n legion wait --for=condition=Ready pod/legion-tree-shell --timeout=300s
kubectl -n legion exec -it legion-tree-shell -- sh
kubectl -n legion delete pod legion-tree-shell
```

The pod carries no credential. An agent of that tree could plant git and jj configuration in a
clone on the volume ([Trust model](#trust-model-the-provisioning-token)), which a `jj` run in this
shell would obey, and the pod gives that configuration nothing to take. Run no command in it that
holds a token.

### Running a workload in the cluster from a role

A role sometimes needs something *running* rather than installed — a database for a test, a
broker, a second copy of an application under test. Its own pod holds a server only as a test
harness's own rootless install at the pod's fixed reservation, which is how the Dispatch browser
suite's Postgres runs today
([The tools a role's own work needs](#the-tools-a-roles-own-work-needs)); a server image, or more
processors than the pod reserves, it cannot hold, and the worker image cannot carry a running
server, so a role starts one in the cluster as a Job and reaches it by a
Service name. What makes that possible is the deployment's, not Legion's: the ingress rule that
admits connections from the namespace Legion's pods run in, the grant on Services and the
admission rule that holds them, and the quota that counts them are declared in the deployment's
own repository and applied by a person (dispatch://INFRA-206). This section is what a role does
where they exist. Every rule below is that issue's and this issue's specification
(dispatch://INFRA-71), each one stated as the condition the namespace's admission enforces; a
workload that breaks one is refused at submission with a message naming the rule it broke, so
nothing here has to be taken on trust — with the one exception this section marks where it
arises, the reachability label, which admission does not check and whose absence is a workload
that starts, costs its quota and answers nothing.

**A role has this only where its deployment gives it.** Every Legion pod sets
`automountServiceAccountToken: false` (`AutomountServiceAccountToken`,
`internal/runtime/sandbox/manifest.go`), so a role holds a Kubernetes credential only where its
operator projects one through `runtime.kubernetes.pod.volumes`
([Configuration](#configuration)); the image carries the client and no credential for it
([Worker image](#worker-image)). Three answers tell a role which deployment it is in, and they
are the gate for everything below:

```sh
kubectl -n <namespace> auth can-i create jobs      # may this role start a workload at all
kubectl -n <namespace> auth can-i create services  # may it give one a name on the network
kubectl -n <namespace> auth can-i get services     # `kubectl apply` reads the live object before it creates one
```

The third is not a duplicate of the second. The sequence writes its Service with `kubectl apply`,
which is client-side by default and reads the object it is about to create before it creates it,
so a role that may create a Service but not read one gets `Error from server (Forbidden): error
when retrieving current configuration of: … cannot get resource "services"` and never reaches the
create. Both verbs are needed for the sequence as written, which is why the gate asks for both. A
deployment that granted the create without the read would still admit a `kubectl create` of the
same file; this section publishes no second form for that case, and no deployment answering that
way has been measured — in this one, before dispatch://INFRA-206's declarations were applied, both
Service answers were `no`.

None of the three is the answer a role with no credential gets. The image ships no kubeconfig
and sets no `KUBECONFIG`, so without a projected token `kubectl` builds no in-cluster
configuration at all: it falls back to `localhost:8080` and **exits 1**, its last line `The
connection to the server localhost:8080 was refused - did you specify the right host or port?`
(measured from a role container with the in-cluster environment removed). That is as conclusive
as `no` — nothing in this section applies, and a tool the work needs on the issue's own files
arrives the way it already does, below. A `yes` to the first and a `no` to either of the others
is a deployment where the sequence as written does not run: a workload can be started there, and
the sequence cannot give it a name to be reached by.

**Unless all three commands answer a bare `yes`, submit nothing.** The sequence submits its Job
before its Service, and the Service step is refused `Forbidden` wherever either Service answer is
`no`, so a refusal there finds a Job already submitted, and the sequence's own trap deletes it.
Measured on 2026-10-25 with a Service step the namespace's admission refused — a refusal of
another kind that ends the step the same way, with `kubectl apply` failing — the trap deleted the
Job within six seconds of its submission, while its pod was still `Pending`, and nothing of it was
left. A role whose shell went away before the trap ran removes it with the recovery line under
the sequence.

`<namespace>` is the namespace the workload runs in, and each command names it because a pod's
`kubectl` has no kubeconfig and takes its default namespace from the pod instead: `POD_NAMESPACE`
when that is set, else the `namespace` file the projected credential carries
(`/var/run/secrets/kubernetes.io/serviceaccount/namespace`), else `default`. Without `-n`, the gate
answers for whatever namespace the pod's projection names, which a role cannot see from this page
and cannot assume is the workload's. In a pod projected as this deployment projects one, that file
names the workload's namespace, so the form without `-n` answers as `-n <namespace>` does, which is
why it reads as working; the same account asked with `-n` the namespace Legion's own pods run in
answers `no` to all three (measured from a role container on 2026-10-25). The sequence below
passes no `-n` and runs in that default namespace, so the gate's answers hold for it only where
the default is `<namespace>`.

The sequence has two sources for its namespace, and they agree only where `POD_NAMESPACE` does
not say otherwise. Its `kubectl` commands, the recovery line's included, take that default,
`POD_NAMESPACE` first; its reachability address is built from the `namespace` file alone. Where a
pod set `POD_NAMESPACE` to another namespace, the Job and its Service would be made in one and the
check would dial a name in the other, with no Service behind it. That gap is latent, recorded here
rather than closed: Legion's runtime sets no `POD_NAMESPACE` (nothing in `packages/daemon` names
it), an operator's `runtime.kubernetes.pod.env` could, and it was unset in the role container the
runs of 2026-10-25 below came from.

The rules the workload must satisfy. Every one but the last is enforced by the namespace's
admission rather than by anyone's good behaviour; the last is enforced by nothing, which is why
it is marked where it stands:

- it runs under gVisor (`runtimeClassName: gvisor`) on the compute class the deployment's
  declaration names, selected by `nodeSelector`; that class creates a machine rather than falling
  back to a general one, so a small workload waits about eighty seconds to start;
- its largest container requests at least four processors — a smaller job belongs in the role's
  own pod;
- it bounds itself **twice**: `activeDeadlineSeconds` on the Job *and* on its pod template, each
  at most twelve hours, with `ttlSecondsAfterFinished` on the Job at most twenty-four, so the Job
  and its pod go on their own and nobody may orphan one. Each of the three is a refusal of its
  own: a manifest that sets the deadline on the template alone is refused as plainly as one that
  sets neither;
- it carries no identity: `automountServiceAccountToken: false`, and no service account of its
  own;
- it mounts only an empty directory, the downward API, or an ephemeral filesystem volume of the
  scratch storage class the deployment's declaration names, whose claim template carries no
  labels, annotations, data source, volume or selector. **No disk outlives the workload**, which
  is what a database a test fills and discards needs;
- it carries the pod label the deployment's declaration names for a workload that asks to be
  reachable. **This is the one rule admission does not enforce**: a workload without that label
  is admitted, started, charged against the shared quota, and reachable by nothing, because the
  ingress rule selects the label rather than the namespace. Nothing refuses it and nothing says
  so; the symptom is the reachability check below ending at its deadline with `TimeoutError`
  every try — a timeout, not a refusal. A refusal is a workload the packets do reach: still
  starting in the first seconds, nothing listening on the port if it lasts the whole wait, as the
  readings under the sequence set out. It is why the check is the step after the wait rather than
  the last. A role that sees it compares the labels on **the workload's** pod, which the
  sequence's trap prints before it deletes anything (`--show-labels`; a pod of this namespace — a
  role may not read its own, in the namespace Legion's pods run in), with the deployment's
  declaration — not the Service's endpoints, which this identity may not read (`kubectl auth
  can-i get endpoints` answers `no`). The label's name is in that declaration, with the compute
  class, the storage class and the namespace; a role reads it there rather than here.

Name the workload for the issue **and for the run**, as everything else on this cluster is named
for what owns it (`issue-legion-<project>-<issue key lowercased>`,
`legion-<project>-<issue key lowercased>` above). The issue is the name's prefix,
`<issue key lowercased>-<what it is>-`, which the Job's manifest carries as
`metadata.generateName`; the run is the API server's part, five random characters it appends at
each create, answering with the whole name — for instance `infra-71-postgres-` followed by five
such characters — which the sequence reads from that answer and writes into its Service. The issue
key carries its project, so the name cannot collide with another project's. The rest is the
server's for a reason: a name a role picks itself can be one that something else in the namespace
already holds or will declare, and a Service takes its Job's name, so the Job's name is taken among
the Services too, and the first anyone knows of it is another stack's apply failing
`AlreadyExists`. A name the API server generates is not one any declaration chose: it exists from
the create that made it. `kubectl apply` refuses `generateName` before it sends anything
(`error: from <prefix>: cannot use generate name with apply`), which is why the sequence creates its
Job with `kubectl create` and applies only its Service.

A name a role reuses is a name something else is using, as was measured here before the server
named the run. A bare `postgres`, or even `infra-71-postgres`, is shared by six roles of one issue
under one identity, and every step after the Job's acts on whatever holds that name: both waits
select its pod, the reachability check reaches its database, and the trap's `kubectl delete job`
takes it and its Service away mid-test. And a role that re-ran the sequence after any failure met a
worse version of the same thing, because a pod keeps its `job-name` label while it terminates:
measured here, the second run's `--for=create` returned 0 in **1 s on the previous run's dying
pod**, and the Ready wait reported `condition met` for one pod and `NotFound` for the other while
the workload that would actually serve was still `Pending`. A reader who took that Ready at its
word would run the reachability check against a workload that is not up and be sent, by the bullet
above, to the one failure this section says announces nothing. A suffix new at each create, which
the server makes, is what removes the whole class: the selector cannot match a corpse that never
carried it.

The Job's manifest is the first of the two the sequence below writes, under the name the API server
gives it. The name on the network is the second, a Service, held to a cluster-internal address: no
node port, no load balancer, no external address and no external name. It is admitted only while
the Job it fronts owns it, so removing the Job removes it too and leaves nothing for a person to
sweep — which is why its `ownerReferences` carries the Job's own uid, read from the create's own
answer.
It selects its pods by that same uid and by nothing else: the namespace's admission requires a
Service's selector to be exactly `batch.kubernetes.io/controller-uid` at the uid of the Job that
owns it, because a rule checking a submission sees only the object submitted and cannot look a
Job up, so a selector by name would let a Service owned by one Job front another's pods. A
`job-name` selector, though the Job puts that label on its pods too, is refused at submission,
naming that rule.

The whole sequence, from a role holding nothing but its checkout and that credential.
It writes both manifests outside the checkout, into a directory `mktemp` makes under `$TMPDIR`
(or `/tmp`), never the issue's workspace: the workspace is a jj working copy of this public
repository, so a manifest left there is snapshotted into the role's working-copy commit and the
password in it can reach the branch. It runs as a `bash` of its own, from a file, and detached
from the command that starts it, because its clean-up is a trap and a role's bash tool ends a
command that runs past its timeout by killing that command and every process under it, too fast
for any trap to run (below). Detached, the sequence is outside what the tool kills; the last line
holds the call until the sequence has ended and then prints what it printed. The three lines in
angle brackets are the only ones a role fills in: the issue key, and the two lines it reads from
the deployment's declaration.

```sh
sequence=$(mktemp "${TMPDIR:-/tmp}/sequence.XXXXXX")              # not the checkout: see above
cat > "$sequence" <<'SEQUENCE'
set -euo pipefail
echo "sequence: process $$; a deliberate stop is: kill -TERM -- -$$"
workload=""                                    # the Job's create sets it; the trap reads it
run=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
echo "run: sequence-run=$run"                  # the Job's label, which exists before its name
dir=$(mktemp -d "${TMPDIR:-/tmp}/workload.XXXXXX")
password=$(python3 -c 'import secrets; print(secrets.token_hex(16))')

cleanup() {
  status=$?
  trap - EXIT INT TERM HUP
  trap '' INT TERM HUP                         # this shell ignores any further stop
  # Each kubectl below runs under setsid -w, in a session of its own, out of the stopped group:
  # kubectl dies of a SIGTERM it inherited ignored, and its waits catch SIGINT and SIGHUP too.
  set +e
  if [ -z "$workload" ]; then                  # no name read: find this run's Job by its label
    echo "cleanup: no Job name was read; deleting by label sequence-run=$run"
    setsid -w kubectl delete job -l sequence-run="$run" --cascade=foreground --ignore-not-found \
      || { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=1; }
    setsid -w kubectl get jobs -l sequence-run="$run"   # empty is no proof none is left: see below
    rm -rf "$dir"
    echo "sequence: exit $status"
    exit "$status"
  fi
  if [ "$status" -ne 0 ]; then                 # what the readings below send a role to look at
    setsid -w kubectl get pods -l job-name="$workload" --show-labels
    setsid -w kubectl logs -l job-name="$workload" --tail=20
    setsid -w kubectl get pods -l job-name="$workload" \
      -o jsonpath='{range .items[*].status.containerStatuses[*]}{.name}: {.state}{"\n"}{end}'
    setsid -w kubectl get service "$workload" --ignore-not-found \
      -o jsonpath='{range .spec.ports[*]}port {.port} -> targetPort {.targetPort}{"\n"}{end}'
  fi
  setsid -w kubectl delete job "$workload" --cascade=foreground --ignore-not-found \
    || { echo "cleanup: the Job's delete failed"; [ "$status" -ne 0 ] || status=1; }
  setsid -w kubectl wait --for=delete service/"$workload" --timeout=60s \
    || { echo "cleanup: the Service is still there"; [ "$status" -ne 0 ] || status=1; }
  setsid -w kubectl get job,service "$workload" --ignore-not-found   # nothing once both are gone
  setsid -w kubectl get pods -l job-name="$workload"                 # "No resources found"
  rm -rf "$dir"
  echo "sequence: exit $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cat > "$dir/job.yaml" <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  generateName: <issue key lowercased>-postgres-   # the API server adds five characters
  labels: { sequence-run: "$run" }      # on the Job alone, not its pods: how the trap finds it unnamed
spec:
  activeDeadlineSeconds: 3600           # on the Job, at most 43200
  ttlSecondsAfterFinished: 3600         # at most 86400
  backoffLimit: 0
  template:
    metadata:
      labels:
        <the pod label the deployment's declaration names for a workload that asks to be reachable>
    spec:
      restartPolicy: Never
      activeDeadlineSeconds: 3600         # on the pod template, at most 43200
      automountServiceAccountToken: false
      runtimeClassName: gvisor
      nodeSelector:
        <the compute-class selector the deployment's declaration names>
      containers:
        - name: db
          image: mirror.gcr.io/library/postgres:16   # the mirror, as every image here is pulled
          terminationMessagePolicy: FallbackToLogsOnError   # an exited server's last lines stay in its status
          env:
            - { name: POSTGRES_PASSWORD, value: "$password" }
          resources:
            requests: { cpu: "4", memory: 8Gi }      # the largest container asks at least 4 CPU
          volumeMounts: [{ name: data, mountPath: /var/lib/postgresql/data }]
      volumes:
        - name: data
          emptyDir: {}                    # dies with the pod, by design
YAML
created=$(kubectl create -o jsonpath='{.metadata.name} {.metadata.uid}' -f "$dir/job.yaml")
workload=${created% *} uid=${created#* }       # the name it was given, and what the Service must name
echo "workload: $workload"                     # the name the recovery line below needs

cat > "$dir/service.yaml" <<YAML
apiVersion: v1
kind: Service
metadata:
  name: $workload
  ownerReferences:
    - { apiVersion: batch/v1, kind: Job, name: $workload, uid: $uid }
spec:
  type: ClusterIP                         # the only kind this namespace admits
  selector: { batch.kubernetes.io/controller-uid: $uid }   # its owner's pods, no one else's
  ports: [{ port: 5432, targetPort: 5432 }]
YAML
kubectl apply -f "$dir/service.yaml"

kubectl wait --for=create pod -l job-name="$workload" --timeout=120s   # it does not exist yet
kubectl wait --for=condition=Ready pod -l job-name="$workload" --timeout=300s
ns=$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)
address="$workload.$ns.svc.cluster.local"
python3 - "$address" 5432 <<'PY'                                  # does it answer yet?
import socket, sys, time
host, port = sys.argv[1], int(sys.argv[2])
readings = {
    "ConnectionRefusedError": "reachable, and nothing listens on that port: read the pod's status, log and ports printed below",
    "TimeoutError": "packets dropped: compare the pod's labels printed below with the reachability label",
    "gaierror": "the name does not resolve: no Service of that name in the namespace the address names",
}
deadline = time.monotonic() + 60
while True:
    try:
        with socket.create_connection((host, port), timeout=5):
            print("answered")
            break
    except OSError as error:
        if time.monotonic() >= deadline:
            kind = type(error).__name__
            print(f"never answered: {kind}: {error}")
            print(f"reading: {readings.get(kind, 'not one this section reads')}")
            sys.exit(1)
        time.sleep(2)
PY
kubectl logs -l job-name="$workload" --tail=5                    # and what it says it is doing

# The work itself goes here, inside this script: "$address" port 5432, user postgres, password
# "$password". The trap above tries to remove the workload on every exit it runs on, and prints
# `sequence: exit <status>` last. It can leave the workload: on a SIGKILL or the role's pod going
# away, or on another signal the trap neither catches nor ignores, which can end the sequence or
# its trap part-way with no `sequence: exit` line; on a clean-up step that fails, which it prints;
# and on a create the API server stores after its label delete has listed the Jobs (see below).
SEQUENCE
echo "sequence: $sequence, its output in $sequence.log"
flock "$sequence" setsid -f bash "$sequence" > "$sequence.log" 2>&1 < /dev/null   # out of the tool's reach
flock "$sequence" cat "$sequence.log"          # waits for the sequence to end, then prints its output
```

On every exit of that `bash` — the work's end, a failed step, or `SIGINT`, `SIGTERM` or `SIGHUP`
— the trap runs. When the status is not 0 it first prints what the readings below send a role to
look at: the workload pod's line with its labels, the last twenty lines of its log, its
container's state, and the Service's port and target port. The state is there because a log read
from a pod whose server has exited can fail — the read goes to the machine the pod ran on, and on
2026-10-25 it timed out in two of eight runs of a server that had stopped — while the state comes
from the cluster's own record, and `FallbackToLogsOnError` in the Job puts the last lines of the
server's log into it when the server exits with an error. Then the trap deletes the Job with
`--cascade=foreground`, which returns once the Job's pod is gone, since the Job controller's owner
reference on the pod holds the Job's deletion back (6 s for a running `postgres:16`, 31 s for one
still initialising, measured 2026-10-25); waits for the Service, whose owner reference, written
without `blockOwnerDeletion`, holds nothing back, so the foreground delete does not wait for it;
lists the Job, the Service and the pods by this run's name, which prints nothing and `No resources
found`; and exits with the status it began with, or 1 when a clean-up step failed after a run that
had succeeded, printing it last as `sequence: exit <status>`, since the call that waits for a
detached sequence prints its output and not its status. A trap that runs before the sequence has
read its Job's name from the create's answer — the create refused, or a stop that landed first —
looks for the Job by this run's label instead. The sequence draws eight random hexadecimal
characters before the create, prints them as `run: sequence-run=<value>`, and writes them as a
label on the Job's own metadata, not its pod template, so the label is never one a pod carries and
cannot be read as the reachability label. That trap prints `cleanup: no Job name was read; deleting
by label sequence-run=<value>`, deletes every Job the server lists with that label, with
`--cascade=foreground`, lists the Jobs carrying it, and exits with its status. After a refused
create, or a stop before the create's request left, there is no Job, and the delete finds nothing.
Otherwise the delete finds the Job only if the API server has stored it by the time the delete
lists the Jobs. In every run measured on 2026-10-25 for dispatch://INFRA-232 whose label delete
found a Job — the stop placed between the create's answer and the line that reads it, and two of
the twenty-six stops in the bullet below, sent 0.33 s and 0.35 s after the first line (that
issue's tester measured two more, at 0.318 s and 0.320 s, sjawhar/legion#2195) — the delete
removed it, and none of the twenty-six left a Job. Nothing the 0.33 s and 0.35 s runs printed
shows whether their stop landed while the create was in flight or after its answer. A
create the server finishes only after that listing — its request still in flight when a stop or a
failed connection ended `kubectl create` — leaves a Job the delete never saw. No run measured that
case, and none rules it out. The trap's own listing shows such a Job only if the server stored it
before that listing, and prints `No resources found` if later, so an empty listing there does not
show that no such Job exists; the block's closing comment names this case among those the trap can
miss. Such a Job would run until the 3600 s deadline it and its pod carry, holding the four
processors it requested. So after every run whose trap printed `cleanup: no Job name was read`,
list `kubectl get jobs -l sequence-run=<the printed value>` once more after the sequence has ended,
and remove any Job it lists with
`kubectl delete job -l sequence-run=<the printed value> --cascade=foreground`, the recovery that
removed a Job a `SIGKILL` left before its name was printed, measured on 2026-10-25. No wait was
measured either: how long after its client is gone the server can still store a create is not
known, so an empty listing shows only that no such Job was stored by the time the listing ran.
Another run's Job carries a value drawn for that run, so neither command touches it. No Service
exists on that path: the sequence applies its Service only once it has read the name.

**A timeout of the role's own bash tool is what the detaching is for.** The tool ends a call that
runs past its timeout by signalling the command and every process under it and killing them almost
at once: on 2026-10-25 a probe logging every 20 ms after a `SIGTERM` logged for 70 ms in one run
and not at all in two others, and the sequence as this page published it before it detached, run
in a call's foreground with a 40 s timeout, was gone with its Job, its Service and its running pod
left behind, in two runs. That is the `SIGKILL` case below, and nothing in the call's output says
so. A process in a session of its own is still killed while it descends from the call
(`setsid --wait` was, measured the same day); `setsid -f` hands it to the system's init, which is
what takes it out of the tool's reach. Run as published here, with the same 40 s timeout, the call
ended at its timeout and the sequence ran on, answered, ran its trap and left nothing. A call that
timed out has printed the sequence's file and its log before anything else, so a later call
re-attaches with `flock <the sequence's file> cat <its log>`, which waits for the sequence to end
and prints everything it printed; `cat` of the log alone shows how far it has got.

A stop signal sent to the sequence's process group — `kill -TERM -- -<process>`, which its first
line prints, is the deliberate stop — runs the trap at once; one sent to its `bash` alone runs it
only when the step in progress returns, which each step's own timeout bounds. Measured on
2026-10-25: that `kill -TERM` during the work ran the trap and the sequence exited 143 with nothing
left; `SIGHUP` and `SIGINT` sent to the group during the Ready wait, and `SIGINT` and `SIGTERM`
sent to it during the check, each ran the trap within a second; `SIGTERM` sent to the `bash` alone
during the Ready wait ran it 76 s later, when the wait returned, and `SIGINT` sent to it alone
during a check that was being refused ran it 58 s later, when the check gave up. Once the trap
runs, its shell ignores further `SIGINT`, `SIGTERM` and `SIGHUP`, and it runs each of its `kubectl`
commands under `setsid -w`, in a session of its own, so a stop sent to the sequence's group while
the trap runs — a second stop, or one sent after a failed step or the work's end started the trap —
reaches neither the shell nor its `kubectl`, and the clean-up runs to its end. The sessions are
needed because `kubectl`, a Go program, dies of a `SIGTERM` it inherited ignored, and while it waits
it catches `SIGINT` and `SIGHUP` too: on 2026-10-25, `kubectl wait --for=delete` started with all
three ignored, against an API server whose watch never answered, ended on each of them, and
kubectl's source shows its `delete` waiting through the same code. Of the stop signals, only a
`SIGKILL` ends a clean-up in progress, and it ends the shell alone: the `kubectl` then running
finishes in its own session, and `flock … cat` waits for it. A signal the trap neither catches nor
ignores can end the clean-up too. Measured on 2026-10-25 on this block, filled in, with one fixture
line for the work, each stop sent to the group: `SIGTERM` once it had answered and then a second
`SIGTERM` 0.5 s later, a `SIGINT` 0.3 s later or a `SIGHUP` 0.3 s later, each printed
`sequence: exit 143` and left nothing; with failing work, one `SIGINT` or one `SIGTERM` sent as the
trap began its readings printed `sequence: exit 1` and left nothing. The block without these lines
failed those runs: a `SIGTERM` during the readings ended the trap there, and with the ignore but no
sessions a second `SIGTERM` killed the trap's `kubectl delete` (it printed `cleanup: the Job's
delete failed`); each left the Job, its Service and its pod. A `SIGKILL` before the trap runs, or
the role's pod going away, ends the sequence with no trap and no `sequence: exit` line: a
`kill -KILL -- -<process>` once it had answered left the Job, its Service and its pod, and the two
lines below removed them in 1 s. The Job's two deadlines bound what such a run leaves, and a role
that stopped that way — or that ran the sequence in its tool's foreground and saw the call time
out — removes it with the name the sequence printed once its Job was created:

```sh
kubectl delete job <the printed workload name> --cascade=foreground
kubectl wait --for=delete service/<the printed workload name> --timeout=60s
```

A role that lost the name lists the Jobs with `kubectl get jobs`. Its issue's start with its issue
key, and so do its sibling roles', so it deletes only the one it started. A run can leave a Job
with no name printed: a `SIGKILL` or the role's pod going away before the name printed; a label
delete that failed, printing `cleanup: the Job's delete failed`; and a stop or failed connection
whose create the server stored only after the trap's label delete had listed the Jobs (above).
Each printed its label first, and
`kubectl delete job -l sequence-run=<the printed value> --cascade=foreground` removes that Job once
the server has stored it, as it removed the one a `SIGKILL` left on 2026-10-25; it had no Service
yet.

That check is `python3`, which the image carries, because **the role container had no client for
any of these servers** when it was written ([Worker image](#worker-image)), a role may not `exec`
into the workload's pod, and installing one in its own pod is the withheld `operator-setup`
capability. The first of those three reasons has since changed, and only the first: the image
this repository builds carries `psql`, `redis-cli`, `sqlite3`, `nc` and `dig` from
dispatch://INFRA-221's merge on 2026-10-25 onward, and a role has them once its deployment pins a
digest built from that merge or a later commit of `main` ([Pin by digest, never by
tag](#pin-by-digest-never-by-tag)) — a role on an older digest still has none of them, and
`python3` runs on both. The role container the runs of 2026-10-25 below came from was one of
those: `command -v psql redis-cli sqlite3 nc dig` found none of the five. What the check proves
is what a role needs first, that the address resolves and the port answers, and when it does not,
it names the class of the failure, which is what the readings below key on.

It loops because `kubectl wait` cannot tell a role the server is listening. The pod carries no
readiness probe, so `condition=Ready` means the container started, and a database spends its
first seconds initialising with its listener closed — `postgres:16`'s entrypoint does exactly
that. A connection *refused* in those first seconds is the workload reachable and not listening
yet: the loop retries and it answers. When the minute runs out, the check prints the class of its
last attempt alone, with a reading for it, and each reading is the next thing to check rather than
a proof:

- `ConnectionRefusedError` — the packets reach the workload and nothing listens on the port its
  Service sends them to. The pod's line, which the trap prints with the rest before it deletes
  anything, says which way to look. `Running` is a server listening somewhere other than the
  Service's `port -> targetPort`, or one still starting a minute after Ready, and its log tells
  the two apart; any other status is a server that stopped, whose reason is in its log or, when
  the log cannot be read, in its container's state. The cluster refuses rather than drops in both
  cases, whether the pod runs with nothing on that port or has already exited. A server that stops
  by itself shows here and not at the Ready wait: a `postgres:16` given an invalid setting ran its
  initialisation, was Ready long enough for the wait to return and exited about a second later,
  and a container that exited the moment it started was reported Ready all the same, so in both
  the check was refused every try.
- `TimeoutError` — the packets are dropped rather than refused, which is what the missing
  reachability label above produces: compare the pod's labels, which the trap prints, with the
  deployment's declaration first.
- `gaierror` — the name does not resolve: there is no Service of that name in the namespace the
  address names, because the address was mistyped or because of the namespace gap above. The
  sequence stops at a refused Service step before it reaches the check, so a refusal there never
  shows as this.

Speaking the protocol is a client library onto the issue's own disk, the way every other tool
that works from the checkout arrives (below), run from the checkout against that same address,
or, on a digest that carries them, one of the image's five clients — never a binary a role
installs into its pod itself. The trap's listing asks for the Service by name as well as the Job
and its pod, because the grant that admits a Service also lets this identity get, list and watch
one, so the listing is what shows the Service went with its Job. The Service's endpoints are
still out of reach (`kubectl auth can-i get endpoints` answers `no`, and so does the same
question of `endpointslices`), which is why the bullet above compares the workload pod's labels
rather than the Service's endpoints.

**What of this has been run, where, and when.** This section is written from commands executed in
the deployment it describes, and that deployment's grants change under it: the apply of
dispatch://INFRA-206's declarations on 2026-10-25 turned two of the gate's three answers from `no`
to `yes`. So each bullet below carries the date of the run behind it, and a reader runs the three
gate commands itself rather than trusting the answers recorded here:

- *Executed from a role container on 2026-10-25, for dispatch://INFRA-232, with the sequence as
  this page published it then, through the role's own bash tool* (dispatch://INFRA-386 has since
  added the trap's ignore line, put `setsid -w` in front of the trap's `kubectl` commands and
  changed comments in the block): the block taken from this page with only its three bracketed
  lines filled in, each run under the name the API server gave it
  and a run label of its own, and a fixture changing one line of it where named. As published,
  with reads of the workload pod's labels and of the Jobs carrying the run label added:
  `run: sequence-run=` and eight characters printed first, `workload: infra-232-postgres-` and five
  characters printed after the create, the Job admitted with its run label and listed by it, the
  pod carrying its Job's `batch.kubernetes.io/controller-uid` at the uid the create returned and
  not the run label, the Service created under the Job's name, `answered`, the trap's listing
  empty, `sequence: exit 0` about 90 s after the start, and no Job of that name left. While that
  run was under way, a second run stopped by a `kill -TERM -- -$$` placed right after the create,
  before the line that reads its answer: the trap printed `cleanup: no Job name was read; deleting
  by label sequence-run=<value>`, deleted that run's Job, listed none and ended
  `sequence: exit 143`, while the first run's Job stayed `Running` beside it and went on to
  `sequence: exit 0`. The same stop on the block as it stood before the label printed
  `cleanup: no Job name was read; nothing removed` and left its Job running until a delete by name
  removed it. Twenty-six more runs, each sent `kill -TERM -- -<process>` between 0.10 s and 0.45 s
  after its first line while the first run was still under way: the earliest, before the trap was
  installed, printed nothing and made nothing; of those that took the label path, two found the Job
  the create had made and deleted it by the label, and the rest found nothing; the later ones had
  read the name and deleted their Job by it. Each of the others ended `sequence: exit 143`, and no
  Job of theirs was left. For the two that found a Job, nothing they printed shows whether the stop
  landed while the create was in flight or after its answer; for the rest, whether it landed
  before the create's request left or while it was in flight. A `kill -KILL -- -<process>` that
  landed before the name printed left a Job with no name printed, and
  `kubectl delete job -l sequence-run=<the printed value> --cascade=foreground` removed it in
  31 s, its pod still initialising. With the Job's `cpu` request `"1"`: the create refused by
  admission naming the four-processor rule, under a name the server had already generated, the
  trap deleting by the run label and finding nothing, `sequence: exit 1`, and no Job left; that
  fixture with the trap's no-name branch taken out, run before the label was added, printed
  `error: resource name may not be empty` and `cleanup: the Job's delete failed`, then
  `cleanup: the Service is still there`, for objects that never existed. With the Service
  selecting by `job-name`: refused by admission at the apply, naming its rule, and the generated
  Job deleted while its pod was `ContainerCreating`, nothing left. With a 120 s stand-in for the
  work and a call timeout of 30 s: the call ended at its timeout, the log showed `answered`, and
  `kill -KILL -- -<process>` then left the Job, its Service and its running pod and printed no
  `sequence: exit` line; the two recovery lines, given the printed name, removed them in 1 s. And
  the two facts the naming paragraphs rest on, run before the label was added: `kubectl apply` of
  the Job's manifest, run in the block in place of the create, refused it before sending anything,
  `error: from infra-232-postgres-: cannot use generate name with apply`, and the trap removed
  nothing; and `kubectl create --dry-run=server` admitted a Job with a generated name, a Job whose
  name the client chose, and one setting both a name and a one-letter `generateName` under the
  client's name — on that date the namespace's admission did not require a generated name, which
  dispatch://INFRA-383 adds.
- *Executed from a role container on 2026-10-25, for dispatch://INFRA-215, with the sequence as
  published then (before dispatch://INFRA-232 moved the Job to a generated name), through the
  role's own bash tool*: the block taken from this page
  with only its three bracketed lines filled in and pasted into a tool call, each run under its own
  suffix, and a fixture changing one line of it where named. As published: `answered`, the trap's
  listing empty, `sequence: exit 0`, the call returning 92 s after it started. With the Service's
  `targetPort` changed: `never answered: ConnectionRefusedError` and its reading after the minute,
  the pod `Running` and `port 5432 -> targetPort 5433` printed, `sequence: exit 1`. With
  `postgres:16` given an invalid setting: refused every try, the pod `Error`, its log and its
  container's state both carrying the server's `FATAL` line. With the reachability label's line
  removed: `TimeoutError` after the minute. With the address renamed: `gaierror`. With a call
  timeout of 40 s and of 20 s (the second with a 90 s stand-in for the work): the call ended at
  its timeout, the sequence ran on to `sequence: exit 0` with nothing left, and a later call's
  `flock … cat …` waited for the end and printed the whole run. `kill -TERM -- -<process>` during
  the work: the trap ran, `sequence: exit 143`, nothing left. `kill -KILL -- -<process>` during the
  work: the Job, its Service and its pod left, and the two recovery lines removed them in 2 s.
  And, run the same way, the sequence as this page published it until then, in the call's
  foreground with a 40 s timeout: the call ended at its timeout and the Job, its Service and its
  running pod were left, until the recovery lines removed them. None of this issue's Jobs was left
  after the runs.
- *Executed from a role container on 2026-10-25, for dispatch://INFRA-215, with the sequence run
  in the foreground of a process of its own, before it detached*: the steps from the Job to the
  trap were as published then. The three gate commands answering `yes`, and `get endpoints`,
  `get endpointslices` and `patch jobs` answering `no`; `POD_NAMESPACE` unset, and none of `psql`,
  `redis-cli`, `sqlite3`, `nc` and `dig` on the `PATH`. Ready 79 s after the submission on a cold
  machine, `answered` 2 s later, the trap's foreground delete in 6 s and the Service wait at once.
  The `targetPort`, invalid-setting, label and address fixtures as above, and a container that
  exits the moment it starts, reported Ready and then refused every try. With the Service selecting
  by `job-name`: refused by admission at the apply, naming its rule, and the Job deleted while its
  pod was `Pending`. The other signals the paragraph on stopping sets out, with what it records of
  each, and a `SIGKILL` to the `bash` during the Ready wait, which left the Job for the two recovery
  lines to remove in 2 s. The namespace's quota for Jobs counted 4 processors per workload and was
  back at 0 after them. Once, with fourteen sequences submitted within four seconds, a Service step
  failed `Error from server (Conflict)` on the namespace's quota object, which another submission
  had just changed; the trap removed that run's Job, and a re-run went through. And the
  credential-less gate again, with the in-cluster environment removed: exit 1, `The connection to
  the server localhost:8080 was refused …`.
- *Executed from a role container on 2026-10-25, for dispatch://INFRA-234*: the three gate
  commands, **all three answering `yes`**, and the grant matrix behind them
  (`create`/`delete`/`get`/`list`/`watch jobs`, `get`/`list pods`, `get pods/log`, `delete pods`
  and `create`/`get`/`list`/`watch`/`delete services` `yes`; `patch`/`update jobs`, `create pods`
  and `get`/`list` of `endpoints` and of `endpointslices` `no`); and **the whole sequence for
  real**, as published then under a fresh suffix — the Job applied and created, its uid read, the
  Service applied exactly as published then and created, the pod waited for into existence (at
  once) and then to Ready (70 s), the workload pod's labels read and carrying both its Job's
  `batch.kubernetes.io/controller-uid` and the reachability label, the reachability check
  answering by the Service name about two seconds after the Ready wait returned — the same block
  with one print per attempt added saw its first connection refused and the retry answer, the
  listener opening a second or so after Ready, which is the starting-up case the paragraph under
  the sequence describes — the Job's log ending `database system is ready to accept connections`,
  `kubectl delete job` run, and the closing listing showing the Job and its Service gone at once
  and its pod `Terminating`, then nothing of it twenty seconds later.
- *Executed from a role container on 2026-10-24, for dispatch://INFRA-71*: the sequence's Job half
  (87 s to Ready on a cold machine, 10 s on a warm one), which is where the two waits come from:
  the single `wait` this section published before exited 1 at once with `error: no matching
  resources found`, because the Job's pod does not exist the instant the apply returns; and the
  reachability block, run as published then against a listener and against a port that refuses
  and then listens. And, *not run again since*: one deliberate breakage of each **enforced** rule
  above, put through the namespace's own admission server-side with nothing created, each refused
  naming the rule — the reachability label has no breakage to record, since admission does not
  check it, which is the whole of why that bullet is marked. (The credential-less gate and the
  reachability block against an address whose packets are dropped, on this list until then, were
  run again for dispatch://INFRA-215, above.)
- *Refused as published, and corrected on 2026-10-25*: until dispatch://INFRA-234 the Service
  manifest above selected the Job's pods by `job-name`, and the namespace's admission refuses
  that, naming the rule that a Service there selects by `batch.kubernetes.io/controller-uid` at
  its owning Job's uid and by nothing else. Both forms were put through
  `kubectl create --dry-run=server` with nothing created: the old one refused, the one above
  admitted.

What a workload there cannot do, so nobody plans around it: it cannot mount the issue's disk,
which belongs to another namespace, to one node at a time and to one pod, and a role may not run
a command
inside its pod — the grant is create, read, follow and delete, never `exec`. A formatter started
that way would never see the file it would format, so **a tool that works on the checkout does not
belong here**: those are the image's own clients, or arrive through its package runners onto the
pod's own disk, which every role of the issue shares for as long as that pod runs. That is
also why the two notes recording `kubectl auth can-i` answering `no` are not contradicted here
(`a-pod-rehearses-a-checkpoints-own-readers-…-LEGION-578.md` in `docs/solutions/legion/`, and
`prove-a-published-worker-image-from-a-pod-…-LEGION-764.md` in `docs/solutions/testing/`): each
answers for a different verb — creating a Sandbox, creating a pod, `exec` into one — in the
namespace Legion's own pods run in, which this changes nothing about.

Two costs are shared and are the reason a role cleans up after itself rather than waiting for a
deadline. One quota covers that whole namespace and is shared with the team's builds, so a
workload left running is charged against both: measured on 2026-10-25, the namespace's quota for
Jobs counted `requests.cpu` 4 for each of these workloads while it existed, 48 with twelve at once,
and was back at 0 once the last of them was deleted. And every project's agents hold the one
identity (`legion-worker`), so a password written into a Job is readable by every other agent,
and any of them may delete any workload there. The other half is reach: the ingress rule admits
the namespace Legion's pods run in rather than an identity, naming no pod and no port, so every
pod there — every project's agents, with a Kubernetes credential or without one — reaches a
labelled workload at its Service name, and the password in the Job, which every holder of the
identity can read, is the only lock on it. Narrowing the identity to a per-project one is
dispatch://INFRA-153's work, not something to design around here.

### Issue sizing: one reservation per pod

Every container of a Legion pod carries a cpu and memory limit with a request no greater than it
(`requirements`, `internal/daemon/kubernetes.go`), and an ephemeral-storage limit over a smaller
ephemeral-storage request (the node-disk bound, below): an issue pod's `roles` container and both
its init containers the issue pod's one reservation (`runtime.kubernetes.resources.issue`, the
daemon's default where the file sets none — [Configuration](#configuration)), the controller's
launcher the controller's (`runtime.kubernetes.resources.controller`), and the
image probe's one container the probe's own fixed 250m / 1Gi, request equal to limit
(`probeReservation`, [Configuration](#configuration)). At the default an issue pod reserves 3 CPU
and 19 GiB, the init containers adding nothing: a pod's effective request is the larger of its
containers' sum and its largest init container, and they carry the same reservation. Every pod
reserved is what the `resource-limits` row of
[The deployment's capability report](#the-deployments-capability-report) measures, so on a
Kubernetes deployment it is `present` with or without a `resources` block; its detail names the
pods, if any, that are `Burstable`.

One number frees nothing at run time. Under gVisor every process of a pod already used whatever the
pod had free (below), so the pod's peak must fit its reservation: an issue pod on the production
cluster, measured at 1-minute resolution, used its whole 19Gi for minutes while its reviewer ran a
review (2026-10-21).

**Guaranteed or Burstable.** The kubelet's QoS class reads cpu and memory alone (the disk bound's
request under its limit changes no pod's class). A deployment that sets no `cpu_request` or
`memory_request` below its limit runs only `Guaranteed` pods: each reserves its peak for its whole
life and bursts past nothing, and it is never the class the kubelet evicts first under node memory
pressure. An issue pod given a lower request — `issue: {memory: 32Gi, memory_request: 8Gi}`, for
issues whose `lean` processes each map a few GiB at import and can pass 20 GiB on a large proof,
but which use little between builds — is `Burstable`, as is the controller's pod given a lower
`controller` request; the image probe pod stays `Guaranteed`. The init containers carry the same
reservation, so they never raise the pod's effective request above its container's. The
scheduler and the pool's limits count the requests, so more pods fit a node and a request-billed
cluster bills less. The cost is that the burst is not reserved: a node whose pods together use more
memory than it has is under memory pressure, and the kubelet evicts `Burstable` pods using more than
their request before any `Guaranteed` pod, or the kernel OOM-kills the container. Either stalls the
tree as an OOM kill of a `Guaranteed` pod does — the pod is relaunched and its roles resume their
sessions, but the turn in flight is lost. Request at least what the issue's roles need to boot and
to idle together, and set a `Burstable` pod only where losing a turn under pressure is cheaper than
reserving the peak.

On GKE Autopilot, which bills a pod by its requests, a pod on the default general-purpose platform
or the `Balanced` or `Scale-Out` class is honoured a request below its limit only in a cluster that
supports bursting: GKE 1.30.2-gke.1394000 or later on cgroup v2, with the GKE-managed
`efficiency-daemon` DaemonSet running (`kubectl get daemonset -n kube-system efficiency-daemon`); a
cluster that does not support it sets each limit to the request, so the issue pod above would run
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
general-purpose platform an issue pod's ceiling is about its own requests, whatever its limits
say: with `issue: {memory: 32Gi, memory_request: 8Gi}` the pod requests 8Gi and never reaches
its 32Gi limit. On a `Performance` node the ceiling is those requests rounded up to a machine
type, and a lower request still picks a smaller, cheaper VM. To give an issue real headroom on
Autopilot, raise its request rather than its limit alone.

The defaults were sized from an issue pod on the production cluster (measured 2026-10-09, under
`GOMAXPROCS=3`): `go test ./...` of `packages/daemon` peaks at about 1.45 GiB of summed RSS, a cold
`go build ./...` at about 0.97 GiB, `bun test` of a plugin package at about 1.2 GiB, Biome and
`bun install` at about 0.8 GiB each and `tsc` at about 0.75 GiB; a role's Oh My Pi process is about
1 GiB after half an hour of work, and a headless Chromium's largest process about 0.45 GiB. A
lane-running role's Go test lane, its agent and a browser sum to about 3.9 GiB, at the limit of the
4Gi they were first given, and a `Guaranteed` pod OOM-killed mid-turn stalls its tree; so the
implementer's and tester's 6Gi held its agent, one lane and a browser with room over that sum,
the reviewer's 4Gi its agent, a browser and its review pair, and 1Gi each the architect, planner and
merger. The `issue` default, 19 GiB, is the sum of those six figures.

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
than this two-per-node packing. The **controller's** pod, whose Sandbox owns its claim, is the one
that waits on a volume when it is relaunched onto another node: its `ReadWriteOnce` volume must
detach from the old node first, which shows as transient `FailedAttachVolume` or `Multi-Attach`
events until the old pod is gone; then it runs on. An **issue** pod waits on neither: its disk is
the pod's own generic ephemeral volume, so a replacement takes no predecessor's claim and nothing
detaches (INFRA-80). What it may wait on instead is the predecessor's claim being *reaped* — the
kubelet and the garbage collector removing it with its pod — which is a different mechanism with
different events, and which [The disk dies with its pod](#the-disk-dies-with-its-pod) covers.

Under gVisor the container cgroup changes nothing inside the sandbox: `runsc` sizes the sandbox
from the pod's cgroup, so inside a pod `nproc` is max(2, ⌈Σ cpu limits⌉) — except on GKE Autopilot,
confirmed live by [Stage 4a's live proof](../scripts/e2e/README.md): there `nproc` reflects
gVisor's own host-vCPU presentation instead, a known gap
([kubernetes/kubernetes#91665](https://github.com/kubernetes/kubernetes/issues/91665)), and the
pod's own cgroup cpu quota (`cpu.cfs_quota_us`/`cpu.cfs_period_us`) is what actually tracks Σ cpu
limits there — and `/proc/meminfo`'s `MemTotal` is about Σ memory limits of the pod's containers,
and an active role may use the pod's whole reservation while its siblings idle. The reservation is
the pod's working set, and its request is what the scheduler counts.

**The bound.** Concurrently running issue pods are bounded by what the pool's `limits.cpu` and
`limits.memory` leave for pods of the per-pod sum of requests: at the defaults, two pods fit a floor
node and `limits.cpu: 256` places about 80 three-CPU pods (less what the nodes' daemonsets hold),
and `limits.memory` divided by 19 GiB bounds them too when that is the smaller (a `Burstable`
issue pod's lower request places more) — where the
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

**The node-disk bound.** Pods of unrelated trees share a node, and what an issue pod writes outside
every volume — the `roles` container's root filesystem (the image's own content and whatever a
recipe writes outside the told directories) and the `role-homes` disk `emptyDir`, which holds each
role's home overlay and its `/tmp` and `/var/tmp` (the issue's temporary directory, its cache and
data homes and bun's install cache are one set its roles share on the issue's volume, each role's
state home is a separate directory there, and the two Go caches are one pair on that volume shared by
the issue's roles) — is the node's disk, the node's allocatable ephemeral storage
(its root volume; read it with `kubectl get node <n> -o
jsonpath='{.status.allocatable.ephemeral-storage}'`). That disk must hold the sum of the pods'
actual disk use, which Σ of their `ephemeral_storage` limits bounds above: an issue pod's limit is
80Gi at the default, its request 6Gi. A pod past its limit is evicted — the offending issue's pod
alone — rather than the node reaching `DiskPressure`, where the kubelet evicts by its own ranking
and another tree's pod can go. The request is what the scheduler counts against the node's
allocatable ephemeral storage; at the default 6Gi it binds little, so a node's disk can be
oversubscribed by the pods' limits. In production (read 2026-10-09) the Legion node class carries
one 700Gi gp3 root device, and a node's allocatable ephemeral storage is about 629 GiB of it; with
CPU and memory placing about two issue pods per node, their limits sum to about 160Gi of those 629
GiB, so the bound has room before any request is raised. An operator with a known root volume
raises `ephemeral_storage_request` toward the issue's expected use so the scheduler reserves disk
and places no pod a full node could not hold.

**GKE Autopilot's bound is per pod, not per node.** The node-disk framing above is AWS/self-managed
EKS's: ephemeral storage there is the shared node's own root volume, and raising
`ephemeral_storage_request` toward an issue's real use is an operator's own call, bounded only by
what the node has. GKE Autopilot has no such headroom: Warden, its admission webhook, caps
ephemeral storage at 10Gi for the sum of a pod's main containers and 10Gi for each init container
(server dry runs on the production cluster, 2026-10-24: two init containers at 10Gi beside a 10Gi
main container are admitted; a main container at 11Gi is refused, `Total ephemeral-storage
requested by containers … higher than the Autopilot maximum of '10Gi'`; an init container at 11Gi
is refused, `Max ephemeral-storage requested by init containers …`), and it clamps a container's
limit to its request. The init containers carrying the pod's reservation add nothing against that
cap, so a GKE deployment sets
`issue: {ephemeral_storage: 10Gi, ephemeral_storage_request: 10Gi}`: the issue pod's whole 10Gi,
shared by its six roles.

### The disk dies with its pod

An issue pod's disk is the pod's own: a generic ephemeral volume declared in its pod template
(`issuePod.disk`, `podkind.go`), whose claim Kubernetes creates when the pod starts and
deletes when the pod goes. Its size and class are what they were when an issue's Sandbox owned the
claim — `runtime.kubernetes.issue_volume` (50Gi by default) and `storage_class`, `ReadWriteOnce` —
and what changed is who owns it and how long it lives. The claim is named for the pod and the
volume, `<pod>-issue`, the pod's name being the issue's Sandbox name
(`legion-<project>-<issue key lowercased>`); the pod is its owner; and garbage collection removes
it with the pod. It carries no label at all: the pinned Sandbox CRD declares an ephemeral volume's
`volumeClaimTemplate.metadata` a bare `type: object` with no properties and no
`x-kubernetes-preserve-unknown-fields`
(`internal/runtime/sandbox/testdata/sandboxes.agents.x-k8s.io.yaml`), so the API server prunes
whatever is set there without a word, and a test holds the manifest the runtime builds to that
pinned CRD (`TestManifestMatchesTheSandboxCRD`). An operator therefore reads an issue's disk by its
name and its owner, never by a selector:

```sh
kubectl -n legion get pvc legion-<project>-<issue key lowercased>-issue \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,OWNER:.metadata.ownerReferences[?(@.controller==true)].kind,OWNERNAME:.metadata.ownerReferences[?(@.controller==true)].name'
```

`kubectl -n legion get pvc -l legion.dev/issue=<KEY>` answers nothing whatever the issue is doing,
and so does any other selector over an issue's disk.

An issue's Sandbox owns no `volumeClaimTemplates` entry at all (`issuePod.disk`), so
nothing an issue works on is held by an object that outlives its pod. Two claims are still a
Sandbox's and are untouched by this release: the project controller's,
`issue-legion-<project>-controller` (`IssueClaimName`, from its Sandbox's one `issue` template,
[Daemon-launched controller](#daemon-launched-controller)), and each repository cache's,
`cache-<cache sandbox>` ([The repository cache](#the-repository-cache)). Neither holds an issue's
clone or workspace.

Every end of an issue pod is therefore the end of that pod's disk, and the ends are these:

- **a launch replaces the pod**, because it cannot run the launching role as a pod created now
  would: another image, a moved worker stream, a secrets-broker enrollment or session-database key
  the pod lacks, a role's launcher Secret that no longer binds it, or a Sandbox of the disk shape
  before this release ([Pinning a release that drops what a running pod still
  uses](#pinning-a-release-that-drops-what-a-running-pod-still-uses),
  [Cutting over to the pod-owned disk](#cutting-over-to-the-pod-owned-disk));
- **the issue is stopped**: its close as `done`, or a move to `backlog`, `icebox` or `triage`,
  runs the durable suspend effect, which sets the Sandbox `Suspended`, waits its pod out and
  deletes the Sandbox with the six role Secrets it owns (`SuspendIssue`,
  `internal/runtime/sandbox/issue_suspend.go`), logging `sandbox runtime: deleted the closed
  issue's Sandbox and the role Secrets it owned`. The pod's disk went with the pod before the
  Sandbox did;
- **the tree closes**: at the linger's expiry its cleanup foreground-deletes every issue Sandbox
  of the tree, in any order (`CleanupTree`, `internal/runtime/sandbox/cleanup.go`), and hands none
  over — a child carried into a tree of its own gets a Sandbox of its own at its next launch,
  since there is no disk left to hand it;
- **the orphan sweep** deletes a Sandbox of a tree whose cleanup confirmed or that has no
  lifecycle (`ReconcileOrphans`, `internal/runtime/sandbox/sandbox.go`), taking its pod, and the
  pod its disk;
- **the node goes**, or the kubelet evicts the pod. Agent Sandbox recreates a `Running` Sandbox's
  pod under the same name at once, and that pod brings a claim of its own: the same name, a new
  uid, the new pod as its owner. Nothing waits for a detach, since the replacement mounts no
  volume the pod it replaces held, so the `FailedAttachVolume` and `Multi-Attach` events a
  reattached `ReadWriteOnce` volume used to produce do not arise for an issue's disk.

What a pod's disk carries is therefore the issue's work in progress and nothing more: the clone
and the jj workspace, the issue's scratch and caches, uv's Pythons and packages, the workspace's
CodeGraph index and each role's state home with Oh My Pi's logs under it. None of it reaches the
next pod. What does reach it is what lives elsewhere — each role's session, a row in the session
database ([Selecting the store](#selecting-the-store)); the issue's branch on GitHub,
`legion/<KEY>`, which every pushed turn is in; the issue's record in the daemon's own Postgres;
and the repository's cache, which fills the next pod's feed.

A pod's lifetime is not a role's, and this paragraph is the one place that says how they differ.
A pod is created by a launch — the issue's first, one that finds no live pod (after a suspension,
say), or one that must replace the pod (the first end above) — and provisions its workspace from
the issue's pushed branch (`legion/<KEY>`, or the repository's default branch when none was
pushed) with the same two init containers every new pod runs ([Anatomy of a Sandbox
pod](#anatomy-of-a-sandbox-pod)); it then lives until one of the ends above, and its disk dies
with it. A role launched while that pod is live and nothing above replaces it starts in that pod
and provisions nothing: no init container runs, the daemon mints no provisioning token, and the
role finds the workspace as the roles before it left it, unpushed work included (`relaunch`,
`internal/runtime/sandbox/relaunch.go`, which provisions only inside the branch that replaces the
pod; `TestRoleStartsShareOneIssuePodAndDoNotReinitializeIt`). In an issue's ordinary run that is
five of its six roles: the first launch creates the pod and the other five start into it. So only
a new or replacement pod provisions afresh, and an agent resumed into a pod created after the one
it last worked in finds a workspace newer than its session's last write; it is told so before its
next turn, by `LEGION_WORKSPACE_RECREATED` and the message its plugin saves from it ([Selecting
the store](#selecting-the-store)). An agent's unpushed work is lost with the pod, at one of the
ends above and never at a role start into a live pod, which is why a phase pushes what it has
done.

A near-full disk slows jj's working-copy snapshot before a push, which is why `legion push`'s
grant lives `credential.pushTTL` (5 minutes) in place of the usual 60 seconds
(`internal/credential/grants.go`; `docs/solutions/legion/worker-pane-shell-gotchas.md` has the
mechanics).

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
  the repository cache, then its stream and verification under one `workspace.FetchTimeout` and
  its delta under another, rather than `workspace.CommandTimeout`, all within
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
  `workspace.FetchBudget` and `workspace.CloneTimeout` together (220 min;
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
  ready and a transfer that keeps making progress can hold the launch for the fetch's whole 190
  minutes, and `workspace-init`'s shared clone of the feed, for a large repository, up to 30 more.
  Each launch try is bounded inside the runtime (`sandbox.Runtime.LaunchBound`,
  `internal/runtime/sandbox/relaunch.go`) at the sum of the try's waits and bounded calls — three
  boot timeouts; the repository cache's waits, five more boot timeouts and the calls of one other
  ensure of the cache that may hold its turn ahead of the try; the launcher wait's backstop of
  twice the boot timeout plus `sandbox.Runtime.ProvisionBound`; the termination grace; and the
  try's client calls at their 30 s timeout — about 4 h 50 min at the defaults; a try that reaches
  it is charged a launch failure and the next try runs, while a try the daemon's own stop ends is
  still charged nothing.
  Once the launch returns, the boot watchdog re-arms on the claim, bounded by its registration
  deadline
  (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, default 360 s,
  6 min): under Kubernetes, the deadline carries an added bound of `sandbox.Runtime.ProvisionBound`
  until the shim's first hello, and from there the daemon re-arms the base deadline alone, the same
  one a tmux pane runs under throughout. A pod that never says hello is retired at the launch's return
  plus the base deadline plus the bound, armed as one (226 min at the defaults); one that says hello
  and never registers is retired at hello plus the base deadline alone (6 min from the hello); and
  one whose agent registers and never says it is ready is retired at its registration plus the base
  deadline alone (6 min from the registration, again from a daemon restart that finds it
  registered), then resumed as the same session one generation later and counted as a launch
  failure. A tmux pane carries no bound to begin with, since it starts the agent at once with no
  init phase. Nothing serializes the pods of one tree against each other: each issue pod is created
  as its launch comes, and only the relaunches, suspension, release and orphan deletion of one pod
  take turns, keyed by its Sandbox's name (`lockPod`);
- the init container **terminated non-zero** (its current state, or `LastTerminationState` once the
  kubelet has already restarted it) → **dead (gone)**, its log tail quoted; under
  `restartPolicy: Always` the pod never turns `Failed` for
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
- `Running`: each role judged by its own launcher connection's state, never the whole pod — one
  role's process exiting is that role's **dead (gone)** alone, and changes nothing for its
  siblings. The `roles` container terminated (its PID 1 exited, `OOMKilled`, an eviction) ends
  every role of the issue: each is **dead (gone)**, its detail naming the container's reason and
  exit code and quoting that role's own last 20 `<role>: ` lines of the container's log, then the
  container's last 20 unprefixed lines (`roleLog`, `observe.go`). No status at all for the
  container → **unknown**. A connected launcher reporting
  a child of the recorded generation for the role is **alive**, even when Kubernetes still shows
  the container's previous instance terminated for a moment after a restart: that one answer is
  asked first, and it alone outranks the terminated status. Otherwise, with no launcher connection
  for the role → **unknown** (a booting or redialing launcher is not death; the boot watchdog
  decides), its detail adding the container's waiting reason and, once it has restarted, its
  restart count and its last instance's reason and exit code (`role tester's launcher is
  disconnected; container roles waiting (CrashLoopBackOff: …); container roles restarted 5 times,
  its last instance terminated (Error, exit code 2 at …)`), so one crash-looping `roles` container
  that holds every role `unknown` says why. Otherwise the connected launcher's remaining answers: a
  child of another generation is
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
  `https://github.com/<owner>/<repo>`; a feed the pod's disk already holds (a retry of the init
  container in the same pod) gets that fetch alone —
  reading no git configuration but the feed's own, which only this container writes
  (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_PARAMETERS` unset), with a
  one-shot credential git asks for `https://github.com` alone. The cache is asked with the
  cache's own token, from the same Secret, and an answer naming another repository is refused.
  Its tar may hold only the repository's refs, packs and the index of each pack (`HEAD`,
  `packed-refs`, `refs/`, `objects/pack/pack-<id>.pack` and `.idx`), each a regular file or a
  directory inside the feed, never a configuration, hook, reverse index, bitmap or alternates file;
  the first entry that is not is refused by name and the fetch fails (`FeedEntry`,
  `internal/workspace/clone.go`), as it does when a pack or an index fails the checksums git wrote
  into it, a pack without an index does not index, or the history is not whole. The index's names
  are taken on the cache's word ("The repository cache"). A feed is filled beside its final path and renamed into
  place once whole, with git's automatic garbage collection off and every fetch's objects kept as
  a pack, since the issue's clone's object store owns its links to that feed. It mounts no other part of the issue's
  volume and no config home, and it takes an existing feed repository only in the shape it writes
  (no symlink, gitfile, hook, alternates, configuration key of anyone else's, or configuration line
  outside git's own form; see "Anatomy of a Sandbox pod"), so nothing an earlier release's agents
  wrote at `/legion/feed` runs in it. No volume is ever carried over, so there is no earlier
  release's `feed` to meet: an issue pod's disk is created with the pod and deleted with it
  (INFRA-80), and every feed a fetch meets is one this pod's own init container wrote, which that
  same check — run before any git, in an init container no agent runs beside — is what keeps the
  fetch from running anything an agent of this pod planted. Every other provisioning command is
  bounded by `workspace.CommandTimeout` (5 minutes, fixed), but a transfer's duration follows the
  repository's size and the network's speed, not a fixed step in provisioning: once the fetch may
  start — at once, or once the repository cache is ready, after a wait of at most 130 minutes, which
  covers the cache's own first clone — the
  cache's stream and its verification run under one `workspace.FetchTimeout` (30 minutes), and every
  attempt of the transfer from GitHub and its reads of GitHub's `HEAD` under another, each given what
  remains of its own, so the fetch's waits, transfers and checks end within `workspace.FetchBudget` (190
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
  pod's launchers while either init container runs, up to `sandbox.Runtime.ProvisionBound` (220
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
  /legion/feed`: the clone is a local `git clone` of the feed, its origin set to
  `https://github.com/<owner>/<repo>`, and its fetch reaches that remote at the feed over git's
  file transport, bringing the default branch and the issue's branch alone, each by its exact
  name, then the workspace add, `update-stale`, and the configuration writes, once it has removed
  the clone's stale git locks ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)). Local clone
  object entries survive a feed replacement, while its checkout of the working tree and jj's import
  of every commit follow the repository's size, so the two share one bound of their own,
  `workspace.CloneTimeout` (30 minutes), the import given what the checkout left, rather than
  `workspace.CommandTimeout`; every other command here keeps the 5 minutes.
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

The boundary above protects the provisioning token alone. Since LEGION-631 each role holds its own
App's tokens, read-only, in the gh bundle its `LEGION_GH_OWNERS_DIR` names, which only its own
namespaces bind ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)): an implement role (the
implementer, the merger) holds the implement App's tokens — for the issue's owner the same App,
from the same lease, as the provisioning token — and a review role (the architect,
planner, tester, reviewer) the review App's, every owner but the issue's narrowed to reading. No
review role holds the implement App's token, and no implement role the review App's (the review
App cannot resolve a review thread on the implementer's pull request, so the reviewer names the
bot threads it accepted to the implementer, which resolves them with its own `gh`; the daemon
serves no thread route). That split keeps each role's tokens out of a sibling's own reads. It is
not a boundary against code an agent of the same issue plants: each role's `gh`, `git`, jj, Oh My
Pi and tools run on the issue's volume, with whatever an agent of the issue planted there, and the
plant runs with the credentials of the role that runs it. The implementer can plant a git filter
driver, a test or a script that the tester or the reviewer runs, a jj setting, an Oh My Pi
extension in the workspace's `.omp/`, a module in a cache the roles share, or an `http.proxy` that
receives the review role's token, and then act as the review App; a review role can act as the
implement App the same ways. The full list is in [Anatomy of a Sandbox
pod](#anatomy-of-a-sandbox-pod), under the git pins. So an approval posted as the review App says
that a process holding the review App's token posted it, which code from any role of the issue can
be; it does not prove that the reviewer's agent decided it. The same holds for a push or a pull
request as the implement App. A repository that needs a person's approval sets that in its own
branch protection, which Legion neither reads nor writes.

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
- `services`: list, watch, which the Service informer reads, so a launch hands an issue pod
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
ConfigMaps, Secrets, ServiceAccount, providers Secret and shares (claims and NFS exports) that exist then. A change that adds one of
them applies it first, and fails here at once until it does, naming the object — the missing
ConfigMap or Secret and the volume the pod block gives it to (`the probe pod <name> cannot mount
volume "<volume>", which runtime.kubernetes.pod.volumes gives it from ConfigMap "<name>": …`), as
the daemon's boot would; a change that removes or renames one the pod still names passes here and
fails at the restart.

## Kubernetes runtime (TypeScript daemon)

This section is the retired TypeScript daemon's runtime, which it no longer runs: it refuses
`runtime: kubernetes` at config load (LEGION-286); its sources were deleted on 2026-10-03 (commit
`ed00df8b2`, "Legion runs on the Go daemon alone"), so the section names no live directory of its
own and is kept for reference until someone removes it — except its "Operator configuration"
subsection and that subsection's "Network shares", which describe the pods Legion runs today.
With `runtime: kubernetes`, the daemon runs the tree's root architects and phase workers as one
Kubernetes pod per process, on one disk volume per issue tree. The controller always remains an
interactive tmux pane on the daemon host: `runtime: kubernetes` selects where roots and workers run,
never the controller. Attach with `tmux -L legion-<project> attach -t legion-<project>`. LEGION-25
Part B (`legion controller start`, "Operator-launched controller" below) is unaffected and optional.

The daemon keeps owning retries, generations, the same-agent `--resume`, and the running-worker cap
exactly as it does on a single machine; Kubernetes only supplies the root or worker process, volume,
and resource allowance. Nothing in this mode uses a Job, a StatefulSet, or `activeDeadlineSeconds`.

### Configuration

This section is the retired TypeScript daemon's, which now refuses `runtime: kubernetes` outright,
naming the Go daemon (LEGION-286); what follows is the block it read before — for the pods Legion
runs today see [Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod), and for the live settings
[Configuration](#configuration), the Go daemon's. The Go coordinator (`packages/daemon`, LEGION-208)
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
`runtime.kubernetes.pod` — `env`, `volumes` (each one `secret`, `config_map`, `projected`,
`persistent_volume_claim` or `nfs` source), `volume_mounts` (read-only unless `read_only: false`), and `service_account` — which it
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
    session_store: pvc           # never read by this daemon, and refused by the Go daemon (see Session store)
    session_dsn_secret: SESSION_DSN  # likewise: the Go daemon requires this key under session_store: postgres
    session_database:            # likewise: the Go daemon requires it under session_store: cloudsql-iam
      instance: example-project:us-central1:example-instance  # the Cloud SQL connection name, project:region:instance
      name: legion_sessions      # the database in that instance
      user: legion-sessions@example-project.iam  # the IAM login every pod signs in as
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

The block's three `session_*` keys are historical twice over: the TypeScript daemon read none of
them, and the Go daemon that does read them takes only a store that keeps sessions in a database,
refusing `session_store: pvc` and an omitted `session_store` with the same line ([Selecting the
store](#selecting-the-store)).
Copied into a live `legion.yaml`, that `pvc` is a boot refusal rather than a default, and the
`session_store` clauses of the refusal list below are this section's history, not this release's
rule.

A user block with `exec:` (the shape `aws eks update-kubeconfig` writes) is honoured: the command
runs with the daemon's environment plus `exec.env`, its `status.token` is cached until one minute
before `status.expirationTimestamp`, and one 401 triggers one refresh-and-retry. uid `legion` on
the devbox receives the instance role through IMDS, so no credential file exists.

Every Legion pod is annotated `karpenter.sh/do-not-disrupt: "true"`, which stops consolidation and
drift from evicting it; a NodePool's `expireAfter` is forceful in Karpenter v1 and must be `Never`
for Legion's pool, provisioned by your infrastructure-as-code.

The block is file-only: there are no `LEGION_KUBERNETES_*` environment keys, and `LEGION_RUNTIME`
never outranks the file (a disagreement is logged once and ignored). Defaults (root spec §3):

This table is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

| profile | requests (cpu / memory / ephemeral-storage) | limits (cpu / memory / ephemeral-storage) |
| :--- | :--- | :--- |
| `small` | 500m / 1Gi / 2Gi | 2 / 3Gi / 8Gi |
| `medium` | 1 / 2Gi / 10Gi | 4 / 6Gi / 30Gi |
| `large` | 2 / 4Gi / 20Gi | 6 / 12Gi / 60Gi |

This table is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

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
process to wrap; `session_store` is anything but `pvc`, `postgres` or `cloudsql-iam`;
`session_store: postgres` comes
without `session_dsn_secret`, or with one that is empty, is not a Secret data key
(`[-._a-zA-Z0-9]+`), or is named `OMP_SESSION_STORAGE` or `OMP_SESSION_SQL_DSN_FILE` (see
[Selecting the store](#selecting-the-store)); `session_store: cloudsql-iam` comes without
`session_database`, or with one that is not a mapping, names a member but `instance`, `name` and
`user`, leaves one of the three empty, or whose `instance` is not a Cloud SQL connection name the
connector uses whole (`project:region:instance`, or `domain:project:region:instance` for a
domain-scoped project, with no whitespace); `session_dsn_secret` is set under `pvc` or
`cloudsql-iam`, or
`session_database` under `pvc` or `postgres` (an inert key
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
issue's pod (every role of it alike) and the image probe's.

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
  `volumes` are each one `secret`, `config_map`, `projected`, `persistent_volume_claim` (a claim of
  the daemon's namespace, by `name`) or `nfs` (`server` and `path`) source, never an inline `csi`
  volume ([Network shares](#network-shares)); a projected
  `service_account_token` must last at least 600 s, the least the API server issues, and its
  `audience` may not still hold a `${…}` placeholder, which nothing expands. `volume_mounts`
  are read-only unless `read_only: false`, and may use `sub_path`. `service_account` is the pods'
  ServiceAccount; unset, pods run as the namespace's `default` ServiceAccount. A name or path that
  collides with Legion's own is refused at load, naming both: a variable the runtime, the worker
  image's `ENV` or every launch sets, a volume name Legion uses, or a mount at, under or above a path
  Legion mounts, the image owns, or a tool runs from, or `workspace-init`'s state home
  (`/home/legion/.local/state`, where `workspace-init` keeps the XDG state it provisions the issue
  under: no role's agent keeps its Oh My Pi state there any more — every role's state home is under
  the issue's scratch on the issue's volume, which the `/legion` mount fence already covers — but a
  mount there would still land on what that init container writes, so it is refused here rather
  than left to fail in a pod).
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
  URL with no userinfo, a host that is a DNS name of ASCII letters, digits and hyphens (no `xn--`
  label, no trailing dot, a last label that is not a number), a dotted-quad IPv4 address or a
  bracketed IPv6 address, and any port from 1 to 65535: a rule narrower than the agents' URL
  parser, so a value load accepts never turns Dispatch off in an agent. The controller's
  `slack_post` refuses a reply linking an address inside the cluster.
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
- **AskGate's time limit** is the operator's too. Every pod loads pi-envoy, whose AskGate reviews
  each Dispatch write of a role's main agent before it posts. Its review must finish inside Oh My
  Pi's limit for one `tool_call` handler, `extensionHandlers.toolCallTimeoutMs`, and the gate holds
  its deadline 5 s under that limit. The limit is 30 s by default, so the deadline is then 25 s. A
  review of a long session often takes longer, a review that times out lets the write post
  unreviewed, and three timeouts in a row stop the gate for the rest of that session. Set
  `extensionHandlers.toolCallTimeoutMs: 120000` in the operator's `overlay.yml`, as the example
  overlay does (pi-envoy's README, "The time limit"). Legion's own turn-scoping overlay does not set
  it. Without it, the gate logs one warning at start, `AskGate: the review deadline is 25000 ms`,
  through Oh My Pi's logger. The gate binds in a pod because the pod has Dispatch; one without it
  gets no gate. The pod's `dispatch` command enforces the review: while the gate is bound, a
  gated write that is not one literal `dispatch` call the gate reviewed (a pipe other than one
  trailing `head` or `tail`, a script, `python3 -c`, an eval cell's process) is refused by the
  CLI, in block mode, for want of the single-use marker the gate mints. It reviews the writes of
  a cooperating agent and is no
  sandbox: it does not cover a direct HTTP request to Dispatch, an older or a different CLI, or a
  process of the pod's own user that removes the CLI's AskGate record or a marker, acts as another
  session id or state directory, or otherwise goes around the CLI (pi-envoy's README, "What
  AskGate is not").
- **`runtime.kubernetes.agent_secrets`** enrolls every pod the daemon runs with the secrets broker
  (the broker design's pod-enrollment plan), so an agent in a pod runs
  `agent-secrets <SECRET> -- <command>` and gets only
  that pod generation's grants. `url` is the broker's base URL (https, or http to a loopback
  address; an ASCII host), and `token_file` (required, absolute) is where the daemon's own pod
  mounts its projected ServiceAccount token for the broker's audience, `agent-secrets`, with an
  `expirationSeconds` of at most 3600: the broker refuses a token that lives longer than an hour
  from its issue
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
  other session (the broker's concepts page, "Owner and tier"). When the daemon's pods run as
  several accounts, the setting names each in a `legion-daemon=` entry of its own. The broker
  refuses to start with a namespace's `default` account in `BROKER_SERVICES`, since every pod there
  that names no account runs as it, so pods that receive service-owned secrets name a dedicated
  account. The daemon's own ServiceAccount, which logs in, is a different account from its pods',
  and it is named in `BROKER_SERVICE_LOGINS`, not `BROKER_SERVICES`; the broker refuses to start
  with an account in `BROKER_SERVICE_LOGINS` that is a namespace's `default` or one
  `BROKER_SERVICES` binds, since every pod running as it could then log in as the daemon.
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
  Anything in that role's namespaces that knows the broker's URL can renew it, though: the role's
  key directory is bound there and in no sibling's, the broker's projected token is mounted in the pod, the
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


#### Network shares

A volume may also be an existing network share, mounted into every pod beside Legion's own volumes —
a team NAS, a build cache, a Filestore instance:

| source | what it names | `runtime: kubernetes` |
| :--- | :--- | :--- |
| `persistent_volume_claim: {name: <claim>}` | a PersistentVolumeClaim the operator made in the daemon's namespace, bound to the share's PersistentVolume: one whose access mode lets pods on many nodes mount it, `ReadWriteMany` (a Filestore instance through its CSI driver, an NFS export) | supported |
| `nfs: {server: <IP or DNS name>, path: <export>}` | an NFS export the nodes reach, mounted by the node's own NFS client | supported |
| an inline `csi` volume | — | not offered |

`runtime: tmux` runs no pods and has no `runtime.kubernetes.pod` block, so a share is a
Kubernetes-runtime source alone; under tmux the panes read whatever the host has mounted.

Every issue pod mounts the share at once, and Legion pins no pod to a node, so the claim must be
`ReadWriteMany`: a `ReadWriteOnce` disk attaches to one node, and pods on any other fail to attach
it (`Multi-Attach error`), and a `ReadWriteOncePod` claim another pod holds refuses the boot in the
image probe. A `ReadOnlyMany` claim attaches when every mount of it is read-only, since the loader
then sets `readOnly` on the claim's volume source too.

The mount is read-only unless its `volume_mounts` entry says `read_only: false`; the source itself
takes no `read_only`. A share no mount writes is read-only at its source as well: the loader sets
`readOnly` on the claim's or the NFS volume's source, so the node mounts the export `-o ro` and the
kubelet changes no ownership on it. A writable claim whose CSI driver's `fsGroupPolicy` is `File`
has the group of every file on it set to the pods' `fsGroup` (1000) at each mount, as the kubelet
does for any such volume; the built-in `nfs` source and a PersistentVolume of it never do. On GKE Autopilot both sources are among the volume types it admits
(`persistentVolumeClaim` and `nfs`, [Autopilot security](https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security)),
and a gVisor pod mounts both: the node mounts the share and gVisor's gofer serves it to the sandbox.
An inline `csi` volume is not offered because a CSI driver runs outside gVisor's isolation with
privileged access to the node ([GKE Sandbox, External dependencies](https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods#limitations)),
so a pod naming an arbitrary driver would reach past the sandbox; a share behind a CSI driver is
mounted through a claim the operator made, whose driver the cluster's administrator chose.
`host_path` is refused too: Autopilot refuses a host path, and one would hand the agent the node's
files. A claim named as Legion's own
claims are (`issue-legion-…`, `cache-legion-…`, and `tree-legion-…` of the earlier layout) is refused at load, since every pod would then mount
an issue's workspace or a repository cache. The server must be an IP address or an ASCII DNS name with no
scheme, port or path, and neither the node's loopback nor an unspecified address, and the export a clean absolute path; the claim a Kubernetes name, named by
one volume only (the kubelet mounts a claim's volume once per pod, so two volumes of one claim
would let one's `read_only` decide the other's; mount the one volume at each path instead). A share
the pods cannot mount refuses the boot in the image probe, naming the volume and the share: a claim
the scheduler cannot find, is deleting or has lost its volume (`persistentvolumeclaim "<name>" not
found`, `… is being deleted`, `… bound to non-existent persistentvolume …`, at once, never counted as
a capacity wait), a mount the node ran and that failed (`… : mount failed: …`, refused on the
kubelet's first report: for a server that never answers or refuses the connection that report
already stands for about two minutes of mount.nfs's own retries, and an export the server denies or
lacks fails at once, so its first report is its one try; or a CSI driver's `… : rpc error: code = …`,
refused once the kubelet's retry, about half a second later, has met it again), and a share mounted but unreadable,
which gVisor reports as the container's failed start (`reading mounts file: …`). With two or more
claims in the block, a claim's failure names its PersistentVolume and every claim it may be bound
to, since the daemon reads no claim to tell which. A CSI failure whose gRPC code says the operation
may still be under way (`Aborted`, `DeadlineExceeded`, `Unavailable`, `ResourceExhausted`,
`Canceled`, the codes the kubelet's CSI client does not treat as final) is left to the attempt's
budget while the kubelet retries it, as is a failure the image pull after it shows a later try
cleared. The kubelet retries every failed mount with backoff; a refused share is refused even when
the network would have answered later, and the daemon exits, so whatever started it (its
Deployment, or the operator) starts it again. A claim that exists but is not bound yet is waited on through every attempt of the
probe's budget, then fails as any probe that never completed does, and a `ReadWriteOncePod` claim
another pod holds is refused at once. The daemon needs no right on
PersistentVolumeClaims for any of this: the pod names the claim and the kubelet mounts it. An `nfs`
volume is not among the Pod Security "restricted" profile's volume types, which GKE Autopilot's
own policy extends with it: a namespace that enforces `restricted` (`pod-security.kubernetes.io/enforce`)
refuses such a pod, and takes the share through a `persistent_volume_claim` bound to an NFS
PersistentVolume instead.
`scripts/e2e/share-volumes-kind.sh` is the live proof.

### Cutting over an instance from tmux to pods

This subsection is the retired TypeScript daemon's, its configuration block included; for the pods
Legion runs today see [Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

Drain every tree while it is still on tmux, stop the daemon, choose a new `state_dir`, and start
with `runtime: kubernetes`. The state file intentionally holds the controller's tmux locator beside
root and worker pod locators; the daemon routes them by process kind.

The Go daemon's cutover is the same drain and fresh `state_dir`; its
[Configuration](#configuration) says where its rules differ, and its
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

**`--resume` keeps the same-agent invariant, and the table is what keeps it.** The tmux runtime
`stat`s the recorded session file on the daemon host and refuses to spawn when it is missing (a
silent fresh agent would lose the session), and Oh My Pi itself does not refuse: verified against
the pinned build (`omp --resume=<missing path> --mode rpc`), it exits 0, answers `ready`, and runs
as a fresh agent, with no error frame and no mention of the file. Under Kubernetes there is no
file to `stat` and no init container that looks for one: every session is a row in the session
database, and the pod's disk, which dies with the pod, never holds a transcript. The role's own
launcher makes the check instead, inside the pod and before it starts the child: it looks the
recorded session up in the table through the mounted URL file and refuses the start when the table
holds none (`resume session <path>: the session table holds no such session`), which the daemon
counts as a launch failure, three of them retiring the claim. Should a resume reach Oh My Pi with
its row gone anyway, it starts a fresh session with a new id that the daemon refuses to register
(`409 Worker respawn must resume the same agent session`), so the pod exits and the launch failure
is counted there. Either way the respawn fails loudly rather than quietly starting a new agent
under the old role token (see [Selecting the store](#selecting-the-store)).

**On kind, let the node pull the image from GHCR by digest** (the package is public; a fresh node
pulled the 377 MB image in about 10 s). Do not `kind load docker-image` a digest-only reference: kind
imports it as an anonymous `docker.io/library/import-<date>` image, containerd then answers every pod's
pull for that digest with `image "docker.io/library/import-…@sha256:…": not found`, and neither
`crictl rmi` nor `ctr images rm` clears it — the cluster has to be recreated.

### Anatomy of a pod

This subsection is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

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

This table is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

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

This table is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

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

This subsection is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod), and for what an operator gives the live
runtime [Operator configuration](#operator-configuration).

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

This subsection is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod), and for the live rules
[Contract discipline](#contract-discipline), the Go daemon's.

For a daemon contract change, first merge the worker image and the two plugin releases, then set
`runtime.kubernetes.image` to its digest, install both plugin releases (`@sjawhar/pi-envoy`,
`@sjawhar/pi-legion`) in the Legion profile, and restart the daemon. The Go daemon's own contract
discipline — which processes a boot pass relaunches, how, and what it logs — is "Contract
discipline" under "Kubernetes runtime: the Go daemon on Agent Sandbox" above; this runtime does
none of it.

### RBAC the daemon needs

This subsection is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod), and for the live Role
[RBAC the Go daemon needs](#rbac-the-go-daemon-needs).

For the daemon's Role (LEGION-25), the verbs this runtime uses on core/v1 in its namespace:

This table is the retired TypeScript daemon's; for the pods Legion runs today see
[Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod).

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

Under either mode a controller reads its daemon's resolved settings — `max_fix_attempts`,
`worker_boot_timeout_seconds`, `admission_cap` and the rest of `legion.yaml`, with every credential,
credential source and credential file path left out — from `read_state`'s `settings` (contract 35,
LEGION-791), never from the ConfigMap in `legion-system`, which `legion-worker` may not read.

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
directory made under its state home, `/legion/scratch/state/controller`), on the daemon's
command, as an issue pod's launcher does each role's, so a relaunch in a healthy pod is a new
generation of that child rather than a new pod. Its role Secret,
`legion-<project>-controller-controller-boot`, holds the launcher's token and its gh bundle (each
owner's `github-hosts-<owner>` and `github-config-<owner>`, the review App's read-only `hosts.yml`
and `config.yml` for every owner of the project, and `github-default`, the first owner), refreshed
every minute as every role's are, and is the only Secret its launch writes: there is no
provisioning `-boot` Secret.
Its Sandbox owns a volume of its own, where an issue's owns none (`issue-legion-<project>-controller`, from
its one `issue` claim template, of `runtime.kubernetes.issue_volume` and `storage_class`), mounted
at `/legion`, which is the working root the controller runs in, so what one of its pods leaves
there the next one finds; its session is a row in the session database like every other, not
anything on that volume. It provisions no workspace: no `workspace-fetch`, no provisioning token. Its `gh`
bundle, projected read-only at `/var/run/legion/gh` by a `gh-controller` volume, acts as the review
App, refreshed every minute as every role's is; a failed review-App mint fails the launch naming
the role, and the keeper relaunches it. A project with no GitHub owner to mint for (none of its
repositories has an App) starts the controller anyway with `gh` acting as nobody, as `legion
controller start` does there: its role Secret records an empty `github-default`, its `gh-controller`
volume is an empty directory, and it holds owners from its next pod on. It runs no init container
(`controllerPod.initContainers`): nothing on its volume needs preparing before the launcher starts.
The pod
carries `legion.dev/project` and `legion.dev/role=controller` and no tree or issue label, so no
tree's cleanup lists it, and the boot census holds it to exactly the one launcher those labels
name and to the volume it owns. Under `runtime.kubernetes.agent_secrets` it is enrolled with the
secrets broker as an issue pod's roles are: it projects the broker's token, has a key directory of
its own, and its shim takes the three agent-secrets flags and its agent `AGENT_SECRETS_URL` and
`AGENT_SECRETS_KEY_DIR`. It runs as the operator's ServiceAccount (`runtime.kubernetes.pod`), the
same account as every issue pod, and it is the `legion-daemon` service's own pod because
`BROKER_SERVICES` maps that account to `legion-daemon`. A service may name several accounts, so a
controller given an account of its own keeps that service's secrets only when `BROKER_SERVICES`
also names that account for `legion-daemon`. The broker grants it, with no person asked, an agent
secret whose owner is `legion-daemon` and whose tier is `agent`, which is how the operator stores
`LEGION_SLACK_BOT_TOKEN`, the Slack app's bot token its `slack_post` replies with. Legion sets no
affinity on it, so under
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
directory there, `LEGION_GH_PATH=/usr/local/bin/gh`, `GH_TOKEN=""`, `GITHUB_TOKEN=""`, `GH_HOST=""`)
and the git credential entries every role of an issue pod is told
([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)), and nothing of
a tree, an issue or a checkout. Its system prompt is the controller's role
prompt, a part saying it runs headless in a pod, the daemon's `Design gate policy:` line, the
project's repositories sentence, the project's `Slack channels:` line after it when `slack_channels` names any, and the deployment
instructions. Its one container, the launcher, carries the
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
keeps it while the controller's claim is not retired, running or suspended, so a suspended
controller's session survives, and deletes it once the claim is retired. The sweep reads the claim
from the store again under the pod's launch turn, so a controller launched after the sweep read the
daemon's claims keeps its Sandbox. Either delete runs under the pod's launch turn and only at the
version of the Sandbox it was decided on: a controller relaunched into the Sandbox since has written
it, so the delete is refused, the daemon logs `sandbox runtime: kept a sandbox written since its
delete was decided; the orphan sweep decides it again` naming it, and the next sweep decides again.
The controller Sandbox a daemon made before the controller ran in a launcher pod (`role=controller`,
one `worker` container), and the one a daemon of the tree-volume layout made (a claim template named
`tree`), are refused at boot like every Sandbox of a layout before this one, each named: remove it
before enabling issue pods
([Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees), step 3).

**Reaching it.** Nobody types into the pod. A person reaches the controller through Dispatch (a
message to its session on the Agents page, a reply to its ask, a mention) or Envoy, and reads its
session with `kubectl -n <namespace> logs legion-<project>-controller -c controller` (its launcher's
log, which carries the shim's and Oh My Pi's) or its session in the session database, where every
session is a row rather than a file on any volume. A Send from the
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
registered, or none, is left as it is. The file store left a residual here — a controller whose
session died with its volume, switched back before its fresh launch registered, left the record
naming a dead session the switch back would not end — and the table closes it: a session outlives
the pod and the volume it ran on, so the claim this daemon stops still records the session the
record names. A daemon that cannot stop the claim
refuses to boot, naming
it (`stop legion-<project>-controller, …`), with the record untouched; one that stopped it but
cannot end its registration refuses naming it too (`end the registration of
legion-<project>-controller, …`), and the next boot, finding the claim retired, ends it. The
claim route refuses a launch of that claim on such a daemon (409, `legion-<project>-controller is a
launch of the daemon's own controller, and this daemon leaves the controller to its operator
(controller: operator)`), and the token of an earlier launch is an invalid boot token, so no pod of
the daemon's can replace the operator's registration. Then start the controller with `legion
controller start` ([Operator-launched controller](#operator-launched-controller)). Releasing the
Sandbox deletes the controller's volume with it, and with the volume the working root its pods
shared, but not its session, which is a row in the session database. So switching to
`controller: daemon` again relaunches the claim after the keeper's first one-minute wait, on fresh
budgets, and the controller comes back on the conversation it recorded, in a pod whose `/legion`
is empty. A `legion claims stop` of the controller under `controller: daemon` releases it the same
way, so the keeper's retry after it brings the same session back on the same empty root.

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
`max(worker_boot_timeout_seconds, 4 × 90 s)`, which is 360 s at the defaults. It is gone when the role
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
