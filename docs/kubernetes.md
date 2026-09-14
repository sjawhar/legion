# Legion on Kubernetes

This runbook covers the worker image, the Kubernetes runtime, the session store, and the in-cluster daemon.

## Worker image

Every Legion agent process under `runtime: kubernetes` — architect, planner, implementer, tester, reviewer,
merger — runs from one image, `ghcr.io/sjawhar/legion-worker` (public). It carries:

- the pinned OMP fork build the daemon's default `omp_invocation` names — resolved at build time with the
  same `mise x github:sjawhar/oh-my-pi@<pin>` mechanism a tmux host uses, from the single pin source
  `packages/daemon/src/daemon/omp-pin.ts`; installed at `/opt/omp/bin/omp` (`LEGION_OMP_PATH`);
- the `legion` CLI compiled from the same commit (`legion`, `worker-shim`, `credential`, `gh`, `handoff`,
  `workspace-init`, and the hidden `probe-image`), at `/opt/legion/bin/legion`;
- `@sjawhar/pi-legion-envoy` packed from that commit's `packages/pi-envoy` (the exact `bun pm pack` steps
  `release.yaml`'s `pi_envoy` job runs) and linked into the isolated OMP profile `legion`
  (`OMP_PROFILE=legion`; plugins resolve to `/home/legion/.omp/profiles/legion/plugins/node_modules`);
- the role prompts, `packages/pi-envoy/roles/*.md`, at `/opt/legion/roles` (`LEGION_ROLE_PROMPTS_DIR`): what
  the in-cluster daemon reads for every process it spawns — they are not part of the packed plugin (its
  `files` is `dist`), and the compiled `legion` binary cannot find them beside its sources the way a
  daemon run from a checkout does, so boot refuses, naming the directory and the missing file, if any
  prompt is absent there;
- OMP's native modules, pre-downloaded into `/home/legion/.omp/natives/<version>/` so a pod never fetches them;
- pinned Bun, `jj` (Sami's fork, the version the dogfood daemon runs), `gh`, and `git` from the
  `debian:trixie-slim` base — jj's git backend requires git >= 2.42 (bookworm's 2.39.5 made every
  `jj git clone` in the init container fail), so the build's last step also proves the image's jj accepts
  its git with a network-free `jj git clone` of a scratch bare repository before the probes run.

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

The image's `ENTRYPOINT` is `["legion"]`: the CLI, so `docker run --rm <image> probe-image` and
`docker run --rm <image> --help` work. A pod never relies on it — the Kubernetes runtime sets every
container's `command` explicitly (see *Anatomy of a pod* below). To run anything else in the image,
override it: `docker run --rm --entrypoint sh <image> -c '…'`.

## Session store

A Legion agent's conversation — its OMP session — is by default a JSONL file under `HOME`, so a pod that
loses its disk loses the conversation. The OMP fork the daemon pins (`OMP_FORK_PIN`) can store the session
in a SQL database instead, selected by two environment variables the pod (or a tmux pane) carries; the
daemon-side setting that delivers them, `session_store: postgres`, is LEGION-31's second child and is not in
this section.

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

## Kubernetes runtime

With `runtime: kubernetes`, the daemon runs every Legion agent — the tree's root architect and each
phase worker — as one Kubernetes pod per process, on one disk volume per issue tree. The daemon keeps
owning retries, generations, the same-agent `--resume`, and the running-worker cap exactly as it does
on a single machine; Kubernetes only supplies the process, the volume, and the resource allowance.
Nothing in this mode uses a Job, a StatefulSet, or `activeDeadlineSeconds`. The single-machine (tmux)
mode is unchanged byte for byte.

### Configuration

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
    resources:                   # optional overrides of the profile table below, per quantity
      large:
        limits:
          memory: 24Gi
    role_profiles:               # optional overrides of the role -> profile map below
      planner: medium
daemon_url: http://<address pods reach the daemon at>:13370   # required under kubernetes
bind: 0.0.0.0
envoy_token_file: /var/run/legion/providers/ENVOY_TOKEN       # required under kubernetes
```

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
bound off loopback requires a bearer, see "The Envoy token" below); `omp_launch_prefix` (or
`LEGION_OMP_LAUNCH_PREFIX`) is set — provider keys come from the mounted Secret, so the prefix has no
process to wrap; or the daemon runs neither with a `kubeconfig` nor inside a pod
(`runtime.kubernetes.kubeconfig is not set and /var/run/secrets/kubernetes.io/serviceaccount/token
does not exist`). Every one of those is a boot refusal before anything is spawned.

Four prerequisites and caveats the configuration cannot check for you:

- **The Envoy listener the pods publish to must accept the daemon's publishes without a bearer.** The
  daemon's own `publishToEnvoy` sends none, so `ENVOY_URL` must name a listener that is unauthenticated
  or that shares the daemon's network boundary (loopback on the daemon host, or the in-cluster Service
  a network policy scopes to the daemon's namespace). Pods that need a bearer for *their* Envoy calls
  take it from the providers Secret's `ENVOY_TOKEN`. Giving the daemon a bearer of its own is not part
  of this runtime.
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
fails the respawn loudly instead of quietly starting a new agent under the old role token.

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
| `grant` | memory-backed `emptyDir` (1Mi) | `/var/run/legion/grant` | main — where the extension writes each bash command's grant |

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
of every tool the agent runs — and never into its own
(`/proc/1/environ` inside the pod carries no key; the OMP child's does, by design).

### The providers Secret

`legion-<project>-providers` (where `<project>` is `legion.yaml`'s `project` lowercased with
non-alphanumerics removed) is created per deployment, never by the daemon, with one key per variable
OMP or the extension reads: `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`, `DISPATCH_TOKEN`,
and `ENVOY_TOKEN` when the Envoy listener requires a bearer. It is mounted read-only into every main
container, and every pod's `ENVOY_TOKEN_FILE` points at its `ENVOY_TOKEN` key — so a daemon that runs
outside the cluster over a kubeconfig and has an `envoy_token_file` must put that same token in this
Secret, or its pods' listener calls are refused. The daemon cannot `get` or `list` Secrets through the API (its Role grants neither); the
in-cluster daemon receives `DISPATCH_TOKEN` from it through the Deployment's env and reads
`ENVOY_TOKEN` from the mounted file `envoy_token_file` names. The orphan sweep never touches it: the
sweep deletes only the per-pod Secret named after an orphan pod. Nothing else belongs in it — every
worker pod mounts the volume and the shim exports each file that has no `<NAME>_FILE` pointer in the
pod into the OMP child's environment, so a GitHub App private key here would reach every worker; the
in-cluster daemon keeps those in its own daemon-only Secret (below).

### Volume retention

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

For the in-cluster daemon's Role (LEGION-25), the verbs this runtime uses on core/v1 in its namespace:

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

## In-cluster daemon

`runtime: kubernetes` can run the daemon itself as a Deployment of the worker image — the image already
carries the `legion` CLI and the `jj`, `git`, and `gh` the daemon runs — with its state on a
PersistentVolumeClaim and its configuration in a ConfigMap. Everything the tmux daemon does with a
terminal multiplexer is absent: no `mise`, no `tmux`, no local OMP (`omp_invocation` and
`omp_launch_prefix` must not be set), and the boot probes run inside a pod of the image (below). The
API binds `bind: 0.0.0.0` and `daemon_url` names the Service, so the pods it launches reach it. The
state page (`GET /legion/v1/state`, redacted) stays unauthenticated on the pod network; the
NetworkPolicy limits who reaches it. `legion status`/`stop`/`restart` assume a shared machine and are
not the way to operate a pod: the Deployment is (`kubectl rollout restart`, `kubectl scale`); SIGTERM
runs the daemon's own persist-and-exit handler. The controller is not a pod — it is started by the
operator on their own machine and connects to the in-cluster daemon (LEGION-25 Part B, following the
attachable-controller work of LEGION-16).

### Manifests

`deploy/kubernetes/daemon/` is plain kustomize, no Helm. The base carries the example project `demo`:

| resource | name | what it is |
| :--- | :--- | :--- |
| Namespace | `legion` | everything below, and the pods the daemon launches (`runtime.kubernetes.namespace`) |
| ServiceAccount, Role, RoleBinding | `legion-daemon` | exactly the verbs in "RBAC the daemon needs" — no `watch`, nothing watches |
| PersistentVolumeClaim | `legion-daemon-demo-state` | `state_dir` (`/var/lib/legion`): `state.json`, the instance lock, secrets, the image-probe cache; ReadWriteOnce, 5Gi, the cluster's default class |
| Deployment | `legion-daemon-demo` | 1 replica, `Recreate` (one instance lock, one RWO mount), `legion start demo --config /etc/legion/legion.yaml`, `DISPATCH_TOKEN` from the providers Secret, liveness `GET /legion/v1/state` (no readiness probe: the page answers throughout boot, and a definitive probe failure exits the process, which the Deployment restarts) |
| ConfigMap (generated) | `legion-daemon-demo-config-<hash>` | `legion.yaml` and `instructions.md` at `/etc/legion`; the hash suffix rolls the Deployment on a config change |
| Service | `legion-daemon-demo` | ClusterIP, `13370` (the API) and `13371` (the worker stream the shims reverse-dial) |
| NetworkPolicy | `legion-daemon-demo` | ingress from `legion.dev/project: demo` pods on both ports; egress DNS, 443 (GitHub, Dispatch, model endpoints), 4222 (NATS), 9020 (Envoy), 6443 (the API server); cluster-specific addresses and the operator's ingress are overlay additions. kind's kindnet enforces it (kind ≥ v0.21): an unlabelled pod's request to the Service times out |

Two Secrets, created by the deployment (never by the daemon), both mounted read-only:

- `legion-demo-providers` at `/var/run/legion/providers` — LEGION-24's providers contract, shared with
  every worker pod: `DISPATCH_TOKEN` (the daemon's own is the Deployment's env from this key),
  `ENVOY_TOKEN` (`envoy_token_file` points at its file), and the provider keys.
- `legion-demo-daemon` at `/var/run/legion/daemon` — daemon-only: `github-app-implement.pem` and
  `github-app-review.pem`, read by `github_apps.<role>.private_key_command: cat …`. Never the providers
  Secret: every worker pod mounts that one, and the shim exports each of its files without a
  `<NAME>_FILE` pointer into the OMP child's environment.

The project name is literal in four resource names (`legion-daemon-demo`, `legion-daemon-demo-state`,
`legion-demo-providers`, `legion-demo-daemon`) because kustomize cannot suffix the two Secret names
the runtime derives from `project`; another project is an overlay that patches those four and its own
`legion.yaml`. The base's image digest is a zero placeholder — an overlay pins the real one in **two**
places that must agree: `images:` (the Deployment) and `runtime.kubernetes.image` in `legion.yaml`
(the pods and the probe pod). The daemon pod is labelled `app.kubernetes.io/name: legion-daemon` and
`app.kubernetes.io/instance: demo`, never `legion.dev/project`: that label is the runtime's ownership
marker for the pods the daemon launches, and the orphan sweep deletes every pod carrying it that no
recorded locator names — grace 0 at boot — so a daemon pod labelled that way deletes itself (the
LEGION-25 kind run found this the hard way).

### The probe pod

A daemon in a pod is not the worker image at the configured digest, and it has no local OMP, so the
three local boot checks (`pi.agents`, plugin load, plugin daemon-API contract) run **inside a one-shot
pod of that image**: `legion-probe-<project>-<first 12 hex of the digest>` (one name per project and
image, so two projects' daemons in one namespace never contend for a pod), `restartPolicy: Never`, the providers
Secret mounted read-only (a missing Secret fails the mount — one of the things the probe proves), no
tree volume and no shim, running `legion probe-image --daemon-api-version <N>` with the `small`
profile's resources. The image's own CLI runs the two OMP probes and compares its plugin's
`legion.daemonApiVersion` to `<N>`, the daemon's contract; a mismatch exits 1 naming both. The daemon
polls the pod every 2 s under `slow_command_timeout_seconds`, reads its last 50 log lines, and always
deletes it — a leftover of the same name from a crashed boot is deleted and awaited first, but only
when its `legion.dev/project` label is this daemon's; a same-name pod of another project (or of none)
is a definitive refusal naming that project, and nothing of theirs is deleted.

| outcome | classification | what happens |
| :--- | :--- | :--- |
| `Succeeded` with `probe-image: OK … daemon-api-version=<N>` in the log | pass | `<state_dir>/image-probes/<64 hex>.json` written atomically: `{digest, daemonApiVersion, probedAt}`; the launch hold releases |
| `Failed` | definitive | startup refuses, quoting the log — a contract mismatch names both versions and the digest; the daemon exits 1 and the Deployment restarts it |
| `Succeeded` with an OK line carrying no `daemon-api-version=` | definitive | refused (`… predates the check`): an image whose `legion` CLI predates `--daemon-api-version` ignores the flag (citty drops unknown options), runs the two OMP probes, and prints a bare `probe-image: OK` — it checked no contract, so it is not waved through |
| `Succeeded` with an OK line confirming another contract `<M>` | definitive | refused: `confirmed daemon API contract <M>, this daemon requires <N>` |
| the pod vanished mid-poll (404) | transient | another actor deleted it; the next attempt creates it again |
| API failure while creating or reading the pod | 400/401/403/422 definitive (the request will be refused again: a malformed request, RBAC, credentials, a rejected manifest), and a 404 on the create (the namespace does not exist); anything else transient | the message is the detail; a transient one is retried with the probe backoff |
| still `Pending`/`Running` at the budget | transient | retried with the daemon's probe backoff (10 s doubling to 5 min, unbounded), the phase and the container's waiting reason (`ImagePullBackOff`, …) logged each attempt; the state page answers meanwhile |
| container waiting with `InvalidImageName` / `ErrImageNeverPull` | definitive | refused at once |

The cache is per (digest, contract): a restart with the same image and the same daemon contract passes
from the file with no API call (`worker image <digest> passed its probe at <probedAt> … reusing <file>`
in the log), so a crash-restart loop never launches a second probe pod for a digest that already
passed. A cache file that is unreadable, malformed, or names another digest or contract is logged
naming the file, ignored, and rewritten by the next pass. Only a pass is cached: a definitive failure
runs a probe pod on every restart, each quoting the failure, spaced by `CrashLoopBackOff`.

### The Envoy token

A listener bound off loopback requires `ENVOY_API_TOKEN` (it refuses to start otherwise —
`ENVOY_API_ALLOW_UNAUTHENTICATED=1` is a Fargate-transition flag, never something these manifests set),
and answers every `/v1` request without that bearer with 401. The daemon reads its bearer from
`envoy_token_file` (a relative path resolves against `legion.yaml`'s directory) or `ENVOY_TOKEN_FILE` — a
0600 file's trimmed contents; a set-but-missing, unreadable, or blank file refuses startup naming the
key and the path, never a fallback — or, lower in precedence, the plain `ENVOY_TOKEN` environment
value. Required under `runtime: kubernetes`; optional under tmux, where an unset token changes
nothing. Every daemon call to the listener sends `Authorization: Bearer <token>`; a 401/403 keeps the
usual `EnvoyPublishError` and logs one line naming the listener URL and whether a token was sent.

Every process the daemon launches gets the same token the way it gets its boot token: as a 0600 file
named by `ENVOY_TOKEN_FILE` — under tmux `<state_dir>/secrets/<role token>-envoy_token`, pruned with
the pane's other files; under kubernetes always the providers Secret's own `ENVOY_TOKEN` file
(`/var/run/legion/providers/ENVOY_TOKEN`, mounted in every pod) — the token is never copied into a per-pod
Secret, and never appears as an environment value. `@legion/envoy-client` reads `ENVOY_TOKEN_FILE` ahead of `ENVOY_TOKEN`
(an unreadable or blank file is an error naming both, not a fallback), so the pi-envoy extension in
every pane and pod, and the operator-launched controller, authenticate with it.

### Running it on kind

The overlay `deploy/kubernetes/daemon/overlays/kind` is a template for a cluster of your own
(`kind create cluster --name <name>`); pods pull the public image from GHCR, so nothing is built
locally. Every value in it is a placeholder — the image digest, the host address, the Dispatch
project and repository, the two GitHub App ids, and the secrets — and `kubectl apply -k` runs it as
written, so replace them first; the project and repository must be ones set aside for the run,
never a live one. A NATS server and an Envoy listener run on the host, bound where pods can reach
them — the kind docker network's gateway (`docker network inspect kind -f '{{(index .IPAM.Config 1).Gateway}}'`,
`172.30.0.1` on the box the overlay was written on; substitute yours below and in `legion.yaml`) —
with the listener requiring a token:

```sh
docker run -d --name kind-nats -p 14222:4222 nats:2.10 -js
(cd packages/envoy && go build -o out/envoy-listener ./cmd/listener)
ENVOY_TOKEN=$(openssl rand -hex 24)
PORT=19020 ENVOY_LISTEN_HOST=172.30.0.1 ENVOY_API_TOKEN="$ENVOY_TOKEN" ENVOY_MACHINE_ID=kind \
  NATS_URLS=nats://127.0.0.1:14222 packages/envoy/out/envoy-listener &
```

Then the overlay's values and inputs, and the apply:

```sh
cd deploy/kubernetes/daemon/overlays/kind
# legion.yaml: envoy_url / nats_urls / dispatch_url (the gateway address), dispatch_project and
#   repos (a project and repository set aside for the run), github_apps.<role>.app_id (your Apps)
# kustomization.yaml `images:` digest and legion.yaml `runtime.kubernetes.image`: the digest of the
#   worker image to run (a `Worker Image` workflow run's job summary)
cp secrets/providers.env.example secrets/providers.env        # DISPATCH_TOKEN, ENVOY_TOKEN, provider keys
# secrets/github-app-implement.pem, secrets/github-app-review.pem: the two App private keys
kubectl --context kind-<name> apply -k .
kubectl --context kind-<name> -n legion rollout status deploy/legion-daemon-demo
```

What to look for, in order:

1. **State through the Service** (proves `bind: 0.0.0.0` and `daemon_url` — and the NetworkPolicy:
   the pod must carry the ingress label):
   `kubectl -n legion run curl --rm -i --restart=Never --image=curlimages/curl --labels=legion.dev/project=demo -- curl -s http://legion-daemon-demo.legion.svc:13370/legion/v1/state`
   answers the redacted state JSON; the same pod without `--labels` times out (`HTTP 000`).
2. **The probe pod, once**: `kubectl -n legion logs deploy/legion-daemon-demo` shows
   `worker image sha256:… : probe pod legion-probe-demo-<12> passed: probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=<N>`;
   `kubectl -n legion get pods` shows no `legion-probe-*` afterwards;
   `kubectl -n legion exec deploy/legion-daemon-demo -- cat /var/lib/legion/image-probes/<64 hex>.json`
   is `{digest, daemonApiVersion, probedAt}`.
3. **The cache on restart**: `kubectl -n legion rollout restart deploy/legion-daemon-demo`; the new
   pod's log says `passed its probe at <probedAt> … reusing …`, and
   `kubectl -n legion get events --field-selector reason=Created` names no `legion-probe-*`.
4. **A role publish answers 200** against the token-requiring listener. Until Part B lands a real
   controller, stand in for the holder: register a session with the listener (`POST
   /v1/interests/subscribe`, then `POST /v1/roles/set {role: legion-demo-controller}`), have it
   answer its `notifications.agent.<session>` NATS subject with an empty receipt, relay the Dispatch
   server's issue events onto that NATS as `notifications.dispatch.issue.>` messages, and create a
   root issue in the overlay's `dispatch_project`. The daemon consumes `<KEY>.issue.created`, publishes
   `{"type":"triage","issue":"<KEY>"}` to the controller role with its bearer (the listener logs
   `listener received … topic: notifications.role.legion-demo-controller`; no `refused the publish`
   line in the daemon's), and the stand-in receives the envelope. The same publish by `curl` without
   the bearer answers 401.
5. **Negative controls**: remove the providers Secret's `ENVOY_TOKEN` key
   (`kubectl -n legion patch secret legion-demo-providers --type=json -p '[{"op":"remove","path":"/data/ENVOY_TOKEN"}]'`)
   and restart — startup refuses `envoy_token_file names /var/run/legion/providers/ENVOY_TOKEN, which
   could not be read: ENOENT`; point `runtime.kubernetes.image` (the ConfigMap only — the Deployment keeps the
   real image) at an image built before this branch — its `legion probe-image` ignores
   `--daemon-api-version`, the pod `Succeeded`s with a bare `probe-image: OK`, and the daemon exits
   `failed its probe: pod legion-probe-demo-<12> Succeeded without confirming daemon API contract <N> (its
   legion CLI predates the check) — log tail: probe-image: OK (…)`; an image whose plugin speaks another contract `Failed`s the
   pod instead and the daemon quotes `speaks daemon API contract <M>; this daemon requires <N>`.
