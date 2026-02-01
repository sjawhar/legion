---
title: "Using PR draft status instead of labels for review signaling"
category: integration-issues
tags:
  - github
  - graphql
  - pull-requests
  - state-machine
  - pr-review
  - draft-status
  - batching
module: legion.state
component: fetch/decision
symptoms:
  - needed to detect PR review outcome for worker dispatch
  - PR labels required worker to manage label state
  - race conditions possible with label-based signaling
  - extra API call needed to apply labels
date_solved: 2026-02-01
---

# Using PR Draft Status Instead of Labels for Review Signaling

## Problem

Legion's state collection system needed to detect whether a GitHub PR was approved or had changes requested to determine the next action for worker dispatch. The initial approach used GitHub PR labels (`worker-approved`, `worker-changes-requested`), but this had problems:

- **Label management complexity**: Required the review worker to add/remove labels on the PR
- **Race conditions**: Labels could be in inconsistent states
- **Extra API calls**: Needed to fetch PR labels separately for each PR
- **Fragile coupling**: Relied on workers correctly managing label state

## Solution

Replace label-based signaling with the **native GitHub `isDraft` field**:

- PR in draft state = changes requested (needs more work)
- PR ready for review (not draft) = approved (ready to merge)

This leverages GitHub's built-in PR workflow semantics and eliminates custom label management.

## Implementation

### 1. GitHubPRRef Type (types.py)

Immutable value object for parsing PR URLs with input validation:

```python
@dataclass(frozen=True)
class GitHubPRRef:
    """Parsed GitHub PR reference from URL (immutable value object)."""

    owner: str
    repo: str
    number: int

    @classmethod
    def from_url(cls, url: str) -> "GitHubPRRef | None":
        """Parse a GitHub PR URL into a reference."""
        match = re.match(r"https://github\.com/([^/]+)/([^/]+)/pull/(\d+)", url)
        if not match:
            return None
        owner, repo = match.group(1), match.group(2)
        # Validate owner/repo contain only safe characters
        if not re.match(r"^[\w.-]+$", owner) or not re.match(r"^[\w.-]+$", repo):
            return None
        return cls(owner=owner, repo=repo, number=int(match.group(3)))
```

### 2. Batch GraphQL Fetching (fetch.py)

Single GraphQL query with aliases to fetch multiple PRs across multiple repos:

```python
@retry(
    stop=stop_after_attempt(3),
    wait=wait_exponential(multiplier=1, min=1, max=10),
    retry=retry_if_exception_type(GitHubAPIError),
    reraise=True,
)
async def get_pr_draft_status_batch(
    pr_refs: dict[str, GitHubPRRef],
    *,
    runner: CommandRunner = default_runner,
) -> dict[str, bool | None]:
    """Fetch PR draft status for multiple issues in a single GraphQL query."""
    # Group by repository, build aliases, construct query...
    query = f"query {{ {' '.join(query_parts)} }}"
```

**Generated GraphQL Query Example:**
```graphql
query {
  repo0: repository(owner: "myorg", name: "myrepo") {
    pr0: pullRequest(number: 123) { isDraft }
    pr1: pullRequest(number: 456) { isDraft }
  }
  repo1: repository(owner: "otherorg", name: "otherrepo") {
    pr0: pullRequest(number: 789) { isDraft }
  }
}
```

### 3. Decision Logic (decision.py)

Uses `pr_is_draft` instead of labels to determine review outcome:

```python
case IssueStatus.NEEDS_REVIEW:
    if has_worker_done:
        # Review outcome is signaled by PR draft status:
        # - PR ready (not draft) = approved → transition to retro
        # - PR still draft = changes requested → resume implementer
        # - No PR = wait for PR to be created
        if pr_is_draft is None:
            return "skip"
        if pr_is_draft:
            return "resume_implementer_for_changes"
        return "transition_to_retro"
```

## Key Implementation Details

| Aspect | Implementation |
|--------|----------------|
| **GraphQL Batching** | Uses aliases (`repo0`, `repo1`, `pr0`, `pr1`) to query multiple PRs in one call |
| **Retry Logic** | `tenacity` with exponential backoff: 3 attempts, 1-10 second waits |
| **Input Validation** | Regex validates owner/repo names contain only `[\w.-]+` to prevent injection |
| **Null-Safe Parsing** | Uses `data.get("data") or {}` and checks for `isDraft` key presence |
| **Error Handling** | Custom `GitHubAPIError` raised on non-zero exit or JSON parse failure |
| **Type Safety** | `pr_is_draft: bool | None` - None means no PR exists, True/False for draft status |
| **Selective Fetching** | Only queries PRs for issues in "Needs Review" status with `worker-done` label |

## Prevention Strategies

### Input Validation for URL-Derived Data
- Always return `None` for parsing failures rather than raising exceptions
- Validate extracted components against allowlists (alphanumeric, hyphen, underscore, dot)
- Use anchored regexes (`^...$`) to prevent partial matches

### Null-Safe Response Handling
- Use `dict.get()` with fallback values (`or {}`) for nested access
- Check for existence AND truthiness before accessing nested fields
- Always provide a defined return value rather than omitting keys

### Retry Logic for Transient Failures
- Use exponential backoff to avoid overwhelming rate-limited APIs
- Define a custom exception type for retryable failures
- Limit retry attempts and re-raise after exhaustion

## When to Use Batched GraphQL vs REST

**Use Batched GraphQL when:**
- Fetching the same field(s) from multiple entities
- Data is spread across multiple repositories
- Reducing API call count matters (rate limiting, latency)

**Use REST when:**
- Performing mutations
- Simple single-resource queries
- Using `gh` CLI's built-in commands

## Related Documentation

- [Controller Skill Redesign Plan](../../plans/2026-02-01-feat-controller-skill-redesign-plan.md) - Status transition rules
- [Sync-to-Async Refactoring](../architecture-patterns/sync-to-async-modular-refactoring.md) - GraphQL batching pattern
- [Worker Skill Design](../../plans/2026-01-31-worker-skill-design.md) - Issue state flow

## Test Coverage

Tests in `tests/test_state.py` cover:
- Multiple PRs batched correctly
- Missing PR returns `None`
- `isDraft: null` treated as `False`
- Retry exhaustion raises `GitHubAPIError`
- Retry succeeds on transient failures
- `{"data": null}` handled gracefully
- Cross-repo batching in single query
