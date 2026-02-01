"""Tests for state module."""

import json
import uuid
from pathlib import Path
from unittest.mock import patch, AsyncMock

import pytest

from legion.state import decision, fetch, types


class TestComputeSessionId:
    """Test deterministic session ID generation."""

    def test_returns_uuid_string(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result = types.compute_session_id(team_id, "ENG-21", "implement")
        parsed = uuid.UUID(result)
        assert str(parsed) == result

    def test_same_inputs_same_output(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result1 = types.compute_session_id(team_id, "ENG-21", "implement")
        result2 = types.compute_session_id(team_id, "ENG-21", "implement")
        assert result1 == result2

    def test_different_issue_different_output(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result1 = types.compute_session_id(team_id, "ENG-21", "implement")
        result2 = types.compute_session_id(team_id, "ENG-22", "implement")
        assert result1 != result2

    def test_different_mode_different_output(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result1 = types.compute_session_id(team_id, "ENG-21", "implement")
        result2 = types.compute_session_id(team_id, "ENG-21", "review")
        assert result1 != result2

    def test_raises_value_error_for_invalid_team_id(self) -> None:
        with pytest.raises(ValueError):
            types.compute_session_id("not-a-valid-uuid", "ENG-21", "implement")


class TestIssueStatus:
    """Test status normalization."""

    def test_normalize_direct_match(self) -> None:
        assert types.IssueStatus.normalize("Todo") == "Todo"
        assert types.IssueStatus.normalize("In Progress") == "In Progress"

    def test_normalize_alias(self) -> None:
        assert types.IssueStatus.normalize("In Review") == "Needs Review"

    def test_normalize_unknown(self) -> None:
        assert types.IssueStatus.normalize("Unknown") == "Unknown"


class TestGetLiveWorkers:
    """Test detecting running worker sessions."""

    @pytest.mark.anyio
    async def test_returns_issue_ids_from_worker_sessions(self) -> None:
        with patch("legion.state.fetch.get_tmux_sessions", new_callable=AsyncMock) as mock:
            mock.return_value = [
                "legion-abc123-worker-ENG-21",
                "legion-abc123-worker-ENG-22",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21", "ENG-22"}

    @pytest.mark.anyio
    async def test_ignores_other_sessions(self) -> None:
        with patch("legion.state.fetch.get_tmux_sessions", new_callable=AsyncMock) as mock:
            mock.return_value = [
                "legion-abc123-controller",
                "legion-abc123-worker-ENG-21",
                "other-session",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21"}

    @pytest.mark.anyio
    async def test_normalizes_lowercase_to_uppercase(self) -> None:
        with patch("legion.state.fetch.get_tmux_sessions", new_callable=AsyncMock) as mock:
            mock.return_value = [
                "legion-abc123-worker-eng-21",
                "legion-abc123-worker-Eng-22",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21", "ENG-22"}


class TestGetPrDraftStatusBatch:
    """Test GitHub PR draft status batch fetching."""

    @pytest.mark.anyio
    async def test_returns_draft_status_for_multiple_issues(self) -> None:
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
            response = {
                "data": {
                    "repository": {
                        "pr0": {"isDraft": True},
                        "pr1": {"isDraft": False},
                    }
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {
                "ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1),
                "ENG-22": types.GitHubPRRef(owner="owner", repo="repo", number=2),
            },
            runner=mock_runner,
        )
        assert result == {"ENG-21": True, "ENG-22": False}

    @pytest.mark.anyio
    async def test_skips_issues_with_missing_pr(self) -> None:
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
            response = {
                "data": {
                    "repository": {
                        "pr0": None,  # PR not found
                    }
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=999)},
            runner=mock_runner,
        )
        assert result == {}

    @pytest.mark.anyio
    async def test_handles_command_failure(self) -> None:
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
            return "", "error", 1

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )
        assert result == {}


class TestCheckWorkerBlocked:
    """Test detecting blocked workers from session files."""

    @pytest.mark.anyio
    async def test_not_blocked_when_no_ask_user_question(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.write_text(
            '{"type":"assistant","message":{"content":"Hello"}}\n'
            '{"type":"user","message":{"content":"Hi"}}\n'
        )
        blocked, question = await fetch.check_worker_blocked(session_file)
        assert blocked is False
        assert question is None

    @pytest.mark.anyio
    async def test_blocked_when_ask_user_question_pending(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.write_text(
            '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"question":"Should I proceed?"}}]}}\n'
        )
        blocked, question = await fetch.check_worker_blocked(session_file)
        assert blocked is True
        assert question == "Should I proceed?"

    @pytest.mark.anyio
    async def test_not_blocked_when_answered(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.write_text(
            '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"question":"Should I proceed?"}}]}}\n'
            '{"type":"user","message":{"content":"Yes"}}\n'
        )
        blocked, question = await fetch.check_worker_blocked(session_file)
        assert blocked is False

    @pytest.mark.anyio
    async def test_returns_false_for_nonexistent_file(self, tmp_path: Path) -> None:
        session_file = tmp_path / "nonexistent.jsonl"
        blocked, question = await fetch.check_worker_blocked(session_file)
        assert blocked is False


class TestParseLinearIssues:
    """Test Linear issue parsing."""

    def test_parses_basic_issue(self) -> None:
        issues = [
            {
                "identifier": "ENG-21",
                "state": {"name": "In Progress"},
                "labels": {"nodes": [{"name": "worker-done"}]},
            }
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1
        assert result[0].issue_id == "ENG-21"
        assert result[0].status == "In Progress"
        assert result[0].has_worker_done is True

    def test_normalizes_status(self) -> None:
        issues = [{"identifier": "ENG-21", "state": {"name": "In Review"}, "labels": {"nodes": []}}]
        result = fetch.parse_linear_issues(issues)
        assert result[0].status == "Needs Review"

    def test_skips_issues_without_identifier(self) -> None:
        issues = [
            {"state": {"name": "Todo"}, "labels": {"nodes": []}},
            {"identifier": "ENG-21", "state": {"name": "Todo"}, "labels": {"nodes": []}},
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1

    def test_handles_null_state(self) -> None:
        issues = [{"identifier": "ENG-21", "state": None, "labels": {"nodes": []}}]
        result = fetch.parse_linear_issues(issues)
        assert result[0].status == ""

    def test_handles_null_labels(self) -> None:
        issues = [{"identifier": "ENG-21", "state": {"name": "Todo"}, "labels": None}]
        result = fetch.parse_linear_issues(issues)
        assert result[0].labels == []


class TestSuggestAction:
    """Test action suggestion based on state."""

    def test_todo_no_worker_done_no_live_worker(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.TODO,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "dispatch_planner"

    def test_todo_worker_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.TODO,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "transition_to_in_progress"

    def test_in_progress_no_worker_no_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "dispatch_implementer"

    def test_in_progress_worker_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "dispatch_reviewer"

    def test_in_progress_with_live_worker(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "skip"

    def test_needs_review_approved(self) -> None:
        """PR is ready (not draft) = approved."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=False,  # PR is ready = approved
            is_blocked=False,
        )
        assert action == "transition_to_retro"

    def test_needs_review_changes_requested(self) -> None:
        """PR is draft = changes requested."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=True,  # PR is draft = changes requested
            is_blocked=False,
        )
        assert action == "resume_implementer_for_changes"

    def test_needs_review_no_pr_skips(self) -> None:
        """No PR yet = wait."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,  # No PR
            is_blocked=False,
        )
        assert action == "skip"

    def test_blocked_escalates(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            is_blocked=True,
        )
        assert action == "escalate_blocked"


class TestBuildIssueState:
    """Test building issue state from fetched data."""

    def test_builds_state_with_action(self) -> None:
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="Todo",
            labels=[],
            pr_is_draft=None,
            has_live_worker=False,
            is_blocked=False,
            blocked_question=None,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        assert state.suggested_action == "dispatch_planner"

    def test_skips_when_user_input_needed(self) -> None:
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="Todo",
            labels=["user-input-needed"],
            pr_is_draft=None,
            has_live_worker=False,
            is_blocked=False,
            blocked_question=None,
            has_user_feedback=False,
            has_user_input_needed=True,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        assert state.suggested_action == "skip"


class TestFetchAllIssueData:
    """Test full data fetching."""

    @pytest.mark.anyio
    async def test_fetches_data_for_issues(self) -> None:
        with (
            patch("legion.state.fetch.get_live_workers", new_callable=AsyncMock) as mock_workers,
            patch("legion.state.fetch.get_pr_draft_status_batch", new_callable=AsyncMock) as mock_draft,
        ):
            mock_workers.return_value = {"ENG-21"}
            mock_draft.return_value = {}

            linear_issues = [
                {
                    "identifier": "ENG-21",
                    "state": {"name": "In Progress"},
                    "labels": {"nodes": []},
                }
            ]

            result = await fetch.fetch_all_issue_data(
                linear_issues=linear_issues,
                team_id="7b4f0862-b775-4cb0-9a67-85400c6f44a8",
                short_id="abc123",
            )

            assert len(result) == 1
            assert result[0].issue_id == "ENG-21"
            assert result[0].has_live_worker is True


class TestBuildCollectedState:
    """Test building full collected state."""

    def test_builds_state_for_multiple_issues(self) -> None:
        issues_data = [
            types.FetchedIssueData(
                issue_id="ENG-21",
                status="Todo",
                labels=[],
                pr_is_draft=None,
                has_live_worker=False,
                is_blocked=False,
                blocked_question=None,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
            types.FetchedIssueData(
                issue_id="ENG-22",
                status="In Progress",
                labels=["worker-done"],
                pr_is_draft=None,
                has_live_worker=False,
                is_blocked=False,
                blocked_question=None,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
        ]

        state = decision.build_collected_state(issues_data, "00000000-0000-0000-0000-000000000000")

        assert "ENG-21" in state.issues
        assert "ENG-22" in state.issues
        assert state.issues["ENG-21"].suggested_action == "dispatch_planner"
        assert state.issues["ENG-22"].suggested_action == "dispatch_reviewer"

    def test_to_dict_serializes_correctly(self) -> None:
        issues_data = [
            types.FetchedIssueData(
                issue_id="ENG-21",
                status="Todo",
                labels=[],
                pr_is_draft=None,
                has_live_worker=False,
                is_blocked=False,
                blocked_question=None,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
        ]

        state = decision.build_collected_state(issues_data, "00000000-0000-0000-0000-000000000000")
        result = state.to_dict()

        assert "issues" in result
        assert "ENG-21" in result["issues"]
        assert result["issues"]["ENG-21"]["suggested_action"] == "dispatch_planner"
