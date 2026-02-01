"""Type definitions for Legion state collection.

Contains:
- Dataclasses for internal data structures
- TypedDicts for external API response shapes
- Enums and constants for status normalization
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any, Literal, TypedDict


# =============================================================================
# Status Constants and Normalization
# =============================================================================


class IssueStatus:
    """Canonical issue status values with normalization."""

    TODO = "Todo"
    IN_PROGRESS = "In Progress"
    NEEDS_REVIEW = "Needs Review"
    RETRO = "Retro"
    DONE = "Done"

    # Map Linear's status names to our canonical names
    ALIASES: dict[str, str] = {
        "In Review": NEEDS_REVIEW,
    }

    @classmethod
    def normalize(cls, raw: str) -> str:
        """Normalize a raw status string to canonical form."""
        return cls.ALIASES.get(raw, raw)


class WorkerMode:
    """Worker mode constants for session ID computation."""

    PLAN: str = "plan"
    IMPLEMENT: str = "implement"
    REVIEW: str = "review"
    FINISH: str = "finish"


# =============================================================================
# Action Types
# =============================================================================

ActionType = Literal[
    "skip",
    "dispatch_planner",
    "dispatch_implementer",
    "dispatch_reviewer",
    "dispatch_finisher",
    "resume_implementer_for_changes",
    "resume_implementer_for_retro",
    "transition_to_in_progress",
    "transition_to_retro",
    "escalate_blocked",
]


# =============================================================================
# External API TypedDicts (documenting what APIs return)
# =============================================================================


class LinearStateDict(TypedDict):
    """Linear issue state object."""

    name: str


class LinearLabelNode(TypedDict):
    """Label node in Linear's GraphQL response."""

    name: str


class LinearLabelsContainer(TypedDict):
    """Labels container with nodes array."""

    nodes: list[LinearLabelNode]


class LinearIssue(TypedDict):
    """Linear issue from GraphQL API."""

    identifier: str
    state: LinearStateDict | None
    labels: LinearLabelsContainer | None


class GitHubLabel(TypedDict):
    """GitHub PR label object."""

    name: str


class GitHubPR(TypedDict):
    """GitHub PR from gh CLI JSON output."""

    labels: list[GitHubLabel] | None


# =============================================================================
# Session File TypedDicts
# =============================================================================


class AskUserQuestionInput(TypedDict):
    """Input for AskUserQuestion tool call."""

    question: str


class ToolUseContent(TypedDict):
    """Tool use content item in assistant message."""

    type: Literal["tool_use"]
    name: str
    input: AskUserQuestionInput


class AssistantMessage(TypedDict):
    """Assistant message structure."""

    content: list[ToolUseContent | dict[str, Any]]


class SessionEntry(TypedDict):
    """Entry in Claude session JSONL file."""

    type: Literal["assistant", "user"]
    message: AssistantMessage


# =============================================================================
# Internal Dataclasses
# =============================================================================


@dataclass
class GitHubPRRef:
    """Parsed GitHub PR reference from URL."""

    owner: str
    repo: str
    number: int

    @classmethod
    def from_url(cls, url: str) -> "GitHubPRRef | None":
        """Parse a GitHub PR URL into a reference.

        Args:
            url: GitHub PR URL like https://github.com/owner/repo/pull/123

        Returns:
            GitHubPRRef or None if URL doesn't match expected format
        """
        import re
        match = re.match(r"https://github\.com/([^/]+)/([^/]+)/pull/(\d+)", url)
        if match:
            return cls(owner=match.group(1), repo=match.group(2), number=int(match.group(3)))
        return None


@dataclass
class ParsedIssue:
    """Parsed issue data from Linear API response."""

    issue_id: str
    status: str
    labels: list[str]
    has_worker_done: bool
    has_user_feedback: bool
    has_user_input_needed: bool
    pr_ref: GitHubPRRef | None = None

    @property
    def needs_pr_status(self) -> bool:
        """Whether this issue needs PR draft status lookup."""
        return (
            self.status == IssueStatus.NEEDS_REVIEW
            and self.has_worker_done
            and self.pr_ref is not None
        )


@dataclass
class FetchedIssueData:
    """Complete fetched data for an issue."""

    issue_id: str
    status: str
    labels: list[str]
    pr_is_draft: bool | None  # None if no PR, True if draft, False if ready
    has_live_worker: bool
    is_blocked: bool
    blocked_question: str | None
    has_user_feedback: bool
    has_user_input_needed: bool


@dataclass
class IssueState:
    """Final state for an issue with suggested action."""

    status: str
    labels: list[str]
    pr_is_draft: bool | None  # None if no PR, True if draft, False if ready
    has_live_worker: bool
    suggested_action: ActionType
    session_id: str
    has_user_feedback: bool
    blocked_question: str | None

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for JSON serialization."""
        return {
            "status": self.status,
            "labels": self.labels,
            "pr_is_draft": self.pr_is_draft,
            "has_live_worker": self.has_live_worker,
            "suggested_action": self.suggested_action,
            "session_id": self.session_id,
            "has_user_feedback": self.has_user_feedback,
            "blocked_question": self.blocked_question,
        }


@dataclass
class CollectedState:
    """Complete state collection result."""

    issues: dict[str, IssueState] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        """Convert to dictionary for JSON serialization."""
        return {
            "issues": {k: v.to_dict() for k, v in self.issues.items()}
        }


# =============================================================================
# Utility Functions
# =============================================================================


def compute_session_id(team_id: str, issue_id: str, mode: str) -> str:
    """Compute deterministic session ID using UUIDv5.

    Args:
        team_id: Linear project UUID
        issue_id: Issue identifier (e.g., "ENG-21")
        mode: Worker mode (e.g., "implement", "review")

    Returns:
        UUID string for the session

    Raises:
        ValueError: If team_id is not a valid UUID string
    """
    namespace = uuid.UUID(team_id)
    name = f"{issue_id}:{mode}"
    return str(uuid.uuid5(namespace, name))
