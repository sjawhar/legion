---
title: "Required GitHub App Checks on main"
category: github
tags: ["github", "branch-protection", "checks", "github-apps"]
date: "2026-07-19"
status: "active"
---

# Required GitHub App Checks on `main`

`main` must require these checks before a pull request can merge:

| Check | Publisher App | Source pin |
| --- | --- | --- |
| `CI` | GitHub Actions | App ID `15368` |
| `tester` | Legion reviewer App | `REVIEWER_APP_ID` |
| `architect` | Legion reviewer App | `REVIEWER_APP_ID` |

The reviewer App must also submit an approving pull-request review. Branch
protection requires one approval. GitHub's branch-protection REST API has no
field that restricts the approving reviewer to one App, so source pinning is
available for check runs only. The deployment verification below confirms that
the reviewer App can submit the `tester` check; validate the review workflow
separately before relying on the approval requirement.

## Sequencing

**Do not apply this policy until CI can publish `CI` and the reviewer App can
publish both bare `tester` and `architect` checks.** Applying it earlier blocks
every merge, including the pull request that adds the check-publishing pipeline.

Before applying, verify a scratch pull request head contains all three names and
the expected App IDs:

```bash
gh api -H 'Accept: application/vnd.github+json' \
  "repos/sjawhar/legion/commits/<scratch-pr-head-sha>/check-runs" \
  --jq '.check_runs[] | select(.name == "CI" or .name == "tester" or .name == "architect") | {name, app_id: .app.id, app_slug: .app.slug}'
```

The initial U5 inspection found GitHub Actions App ID `15368` on existing job
checks, but no bare `CI` check on the sampled open PR. That prerequisite must
be satisfied before this script is applied.

## Verify `tester` with the reviewer App

Use a disposable scratch PR, not a production pull request. Mint an installation
token for the reviewer App, keep it in `REVIEWER_APP_TOKEN`, and set
`REVIEWER_APP_ID` to that App's numeric ID.

```bash
scratch_pr=<scratch-pr-number>
head_sha="$(gh pr view "$scratch_pr" --repo sjawhar/legion --json headRefOid --jq .headRefOid)"

created="$(GH_TOKEN="$REVIEWER_APP_TOKEN" gh api -X POST \
  -H 'Accept: application/vnd.github+json' \
  "repos/sjawhar/legion/check-runs" \
  -f name=tester \
  -f head_sha="$head_sha" \
  -f status=completed \
  -f conclusion=success)"

test "$(jq -r .name <<<"$created")" = tester
test "$(jq -r .head_sha <<<"$created")" = "$head_sha"
test "$(jq -r .app.id <<<"$created")" = "$REVIEWER_APP_ID"

GH_TOKEN="$REVIEWER_APP_TOKEN" gh api \
  -H 'Accept: application/vnd.github+json' \
  "repos/sjawhar/legion/commits/${head_sha}/check-runs" \
  --jq '.check_runs[] | select(.name == "tester") | {id, name, head_sha, status, conclusion, app_id: .app.id, app_slug: .app.slug}'
```

Record the created run's `id`, `head_sha`, `app_id`, and `app_slug` with the
deployment evidence. Do not use an implementer-App token: a run made by that
App does not prove the reviewer App can publish the required verdict.

## Apply the prepared policy

The script is dry-run by default and only targets `sjawhar/legion:main`. It
requires a credential with repository Administration write access and never
runs as part of CI.

```bash
REVIEWER_APP_ID=<reviewer-app-id> \
APPLY_BRANCH_PROTECTION=1 \
scripts/branch-protection.sh
```

`CI_APP_ID` defaults to `15368`, the GitHub Actions App ID observed on the
sampled PR. Set `CI_APP_ID=<numeric-id>` only if the CI publisher changes.

For a previously unprotected branch, the script creates the minimum policy:
the three pinned checks and one required approval. For an already protected
branch, it preserves unrelated required checks and review settings, replaces
only the three managed check names with their pinned App IDs, and raises the
required approval count to at least one. Re-running it converges on the same
policy.

## Implementation Notes

### Check Merge Logic

When updating an already-protected branch, the script merges existing checks with required checks. The merge uses only `$current.checks` (which already carries every context with its app_id from the GitHub API response). The legacy `.contexts` array is not included in the merge to avoid duplicate context entries with conflicting app_ids.

### Strict Mode Preservation

On PATCH updates to existing protection, the script preserves the branch's current `strict` value from the GET response. The `strict: true` setting is applied only on fresh PUT operations for unprotected branches. This prevents silent flipping of an existing `strict: false` configuration.
