# Legion

Autonomous development swarm using Oh My Pi agents. Root processes own issue trees, and the Legion daemon supplies durable state, credentials, and event routing.

## Architecture

Legion's issue lifecycle lives entirely on native Dispatch (triage → done); the daemon never
creates, reads, or writes a GitHub issue. It derives role and gate state from Dispatch issue
events plus GitHub PR/CI artifacts, records root-process session locators, and publishes only the
verdict changes each role needs. The daemon (`packages/daemon`, Go) runs every root architect and
phase worker as a headless `omp --mode rpc` process bridged through `legion worker-shim`: one pane
of its private tmux server per process under `runtime: tmux`, or, under `runtime: kubernetes`, one
Agent Sandbox pod per issue running the published worker image, with one container and
`legion launcher` per resident role (`docs/kubernetes.md`; `scripts/e2e/stage4a-sandbox-runtime.sh`
is that runtime's live proof).

- **Daemon** — Dispatch and GitHub intake, the fixed workflow table, the durable issue record in
  Postgres, admission, process supervision under either runtime, each role's GitHub App credential, session grants, and recovery.
- **OMP extensions** — two Oh My Pi plugins: `@sjawhar/pi-envoy` (`packages/pi-envoy`), which
  every session loads for Envoy messaging and the native Dispatch tools, and `@sjawhar/pi-legion`
  (`packages/pi-legion`), which a Legion pane loads beside it and which injects the Legion tool
  (its daemon calls carry session grants the tool mints in-process) and the daemon handshake into that session, reaching the Envoy plugin
  through the versioned interface in `packages/pi-shared`. The daemon starts every phase worker
  (planner/implementer/tester/reviewer/merger) itself, from its fixed workflow table; a
  sub-architect for a child issue runs only when the operator starts one. The controller is an
  interactive OMP session the operator starts on their own machine with `legion controller start`
  (no `--mode rpc`, no shim), unless `legion.yaml` sets `controller: daemon`, where the daemon
  launches it in an Agent Sandbox pod of its own, whose one container runs the controller's
  `legion launcher`, and supervises it like a root architect (`docs/kubernetes.md`, "The
  controller").
- **Skills** — guide the architect and sequential phase workers. Durable `.legion/<issue>/<phase>.json`
  handoffs are the recovery source of truth.

## Tech Stack

- **TypeScript** on **Bun** runtime — the version `.bun-version` pins. Every job installs it through `.github/actions/setup-bun`, and the two Dockerfile `ARG BUN_VERSION` defaults (`packages/envoy/docker/Dockerfile`, `packages/daemon/docker/worker.Dockerfile`) must equal it, since no build passes `--build-arg`. Raising Bun is those three edits together; `.github/scripts/check-bun-version.sh` fails the build until they agree
- **Go** for the Legion daemon and its `legion` CLI (`packages/daemon`) and for Envoy (`packages/envoy`), the two modules the root `go.work` binds
- **Oh My Pi extensions**: `@sjawhar/pi-envoy` (`packages/pi-envoy`) for Envoy messaging and the Dispatch tools in every session, `@sjawhar/pi-legion` (`packages/pi-legion`) for the Legion tool, the daemon handshake, role delivery and phase workers, and the private `@legion/pi-shared` (`packages/pi-shared`) both bundle
- **Zod 4** at one exact version in every manifest that declares it: `@legion/contracts` hands its schemas to other packages, and two zod 4 copies meeting in one call fail the typecheck or the load. Raising zod edits every manifest together; `.github/scripts/check-zod-version.sh` fails the build until they agree
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, native **Dispatch** for issue tracking
- **Container images** from Docker Hub are pulled through Google's mirror `mirror.gcr.io` (`mirror.gcr.io/library/<name>` for an official image, same tags and digests) in Dockerfiles, workflows and tests, because GitHub-hosted runners hit Docker Hub's anonymous pull rate limit; CI jobs that run testcontainers set `TESTCONTAINERS_RYUK_DISABLED`, so its Ryuk image is never pulled

## Commands

```bash
bun install                   # Setup
bunx biome check <package>/   # Lint (the root pins @biomejs/biome so bunx resolves the real Biome; CI's required `lint` job runs `bunx biome check .`, and a package's `bun run lint` checks only the paths its recipe names)
bunx tsc --noEmit             # Type check
bun test                      # Test
```

```bash
legion start [--config <legion.yaml>] [--check-config]  # Run the daemon in the foreground for the project that file names (./legion.yaml by default); --check-config also prints the configuration's open capability gaps after its OK line (exit 0), and under `runtime: kubernetes` the plugin pair a stale-plugin relaunch would install or the gap that leaves it relaunching none (docs/kubernetes.md "Contract discipline")
legion status <team>                 # Check daemon status
legion status <issue> <status>       # The operator's: set an issue's Dispatch lifecycle status (todo|backlog|icebox) from an operator shell; `--operator-token-file <file>` is required (the controller sets a status through its `legion` tool's `set_status`, never from bash)
legion stop [--config <legion.yaml>]  # Stop the registered daemon of the project that file names
legion restart <team>                # Restart daemon, preserve worker sessions
legion legions                       # List registered Legion daemons
legion state                         # An operator and coordinator command: read daemon state; `--json` carries the deployment's capability report (`capabilities`). Agents never run it: a role reads its issue record with the `legion` tool's `read_record`, the controller the whole state with `read_state`
legion worker-shim --connect <unix:///path|tcp://host:port> --boot-token-file <path> [--provider-env-dir <dir>] [--pod-safety] [--stop-grace <duration>] [--warm-codegraph] [--agent-secrets-key-dir <dir> --pod-token-file <path> --agent-secrets-bin <path>] -- <omp argv…>  # Bridges a headless OMP process (a root architect, a phase worker, or the controller the daemon launches under `controller: daemon`) to the daemon: the shim dials the daemon's worker stream listener (`unix://` from a tmux pane, `tcp://` from a pod) and authenticates with its boot token; --provider-env-dir exports each mounted secret file as NAME=contents into the OMP child's environment only (skipping a NAME the pod already consumes through a NAME_FILE pointer, e.g. DISPATCH_TOKEN; and refusing to start — exit 1 naming the key and its file, Oh My Pi never spawned — when a key's name is already a variable of the shim's own environment, since the export would override it silently (LEGION-186)); the shim also answers the daemon's `adopt-working-copy` frame by running the shared `jj metaedit --update-author` in its workspace; with `--pod-safety` (the Sandbox runtime passes it; a tmux pane never does), the shim starts Oh My Pi on Legion's pod baseline (`packages/daemon/internal/podsafety`), which names no model, provider or route and holds nothing of a repository's settings off: the turn-scoping overlay (`bash.autoBackground` and `async` off, the two keys a takeover's abort depends on) written to `LEGION_STATE_DIR` and named first in `PI_CONFIG_FILES`, so the operator's overlay outranks it and both outrank a repository's `.omp/config.yml`, and `PI_CONFIG_DIR=.omp` and `OMP_SESSION_STORAGE=file` each set only when the pod leaves it unset (the runtime refuses both in an operator's pod, since they place the sessions a resume reads); with `--warm-codegraph` (the Sandbox runtime passes it for a role in an issue pod; never for a tmux pane, whose workspace the daemon warms itself, nor for the controller, which has no workspace; refused without `LEGION_WORKSPACE`), the shim builds the CodeGraph index of the workspace `LEGION_WORKSPACE` names in the background once Oh My Pi has written its first frame, under a cross-process lease at `.codegraph/legion-warm.lock` so the six role shims sharing an issue's workspace never build it twice, and a stop mid-build ends the warm-up with Oh My Pi (the shim exits only once the lease is released, so the relaunch never reads a live build; its wait for that release is at least 3 s and at most what `--stop-grace` (the pod's terminationGracePeriodSeconds, `worker_stop_timeout_seconds`) leaves after the agent's 5 s grace and the 1 s stdout drain, 4 s by default, so the whole stop fits from 9 s up and the daemon refuses a smaller `worker_stop_timeout_seconds` under `runtime: kubernetes` at load; a finished build logs one success line with the build's duration and the time since the role's launch); a pod's model route is the operator's (`runtime.kubernetes.pod` and `provider_keys`, docs/kubernetes.md "Operator configuration"); the Sandbox runtime's three flags for a role's process, the daemon-launched controller's included, in a pod enrolled with the secrets broker (runtime.kubernetes.agent_secrets): the shim runs agent-secrets keygen there before its hello, sends the key's thumbprint and the projected token in hello2, answers the daemon's `agent-secrets-token` frame with the token as its mount holds it then (asked when the hello's token would expire before the daemon enrolls), keeps the enrollment id the daemon hands back, and runs agent-secrets renew; the daemon-launched controller's process gets them as a workflow role's does, so its `slack_post` is granted the Legion daemon's `LEGION_SLACK_BOT_TOKEN`, and a tmux pane never gets them (daemon-spawned under either runtime, by a tmux pane's command or by a pod launcher's start command, never run by hand)
legion launcher --connect tcp://<host>:<port> --token-file <path> --sandbox <name> --role <role> [--pod-uid <uid>] --private-dir <dir>  # PID 1 of one role container in an issue pod, or of the one `controller` container of the project controller's own pod under `controller: daemon` (Kubernetes runtime; daemon-spawned): authenticates to the worker stream with the role's own launcher token, bound to the pod's uid (POD_UID by default), reports the child it runs after every connection, and starts or stops that role's `legion worker-shim` process group on the daemon's command; each generation's boot token and launch credentials arrive in the start command and are written owner-only and exclusively into a fresh `g<generation>` directory of --private-dir (anything already there removed without being followed) and removed when the generation ends; it survives a daemon restart without stopping the child, refuses a generation that already ran or a reused request id with another payload, and never restarts a child itself
legion controller start --config <controller.yaml> [--daemon-url <url>]  # either runtime — the operator starts the interactive controller on their own machine: reads the strict operator-side file (deploy/kubernetes/daemon/controller.yaml.example), refuses an operator token file others can read, finds the real gh its session's `gh` wrapper runs (`LEGION_GH_PATH`, or the first `gh` on PATH but its own wrapper; refused when there is none), fetches the controller secret from POST /legion/v1/controller/secret with that token as a bearer, then the controller's GitHub credential from POST /legion/v1/controller/github-credential with that secret (the review App's read-only gh files for every owner of the project, one directory per owner under `<state_dir>/gh`, `GH_CONFIG_DIR` naming the first owner's, refreshed every minute while Oh My Pi runs, logged to `<state_dir>/github-credential.log`; against a daemon with no GitHub App it says so and empties the directory; any other refusal after the mint exits 1 with Oh My Pi never started), launches Oh My Pi in the foreground with the shared controller environment, exits with its code
legion gh [gh args…]  # the `gh` first on every tmux pane's and pod's PATH (a script in the pane's launcher directory, `/opt/legion/bin/gh` in the image) runs it, never a person: it runs the real gh (LEGION_GH_PATH) with GH_CONFIG_DIR at the directory of the owner the command targets under the role's gh bundle (LEGION_GH_OWNERS_DIR: one directory per GitHub owner of the project, the issue's owner's with the role App's token, every other owner's read-only), read as gh reads the command, with pflag's rules, from the real gh's own help of each command down to its leaf (its selectors: -R/--repo before or after the command, a repository or owner argument, before `--` or as a clone's or fork's first after it, a github.com URL argument (for a pr command or agent-task view only a pull request's, any other naming a branch of its own repository), every one of a slot gh's usage takes several of, agent-task view's OWNER/REPO#N, `gh api`'s repos/orgs/users/<owner>/ endpoint and a search endpoint's q, a search query's plain repo:/owner: qualifiers, the options naming a repository, an owner, a team or an issue by URL; and the command's own repository whenever gh resolves it, because no argument names it, or whatever names it for `repo set-default`: GH_REPO for a command that reads it, gh api's `{owner}`/`:owner` placeholders among them, and otherwise every github.com remote and gh-resolved setting of the working directory, read with gh's own URL parser), at the issue owner's when the command names no repository at all and the working directory names none; the real gh runs with GH_PATH at that wrapper, so the git gh runs itself (a clone, a fork's parent, `pr checkout`) asks `legion gh auth git-credential`, which answers as `legion git-credential` does, and with gh's update check and prompts off; it sends nothing when only gh's help or `gh --version` alone runs; it fails closed, refusing with exit 2 and naming the argument, and sending nothing, a command it cannot read for certain (an option the leaf's help does not list or names a repository with in a way it does not know, a subcommand gh does not know, a first word gh's own help does not list, anything before the command other than -R or --help, an argument the leaf's usage has no place for or names in a way it does not know, a search qualifier gh or GitHub could read otherwise, a gh alias or an extension, run or named as `gh help`'s target, whose help it never reads since reading it runs them, `gh copilot`, `gh extension exec`), one naming two owners (an option or another repository naming one owner while gh acts on another's repository among them), one naming an owner the bundle holds no directory for, every command run where PATH's first gh is not Legion's wrapper (a script that execs `legion gh` by its absolute path), and every command under a GH_HOST other than github.com or a set GH_TOKEN or GITHUB_TOKEN (docs/deployment.md "What a pane's gh, git and jj are")
legion git-credential get|store|erase  # every shared clone's git credential helper (`!legion git-credential`, with credential.useHttpPath on), and, as `legion gh auth git-credential`, the helper of the git gh runs itself; git runs it, never a person: `get` answers https://github.com with the token of the owner the requested path names from that owner's directory of LEGION_GH_OWNERS_DIR and from nothing else, answering nothing (exit 1, naming why) for any other host, a request with no path, or an owner the bundle holds no directory for; `store` and `erase` write nothing
legion sessions import --dsn-file <url file> --claims <claims.json|-> [--tree <KEY>] [--tree-volume <dir>]  # the one-off copy from legion-v10.0.0 before runtime.kubernetes.session_store postgres: writes each claim's recorded session file (from `legion claims list --json`) into Oh My Pi's session table under its recorded path, read from the tree volume mounted at --tree-volume; one line per selected claim (copied, copied before, recorded no session, failed, refused), then a `missing` line for every claim of the list whose session the table lacks, then a count of each; exit 1 on a failure, a refusal, a missing session of the selection, or a --tree that selects no claim; a session the table already holds byte for byte is not copied again, and one it holds with other content is refused and left as it is; `scripts/sessions-import-pods.sh` runs the copy in one worker-image pod per tree, finding each tree's volume and root Sandbox by legion-v10.0.0's labels and scheduling the pod as that Sandbox's pods were, and refuses a project on a later release, one with a Sandbox not Suspended, or one with a pod that has not ended (docs/kubernetes.md "Copying file sessions before turning it on")
legion sessions mark-lost --dsn-file <url file> --claims <claims.json|-> --daemon-dsn-file <file>  # copies nothing: marks each claim of the list whose session the table lacks lost in the stopped daemon's database (no session, no session file, its workspace lost), with plain SQL against legion-v10.0.0's claims table rather than this release's store, after refusing, with nothing marked, when that database holds a claim of the list's project recording a session file the list does not give it (one launched after the list was saved)
legion probe-boot [--config <legion.yaml>]  # runtime: kubernetes only — the part of the daemon's boot its worker pod can refuse, from the file and with no workflow: boot's reads of the file, the caller's environment and the files they name (no App key command runs), then the cluster check and the daemon's own image probe, its Sandbox built from the operator's pod under a name of its own run, so it may run while the daemon boots; run the `legion` of the release the daemon will restart on, since the probe's contract, references, command, reservation and verdicts are compiled into it; under the credentials runtime.kubernetes names (a pod's own ServiceAccount in a pod), which give the daemon's result only as the daemon's identity, best as a one-shot pod of the image of the release the daemon will restart on, under its ServiceAccount with the daemon pod's environment and mounts, so no daemon credential leaves the cluster; run it with the daemon's environment (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED shapes the probe when nats_nkey_seed_file is unset); no store, GitHub App mint, NATS or Dispatch; a pass holds only against the cluster's ConfigMaps, Secrets and ServiceAccount as they are at the run, so a change that adds one applies it first; it waits on pool capacity without limit, as the daemon does, so the caller bounds it, and its pod competes with live issue pods for that capacity; prints `probe-boot: OK project=… image=… model-fallback=… capacity-waits=…` and exits 0, or boot's refusal and exits 1 (docs/kubernetes.md "Probing a configuration before the daemon boots on it")
cd packages/daemon && go build ./cmd/legion   # The Legion daemon and CLI (LEGION-208): version|start|stop|state|legions|status|restart|worker-shim|launcher|model-token|claims|sessions|probe-boot|probe-image|workspace-init|controller, its own $XDG_STATE_HOME/legion/legions.json registry, its own Postgres schema; a release build links a `@sjawhar/pi-legion`/`@sjawhar/pi-envoy` pair into the binary, so its `version` prints a second line, `plugins @sjawhar/pi-legion <v> @sjawhar/pi-envoy <v>` (docs/kubernetes.md "Contract discipline"); its `workspace-init` is an issue pod's two init containers — `workspace-init fetch --repo <owner>/<repo> --feed <dir>` clones from GitHub into the pod's feed and is the only process holding the provisioning token (LEGION_PROVISION_TOKEN_FILE; an implement-role container later reads the same App's token from its own gh files, a review-role container never), and `workspace-init provision --issue <KEY> --repo <owner>/<repo> --default-branch <name> [--root /legion] --credential-helper <git helper> --feed <dir>` builds the issue's clone and jj workspace on the issue's own volume from that feed without the token, an issue with no branch starting at the repository's default branch as GitHub names it, which the daemon reads and the pod's command names — one clone per issue pod, under no lock and waiting on no other pod, since no other pod mounts the volume, after removing every stale git lock file (`<file>.lock`) in the clone's git directory and the issue's own worktree entry, each logged with its age, since no role container of the pod has started either (docs/kubernetes.md "Anatomy of a Sandbox pod") — records when it created the issue's workspace (`.jj/legion-created`, which the role launcher compares with a resumed session's last write), and when LEGION_EXPECT_ISSUE_VOLUME is true (the launching claim resumes a session, or any stored claim of the issue already retained one) exits 3 if the volume holds neither the clone nor any retained session, which the daemon reads as the issue's volume lost and answers by relaunching the issue's roles as fresh sessions that recreate the workspace from the issue's branch, never a quiet fresh agent (docs/kubernetes.md "Trust model: the provisioning token" and "Volume retention"); under `runtime.kubernetes.session_store: postgres`, where sessions live in the database rather than on the volume, no pod is told LEGION_EXPECT_ISSUE_VOLUME, so a claim resumed on a new volume provisions its workspace from the issue's pushed branch and continues its own session (docs/kubernetes.md "Selecting the store") — and the daemon-launched controller pod's one init container, `workspace-init controller --root /legion`, which makes its sessions directory and, on a resume whose LEGION_RESUME_SESSION_FILE is missing, exits 3 for a fresh controller, a variable its pod is likewise not told under `postgres` (docs/kubernetes.md "Daemon-launched controller"); `legion controller start` resolves `omp_invocation` as the daemon does, with no pinned default, and refuses before its one daemon call, by launching the operator's Oh My Pi as the controller will run, a pi-legion it does not load, or loads speaking another daemon API contract, or loads without pi-envoy or with a pi-envoy at another plugin interface version, or beside the pre-split package; that call names the contract, and the daemon refuses another with 409, naming both, before it mints a capability, so a `legion` and a daemon of different releases never revoke the running controller; and its controller registers on `/legion/v1/claims/register`, which refuses another contract naming both; `legion start --check-config` validates a file without running its key commands; `legion status <issue> <status> --operator-token-file <file> [--config|--port]` sets a status from an operator shell over the operator bearer, whose file `legion status` and `legion claims` refuse when its group or others can read it, as `controller start` does
bash scripts/e2e/stage1-skeleton.sh    # Stage 1's live proof: the Go daemon boots on a real Postgres, serves GET /legion/v1/state, restarts against the same store, and refuses an unreachable Postgres by host (scripts/e2e/README.md)
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/stage2-tmux-supervision.sh   # Stage 2's live proof, devbox only: real Oh My Pi panes with the branch's two plugins (pi-envoy and pi-legion) under the Go daemon's private tmux server, against a real Envoy listener and NATS — register, role claim, delivery once, resume, suspend, the registration deadline, re-adoption across a restart, the orphan sweep, the gate's refusal of pi-legion without pi-envoy (scripts/e2e/README.md)
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://envoy-nats.<tailnet>.ts.net:4222 bash scripts/e2e/stage3-devbox-workflow.sh      # Stage 3's devbox-only live proof: real OMP panes drive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, the daemon's merge of the READY head, production check, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke (scripts/e2e/README.md)
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> LEGION_E2E_SESSION_DB_PORT=<port> LEGION_E2E_REPO_SECOND_OWNER=<owner>/<name> bash scripts/e2e/stage4a-sandbox-runtime.sh   # Stage 4a's live proof, devbox only: the Go Agent Sandbox runtime on the production cluster (namespace legion) through the Legion daemon's restricted identity, on the operator fixture's pod and a providers Secret of its own — install check, the image probe with the daemon's own command (and its refusal once the operator's overlay drops a role an agent names), gVisor, every container's reservation with no affinity, the operator's ServiceAccount and token, the pod baseline, the provider key reaching the agent alone, suspend/resume, same-agent refusal, kill, relaunch before registration, independent provisioning (two issues of one tree, each pod on a volume of its own), an issue on a second owner's repository reading sjawhar/legion-smoke read-only through its gh and git and refused a push there, re-adoption, the orphan sweep taking the swept issue's volume, release, a resume under `session_store: postgres` held to the table of a scratch Postgres the script starts on the devbox's private address at `LEGION_E2E_SESSION_DB_PORT` (a port the Legion nodes can reach), and a namespace left as it was (scripts/e2e/README.md)
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> LEGION_E2E_DISPATCH_URL=<dispatch> LEGION_E2E_ENVOY_URL=<listener> LEGION_E2E_NATS_URL=nats://<nats>:4222 LEGION_E2E_DISPATCH_TOKEN_FILE=<0600 file> LEGION_E2E_ENVOY_TOKEN_SECRET_ID=<secret id> bash scripts/e2e/stage4b-sandbox-tree.sh   # Stage 4b's live proof, devbox only: the Go daemon drives three real issue trees on the Agent Sandbox runtime in the production cluster (namespace legion), against production Dispatch (project LEGSMOKE), Envoy and NATS, with real agents — admission under the cap, specs, the daemon bound to every interface with every pod dialing its advertise_host, issue independence (a root and two of its children at once, each pod on a volume of its own with no affinity, none able to read another's workspace) and a done child's release of its Sandbox and volume while its parent runs, the repository-configuration fixture, the whole workflow to done with the review pair and a review round no review decides until the architect asks, then a changes-requested round whose approval does not wait for its thread to resolve, and a READY with every thread resolved, CI going red mid-test interrupting the tester's turn before the implementer starts, every worker's phase completion answered in the live session it keeps in its first pod until its issue closes, a finished role answering while another phase runs, token rotation, kill and fence, a daemon restart mid-tree relaunching nothing, a restart with only `envoy_url` respelled relaunching every role in its own issue pod and one with the worker stream and API moved replacing every issue pod, the operator's controller, a worker that dies with its task outstanding, the close suspending every role, a child in a turn at the close carried into a tree of its own by the proof human's label, its new tree launching nothing until that turn ends and keeping its Sandbox, volume and session, node release, linger close, a child the close left mid-run handed to Legion as a carried tree of its own (production Dispatch's refusal of the hand-over's `labels_add` ridden out, then the proof human's label), re-admission, every pod's shape and reservation, the pod watch, and an audit that the run wrote nothing outside LEGSMOKE; one run at a time, STAGE4B_UNTIL=<checkpoint> for a development run (scripts/e2e/README.md)
```

## Configuration

```yaml
projects:
  LEGION: { repo: sjawhar/legion }
  WIDGETS: { repo: acme/widgets, merge_queue_role: merge-queue }
```

## Version Control

**jj (Jujutsu), not git.** Changes auto-accumulate. Push directly.

| Task                | Command                            |
| ------------------- | ---------------------------------- |
| Status / Log / Diff | `jj status` / `jj log` / `jj diff` |
| Push / Fetch        | `jj git push` / `jj git fetch`     |

## WHERE TO LOOK

| Task                   | Location                                      | Notes                                     |
| ---------------------- | --------------------------------------------- | ----------------------------------------- |
| Add CLI command        | `packages/daemon/cmd/legion/main.go`          | one `command` per subcommand                   |
| Change Legion API      | `packages/daemon/internal/api/`               | `state.go` owns the wire shape; `packages/contracts/src/legion-api.ts` mirrors it |
| Change daemon state    | `packages/daemon/internal/store/`             | Postgres migrations and the issue record        |
| Deployment instructions (`legion.yaml` `instructions:`) | `packages/daemon/internal/config/files.go` | Operator markdown appended to every pane's system prompt |
| Add phase guidance     | `skills/legion-worker/SKILL.md`                | See `skills/AGENTS.md`                          |
| Change architect loop  | `skills/legion-architect/SKILL.md`             | See `skills/AGENTS.md`                          |
| Handoff ledger         | `.legion/` on issue branch                     | Committed phase output; retro's last commit removes `.legion/<issue>/` before READY, so none reaches the default branch |
| Envoy event routing    | `packages/envoy/`                              | See `packages/envoy/AGENTS.md`                  |
| Shared event contracts | `packages/contracts/`                          | See `packages/contracts/AGENTS.md`               |
| Envoy OMP adapter      | `packages/pi-envoy/`                          | `@sjawhar/pi-envoy`: the Envoy and Dispatch tools every session loads. See `packages/pi-envoy/AGENTS.md`          |
| Legion OMP plugin      | `packages/pi-legion/`                         | `@sjawhar/pi-legion`: the Legion entry, the Legion tool (`handoff_complete`, `read_record`, the controller's `read_state`/`set_status`), its daemon client, the task agents and the Legion skills. See `packages/pi-legion/AGENTS.md` |
| Shared plugin interface | `packages/pi-shared/`                        | `@legion/pi-shared`: the versioned in-process interface between the two plugins and the modules both bundle. See `packages/pi-shared/AGENTS.md` |
| Worker image (Kubernetes) | `packages/daemon/docker/worker.Dockerfile`, `.github/workflows/worker-image.yaml` | See `docs/kubernetes.md` |
| Operator controller configuration (Kubernetes) | `deploy/kubernetes/daemon/controller.yaml.example` | The operator-side file `legion controller start` reads. See `docs/kubernetes.md` "Operator-launched controller" |
| Prove the Kubernetes runtime live | `scripts/e2e/stage4b-sandbox-tree.sh` (a full tree), `scripts/e2e/stage4a-sandbox-runtime.sh` (the runtime alone) | The Go daemon on Agent Sandbox in the production cluster; `scripts/e2e/README.md` |
| Native Dispatch workspace | `packages/dispatch/`, `packages/envoy/cmd/dispatch/` | React SPA and native Dispatch server; `bun run dispatch:local` runs it here, seeded and signed in |
| Dispatch's document editor | `packages/proof-editor/` | The editor entry, typed blocks and block ids, source-only. Copied from the `sjawhar/proof-sdk` fork at the commit a git dependency pins; the upstream editor modules stay there. See `packages/proof-editor/AGENTS.md` |
| Legion daemon | `packages/daemon/` | The Go module the root `go.work` binds, with the worker image's Dockerfile under `docker/`. `cmd/legion` is its CLI, `internal/api/state.go` owns its wire shape, `packages/contracts/src/legion-api.ts` mirrors it, `scripts/e2e/` holds each stage's live proof. The binary embeds its role prompts (`internal/prompts/roles/`: a phase worker composes `core/<role>.md`, `mechanics/headless.md` and its role's residue, the merger the headless fragment and its residue; the root architect, controller and sub-architect prompts are single files; `roles_test.go` holds their structural rules) and its own parts (`internal/prompts/go/`), and snapshots both below its state directory before any pane or controller starts. A pane's one `--append-system-prompt` value is those parts, then its addressing fragment, then the deployment's `instructions` when `legion.yaml` sets them. |
| Secrets broker | `packages/envoy/cmd/broker`, `packages/envoy/internal/broker` | See `packages/envoy/AGENTS.md` |
| Envoy Pulumi components | `packages/envoy/infra/` | The `envoy-infra` Pulumi plugin and the `envoy-infra` command (`seed`, `render`), a Go module outside the root `go.work`. Operators: `packages/envoy/infra/README.md`; changing it: `packages/envoy/infra/AGENTS.md` |
| Documentation site | `docs/site/` | Astro Starlight, published to GitHub Pages by `.github/workflows/docs.yaml`. See `docs/site/README.md`; generated reference pages come from `docs/site/generators/` |

## Conventions

- **Strict mode** — `strict: true` in tsconfig
- **Biome** — double quotes, semicolons, ES5 trailing commas, 100 char width
- **Imports** — `node:` prefix for builtins, `type` keyword for type-only imports
- **Interfaces** for object shapes, **types** for unions/aliases
- **No barrel files** — direct imports between modules (intentional, avoids circular deps)
- **Dependency injection** — the daemon's parts take their collaborators as interfaces and function fields (a `record.Store`, a `Clock`, a runtime `Options` struct's `Executable`), so a test passes fakes
- **Tests** — co-located `__tests__/` dirs, Bun test runner (`bun:test`)
- **Release jobs keep credentials away from dependency code** — in `release.yaml`, `release-envoy-listener.yaml` and `release-envoy-infra.yaml`, a job that holds `contents: write`, `id-token: write`, `attestations: write` or the deploy key runs no dependency code (no `bun`, `go`, `node`, no built binary, no `scripts/` program) and restores no cache (`cache-mode: none`); every checkout sets `persist-credentials: false`; the deploy key reaches only the `env:` of the steps that run `release-push.sh push-tag` or `push-main`; every version is one `release-push.sh version` call in the build job, which ends with the tripwire `release-push.sh no-credential`; a job whose `if:` calls `always()` tests `needs.<job>.result` for every job whose outputs it reads, and a job whose `if:` calls no `always()` has no upstream job that carries an `if:` of its own (GitHub's implicit `success()` would skip it with that job, whatever its condition says). `.github/scripts/check-release-credentials.sh` fails the build otherwise
- **Point to other guides by plain path** — write another `AGENTS.md` as `` `packages/x/AGENTS.md` ``, never `@packages/x/AGENTS.md`: Oh My Pi and Claude Code read an `@path` outside code as an include and paste the whole file into every session started in this repository, Legion's own phase workers included
- **This repository is public** — no name from the private infrastructure it deploys into goes in the tree or a commit message: no hostname or tailnet machine name, account id, IAM role, admission-policy name, token audience, secret-store id, bucket or alert channel. A test or example uses a placeholder (`<name>.internal.example` hosts, `<tailnet>.ts.net`, `<name>.<tailnet>.ts.net`, or `example-tailnet.ts.net` tailnet hosts, `example-host-<name>` machines, `example/<service>/<name>` secret ids, `<placeholder>` in a shell example); a value a live script needs is a required operator input the script refuses to start without, never a literal. Legion's own names (the `legion` namespace, `legion-worker`, `legion-daemon`, `LEGSMOKE`), its own issue keys (`LEGION-<n>`) and repository paths are not in this class. The private repository Legion is deployed from and the company that runs it are named nowhere either — not the repository, its Dispatch project key or issue keys, the company or its hostnames, and not in a file or directory name, a pull request title, body, branch name, commit message, comment or review, or a release note: write "the deployment repository" or `<deployment repo>`, and a neutral project key such as `ACME` in a fixture or example. `.github/scripts/check-private-names.sh` fails the build on any text file or path that names either, and on any text that names a private internal host or a Tailscale MagicDNS host, and `docs.yaml` runs it over the built site too. `.github/scripts/check-pr-text.sh` runs it over a pull request's title, body, branch name, commits (each one's message, author and committer), issue comments, reviews and review comments, in the required lint job at every push and in `pr-title.yaml` when the pull request is edited or a comment, review or review comment is posted, edited or deleted: with this repository's squash settings a commit subject or the title becomes main's subject and the commit messages its body, the generated release notes list merged titles, and a comment is public on the pull request's page once posted. Only a comment from a trusted author counts: one posted as a Bot (an App installed on this repository, such as Legion's role Apps, or github-actions) or by the repository's owner, a member of its organization or a collaborator. Anyone else's is skipped, with a `skipped <kind>:<id>` line naming its author and not its text, because anyone signed in can comment on a public pull request and the check is required. It names a comment it refuses by kind and id (`comment:<id>`, `review:<id>`, `review-comment:<id>`), never its text; delete that comment, since an edit leaves the old text in its edit history

## Issue Lifecycle

```
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Testing ──► Needs Review ──► Retro ──► Done
         │                  ^           ^            ^                             │
         │                  │           │            │                             │
         ├──────────────────┘           │            └─────────────────────────────┘
         │   (already spec-ready)       │            (changes requested)
         └──────────────────────────────┘
                    (urgent + clear)
```

**Phase roles:** architect → plan → implement → test → review → merge
**Retro:** runs after the reviewer approves the head and before the merger completes with its `READY` packet.
**Production check:** after the merge lands, the implementer — the agent that developed the change — drives it in production and records that on the pull request and the issue; the architect signs off only then.

Statuses above are native Dispatch issue statuses, not GitHub labels — the daemon writes
`in_progress`/`testing`/`needs_review`/`retro` through its outbox (a `dispatch_status` row it delivers to Dispatch) for the issues it
runs, and a human may set any lifecycle status from the Dispatch dashboard. The controller moves an
issue to `todo`/`backlog`/`icebox` through its `legion` tool's `set_status`, and an operator through
`legion status <issue> <status> --operator-token-file <file>`. A human can also
close an issue into `done` and reopen it into `backlog` from the Dispatch dashboard.

**Handover:** the daemon (`packages/daemon`) works only the issues handed to it with the
Dispatch label `legion` (in any case), so it can share a Dispatch project with humans and other
agents; a dedicated project hands its issues over the same way. A human sets the label from the
issue header in the Dispatch dashboard, an agent with `dispatch issue` or `dispatch issue-update`.
The controller hands over work itself: whenever an admission slot is free, it labels and admits
the highest-priority `todo` leaf issue (one with no children) nobody else is working
(`skills/legion-controller/SKILL.md`, "Keeping the slots full", which names when it walks).
A root in `todo` without the label is never admitted, and a root in `triage` without it never wakes
the controller. A child needs none: it runs under its tree's architect once its root is admitted,
and its architect's `release_children` records it in the tree even when Dispatch already showed it
`todo`. A child whose tree is not live is an orphan, admitted as a root of its own, so it needs the
label as any root does. A root that stays `done` through its linger hands every child still in the
workflow to Legion as a root of its own: the daemon sets it `todo` with the label in one Dispatch
write, and the write's event carries the child's phase, design gate, phase records and pull request
into a new generation, then starts that phase's role. A child whose old root had no gate (cleared
by a reopen in its linger) carries none: its architect registers its own spec while the child is
before `awaiting_merge` (after that its READY is already through), and decomposes nothing anew,
since its workers already run their phases. A role of a carried child still in a turn under its old
tree stops first: nothing of the new tree launches until that turn ends, or is stopped at the old
root's linger expiry, or at the stop timeout once that root no longer lingers, and the daemon
suspends the role, keeping its session, which the new tree then resumes. A child stays its own tree
after hand-over; reopening the old root later does not take it back. A root reopened inside its
linger instead keeps its children. Taking the label off a waiting root drops it from the waiting
line, as a status that leaves `todo` does; taking it off a root already admitted does not stop its
tree. The mark is a label rather than the issue's `route` because Dispatch publishes every event of
a routed issue to the route's topic, where a label only marks the issue; it carries no workflow
state.

**Gate:** the design gate, when armed (`gates.design: root-issues` in `legion.yaml`, the default),
is a human approving the root issue's spec document at a version in Dispatch: once the spec's
decision blocks are settled and the human has agreed to every point in it, the architect requests
it with `dispatch request-approval` and a summary of what the human is approving, and registers
the document id and version with the daemon, and the daemon opens the gate on the
`artifact.approved` event for that document at its current version — or at registration itself,
when Dispatch already shows the human approved that version before the architect registered (the
daemon reads the approval; it never writes one). A later spec version closes the gate until
someone approves the new version, which Dispatch does in the human's own name when the version is
his answer to a decision block and that answer is the whole change since the version he approved
(it records an `approved` review pinned to the new version and emits `artifact.approved`, and the
gate reopens on that event), and a `changes_requested` review closes it with the reviewer's
reason. `gates.design: off` is the only way past the gate without a human review — Legion has no
operator approve command; with `off` the root architect is told so in its system prompt and adds
no approval step. Whether a human must approve a pull request before it merges is the repository's
own branch-protection or CODEOWNERS rule: Legion neither reads nor writes it. Only retro's commits
may sit above the reviewer's approved head, those that change `docs/solutions/` and its removal of
`.legion/<issue>/`: READY compares the review round's recorded approved head with READY's head on
GitHub, as the implement App, and refuses anything else above it (`READY_UNREVIEWED_CHANGE`, naming
the paths; also a compare GitHub may have cut at its 300 files or that gives no file list, a head
not built on the approved one, or an approved head GitHub does not know) and a round with no
recorded approval (`READY_NOT_APPROVED`), so the change goes back to
review. The merger sends its
`READY` packet with its completion; the daemon posts it on the Dispatch issue and, when the project's
`projects.<KEY>.merge_queue_role` names one, publishes it to that role. A project may configure
`projects.<KEY>.merge.by_hand` regular-expression paths: a matching pull request completes READY
without auto-arming, the daemon tells that READY with its `MANUAL_MERGE_REQUIRED:` note at the head
of the packet and the packet's `Merge:` line replaced, so neither the issue nor that role is asked to
arm it, and the repository's authorized person merges it. A by-hand READY refuses
`READY_DISARM_PENDING` while an earlier auto-merge disarm for that pull request remains unfinished:
the stale arm must be gone before the person-only head is accepted. Ordinary READY does not wait for
that row. Otherwise, on acceptance the daemon queues a
`ready_merge` outbox row for READY's head, which submits the merge as the implement App with that head
as GitHub's expected head: a merge queue enqueues it once GitHub reports it mergeable, a pull request
GitHub reports mergeable merges at once, a repository that allows auto-merge arms it, and in one that
allows neither the row waits until GitHub reports the pull request mergeable and then merges it. On a
merge-queue base the row never arms auto-merge: GitHub's enable arms a pull request it does not
report mergeable rather than enqueueing it, and a required check may judge the arm, so the row waits,
logged at debug, while a required check still runs. An arm or queue entry is not the end: the row
keeps reading GitHub until it reports the pull request merged at READY's head, and once it has seen
the arm or entry it never submits again after a removal it did not make. The merge queue taking the
pull request out because the merge failed (GitHub's reason `failed_checks`, or an unexplained reason
with a check that failed, timed out, failed to start, needed action or went stale on the commit the
queue built for its `merge_group` CI, every page of its checks read) sends the issue from `awaiting_merge` back to
`implementing`, its implementer told the failing run, as a red head does; any other removal (a person
dequeuing it, whose cancelled runs are no failure, or a person or GitHub turning auto-merge off) ends
the row and tells the architect once (`merge-removed`). Two removals are submitted again: the
implement App's own disarm sent before READY that GitHub applied late (auto-merge
`manually_disabled`, or a `manual` dequeue, by that App), and auto-merge a close turned off
(`closed`), which the reopen at READY's head undid. The repository's required reviews and checks
decide when it merges; Legion bypasses
none, and a review only a person can give is waited for. READY accepts a head whose required checks
are still running, and refuses one whose required check failed or never reported. The row reads
GitHub, then the record, and submits nothing once the record shows the issue has left that READY or
applied a close; while the record still awaits it, a pull request GitHub shows closed or at another
head waits for the two to agree. The bound is GitHub's own clock: once GitHub's last change to the
pull request (its `updatedAt`) is more than 10 minutes old, a webhook never arrived, so the row ends
and tells the architect once (`merge-stalled`); routine waits and rate-limit deferrals never count
toward it. A reopen at READY's head reuses that row, starts it over as though nothing had been armed,
and takes it from any worker still holding it, so the merge is never lost. A refusal GitHub would
repeat on a retry ends the row and is told to the architect once (`merge-refused`).
GitHub checks the expected head only when auto-merge is enabled, and arming is right only for an
awaiting issue's READY head, so the daemon queues an `auto_merge_disable` outbox row on every other
push or reopen, when a child is set back to
`todo`, and when an issue leaves the workflow. The row reads GitHub, then the record: it keeps the arm
only while the issue awaits its merge, its READY is not by hand, and the pull request's head is the
merger's READY commit, and otherwise disables auto-merge and removes the merge-queue entry, as the
implement App; READY is refused (`READY_HEAD_MOVED`) when the head moved past its commit. When a push
or reopen moves the head past
the one READY named, the head's own CI turns red before the merge, the head starts conflicting
with its base (GitHub computes no merge ref for a conflicting head and runs no checks on it at all),
or the merge queue fails its merge, the daemon sends the issue back to
`implementing`, disarms the pull request, posts the READY's withdrawal on the Dispatch issue, and
publishes it to that role. GitHub's own merge queue (`merge_group`
trigger) is a repository setting Legion does not modify. No lifecycle labels exist; GitHub issues
are never read or written by Legion.

**Review signaling:** Native GitHub review API, tester status checks, and committed handoffs are
the phase-verdict artifacts. No lifecycle labels carry worker state.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md` — human-authored design history, not a Legion artifact. A Legion planner's plan lives in `.legion/<issue>/plan.json` and the issue's `plan.md` document on Dispatch; no Legion role commits a plan or spec file here.
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
