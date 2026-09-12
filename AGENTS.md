# Legion

Autonomous development swarm using Oh My Pi agents. Root processes own issue trees, and the Legion daemon supplies durable state, credentials, and event routing.

## Architecture

Legion's issue lifecycle lives entirely on native Dispatch (triage → done); the daemon never
creates, reads, or writes a GitHub issue. It derives role and gate state from Dispatch issue
events plus GitHub PR/CI artifacts, records root-process session locators, and publishes only the
verdict changes each role needs. Root processes run in tmux; phase workers are headless
`omp --mode rpc` processes the daemon spawns directly, one tmux pane per worker, bridged through
`legion worker-shim`.

- **TypeScript daemon** — webhook intake, reducers, durable `LegionState`, root-process lifecycle,
  credential grants, resync, and recovery.
- **OMP extension** — injects the Legion tool and event delivery into active OMP sessions and
  provisions issue workspaces. Phase workers (planner/implementer/tester/reviewer/merger, and
  sub-architects for child issues) are headless `omp --mode rpc` processes the daemon spawns
  directly, one tmux pane per worker, bridged through `legion worker-shim`; the daemon enforces
  recursion limits when spawning them.
- **Skills** — guide the architect and sequential phase workers. Durable `.legion/<phase>.json`
  handoffs are the recovery source of truth.

## Tech Stack

- **TypeScript** on **Bun** runtime
- **citty** for CLI, **Bun.serve** for HTTP daemon
- **Oh My Pi extension** for Legion tools, role delivery, workspace provisioning, and phase workers
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, **Linear** or **GitHub Issues** for issue tracking

## Commands

```bash
bun install                   # Setup
bunx biome check src/         # Lint
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
legion gh -- <args>                  # Run gh with a session-bound GitHub token (refuses `pr merge`; the merge queue merges, not workers)
legion credential                    # Git credential helper for Legion grants
legion state                         # Read daemon state
legion handoff write|read|message    # Workers: write/read structured handoff data on issue branch
legion handoff complete --summary <text>  # Workers: report phase completion to the tree's architect, keeping the role claimed (authenticates via LEGION_GRANT exactly like `legion gh`/`legion credential` — no session secret in the request)
legion worker-shim --socket <path> -- <omp argv…>  # Bridges a headless phase-worker OMP process to the daemon over a unix socket (daemon-spawned, not run by hand)
legion worker-shim --connect tcp://<host>:<port> --boot-token-file <path> -- <omp argv…>  # Same bridge, reverse-dialed: the shim dials the daemon's worker stream listener and authenticates with its boot token (Kubernetes runtime; daemon-spawned)
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
| Add phase guidance     | `skills/legion-worker/SKILL.md`                | See @skills/AGENTS.md                          |
| Change architect loop  | `skills/legion-architect/SKILL.md`             | See @skills/AGENTS.md                          |
| Handoff ledger         | `.legion/` on issue branch                     | Committed phase output                          |
| Envoy event routing    | `packages/envoy/`                              | See @packages/envoy/AGENTS.md                  |
| Shared event contracts | `packages/contracts/`                          | See @packages/contracts/AGENTS.md               |
| Envoy OMP adapter      | `packages/pi-envoy/`                          | See @packages/pi-envoy/AGENTS.md          |
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

**Phase roles:** architect → plan → implement → test → review → merge
**Retro:** runs after the reviewer approves the cleaned head and before the merger publishes `READY`.

Statuses above are native Dispatch issue statuses, not GitHub labels — the daemon owns every
`in_progress`/`testing`/`needs_review`/`retro`/`done` write via `DispatchClient.setStatus`, and a
human or the controller moves `triage`/`icebox`/`backlog`/`todo` from the Dispatch dashboard or
`legion status <issue> <status>`.

**Gate:** the design gate is the architect's `dispatch_ask` on the root issue with an `Approve`
option, armed per deployment by `gates.design` in `legion.yaml` (`root-issues`, the default, or
`off`). With `off`, the daemon satisfies the gate the moment the architect registers it
(`designApproved: "gate-off"`) and sends the same `design-approved` wake a human answer would, so
no one has to click; the ask stays on Dispatch as a record. Whether a human must approve a pull
request before it merges is the repository's own branch-protection or CODEOWNERS rule: Legion
neither reads nor writes it. The merger publishes `READY` to the merge queue, which merges under
its own authority and the repository's rules. No lifecycle labels exist; GitHub issues are never
read or written by Legion.

**Review signaling:** Native GitHub review API, tester status checks, and committed handoffs are
the phase-verdict artifacts. No lifecycle labels carry worker state.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md`
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
```
