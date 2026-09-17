# Legion

Autonomous development swarm using Oh My Pi agents. Root processes own issue trees, and the Legion daemon supplies durable state, credentials, and event routing.

## Architecture

Legion's issue lifecycle lives entirely on native Dispatch (triage → done); the daemon never
creates, reads, or writes a GitHub issue. It derives role and gate state from Dispatch issue
events plus GitHub PR/CI artifacts, records root-process session locators, and publishes only the
verdict changes each role needs. Root processes run in tmux; phase workers are headless
`omp --mode rpc` processes the daemon spawns directly, one tmux pane per worker, bridged through
`legion worker-shim`. Under `runtime: kubernetes` each of those processes is instead one pod of
the published worker image in a cluster (`docs/kubernetes.md`); `scripts/kind-smoke/` is that
runtime's live proof on a throwaway kind cluster.

- **TypeScript daemon** — webhook intake, reducers, durable `LegionState`, root-process lifecycle,
  credential grants, resync, and recovery.
- **OMP extension** — injects the Legion tool and event delivery into active OMP sessions and
  provisions issue workspaces. Phase workers (planner/implementer/tester/reviewer/merger, and
  sub-architects for child issues) are headless `omp --mode rpc` processes the daemon spawns
  directly, one tmux pane per worker, bridged through `legion worker-shim`; the daemon enforces
  recursion limits when spawning them. The controller is an interactive OMP terminal session in
  the daemon's private tmux server (no `--mode rpc`, no shim); reach it with
  `tmux -L legion-<project> select-window -t <window id> \; attach -t legion-<project>`, the
  window id being `controllerLocator.tmuxWindowId` in `legion state --json` (every window opens
  detached, so a bare `attach` lands on whichever window is current). It is also the merge queue.
- **Skills** — guide the architect and sequential phase workers. Durable `.legion/<phase>.json`
  handoffs are the recovery source of truth.

## Tech Stack

- **TypeScript** on **Bun** runtime
- **citty** for CLI, **Bun.serve** for HTTP daemon
- **Oh My Pi extension** for Legion tools, role delivery, workspace provisioning, and phase workers
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, native **Dispatch** for issue tracking

## Commands

```bash
bun install                   # Setup
bunx biome check <package>/   # Lint (the root pins @biomejs/biome so bunx resolves the real Biome; each package's `bun run lint` is the CI recipe)
bunx tsc --noEmit             # Type check
bun test                      # Test
```

```bash
legion start <team> [-w path]        # Start swarm
legion status <team>                 # Check daemon status
legion status <issue> <status>       # Set an issue's Dispatch lifecycle status (todo|backlog|icebox); controller-capability
legion stop <team>                   # Stop swarm
legion restart <team>                # Restart daemon, preserve worker sessions
legion legions                       # List registered Legion daemons
legion gh -- <args>                  # Run gh with a session-bound GitHub token (refuses every GitHub-issue write — Legion issues live on Dispatch; `pr merge` is redeemed with merge intent the daemon grants only to the controller's own grant; every phase-worker grant is refused)
legion threads resolve --pr <n> --repo <owner>/<repo>  # Implementer before every push that answers a review, merger before READY: resolves each unresolved review thread whose newest comment is its opener's `Accepted:` reply (the review App cannot); exits 1 naming a thread GitHub refuses
legion credential                    # Git credential helper for Legion grants
legion state                         # Read daemon state
legion handoff write|read|message    # Workers: write/read structured handoff data on issue branch
legion handoff complete --summary <text>  # Workers: report phase completion to the tree's architect, keeping the role claimed (authenticates exactly like `legion gh`/`legion credential`: reads the grant from LEGION_GRANT_FILE — the 0600 file the daemon names on the pane and the pi-envoy extension writes before each bash command; LEGION_GRANT is the manual fallback — and redeems it for this worker's issue/role/session, never a live session secret in the request)
legion worker-shim --socket <path> -- <omp argv…>  # Bridges a headless phase-worker OMP process to the daemon over a unix socket (daemon-spawned, not run by hand)
legion worker-shim --connect tcp://<host>:<port> --boot-token-file <path> [--provider-env-dir <dir>] -- <omp argv…>  # Same bridge, reverse-dialed: the shim dials the daemon's worker stream listener and authenticates with its boot token; --provider-env-dir exports each mounted secret file as NAME=contents into the OMP child's environment only (skipping a NAME the pod already consumes through a NAME_FILE pointer, e.g. DISPATCH_TOKEN; and refusing to start — exit 1 naming the key and its file, Oh My Pi never spawned — when a key's name is already a variable of the shim's own environment, since the export would override it silently (LEGION-186)); the shim also answers the daemon's `adopt-working-copy` frame by running the shared `jj metaedit --update-author` in its workspace (Kubernetes runtime; daemon-spawned)
legion workspace-init --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper>  # Kubernetes pod init container: shared clone + jj workspace on the tree volume with the mounted repository token (LEGION_PROVISION_TOKEN_FILE), under a per-repository flock held for the process lifetime (contended wait bound: LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS, set by the daemon from its boot deadline; 900 s when unset); when the pod resumes a session, exits non-zero if LEGION_RESUME_SESSION_FILE is missing from the volume (a launch failure, never a fresh agent)
legion controller start --config <controller.yaml> [--daemon-url <url>]  # runtime: kubernetes — the operator starts the interactive controller on their own machine: reads the strict operator-side file (deploy/kubernetes/daemon/controller.yaml.example), refuses an operator token file others can read, fetches the controller secret from POST /legion/v1/controller/secret with that token as a bearer, launches Oh My Pi in the foreground with the shared controller environment, exits with its code
bash scripts/kind-smoke/up.sh          # Throwaway kind instance: own cluster, NATS, Postgres, Envoy listener, scratch Dispatch; SMOKE_WORKER_IMAGE=<digest> required (scripts/kind-smoke/README.md)
bash scripts/kind-smoke/checkpoints.sh <name>   # admitted | architect-pod | spec-posted | tree-moved | kill-pod-resume | pod-hygiene | worker-cap | done
bash scripts/kind-smoke/down.sh        # Tears down exactly what up.sh recorded for the instance
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
| Add CLI command        | `packages/daemon/src/cli/index.ts`            | citty `defineCommand` pattern                  |
| Change Legion API      | `packages/daemon/src/daemon/api.ts`            | See @packages/daemon/src/daemon/AGENTS.md      |
| Change daemon state    | `packages/daemon/src/daemon/legion-state.ts`   | See @packages/daemon/src/state/AGENTS.md       |
| Deployment instructions (`legion.yaml` `instructions:`) | `packages/daemon/src/daemon/deployment-instructions.ts` | Operator markdown appended to every pane's system prompt; see @packages/daemon/src/daemon/AGENTS.md |
| Add phase guidance     | `skills/legion-worker/SKILL.md`                | See @skills/AGENTS.md                          |
| Change architect loop  | `skills/legion-architect/SKILL.md`             | See @skills/AGENTS.md                          |
| Handoff ledger         | `.legion/` on issue branch                     | Committed phase output                          |
| Envoy event routing    | `packages/envoy/`                              | See @packages/envoy/AGENTS.md                  |
| Shared event contracts | `packages/contracts/`                          | See @packages/contracts/AGENTS.md               |
| Envoy OMP adapter      | `packages/pi-envoy/`                          | See @packages/pi-envoy/AGENTS.md          |
| Worker image (Kubernetes) | `packages/daemon/docker/worker.Dockerfile`, `.github/workflows/worker-image.yaml` | See `docs/kubernetes.md` |
| In-cluster daemon (Kubernetes) | `deploy/kubernetes/daemon/`, `packages/daemon/src/daemon/worker-image-probe.ts` | Kustomize base + kind overlay; the probe pod. See `docs/kubernetes.md` "In-cluster daemon" and @packages/daemon/src/daemon/AGENTS.md "In-cluster mode" |
| Prove the Kubernetes runtime live | `scripts/kind-smoke/` | `up.sh` / `checkpoints.sh` / `down.sh`; `docs/kubernetes.md` "Runbook: the kind smoke" |
| Native Dispatch workspace | `packages/dispatch/`, `packages/envoy/cmd/dispatch/` | React SPA and native Dispatch server |

## Conventions

- **Strict mode** — `strict: true` in tsconfig
- **Biome** — double quotes, semicolons, ES5 trailing commas, 100 char width
- **Imports** — `node:` prefix for builtins, `type` keyword for type-only imports
- **Interfaces** for object shapes, **types** for unions/aliases
- **No barrel files** — direct imports between modules (intentional, avoids circular deps)
- **Dependency injection** — daemon accepts `overrides` for testability
- **Tests** — co-located `__tests__/` dirs, Bun test runner (`bun:test`)

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
**Retro:** runs after the reviewer approves the cleaned head and before the merger publishes `READY`.
**Production check:** after the merge lands, the implementer — the agent that developed the change — drives it in production and records that on the pull request and the issue; the architect signs off only then.

Statuses above are native Dispatch issue statuses, not GitHub labels — the daemon writes
`in_progress`/`testing`/`needs_review`/`retro` via `DispatchClient.setStatus` for the issues it
runs, and a human may set any lifecycle status from the Dispatch dashboard. `legion status <issue>
<status>` remains the controller path for `triage`/`icebox`/`backlog`/`todo`. A human can also
close an issue into `done` and reopen it into `backlog` from the Dispatch dashboard.

**Gate:** the design gate, when armed (`gates.design: root-issues` in `legion.yaml`, the default),
is a human approving the root issue's spec document at a version in Dispatch: the architect
requests it with `dispatch_request_approval` and registers the document id and version with the
daemon, and the daemon opens the gate on the `artifact.approved` event for that document at its
current version — or at registration itself, when Dispatch already shows the human approved that
version before the architect registered (the daemon reads the approval; it never writes one). A
later spec version closes the gate until someone approves the new version, and
a `changes_requested` review closes it with the reviewer's reason. `gates.design: off` is the only
way past the gate without a human review — Legion has no operator approve command; with `off` the
root architect is told so in its system prompt and adds no approval step. Whether a human must
approve a pull request before it merges is the repository's own branch-protection or CODEOWNERS
rule: Legion neither reads nor writes it. The merger publishes `READY` to the controller's role
topic; the controller verifies the gates against live GitHub and merges under the implement App and
the repository's rules. Every merge into `main` goes through GitHub's merge queue: enqueueing a
pull request makes GitHub run the `Tests` workflow's `lint`, `typecheck`, and `test` jobs (its
`merge_group` trigger) on a temporary merge of the pull request onto the current `main`, and the
pull request lands only if they pass — no rebase and no new commit on its branch, and a pull
request whose combination with the moved `main` fails is dropped from the queue instead of merged.
No lifecycle labels exist; GitHub issues are never read or written by Legion.

**Review signaling:** Native GitHub review API, tester status checks, and committed handoffs are
the phase-verdict artifacts. No lifecycle labels carry worker state.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md` — human-authored design history, not a Legion artifact. A Legion planner's plan lives in `.legion/plan.json` and the Dispatch issue document; no Legion role commits a plan or spec file here.
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
