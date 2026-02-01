"""Async data fetching for Legion state collection.

All I/O operations are async and can be composed in a single task group.
Uses:
- anyio for subprocess execution
- aiofiles for file I/O
- async_lru for caching
"""

from __future__ import annotations

import json
import logging
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any, Protocol

import aiofiles
import anyio
from async_lru import alru_cache

from legion import tmux
from legion.state.types import (
    FetchedIssueData,
    GitHubPR,
    GitHubPRRef,
    IssueStatus,
    ParsedIssue,
    SessionEntry,
    compute_session_id,
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
# Tmux Session Detection
# =============================================================================


@alru_cache(ttl=2.0)
async def get_tmux_sessions() -> list[str]:
    """Get all tmux session names (cached for 2 seconds)."""
    return await tmux.list_sessions()


async def get_live_workers(
    short_id: str,
    *,
    runner: CommandRunner = default_runner,
) -> set[str]:
    """Get issue IDs of running worker sessions.

    Args:
        short_id: Short project ID for session name prefix
        runner: Command runner for testing

    Returns:
        Set of issue IDs (uppercase) with live workers
    """
    sessions = await get_tmux_sessions()
    prefix = f"legion-{short_id}-worker-"
    workers: set[str] = set()

    for session in sessions:
        if session.startswith(prefix):
            # Normalize to uppercase (Linear uses ENG-21, tmux might use eng-21)
            issue_id = session[len(prefix):].upper()
            workers.add(issue_id)
            logger.debug("Found live worker for %s", issue_id)

    logger.debug("Found %d live workers for project %s", len(workers), short_id)
    return workers


# =============================================================================
# GitHub PR Draft Status Fetching
# =============================================================================


async def get_pr_draft_status_batch(
    pr_refs: dict[str, GitHubPRRef],
    *,
    runner: CommandRunner = default_runner,
) -> dict[str, bool]:
    """Fetch PR draft status for multiple issues using their PR references.

    Groups PRs by repository and fetches isDraft via GraphQL.

    Args:
        pr_refs: Dict mapping issue_id to GitHubPRRef
        runner: Command runner for testing

    Returns:
        Dict mapping issue_id to isDraft boolean
    """
    if not pr_refs:
        return {}

    # Group by repository
    by_repo: dict[tuple[str, str], list[tuple[str, int]]] = {}
    for issue_id, ref in pr_refs.items():
        key = (ref.owner, ref.repo)
        if key not in by_repo:
            by_repo[key] = []
        by_repo[key].append((issue_id, ref.number))

    result: dict[str, bool] = {}

    # Fetch draft status for each repository
    for (owner, repo), issue_prs in by_repo.items():
        # Build GraphQL query for all PRs in this repo
        query_parts = []
        for i, (issue_id, pr_number) in enumerate(issue_prs):
            query_parts.append(f'''
                pr{i}: pullRequest(number: {pr_number}) {{
                    isDraft
                }}
            ''')

        query = f'''
            query {{
                repository(owner: "{owner}", name: "{repo}") {{
                    {" ".join(query_parts)}
                }}
            }}
        '''

        logger.debug("Fetching PR draft status for %d issues from %s/%s", len(issue_prs), owner, repo)

        stdout, stderr, rc = await runner([
            "gh", "api", "graphql", "-f", f"query={query}"
        ])

        if rc != 0:
            logger.warning("GraphQL query failed for %s/%s: %s", owner, repo, stderr)
            continue

        try:
            data = json.loads(stdout)
            repo_data = data.get("data", {}).get("repository", {})

            for i, (issue_id, _) in enumerate(issue_prs):
                pr_data = repo_data.get(f"pr{i}")
                if pr_data and "isDraft" in pr_data:
                    is_draft = pr_data["isDraft"]
                    result[issue_id] = bool(is_draft)
                    logger.debug("Issue %s PR isDraft: %s", issue_id, result[issue_id])
                else:
                    logger.debug("Issue %s PR not found or missing isDraft", issue_id)

        except (json.JSONDecodeError, KeyError, TypeError) as e:
            logger.warning("Failed to parse GraphQL response for %s/%s: %s", owner, repo, e)

    return result


# =============================================================================
# Session File Reading (Blocked Worker Detection)
# =============================================================================


def _extract_ask_user_question(entry: SessionEntry) -> str | None:
    """Extract question text from an AskUserQuestion tool call."""
    if entry.get("type") != "assistant":
        return None

    message = entry.get("message")
    if not message:
        return None

    content = message.get("content", [])
    if not isinstance(content, list):
        return None

    for item in content:
        if (
            isinstance(item, dict)
            and item.get("type") == "tool_use"
            and item.get("name") == "AskUserQuestion"
        ):
            input_data = item.get("input")
            if isinstance(input_data, dict):
                question = input_data.get("question")
                return question if isinstance(question, str) else None

    return None


async def check_worker_blocked(
    session_file: Path,
    n_lines: int = 10,
) -> tuple[bool, str | None]:
    """Check if a worker session is blocked waiting for user input.

    Reads only the last N lines and scans from the end for efficiency.

    Args:
        session_file: Path to the session JSONL file
        n_lines: Number of lines to read from the end

    Returns:
        Tuple of (is_blocked, question_text)
    """
    if not session_file.exists():
        return (False, None)

    try:
        async with aiofiles.open(session_file, mode="rb") as f:
            # Seek to end
            await f.seek(0, 2)
            file_size = await f.tell()

            if file_size == 0:
                return (False, None)

            # Read chunks from the end
            chunk_size = 8192
            position = file_size
            buffer = b""
            lines: list[bytes] = []

            while position > 0 and len(lines) < n_lines + 1:
                read_size = min(chunk_size, position)
                position -= read_size
                await f.seek(position)
                chunk = await f.read(read_size)
                buffer = chunk + buffer

                # Extract complete lines
                parts = buffer.split(b"\n")
                if position > 0:
                    # Keep incomplete first line in buffer
                    buffer = parts[0]
                    lines = parts[1:] + lines
                else:
                    lines = parts + lines

            # Take last n_lines, decode, filter empty
            text_lines = [
                line.decode("utf-8", errors="replace")
                for line in lines[-n_lines:]
                if line.strip()
            ]

    except OSError as e:
        logger.debug("Failed to read session file %s: %s", session_file, e)
        return (False, None)

    # Scan from end - find last user message or AskUserQuestion
    # Early exit: if we find a user message first, not blocked
    for line in reversed(text_lines):
        try:
            entry: SessionEntry = json.loads(line)
        except json.JSONDecodeError:
            continue

        if entry.get("type") == "user":
            # User responded, not blocked
            return (False, None)

        question = _extract_ask_user_question(entry)
        if question is not None:
            # Found AskUserQuestion with no subsequent user message
            logger.debug("Worker blocked on question: %s", question[:50])
            return (True, question)

    return (False, None)


async def check_worker_blocked_any_mode(
    team_id: str,
    issue_id: str,
    session_dir: Path,
) -> tuple[bool, str | None]:
    """Check if any worker mode is blocked for an issue.

    Args:
        team_id: Linear project UUID
        issue_id: Issue identifier
        session_dir: Directory containing session files

    Returns:
        Tuple of (is_blocked, question_text)
    """
    for mode in ("implement", "review", "finish"):
        session_id = compute_session_id(team_id, issue_id, mode)
        session_file = session_dir / f"{session_id}.jsonl"
        blocked, question = await check_worker_blocked(session_file)
        if blocked:
            return (blocked, question)
    return (False, None)


# =============================================================================
# Issue Parsing
# =============================================================================


def parse_linear_issues(
    linear_issues: Sequence[Mapping[str, Any]],
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
        status = IssueStatus.normalize(raw_status)

        # Extract labels
        # Linear MCP returns "labels" as list of strings, raw API might return "labels.nodes"
        labels_raw = issue.get("labels", [])
        if isinstance(labels_raw, list) and all(isinstance(x, str) for x in labels_raw):
            # MCP format: ["label1", "label2"]
            labels: list[str] = labels_raw
        elif isinstance(labels_raw, dict):
            # Raw API format: {"nodes": [{"name": "label1"}, ...]}
            label_nodes = labels_raw.get("nodes", [])
            labels = [
                node.get("name", "")
                for node in (label_nodes or [])
                if isinstance(node, dict)
            ]
        else:
            labels = []

        # Extract PR reference from attachments
        # Linear MCP returns attachments with PR URLs like https://github.com/owner/repo/pull/123
        pr_ref: GitHubPRRef | None = None
        attachments = issue.get("attachments", [])
        for attachment in attachments:
            if isinstance(attachment, dict):
                url = attachment.get("url", "")
                if "github.com" in url and "/pull/" in url:
                    pr_ref = GitHubPRRef.from_url(url)
                    if pr_ref:
                        break

        parsed.append(ParsedIssue(
            issue_id=issue_id,
            status=status,
            labels=labels,
            has_worker_done="worker-done" in labels,
            has_user_feedback="user-feedback-given" in labels,
            has_user_input_needed="user-input-needed" in labels,
            pr_ref=pr_ref,
        ))

    logger.debug("Parsed %d issues from Linear", len(parsed))
    return parsed


# =============================================================================
# Main Data Fetching
# =============================================================================


async def fetch_all_issue_data(
    linear_issues: Sequence[Mapping[str, Any]],
    team_id: str,
    short_id: str,
    session_dir: Path | None = None,
    *,
    runner: CommandRunner = default_runner,
) -> list[FetchedIssueData]:
    """Fetch all data for issues in parallel.

    All I/O operations run concurrently in a single task group:
    - Tmux session list (cached)
    - GitHub PR labels (fetched per-repo based on PR attachments)
    - Session file checks (parallel async file reads)

    Args:
        linear_issues: Raw issue dicts from Linear API
        team_id: Linear project UUID
        short_id: Short project ID for tmux sessions
        session_dir: Optional directory for session files
        runner: Command runner for testing

    Returns:
        List of fully fetched issue data
    """
    # Phase 1: Parse issues (sync, fast)
    parsed_issues = parse_linear_issues(linear_issues)

    # Identify PRs that need draft status lookup
    pr_refs_for_status: dict[str, GitHubPRRef] = {
        p.issue_id: p.pr_ref
        for p in parsed_issues
        if p.needs_pr_status and p.pr_ref is not None
    }

    # Phase 2: Fetch everything in parallel
    live_workers: set[str] = set()
    pr_draft_map: dict[str, bool] = {}
    blocked_map: dict[str, tuple[bool, str | None]] = {}

    async def fetch_workers() -> None:
        nonlocal live_workers
        live_workers = await get_live_workers(short_id, runner=runner)

    async def fetch_pr_draft_status() -> None:
        nonlocal pr_draft_map
        if pr_refs_for_status:
            pr_draft_map = await get_pr_draft_status_batch(pr_refs_for_status, runner=runner)

    async def check_blocked(issue: ParsedIssue) -> None:
        if session_dir:
            result = await check_worker_blocked_any_mode(
                team_id, issue.issue_id, session_dir
            )
            blocked_map[issue.issue_id] = result

    async with anyio.create_task_group() as tg:
        # Start all fetches concurrently
        tg.start_soon(fetch_workers)
        tg.start_soon(fetch_pr_draft_status)

        # Note: We can't check blocked status until we know live workers
        # But we can start preparing session IDs

    # Phase 3: Check blocked status for live workers only
    # (requires knowing which workers are live, so must come after Phase 2)
    # Note: live_workers contains uppercase IDs, so normalize for comparison
    live_worker_issues = [p for p in parsed_issues if p.issue_id.upper() in live_workers]

    if session_dir and live_worker_issues:
        async with anyio.create_task_group() as tg:
            for issue in live_worker_issues:
                tg.start_soon(check_blocked, issue)

    # Phase 4: Build results
    results: list[FetchedIssueData] = []

    for issue in parsed_issues:
        has_live_worker = issue.issue_id.upper() in live_workers
        blocked, blocked_question = blocked_map.get(issue.issue_id, (False, None))

        # PR draft status: None if no PR, True/False if PR exists
        pr_is_draft: bool | None = pr_draft_map.get(issue.issue_id)

        results.append(FetchedIssueData(
            issue_id=issue.issue_id,
            status=issue.status,
            labels=issue.labels,
            pr_is_draft=pr_is_draft,
            has_live_worker=has_live_worker,
            is_blocked=blocked,
            blocked_question=blocked_question,
            has_user_feedback=issue.has_user_feedback,
            has_user_input_needed=issue.has_user_input_needed,
        ))

    return results
