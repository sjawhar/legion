# State Support Module

This directory provides GitHub artifact access shared by the daemon's resync, approval, and catch-up paths. It does not own daemon lifecycle state or worker orchestration.

## Files

| File | Responsibility |
| --- | --- |
| `fetch.ts` | Injectable `gh` command runner plus batched PR review and CI/merge GraphQL helpers. |
| `github-fetch.ts` | Fetches GitHub Project v2 items for resync. |
| `types.ts` | Shared CI, review, and PR-reference status types for the fetch helpers. |

## Boundaries

- `fetch.ts` performs GitHub artifact reads only. It never asks the daemon for worker state.
- Resync and catch-up derive their observable state from GitHub artifacts and `LegionState`.
- New daemon work belongs in `../daemon/legion-state.ts`, reducers, event intake, or process management rather than this support layer.
