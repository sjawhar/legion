# Legion on Kubernetes

This runbook covers the worker image, the Kubernetes runtime, the session store, and the operator-launched controller. The TypeScript daemon no longer runs on Kubernetes: it refuses `runtime: kubernetes` at config load and names the Go daemon (LEGION-286).

## Worker image

Every Legion agent process under `runtime: kubernetes` — architect, planner, implementer, tester, reviewer,
merger — runs from one image, `ghcr.io/sjawhar/legion-worker` (public). It carries:

- the pinned OMP fork build the daemon's default `omp_invocation` names — resolved at build time with the
  same `mise x github:sjawhar/oh-my-pi@<pin>` mechanism a tmux host uses, from the single pin source
  `packages/daemon/src/daemon/omp-pin.ts`; installed at `/opt/omp/bin/omp` (`LEGION_OMP_PATH`);
- the TypeScript `legion` CLI compiled from the same commit (`legion`, `worker-shim`, `credential`, `gh`,
  `handoff`, `workspace-init`, and the hidden `probe-image`), at `/opt/legion/bin/legion` — the `legion`
  on `PATH` and the image's `ENTRYPOINT`;
- the Go coordinator's `legion` (`packages/daemon-go`), compiled from the same commit at `go.work`'s Go
  version, static, at `/opt/legion/go/bin/legion` and off `PATH`, until the Go daemon replaces the
  TypeScript one. It links the commit it was built from: `docker run --rm --entrypoint
  /opt/legion/go/bin/legion ghcr.io/sjawhar/legion-worker@sha256:… version` prints
  `legion (devel) commit <sha>`;
- `@sjawhar/pi-legion-envoy` packed from that commit's `packages/pi-envoy` (the exact `bun pm pack` steps
  `release.yaml`'s `pi_envoy` job runs) and linked into the isolated OMP profile `legion`
  (`OMP_PROFILE=legion`; plugins resolve to `/home/legion/.omp/profiles/legion/plugins/node_modules`);
- the role prompt parts at `/opt/legion/roles` (`LEGION_ROLE_PROMPTS_DIR`): phase workers compose `core/<role>.md`, `mechanics/headless.md`, and the per-role residue; merger composes headless plus its residue; root architect, controller, and sub-architect prompts remain single-file. The in-cluster daemon reads every configured part for the process it spawns. They are not part of the packed plugin (its `files` is `dist`), and the compiled `legion` binary cannot find them beside its sources the way a daemon run from a checkout does, so boot refuses, naming the directory and the missing file, if any prompt part is absent there;
- OMP's native modules, pre-downloaded into `/home/legion/.omp/natives/<version>/` so a pod never fetches them;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs) and `gh` at `/usr/local/bin`, and
  `git` at `/usr/bin/git` from the `debian:trixie-slim` base — jj's git backend requires git >= 2.42
  (bookworm's 2.39.5 made every `jj git clone` in the init container fail), so the build also proves the
  image's jj accepts its git with a network-free `jj git clone` of a scratch bare repository before the
  probes run, and its last step refuses a git anywhere but `/usr/bin/git`.

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
(`packages/daemon/src/daemon/boot-probes.ts`). The image build runs the same two probes through
`legion probe-image`, plus a third only the image runs — the session-storage probe, which prints
`session-storage=probed` on the OK line ([The image guard](#the-image-guard)) — so a build whose OMP or
plugin is broken fails instead of publishing. Its final step runs the Go `legion version`, requiring the
commit the workflow built, then the Go `legion probe-image`: the same three probes, run by the Go
daemon's own code (`packages/daemon-go/internal/daemon/bootgate.go`), with the plugin held to the Go
daemon API contract (`legion.goDaemonApiVersion`) and every task agent and skill Legion's prompts
name (`task(agent="…")`, `skill://…`) resolved by name through the same launch (the plugin ships
`oracle`, `thermonuclear-deep-review` and `thermonuclear-code-quality` in `agents/`, and the pair's
rubrics and `ce-simplify-code` with Legion's other skills in `dist/skills`). The build has none of
the operator's model configuration, so it leaves those agents' models unresolved
(`--skip-agent-models`), printing
`probe-image: OK (/opt/omp/bin/omp) session-storage=probed agent-models=skipped go-daemon-api-version=<N>`. The Go daemon's Agent Sandbox runtime runs the Go command in a probe
Sandbox, `legion-probe-<project>-<digest12>`, with its own contract, under the operator's pod, at every
boot, and requires `agent-models=resolved`: each agent's model resolves, with a working key, as the task
tool resolves a subagent's (`packages/daemon-go/internal/runtime/sandbox/probe.go`). To run them yourself:
`docker run --rm --entrypoint legion ghcr.io/sjawhar/legion-worker@sha256:… probe-image`, and
`docker run --rm --entrypoint /opt/legion/go/bin/legion ghcr.io/sjawhar/legion-worker@sha256:… probe-image --plugin-root /opt/legion/pi-legion-envoy --skip-agent-models`
(`--plugin-root` is required: the plugin root a Sandbox pod loads the plugin from, so the Go probe loads it the same way; without
`--skip-agent-models` it also resolves each agent's model, which needs the operator's model roles).

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
— no third-party builder, no project variable, no extra credential. It runs (1) from `release.yaml` after
the `cli` job on every `main` push that touches the daemon, the plugin, or the Go module and its build
inputs (below), (2) on every head of a pull request against `main` whose diff touches any of
`packages/daemon/docker/**`, the OMP pin (`packages/daemon/src/daemon/omp-pin.ts`), the plugin the image
installs (`packages/pi-envoy/**`, its role prompts and agent definitions included) and the skills it ships
(`skills/**`), the code a pod runs, the Go build inputs, or the workflow itself — building
the PR head and publishing `sha-` only — and
(3) by `gh workflow run worker-image.yaml --ref <ref>` once the workflow exists on `main`. What a pod
executes is part of the image's behaviour, so a change to it builds the image it is proven on: the
TypeScript provisioning (`packages/workspace/**`, `packages/daemon/src/cli/workspace-init.ts`), and the
whole Go module the image compiles the Go `legion` from (`packages/daemon-go/**`) — the command a Sandbox
pod runs, the launch probes `legion probe-image` runs in the image's final step and in the Go daemon's probe
Sandbox, and every package they import. The module is named whole because a hand-kept list of its packages
already missed one. The Go build inputs are `go.work`, `go.work.sum`, and `packages/envoy`'s
`go.mod`/`go.sum`: the image compiles the Go `legion` at `go.work`'s Go version, so a change that moves it
past the build stage's Go fails on its own pull request rather than in the next image build.
Trigger (2) is `pull_request`, not `push`: GitHub evaluates `pull_request` path filters against the whole PR
diff, so a later commit that touches none of those paths (a handoff, a docs fix) still gets the check and the
PR head never loses it; a `push` trigger filters on the pushed commits alone and would leave such a head
unguarded. The workflow's `packages`/`contents` permissions apply to same-repo pull requests (this
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

### Per-deployment toolchains layer on top

The base image carries Legion's own tools only. A deployment whose repositories need more (`uv`, Python,
Node) builds its own image in **its** repo:

```dockerfile
FROM ghcr.io/sjawhar/legion-worker@sha256:…
# deployment toolchain here
```

and pins `runtime.kubernetes.image` to *that* image's digest. The toolchain churns on the deployment's
schedule, not Legion's release schedule, and the deployment's repo owns its own reproducibility — one Legion
release therefore never has to know about anyone's Python version.

### Entrypoint

The image's `ENTRYPOINT` is `["legion"]`: the CLI, so `docker run --rm <image> probe-image` and
`docker run --rm <image> --help` work. A pod never relies on it — the Kubernetes runtime sets every
container's `command` explicitly (see *Anatomy of a pod* below). To run anything else in the image,
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

## Kubernetes runtime

With `runtime: kubernetes`, the daemon runs the tree's root architects and phase workers as one
Kubernetes pod per process, on one disk volume per issue tree. The controller always remains an
interactive tmux pane on the daemon host: `runtime: kubernetes` selects where roots and workers run,
never the controller. Attach with `tmux -L legion-<project> attach -t legion-<project>`. LEGION-25
Part B (`legion controller start`, "Operator-launched controller" below) is unaffected and optional.

The daemon keeps owning retries, generations, the same-agent `--resume`, and the running-worker cap
exactly as it does on a single machine; Kubernetes only supplies the root or worker process, volume,
and resource allowance. Nothing in this mode uses a Job, a StatefulSet, or `activeDeadlineSeconds`.

### Configuration

This section is the TypeScript daemon's, which now refuses `runtime: kubernetes` outright, naming the
Go daemon (LEGION-286); what follows is the block it read before. The Go coordinator (`packages/daemon-go`, LEGION-208)
reads the same `runtime.kubernetes` key with different rules, and refuses the examples below as
written: its runtime selects the Legion pool itself, so `scheduling.node_selector` may not set
`legion.dev/pool`; `resources` is keyed by role, with no `role_profiles`; `storage_class` is
required, and a `gateway` block is refused as removed (LEGION-270: a pod's model route is the
operator's `pod` below); `bind` must be an address pods reach, never `0.0.0.0` or loopback, since every pod
dials the worker stream at `tcp://<bind>:<worker_stream_port>`; and no Legion URL a pod is handed
(`daemon_url`, `envoy_url`, `dispatch_url`, each `nats_urls` entry) may name a loopback or
unspecified host (`packages/daemon-go/internal/config/kubernetes.go`). It also reads
`runtime.kubernetes.pod` — `env`, `volumes` (each one `secret`, `config_map`, or `projected`
source), `volume_mounts` (read-only unless `read_only: false`), and `service_account` — which it
adds to every pod, the image probe's included, refusing any name or path of Legion's own or the
worker image's; and the top-level `provider_keys` maps each variable Oh My Pi reads to a key of the
providers Secret below, which every pod then mounts, those keys alone, for the shim to export.
Legion holds no model route: everything a pod's Oh My Pi needs to reach a model — a `models.yml`,
a settings overlay in `PI_CONFIG_FILES`, a token — is the operator's, through `pod` and
`provider_keys`. `scripts/e2e/fixtures/operator-route/` is one such operator's (the Go live
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
claim's pod and the image probe's.

- **`runtime.kubernetes.pod`** has four keys. `env` is variables set in the agent's container.
  `volumes` are each one `secret`, `config_map`, or `projected` source; a projected
  `service_account_token` must last at least 600 s, the least the API server issues. `volume_mounts`
  are read-only unless `read_only: false`, and may use `sub_path`. `service_account` is the pods'
  ServiceAccount; unset, pods run as the namespace's `default` ServiceAccount. A name or path that
  collides with Legion's own is refused at load, naming both: a variable the runtime, the worker
  image's `ENV` or every launch sets, a volume name Legion uses, or a mount at, under or above a path
  Legion mounts, the image owns, or a tool runs from. `legion start --check-config` runs the same
  check. [`scripts/e2e/fixtures/operator-route/pod.yml`](../scripts/e2e/fixtures/operator-route/pod.yml)
  is a complete one, the live harnesses': a `models.yml` and a settings overlay from a ConfigMap, and
  a projected token its key command reads.
- **`provider_keys`** (top-level) maps each variable Oh My Pi reads to a key of
  [the providers Secret](#the-providers-secret), `legion-<project>-providers`, which the operator
  creates. Every pod mounts those keys alone, and the shim exports each into Oh My Pi's environment,
  never its own. A Secret or key the kubelet cannot mount is refused at boot by the image probe,
  naming the Secret and keys. A `provider_keys` variable that anything else in the pod sets is
  refused at load.
- **Settings order.** Oh My Pi reads `PI_CONFIG_FILES` in order, each overlay outranking the ones
  before it and all of them outranking a repository's `.omp/config.yml`. Legion writes the pod
  baseline's overlay (remote compaction, memory backends, image URLs and dev auto-QA off) and names
  it first, ahead of the operator's, so the operator's overlay outranks it. The baseline also sets
  `OTEL_SDK_DISABLED=true` and `PI_AUTO_QA=0` unless the pod sets them, and keeps the operator's value
  when it does. It sets `PI_CONFIG_DIR=.omp` and `OMP_SESSION_STORAGE=file`, which an operator's pod
  may not set, since they decide where Oh My Pi keeps the session a resume reads.
- **Model roles.** Legion's shipped agents dispatch by role alias: `oracle` as `@oracle`, both
  review agents as `@review`. The boot gate refuses, by agent, any whose role the operator's
  settings (`modelRoles`, or `task.agentModelOverrides`) leave unconfigured, or whose model's key
  does not work, because Oh My Pi's task tool would quietly run it on the parent session's model.
  The bundled agents Legion's prompts also dispatch use Oh My Pi's built-in roles: `scout` is
  `@smol` and `reviewer` is `@slow`. `smol` and `slow`, left unset in every layer, inherit the
  default role's model, which the gate accepts (no other role inherits it). Settings records merge
  key by key across layers, though, so a role the operator's overlay does not name can be named by a
  repository's `.omp/config.yml`. Name each role those agents use (`review`, `oracle`, `smol`,
  `slow`) to keep the choice the operator's.
- **The repository `.env`.** Oh My Pi's runtime loads the working directory's `.env` into its
  environment at start, filling every variable the pod left unset. A repository can therefore set
  anything Oh My Pi reads from its environment: a provider's API key, `PI_SMOL_MODEL`,
  `PI_SLOW_MODEL` and `PI_PLAN_MODEL` (which override those roles), and `CLAUDE_CODE_USE_FOUNDRY`
  with `FOUNDRY_BASE_URL` (Anthropic Foundry, which takes a model's endpoint). Set every such
  variable your route depends on in `pod.env`, where the pod's value outranks `.env`. The fixture
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
  runs off the cluster, and whoever started it starts it again (the in-cluster Deployment runs the
  TypeScript daemon). On the devbox harness, Stage 3 at `6963c3d6` ran the gateway's key command 29
  times: it minted one key, served the rest from its cache, and failed none
  (`scripts/e2e/lib/install-model-gateway.sh`'s key log).

### Cutting over an instance from tmux to pods

Drain every tree while it is still on tmux, stop the daemon, choose a new `state_dir`, and start
with `runtime: kubernetes`. The state file intentionally holds the controller's tmux locator beside
root and worker pod locators; the daemon routes them by process kind.

Before this production cutover, run the local host-daemon smoke and its checkpoints on the stack
head. The block below is the TypeScript daemon's; [Configuration](#configuration) says where the Go
coordinator's rules differ.

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

### Trust model: the provisioning token

The provisioning token, the implement App's installation token, is a credential for the whole
repository, and every agent of a tree can write the tree volume: the shared clone's hooks, its git
and jj configuration (a legacy `.jj/workspace-config.toml` included), its remote URL, its
`http.proxy`. git and jj obey all of it — they run hooks, the git jj is told to run, working-copy
filters and `ext::` transports, and send credentials through the proxy the configuration names —
so no process that can read the token may touch the tree volume. The Go coordinator's pods
(`packages/daemon-go`) keep to that with two init containers:

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

`packages/daemon-go/internal/runtime/sandbox/boundary_test.go` runs both containers exactly as the
manifest states them against nine such plants, with every one of provisioning's git and jj pins made
ineffective. The TypeScript daemon's pods, which this section describes, still provision in one init
container with the Secret mounted for its whole life (LEGION-223).

On the **tmux** runtime there is no such boundary: panes run under the daemon's uid and can read its
0600 credential files, and the daemon's credentialed clone and fetch run in the shared clone itself.
Provisioning's pins there — no git hook (`core.hooksPath=/dev/null`), the git the daemon resolved at
boot as jj's `git.executable-path`, `GIT_ALLOW_PROTOCOL=https`, no working-copy snapshot in the
credentialed fetch, the one-shot credential scoped to `https://github.com` with no askpass, and
`GIT_CONFIG_PARAMETERS` unset — are defence, not a boundary. They hold the settings they name; they
do not stop every program the shared clone's own git or jj configuration can name. One example of
what they leave open: a tree-written `http.proxy` with `http.sslVerify=false` still sees the token
on its way to github.com.

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

Node loss reattaches the EBS volume and resumes the same OMP session. Volume loss is a whole-tree
event: the root's exit first preserves its session record, then the next resync probes that dead,
open tree and attempts its normal resume. A new PVC cannot contain the recorded session, so
`workspace-init` exits 3. The daemon records `workspaceLost` and stamps every root or worker claim
that already existed. Its replacement starts fresh from the committed issue bookmark until its own
fresh session registers, even after the root rebuilt the shared clone; a role first created later
uses the ordinary fresh-worker path.
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

- pod not found → **dead (gone)**;
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
- `Pending` with the init container **terminated non-zero** → **dead (gone)**, its log tail quoted
  (`restartPolicy: Never` turns the pod `Failed` moments later);
- otherwise `Pending` for longer than `worker_boot_timeout_seconds` (unscheduled, image pull, volume
  mount) → **dead (gone)**, with the pod's events quoted; the boot watchdog's existing path retires it
  and its stop deletes the pod;
- otherwise `Pending`, or `Running` — registered stream or not (a booting or redialing shim is not
  death; the boot watchdog decides) → **alive**;
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

Nobody can open a terminal on a pod, so the controller — the one Legion session a person talks to —
is started by that person on their own machine and connects to the daemon (LEGION-25
Part B). The daemon never launches it: `KubernetesRuntime.controllerLaunch` is `operator`, and
`ensureController` mints nothing, arms no registration deadline, opens nothing, and logs
`[legion] controller not registered; run legion controller start` at most once per
`worker_boot_timeout_seconds` until one registers.

**The operator Secret.** `legion-<project>-operator` holds one key, `OPERATOR_TOKEN` — one long random
string (`openssl rand -hex 32`). The daemon's pod mounts it read-only at `/var/run/legion/operator`,
and `legion.yaml`'s
`operator_token_file: /var/run/legion/operator/OPERATOR_TOKEN` names it: required under
`runtime: kubernetes` (`operator_token_file is required when runtime is kubernetes: …`), refused under
tmux (`operator_token_file is only used when runtime is kubernetes: …`), read once at boot with no
mode check (the mount mode is the cluster's), never an environment variable or flag.
`legion start --check-config` validates the path without reading it.

**The operator-side file.** `legion controller start` reads a small file of its own — never the
cluster's `legion.yaml`, whose loader would run `private_key_command` on your laptop and demand the
image, namespace, and Envoy token the controller never uses. `deploy/kubernetes/daemon/controller.yaml.example`
is the complete shape: the same key names as `legion.yaml`, only the twelve the controller needs
(`project`, `daemon_url`, `operator_token_file`, `envoy_url`, `envoy_token_file`, `nats_urls`,
`dispatch_url`, `dispatch_token_file`, `instructions`, `omp_invocation`, `omp_launch_prefix`,
`state_dir`); any other key is refused naming it and the example (`unknown key "runtime" in the
controller configuration; …`), a missing required one is refused naming it, `dispatch_url` and
`dispatch_token_file` go together, and relative paths resolve against the file's own directory
(no `~`). The operator token sits in a file only you can read: `chmod 0600`; a group- or
world-readable file is refused naming the path and mode (`… is readable by its group or others
(mode 0640); chmod 0600 it`) before anything is fetched or written, as is a missing or blank one.

**Starting it.** Reach the daemon's API through a port-forward, then run the command:

```sh
kubectl -n legion port-forward svc/<the daemon's Service> 13370:13370 &
legion controller start --config controller.yaml --daemon-url http://127.0.0.1:13370
```

`--daemon-url` overrides the file's `daemon_url` for both the secret request and the controller's
`LEGION_DAEMON_URL`. The command, in order and writing nothing until the daemon has answered:
`POST /legion/v1/controller/secret` with the operator token as `Authorization: Bearer` (the daemon
compares it in constant time against `operator_token_file`'s hash and mints the controller
capability exactly as it does for its own tmux pane — the previous controller session's secret and
grants stop working, last claim wins); writes the secret 0600 under the local state directory
(`state_dir`, default `$XDG_STATE_HOME/legion/<project>-controller`) beside the `gh` shim
(`worker-bin/gh`, 0700), the `legion` launcher (`bin/legion`), and the deployment-instructions copy a
pane gets; then runs the same interactive OMP command the tmux daemon runs — `omp_launch_prefix` +
`omp_invocation`, one joined `--append-system-prompt` (the controller role prompt, then the
instructions), no `--resume`, no `--mode rpc` — through `sh -c` in the foreground with the shared
controller environment (`LEGION_CONTROLLER=1`, `LEGION_ROLE`, `LEGION_DAEMON_URL`, `LEGION_PROJECT`,
`LEGION_STATE_DIR`, `ENVOY_NATS_URL`, `ENVOY_URL`, the credential environment, `DISPATCH_URL` and
`DISPATCH_TOKEN_FILE`, plus `LEGION_CONTROLLER_SECRET_FILE` and `ENVOY_TOKEN_FILE` pointing at your
own files), and exits with Oh My Pi's exit code (1 on a signal death). The role prompts come from
`LEGION_ROLE_PROMPTS_DIR` or the checkout's `packages/pi-envoy/roles`, exactly as the daemon resolves
them; the directory must contain the core and mechanics fragments, phase residues, and the root,
controller, and sub-architect single-file prompts. The compiled `legion` binary has no checkout beside
it, so set the variable to that directory when running a release binary.

**How the daemon sees it.** The pi-envoy extension in that session claims the controller role and
calls `/controller/ready` by itself, exactly as under tmux; the daemon records the session as
`controllerLocator: {runtime: "kubernetes", external: true, sessionId, registeredAt}` (`legion state --json`,
`GET /legion/v1/state`; daemon-API contract 6). Liveness is the Envoy role registry, not a pane:
`KubernetesRuntime.probe` reads `GET /v1/roles/legion-<project>-controller` with the daemon's bearer and
answers alive while the holder is the recorded session and `last_seen` is within
`max(worker_boot_timeout_seconds, 2 × 120 s)` (240 s at defaults); gone on 404, another holder, or a
stale `last_seen` (each logged once); `unknown` — never dead — when the listener is unreachable.
A dead record is left in state until the next `/controller/ready` overwrites it, so the state page
shows the last known controller and when it registered.

**Re-running and failing.** Running the command again mints a new secret and takes the role (the
previous session's heartbeat learns it lost the role and stops re-asserting it). Closing the terminal
leaves the project without a controller until you run it again; the daemon logs the not-registered
line once per interval. Failures name what to fix: a wrong token —
`http://127.0.0.1:13370/legion/v1/controller/secret answered 403: Invalid operator token — the operator token does not match the daemon's operator_token_file, or this daemon has none configured`;
a closed port-forward — `could not reach the Legion daemon at http://127.0.0.1:13370: …; is the port-forward running? (never falls back to another address)`;
a tmux daemon — `answered 403: This daemon has no operator_token_file configured; the controller secret route is disabled`.
