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
    IssueStatusLiteral,
    WorkerMode,
    WorkerModeLiteral,
)


def suggest_action(
    status: IssueStatusLiteral | str,
    has_worker_done: bool,
    has_live_worker: bool,
    pr_is_draft: bool | None,
) -> ActionType:
    """Suggest action based on issue state.

    Args:
        status: Normalized issue status (IssueStatusLiteral or unknown raw value)
        has_worker_done: Whether issue has worker-done label
        has_live_worker: Whether a tmux worker session is running
        pr_is_draft: PR draft status (None if no PR, True if draft, False if ready)

    Returns:
        Action to take for this issue
    """
    match status:
        case IssueStatus.DONE:
            return "skip"

        case IssueStatus.TRIAGE | IssueStatus.ICEBOX:
            # Controller handles triage routing and Icebox pulls directly
            return "skip"

        case IssueStatus.BACKLOG:
            if has_worker_done:
                return "transition_to_todo"
            if has_live_worker:
                return "skip"
            return "dispatch_architect"

        case IssueStatus.TODO:
            if has_worker_done:
                return "transition_to_in_progress"
            if has_live_worker:
                return "skip"
            return "dispatch_planner"

        case IssueStatus.IN_PROGRESS:
            if has_worker_done:
                return "transition_to_needs_review"
            if has_live_worker:
                return "skip"
            return "dispatch_implementer"

        case IssueStatus.NEEDS_REVIEW:
            if has_worker_done:
                # Review outcome is signaled by PR draft status:
                # - PR ready (not draft) = approved → transition to retro
                # - PR still draft = changes requested → resume implementer
                # - No PR = wait for PR to be created
                if pr_is_draft is None:
                    # No PR yet - wait for it
                    return "skip"
                if pr_is_draft:
                    # PR is draft = changes requested
                    return "resume_implementer_for_changes"
                # PR is ready (not draft) = approved
                return "transition_to_retro"
            if has_live_worker:
                return "skip"
            return "dispatch_reviewer"

        case IssueStatus.RETRO:
            if has_worker_done:
                return "dispatch_merger"
            if has_live_worker:
                return "skip"
            return "resume_implementer_for_retro"

        case _:
            return "skip"


# Direct mapping from action to worker mode
ACTION_TO_MODE: dict[ActionType, WorkerModeLiteral] = {
    "skip": WorkerMode.IMPLEMENT,  # default
    "dispatch_architect": WorkerMode.ARCHITECT,
    "dispatch_planner": WorkerMode.PLAN,
    "dispatch_implementer": WorkerMode.IMPLEMENT,
    "dispatch_reviewer": WorkerMode.REVIEW,
    "dispatch_merger": WorkerMode.MERGE,
    "resume_implementer_for_changes": WorkerMode.IMPLEMENT,
    "resume_implementer_for_retro": WorkerMode.IMPLEMENT,
    "transition_to_in_progress": WorkerMode.IMPLEMENT,
    "transition_to_needs_review": WorkerMode.REVIEW,
    "transition_to_todo": WorkerMode.PLAN,
    "transition_to_retro": WorkerMode.IMPLEMENT,
    "relay_user_feedback": WorkerMode.IMPLEMENT,
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
    # Workers self-escalate: they post to Linear, add user-input-needed label, then exit.
    # When user responds, user-feedback-given label is added.
    if data.has_user_input_needed and data.has_user_feedback:
        # Worker asked for input and user responded - resume worker to check Linear comments
        action: ActionType = "relay_user_feedback"
    elif data.has_user_input_needed:
        # Worker asked for input but user hasn't responded yet - skip until they respond
        action = "skip"
    else:
        action = suggest_action(
            status=data.status,
            has_worker_done="worker-done" in data.labels,
            has_live_worker=data.has_live_worker,
            pr_is_draft=data.pr_is_draft,
        )

    # Compute session_id based on the action's mode
    mode = ACTION_TO_MODE[action]
    session_id = compute_session_id(team_id, data.issue_id, mode)

    return IssueState(
        status=data.status,
        labels=data.labels,
        pr_is_draft=data.pr_is_draft,
        has_live_worker=data.has_live_worker,
        suggested_action=action,
        session_id=session_id,
        has_user_feedback=data.has_user_feedback,
    )


def build_collected_state(
    issues_data: list[FetchedIssueData], team_id: str
) -> CollectedState:
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
