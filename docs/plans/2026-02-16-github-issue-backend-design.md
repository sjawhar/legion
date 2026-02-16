# Pluggable Issue Tracker Backend: GitHub Support

## Goal

Replace Legion's hard dependency on Linear with a pluggable backend that supports both Linear and GitHub Issues (via GitHub Projects V2). This consolidates issue tracking and PR management onto a single platform and eliminates Linear-specific workarounds.

## Decisions

- **Pluggable backend** — `IssueTracker` interface in TypeScript, Linear and GitHub implementations
- **GitHub Projects V2 Status field** for the 8-state lifecycle (Triage through Done)
- **`gh` CLI directly** in skills for mutations (no MCP, no custom wrapper)
- **Workers set status explicitly** (no auto-transition dependency)
- **Single user + explicit transitions** for now (GitHub App as future upgrade)
- **`LEGION_TEAM_ID`** = `owner/project-number` for GitHub (e.g., `sjawhar/5`)
- **CLI flag `--backend`** with `LEGION_ISSUE_BACKEND` env var fallback
- **Breaking change** — no backward compatibility or migration path

## Architecture

### Principle: Skills Own Tracker I/O, TypeScript Owns Enrichment + Logic

Skills (AI agents) handle all issue tracker I/O: fetching issue lists, updating statuses, adding labels, posting comments, creating issues. TypeScript handles enrichment (daemon worker status, GitHub PR draft status) and decision logic (the state machine).

The boundary is the `POST /state/collect` daemon endpoint: the skill pushes raw issue data in, TypeScript enriches and analyzes it, and returns `CollectedState` out. The enrichment step involves internal I/O (daemon HTTP for worker status, `gh api graphql` for PR draft status) — this is TypeScript's responsibility because it's mechanical and deterministic, not tracker-specific.

### Data Flow

```
Controller skill
  │
  ├─ linear_linear(action="search") ──────► Linear JSON
  │  OR                                         │
  ├─ gh project item-list ... ─────────► GitHub JSON
  │                                             │
  └─ POST /state/collect ──────────────────────►│
       {backend: "linear"|"github",             │
        issues: <raw JSON>}                     │
                                                ▼
                              ┌─────────────────────────┐
                              │   Daemon Server          │
                              │                          │
                              │   Backend parser         │
                              │   (linear.ts / github.ts)│
                              │         │                │
                              │         ▼                │
                              │   ParsedIssue[]          │
                              │         │                │
                              │   fetchAllIssueData()    │
                              │   (workers + PR draft)   │
                              │         │                │
                              │         ▼                │
                              │   decision.ts            │
                              │   (zero changes)         │
                              │         │                │
                              │         ▼                │
                              │   CollectedState         │
                              └────────────┬────────────┘
                                           │
                                           ▼
                              Controller routes actions
                                ├─ dispatch_ → legion dispatch
                                ├─ transition_ → gh/linear mutation
                                ├─ resume_ → legion prompt
                                └─ skip → check raw signals
```

### Mutations (Skill Layer)

Workers and controller perform mutations directly via backend-specific tools:

| Operation | Linear | GitHub |
|-----------|--------|--------|
| Update status | `linear_linear(action="update", state="Done")` | `gh api graphql` (update Projects V2 Status field) |
| Add label | `linear_linear(action="update", labels=[current + new])` | `gh issue edit N --add-label "worker-done" -R owner/repo` |
| Remove label | `linear_linear(action="update", labels=[current - old])` | `gh issue edit N --remove-label "worker-active" -R owner/repo` |
| Comment | `linear_linear(action="comment", body="...")` | `gh issue comment N --body "..." -R owner/repo` |
| Create issue | `linear_linear(action="create", title="...")` | `gh issue create --title "..." -R owner/repo` |
| Get issue | `linear_linear(action="get", id="LEG-42")` | `gh issue view N --json ... -R owner/repo` |

GitHub labels are additive (`--add-label`, `--remove-label`) — no read-modify-write.

## TypeScript Changes

### New: `IssueTracker` Interface

```typescript
// packages/daemon/src/state/issue-tracker.ts

interface IssueTracker {
  /** Parse raw issue data from the tracker into normalized form.
   *  PR refs: Linear backend extracts from attachments (sync).
   *  GitHub backend includes repo context but NOT PR refs (requires I/O).
   *  PR discovery happens in fetchAllIssueData() for both backends. */
  parseIssues(raw: unknown): ParsedIssue[];

  /** Resolve a team/project reference to a stable internal ID.
   *  Linear: UUID passthrough or GraphQL lookup.
   *  GitHub: owner/project-number → project node ID via gh project view. */
  resolveTeamId(ref: string): Promise<string>;
}
```

Each backend implements this. The interface is deliberately narrow — only what the state machine pipeline needs.

**PR discovery:** For GitHub, PRs are discovered by branch naming convention during the enrichment step (`fetchAllIssueData()`), not during parsing. The GitHub parser returns `ParsedIssue` with `prRef: null`, and `fetchAllIssueData()` runs `gh pr list --head {issueId} --json isDraft,number` to find linked PRs. This is consistent with how PR *draft* status is already fetched in the enrichment step for both backends.

### New: Backend Implementations

```
packages/daemon/src/state/backends/
├── linear.ts    # Existing parseLinearIssues() logic, relocated
├── github.ts    # New: parses gh project item-list JSON
└── index.ts     # Factory: getBackend(name) → IssueTracker
```

**`linear.ts`**: Preserves existing MCP + GraphQL dual-format parsing. `resolveTeamId()` does Linear GraphQL lookup (from current `team-resolver.ts`).

**`github.ts`**: Parses `gh project item-list --format json` output. Extracts issue number, repo, status (from Projects V2 Status field), and labels. PR discovery uses branch naming convention: `gh pr list --head {issueId}` to find linked PRs (since Legion controls branch names). `resolveTeamId()` takes `owner/project-number` (e.g., `sjawhar/5`), calls `gh project view` to resolve it to a project node ID (e.g., `PVT_kwDO...`), which becomes the stable internal team ID used for session ID computation.

### New: `POST /state/collect` Endpoint

Added to `server.ts`:

```typescript
POST /state/collect
Body: { backend: "linear" | "github", issues: <raw JSON array> }
Returns: CollectedStateDict (same shape as current CLI output)
```

The daemon already knows `teamId` from its startup config — the request body only carries backend type and raw issue data. Internally: `getBackend(backend).parseIssues(issues)` → `fetchAllIssueData()` → `buildCollectedState(issuesData, teamId)` → JSON response. This replaces the `cli.ts` stdin/stdout pipe.

### New: `legion collect-state` CLI Command

Thin wrapper around `POST /state/collect` for manual debugging:

```bash
echo "$ISSUES_JSON" | legion collect-state --backend github
```

Reads stdin, POSTs to daemon, prints `CollectedState` JSON.

### Modified: Session ID Computation

`computeSessionId()` no longer requires the team ID to be a UUID. Any string is hashed into a deterministic UUID namespace:

```typescript
const LEGION_NAMESPACE = "..."; // Fixed UUID constant

function teamIdToNamespace(teamId: string): string {
  return uuidv5(teamId, LEGION_NAMESPACE);
}
```

Session ID format (`ses_` + 12 hex + 14 Base62) is unchanged. Determinism is preserved: same (teamId, issueId, mode) always produces the same session ID.

### Modified: Types

`LinearIssueRaw`, `LinearStateDict`, `LinearLabelsContainer`, `LinearLabelNode`, `LinearAttachment` move from `types.ts` to `backends/linear.ts`. They become internal to the Linear backend.

`types.ts` retains only backend-agnostic types: `ParsedIssue`, `FetchedIssueData`, `IssueState`, `IssueStatus`, `ActionType`, `WorkerMode`, `CollectedState`.

### Unchanged

- `decision.ts` — zero changes (pure logic, only sees `FetchedIssueData`)
- `IssueStatus` canonical values and normalization
- `ActionType` enum and `ACTION_TO_MODE` mapping
- `WorkerMode` enum
- `getPrDraftStatusBatch()` — already uses GitHub GraphQL regardless of backend
- `getLiveWorkers()` — already uses daemon HTTP
- `server.ts` — all existing endpoints unchanged (one new endpoint added)

## Issue Identifier Format

### Problem

Worker IDs use the format `{issueId}-{mode}` (e.g., `leg-42-implement`). The mode is parsed by splitting on the last hyphen. GitHub identifiers like `owner/repo#123` contain `/` and `#` which break filesystem paths and ID parsing.

### Solution

The issue ID is the human-readable short form: `{repo}-{number}` (e.g., `gh-42`, `legion-15`). The repo prefix is derived from the repository name (not the owner). The number is always the trailing numeric segment, so repo names with hyphens (e.g., `my-repo-42`) are unambiguous — parse from the right. For a single-repo project, this is concise. For multi-repo projects, the repo name disambiguates.

The full context (owner, repo, issue number) is stored in the `ParsedIssue` and available to workers through the issue metadata. Workers get the issue ID in their prompt (e.g., `/legion-worker implement mode for gh-42`) and can look up the full details via `gh issue view`.

### Multi-Repo Projects

GitHub Projects V2 can contain issues from multiple repos. The `gh project item-list` output includes the repository for each item. The GitHub backend parser extracts this and stores it in `ParsedIssue`. Workers use the repo from issue metadata, not from a global config var.

## Environment Variables

### Daemon

| Variable | Purpose | Example |
|----------|---------|---------|
| `LEGION_ISSUE_BACKEND` | Backend selection (CLI flag fallback) | `github` or `linear` |
| `LEGION_TEAM_ID` | Tracker scope identifier | `sjawhar/5` (GitHub) or UUID (Linear) |

Backend-specific auth:
- **Linear**: `LINEAR_API_TOKEN` (for Linear MCP in skills)
- **GitHub**: `gh` CLI auth (no env var — uses `gh auth login`)

### Workers

Workers receive **no issue-tracker env vars**. They get:
- The issue ID in their prompt (e.g., `/legion-worker implement mode for gh-42`)
- The backend type via skill loading: the daemon includes the backend-specific skill name in the worker's initial prompt (e.g., `/legion-worker implement mode for gh-42` with `github` skill pre-loaded). The worker `SKILL.md` checks `LEGION_ISSUE_BACKEND` (set process-wide by the daemon) to conditionally reference `github/SKILL.md` or `linear/SKILL.md` patterns.

### Removed

- `LINEAR_TEAM_ID` — replaced by `LEGION_TEAM_ID`
- `LINEAR_ISSUE_ID` — removed (issue ID comes from prompt)
- `LINEAR_API_KEY` — consolidated into `LINEAR_API_TOKEN` (only for Linear backend)

## Skill Changes

### New: `github/SKILL.md`

Documents `gh` CLI patterns for all issue operations. Parallel to `linear/SKILL.md`. No MCP dependency — uses standard `gh` commands.

Key differences from Linear skill:
- Labels are additive (`--add-label` / `--remove-label`)
- Status changes go through Projects V2 GraphQL API
- Issue creation specifies repo explicitly (`-R owner/repo`)
- PR discovery uses branch naming convention (`gh pr list --head {issueId}`), not attachments

### Modified: Controller Skill

The fetch step becomes backend-conditional:

```
If LEGION_ISSUE_BACKEND=linear:
  ISSUES=$(linear_linear(action="search", query={"team": "$LEGION_TEAM_ID"}))
If LEGION_ISSUE_BACKEND=github:
  ISSUES=$(gh project item-list $PROJECT_NUM --owner $OWNER --format json)

STATE=$(curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect \
  -H 'Content-Type: application/json' \
  -d "{\"backend\": \"$LEGION_ISSUE_BACKEND\", \"issues\": $ISSUES}")
```

Action routing is unchanged — `suggestedAction` values are backend-agnostic.

Status transitions use the appropriate tool:
- Linear: `linear_linear(action="update", id="LEG-42", state="Done")`
- GitHub: `gh api graphql` mutation to update Projects V2 Status field

### Modified: Worker Skills

Worker `SKILL.md` and workflow files reference the issue tracker skill conditionally. The `references/linear-labels.md` file gets a GitHub counterpart documenting `gh issue edit --add-label` patterns.

The implement workflow no longer depends on auto-transition. Workers set status explicitly before exiting.

## GitHub Projects V2 Setup

The target GitHub Project needs:

1. **Status field** (single-select) with options matching the 8 canonical states:
   `Triage`, `Icebox`, `Backlog`, `Todo`, `In Progress`, `Needs Review`, `Retro`, `Done`

2. **Labels** created on the repo(s):
   `worker-done`, `worker-active`, `user-input-needed`, `user-feedback-given`, `needs-approval`, `human-approved`

3. **Built-in automations** (optional but recommended):
   - Item closed → Status: Done
   - PR merged → Status: Done

All other status transitions are handled explicitly by workers/controller.

## Complexity Eliminated

| Linear Pain Point | Resolution with GitHub |
|----|---|
| Dual API response formats (MCP vs GraphQL) | Single `gh` CLI JSON format |
| Label replace-all semantics | Additive `--add-label` / `--remove-label` |
| PR-via-attachments discovery | Native GitHub PR-issue linking |
| Auto-transition dependency (unreliable for external repos) | Explicit status transitions |
| Stream Linear MCP dependency | Standard `gh` CLI (no extra dependency) |
| `LINEAR_API_KEY` + `LINEAR_API_TOKEN` credential split | `gh auth` (no token env vars) |
| `bun run` pipe through bash | `POST /state/collect` HTTP endpoint |

## Implementation Sequence

To avoid a single high-risk mega-change, implement in phases:

### Phase 1: TypeScript abstraction (no behavior change)

1. Create `IssueTracker` interface and `backends/` directory
2. Move Linear-specific types and parsing to `backends/linear.ts`
3. Create `backends/github.ts` parser (can be tested with mock data)
4. Generalize `computeSessionId()` to accept any string team ID
5. Add `POST /state/collect` endpoint to daemon server
6. Add `legion collect-state` CLI command
7. Keep existing `cli.ts` working (both paths functional)

All existing tests must pass. New tests for GitHub parser + new endpoint.

### Phase 2: GitHub skill + controller wiring

1. Create `github/SKILL.md` with `gh` CLI patterns
2. Add `--backend` flag to `legion start`
3. Add `LEGION_ISSUE_BACKEND` env var handling
4. Modify controller skill to support both backends
5. Set up a test GitHub Project with the 8 Status options + labels

### Phase 3: Worker workflows

1. Modify worker `SKILL.md` to conditionally reference backend skill
2. Update each workflow file (architect, plan, implement, review, merge) for GitHub mutations
3. Update `references/linear-labels.md` with GitHub counterpart
4. Remove auto-transition dependency from implement workflow

### Phase 4: Cleanup

1. Remove `cli.ts` (replaced by `/state/collect`)
2. Consolidate env vars (`LINEAR_TEAM_ID` → `LEGION_TEAM_ID`, etc.)
3. Remove `LINEAR_ISSUE_ID` from worker dispatch
4. Update all documentation (AGENTS.md files, solution docs)

## Testing Strategy

### TypeScript (unit tests)

- `backends/github.ts`: mock `gh project item-list` JSON → verify `ParsedIssue[]` output
- `backends/linear.ts`: existing `parseLinearIssues` tests relocated
- `POST /state/collect`: mock issue data → verify `CollectedState` output
- `teamIdToNamespace()`: verify deterministic hashing for various string formats
- `decision.ts`: unchanged, existing tests pass

### Skill Layer (manual + integration)

- Create a test GitHub Project with the 8 Status field options
- Verify controller fetch → collect → route cycle with real data
- Verify worker mutations (label add/remove, status change, comment, issue creation)
- Verify Projects V2 Status field updates via `gh api graphql`

## Error Handling

### `POST /state/collect`

| Condition | Response |
|-----------|----------|
| Invalid `backend` value | 400 `{"error": "invalid_backend"}` |
| Unparseable `issues` JSON | 400 `{"error": "invalid_issues", "detail": "..."}` |
| Backend parser returns 0 issues | 200 with empty `CollectedState` (not an error) |
| Enrichment failure (daemon HTTP or PR draft) | 200 with best-effort data (`prIsDraft: null`, `hasLiveWorker: false`) — matches current behavior |
| Unknown status value from tracker | Passes through; `suggestAction()` returns `skip` for unrecognized statuses |

### Status field mismatch

If GitHub Projects V2 Status field options don't exactly match `IssueStatusLiteral` names, the parser logs a warning and passes the raw value through. The state machine's `suggestAction()` returns `skip` for unrecognized statuses. This is silent but safe — no workers dispatched for issues in unknown states.

The `github/SKILL.md` documents the required Status field option names to prevent misconfiguration.

## GitHub Projects V2 Status Field IDs

Updating the Status field via GraphQL requires field ID and option IDs (not human-readable names). Resolution strategy:

1. During `resolveTeamId()`, the GitHub backend also queries the project's fields and caches the Status field ID + option ID mapping.
2. This mapping is returned alongside the project node ID and stored in daemon config.
3. The `github/SKILL.md` documents the GraphQL mutations using these cached IDs.
4. If the cache is stale (option renamed/added), the controller skill re-runs `resolveTeamId()` to refresh.

The `github/SKILL.md` can include a helper pattern:
```bash
# Resolve status option ID from name (cached in daemon state)
STATUS_ID=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/config/status-ids | jq -r '.["In Progress"]')
```

## Non-Issue Project Items

GitHub Projects V2 can contain draft items, PRs (without issues), and notes. The GitHub parser skips items that are not issues (no `content.number` or `content.repository`). Only items with a valid issue number and repository are included in `ParsedIssue[]`.

## Open Questions

1. **Multi-repo issue numbering**: If the project spans repos, two issues could have the same number in different repos. The `repo-number` format handles this, but the workspace naming (`jj workspace add --name repo-42`) needs to be unique. Verify no collisions in practice.

2. **GitHub API rate limits**: `gh` CLI calls count against the user's rate limit (5000/hr for authenticated users). The controller polls every 30s. At ~2 calls per loop (item-list + collect), that's ~240/hr — well within limits. But workers making mutations add to this. Monitor in practice.

3. **Controller JSON quoting**: The controller skill passes issue JSON to `POST /state/collect` via curl. Large JSON payloads should use `--data @-` (stdin) rather than inline string interpolation to avoid shell quoting issues and arg-length limits.
