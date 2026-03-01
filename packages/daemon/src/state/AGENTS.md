# State Module

Issue state machine. Fetches data from issue tracker + daemon, runs decision logic, outputs action recommendations. Invoked by the controller skill via `POST /state/collect` or the legacy stdin/stdout pipe.

## Data Flow

```
POST /state/collect {backend, issues} → backend.parseIssues() → enrichParsedIssues() → buildCollectedState() → JSON
                                                                        ↓
                                                            parallel: daemon /workers + gh api graphql (draft status + CI status)
```

## Files

| File | Responsibility |
|------|---------------|
| `types.ts` | All domain types. `IssueStatus` (enum-like with `normalize()`), `WorkerMode`, `ActionType`, `CiStatusLiteral`, `ParsedIssue`, `FetchedIssueData`, `IssueState`, `CollectedState`. Also `computeSessionId()` and `computeControllerSessionId()` — shared by daemon. |
| `fetch.ts` | All I/O. `enrichParsedIssues()` enriches parsed issues with worker status + PR draft status + CI status. `fetchAllIssueData()` is the legacy wrapper that parses Linear JSON then enriches. `getLiveWorkers()` (daemon HTTP) + `getPrDraftStatusBatch()` + `getCiStatusBatch()` (GitHub GraphQL with 3x retry). Accepts injectable `CommandRunner` for testing. |
| `decision.ts` | Pure logic, zero I/O. `suggestAction(status, flags...)` → `ActionType`. `buildIssueState()` and `buildCollectedState()` assemble final output. `ACTION_TO_MODE` maps actions to worker modes. |
| `cli.ts` | Legacy entry point for pipe invocation: `echo $JSON | bun run packages/daemon/src/state/cli.ts --team-id X --daemon-url Y`. Reads stdin, calls fetch + decision, writes JSON to stdout. Superseded by `POST /state/collect`. |
| `backends/` | Pluggable issue tracker backends. `issue-tracker.ts` defines the `IssueTracker` interface. `linear.ts` and `github.ts` implement it. `index.ts` has the factory. |

## ActionType State Machine (decision.ts)

The core of the controller's decision-making. Key transitions:

| Status | worker-done? | live worker? | PR state | CI status / test labels | → Action |
|--------|-------------|-------------|----------|------------------------|----------|
| Backlog | yes | — | — | — | `transition_to_todo` |
| Todo | no | no | — | — | `dispatch_planner` |
| In Progress | yes | — | — | — | `transition_to_testing` |
| Testing | no | no | — | — | `dispatch_tester` |
| Testing | yes | — | — | test-passed | `transition_to_needs_review` |
| Testing | yes | — | — | !test-passed | `resume_implementer_for_test_failure` |
| Needs Review | yes | — | ready | passing/null | `transition_to_retro` |
| Needs Review | yes | — | ready | failing | `resume_implementer_for_ci_failure` |
| Needs Review | yes | — | ready | pending | `retry_ci_check` |
| Needs Review | yes | — | draft | — | `resume_implementer_for_changes` |
| Needs Review | yes | — | no PR | — | `investigate_no_pr` |
| Needs Review | no | no | has PR | failing | `resume_implementer_for_ci_failure` |
| Needs Review | no | no | has PR | pending | `retry_ci_check` |
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

**CI status gating (from #62):**
- `ciStatus` (`passing` | `failing` | `pending` | `null`) is fetched via `getCiStatusBatch()` alongside `getPrDraftStatusBatch()` in `enrichParsedIssues()`.
- `prIsDraft` checks take precedence over `ciStatus` — if PR is draft, redirect to implementer without checking CI.
- `ciStatus` is checked in NEEDS_REVIEW both before dispatching reviewer (no worker-done) and before transitioning to retro (worker-done).
- `null` ciStatus (no PR, no checks configured, or API failure) is treated as safe — does not block progression.

## Anti-Patterns

- **Don't import from `fetch.ts` in decision.ts** — decision must stay pure (no I/O)
- **Don't add status aliases in code** — use `IssueStatus.ALIASES` map in types.ts
- `types.ts` is imported by `../daemon/server.ts` — changes to exported types affect both modules
