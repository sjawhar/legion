# State Support Module

This directory provides GitHub artifact access shared by the daemon's resync, approval, and catch-up paths. It does not own daemon lifecycle state or worker orchestration.

## Files

| File | Responsibility |
| --- | --- |
| `fetch.ts` | Injectable `gh` command runner plus batched PR review and CI/merge GraphQL helpers. |
| `github-fetch.ts` | Fetches GitHub Project v2 items for resync. |
| `backends/github.ts` | GitHub issue-tracker parsing and project-status mutations retained for repository integrations. |
| `backends/linear.ts` | Linear backend support retained for non-Legion consumers. |
| `types.ts` | Shared legacy tracker data types used by the retained backend adapters. |

## Boundaries

- `fetch.ts` performs GitHub artifact reads only. It never asks the daemon for worker state.
- Resync and catch-up derive their observable state from GitHub artifacts and `LegionState`; they do not reconstruct the removed shared-serve worker map.
- New daemon work belongs in `../daemon/legion-state.ts`, reducers, event intake, or process management rather than this support layer.
