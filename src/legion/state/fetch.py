"""Async data fetching for Legion state collection.

All I/O operations are async and can be composed in a single task group.
Uses:
- anyio for subprocess execution
- tenacity for retry logic
"""

from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from typing import Protocol, TypedDict

import anyio
from tenacity import (
    retry,
    retry_if_exception_type,
    stop_after_attempt,
    wait_exponential,
)

from legion import tmux
from legion.state import types
from legion.state.types import (
    FetchedIssueData,
    LinearIssueRaw,
    ParsedIssue,
)

logger = logging.getLogger(__name__)


# =============================================================================
# Protocols for Dependency Injection
# =============================================================================


class CommandRunner(Protocol):
    """Protocol for running external commands."""

    async def __call__(self, cmd: list[str]) -> tuple[str, str, int]:
        """Run command and return (stdout, stderr, returncode)."""
        ...


# Default command runner using tmux module
async def default_runner(cmd: list[str]) -> tuple[str, str, int]:
    """Default command runner using anyio."""
    return await tmux.run(cmd)


# =============================================================================
# Tmux Worker Detection
# =============================================================================


async def get_live_workers(
    short_id: str,  # noqa: ARG001
    tmux_session: str,
) -> dict[str, str]:
    """Get live workers as {issue_id: mode} from tmux windows.

    Workers run as windows within the controller's tmux session.
    Window naming convention: {mode}-{issue_id} (e.g., "implement-ENG-21").

    Args:
        short_id: Short project ID (unused, kept for API compatibility)
        tmux_session: Name of the tmux session containing worker windows

    Returns:
        Dict mapping issue_id (uppercase) to mode
    """
    _ = short_id  # Explicitly mark as intentionally unused
    windows = await tmux.list_windows(tmux_session)
    workers: dict[str, str] = {}

    for window in windows:
        if window == "main":
            continue

        parts = window.split("-", 1)
        if len(parts) == 2:
            mode, issue_id = parts
            workers[issue_id.upper()] = mode
            logger.debug("Found live worker for %s in mode %s", issue_id.upper(), mode)

    logger.debug("Found %d live workers in session %s", len(workers), tmux_session)
    return workers


# =============================================================================
# GitHub PR Draft Status Fetching
# =============================================================================


class PRDraftData(TypedDict, total=False):
    """GraphQL response for a single PR's draft status."""

    isDraft: bool


class GraphQLResponse(TypedDict, total=False):
    """Top-level GraphQL response structure."""

    data: dict[str, dict[str, PRDraftData | None]]


class GitHubAPIError(Exception):
    """Raised when GitHub API calls fail after retries."""


@retry(
    stop=stop_after_attempt(3),
    wait=wait_exponential(multiplier=1, min=1, max=10),
    retry=retry_if_exception_type(GitHubAPIError),
    reraise=True,
)
async def get_pr_draft_status_batch(
    pr_refs: dict[str, types.GitHubPRRef],
    *,
    runner: CommandRunner = default_runner,
) -> dict[str, bool | None]:
    """Fetch PR draft status for multiple issues in a single GraphQL query.

    Batches all PRs across all repositories into one API call.

    Args:
        pr_refs: Dict mapping issue_id to GitHubPRRef
        runner: Command runner for testing

    Returns:
        Dict mapping issue_id to:
        - True: PR is draft (changes requested)
        - False: PR is ready (approved)
        - None: PR not found

    Raises:
        GitHubAPIError: If GraphQL query fails after retries
    """
    if not pr_refs:
        return {}

    # Group by repository for query structure
    by_repo: dict[tuple[str, str], list[tuple[str, int]]] = {}
    for issue_id, ref in pr_refs.items():
        key = (ref.owner, ref.repo)
        by_repo.setdefault(key, []).append((issue_id, ref.number))

    # Build single GraphQL query for all repos and PRs
    # Maps: repo_alias -> (owner, repo), pr_alias -> issue_id
    repo_alias_map: dict[str, tuple[str, str]] = {}
    pr_alias_map: dict[
        str, dict[str, tuple[str, int]]
    ] = {}  # repo_alias -> {pr_alias -> (issue_id, pr_number)}

    query_parts: list[str] = []
    for repo_idx, ((owner, repo), issue_prs) in enumerate(by_repo.items()):
        repo_alias = f"repo{repo_idx}"
        repo_alias_map[repo_alias] = (owner, repo)
        pr_alias_map[repo_alias] = {}

        pr_parts: list[str] = []
        for pr_idx, (issue_id, pr_number) in enumerate(issue_prs):
            pr_alias = f"pr{pr_idx}"
            pr_alias_map[repo_alias][pr_alias] = (issue_id, pr_number)
            pr_parts.append(
                f"{pr_alias}: pullRequest(number: {pr_number}) {{ isDraft }}"
            )

        query_parts.append(
            f'{repo_alias}: repository(owner: "{owner}", name: "{repo}") {{ {" ".join(pr_parts)} }}'
        )

    query = f"query {{ {' '.join(query_parts)} }}"

    logger.debug(
        "Fetching PR draft status for %d issues across %d repos",
        len(pr_refs),
        len(by_repo),
    )

    stdout, stderr, rc = await runner(["gh", "api", "graphql", "-f", f"query={query}"])

    if rc != 0:
        logger.error("GraphQL query failed: %s", stderr)
        raise GitHubAPIError(f"GraphQL query failed: {stderr}")

    try:
        response: GraphQLResponse = json.loads(stdout)
    except json.JSONDecodeError as e:
        logger.error("Failed to parse GraphQL response: %s", e)
        raise GitHubAPIError(f"Failed to parse GraphQL response: {e}") from e

    data_obj: dict[str, dict[str, PRDraftData | None]] = response.get("data") or {}
    result: dict[str, bool | None] = {}

    # Parse response using alias maps
    for repo_alias, (owner, repo) in repo_alias_map.items():
        repo_data: dict[str, PRDraftData | None] = data_obj.get(repo_alias) or {}

        for pr_alias, (issue_id, pr_number) in pr_alias_map[repo_alias].items():
            pr_data: PRDraftData | None = repo_data.get(pr_alias)
            if pr_data is not None and "isDraft" in pr_data:
                # Convert None to False for safety (null isDraft means not draft)
                result[issue_id] = bool(pr_data["isDraft"])
                logger.debug(
                    "Issue %s PR #%d isDraft: %s", issue_id, pr_number, result[issue_id]
                )
            else:
                result[issue_id] = None
                logger.warning(
                    "Issue %s PR #%d not found in %s/%s",
                    issue_id,
                    pr_number,
                    owner,
                    repo,
                )

    return result


# =============================================================================
# Issue Parsing
# =============================================================================


def parse_linear_issues(
    linear_issues: Sequence[LinearIssueRaw],
) -> list[ParsedIssue]:
    """Parse Linear API response into structured data.

    Args:
        linear_issues: Raw issue dicts from Linear API

    Returns:
        List of parsed issues with normalized data
    """
    parsed: list[ParsedIssue] = []

    for issue in linear_issues:
        issue_id = issue.get("identifier", "")
        if not issue_id:
            continue

        # Extract and normalize status
        # Linear MCP returns "status" as string, but raw API might return "state.name"
        raw_status = issue.get("status", "")
        if not raw_status:
            state_obj = issue.get("state")
            raw_status = state_obj.get("name", "") if state_obj else ""
        status = types.IssueStatus.normalize(raw_status)

        # Extract labels
        # Linear MCP returns "labels" as list of strings, raw API might return "labels.nodes"
        # Note: isinstance checks below are defensive guards against malformed API responses.
        # basedpyright reports them as unnecessary based on TypedDicts, but the API can
        # return unexpected shapes (nulls, wrong types) that the types don't capture.
        labels_raw = issue.get("labels", [])
        labels: list[str] = []
        if isinstance(labels_raw, dict):
            # Raw API format: {"nodes": [{"name": "label1"}, ...]}
            labels = [
                node["name"]
                for node in labels_raw.get("nodes") or []
                if isinstance(node, dict) and node.get("name")  # pyright: ignore[reportUnnecessaryIsInstance]
            ]
        elif isinstance(labels_raw, list):  # pyright: ignore[reportUnnecessaryIsInstance]
            # MCP format: ["label1", "label2"] - filter out empty/non-string values
            labels = [x for x in labels_raw if isinstance(x, str) and x]  # pyright: ignore[reportUnnecessaryIsInstance]

        # Extract PR reference from attachments
        # Linear MCP returns attachments with PR URLs like https://github.com/owner/repo/pull/123
        pr_ref: types.GitHubPRRef | None = None
        attachments = issue.get("attachments") or []
        if not isinstance(attachments, list):  # pyright: ignore[reportUnnecessaryIsInstance]
            attachments = []
        for attachment in attachments:
            if isinstance(attachment, dict):  # pyright: ignore[reportUnnecessaryIsInstance]
                url = attachment.get("url", "")
                if "github.com" in url and "/pull/" in url:
                    pr_ref = types.GitHubPRRef.from_url(url)
                    if pr_ref:
                        break

        parsed.append(
            ParsedIssue(
                issue_id=issue_id,
                status=status,
                labels=labels,
                pr_ref=pr_ref,
            )
        )

    logger.debug("Parsed %d issues from Linear", len(parsed))
    return parsed


# =============================================================================
# Main Data Fetching
# =============================================================================


async def fetch_all_issue_data(
    linear_issues: Sequence[LinearIssueRaw],
    short_id: str,
    tmux_session: str,
    *,
    runner: CommandRunner = default_runner,
) -> list[FetchedIssueData]:
    """Fetch all data for issues in parallel.

    All I/O operations run concurrently in a single task group:
    - Tmux window list (for live workers)
    - GitHub PR draft status (fetched per-repo based on PR attachments)

    Args:
        linear_issues: Raw issue dicts from Linear API
        short_id: Short project ID for tmux sessions
        tmux_session: Name of the tmux session containing worker windows
        runner: Command runner for testing

    Returns:
        List of fully fetched issue data
    """
    # Phase 1: Parse issues (sync, fast)
    parsed_issues = parse_linear_issues(linear_issues)

    # Identify PRs that need draft status lookup
    pr_refs_for_status: dict[str, types.GitHubPRRef] = {
        p.issue_id: p.pr_ref
        for p in parsed_issues
        if p.needs_pr_status and p.pr_ref is not None
    }

    # Phase 2: Fetch everything in parallel
    live_workers: dict[str, str] = {}
    pr_draft_map: dict[str, bool | None] = {}

    async def fetch_workers() -> None:
        nonlocal live_workers
        live_workers = await get_live_workers(short_id, tmux_session)

    async def fetch_pr_draft_status() -> None:
        nonlocal pr_draft_map
        if pr_refs_for_status:
            pr_draft_map = await get_pr_draft_status_batch(
                pr_refs_for_status, runner=runner
            )

    async with anyio.create_task_group() as tg:
        tg.start_soon(fetch_workers)
        tg.start_soon(fetch_pr_draft_status)

    # Phase 3: Build results
    results: list[FetchedIssueData] = []

    for issue in parsed_issues:
        has_live_worker = issue.issue_id.upper() in live_workers
        pr_is_draft: bool | None = pr_draft_map.get(issue.issue_id)

        results.append(
            FetchedIssueData(
                issue_id=issue.issue_id,
                status=issue.status,
                labels=issue.labels,
                pr_is_draft=pr_is_draft,
                has_live_worker=has_live_worker,
                has_user_feedback=issue.has_user_feedback,
                has_user_input_needed=issue.has_user_input_needed,
            )
        )

    return results
