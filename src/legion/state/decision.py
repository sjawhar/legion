"""Decision logic for Legion state collection.

Pure functions that determine actions based on state.
No I/O operations - just business logic.
"""

from __future__ import annotations

from legion.state.types import (
    ActionType,
    CollectedState,
    compute_session_id,
    FetchedIssueData,
    IssueState,
    IssueStatus,
    WorkerMode,
)


def suggest_action(
    status: str,
    has_worker_done: bool,
    has_live_worker: bool,
    pr_labels: list[str],
    is_blocked: bool,
) -> ActionType:
    """Suggest action based on issue state.

    Args:
        status: Normalized issue status
        has_worker_done: Whether issue has worker-done label
        has_live_worker: Whether a tmux worker session is running
        pr_labels: Labels on the associated PR
        is_blocked: Whether the worker is blocked on user input

    Returns:
        Action to take for this issue
    """
    # Blocked workers always escalate (highest priority)
    if is_blocked:
        return "escalate_blocked"

    match status:
        case IssueStatus.DONE:
            return "skip"

        case IssueStatus.TODO:
            if has_worker_done:
                return "transition_to_in_progress"
            if has_live_worker:
                return "skip"
            return "dispatch_planner"

        case IssueStatus.IN_PROGRESS:
            if has_worker_done:
                return "dispatch_reviewer"
            if has_live_worker:
                return "skip"
            return "dispatch_implementer"

        case IssueStatus.NEEDS_REVIEW:
            if has_worker_done:
                has_changes_requested = "worker-changes-requested" in pr_labels
                has_approved = "worker-approved" in pr_labels

                # Handle conflicting labels: treat as changes requested
                # (controller will remove both labels, implementer will re-evaluate)
                if has_changes_requested and has_approved:
                    return "resume_implementer_for_changes"
                if has_changes_requested:
                    return "resume_implementer_for_changes"
                if has_approved:
                    return "transition_to_retro"
                # No PR label yet - wait for propagation
                return "skip"
            if has_live_worker:
                return "skip"
            return "dispatch_reviewer"

        case IssueStatus.RETRO:
            if has_worker_done:
                return "dispatch_finisher"
            if has_live_worker:
                return "skip"
            return "resume_implementer_for_retro"

        case _:
            return "skip"


# Direct mapping from action to worker mode
ACTION_TO_MODE: dict[ActionType, str] = {
    "skip": WorkerMode.IMPLEMENT,  # default
    "dispatch_planner": WorkerMode.PLAN,
    "dispatch_implementer": WorkerMode.IMPLEMENT,
    "dispatch_reviewer": WorkerMode.REVIEW,
    "dispatch_finisher": WorkerMode.FINISH,
    "resume_implementer_for_changes": WorkerMode.IMPLEMENT,
    "resume_implementer_for_retro": WorkerMode.IMPLEMENT,
    "transition_to_in_progress": WorkerMode.IMPLEMENT,
    "transition_to_retro": WorkerMode.IMPLEMENT,
    "escalate_blocked": WorkerMode.IMPLEMENT,
}


def build_issue_state(data: FetchedIssueData, team_id: str) -> IssueState:
    """Build final issue state with suggested action.

    Args:
        data: Fetched issue data
        team_id: Linear project UUID for session ID computation

    Returns:
        Issue state with suggested action
    """
    # Determine action based on state
    if data.has_user_input_needed:
        action: ActionType = "skip"
    else:
        action = suggest_action(
            status=data.status,
            has_worker_done="worker-done" in data.labels,
            has_live_worker=data.has_live_worker,
            pr_labels=data.pr_labels,
            is_blocked=data.is_blocked,
        )

    # Compute session_id based on the action's mode
    mode = ACTION_TO_MODE[action]
    session_id = compute_session_id(team_id, data.issue_id, mode)

    return IssueState(
        status=data.status,
        labels=data.labels,
        pr_labels=data.pr_labels,
        has_live_worker=data.has_live_worker,
        suggested_action=action,
        session_id=session_id,
        has_user_feedback=data.has_user_feedback,
        blocked_question=data.blocked_question,
    )


def build_collected_state(issues_data: list[FetchedIssueData], team_id: str) -> CollectedState:
    """Build complete collected state from fetched data.

    Args:
        issues_data: List of fetched issue data
        team_id: Linear project UUID for session ID computation

    Returns:
        Complete state with all issues and suggested actions
    """
    result = CollectedState()

    for data in issues_data:
        result.issues[data.issue_id] = build_issue_state(data, team_id)

    return result
