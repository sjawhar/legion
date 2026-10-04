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
- `@sjawhar/pi-legion-envoy` packed from that commit's `packages/pi-envoy` (the exact `bun pm pack` steps
  `release.yaml`'s `pi_envoy` job runs) and linked into the isolated OMP profile `legion`
  (`OMP_PROFILE=legion`; plugins resolve to `/home/legion/.omp/profiles/legion/plugins/node_modules`);
- `@bopstack/pi-codegraph` (from npm, pinned) linked into the same OMP profile, backed by the CodeGraph
  CLI (`@colbymchenry/codegraph`, pinned) at `/opt/codegraph/bin` (`PATH`) — the `codegraph` tool a tester
  queries for `affected` tests and a reviewer for `impact`/`callers` blast radius (`packages/daemon/internal/prompts/roles/core/tester.md`, `core/reviewer.md`);
- OMP's native modules, pre-downloaded into `/home/legion/.omp/natives/<version>/` so a pod never fetches them;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs) and `gh` at `/usr/local/bin`, and
  `git` at `/usr/bin/git` from the `debian:trixie-slim` base — jj's git backend requires git >= 2.42
  (bookworm's 2.39.5 made every `jj git clone` in the init container fail), so the build also proves the
  image's jj accepts its git with a network-free `jj git clone` of a scratch bare repository before the
  probes run, and its last step refuses a git anywhere but `/usr/bin/git`;
- a generic toolchain for the repositories the workers work, specific to none of them: `uv` and `uvx`;
  `node`, `npm`, `npx` and `corepack` from Node 24 LTS, with `pnpm`, `pnpx`, `yarn` and `yarnpkg` linked
  to corepack's shims (the links `corepack enable` would write, which uid 1000 cannot); and the AWS CLI
  v2's `aws` — all at `/usr/local/bin`, which is on the image's `PATH` and every pod's. Each is a pinned
  release whose linux/amd64 archive the build checks against a pinned SHA-256 before unpacking it (the
  `ARG`s at the top of `worker.Dockerfile`). A corepack shim runs the version a project's
  `packageManager` names, or else the default that corepack ships (the image sets
  `COREPACK_DEFAULT_TO_LATEST=0`, so never npm's newest release), fetched on first use into the user's
  corepack cache.
  The image bakes no Python: `uv` installs each project's own, from its `.python-version` or
  `requires-python`, the first time the project runs (`uv sync`, `uv run`). In a Sandbox pod that is on
  the tree volume ([Anatomy of a Sandbox pod](#anatomy-of-a-sandbox-pod)); elsewhere it is uv's default,
  `~/.local/share/uv/python`. Node and the AWS CLI keep their trees at `/opt/node` and `/opt/aws-cli`,
  outside `HOME`, so no volume a pod mounts under `HOME` shadows any of it.

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
(`packages/daemon/internal/daemon/bootgate.go`). The image build's final step runs `legion version`,
requiring the commit the workflow built, then `legion probe-image`: the same two probes, run by the
daemon's own code, plus a third only the image runs — the session-storage probe, which prints
`session-storage=probed` on the OK line ([The image guard](#the-image-guard)) — with the plugin held to
the daemon API contract (`legion.daemonApiVersion`) and every task agent and skill Legion's prompts
name (`task(agent="…")`, `skill://…`) resolved by name through the same launch (the plugin ships
`oracle`, `deep-worker`, `thermonuclear-deep-review` and `thermonuclear-code-quality`, and the
planner's `plan-gap-analyst` and `plan-reviewer`, in `agents/`, and the pair's rubrics and
`ce-simplify-code` with Legion's other skills in `dist/skills`), so a build whose OMP or plugin is
broken fails instead of publishing. The build has none of the operator's model configuration, so it
leaves those agents' models unresolved (`--skip-agent-models`), printing
`probe-image: OK (/opt/omp/bin/omp) session-storage=probed agent-models=skipped daemon-api-version=<N>`. The daemon's Agent Sandbox runtime runs the same command in a probe
Sandbox, `legion-probe-<project>-<digest12>`, with its own contract, under the operator's pod, at every
boot, and requires `agent-models=resolved`: each agent's model resolves, with a working key, as the task
tool resolves a subagent's (`packages/daemon/internal/runtime/sandbox/probe.go`). To run it yourself:
`docker run --rm ghcr.io/sjawhar/legion-worker@sha256:… probe-image --plugin-root /opt/legion/pi-legion-envoy --skip-agent-models`
(`--plugin-root` is required: the plugin root a Sandbox pod loads the plugin from, so the probe loads it the same way; without
`--skip-agent-models` it also resolves each agent's model, which needs the operator's model roles).
A step of its own, before that final one, runs every toolchain command listed above as `legion`, from
the image `PATH`, the corepack shims fetching their shipped default pnpm and yarn into a scratch
directory the step removes. It reruns only when the toolchain or an earlier layer changes, so a commit
that only rebuilds `legion` needs no package registry.
To check a published image's toolchain end to end, Python install included:
`docker run --rm --entrypoint sh ghcr.io/sjawhar/legion-worker@sha256:… -c 'uv --version && node --version && npm --version && aws --version && uv python install 3.13 && uv run --python 3.13 python -c "print(1)"'`.

### Pin by digest, never by tag

`legion.yaml` `runtime.kubernetes.image` accepts only `ghcr.io/sjawhar/legion-worker@sha256:…`. A tag is
mutable; the daemon must know exactly what it probed, so a tag reference is refused at startup with
`runtime.kubernetes.image must be pinned by digest (@sha256:…)` (`packages/daemon/internal/config/kubernetes.go`).

Where the digest is published:

- the job summary of every `Worker Image` run (Actions → Worker Image → the run → Summary);
- the body of the `legion-v<version>` GitHub release, under "Worker image", when `release.yaml` released
  `legion` in the same run (`gh release view legion-v<version> --json body -q .body`);
- `docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:<tag>` for any published tag.

Tags: `sha-<12 hex of the built commit>` on every run (on a pull request that is the PR head, never the
ephemeral merge commit); `<legion version>` only on `main` when the `legion` job released that version in the same run.
Runs from any other ref publish the `sha-` tag only and never touch a release.

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
whole — the Dockerfile (`packages/daemon/docker/**`), the plugin and what it bundles (`packages/pi-envoy/**`,
`packages/envoy-client/**`, `packages/contracts/**`) and the skills (`skills/**`); the OMP pin
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
repository takes no fork PRs, whose token would be read-only).

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
specific to one repository, so a Python or Node repository runs on the published image as it is. A
deployment whose repositories need more than that (a system library, another language, a different Node
line) builds its own image in **its** repo:

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
`docker run --rm <image> probe-image --plugin-root /opt/legion/pi-legion-envoy --skip-agent-models`
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

### The extension under SQL storage

`@sjawhar/pi-legion-envoy` recognises a `task` subagent inside a Legion worker without looking for the
parent's transcript on disk: the extension records which session it bootstrapped in the process, and a later
session start in the same process with a different transcript path is a subagent (`packages/pi-envoy/AGENTS.md`).
Nothing in the extension reads the two variables, and it needs no other change for SQL storage.

### Selecting the store

Two keys in `legion.yaml`, both inside the `runtime.kubernetes` mapping:

```yaml
runtime:
  kubernetes:
    session_store: postgres        # pvc (the default) keeps sessions on the tree's disk volume
    session_dsn_secret: SESSION_DSN  # the providers-Secret key that holds the connection URL
```

`pvc` is today's behaviour: each conversation is a file in the `sessions` directory of the tree's disk
volume. `postgres` moves it into a database. The daemon never holds the connection string: you put it
in the providers Secret (`legion-<project>-providers`, the same Secret that carries the provider API
keys and `DISPATCH_TOKEN`) under the key `session_dsn_secret` names, as one `postgres://…` URL. Every pod
already mounts that Secret read-only at `/var/run/legion/providers/<key>`, so no new volume is needed.

With `postgres` on, the daemon adds exactly two variables to every pod it opens for a tree — the root
architect, a sub-architect, each phase worker — in the one place a pod's environment is shaped
(`podEnvironment`, `runtime-kubernetes.ts`): `OMP_SESSION_STORAGE=sql` and
`OMP_SESSION_SQL_DSN_FILE=/var/run/legion/providers/<key>`. Nothing else about the pod changes: same
image, same volume and mounts, same `--resume` argument on a replacement. The init container never runs
Oh My Pi and does not see the Secret; under `postgres` it also omits the recorded-session check it runs
under `pvc` (`LEGION_RESUME_SESSION_FILE`), because the transcript is a database row it cannot look for.
`HOME` and `OMP_PROFILE` come only from the image's own environment and the daemon never overrides them,
which is what lets a replacement pod open the same row (its key embeds the home-relative sessions root).

When that row is missing — the database lost it, or the connection string now points at another
database — Oh My Pi does not refuse: it starts a fresh session with a **new** session id at that path.
The daemon refuses it instead. Every relaunch that passes `--resume` — a phase worker's or sub-architect's
respawn, a root's resurrection — mints its boot token with the session id the previous generation
registered, and the registration route (`/process/started` for a root, `/worker/started` for the rest)
answers a different id with `409 Worker respawn must resume the same agent session`; the extension exits
the process on that answer, the pod ends, the daemon counts a launch failure, and — because the resumed
path stays on the tree's or claim's locator — the next relaunch resumes the same path and expects the
same session, until the role ends in `worker-died` (a root: `launch-failed`) at the bound — never a fresh
agent under the old role or tree. A first launch, and a root re-admitted after `launch-failed`, resume
nothing, record no expectation, and are accepted as before.

Name the key so that nothing reads it — `SESSION_DSN` is a good choice. The pod's worker shim exports
every key of the providers Secret into Oh My Pi's process environment under the key's own name, as it
does for every provider key, so the connection string is also visible there as `<key>=postgres://…`
(the same exposure class as the provider keys: same user, same pod). A key named `OMP_SESSION_STORAGE`
or `OMP_SESSION_SQL_DSN_FILE` would shadow the daemon's own value through that export, so the daemon
refuses those two names at startup; `postgres` without a key, an empty key, a key with a `/` or other
character Kubernetes does not allow in a Secret data key, and a key given under `pvc` are refused the
same way, each naming the field.

When the key is missing from the Secret, the pod still starts (the Secret is mounted whole, so a missing
key is a missing file): Oh My Pi refuses with `OMP_SESSION_SQL_DSN_FILE names
/var/run/legion/providers/<key>, which could not be read: ENOENT …`, exits 1, the pod goes `Failed`,
the daemon quotes its log tail and counts a launch failure exactly as for any other boot failure — and
never falls back to file storage. A blank file, a value the driver cannot parse, or an unreachable
database ends the same way ([Refusals](#refusals) above).

### Why pods stay node-affine

`postgres` moves only the conversation. The issue's working copy — the jj workspace every phase edits —
is still on the tree's disk volume, which is `ReadWriteOnce`: one node at a time. So every pod of a tree
is still required to schedule on the node that runs the tree's other pods (the affinity term in
[Anatomy of a pod](#anatomy-of-a-pod)), under both stores. Affinity goes away only when the workspace
moves off the volume, which is separate work.

### The image guard

An Oh My Pi built before the `session.storage` setting ignores the two variables and keeps sessions on
files without a word — the one silent fallback this setting must never allow. The check lives inside the
image: `legion probe-image`, which the image build runs before it publishes, starts the image's own Oh My
Pi with a nonsense `OMP_SESSION_STORAGE` value and passes only if it refuses, then prints
`session-storage=probed` on its OK line. Under `session_store: postgres` the daemon's worker-image probe
requires that token in the probe pod's log: an image whose OK line
lacks it is refused before the daemon serves — `pod <name> Succeeded without printing
session-storage=probed, which session_store: postgres requires (its Oh My Pi or legion CLI predates the
session-storage setting)` — exactly as one lacking `daemon-api-version=<N>` is. Under `pvc` the token
is not required. The daemon never probes a host Oh My Pi for this: pods run the image's build, not the
host's.

The probe cache records whether the token was seen (`sessionStorageProbed: true`). A pass cached under
`pvc` on an image that printed the token is reused under `postgres`; one cached without the field —
written by an older daemon, or for an image that never printed it — is ignored under `postgres`, logged
`it records no session-storage probe, and this daemon runs session_store: postgres`, and the probe pod
runs again. Because the image builds the `legion` CLI and the `@sjawhar/pi-legion-envoy` plugin from one
checkout, an image that prints the token also carries the plugin's storage-independent subagent guard
([The extension under SQL storage](#the-extension-under-sql-storage)).

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

Releasing a role ends only its process. The issue owns its Sandbox, its role Secrets and, for a
root, the tree PVC, recorded in the daemon's store (`issue_resources`) before any role of it starts.
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
Re-admission during linger reuses those resources and resumes the recorded sessions; the issue's
launch lock orders a concurrent resume after any suspension already in flight.

Linger expiry reserves its tree only while the root still lingers at the close's generation, so
a re-admission that committed first fences it; an operator close reserves after it authenticated
the root close. Either reservation comes before the census, which then reads every stored claim of
the tree, including one that persisted before the reservation but has not admitted resources yet,
and deletes nothing until all of them retired. A reservation that is not confirmed resumes on the
next attempt, even after a re-admission, and stays the new start's wait until API confirmation:
every close row of the tree, however stale its generation or the linger it closed, still retires
its claim and drives that cleanup before the checks that finish a stale close apply. A launch whose
resource recheck meets a reservation committed after its own check is refused at once, with the
same uncharged wait, rather than held toward the boot deadline. Every lifecycle step takes the
global serializer before any lifecycle or resource row.

A child issue's Sandbox is deleted and its absence confirmed through the API. A root is cleaned
last: its cleanup begins only when every child record is confirmed, then refuses new children and
requires the API to list no child Sandbox of the tree. The root Sandbox delete carries its UID and
resourceVersion plus `foreground` propagation. Agent Sandbox v1.0.3 creates the root tree PVC with
that Sandbox as its controller owner and `blockOwnerDeletion: true`; foreground deletion keeps the
owner visible until Kubernetes garbage collection deletes that blocking dependent. The restricted
daemon has no PVC API verb, so it confirms the root Sandbox is NotFound before confirming the
durable cleanup; it does not read or delete a PVC. The live runtime proof must observe the actual
PVC owner reference and its absence after foreground deletion, rather than infer that result from
labels or a generic garbage-collection rule.

An operator-created tree has no workflow record: its stored operator authority, not a zero or
sentinel generation, selects the operator cleanup entry point, which then applies the same
child-first/root-last cleanup without manufacturing a workflow issue. A tree closed before its
first resource record confirms its reservation without deleting anything.

Before the daemon opens its store, so before any schema write, image probe or reconcile, it checks
that Agent Sandbox is installed and refuses a namespace that still holds a per-claim Sandbox of the
layout before issue pods, naming it; once the store opens and before it migrates, it refuses a
claim that still records such a Sandbox. After it migrates and before it installs the issue-pod
layout marker, it refuses an outbox tree close of its project that could never run, naming each
row: one the outbox's strict decode refuses, or whose linger (the root generation the close
expires) is beyond the store's largest generation. No daemon writes one, and such a row would fail
on every attempt. Migrate or remove those first. The daemon runs on a host
its pods can reach and serves the worker stream they dial. The controller is
`legion controller start` on the operator's machine
([Operator-launched controller](#operator-launched-controller)).

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
    resources:                  # optional; a role absent here gets no requests or limits
      tester: { limits: { memory: 8Gi } }
    pod:                        # the operator's: env, volumes, mounts, ServiceAccount (below)
      service_account: legion-worker
      env: { PI_CONFIG_FILES: /etc/legion-operator/overlay.yml }
      volumes: [...]
      volume_mounts: [...]
bind: <the daemon host's own address>   # pods dial tcp://<bind>:<worker_stream_port>
worker_stream_port: 13371
daemon_url: http://<the daemon host's own address>:13370
```

`packages/daemon/internal/config/kubernetes.go` reads the block and refuses, naming the key:
- anything it does not model: `role_profiles`, since each role's requests and limits go under
  `resources`;
- `gateway`, removed with LEGION-270: a pod's model route is the operator's `pod`;
- an image that is not pinned by digest;
- `session_store: postgres` until Stage 6, since a pod's session lives on the tree volume, and a
  `session_dsn_secret` under `pvc`.

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
`omp_invocation` and `omp_launch_prefix`: every pod runs the worker image's Oh My Pi. Every address a pod is handed must be one a pod can reach, so
`bind`, `daemon_url`, `envoy_url`, `dispatch_url` and each `nats_urls` entry may be neither loopback
nor the unspecified address. `legion start --check-config` runs all of it without starting the
daemon, writing a file or running a key command, and then every refusal boot makes from the files
and the environment before its first write, in boot's words: the operator, Envoy and Dispatch
bearers' files, the NATS nkey seed, the instructions file, and the runtime's own
reads (the kubeconfig and every value's translation; under tmux, the OMP invocation, through `mise
where` when it names a `mise` tool, and the host's `gh`, `git` and `jj`). What it does not do is
what boot writes or runs: the state directory, secretsd's provider keys, the plugin gate and the
image probe.

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
with its `server`, and a terminal close (a fatal server `-ERR`, or reconnects run out) at error,
once, as `NATS connection closed` with its `error`; the workflow's `workflow intake stopped` error
then names the same cause as the connection's last error.

Rollout order for the server's `legion-daemon` user: the server admits
`legion-daemon` (its public key applied) with the daemon's grants first; then its seed is stored,
every daemon gets it and restarts, and each boot line must name the daemon's own user: the
daemon's `legion daemon connects to NATS` line reads `paneUser=false` (#1494). Only then is the
`legion-pane` seed written. A clean boot line proves the user, not every grant: the check before
the pane seed is written also has each daemon consume a Dispatch and a GitHub event with no error
line, and searches each daemon's log for `NATS refused the daemon`, since a missing grant on the
exceptions lane (`notifications.envoy.exceptions.notifications.role.>`) still boots healthy and
consumes both events, and that error line is its only sign. `legion-pane` is never granted the
daemon's subjects above. Reversed, a daemon holding only the `legion-pane` seed connects as
`legion-pane`, and each refused subject logs the error line above (a refused consumer or
subscription never delivers).

### Anatomy of a Sandbox pod

Each Sandbox carries `legion.dev/project`, `legion.dev/tree` and `legion.dev/issue` (`names.go`):
one Sandbox per issue, shared by every role that works it. The pod template (`manifest.go`) has
two init containers and one container per role (`claim.Roles`: architect, planner, implementer,
tester, reviewer, merger), named for its role:

1. `workspace-fetch` clones the repository into the pod's feed. It is the only process that holds
   the provisioning token ([Trust model](#trust-model-the-provisioning-token)).
2. `workspace-init` provisions the tree volume's shared clone and the issue's jj workspace from the
   read-only feed.
3. Each role container runs `legion launcher --connect tcp://<bind>:<worker_stream_port>
   --token-file … --sandbox <name> --role <role> --private-dir …`: PID 1 of that role, starting and
   stopping the role's `legion worker-shim` (Oh My Pi) child on the daemon's command, never a worker
   process of its own.

The tree volume is the root Sandbox's `volumeClaimTemplates` entry, and each issue's Sandbox
references that claim by name. Every role container mounts it at `/legion`, and again at Oh My Pi's
sessions directory through a `subPath`, so a session survives its pod. Each role's Secret is
projected twice: its boot half into that role's own container, read-only, and the provisioning half
into `workspace-fetch` alone. The operator's volumes and mounts join every role container's, and the
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
workspace of a tree. `uv cache clean` and `uv cache prune` remove cache entries under a lock that also
stops at the pod, so neither may run while another pod of the tree is using uv.

Every pod runs:
- with `runtimeClassName: gvisor`;
- with `serviceAccountName` set to the operator's `pod.service_account` (the namespace's default
  when it names none) and `automountServiceAccountToken: false`;
- under Pod Security "restricted": non-root user 1000 on the pod, and on each container no
  privilege escalation, ALL capabilities dropped and the RuntimeDefault seccomp profile;
- on the Legion pool, with its node selector and toleration;
- annotated `karpenter.sh/do-not-disrupt: "true"`.

The image probe runs as a Sandbox of its own, `legion-probe-<project>-<digest12>`, with
`shutdownPolicy: Delete` ([The image is probed before it publishes](#the-image-is-probed-before-it-publishes)).

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

**A pod of the tree is running.** Its `worker` container mounts the volume at `/legion`:

```sh
kubectl -n legion get pods -l legion.dev/tree=<KEY> --field-selector=status.phase=Running
kubectl -n legion exec -it <pod> -c worker -- sh
jj bookmark list --all-remotes legion/<KEY> -R /legion/repos/github.com/<owner>/<repo>
```

The shell runs as the tree's agents do, user 1000 under gVisor, with nothing they lack.

**No pod of the tree is running**, as when the only pod is the one whose `workspace-init`
refuses: mount the tree's claim in a pod of your own, `tree-<root Sandbox>`, e.g.
`tree-legion-<project>-<root issue>-architect`
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
  volumes: [{ name: tree, persistentVolumeClaim: { claimName: tree-legion-<project>-<root issue>-architect } }]
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

The pool's floor, not the pod, decides node size. Legion pods carry no instance-size selector and
no resource requests. The `legion` NodePool's `karpenter.k8s.aws/instance-cpu Gt 3` and
`karpenter.k8s.aws/instance-memory Gt 65535` requirements make Karpenter launch the cheapest type
with at least 4 vCPU and 64 GiB.

Every tree pod carries two rules:
- a required pod affinity to the pods of its own tree, since the volume attaches to one node;
- a required anti-affinity against the pods of every other tree (`legion.dev/tree Exists` and
  `NotIn [<own tree>]`, at `kubernetes.io/hostname`).

So concurrent trees never share a node. Requests stay unset because under required colocation the
first pod placed decides the node, and a request on a later pod would strand it.

**The bound.** A tree pod's anti-affinity names no project, since a second tree of any project
would overrun a node sized for one. It has no `namespaceSelector` either, so it applies only within
the pod's own namespace. One tree per node therefore holds across every project whose daemon shares
the `legion` pool only while all of them run their trees in the same namespace (`legion` today);
a daemon in another namespace could place a tree on a node another namespace's tree holds. Under
that condition, the pool's limits divided by the node size its floor sets cap the trees running at
once across all projects together. With the floor at `instance-memory Gt 65535` (64 GiB) and
`limits.memory: 256Gi`, that is four. An `admission_cap` (summed over the daemons sharing the pool)
above that admits trees whose pods stay Pending until a node frees.

### Trust model: the provisioning token

The provisioning token, the implement App's installation token, is a credential for the whole
repository, and every agent of a tree can write the tree volume: the shared clone's hooks, its git
and jj configuration (a legacy `.jj/workspace-config.toml` included), its remote URL, its
`http.proxy`. git and jj obey all of it — they run hooks, the git jj is told to run, working-copy
filters and `ext::` transports, and send credentials through the proxy the configuration names —
so no process that can read the token may touch the tree volume. The Go coordinator's pods
(`packages/daemon`) keep to that with two init containers:

- **`workspace-fetch`** mounts the provisioning Secret, an in-memory `TMPDIR` of its own, and the
  pod's `feed` `emptyDir`, and runs `legion workspace-init fetch --repo <owner>/<repo> --feed
  /var/run/legion/feed`: one `git clone --bare` of `https://github.com/<owner>/<repo>` into the feed,
  reading no git configuration but its own (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`,
  `GIT_CONFIG_PARAMETERS` unset), with a one-shot credential git asks for `https://github.com` alone.
  It mounts neither the tree volume nor the config home.
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
operator's `pod` below); `bind` must be an address pods reach, never `0.0.0.0` or loopback, since every pod
dials the worker stream at `tcp://<bind>:<worker_stream_port>`; and no Legion URL a pod is handed
(`daemon_url`, `envoy_url`, `dispatch_url`, each `nats_urls` entry) may name a loopback or
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
  with: it still resolves OMP and the `pi-legion-envoy` plugin through mise for its two boot probes, and
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
  since both are mounted by `subPath`, which the kubelet never refreshes.
- **`runtime.kubernetes.agent_secrets`** enrolls every pod the daemon runs with the secrets broker
  (the broker design's pod-enrollment plan), so an agent in a pod runs
  `agent-secrets <SECRET> -- <command>` and gets only
  that pod generation's grants. `url` is the broker's base URL (https, or http to a loopback
  address); `operator` is the email of the person who approves this daemon's own machine logins on
  the Dispatch credential page — there is no launcher-token file and no manual CLI step. The daemon
  runs its own login at boot, on a background context, and logs the confirmation code exactly once:
  `agent-secrets machine login: enter code XXXX-XXXX on the Dispatch credential page (approver:
  <operator>); pod enrollment is held until approved`. The same code and the login's current status
  ("none", "pending", "issued", "denied", or "expired") are on `GET /legion/v1/state`'s
  `agentSecretsLogin` (daemon API contract 9); pod enrollment fails closed and retries until a human
  approves the code there. On expiry or revocation the daemon starts a fresh login and logs a new
  code. `provider_keys` may not name an `AGENT_SECRETS_*` variable; `audience` (default
  `agent-secrets`) and `token_expiry_seconds` (default 3600, at most 3600, the cluster's admission cap)
  shape the one projected token every pod carries for the broker, alone in its volume beside the
  operator's middleman token. With the block, the worker container mounts that token read-only at
  `/var/run/legion/agent-secrets-token/token`, a memory-backed key directory at
  `/var/run/legion/agent-secrets`, and is told `AGENT_SECRETS_URL` and `AGENT_SECRETS_KEY_DIR`; the
  shim generates the pod's key there before it dials, its `hello2` carries the key's thumbprint and
  the token, and the daemon enrolls the pod under its own won credential once the agent has
  registered — naming the pod UID it recorded at spawn (the enrollment carries no issue or
  approver at all; the broker's rules pick the approver for the pod's later credential requests at
  request time) — hands the enrollment id back to the shim, and revokes it wherever it lets the pod
  go (a death, the registration deadline, a suspension, a stop, the tree's close). Without the
  block, pods carry none of this. An older worker image is refused at the image probe: the block's
  pod variables are daemon API contract 8.
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
  before it and all of them outranking a repository's `.omp/config.yml`. Legion writes the pod
  baseline's overlay (remote compaction, memory backends, image URLs and dev auto-QA off; Python
  eval off) and names it first, ahead of the operator's, so the operator's overlay outranks it.
  The worker image supplies no Oh My Pi Python kernel: JavaScript eval stays enabled by OMP's
  default, while an operator whose image supplies a compatible kernel may set `eval.py: true`.
  The baseline also sets `OTEL_SDK_DISABLED=true` and `PI_AUTO_QA=0` unless the pod sets them, and
  keeps the operator's value when it does. It sets `PI_CONFIG_DIR=.omp` and
  `OMP_SESSION_STORAGE=file`, which an operator's pod may not set, since they decide where Oh My
  Pi keeps the session a resume reads.
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
| `LEGION_GRANT_FILE` | `/var/run/legion/grant/<role token>-grant` | the pi-envoy extension (writes), `legion credential`/`gh`/`handoff complete` (read) |
| `LEGION_STATE_DIR` | `/legion` | the extension's jj attribution overlay |
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

For a daemon contract change, first merge the worker image and plugin release, then set
`runtime.kubernetes.image` to its digest, install the plugin release in the Legion profile, restart
the daemon, and relaunch every live root, worker, and controller. The boot log is the checklist:
each line naming an older or unrecorded `pi-legion-envoy` process identifies one process to relaunch.
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
- `Pending` with the `workspace-init` init container **running** → **alive**, whatever the pod's age: the
  pod is provisioning its working copy (a clone or fetch of up to `slow_command_timeout_seconds` each, or
  a wait behind another pod's lock on the shared clone), and a live initialiser is a live process — as
  the tmux runtime's own in-process provisioning is. The boot watchdog re-arms on it, bounded by its
  registration deadline (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`,
  default 360 s), after which it retires the pod and spawns the next generation. The pod's own lock wait
  is sized from that same deadline: the runtime sets `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` on the init
  container to the deadline plus one more interval (default 480 s), and `workspace-init` passes it to
  `flock --timeout`, so the init container never gives up on a wait the daemon would still tolerate,
  whatever the deployment configures (a manual `legion workspace-init` without the variable waits 900 s);
- the init container **terminated non-zero** (its current state, or `LastTerminationState` once the
  kubelet has already restarted it) → **dead (gone)**, its log tail quoted, `WorkspaceLost` set when
  it is `workspace-init` exiting 3; under `restartPolicy: Always` the pod never turns `Failed` for
  this — the kubelet leaves it `Pending` in `Init:Error`/`Init:CrashLoopBackOff` and keeps retrying
  the container itself — so the daemon reads the failed attempt directly instead of waiting for a
  phase that will not come, and `relaunch` replaces the pod outright rather than waiting on its
  launchers;
- otherwise `Pending` for longer than `worker_boot_timeout_seconds` (unscheduled, image pull, volume
  mount) → **dead (gone)**, with the pod's events quoted; the boot watchdog's existing path retires it
  and its stop deletes the pod;
- the Sandbox's `Ready` condition reports `MultiplePods` or `ReconcilerError` → **unknown**: the
  Sandbox controller itself cannot resolve the pod it owns, so nothing here can either;
- `Pending` otherwise → **alive**;
- `Running`: judged through the recorded role's own container, never the whole pod — a neighbour
  role's container restart changes nothing here. No status at all for that container → **unknown**;
  terminated → **dead (gone)**, its log tail quoted; otherwise the role's `legion launcher` must be
  connected on the worker stream or the verdict is **unknown** (a booting or redialing launcher is
  not death; the boot watchdog decides); once connected, its reported child generation matching the
  recorded one is **alive**, a child of another generation is **dead (not the recorded process)**,
  its last-reported exit matching the recorded generation is **dead (gone)**, a last exit of another
  generation is again **dead (not the recorded process)**, and no child ever reported is
  **dead (gone)**;
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

## Operator-launched controller

The controller is the one Legion session a person talks to, and that person starts it. The Go
daemon launches it under neither runtime, tmux included: it has no process of the controller to
start, stop or resume. It holds the controller's record and reads the session's liveness from the
Envoy role registry. While no session holds the current capability, or the registry says the
session is gone, the daemon logs `controller not registered; run legion controller start` at most
once per `worker_boot_timeout_seconds`.

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
   refuses a pi-legion-envoy it does not load, or one speaking another daemon API contract;
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
   and the deployment instructions, a start message as Oh My Pi's first prompt so the controller's first turn runs its start
   procedure with nothing typed, no `--resume`, no `--mode rpc`) with the controller's environment
   (`LEGION_CONTROLLER=1`, `LEGION_ROLE=controller`, `LEGION_DAEMON_URL`,
   `LEGION_PROJECT`, `LEGION_STATE_DIR`, its grant and secret files, the Envoy and Dispatch
   endpoints, and `NATS_NKEY_SEED_FILE` naming `nats_nkey_seed_file` when the file sets it) on top
   of the operator's own environment, less `NATS_DAEMON_NKEY_SEED` and `NATS_DAEMON_NKEY_SEED_FILE`
   (the controller is pane-side, and never gets the daemon's seed), and exits with Oh My Pi's exit
   code.

A refusal before the secret is written removes the directories made for the probe, so the state
directory is as it was.

**How the daemon sees it.** The session's pi-envoy extension registers on
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
