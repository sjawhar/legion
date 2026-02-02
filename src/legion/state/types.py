"""Type definitions for Legion state collection.

Contains:
- Dataclasses for internal data structures
- TypedDicts for external API response shapes
- Enums and constants for status normalization
"""

from __future__ import annotations

import re
import uuid
from dataclasses import dataclass, field
from typing import Literal, TypedDict


# =============================================================================
# Status Constants and Normalization
# =============================================================================

# Literal types for compile-time safety
IssueStatusLiteral = Literal[
    "Triage",
    "Icebox",
    "Backlog",
    "Todo",
    "In Progress",
    "Needs Review",
    "Retro",
    "Done",
]
WorkerModeLiteral = Literal["architect", "plan", "implement", "review", "merge"]


class IssueStatus:
    """Canonical issue status values with normalization."""

    TRIAGE: IssueStatusLiteral = "Triage"
    ICEBOX: IssueStatusLiteral = "Icebox"
    BACKLOG: IssueStatusLiteral = "Backlog"
    TODO: IssueStatusLiteral = "Todo"
    IN_PROGRESS: IssueStatusLiteral = "In Progress"
    NEEDS_REVIEW: IssueStatusLiteral = "Needs Review"
    RETRO: IssueStatusLiteral = "Retro"
    DONE: IssueStatusLiteral = "Done"

    # Map Linear's status names to our canonical names
    ALIASES: dict[str, IssueStatusLiteral] = {
        "In Review": NEEDS_REVIEW,
    }

    @classmethod
    def normalize(cls, raw: str | None) -> IssueStatusLiteral | str:
        """Normalize a raw status string to canonical form.

        Returns the canonical IssueStatusLiteral if the raw value matches
        a known alias, otherwise returns the original string unchanged.
        Returns empty string if raw is None.
        """
        if raw is None:
            return ""
        return cls.ALIASES.get(raw, raw)


class WorkerMode:
    """Worker mode constants for session ID computation."""

    ARCHITECT: WorkerModeLiteral = "architect"
    PLAN: WorkerModeLiteral = "plan"
    IMPLEMENT: WorkerModeLiteral = "implement"
    REVIEW: WorkerModeLiteral = "review"
    MERGE: WorkerModeLiteral = "merge"


# =============================================================================
# Action Types
# =============================================================================

ActionType = Literal[
    "skip",
    "dispatch_architect",
    "dispatch_planner",
    "dispatch_implementer",
    "dispatch_reviewer",
    "dispatch_merger",
    "resume_implementer_for_changes",
    "resume_implementer_for_retro",
    "transition_to_in_progress",
    "transition_to_needs_review",
    "transition_to_retro",
    "transition_to_todo",
    "relay_user_feedback",
    "remove_worker_active_and_redispatch",
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


class LinearAttachment(TypedDict, total=False):
    """Linear attachment object."""

    url: str


class LinearIssueRaw(TypedDict, total=False):
    """Raw Linear issue from API (MCP or GraphQL).

    Uses total=False since different APIs return different fields.
    The identifier field is required for valid issues.
    """

    identifier: str
    status: str  # MCP format: status as string
    state: LinearStateDict  # GraphQL format: state.name
    labels: list[str] | LinearLabelsContainer  # MCP: list[str], GraphQL: {nodes: [...]}
    attachments: list[LinearAttachment]


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


class TextContent(TypedDict):
    """Text content item in assistant message."""

    type: Literal["text"]
    text: str


class AssistantMessage(TypedDict):
    """Assistant message structure."""

    content: list[ToolUseContent | TextContent]


class SessionEntry(TypedDict, total=False):
    """Entry in Claude session JSONL file.

    Note: `message` is only present for assistant entries, not user entries.
    Using total=False since not all fields are present in all entry types.
    """

    type: Literal["assistant", "user"]  # Required via runtime check
    message: AssistantMessage  # Only present for assistant type


# =============================================================================
# Internal Dataclasses
# =============================================================================


@dataclass(frozen=True)
class GitHubPRRef:
    """Parsed GitHub PR reference from URL (immutable value object)."""

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
        # Validate URL format and owner/repo characters (alphanumeric, hyphen, underscore, dot)
        match = re.match(r"https://github\.com/([\w.-]+)/([\w.-]+)/pull/(\d+)", url)
        if not match:
            return None
        pr_number = int(match.group(3))
        # Guard against unreasonably large PR numbers (GraphQL uses 32-bit int)
        if pr_number > 2_147_483_647:
            return None
        return cls(owner=match.group(1), repo=match.group(2), number=pr_number)


@dataclass
class ParsedIssue:
    """Parsed issue data from Linear API response."""

    issue_id: str
    status: IssueStatusLiteral | str  # Canonical status or unknown raw value
    labels: list[str]
    pr_ref: GitHubPRRef | None = None

    @property
    def has_worker_done(self) -> bool:
        """Whether this issue has the worker-done label."""
        return "worker-done" in self.labels

    @property
    def has_user_feedback(self) -> bool:
        """Whether this issue has the user-feedback-given label."""
        return "user-feedback-given" in self.labels

    @property
    def has_user_input_needed(self) -> bool:
        """Whether this issue has the user-input-needed label."""
        return "user-input-needed" in self.labels

    @property
    def has_worker_active(self) -> bool:
        """Whether this issue has the worker-active label."""
        return "worker-active" in self.labels

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
    status: IssueStatusLiteral | str  # Canonical status or unknown raw value
    labels: list[str]
    pr_is_draft: bool | None  # None if no PR, True if draft, False if ready
    has_live_worker: bool
    has_user_feedback: bool
    has_user_input_needed: bool


class IssueStateDict(TypedDict):
    """Serialized form of IssueState."""

    status: IssueStatusLiteral | str
    labels: list[str]
    pr_is_draft: bool | None
    has_live_worker: bool
    suggested_action: ActionType
    session_id: str
    has_user_feedback: bool


class CollectedStateDict(TypedDict):
    """Serialized form of CollectedState."""

    issues: dict[str, IssueStateDict]


@dataclass
class IssueState:
    """Final state for an issue with suggested action."""

    status: IssueStatusLiteral | str  # Canonical status or unknown raw value
    labels: list[str]
    pr_is_draft: bool | None  # None if no PR, True if draft, False if ready
    has_live_worker: bool
    suggested_action: ActionType
    session_id: str
    has_user_feedback: bool

    def to_dict(self) -> IssueStateDict:
        """Convert to dictionary for JSON serialization."""
        return {
            "status": self.status,
            "labels": self.labels,
            "pr_is_draft": self.pr_is_draft,
            "has_live_worker": self.has_live_worker,
            "suggested_action": self.suggested_action,
            "session_id": self.session_id,
            "has_user_feedback": self.has_user_feedback,
        }


@dataclass
class CollectedState:
    """Complete state collection result."""

    issues: dict[str, IssueState] = field(default_factory=dict)

    def to_dict(self) -> CollectedStateDict:
        """Convert to dictionary for JSON serialization."""
        return {"issues": {k: v.to_dict() for k, v in self.issues.items()}}


# =============================================================================
# Utility Functions
# =============================================================================


def compute_session_id(team_id: str, issue_id: str, mode: WorkerModeLiteral) -> str:
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


def compute_controller_session_id(team_id: str) -> str:
    """Compute deterministic session ID for controller.

    Args:
        team_id: Linear team UUID (must be valid UUID string)

    Returns:
        UUID string for the controller session

    Raises:
        ValueError: If team_id is not a valid UUID string
    """
    namespace = uuid.UUID(team_id)
    return str(uuid.uuid5(namespace, "controller"))
