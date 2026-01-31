"""Legion state collection module.

Collects state from tmux, GitHub PRs, and session files,
then determines suggested actions for each issue.

Main entry points:
- `fetch_all_issue_data()` - Fetch all data for issues
- `build_collected_state()` - Build state from fetched data
- `suggest_action()` - Determine action for a single issue

CLI usage:
    echo '$LINEAR_JSON' | python -m legion.state \\
        --project-id UUID --short-id abc123 --owner owner --repo repo
"""

from legion.state.decision import ACTION_TO_MODE, build_collected_state, build_issue_state, suggest_action
from legion.state.fetch import (
    check_worker_blocked,
    fetch_all_issue_data,
    get_live_workers,
    get_pr_labels_batch,
    parse_linear_issues,
)
from legion.state.types import (
    ActionType,
    CollectedState,
    FetchedIssueData,
    IssueState,
    IssueStatus,
    ParsedIssue,
    WorkerMode,
    compute_session_id,
)

__all__ = [
    # Types
    "ActionType",
    "CollectedState",
    "FetchedIssueData",
    "IssueState",
    "IssueStatus",
    "ParsedIssue",
    "WorkerMode",
    # Functions
    "ACTION_TO_MODE",
    "build_collected_state",
    "build_issue_state",
    "check_worker_blocked",
    "compute_session_id",
    "fetch_all_issue_data",
    "get_live_workers",
    "get_pr_labels_batch",
    "parse_linear_issues",
    "suggest_action",
]
