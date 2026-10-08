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
  Postgres, admission, process supervision under either runtime, credential grants, and recovery.
- **OMP extensions** — two Oh My Pi plugins: `@sjawhar/pi-envoy` (`packages/pi-envoy`), which
  every session loads for Envoy messaging and the native Dispatch tools, and `@sjawhar/pi-legion`
  (`packages/pi-legion`), which a Legion pane loads beside it and which injects the Legion tool,
  the daemon handshake and the credential grants into that session, reaching the Envoy plugin
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

## Commands

```bash
bun install                   # Setup
bunx biome check <package>/   # Lint (the root pins @biomejs/biome so bunx resolves the real Biome; CI's required `lint` job runs `bunx biome check .`, and a package's `bun run lint` checks only the paths its recipe names)
bunx tsc --noEmit             # Type check
bun test                      # Test
```

```bash
legion start [--config <legion.yaml>] [--check-config]  # Run the daemon in the foreground for the project that file names (./legion.yaml by default)
legion status <team>                 # Check daemon status
legion status <issue> <status>       # Set an issue's Dispatch lifecycle status (todo|backlog|icebox); controller-capability
legion stop [--config <legion.yaml>]  # Stop the registered daemon of the project that file names
legion restart <team>                # Restart daemon, preserve worker sessions
legion legions                       # List registered Legion daemons
legion gh -- <args>                  # Run gh with a session-bound GitHub token; every merge-shaped invocation is refused for every role (Legion never merges)
legion threads resolve --pr <n> --repo <owner>/<repo> [--gh]  # Implementer after every push that answers a review, before its completion, merger before READY: resolves each unresolved review thread whose newest comment is its opener's submitted `Accepted:` reply, and each thread a bot account opened that is none of Legion's role Apps (the logins the daemon names, keyed by App role, on the grant's gh-token answer; a thread either Legion App opened still needs its opener's `Accepted:`) whose newest submitted comment is the Legion review App's `Accepted:`, since GitHub cannot tell a CI bot from an App-routed person and the reviewer adjudicates the finding; the pull request author's reply closes nothing, and each `resolved <url>` line says whose acceptance closed the thread; with `--gh`, which has no grant and so no logins, no thread counts as a bot's (GitHub grants resolving to the pull request's author, and the implementer opens it); a draft in a pending review, which GitHub shows only to its author, never counts; exits 1 naming a thread GitHub refuses. In a Legion pane it authenticates with the pane's grant file, and `--gh` there is refused (the pane's `gh` is `legion gh`); a session outside a Legion pane (no grant) adds `--gh`, which applies the same rule through its own `gh` with `GH_REPO` set to `--repo`, so it runs from any directory (the `legion` release installed outside panes), and shows gh's stderr on success too, where the devbox shim names an inherited `GH_TOKEN` or a fallback personal token the call then acts as. In the reviewer's pane (`LEGION_ROLE=reviewer`), whose review App GitHub refuses a resolve on the implementer's pull request, it asks the daemon instead (`POST /legion/v1/threads/resolve`, with the grant and the pull request): the daemon refuses every other role's grant and any pull request but the one recorded for the grant's issue, resolves as the implement App only the bot threads whose newest submitted comment is the review App's `Accepted:` (never a thread a Legion App opened), logs each resolution, and answers each thread's outcome, how many threads it left open unnamed because their newest comment is a draft in the implement App's pending review (which GitHub shows the daemon and not the reviewer), and the thread GitHub refused with its message when one stopped the run. The command prints the outcomes as the same lines, then that number when it is not zero, then the refusal, and exits 1 on a refusal or on any thread left open behind the implement App's pending draft, which closes only once that review is submitted or discarded; the implement App's token never leaves the daemon. The reviewer runs it when a review workflow its project declares (`projects.<KEY>.review_workflows`) is red on a bot's findings, before re-running the failed run. Outside a pane the reviewer and the implementer can post as one account (the devbox `gh` routes every session to the repository owner's App), where the opener check cannot tell an implementer's `Accepted:` from the reviewer's, so only the reviewer ever writes `Accepted:`
legion credential                    # Git credential helper for Legion grants
legion push                          # The issue-branch push of @- to legion/<LEGION_ISSUE> every worker runs (the ancestry check against the remote branch or the recorded rewritten tip, the bookmark, the push); a fast-forward that changes nothing but handoffs whose phase guarantees a later push (the planner's plan.json, the tester's test.json, a reviewer's review.json with verdict changes_requested) ends @-'s message with GitHub's `skip-checks: true` trailer, so it starts no CI and the daemon carries the code head's verdict to it; every other push, which can leave the head a human merges, runs in full; the permitted paths are read from the push's commits rather than its net tree diff, so no commit of a skipping push may touch a path outside those three even where a later commit undoes it, since the daemon classifies the push from the union of its commits' paths and would carry no verdict to that head; a head whose message carries one of GitHub's bracket keywords (`[skip ci]` and its four siblings), which start no workflow run wherever they stand in the message, is refused, since whether a push skips is `legion push`'s decision; GitHub honours the trailer only as the last line, so the describe runs with jj's `templates.commit_trailers` empty and the message is read back, and a message that does not end as the rule says is refused (exit 1, naming its last line) before anything is pushed
legion state                         # Read daemon state
legion handoff write|read            # Write/read structured handoff data on the issue branch; workers run them through the `legion` tool's `handoff_write` and `handoff_read`; `write` takes its JSON object from `--data` or, when that is omitted, from stdin, so no handoff hits the 128 KiB cap on one argv string
legion handoff complete --summary <text>  # Report phase completion to the tree's architect, keeping the role claimed; `--ready` (the merger's) first refuses unless every check the base branch requires (rulesets and branch protection) has succeeded on the pull request's head on GitHub, and every workflow its rulesets require (a `workflows` rule) has a pull-request run for that head whose latest succeeded, naming the head and the check or workflow; workers call it through the `legion` tool's `handoff_complete`, never from bash, where the pi-legion extension refuses it (authenticates exactly like `legion gh`/`legion credential`: reads the grant from LEGION_GRANT_FILE — the 0600 file the daemon names on the pane and the pi-legion extension writes before each bash command, each `handoff_complete`, and each tool call Oh My Pi serves with gh; LEGION_GRANT is the manual fallback — and redeems it for this worker's issue/role/session, never a live session secret in the request)
legion worker-shim --connect <unix:///path|tcp://host:port> --boot-token-file <path> [--provider-env-dir <dir>] [--pod-safety] [--warm-codegraph] [--agent-secrets-key-dir <dir> --pod-token-file <path> --agent-secrets-bin <path>] -- <omp argv…>  # Bridges a headless OMP process (a root architect, a phase worker, or the controller the daemon launches under `controller: daemon`) to the daemon: the shim dials the daemon's worker stream listener (`unix://` from a tmux pane, `tcp://` from a pod) and authenticates with its boot token; --provider-env-dir exports each mounted secret file as NAME=contents into the OMP child's environment only (skipping a NAME the pod already consumes through a NAME_FILE pointer, e.g. DISPATCH_TOKEN; and refusing to start — exit 1 naming the key and its file, Oh My Pi never spawned — when a key's name is already a variable of the shim's own environment, since the export would override it silently (LEGION-186)); the shim also answers the daemon's `adopt-working-copy` frame by running the shared `jj metaedit --update-author` in its workspace; with `--pod-safety` (the Sandbox runtime passes it; a tmux pane never does), the shim starts Oh My Pi on Legion's pod baseline (`packages/daemon/internal/podsafety`), which names no model, provider or route and holds nothing of a repository's settings off: the turn-scoping overlay (`bash.autoBackground` and `async` off, the two keys a takeover's abort depends on) written to `LEGION_STATE_DIR` and named first in `PI_CONFIG_FILES`, so the operator's overlay outranks it and both outrank a repository's `.omp/config.yml`, and `PI_CONFIG_DIR=.omp` and `OMP_SESSION_STORAGE=file` each set only when the pod leaves it unset (the runtime refuses both in an operator's pod, since they place the sessions a resume reads); with `--warm-codegraph` (the Sandbox runtime passes it for a role in an issue pod; never for a tmux pane, whose workspace the daemon warms itself, nor for the controller, which has no workspace; refused without `LEGION_WORKSPACE`), the shim builds the CodeGraph index of the workspace `LEGION_WORKSPACE` names in the background once Oh My Pi has written its first frame, under a cross-process lease at `.codegraph/legion-warm.lock` so the six role shims sharing an issue's workspace never build it twice, and a stop mid-build ends the warm-up with Oh My Pi (the shim exits only once the lease is released, so the relaunch never reads a live build); a pod's model route is the operator's (`runtime.kubernetes.pod` and `provider_keys`, docs/kubernetes.md "Operator configuration"); the Sandbox runtime's three flags for a workflow role's process in a pod enrolled with the secrets broker (runtime.kubernetes.agent_secrets): the shim runs agent-secrets keygen there before its hello, sends the key's thumbprint and the projected token in hello2, keeps the enrollment id the daemon hands back, and runs agent-secrets renew; a tmux pane and the controller's process, which holds no human-tier key, never get them (daemon-spawned under either runtime, by a tmux pane's command or by a pod launcher's start command, never run by hand)
legion launcher --connect tcp://<host>:<port> --token-file <path> --sandbox <name> --role <role> [--pod-uid <uid>] --private-dir <dir>  # PID 1 of one role container in an issue pod, or of the one `controller` container of the project controller's own pod under `controller: daemon` (Kubernetes runtime; daemon-spawned): authenticates to the worker stream with the role's own launcher token, bound to the pod's uid (POD_UID by default), reports the child it runs after every connection, and starts or stops that role's `legion worker-shim` process group on the daemon's command; each generation's boot token and launch credentials arrive in the start command and are written owner-only and exclusively into a fresh `g<generation>` directory of --private-dir (anything already there removed without being followed) and removed when the generation ends; it survives a daemon restart without stopping the child, refuses a generation that already ran or a reused request id with another payload, and never restarts a child itself
legion controller start --config <controller.yaml> [--daemon-url <url>]  # either runtime — the operator starts the interactive controller on their own machine: reads the strict operator-side file (deploy/kubernetes/daemon/controller.yaml.example), refuses an operator token file others can read, fetches the controller secret from POST /legion/v1/controller/secret with that token as a bearer, launches Oh My Pi in the foreground with the shared controller environment, exits with its code
cd packages/daemon && go build ./cmd/legion   # The Legion daemon and CLI (LEGION-208): version|start|stop|state|legions|status|restart|worker-shim|model-token|claims|gh|credential|handoff|threads|push|probe-image|workspace-init|controller, its own $XDG_STATE_HOME/legion/legions.json registry, its own Postgres schema; its `workspace-init` is an issue pod's two init containers — `workspace-init fetch --repo <owner>/<repo> --feed <dir>` clones from GitHub into the pod's feed and is the only process holding the provisioning token (LEGION_PROVISION_TOKEN_FILE), and `workspace-init provision --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper> --feed <dir>` builds the shared clone and jj workspace on the tree volume from that feed without the token, under a per-repository flock held for the process lifetime (contended wait bound: LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS, set by the daemon from its boot deadline; 900 s when unset), and when LEGION_EXPECT_TREE_VOLUME is true (the launching claim resumes a session, or any stored claim of the tree already retained one) exits non-zero if the volume holds neither the shared clone nor any retained session (a launch failure, never a fresh agent) (docs/kubernetes.md "Trust model: the provisioning token") — and the daemon-launched controller pod's one init container, `workspace-init controller --root /legion`, which makes its sessions directory and, on a resume whose LEGION_RESUME_SESSION_FILE is missing, exits 3 for a fresh controller (docs/kubernetes.md "Daemon-launched controller"); `legion controller start` resolves `omp_invocation` as the daemon does, with no pinned default, and refuses before its one daemon call, by launching the operator's Oh My Pi as the controller will run, a pi-legion it does not load, or loads speaking another daemon API contract, or loads without pi-envoy or with a pi-envoy at another plugin interface version, or beside the pre-split package; that call names the contract, and the daemon refuses another with 409, naming both, before it mints a capability, so a `legion` and a daemon of different releases never revoke the running controller; and its controller registers on `/legion/v1/claims/register`, which refuses another contract naming both; `legion start --check-config` validates a file without running its key commands; `legion status <issue> <status> --operator-token-file <file> [--config|--port]` sets a status from an operator shell over the operator bearer, whose file `legion status` and `legion claims` refuse when its group or others can read it, as `controller start` does
bash scripts/e2e/stage1-skeleton.sh    # Stage 1's live proof: the Go daemon boots on a real Postgres, serves GET /legion/v1/state, restarts against the same store, and refuses an unreachable Postgres by host (scripts/e2e/README.md)
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic bash scripts/e2e/stage2-tmux-supervision.sh   # Stage 2's live proof, devbox only: real Oh My Pi panes with the branch's two plugins (pi-envoy and pi-legion) under the Go daemon's private tmux server, against a real Envoy listener and NATS — register, role claim, delivery once, resume, suspend, the registration deadline, re-adoption across a restart, the orphan sweep, the gate's refusal of pi-legion without pi-envoy (scripts/e2e/README.md)
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic SMOKE_UPSTREAM_NATS=nats://envoy-nats.<tailnet>.ts.net:4222 bash scripts/e2e/stage3-devbox-workflow.sh      # Stage 3's devbox-only live proof: real OMP panes drive a scratch Dispatch issue through the Go workflow, the design gate, review rounds, ordinary human merge, production check, restart, held worker, pending status write, and pane credentials against sjawhar/legion-smoke (scripts/e2e/README.md)
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> bash scripts/e2e/stage4a-sandbox-runtime.sh   # Stage 4a's live proof, devbox only: the Go Agent Sandbox runtime on the production cluster (namespace legion) through the Legion daemon's restricted identity, on the operator fixture's pod and a providers Secret of its own — install check, the image probe with the daemon's own command (and its refusal once the operator's overlay drops a role an agent names), gVisor, the operator's ServiceAccount and token, the pod baseline, the provider key reaching the agent alone, affinity, suspend/resume, same-agent refusal, kill, relaunch before registration, concurrent provisioning, re-adoption, the orphan sweep, release, and a namespace left as it was (scripts/e2e/README.md)
LEGION_E2E_RUNTIME_CONTEXT=<restricted context> LEGION_E2E_IMAGE=<worker image@sha256> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic LEGION_E2E_MODEL_GATEWAY_AUDIENCE=<gateway audience> LEGION_E2E_DISPATCH_URL=<dispatch> LEGION_E2E_ENVOY_URL=<listener> LEGION_E2E_NATS_URL=nats://<nats>:4222 LEGION_E2E_DISPATCH_TOKEN_FILE=<0600 file> LEGION_E2E_ENVOY_TOKEN_SECRET_ID=<secret id> bash scripts/e2e/stage4b-sandbox-tree.sh   # Stage 4b's live proof, devbox only: the Go daemon drives three real issue trees on the Agent Sandbox runtime in the production cluster (namespace legion), against production Dispatch (project LEGSMOKE), Envoy and NATS, with real agents — admission under the cap, specs, the daemon bound to every interface with every pod dialing its advertise_host, tree separation across nodes, the repository-configuration fixture, the whole workflow to done with the review pair and a review round no review decides until the architect asks, then a changes-requested round whose approval does not wait for its thread to resolve, and a READY with every thread resolved, CI going red mid-test interrupting the tester's turn before the implementer starts, every worker's phase completion answered in the live session it keeps in its first pod until its issue closes, a finished role answering while another phase runs, token rotation, kill and fence, a daemon restart mid-tree relaunching nothing, a restart with only `envoy_url` respelled relaunching every role in its own issue pod and one with the worker stream and API moved replacing every issue pod, the operator's controller, a worker that dies with its task outstanding, the close suspending every role, node release, linger close, re-admission, every pod's shape, the pod watch, and an audit that the run wrote nothing outside LEGSMOKE; one run at a time, STAGE4B_UNTIL=<checkpoint> for a development run (scripts/e2e/README.md)
```

## Configuration

```yaml
projects:
  LEGION: { repo: sjawhar/legion }
  WIDGETS: { repo: acme/widgets, merge_queue_role: merge-queue, review_workflows: [.github/workflows/review.yml] }
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
| Handoff ledger         | `.legion/` on issue branch                     | Committed phase output                          |
| Envoy event routing    | `packages/envoy/`                              | See `packages/envoy/AGENTS.md`                  |
| Shared event contracts | `packages/contracts/`                          | See `packages/contracts/AGENTS.md`               |
| Envoy OMP adapter      | `packages/pi-envoy/`                          | `@sjawhar/pi-envoy`: the Envoy and Dispatch tools every session loads. See `packages/pi-envoy/AGENTS.md`          |
| Legion OMP plugin      | `packages/pi-legion/`                         | `@sjawhar/pi-legion`: the Legion entry, its daemon client, grants and `legion` tool, the task agents and the Legion skills. See `packages/pi-legion/AGENTS.md` |
| Shared plugin interface | `packages/pi-shared/`                        | `@legion/pi-shared`: the versioned in-process interface between the two plugins and the modules both bundle. See `packages/pi-shared/AGENTS.md` |
| Worker image (Kubernetes) | `packages/daemon/docker/worker.Dockerfile`, `.github/workflows/worker-image.yaml` | See `docs/kubernetes.md` |
| Operator controller configuration (Kubernetes) | `deploy/kubernetes/daemon/controller.yaml.example` | The operator-side file `legion controller start` reads. See `docs/kubernetes.md` "Operator-launched controller" |
| Prove the Kubernetes runtime live | `scripts/e2e/stage4b-sandbox-tree.sh` (a full tree), `scripts/e2e/stage4a-sandbox-runtime.sh` (the runtime alone) | The Go daemon on Agent Sandbox in the production cluster; `scripts/e2e/README.md` |
| Native Dispatch workspace | `packages/dispatch/`, `packages/envoy/cmd/dispatch/` | React SPA and native Dispatch server |
| Dispatch's document editor | `packages/proof-editor/` | The editor entry, typed blocks and block ids, source-only. Copied from the `sjawhar/proof-sdk` fork at the commit a git dependency pins; the upstream editor modules stay there. See `packages/proof-editor/AGENTS.md` |
| Legion daemon | `packages/daemon/` | The Go module the root `go.work` binds, with the worker image's Dockerfile under `docker/`. `cmd/legion` is its CLI, `internal/api/state.go` owns its wire shape, `packages/contracts/src/legion-api.ts` mirrors it, `scripts/e2e/` holds each stage's live proof. The binary embeds its role prompts (`internal/prompts/roles/`: a phase worker composes `core/<role>.md`, `mechanics/headless.md` and its role's residue, the merger the headless fragment and its residue; the root architect, controller and sub-architect prompts are single files; `roles_test.go` holds their structural rules) and its own parts (`internal/prompts/go/`), and snapshots both below its state directory before any pane or controller starts. A pane's one `--append-system-prompt` value is those parts, then its addressing fragment, then the deployment's `instructions` when `legion.yaml` sets them. |
| Secrets broker | `packages/envoy/cmd/broker`, `packages/envoy/internal/broker` | See `packages/envoy/AGENTS.md` |
| Documentation site | `docs/site/` | Astro Starlight, published to GitHub Pages by `.github/workflows/docs.yaml`. See `docs/site/README.md`; generated reference pages come from `docs/site/generators/` |

## Conventions

- **Strict mode** — `strict: true` in tsconfig
- **Biome** — double quotes, semicolons, ES5 trailing commas, 100 char width
- **Imports** — `node:` prefix for builtins, `type` keyword for type-only imports
- **Interfaces** for object shapes, **types** for unions/aliases
- **No barrel files** — direct imports between modules (intentional, avoids circular deps)
- **Dependency injection** — the daemon's parts take their collaborators as interfaces and function fields (a `record.Store`, a `Clock`, a runtime `Options` struct's `Executable`), so a test passes fakes
- **Tests** — co-located `__tests__/` dirs, Bun test runner (`bun:test`)
- **Point to other guides by plain path** — write another `AGENTS.md` as `` `packages/x/AGENTS.md` ``, never `@packages/x/AGENTS.md`: Oh My Pi and Claude Code read an `@path` outside code as an include and paste the whole file into every session started in this repository, Legion's own phase workers included
- **This repository is public** — no name from the private infrastructure it deploys into goes in the tree or a commit message: no hostname or tailnet machine name, account id, IAM role, admission-policy name, token audience, secret-store id, bucket or alert channel. A test or example uses a placeholder (`<name>.internal.example` hosts, `example-host-<name>` machines, `example/<service>/<name>` secret ids, `<placeholder>` in a shell example); a value a live script needs is a required operator input the script refuses to start without, never a literal. Legion's own names (the `legion` namespace, `legion-worker`, `legion-daemon`, `LEGSMOKE`), its own issue keys (`LEGION-<n>`) and repository paths are not in this class. The private repository Legion is deployed from and the company that runs it are named nowhere either — not the repository, its Dispatch project key or issue keys, the company or its hostnames, and not in a file or directory name, a pull request title, body, branch name or commit message, or a release note: write "the deployment repository" or `<deployment repo>`, and a neutral project key such as `ACME` in a fixture or example. `.github/scripts/check-private-names.sh` fails the build on any text file or path that names either or a private internal host, and `docs.yaml` runs it over the built site too. `.github/scripts/check-pr-text.sh` runs it over a pull request's title, body, branch name and commits (each one's message, author and committer), in the required lint job at every push and in `pr-title.yaml` when the pull request is edited: with this repository's squash settings a commit subject or the title becomes main's subject and the commit messages its body, and the generated release notes list merged titles

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
runs, and a human may set any lifecycle status from the Dispatch dashboard. `legion status <issue>
<status>` remains the controller path for `triage`/`icebox`/`backlog`/`todo`. A human can also
close an issue into `done` and reopen it into `backlog` from the Dispatch dashboard.

**Handover:** the daemon (`packages/daemon`) works only the issues handed to it with the
Dispatch label `legion` (in any case), so it can share a Dispatch project with humans and other
agents; a dedicated project hands its issues over the same way. A human sets the label from the
issue header in the Dispatch dashboard, an agent with `dispatch_issue` or `dispatch_issue_update`.
The controller hands over work itself: whenever an admission slot is free, it labels and admits
the highest-priority `todo` leaf issue (one with no children) nobody else is working
(`skills/legion-controller/SKILL.md`, "Keeping the slots full", which names when it walks).
A root in `todo` without the label is never admitted, and a root in `triage` without it never wakes
the controller. A child needs none: it runs under its tree's architect once its root is admitted. A
child whose tree is not live is an orphan, admitted as a root of its own, so it needs the label as
any root does. Taking the label off a waiting root drops it from the waiting line, as a status that
leaves `todo` does; taking it off a root already admitted does not stop its tree. The mark is a
label rather than the issue's `route` because Dispatch publishes every event of a routed issue to
the route's topic, where a label only marks the issue; it carries no workflow state.

**Gate:** the design gate, when armed (`gates.design: root-issues` in `legion.yaml`, the default),
is a human approving the root issue's spec document at a version in Dispatch: once the spec's
decision blocks are settled and the human has agreed to every point in it, the architect requests
it with `dispatch_request_approval` and a summary of what the human is approving, and registers
the document id and version with the daemon, and the daemon opens the gate on the
`artifact.approved` event for that document at its current version — or at registration itself,
when Dispatch already shows the human approved that version before the architect registered (the
daemon reads the approval; it never writes one). A later spec version closes the gate until
someone approves the new version, and a `changes_requested` review closes it with the reviewer's
reason. `gates.design: off` is the only way past the gate without a human review — Legion has no
operator approve command; with `off` the root architect is told so in its system prompt and adds
no approval step. Whether a human must
approve a pull request before it merges is the repository's own branch-protection or CODEOWNERS
rule: Legion neither reads nor writes it. The merger sends its `READY` packet with its completion;
the daemon posts it on the Dispatch issue and, when the project's `projects.<KEY>.merge_queue_role`
names one, publishes it to that role; a human merges
under the repository's code-owner rule. When the head's own CI turns red before the merge, or the
head starts conflicting with its base (GitHub computes no merge ref for a conflicting head and
runs no checks on it at all), the daemon sends the issue back to `implementing`, posts the READY's
withdrawal on the Dispatch issue, and publishes it to that role.
GitHub's own merge queue (the `merge_group` trigger) is a
repository setting Legion neither reads nor writes. No lifecycle labels exist; GitHub issues are
never read or written by Legion.

**Review signaling:** Native GitHub review API, tester status checks, and committed handoffs are
the phase-verdict artifacts. No lifecycle labels carry worker state.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md` — human-authored design history, not a Legion artifact. A Legion planner's plan lives in `.legion/<issue>/plan.json` and the issue's `plan.md` document on Dispatch; no Legion role commits a plan or spec file here.
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
