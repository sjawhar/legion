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
  `packages/pi-legion` (the exact `bun pm pack` steps `release.yaml`'s `pi_envoy` and `pi_legion` jobs
  run), unpacked at `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`, the two roots a pod names as Oh
  My Pi's explicit extensions, the Envoy plugin first, with extension discovery on. Neither is linked
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
- a generic toolchain for the repositories the workers work, specific to none of them: `uv` and `uvx`;
  `node`, `npm`, `npx` and `corepack` from Node 24 LTS, with `pnpm`, `pnpx`, `yarn` and `yarnpkg` linked
  to corepack's shims (the links `corepack enable` would write, which uid 1000 cannot); the Go toolchain
  at `go.work`'s version — `go` and `gofmt`, the tree at `/opt/go` (its `GOROOT`, which `go` finds by
  resolving the `/usr/local/bin/go` symlink; the image sets no `GOROOT`); and the AWS CLI
  v2's `aws` — all at `/usr/local/bin`, which is on the image's `PATH` and every pod's. Each is a pinned
  release whose linux/amd64 archive the build checks against a pinned SHA-256 before unpacking it (the
  `ARG`s at the top of `worker.Dockerfile`). A corepack shim runs the version a project's
  `packageManager` names, or else the default that corepack ships (the image sets
  `COREPACK_DEFAULT_TO_LATEST=0`, so never npm's newest release), fetched on first use into the user's
  corepack cache. Node, Go and the AWS CLI keep their trees at `/opt/node`, `/opt/go` and
  `/opt/aws-cli`, outside `HOME`, so no volume a pod mounts under `HOME` shadows any of it;
- what the capability check (`packages/daemon/internal/capabilities`, which `legion probe-image` runs —
  [the probe subsection](#the-image-is-probed-before-it-publishes)) requires of the image, each outside
  `HOME` for the same reason: the language servers Oh My Pi's LSP tools start when a working directory
  carries their root markers — `gopls` (built static in the Dockerfile's `go` stage, at
  `/usr/local/bin/gopls`), `typescript-language-server` and `pyright-langserver`, the latter two npm
  global packages under `/opt/node/lib/node_modules` with their launchers (and TypeScript's `tsc` and
  `tsserver`, and the `pyright` CLI) linked at `/usr/local/bin`. TypeScript is pinned to the version
  this repository's lockfile holds, since `typescript-language-server` runs `tsserver`, which TypeScript
  7 no longer ships; the server resolves the TypeScript installed beside it unless the repository's own
  `node_modules` holds one, which it prefers. From Debian trixie's archive, at `/usr/bin`: `python3`
  (the interpreter Oh My Pi's Python eval runs — `omp setup python --check` answers `available: true`
  on it), `curl`, `wget`, and `chromium` for the browser tools, installed without its Recommends (no
  setuid sandbox: Oh My Pi launches it with `--no-sandbox --disable-setuid-sandbox`). Oh My Pi picks
  its browser from `PUPPETEER_EXECUTABLE_PATH` first, else the first Chrome or Chromium it finds on
  `PATH`, and downloads Chrome for Testing into its cache only with none of those — so the image's
  `chromium` is the one every worker gets, with nothing downloaded, and the probe runs that same
  resolution. The image sets no `PUPPETEER_EXECUTABLE_PATH`: a variable the image's `ENV` sets is one
  `runtime.kubernetes.pod.env` is refused, and an operator's pod env naming another browser is what the
  probe checks (it runs that executable), not something to refuse. `uv` still installs each project's own Python, from its
  `.python-version` or `requires-python`, the first time the project runs (`uv sync`, `uv run`): in a
  Sandbox pod that is on the tree volume ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod));
  elsewhere it is uv's default, `~/.local/share/uv/python`. The image's `python3` is for Oh My Pi's
  own eval, not a project's interpreter.

It runs as user `legion` (uid 1000, declared numerically so `runAsNonRoot` can verify it from the image
alone) with `HOME=/home/legion`, which must be writable (OMP writes sessions, logs, and `models.db` under
`~/.omp/profiles/legion`). Mount writable volumes below the profile directory, never at `/home/legion`
itself: everything the image-time probe proved lives under `HOME` — the CodeGraph plugin's link and the
lock at `~/.omp/profiles/legion/plugins`, the natives at `~/.omp/natives` — and a volume at `HOME` (an `emptyDir`,
or a `HOME` volume under `readOnlyRootFilesystem`) shadows all of it silently. It is `linux/amd64` only:
the OMP fork release has no linux/arm64 build. One commit ⇒ one image: Legion's own plugins come from
the commit, never from an npm release; what the image does take from npm (the CodeGraph plugin and
CLI, TypeScript and the two npm language servers) is pinned by the `ARG`s at the top of
`worker.Dockerfile`.

### Image size and pull time

A node that has never run the image pulls it whole before the pod's init containers start, so the
image's compressed size is a cold launch's first cost. Before the capability tools above,
`ghcr.io/sjawhar/legion-worker:1.18.1` was 12 layers and 369 MiB compressed (386,510,448 bytes). With
them it is 22 layers and 1007 MiB compressed (1,056,280,622 bytes, the `sha-ff33c8c9fb1b` build of the
pull request that added them): the Debian layer grows from about 30 MiB to 307 MiB, almost all of it
Chromium and the library closure it depends on; the Go toolchain is 64 MiB and `gopls` 20 MiB; Node's
tree with the two npm language servers is 66 MiB. How long a node takes to pull that is not
recorded here: the stage 4a launch below is where it is measured. The bound that pull must fit,
with the init containers' clone and Oh My Pi's boot after it, is the registration deadline:
`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`
(`packages/daemon/internal/config/config.go`; 120 s × 3 = 360 s by default), which the Sandbox
runtime widens by its provisioning bound while the claim is still launching (`armRegistration`,
`packages/daemon/internal/supervise/machine.go`); a pod that has not registered by then is retired and
a launch failure counted. The stage 4a harness's `image-probe` and `root-ready` checks
(`scripts/e2e/README.md`) are where a launch of the published image on the production cluster is
measured. This change moves no deadline.

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
profile's lock, and `go`, `curl`, `wget`, `python3`, `node`, `bun` and `uv` on `PATH` with `go version`
running — and prints the table, one `probe-image: capability <name>: <status> (<detail>)` line per
row, before the OK line:
`probe-image: OK (/opt/omp/bin/omp) session-storage=probed extensions=discovered agent-models=skipped capabilities=checked model-fallback=on daemon-api-version=<N>`
(`model-fallback=on` is Oh My Pi's own default for `retry.modelFallback`: the build runs under no operator overlay, where a probe pod reads the operator's value).
An image row the image carries prints `present` with its evidence (`codegraph`'s: the CLI on PATH and
the plugin enabled in the profile's lock, which a pod's launch loads with extension discovery on). A live row prints
`live` with the check still to prove it, a deployment row `reported`, a withheld row `withheld` with
its ruling, and an image row the image lacks `missing` with why.
A build whose image lacks a capability fails, the probe naming every missing one. The daemon's Agent Sandbox runtime runs the same command in a probe
Sandbox, `legion-probe-<project>-<digest12>`, with its own contract, under the operator's pod, at every
boot, and requires `agent-models=resolved`: each agent's model resolves, with a working key, as the task
tool resolves a subagent's (`packages/daemon/internal/runtime/sandbox/probe.go`). It requires
`capabilities=checked` too: an image whose `legion` predates the capability check prints no such mark and
is refused naming that (`Succeeded without checking the capability list (its legion CLI predates the
check): build the image from this daemon's commit`), and a probe pod that exits 1 on a missing capability
is refused with its log tail, which names it (`capability <name> is missing: <detail>` under the table's
`missing` row; the stage 4a harness's `image-probe-capability-negative` check drives that with an operator
pod env `PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium`). The runtime keeps the line's `model-fallback`
mark for the deployment's capability report ([The deployment's capability report](#the-deployments-capability-report)). To run it yourself:
`docker run --rm ghcr.io/sjawhar/legion-worker@sha256:… probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models`
(both roots are required: the Legion and Envoy plugin roots a Sandbox pod loads the plugins from, so the probe loads them the same way; without
`--skip-agent-models` it also resolves each agent's model, which needs the operator's model roles).
A step of its own, before that final one, runs every toolchain command listed above as `legion`, from
the image `PATH` — the language servers (`gopls version`, `typescript-language-server --version`,
`tsc --version`, and `pyright --version` for `pyright-langserver`, which takes no `--version`), `go
version` with `go env GOROOT` required to be `/opt/go`, `gofmt`, and trixie's `python3`, `curl`, `wget`
and `chromium --version` included — the corepack shims fetching their shipped default pnpm and yarn
into a scratch directory the step removes. It reruns only when the toolchain or an earlier layer
changes, so a commit that only rebuilds `legion` needs no package registry.
To check a published image's toolchain end to end, a project Python install included:
`docker run --rm --entrypoint sh ghcr.io/sjawhar/legion-worker@sha256:… -c 'uv --version && node --version && npm --version && aws --version && go version && gopls version && python3 --version && chromium --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.

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
PR head, never the ephemeral merge commit); `<legion version>` only on `main` when the `legion` job
released that version in the same run. Runs from any other ref publish the `sha-` worker-image tag
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
the `legion` job on every `main` push that touches any file the image builds from, (2) on every head of a pull
request against `main` whose diff touches any of those files — building the PR head and publishing `sha-`
only — and (3) by `gh workflow run worker-image.yaml --ref <ref>` once the workflow exists on `main`. The
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
(`--failed` keeps the `legion` job's recorded outputs; a whole-run rerun of a `release.yaml` call re-executes
`legion` against its own tag and empties `legion_version`) or by pushing the branch again.

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

Only the transcript moves. Tool artifacts and image blobs stay under the agent directory on local disk
(`~/.omp/profiles/legion/agent/…`), as do OMP's logs and `models.db`; `.legion/` handoffs live in the
repository. A pod that dies loses those local files as it does today — the conversation it does not.
The workspace's CodeGraph index, `.codegraph/` in the workspace on the tree volume (185 MB for this
repository), is built by a role's shim after its Oh My Pi starts ([Anatomy of a Sandbox
pod](#anatomy-of-a-sandbox-pod)) and outlives the pod with the workspace.

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
    session_store: postgres        # pvc (the default) keeps sessions on the tree's disk volume
    session_dsn_secret: SESSION_DSN  # the providers-Secret key that holds the connection URL
```

`pvc` keeps each conversation a file in the `sessions` directory of the tree's volume. `postgres`
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
told `LEGION_EXPECT_TREE_VOLUME` for a session, and the controller's pod is not told
`LEGION_RESUME_SESSION_FILE`. So a claim resumed on a new volume (a tree drained or suspended, its
volume deleted, then resumed or re-admitted) provisions its workspace from the issue's pushed
branch (`legion/<issue>`, or `main` when none was pushed) and continues its own session; anything
it had not pushed is gone with the old volume. For the same reason a child issue re-admitted as a
root of its own keeps its roles' sessions under `postgres` (`Machine.Retree`, which drops them only
under a runtime that keeps sessions on the tree's volume, `SessionsOnVolume`).

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
to legion/<KEY> (main if nothing was), and anything you had not pushed is gone. …`
(`packages/pi-legion/src/workspace-recreated.ts`, a steer that starts no turn), with an id of its
own process in the message's `details`. A prompt, the daemon's next task or a person's direct
message that pi-envoy delivers as a user turn, first recovers a failed last turn, and Oh My Pi
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

A resumed session also restores the model it last used, and since Oh My Pi 18.8.3, the release
every pod runs under either store, `--mode rpc` refuses to resume one whose model the profile no
longer has (`Could not restore model <provider>/<model>`, exit 1). So a change to the pods' model
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
   Sandbox's pods were: its pod template's node selector, tolerations, priority class and service
   account. Before anything it refuses a project whose Sandboxes are issue pods (a later release),
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
shares the issue's checkout, the tree volume, the sessions directory and the pod's network; each
role keeps its own state directory, agent-secrets key and launch credentials. A role's Secret holds
only its launcher token, projected into that role's container alone and bound to the pod's uid; the
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
Secret and, for a root, the tree PVC; each Sandbox carries the tree's `legion.dev/tree` label, which
is how the tree's cleanup finds them.
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
issue's Sandbox to `Suspended` and waits for its pod to disappear. The Sandbox, tree volume and
recorded sessions remain until linger cleanup. A daemon restart retries the stored effect.
Re-admission during linger reuses those resources and resumes the recorded sessions; the issue
pod's launch turn orders a concurrent resume after any suspension already in flight.

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
listing them from the API by the tree's label: each child issue's Sandbox first, each awaited
until the API no longer has it, and the listing read again until it holds none; only then the
root's. The root Sandbox delete carries its UID and resourceVersion plus `foreground` propagation,
and a delete the API refuses because the Sandbox changed since the listing lists again after a
short wait. Agent Sandbox v1.0.3 creates the root tree PVC with that Sandbox as its controller
owner and `blockOwnerDeletion: true`; foreground deletion keeps the
owner visible until Kubernetes garbage collection deletes that blocking dependent. The restricted
daemon has no PVC API verb, so it confirms the root Sandbox is NotFound before confirming the
durable cleanup; it does not read or delete a PVC. The live runtime proof must observe the actual
PVC owner reference and its absence after foreground deletion, rather than infer that result from
labels or a generic garbage-collection rule.

An operator-created tree has no workflow record: its stored operator authority, not a zero or
sentinel generation, selects the operator cleanup entry point, which then applies the same
child-first/root-last cleanup without manufacturing a workflow issue. A tree closed before any of
its Sandboxes was created lists none and confirms its reservation without deleting anything.

A child of a closed tree re-admitted as a root of its own keeps its key, so its claims and its
Sandbox's name. Its old tree's cleanup does not wait on a claim of it that runs nothing; each of
its claims is re-pointed to its new tree before its first start there, and binds that tree's epoch;
and its root launch replaces the Sandbox its old tree suspended (old tree label, no tree volume)
with one of its own tree. A Sandbox of the old tree that still runs roles is not replaced: the
launch is refused until they stop.

Before the daemon opens its store, so before any schema write, image probe or reconcile, it checks
that Agent Sandbox is installed and refuses a namespace that holds a Sandbox of its project whose pod
is not exactly the launchers its labels name — six for an issue's (`legion.dev/issue`), the
controller's alone for the project controller's (`legion.dev/role=controller` with no tree) — naming
every such Sandbox, each with its reason, and how many there are. That catches every per-claim
Sandbox of the layout before issue pods, running or suspended, and the controller Sandbox a
`controller: daemon` daemon made before the controller ran in a launcher pod (`role=controller`, one
`worker` container). Once the store opens and before it migrates, it refuses a claim that still
records a Sandbox locator of the layout before issue pods (no pod uid, container or generation),
that earlier controller's claim included. A deployment of the earlier release clears both before it
upgrades ([Upgrading a deployment with running trees](#upgrading-a-deployment-with-running-trees)).
The daemon runs on a host its pods can reach and serves the worker stream they dial. The controller
is `legion controller start` on the operator's machine, or, under `controller: daemon`, a pod of its
own with one launcher that the daemon launches ([The controller](#the-controller)).

### Upgrading a deployment with running trees

This section is for a deployment that runs the release before issue pods (`legion-v10.0.0`), whose
every role runs in a Sandbox of its own. A daemon at this release refuses to boot while that
deployment's work is still in place:

- **A per-claim Sandbox** in its namespace, running or suspended, is refused by the census before
  the store opens (`rejectLegacyIssueSandboxes` in `internal/runtime/sandbox/sandbox.go`, run by
  `run` in `internal/daemon/daemon.go` before `store.Open`).
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
carries over (below), so the upgrade drains the deployment while it still runs the earlier release:

1. **Drain every tree.** Move each tree's root issue out of the workflow on Dispatch (`done`,
   `backlog`, `icebox` or `triage`) and let its linger expire, which closes the tree. A tree no
   workflow issue backs, one an operator spawned, is closed with `legion claims close` on its root
   claim; the earlier release refuses that for a workflow issue's tree, which the workflow closes.
   Each claim of a closed tree is released and retires, and a retired claim records no locator.
2. **Let the cleanup finish.** On the earlier release, releasing a claim deletes its Sandbox by
   name, and the root's Sandbox takes the tree volume with it (`Release` in
   `internal/runtime/sandbox/sandbox.go` at `legion-v10.0.0`). Wait until every Sandbox is gone, or
   delete one left behind once no claim of it runs.
3. **Clear the controller's claim, if `controller: daemon` ran.** Set `controller: operator` and
   boot the earlier release once. It stops the controller its earlier boot launched, logging
   `controller: stopping the controller an earlier boot under controller: daemon launched; this
   daemon leaves the controller to its operator`: the stop deletes the claim's Sandbox, and with it
   the controller's volume and session, and retires the claim, so `legion claims list` shows
   `legion-<project>-controller` `retired`. In the cluster each of these boots is a configuration
   change of its own through the deploy path.
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
   - the daemon at this release;
   - `runtime.kubernetes.image` pinned to a worker image built from it, since the image probe
     refuses a worker image whose `pi-legion` declares another daemon API contract (`ProbeImage` in
     `internal/runtime/sandbox/probe.go`); and
   - for an operator-launched controller, every operator's `legion` and `pi-legion` at this
     release's daemon API contract (`DaemonAPIVersion` in `internal/api/version.go`).
     `legion controller start` refuses a `pi-legion` speaking another contract (`probeAndMint` in
     `cmd/legion/controller.go`), and the daemon refuses a `legion` or `pi-legion` of another
     contract with 409 (`internal/api/controller.go`). That `pi-legion` release must be published
     before the rollout.

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
(`SandboxName` and `TreeClaimName` in `names.go` at `legion-v10.0.0`), and this release
`tree-legion-<project>-<root issue>`, from the root issue's Sandbox (`SandboxName` and
`TreeClaimName` in `internal/runtime/sandbox/names.go`). Suspending the tree's claims on the earlier
release instead (`legion claims suspend`) keeps their Sandboxes, `Suspended`, and the census refuses
a suspended per-claim Sandbox all the same.

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

### Configuration

```yaml
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:<64 hex>   # digest only
    storage_class: gp2          # required: the tree volume's class (the cluster has no default)
    tree_volume: 20Gi           # default 20Gi
    kubeconfig: /home/ubuntu/.kube/legion-daemon-production   # relative to legion.yaml's directory
    context: legion-daemon@example   # required when the kubeconfig sets no current context
    scheduling:                 # optional, beyond the Legion pool the runtime always selects
      node_selector: {}         # merged over legion.dev/pool=legion, which it may not name
      tolerations: []
      priority_class: legion
    resources:                  # optional; a role absent here gets no requests or limits. A role is covered for the
                                # resource-limits capability when it sets CPU and memory in both requests and limits
      tester: { requests: { cpu: 2, memory: 4Gi }, limits: { cpu: 4, memory: 12Gi } }
      controller: { requests: { cpu: 250m, memory: 1Gi }, limits: { cpu: 1, memory: 2Gi } }   # controller: daemon only
    pod:                        # the operator's: env, volumes, mounts, ServiceAccount (below)
      service_account: legion-worker
      env: { PI_CONFIG_FILES: /etc/legion-operator/overlay.yml }
      volumes: [...]
      volume_mounts: [...]
bind: <the daemon host's own address, or 0.0.0.0 once advertise_host names one>
advertise_host: <optional: a stable Service name, e.g. legion-daemon-<project>.<namespace>.svc, every pod dials instead of bind, at worker_stream_port>
worker_stream_port: 13371
daemon_url: http://<the address pods reach the daemon at>:13370
capabilities:                   # optional: the deployment capabilities decided by name, with the reason (The deployment's capability report, below); a gap is reported, never refused
  decided: { secrets: "<reason>", model-fallback: "<reason>", resource-limits: "<reason>" }
```

`packages/daemon/internal/config/kubernetes.go` reads the block and refuses, naming the key:
- anything it does not model: `role_profiles`, since each role's requests and limits go under
  `resources`;
- a `resources` key that is neither a workflow role nor `controller`, and `resources.controller`
  unless `controller: daemon`, the one setting under which the daemon launches the controller's pod;
- `gateway`, removed with LEGION-270: a pod's model route is the operator's `pod`;
- `agent_secrets.operator`, removed with LEGION-664: the daemon's machine login is the
  `legion-daemon` service's, which anyone signed in to Dispatch approves, so it names no approver;
- an image that is not pinned by digest;
- `session_store` other than `pvc` or `postgres`; `postgres` without `session_dsn_secret`, or with
  one that is empty or not a Secret data key (`[-._a-zA-Z0-9]+`); and a `session_dsn_secret` under
  `pvc` (an inert key is refused, never ignored). At boot the daemon also refuses a
  `session_dsn_secret` that is the providers Secret's `NATS_NKEY_SEED`, a `provider_keys` entry
  that reads the same key (the shim would export the URL into Oh My Pi's environment), and an
  operator's `pod.env` or `provider_keys` naming `OMP_SESSION_STORAGE` or
  `OMP_SESSION_SQL_DSN_FILE` ([Selecting the store](#selecting-the-store)).

Legion holds no model route. `pod` is the operator's: `env`, `volumes` (each a `secret`,
`config_map` or `projected` source), `volume_mounts` and `service_account`, added to every pod, the
image probe's included, and refused where they name a path or variable of Legion's own or the
worker image's. `provider_keys` names keys of the providers Secret, which every pod mounts, those
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
with no decision — `resource-limits` while a role reserves no CPU and memory, and `secrets` when no
broker is configured — in boot's words (`capability <name> is open: <detail>; to record a decision,
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
connection authenticates as. That user's grants, which `legion-pane` does not hold, are exactly
what the connection uses: publish `$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS`,
`$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>`, `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>`,
`$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>` and `$JS.ACK.ENVOY_NOTIFICATIONS.>` (its two
durable consumers, `legion-go-<project>-dispatch` and `legion-go-<project>-github`), and subscribe
`notifications.envoy.exceptions.notifications.role.>` and `_INBOX.>`. It publishes on no core
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
reports the daemon's PID alive but not yet answering. A pane's own `bash` calls (each one mints
its own grant first), `legion credential`, `gh`, `handoff complete` and `controller start` all
wait on that same API, so none of them succeeds until the daemon actually serves.

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
every worker is a full agent), 19 rows, each checked at one site. The image rows (`eval-js`,
`eval-python`, `browser`, `lsp`, `codegraph`, `skills`, `toolchain`) are `legion probe-image`'s
([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)), `present` once
the probe passed; the live rows (`subagents`, `web-search`, `mcp`, `repository-extensions`,
`dispatch-envoy-tools`, `github`) are to be proved by a live check against a running pod —
dispatch://LEGION-633's integration check and dispatch://LEGION-629's checks, none of which runs
yet, so each reads `live` with the check it awaits, never as proved; the withheld rows carry the ruling that keeps them from
every worker (`network`: dispatch://LEGION-5, the pod is the boundary; `operator-setup`:
dispatch://LEGION-200, Legion owns its dependencies; `production-identities`: dispatch://LEGION-551
and dispatch://LEGION-205); and the three deployment rows are the daemon's to measure from its own
configuration, since no image or pod can show them:

- `secrets`: `runtime.kubernetes.agent_secrets` is configured and the daemon's broker login is
  `issued`. Under tmux it is open unless decided: no process is enrolled with the broker.
- `model-fallback`: the pod's Oh My Pi has `retry.modelFallback` true, as the probe's OK line's
  `model-fallback` mark reports it; under tmux the plugin gate reads the host's Oh My Pi once the gate
  has passed. The example overlay turns it on ([Operator configuration](#operator-configuration)).
- `resource-limits`: every workflow role — and the controller's, under `controller: daemon` — sets
  CPU and memory in both requests and limits under `runtime.kubernetes.resources`
  (`config.RoleResources.Reserved`); the row names each role that does not. Under tmux it is open
  unless decided: a pane has no requests or limits.

A deployment row is `present` when the deployment satisfies it, `decided` when `legion.yaml`'s
`capabilities.decided.<name>: "<reason>"` records a decision on it (the report shows the reason in
the gap's place; a decision on a satisfied row is moot and the row reads present), and `open`
otherwise, carrying the line that records one. A name that is not one of the three is refused at
load naming them; a blank reason too. The report appears in four places: the daemon's log, one
warning per open row at boot and again at the first controller tick after the set of open rows
changed (the tick asks only when it wakes the controller — one registered, no tick pending — and the
broker login reaching `issued` is the one change a running daemon sees), as `capability <name> is open: <detail>;
to record a decision, add to legion.yaml: capabilities.decided.<name>: "<reason>"`; `legion state
--json` under `capabilities` (daemon API contract 16: every row as `{name, status, detail,
decision?, configLine?}`, `status` one of `present`, `installed`, `unchecked`, `live`, `withheld`,
`decided` or `open`, the image rows `present` once the probe passed and `unchecked` before it or
under tmux);
the controller's `tick` notice, whose `openCapabilities` names the open rows so the day's report
names each gap (`skills/legion-controller/SKILL.md`); and `legion start --check-config`, which prints
after its OK line the rows the file alone leaves open. Nothing refuses to start over a gap: a
daemon that would not run its pods would itself keep workers from working, so a gap is the
operator's to close or to decide, by name and with the reason. The example `legion.yaml`
(`deploy/kubernetes/daemon/legion.yaml.example`) reserves CPU and memory for every role and decides
`secrets`.

### Anatomy of a Sandbox pod

Each Sandbox carries `legion.dev/project`, `legion.dev/tree` and `legion.dev/issue` (`names.go`):
one Sandbox per issue, shared by every role that works it. The pod template (`manifest.go`) has
two init containers and one container per role (`claim.Roles`: architect, planner, implementer,
tester, reviewer, merger), named for its role:

1. `workspace-fetch` clones the repository into the pod's feed. It is the only process that holds
   the provisioning token ([Trust model](#trust-model-the-provisioning-token)).
2. `workspace-init` provisions the tree volume's shared clone and the issue's jj workspace from the
   read-only feed. The workspace starts at the issue's branch, `legion/<KEY>`, which the daemon
   created on GitHub at `main` before the issue's architect or planner started.
3. Each role container runs `legion launcher --connect tcp://<advertise_host, or bind with none
   set>:<worker_stream_port> --token-file … --sandbox <name> --role <role> --private-dir …`: PID 1
   of that role, starting and stopping the role's `legion worker-shim` (Oh My Pi) child on the
   daemon's command, never a worker process of its own.

The tree volume is the root Sandbox's `volumeClaimTemplates` entry, and each issue's Sandbox
references that claim by name. Every role container mounts it at `/legion`, and again at Oh My Pi's
sessions directory through a `subPath`, so a session survives its pod. Each role's Secret holds only
that role's launcher token, projected read-only into the role's own container; the issue's `-boot`
Secret holds the provisioning token, projected into `workspace-fetch` alone. The operator's volumes and mounts join every role container's, and the
providers Secret's configured keys when there are any, with its `NATS_NKEY_SEED` key when the daemon
has a NATS nkey seed. Each role's private and state directories, `/tmp` and the XDG config home are
in-memory, one set per role so no role's launcher or state collides with a sibling's.

Every role container is told `UV_PYTHON_INSTALL_DIR=/legion/uv/python/<issue>` (the issue key as a
DNS label), `UV_CACHE_DIR=/legion/uv/cache` and `UV_LINK_MODE=copy`. uv keeps the Pythons it installs and its cache
on the tree volume beside the workspaces, so a project's `.venv`, which links to its interpreter there,
runs as it is in every later pod of the issue, and every pod of the tree reuses the packages an earlier
pod downloaded. uv's own file locks stop at the pod (a gVisor pod's lock reaches no other pod), so two
pods that first install one Python into a shared directory at the same moment can each delete the
other's interpreter. Each issue therefore gets its own Python directory, which only that issue's pods
share, at the cost of one download per issue. uv copies each package from the shared cache into a
`.venv`. With the cache and the `.venv` on one filesystem it would otherwise hardlink them, and an edit
made in place inside one workspace's `.venv` would change the cache and every other `.venv` of the tree
that installed the package, including ones installed later. So each `.venv` is a full copy of its
packages on the tree volume, beside the cache, and a deployment sizes `tree_volume` for one copy per
workspace of a tree, and for one CodeGraph index per workspace (`.codegraph/`, 185 MB for this
repository). `uv cache clean` and `uv cache prune` remove cache entries under a lock that also
stops at the pod, so neither may run while another pod of the tree is using uv.

Each role's shim is started with `--warm-codegraph`: once its Oh My Pi has written its first frame —
its extensions loaded, its RPC loop serving — the shim builds the workspace's CodeGraph index in the
background (`codegraph init`, or `codegraph index` to repair one an earlier build left partial, as
`codegraph status --json` decides). Nothing waits on it: not the launch, not the role's registration,
not a prompt. Six role shims share one workspace, and a draining pod can overlap its replacement, so
the warm-up holds a lease at `.codegraph/legion-warm.lock` for its run — an exclusive create whose
holder refreshes its mtime every 10 s, taken over once it is 60 s stale, the takeover itself under a
second exclusive create beside it so two shims that both read one stale lease never both build — and a
shim that finds the lease held leaves the build to its holder (a pod's `flock` reaches no other pod
under gVisor, so the lease is a file, not a lock). A stop mid-build — the launcher's one SIGTERM to
the role's process group — ends Oh My Pi and the `codegraph` child together, and the shim exits only
once the warm-up has released its lease, inside the pod's `terminationGracePeriodSeconds`
(`worker_stop_timeout_seconds`, which the launcher passes the shim as `--stop-grace`), so the
relaunch is never told a live build holds the workspace; it runs the warm-up again, and `status` on
the volume's existing index answers complete, so nothing runs, or reports the index the stop left
partial, which `codegraph index` repairs. The tester's `affected` and
the reviewer's `impact`/`callers` queries answer once `codegraph status --json` reports
`index.state: "complete"`; before that a role falls back to grep, as its prompt says. The init
containers never call `codegraph`.

Every pod runs:
- with `runtimeClassName: gvisor`;
- with `serviceAccountName` set to the operator's `pod.service_account` (the namespace's default
  when it names none) and `automountServiceAccountToken: false`;
- under Pod Security "restricted": non-root user 1000 on the pod, and on each container no
  privilege escalation, ALL capabilities dropped and the RuntimeDefault seccomp profile;
- on the Legion pool, with its node selector and toleration;
- annotated `karpenter.sh/do-not-disrupt: "true"`.

The project controller's Sandbox under `controller: daemon`, `legion-<project>-controller`, is the
same pod with a one-role list: it carries `legion.dev/project` and `legion.dev/role=controller` and
no tree or issue label, one init container (`workspace-init controller`) and one launcher container,
`controller` ([Daemon-launched controller](#daemon-launched-controller)).

The image probe runs as a Sandbox of its own, `legion-probe-<project>-<digest12>`, with
`shutdownPolicy: Delete` ([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)).

### A pod whose address moved

A role process's addresses are fixed when it is launched, and nothing changes a running process's
argv or environment. The daemon hands every role process six from its configuration, and the
daemon-launched controller, which never enrolls with the secrets broker, every one but
`AGENT_SECRETS_URL`, which it is compared without. The
worker stream listener, `tcp://<advertise_host, or bind with none set>:<worker_stream_port>`, is in
every launcher container's command (item 3 of the anatomy list above), so it is fixed for the pod's
life: a launcher dials it and never redials elsewhere. The other five reach a generation's Oh
My Pi in the start command its launcher receives, never in the pod spec, so they are fixed for that
generation's life: `LEGION_DAEMON_URL` (`daemon_url`), `ENVOY_NATS_URL` (`nats_urls`), `ENVOY_URL`
(`envoy_url`), `DISPATCH_URL` (`dispatch_url`) and, for a workflow role, `AGENT_SECRETS_URL`
(`runtime.kubernetes.agent_secrets.url`). Each moves when the daemon restarts with its key changed,
and with no `advertise_host` set the stream also moves when the daemon restarts on another host. A
daemon that restarts re-adopts each live claim by its recorded locator (the boot orphan sweep), and
re-adoption alone would leave the role holding the old addresses: a launcher dialling a stale
stream never reaches the new daemon, and a stale `LEGION_DAEMON_URL` fails every call the agent
makes to the daemon's API (its credential helper, `legion gh`, its phase completion) while its
stream still works.

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
it. The controller's pod, whose one launcher dials a stale stream, is replaced the same way at its
relaunch; it never enrolls, so the broker's settings never replace it. A role whose environment
alone moved starts its next generation in the same pod, handed the current addresses, and the pod
and its other roles are left as they are. The stale observation is never charged, since the
process did nothing wrong; a relaunch the runtime refuses is charged as any launch failure is.
A turn the stale process was in used the addresses it holds and ends with the relaunch, so the turn
is lost: its task goes back to waiting and is sent again once the relaunched agent is ready, the
recovery a death in a turn gets. A claim whose suspension was held for that turn's end is suspended
instead, as it is when its process dies.

The record costs one API write per generation started, the annotation's patch (844 bytes in the
runtime's tests, with Dispatch and the broker unset), where a start in a running issue pod made none
before. Stage 4b drives both cases with real agents on the cluster: `address-moved-env-same-pod`
respells `envoy_url`'s host, and `address-moved-stream-new-pod` swaps the daemon's API and
worker-stream ports ([`scripts/e2e/README.md`](../scripts/e2e/README.md#stage4b-sandbox-treesh)).

### A shell on the tree volume

Some of provisioning's refusals name `jj` commands against the tree's shared clone,
`-R /legion/repos/github.com/<owner>/<repo>` (`packages/daemon/internal/workspace/bookmark.go`):
- a local bookmark deleted and never pushed: restore it, cancel the deletion, or start from main;
- a conflicted local bookmark: keep an added commit, or start from main.

The refusals for an origin row that racing fetches conflicted or moved name no command, because
provisioning again settles them.

Each refusal is in the failing pod's `workspace-init` log
(`kubectl -n legion logs <pod> -c workspace-init`), and the daemon's log quotes its tail. Nothing
is registered before a refusal, so the pod's next attempt refuses again until the operator acts.
The clone is only on the tree volume, so the commands run in a pod that mounts it, from the
operator's context: the daemon's identity creates no pod and has no exec
([RBAC](#rbac-the-go-daemon-needs)).

**A pod of the tree is running.** Each of its role containers mounts the volume at `/legion`:

```sh
kubectl -n legion get pods -l legion.dev/tree=<KEY> --field-selector=status.phase=Running
kubectl -n legion exec -it <pod> -c architect -- sh
jj bookmark list --all-remotes legion/<KEY> -R /legion/repos/github.com/<owner>/<repo>
```

The shell runs as the tree's agents do, user 1000 under gVisor, with nothing they lack.

**No pod of the tree is running**, as when the only pod is the one whose `workspace-init`
refuses: mount the tree's claim in a pod of your own, `tree-<root Sandbox>`, e.g.
`tree-legion-<project>-<root issue>`
(`kubectl -n legion get pvc -l legion.dev/tree=<KEY>`). The claim is `ReadWriteOnce`, so the pod
must land on the node where the tree's pods hold it; the preferred affinity below puts it there.
Delete it before the tree's next pod starts elsewhere, because that pod cannot attach the volume
while this one holds it:

```yaml
apiVersion: v1
kind: Pod
metadata: { name: legion-tree-shell, namespace: legion }   # no legion.dev/* labels
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  nodeSelector: { legion.dev/pool: legion }
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  affinity:
    podAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector: { matchLabels: { legion.dev/tree: <KEY> } }
            topologyKey: kubernetes.io/hostname
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000             # the tree pods' own: their files on the volume are group 1000
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: shell
      image: <runtime.kubernetes.image>      # the daemon's worker image, by digest
      command: [sleep, "3600"]
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
      volumeMounts: [{ name: tree, mountPath: /legion }]
  volumes: [{ name: tree, persistentVolumeClaim: { claimName: tree-legion-<project>-<root issue> } }]
```

```sh
kubectl -n legion apply -f legion-tree-shell.yaml
kubectl -n legion wait --for=condition=Ready pod/legion-tree-shell --timeout=300s
kubectl -n legion exec -it legion-tree-shell -- sh
kubectl -n legion delete pod legion-tree-shell
```

The pod carries no credential. A tree agent can plant git and jj configuration in the shared clone
([Trust model](#trust-model-the-provisioning-token)), which this shell's `jj` obeys, and the pod
gives that configuration nothing to take. Run no command in it that holds a token.

### Tree sizing: one tree per node

The pool's floor, not the pod, decides node size. Legion pods carry no
`karpenter.k8s.aws/instance-cpu` selector, and only the requests and limits
`runtime.kubernetes.resources` gives their role. The `legion` NodePool's
`karpenter.k8s.aws/instance-cpu Gt 3` requirement makes Karpenter launch the cheapest 4-vCPU type.

Every tree pod carries two rules:
- a required pod affinity to the pods of its own tree, since the volume attaches to one node;
- a required anti-affinity against the pods of every other tree (`legion.dev/tree Exists` and
  `NotIn [<own tree>]`, at `kubernetes.io/hostname`).

So concurrent trees never share a node. Under required colocation the first pod placed decides the
node, so a request on a later pod that the node cannot fit beside the tree's resident pods strands
it: requests, when set, must fit the tree's node alongside the pods resident at once — an architect
and one phase worker at a time — which is how the example `legion.yaml` sizes them for the 4-vCPU
node (the `resource-limits` capability, LEGION-578's ruling; [The deployment's capability
report](#the-deployments-capability-report)). A pod per issue, with a reservation of its own and the
node spread that allows, is dispatch://LEGION-632's.

**The bound.** The pool's `limits.cpu: 64`, with one tree per 4-vCPU node, caps concurrently running
trees at **16**. The TypeScript production configuration runs `admission_cap: 29`. Stage 7's cutover
raises the `legion` NodePool's `limits.cpu` to at least `4 × admission_cap`; until
then an `admission_cap` above 16 admits trees whose pods cannot schedule.

### Trust model: the provisioning token

The provisioning token, the implement App's installation token, is a credential for the whole
repository, and every agent of a tree can write the tree volume: the shared clone's hooks, its git
and jj configuration (a legacy `.jj/workspace-config.toml`, which jj would migrate into what it
reads, included), its remote URL, its `http.proxy`. git and jj obey all of it — they run hooks, the
git jj is told to run, working-copy filters and `ext::` transports, and send credentials through
the proxy the configuration names —
so no process that can read the token may touch the tree volume. The Go coordinator's pods
(`packages/daemon`) keep to that with two init containers:

- **`workspace-fetch`** mounts the provisioning Secret, an in-memory `TMPDIR` of its own, and the
  pod's `feed` `emptyDir`, and runs `legion workspace-init fetch --repo <owner>/<repo> --feed
  /var/run/legion/feed`: one `git clone --bare` of `https://github.com/<owner>/<repo>` into the feed,
  reading no git configuration but its own (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`,
  `GIT_CONFIG_PARAMETERS` unset), with a one-shot credential git asks for `https://github.com` alone.
  It mounts neither the tree volume nor the config home. Every other provisioning command is bounded
  by `workspace.CommandTimeout` (5 minutes, fixed), but this one clone's duration follows the
  repository's size and the network's speed, not a fixed step in provisioning: it runs under
  `workspace.FetchTimeout` (30 minutes) instead. The daemon's own registration deadline (below,
  "Liveness rules") carries a matching bound under Kubernetes, so this wider bound has room to run
  before the daemon would otherwise retire the pod for an agent that never registered.
- **`workspace-init`** mounts the tree volume, the feed read-only, and the config home — never the
  Secret — and runs `legion workspace-init provision`: the shared clone's clone and fetch reach
  `https://github.com/<owner>/<repo>`, the remote its origin names, at the feed over git's file
  transport, then the workspace add, `update-stale`, and the configuration writes. What a tree agent
  planted can run there, with nothing to take that the agent does not already hold.

`packages/daemon/internal/runtime/sandbox/boundary_test.go` runs both containers exactly as the
manifest states them against nine such plants, with every one of provisioning's git and jj pins made
ineffective.

On the **tmux** runtime there is no such boundary: panes run under the daemon's uid and can read its
0600 credential files, and the daemon's credentialed clone and fetch run in the shared clone itself.
Provisioning's pins there — no git hook (`core.hooksPath=/dev/null`), the git the daemon resolved at
boot as jj's `git.executable-path`, `GIT_ALLOW_PROTOCOL=https`, no working-copy snapshot in the
credentialed fetch, the one-shot credential scoped to `https://github.com` with no askpass, and
`GIT_CONFIG_PARAMETERS` unset — are defence, not a boundary. They hold the settings they name; they
do not stop every program the shared clone's own git or jj configuration can name. One example of
what they leave open: a tree-written `http.proxy` with `http.sslVerify=false` still sees the token
on its way to github.com.

### RBAC the Go daemon needs

The daemon runs as a restricted identity (in production, an IAM role mapped to the `legion-daemon`
group). It needs:
- `sandboxes`: create, get, list, watch, patch, delete; `sandboxes/status`: get;
- `secrets`: create, delete, update, get, but never list;
- `pods`: get, list, watch, which the incarnation fence's pod informer reads; `pods/log`: get;
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
daemon_url: http://<address pods reach the daemon at>:13370   # required under kubernetes
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

- **`runtime.kubernetes.pod`** has four keys. `env` is variables set in the agent's container.
  `volumes` are each one `secret`, `config_map`, or `projected` source; a projected
  `service_account_token` must last at least 600 s, the least the API server issues, and its
  `audience` may not still hold a `${…}` placeholder, which nothing expands. `volume_mounts`
  are read-only unless `read_only: false`, and may use `sub_path`. `service_account` is the pods'
  ServiceAccount; unset, pods run as the namespace's `default` ServiceAccount. A name or path that
  collides with Legion's own is refused at load, naming both: a variable the runtime, the worker
  image's `ENV` or every launch sets, a volume name Legion uses, or a mount at, under or above a path
  Legion mounts, the image owns, or a tool runs from. `legion start --check-config` runs the same
  check. [`deploy/kubernetes/operator-route/`](../deploy/kubernetes/operator-route/README.md) is an
  example of one, the one the Go live harnesses run on: a `models.yml` and a settings overlay from a
  ConfigMap, and a mounted token its key command reads. Its README lists what an operator supplies
  and how `pod` and `provider_keys` compose.
- **The Legion machine-user sign-in.** `legion model-token` is a model `apiKey` command that signs a
  pod in to Cognito with its projected service-account token and prints the access token; the
  [operator route README](../deploy/kubernetes/operator-route/README.md) shows the line and what the
  command does.
- **Each role's model** is the operator's: the `models.yml` and `overlay.yml` they keep in a
  directory of their own (for example `~/.local/state/legion-model-config`), which
  `deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>` writes into the
  ConfigMap `legion-operator-route` in namespace `legion`, its only writer. To change a role's
  model, edit its line under `modelRoles` in that `overlay.yml` and run the same command again.
  Pods started after that use it; a running pod keeps the files it started with until it restarts,
  since both are mounted by `subPath`, which the kubelet never refreshes. The example overlay also
  turns `retry.modelFallback` on (LEGION-578's ruling: a failed model call is retried on another
  model; which model is the repository's `retry.fallbackChains` to name): the probe pod reads the
  effective value under the operator's configuration (`omp config get retry.modelFallback`) onto its
  OK line's `model-fallback` mark, and the daemon reports the `model-fallback` capability open while
  a deployment's copy turns it off — a report, not a refusal; a `capabilities.decided.model-fallback:
  "<reason>"` line in `legion.yaml` records a decision to keep it off ([The deployment's capability
  report](#the-deployments-capability-report)).
- **`runtime.kubernetes.agent_secrets`** enrolls every pod the daemon runs with the secrets broker
  (the broker design's pod-enrollment plan), so an agent in a pod runs
  `agent-secrets <SECRET> -- <command>` and gets only
  that pod generation's grants. `url` is the broker's base URL (https, or http to a loopback
  address). The block names no approver: the daemon's login is the `legion-daemon` service's, which
  anyone signed in to Dispatch approves on the Dispatch credential page, and a file that sets
  `operator` is refused at load naming the key — there is no launcher-token file and no manual CLI
  step. The daemon runs its own login at boot, on a background context, and logs the confirmation
  code exactly once: `agent-secrets machine login: enter code XXXX-XXXX on the Dispatch credential
  page, where anyone signed in may approve it; pod enrollment is held until approved`. The same code and the login's current status
  ("none", "pending", "issued", "denied", "expired", or "failed" when the broker never opened the
  login) are on `GET /legion/v1/state`'s
  `agentSecretsLogin` (daemon API contract 9); pod enrollment fails closed and retries until a
  person signed in to Dispatch approves the code there. When a login ends without a credential
  (the broker refused it or could not be reached, its code expired undecided, or someone denied
  it), or the broker revokes or expires the credential, the next pod enrollment starts a fresh
  login, with no restart. The new code is on `agentSecretsLogin` and in the supervisor's
  `enrollment failed; retrying on the next observation` warning. After a login that ends without a
  credential the daemon waits 30 s before the next, doubling each time to at most 5 minutes, and an
  approved login resets the wait. It never starts a login while one is pending.
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
  tree's close). The daemon's login is the service `legion-daemon`'s, and its pods run as the
  ServiceAccount `runtime.kubernetes.pod.service_account` names (`default` when unset, above). A
  broker configured with that account for `legion-daemon`, such as
  `BROKER_SERVICES=legion-daemon=system:serviceaccount:legion:legion-worker` for pods running as
  `legion-worker`, gives a secret tagged `owner=legion-daemon` to every such pod at once and to no
  other session (the broker's concepts page, "Owner and tier").
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
  key directory is mounted into that container and no other, the broker accepts a renewal on the
  key's proof alone, and `agent-secrets renew`, which the image ships, needs only that URL and the
  key directory. So the enrollment can stay live until the pod is replaced, at the latest when its
  issue closes, plus one lease (`BROKER_LEASE_SECONDS`). To end it at once, anyone signed in to
  Dispatch revokes the daemon's machine login on Dispatch's machine-login page
  (`/credentials/machine`), which ends every session it enrolled
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
daemon_url: http://<devbox VPC IP>:13370
bind: <devbox VPC IP>
worker_stream_port: 13371
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
| `DISPATCH_TOKEN_FILE` | `/var/run/legion/providers/DISPATCH_TOKEN` | `resolveDispatchConfig` (when the deployment sets `dispatch_url`) |
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
`@sjawhar/pi-legion`) in the Legion profile, restart
the daemon, and relaunch every live root, worker, and controller. The boot log is the checklist:
each line naming an older or unrecorded `pi-legion` process identifies one process to relaunch.
### Volume retention

Node loss reattaches the EBS volume to a replacement node and the pod resumes where it left off.
Volume loss — a PVC that lost the shared clone and every claim's retained session, as a fresh EBS
volume after node loss can — is detected through the shared `workspace-init` init container, which
every role's launcher waits behind: it expects the tree's clone (`LEGION_EXPECT_TREE_VOLUME`) once
the launching claim resumes a session or any stored claim of the tree recorded one, and exits 3 only
once both the clone and every retained session are gone. Under `restartPolicy: Always` a failed init
container never turns the pod `Failed`; the kubelet leaves it `Pending` in `Init:Error` or
`Init:CrashLoopBackOff` and keeps retrying it forever on its own. The runtime does not wait for a
phase that will not come: `evaluate` reads the init container's current or last-terminated state as
**Gone**, with `WorkspaceLost` true for workspace-init's exit 3, and `relaunch` replaces the
init-failed pod outright (delete, then recreate) instead of waiting on its launchers. The daemon
stamps every claim of the tree that already existed `workspaceLost`; each one's replacement starts
fresh from the committed issue bookmark until its own fresh session registers, even after the new
pod rebuilt the shared clone; a role first created later uses the ordinary fresh-worker path.
A pre-loss worker is never downgraded to an ordinary missing-session failure. Its prompt begins: `Your workspace was recreated from
`legion/<KEY>` because the tree's volume was lost. Anything you had not committed and pushed is
gone. Re-read .legion and your last handoff, and reconcile before continuing.`

One PVC per tree, `legion-<tree-slug>` (`ReadWriteOnce`, `tree_volume`, `storage_class`), created by the
tree's first spawn — create-if-missing on every spawn, before its pod. When no recorded process names
the volume any more (the tree closed), the daemon's orphan sweep annotates it
`legion.dev/unreferenced-since: <RFC 3339>` the first time it finds it unreferenced, and deletes it once
that mark is 7 days old. A spawn that mounts the volume again — or a sweep that finds it referenced
again — removes the mark, so the clock restarts only when it is unreferenced once more. To see what is
pending deletion:

```sh
kubectl get pvc -l legion.dev/project=<project> \
  -o custom-columns=NAME:.metadata.name,SINCE:.metadata.annotations.legion\.dev/unreferenced-since
```

The same sweep deletes pods labelled with the project that no recorded process names and that are
older than the sweep's grace period, each with its per-pod Secret. A per-pod Secret left behind
*without* its pod — the daemon died between the Secret and the Pod create, or a Pod create failed and
its cleanup delete failed too — is not the sweep's to find (no pod names it, and the daemon never
lists Secrets); the next spawn of that `(issue, role)` reclaims it: once `retirePreviousPods` has
proven no pod of the role exists, the spawn deletes the same-name Secret by name (a 404 is nothing to
reclaim) before creating its own, exactly as it treats a same-name pod.

### Finished siblings' workspaces

The tree volume otherwise only grows: every issue's jj workspace stays on it even once that issue
is done. On every `workspace-init provision`, which runs once for each issue pod a launch creates
(a role started in a running issue pod provisions nothing), the Go daemon computes which of the
tree's other issues are safe to remove and passes that list as JSON in `LEGION_REMOVABLE_WORKSPACES`
on the `provision` init container alone (never `workspace-fetch`, never a role container or the
worker process its launcher starts).
`removableWorkspaces` (`packages/daemon/internal/daemon/removable.go`) states the candidate rule
from the daemon's own claim store; `relaunch` (`internal/runtime/sandbox`) also drops any
candidate that still has a live, non-terminal pod of its own tree, a second guarantee on
different evidence — it cannot tell a claim whose `fail` persisted `StateFailed` despite its own
`suspendProcess` erroring from one truly gone, so that pod, not the daemon's own claim store, is
checked directly for this one question. `workspace-init` is the process that judges and removes
each candidate. No jj configuration a tree agent writes ahead of a command reaches its jj commands:
jj 0.38 and later keep a repository's and a workspace's configuration in the config home (the
pod's own), and the one way a file on the tree volume becomes jj configuration, jj migrating a
legacy `.jj/workspace-config.toml` or `.jj/repo/config.toml` that has no id file beside it, is
closed by removing that file before each jj command provisioning and removal run
(`disarmLegacyConfig`, `internal/workspace/config.go`), the repository's only in the shared
clone's own `.jj/repo`. Every jj command run in a workspace names it with `-R`, so jj never walks
up to an ancestor's `.jj`. A jj command is refused when the `.jj` it would open, or the shared
clone's `.jj` or `.jj/repo`, is a symlink or anything but a real directory; when any directory of
the layout between the state directory and a workspace or the shared clone (`repos/<host>/<owner>/
<name>`, `workspaces/<owner>/<name>/<issue>`) is a symlink, which the refusal names (the state
directory itself may be one); when it runs in a workspace that has no `.jj`; and when the
workspace's `.jj/repo` names any other directory, followed through symlinks and `..` as jj itself
follows it, or is neither a directory nor a regular file. The worker image's jj is 0.45; on the
tmux runtime, which runs the host's jj through the same code, the daemon refuses to start with a
jj older than 0.38 (`resolveTools`, naming `LEGION_JJ_PATH`), since before 0.38
`.jj/repo/config.toml` is the repository's live configuration. It snapshots the candidate's own
working copy with `--config` overrides that hold the snapshot's working-copy filter and signing
programs off even so (`snapshotOverrides`, `internal/workspace/removal.go`), keeps the workspace
whenever that snapshot leaves anything unaccounted for — an untracked path, anything on stderr, or
a nested repository the snapshot cannot see at all — and otherwise removes it only once every
commit it holds is reachable from a remote bookmark or the recorded merged pull-request head,
renaming its directory aside before the slower recursive delete so a kill mid-delete is finished, not
re-judged, on the next pass. A removed workspace's gitignored content is deleted with it: nothing
but a pushed commit protects anything on this volume, and gitignored content is never pushed. The
pass runs inside a 90 s budget, deferring the rest of the list to the tree's next launch once
spent, and rotates the candidate order by the pod's own issue together with the role and
generation of the launch that created it (issue alone never changes across relaunches of the
same issue, and generation alone does not distinguish one issue's roles' first launches, all at
generation 1) so one expensive candidate does not starve the same candidates on every launch. The
daemon's own list is
stamped with the launch time plus the init-wait window (`initWaitSeconds`); `workspace-init`
removes nothing at all once its own `workspace-fetch` started later than that — comparing the
fetch's own start, not wall-clock time at removal, is what keeps this bound independent of how
long the clone itself then takes (`workspace.FetchTimeout`, up to 30 minutes) — so a pod the
Sandbox controller recreates on its own long after the daemon last computed the list (an
eviction, a node drain, a hand deletion) cannot act on one gone stale. A list the pod's own
`legion` cannot read in full (a field it does not know, anything after the JSON object, no
`notAfter`, no candidates) likewise removes nothing, and the pass logs why; the payload is part of
`DaemonAPIVersion`'s contract, so a change to its shape bumps that number and the daemon's image
probe refuses an image whose `legion` would read it the old way.

The worker's own jj working-copy snapshot before `legion push`'s network push can take 63-100 s
on a near-full volume (`removalBudget`'s own doc comment, `cmd/legion/workspace_init.go`, names
the measured range), so that push gets `credential.pushTTL` (5 minutes) in place of the usual 60
seconds, minted whenever a bash call invokes it — alone, as one segment of a compound command, or
a pipeline's last stage (`docs/solutions/legion/worker-pane-shell-gotchas.md` has the mechanics
and the LEGION-17 case this closes).

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
  whatever the pod's age: the pod is provisioning its working copy (`workspace-fetch`'s one clone,
  bounded by its own `workspace.FetchTimeout` rather than `workspace.CommandTimeout`;
  `workspace-init`'s own commands, each up to `workspace.CommandTimeout`; or a wait behind another
  pod's lock on the shared clone), and a live initialiser is a live process — as the tmux runtime's
  own in-process provisioning is. The boot watchdog re-arms on it, bounded by its registration
  deadline (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, default
  360 s, 6 min): under Kubernetes, the deadline carries an added bound of `workspace.FetchTimeout`
  (30 min) plus the lock-wait budget `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` is sized by
  (`sandbox.Runtime.ProvisionBound`; 8 min at the defaults, so 38 min total) until the shim's first
  hello, which can only arrive once both init containers have finished: from there the daemon
  re-arms the base deadline alone, the same one a tmux pane runs under throughout. A pod that
  never says hello is retired at launch plus the base deadline plus the full bound, armed as one
  (44 min at the defaults); one that says hello and never registers is retired at hello plus the
  base deadline alone (6 min from the hello); and one whose agent registers and never says it is
  ready is retired at its registration plus the base deadline alone (6 min from the registration,
  again from a daemon restart that finds it registered), then resumed as the same session one
  generation later and counted as a launch failure. A tmux pane carries no bound to begin with,
  since it starts the agent at once with no init phase.

  The runtime's own wait for a tree's other pods to finish initializing before this one provisions
  (`awaitTreeInitialized`, bounded by `treeWaitBound`) is the sibling's own full pre-hello deadline
  — base plus `ProvisionBound`, the same sum the registration deadline above arms while a claim is
  still launching — plus one more boot interval of headroom (46 min at the defaults): the same
  relationship `ceil(boot) × (intervals + 1)` holds against `boot × intervals` alone for the lock
  wait. A launch waiting on a sibling therefore never gives up before the daemon's own deadline for
  that sibling would, up to the sibling's own hello: a sibling this wait still counts as
  initializing has not reached its hello yet, so its own deadline has not re-armed past hello
  either. Two mechanisms together keep two pods from actually provisioning the shared clone at
  once: `lockTree` holds the tree's launch turn only until the new pod is in the store, well before
  that pod's own init finishes, so by itself it would let a third pod start initializing while a
  second one still is; `awaitTreeInitialized` is what closes that gap, since no new pod is ever
  created while an existing tree pod is still initializing. Because of those two mechanisms, the
  lock wait itself (`LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS`, the `flock --timeout`
  `workspace-init` passes when contending for another pod's hold on the shared clone) almost never
  actually contends, so it is sized as a safety net for whatever can still race around them — the
  `ceil(boot) × (intervals + 1)` lock-wait budget alone (`sandbox.Runtime`'s own `initWaitSeconds`),
  with no added `FetchTimeout` — rather than as a budget matched against another pod's own
  remaining registration deadline (a manual `legion workspace-init` without the variable waits
  900 s);
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
`POST /legion/v1/controller/secret` answers 409 (`this daemon launches the project's controller
itself (controller: daemon), so legion controller start has none to start`): one controller runs per
project.

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
the pod baseline (`--pod-safety`: the turn-scoping overlay first in `PI_CONFIG_FILES` and the two
session-placing variables, [Settings order](#operator-configuration)), on the daemon's command, as
an issue pod's role containers do theirs, so a relaunch in a healthy pod is a new generation of that
child rather than a new pod. Its role Secret, `legion-<project>-controller-controller-boot`, holds
that launcher's token alone, bound to the pod's uid, and is the only Secret its launch writes: there
is no provisioning `-boot` Secret.
Its Sandbox owns a volume of its own (`tree-legion-<project>-controller`, of
`runtime.kubernetes.tree_volume` and `storage_class`), mounted at `/legion` with its `sessions`
directory at Oh My Pi's sessions directory, so a relaunch resumes the session. It provisions no
workspace and holds no repository credential: no `workspace-fetch`, no provisioning token, no gh
shim. Its one init container, `legion workspace-init controller --root /legion`, makes the
sessions directory; in a pod created to resume a session whose file is gone it exits 3, which the
daemon reads as a lost volume and answers with a fresh controller. The pod carries
`legion.dev/project` and `legion.dev/role=controller` and no tree or issue label, so no tree pod's
anti-affinity counts it as another tree's, and the boot census holds it to exactly the one launcher
those labels name. It is never enrolled with the secrets broker: it has no agent-secrets volume,
shim flag or `AGENT_SECRETS_URL`. It has no affinity of its own, so it lands on any Legion node,
and like every Legion pod it is annotated `karpenter.sh/do-not-disrupt: "true"`: Karpenter never
consolidates or replaces for drift the node it runs on while it runs, which is as long as the
daemon keeps it, though tree pods can still be scheduled onto that node. Its agent is told
`LEGION_CONTROLLER=1`, `LEGION_ROLE=controller`, `LEGION_PROJECT`, `LEGION_DAEMON_URL`, the Envoy,
NATS and Dispatch settings and the launch secrets' `<NAME>_FILE` pointers, and nothing of a tree,
an issue or a checkout. Its system prompt is the controller's role prompt, a part saying it runs
headless in a pod, the daemon's `Design gate policy:` line and the deployment instructions. Both
its containers, the init container and the launcher, carry `runtime.kubernetes.resources.controller`,
the key a workflow role's pod is sized by; with none set the pod is BestEffort, the class the
kubelet evicts first under node memory pressure, and each eviction is a relaunch whose ready costs
the controller a start-procedure turn, so size it.

The Sandbox is the claim's alone, where an issue's is its tree's. A release of the claim (a switch
back, or `legion claims stop`) deletes it, and its role Secret and volume with it. The orphan sweep
keeps it while the daemon knows the claim, running or suspended, so a suspended controller's
session survives, and deletes it once the claim is retired. Either delete runs under the pod's
launch turn and only at the version of the Sandbox it was decided on: a controller relaunched into
the Sandbox since has written it, so the delete is refused, the daemon logs `sandbox runtime: kept a
sandbox written since its delete was decided; the orphan sweep decides it again` naming it, and the
next sweep decides again on the claims known then. The controller Sandbox a daemon made before the
controller ran in a launcher pod (`role=controller`, one `worker` container) is refused at boot like
every Sandbox of the layout before issue pods, naming it: remove it before enabling issue pods.

**Reaching it.** Nobody types into the pod. A person reaches the controller through Dispatch (a
message to its session on the Agents page, a reply to its ask, a mention) or Envoy, and reads its
session with `kubectl -n <namespace> logs legion-<project>-controller -c controller` (its launcher's
log, which carries the shim's and Oh My Pi's) or its transcript on the volume. Wakes reach
it as they reach the operator's controller: it subscribes to
`notifications.legion.<project>.controller` once it holds the role.

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
2. probes that Oh My Pi as the controller will run it, with `omp models`, which starts no session, and
   refuses a pi-legion it does not load, or one speaking another daemon API contract, loaded without
   pi-envoy or with a pi-envoy at another plugin interface version, or loaded beside the pre-split package;
3. asks `POST /legion/v1/controller/secret` with the operator token as `Authorization: Bearer` and
   the contract step 2 held the plugin to (`{"pluginContract": <N>}`). The daemon compares the token
   in constant time, then refuses a contract that is not its own with 409, naming both, before it
   mints anything, so a `legion` and a daemon from different releases never cut the running
   controller off. Otherwise it mints a fresh controller capability, which replaces the previous
   one and its registration and ends every controller grant: the last start wins. The answer also
   carries the daemon's `gates.design`, and an answer without `root-issues` or `off` is refused
   with a request to upgrade the daemon;
4. writes the secret 0600 under the local state directory (`state_dir`, by default
   `$XDG_STATE_HOME/legion/<project>-controller`), beside the `gh` shim, the `legion` launcher and the
   deployment instructions;
5. runs Oh My Pi interactive in the foreground (`omp_launch_prefix` and `omp_invocation`, one joined
   `--append-system-prompt` holding the controller prompt, the daemon's `Design gate policy:` line
   and the deployment instructions, no `--resume`, no `--mode rpc`) with the controller's
   environment (`LEGION_CONTROLLER=1`, `LEGION_ROLE=controller`, `LEGION_DAEMON_URL`,
   `LEGION_PROJECT`, `LEGION_STATE_DIR`, `LEGION_CONTROLLER_START_MESSAGE` (the pi-legion
   extension sends it as the session's first turn right after its role claim succeeds and before
   it opens the live wake subscription, so the controller's first turn runs its start procedure
   deterministically, with nothing typed, rather than racing a wake for the session's one
   first-turn slot — LEGION-392), its grant and secret files, the Envoy and Dispatch
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
