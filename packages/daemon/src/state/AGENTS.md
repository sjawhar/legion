# State Module

Issue state machine. Fetches data from issue tracker + daemon, runs decision logic, outputs action recommendations. Invoked by the controller skill via `POST /state/collect` or the legacy stdin/stdout pipe.

## Data Flow

```
POST /state/collect {backend, issues} → backend.parseIssues() → enrichParsedIssues() → buildCollectedState() → JSON
                                                                        ↓
                                                            parallel: daemon /workers + gh api graphql
```

## Files

| File | Responsibility |
|------|---------------|
| `types.ts` | All domain types. `IssueStatus` (enum-like with `normalize()`), `WorkerMode`, `ActionType` (21 actions), `ParsedIssue`, `FetchedIssueData`, `IssueState`, `CollectedState`. Also `computeSessionId()` and `computeControllerSessionId()` — shared by daemon. |
| `fetch.ts` | All I/O. `enrichParsedIssues()` enriches parsed issues with worker status + PR draft status. `fetchAllIssueData()` is the legacy wrapper that parses Linear JSON then enriches. `getLiveWorkers()` (daemon HTTP) + `getPrDraftStatusBatch()` (GitHub GraphQL with 3x retry). Accepts injectable `CommandRunner` for testing. |
| `decision.ts` | Pure logic, zero I/O. `suggestAction(status, flags...)` → `ActionType`. `buildIssueState()` and `buildCollectedState()` assemble final output. `ACTION_TO_MODE` maps actions to worker modes. |
| `cli.ts` | Legacy entry point for pipe invocation: `echo $JSON | bun run packages/daemon/src/state/cli.ts --team-id X --daemon-url Y`. Reads stdin, calls fetch + decision, writes JSON to stdout. Superseded by `POST /state/collect`. |
| `backends/` | Pluggable issue tracker backends. `issue-tracker.ts` defines the `IssueTracker` interface. `linear.ts` and `github.ts` implement it. `index.ts` has the factory. |

## ActionType State Machine (decision.ts)

The core of the controller's decision-making. Key transitions:

| Status | worker-done? | live worker? | PR state | test labels | → Action |
|--------|-------------|-------------|----------|-------------|----------|
| Backlog | yes | — | — | — | `transition_to_todo` |
| Todo | no | no | — | — | `dispatch_planner` |
| In Progress | yes | — | — | — | `transition_to_testing` |
| Testing | no | no | — | — | `dispatch_tester` |
| Testing | yes | — | — | test-passed | `transition_to_needs_review` |
| Testing | yes | — | — | !test-passed | `resume_implementer_for_test_failure` |
| Needs Review | yes | — | ready | — | `transition_to_retro` |
| Needs Review | yes | — | draft | — | `resume_implementer_for_changes` |
| Needs Review | yes | — | no PR | — | `investigate_no_pr` |
| Retro | yes | — | — | — | `dispatch_merger` |
| Retro | no | no | — | — | `dispatch_implementer_for_retro` |
| Any | — | yes | — | — | `skip` (worker already running) |

**Note:** The state machine only checks `hasTestPassed` (presence of `test-passed` label). `hasTestFailed`/`test-failed` is computed and wired but not used in decision logic — it exists for human visibility and controller label cleanup. `worker-done` without `test-passed` is treated as failure regardless of whether `test-failed` is present.

**Hardening notes (from #65):**
- `needs-approval` check is scoped to Backlog/Todo only. A leaked `needs-approval` label on other statuses does not freeze the issue.
- In Progress with `hasPr` but no live worker and no `worker-done` dispatches a fresh implementer (deadlock recovery).
- Retro with a live worker returns `skip` (not `resume_implementer_for_retro`) to prevent prompt spam.
- `skip` action uses the actual `workerMode` from the daemon (when available) for sessionId computation, instead of defaulting to `implement`.
- `transition_to_done` action exists for explicit Done transitions (mapped to `merge` mode).

## Anti-Patterns

- **Don't import from `fetch.ts` in decision.ts** — decision must stay pure (no I/O)
- **Don't add status aliases in code** — use `IssueStatus.ALIASES` map in types.ts
- `types.ts` is imported by `../daemon/server.ts` — changes to exported types affect both modules
