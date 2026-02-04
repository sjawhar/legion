"""Tests for state module."""

import json
import time
import uuid
from unittest import mock

import pytest

from legion.state import decision, fetch, types
from legion.state.types import LinearIssueRaw


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


class TestComputeControllerSessionId:
    """Test controller session ID generation."""

    def test_returns_uuid_string(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result = types.compute_controller_session_id(team_id)
        parsed = uuid.UUID(result)
        assert str(parsed) == result

    def test_same_input_same_output(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result1 = types.compute_controller_session_id(team_id)
        result2 = types.compute_controller_session_id(team_id)
        assert result1 == result2

    def test_different_from_worker_session_id(self) -> None:
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        controller_id = types.compute_controller_session_id(team_id)
        worker_id = types.compute_session_id(team_id, "ENG-21", "implement")
        assert controller_id != worker_id

    def test_raises_value_error_for_invalid_team_id(self) -> None:
        with pytest.raises(ValueError):
            types.compute_controller_session_id("not-a-valid-uuid")


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
    """Test detecting running worker windows."""

    @pytest.mark.anyio
    async def test_returns_issue_ids_and_modes_from_worker_windows(self) -> None:
        """Workers are windows named {mode}-{issue_id}."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = [
                "main",
                "implement-ENG-21",
                "plan-ENG-22",
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            assert result == {"ENG-21": "implement", "ENG-22": "plan"}
            mock_windows.assert_called_once_with("legion-abc123")

    @pytest.mark.anyio
    async def test_skips_main_window(self) -> None:
        """The main window is the controller, not a worker."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = [
                "main",
                "implement-ENG-21",
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            assert result == {"ENG-21": "implement"}
            assert "main" not in result

    @pytest.mark.anyio
    async def test_normalizes_issue_id_to_uppercase(self) -> None:
        """Issue IDs are normalized to uppercase."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = [
                "implement-eng-21",
                "plan-Eng-22",
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            assert result == {"ENG-21": "implement", "ENG-22": "plan"}

    @pytest.mark.anyio
    async def test_ignores_window_names_without_hyphen(self) -> None:
        """Window names without hyphen are ignored (no mode-issue split)."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = [
                "main",
                "implement-ENG-21",
                "invalid",  # Not valid: no hyphen, can't split into mode-issue
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            # Only implement-ENG-21 should be parsed
            assert result == {"ENG-21": "implement"}

    @pytest.mark.anyio
    async def test_handles_empty_window_list(self) -> None:
        """Empty window list returns empty dict."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = []
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            assert result == {}


class TestGetPrDraftStatusBatch:
    """Test GitHub PR draft status batch fetching."""

    @pytest.mark.anyio
    async def test_returns_draft_status_for_multiple_issues(self) -> None:
        """Multiple PRs in same repo are fetched in single query."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            # Single repo uses repo0 alias
            response = {
                "data": {
                    "repo0": {
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
    async def test_returns_none_for_missing_pr(self) -> None:
        """PR not found returns None (not omitted from results)."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            response = {
                "data": {
                    "repo0": {
                        "pr0": None,  # PR not found
                    }
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=999)},
            runner=mock_runner,
        )
        assert result == {"ENG-21": None}

    @pytest.mark.anyio
    async def test_handles_null_is_draft_value(self) -> None:
        """isDraft: null in response is treated as False (not draft)."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            response = {
                "data": {
                    "repo0": {
                        "pr0": {"isDraft": None},  # Malformed response
                    }
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )
        assert result == {"ENG-21": False}

    @pytest.mark.anyio
    async def test_raises_on_command_failure_after_retries(self) -> None:
        """GraphQL failure raises GitHubAPIError after retries."""
        call_count = 0

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            nonlocal call_count
            call_count += 1
            return "", "rate limited", 1

        with pytest.raises(fetch.GitHubAPIError, match="GraphQL query failed"):
            await fetch.get_pr_draft_status_batch(
                {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
                runner=mock_runner,
            )

        # Verify retries happened (3 attempts)
        assert call_count == 3

    @pytest.mark.anyio
    async def test_raises_on_malformed_json_after_retries(self) -> None:
        """Malformed JSON raises GitHubAPIError after retries."""
        call_count = 0

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            nonlocal call_count
            call_count += 1
            return "not valid json {[", "", 0  # Success exit but bad JSON

        with pytest.raises(
            fetch.GitHubAPIError, match="Failed to parse GraphQL response"
        ):
            await fetch.get_pr_draft_status_batch(
                {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
                runner=mock_runner,
            )

        # Verify retries happened (3 attempts)
        assert call_count == 3

    @pytest.mark.anyio
    async def test_succeeds_after_transient_failures(self) -> None:
        """API succeeds after transient failures - retries work correctly."""
        call_count = 0

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            nonlocal call_count
            call_count += 1
            if call_count < 3:
                return "", "temporary network error", 1
            # Succeed on third attempt
            response = {"data": {"repo0": {"pr0": {"isDraft": False}}}}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )

        assert result == {"ENG-21": False}
        assert call_count == 3  # Two failures, one success

    @pytest.mark.anyio
    async def test_handles_null_data_in_response(self) -> None:
        """GitHub returning null data field is handled gracefully."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            # GitHub can return {"data": null, "errors": [...]} for invalid repos
            response = {"data": None, "errors": [{"message": "Not found"}]}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )

        # Should return None for the PR (not found), not crash
        assert result == {"ENG-21": None}

    @pytest.mark.anyio
    async def test_handles_non_dict_data_in_response(self) -> None:
        """GitHub returning non-dict data field is handled gracefully."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            # Malformed response where data is a string instead of dict
            response = {"data": "unexpected string"}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )

        # Should return None for the PR (malformed response), not crash
        assert result == {"ENG-21": None}

    @pytest.mark.anyio
    async def test_handles_non_dict_repo_in_response(self) -> None:
        """GitHub returning non-dict repo field is handled gracefully."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            # Malformed response where repo is a string instead of dict
            response = {"data": {"repo0": "unexpected string"}}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )

        # Should return None for the PR (malformed response), not crash
        assert result == {"ENG-21": None}

    @pytest.mark.anyio
    async def test_handles_non_dict_pr_in_response(self) -> None:
        """GitHub returning non-dict PR field is handled gracefully."""

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            # Malformed response where PR is a string instead of dict
            response = {"data": {"repo0": {"pr0": "unexpected string"}}}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=mock_runner,
        )

        # Should return None for the PR (malformed response), not crash
        assert result == {"ENG-21": None}

    @pytest.mark.anyio
    async def test_batches_multiple_repos_in_single_query(self) -> None:
        """PRs from different repos are batched into a single GraphQL query."""
        queries_received: list[str] = []

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
            # Extract query from command
            query = cmd[-1]  # "query=..."
            queries_received.append(query)

            # Both repos in one response
            response = {
                "data": {
                    "repo0": {"pr0": {"isDraft": True}},
                    "repo1": {"pr0": {"isDraft": False}},
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {
                "ENG-21": types.GitHubPRRef(owner="org", repo="repo1", number=1),
                "ENG-22": types.GitHubPRRef(owner="org", repo="repo2", number=2),
            },
            runner=mock_runner,
        )

        # Single query containing both repos
        assert len(queries_received) == 1
        assert "repo1" in queries_received[0]
        assert "repo2" in queries_received[0]
        assert result == {"ENG-21": True, "ENG-22": False}


class TestParsedIssueWorkerActive:
    """Test has_worker_active property on ParsedIssue."""

    def test_has_worker_active_returns_true_when_label_present(self) -> None:
        """ParsedIssue.has_worker_active returns True when worker-active label exists."""
        issue = types.ParsedIssue(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-active"],
        )
        assert issue.has_worker_active is True

    def test_has_worker_active_returns_false_when_label_absent(self) -> None:
        """ParsedIssue.has_worker_active returns False when worker-active label is missing."""
        issue = types.ParsedIssue(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-done"],
        )
        assert issue.has_worker_active is False

    def test_has_worker_active_returns_false_with_empty_labels(self) -> None:
        """ParsedIssue.has_worker_active returns False with no labels."""
        issue = types.ParsedIssue(
            issue_id="ENG-21",
            status="In Progress",
            labels=[],
        )
        assert issue.has_worker_active is False


class TestParseLinearIssues:
    """Test Linear issue parsing."""

    def test_parses_basic_issue(self) -> None:
        issues: list[LinearIssueRaw] = [
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
        issues: list[LinearIssueRaw] = [
            {
                "identifier": "ENG-21",
                "state": {"name": "In Review"},
                "labels": {"nodes": []},
            }
        ]
        result = fetch.parse_linear_issues(issues)
        assert result[0].status == "Needs Review"

    def test_skips_issues_without_identifier(self) -> None:
        issues: list[LinearIssueRaw] = [
            {"state": {"name": "Todo"}, "labels": {"nodes": []}},
            {
                "identifier": "ENG-21",
                "state": {"name": "Todo"},
                "labels": {"nodes": []},
            },
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1

    def test_handles_null_state(self) -> None:
        issues: list[LinearIssueRaw] = [
            {"identifier": "ENG-21", "state": None, "labels": {"nodes": []}}  # pyright: ignore[reportAssignmentType]
        ]
        result = fetch.parse_linear_issues(issues)
        assert result[0].status == ""

    def test_handles_null_labels(self) -> None:
        issues: list[LinearIssueRaw] = [
            {"identifier": "ENG-21", "state": {"name": "Todo"}, "labels": None}  # pyright: ignore[reportAssignmentType]
        ]
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
            has_pr=False,
        )
        assert action == "dispatch_planner"

    def test_todo_worker_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.TODO,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "transition_to_in_progress"

    def test_in_progress_no_worker_no_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "dispatch_implementer"

    def test_in_progress_worker_done(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "transition_to_needs_review"

    def test_in_progress_with_live_worker(self) -> None:
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_needs_review_approved(self) -> None:
        """PR is ready (not draft) = approved."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=False,  # PR is ready = approved
            has_pr=True,
        )
        assert action == "transition_to_retro"

    def test_needs_review_changes_requested(self) -> None:
        """PR is draft = changes requested."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=True,  # PR is draft = changes requested
            has_pr=True,
        )
        assert action == "resume_implementer_for_changes"

    def test_needs_review_worker_done_has_pr_but_unknown_status_skips(self) -> None:
        """PR exists but couldn't check status - transient, retry later."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=True,  # Has PR but couldn't check
        )
        assert action == "skip"

    def test_needs_review_worker_done_no_pr_investigates(self) -> None:
        """No PR linked - needs attention."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,  # No PR attached
        )
        assert action == "investigate_no_pr"

    def test_needs_review_no_worker_done_dispatches_reviewer(self) -> None:
        """Issue in Needs Review without worker-done = dispatch reviewer."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "dispatch_reviewer"

    def test_needs_review_with_live_worker_no_done_skips(self) -> None:
        """Active reviewer session without worker-done = skip."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_needs_review_worker_done_ignores_live_worker(self) -> None:
        """worker-done label takes precedence over live worker check."""
        # When worker-done is set, we trust it and proceed with PR status
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,
            has_live_worker=True,  # Stale session still running
            pr_is_draft=False,
            has_pr=True,
        )
        assert action == "transition_to_retro"

    def test_backlog_no_worker_done_dispatches_architect(self) -> None:
        """Backlog without worker-done dispatches architect."""
        action = decision.suggest_action(
            status=types.IssueStatus.BACKLOG,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "dispatch_architect"

    def test_backlog_worker_done_transitions_to_todo(self) -> None:
        """Backlog with worker-done transitions to Todo."""
        action = decision.suggest_action(
            status=types.IssueStatus.BACKLOG,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "transition_to_todo"

    def test_backlog_with_live_worker_skips(self) -> None:
        """Backlog with active architect session skips."""
        action = decision.suggest_action(
            status=types.IssueStatus.BACKLOG,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_triage_skips(self) -> None:
        """Triage status is handled by controller directly, state script skips."""
        action = decision.suggest_action(
            status=types.IssueStatus.TRIAGE,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_icebox_skips(self) -> None:
        """Icebox status is handled by controller directly, state script skips."""
        action = decision.suggest_action(
            status=types.IssueStatus.ICEBOX,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_retro_worker_done_dispatches_merger(self) -> None:
        """Retro with worker-done dispatches merger."""
        action = decision.suggest_action(
            status=types.IssueStatus.RETRO,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "dispatch_merger"

    def test_retro_no_worker_done_resumes_implementer(self) -> None:
        """Retro without worker-done resumes implementer for retro workflow."""
        action = decision.suggest_action(
            status=types.IssueStatus.RETRO,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "resume_implementer_for_retro"

    def test_retro_with_live_worker_skips(self) -> None:
        """Retro with active worker session skips."""
        action = decision.suggest_action(
            status=types.IssueStatus.RETRO,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_done_always_skips(self) -> None:
        """Done status always skips."""
        action = decision.suggest_action(
            status=types.IssueStatus.DONE,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"

    def test_unknown_status_skips(self) -> None:
        """Unknown/unrecognized status skips."""
        action = decision.suggest_action(
            status="SomeUnknownStatus",
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"


class TestBuildIssueState:
    """Test building issue state from fetched data."""

    def test_builds_state_with_action(self) -> None:
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="Todo",
            labels=[],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        assert state.suggested_action == "dispatch_planner"

    def test_skips_when_user_input_needed(self) -> None:
        """Worker asked for input but user hasn't responded - skip."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="Todo",
            labels=["user-input-needed"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=True,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        assert state.suggested_action == "skip"

    def test_relay_user_feedback_when_user_responded(self) -> None:
        """When worker asked for input and user responded, relay it."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,  # Worker exited after asking
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        assert state.suggested_action == "relay_user_feedback"

    def test_waiting_for_feedback_skips(self) -> None:
        """Worker asked for input, waiting for user response - skip."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        assert state.suggested_action == "skip"

    def test_feedback_without_input_needed_follows_normal_flow(self) -> None:
        """Feedback-given label without input-needed follows normal flow."""
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-feedback-given", "worker-done"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=False,  # No input was requested
        )

        state = decision.build_issue_state(data, team_id)

        # Should follow normal flow: In Progress + worker-done → transition_to_needs_review
        assert state.suggested_action == "transition_to_needs_review"
        expected_session_id = types.compute_session_id(team_id, "ENG-21", "review")
        assert state.session_id == expected_session_id

    def test_relay_feedback_computes_correct_session_id(self) -> None:
        """relay_user_feedback action computes session ID for implement mode."""
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, team_id)

        # relay_user_feedback should use implement mode
        expected_session_id = types.compute_session_id(team_id, "ENG-21", "implement")
        assert state.session_id == expected_session_id
        assert state.suggested_action == "relay_user_feedback"

    def test_relay_feedback_in_different_statuses(self) -> None:
        """relay_user_feedback works regardless of issue status."""
        team_id = "00000000-0000-0000-0000-000000000000"

        # Test in Todo status
        data_todo = types.FetchedIssueData(
            issue_id="ENG-21",
            status="Todo",
            labels=["user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=True,
        )
        state_todo = decision.build_issue_state(data_todo, team_id)
        assert state_todo.suggested_action == "relay_user_feedback"

        # Test in Needs Review status
        data_review = types.FetchedIssueData(
            issue_id="ENG-22",
            status="Needs Review",
            labels=["user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=True,
        )
        state_review = decision.build_issue_state(data_review, team_id)
        assert state_review.suggested_action == "relay_user_feedback"

    def test_all_labels_present_relay_takes_precedence(self) -> None:
        """When worker-done, user-input-needed, and user-feedback-given all present, relay wins."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-done", "user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        # relay_user_feedback takes precedence over normal state transitions
        assert state.suggested_action == "relay_user_feedback"


class TestOrphanDetection:
    """Test orphan detection in build_issue_state."""

    def test_orphan_detected_when_worker_active_but_no_live_worker(self) -> None:
        """Orphan: worker-active label exists but has_live_worker is False."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-active"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,  # Worker died
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        assert state.suggested_action == "remove_worker_active_and_redispatch"

    def test_no_orphan_when_worker_active_and_live_worker(self) -> None:
        """Not orphan: worker-active label exists and has_live_worker is True."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-active"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=True,  # Worker is running
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        # Should follow normal flow - skip because worker is running
        assert state.suggested_action == "skip"

    def test_no_orphan_when_no_worker_active_label(self) -> None:
        """Not orphan: no worker-active label, even if has_live_worker is False."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=[],  # No worker-active label
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        # Should follow normal flow - dispatch implementer
        assert state.suggested_action == "dispatch_implementer"

    def test_orphan_detected_in_various_statuses(self) -> None:
        """Orphan detection works in any status."""
        team_id = "00000000-0000-0000-0000-000000000000"

        for status in ["Todo", "In Progress", "Backlog", "Needs Review"]:
            data = types.FetchedIssueData(
                issue_id="ENG-21",
                status=status,
                labels=["worker-active"],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=False,
            )
            state = decision.build_issue_state(data, team_id)
            assert state.suggested_action == "remove_worker_active_and_redispatch", (
                f"Failed for status {status}"
            )

    def test_user_feedback_takes_precedence_over_orphan(self) -> None:
        """User feedback relay takes precedence over orphan detection."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-active", "user-input-needed", "user-feedback-given"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=True,
            has_user_input_needed=True,
        )
        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")
        # User feedback should take precedence
        assert state.suggested_action == "relay_user_feedback"

    def test_orphan_action_mode_mapping(self) -> None:
        """Verify remove_worker_active_and_redispatch has a mode in ACTION_TO_MODE."""
        assert "remove_worker_active_and_redispatch" in decision.ACTION_TO_MODE


class TestFetchAllIssueData:
    """Test full data fetching."""

    @pytest.mark.anyio
    async def test_fetches_data_for_issues(self) -> None:
        with (
            mock.patch(
                "legion.state.fetch.get_live_workers", autospec=True
            ) as mock_workers,
            mock.patch(
                "legion.state.fetch.get_pr_draft_status_batch",
                autospec=True,
            ) as mock_draft,
        ):
            # Return dict mapping issue_id -> mode (new signature)
            mock_workers.return_value = {"ENG-21": "implement"}
            mock_draft.return_value = {}

            linear_issues: list[LinearIssueRaw] = [
                {
                    "identifier": "ENG-21",
                    "state": {"name": "In Progress"},
                    "labels": {"nodes": []},
                }
            ]

            result = await fetch.fetch_all_issue_data(
                linear_issues=linear_issues,
                short_id="abc123",
                tmux_session="legion-abc123",
            )

            assert len(result) == 1
            assert result[0].issue_id == "ENG-21"
            assert result[0].has_live_worker is True
            # Verify get_live_workers was called with both parameters
            mock_workers.assert_called_once_with("abc123", "legion-abc123")


class TestBuildCollectedState:
    """Test building full collected state."""

    def test_builds_state_for_multiple_issues(self) -> None:
        issues_data = [
            types.FetchedIssueData(
                issue_id="ENG-21",
                status="Todo",
                labels=[],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
            types.FetchedIssueData(
                issue_id="ENG-22",
                status="In Progress",
                labels=["worker-done"],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
        ]

        state = decision.build_collected_state(
            issues_data, "00000000-0000-0000-0000-000000000000"
        )

        assert "ENG-21" in state.issues
        assert "ENG-22" in state.issues
        assert state.issues["ENG-21"].suggested_action == "dispatch_planner"
        assert state.issues["ENG-22"].suggested_action == "transition_to_needs_review"

    def test_to_dict_serializes_correctly(self) -> None:
        issues_data = [
            types.FetchedIssueData(
                issue_id="ENG-21",
                status="Todo",
                labels=[],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
        ]

        state = decision.build_collected_state(
            issues_data, "00000000-0000-0000-0000-000000000000"
        )
        result = state.to_dict()

        assert "issues" in result
        assert "ENG-21" in result["issues"]
        assert result["issues"]["ENG-21"]["suggested_action"] == "dispatch_planner"

    def test_relay_feedback_with_multiple_issues(self) -> None:
        """Integration test: relay_user_feedback works with multiple issues."""
        team_id = "00000000-0000-0000-0000-000000000000"
        issues_data = [
            # Normal issue - dispatch planner
            types.FetchedIssueData(
                issue_id="ENG-21",
                status="Todo",
                labels=[],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=False,
            ),
            # User responded - relay feedback
            types.FetchedIssueData(
                issue_id="ENG-22",
                status="In Progress",
                labels=["user-input-needed", "user-feedback-given"],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=True,
                has_user_input_needed=True,
            ),
            # Waiting for input - skip
            types.FetchedIssueData(
                issue_id="ENG-23",
                status="In Progress",
                labels=["user-input-needed"],
                has_pr=False,
                pr_is_draft=None,
                has_live_worker=False,
                has_user_feedback=False,
                has_user_input_needed=True,
            ),
        ]

        state = decision.build_collected_state(issues_data, team_id)

        assert len(state.issues) == 3
        assert state.issues["ENG-21"].suggested_action == "dispatch_planner"
        assert state.issues["ENG-22"].suggested_action == "relay_user_feedback"
        assert state.issues["ENG-23"].suggested_action == "skip"


class TestGetPrDraftStatusBatchErrorHandling:
    """Test error handling in PR draft status fetching."""

    @pytest.mark.anyio
    async def test_handles_github_rate_limit_with_retry(self) -> None:
        """Rate limit errors are retried with exponential backoff."""
        call_count = 0
        call_times: list[float] = []

        async def rate_limited_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            nonlocal call_count
            call_count += 1
            call_times.append(time.time())

            if call_count < 3:
                # First 2 calls fail with rate limit
                return "", "API rate limit exceeded", 1
            # Third call succeeds
            response = {"data": {"repo0": {"pr0": {"isDraft": False}}}}
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {"ENG-21": types.GitHubPRRef(owner="owner", repo="repo", number=1)},
            runner=rate_limited_runner,
        )

        # Should succeed after retries
        assert result == {"ENG-21": False}
        assert call_count == 3

        # Verify exponential backoff happened (at least 1 second between attempts)
        if len(call_times) >= 2:
            assert call_times[1] - call_times[0] >= 1.0

    @pytest.mark.anyio
    async def test_handles_empty_pr_refs(self) -> None:
        """Empty PR refs dict returns empty result without API call."""
        call_count = 0

        async def counting_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            nonlocal call_count
            call_count += 1
            return "{}", "", 0

        result = await fetch.get_pr_draft_status_batch({}, runner=counting_runner)

        assert result == {}
        assert call_count == 0  # Should not call API

    @pytest.mark.anyio
    async def test_handles_mixed_repo_failure(self) -> None:
        """Handles case where some repos return data, others don't."""

        async def mixed_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            response = {
                "data": {
                    "repo0": {"pr0": {"isDraft": True}},
                    "repo1": None,  # This repo failed
                }
            }
            return json.dumps(response), "", 0

        result = await fetch.get_pr_draft_status_batch(
            {
                "ENG-21": types.GitHubPRRef(owner="org", repo="repo1", number=1),
                "ENG-22": types.GitHubPRRef(owner="org", repo="repo2", number=2),
            },
            runner=mixed_runner,
        )

        # repo1 succeeded, repo2 failed
        assert "ENG-21" in result
        assert "ENG-22" in result


class TestGetLiveWorkersEdgeCases:
    """Test edge cases in live worker detection."""

    @pytest.mark.anyio
    async def test_handles_window_with_many_hyphens(self) -> None:
        """Windows with many hyphens are parsed correctly."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            # Issue IDs can have hyphens: TEAM-PROJECT-123
            mock_windows.return_value = [
                "implement-TEAM-PROJECT-123",
                "plan-MY-COMPLEX-ISSUE-456",
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")

            # Should parse as mode="implement", issue="TEAM-PROJECT-123"
            assert result == {
                "TEAM-PROJECT-123": "implement",
                "MY-COMPLEX-ISSUE-456": "plan",
            }

    @pytest.mark.anyio
    async def test_handles_tmux_list_windows_failure(self) -> None:
        """Handles tmux command failure gracefully."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            # tmux.list_windows returns empty list on error
            mock_windows.return_value = []
            result = await fetch.get_live_workers("abc123", "legion-abc123")
            assert result == {}

    @pytest.mark.anyio
    async def test_handles_case_sensitivity_in_issue_ids(self) -> None:
        """Issue IDs are normalized to uppercase consistently."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = [
                "implement-eng-21",  # lowercase
                "plan-Eng-22",  # mixed case
                "review-ENG-23",  # uppercase
            ]
            result = await fetch.get_live_workers("abc123", "legion-abc123")

            # All should be uppercase
            assert "ENG-21" in result
            assert "ENG-22" in result
            assert "ENG-23" in result
            assert "eng-21" not in result
            assert "Eng-22" not in result


class TestFetchAllIssueDataErrorHandling:
    """Test error handling in fetch_all_issue_data."""

    @pytest.mark.anyio
    async def test_github_api_failure_sets_pr_draft_to_none(self) -> None:
        """GitHub API failure: has_pr=True, pr_is_draft=None -> skip (not investigate_no_pr)."""

        async def failing_runner(cmd: list[str]) -> tuple[str, str, int]:  # pyright: ignore[reportUnusedParameter]
            return "", "rate limited", 1

        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = []

            linear_issues: list[LinearIssueRaw] = [
                {
                    "identifier": "ENG-21",
                    "state": {"name": "Needs Review"},
                    "labels": {"nodes": [{"name": "worker-done"}]},
                    "attachments": [{"url": "https://github.com/owner/repo/pull/1"}],
                }
            ]

            result = await fetch.fetch_all_issue_data(
                linear_issues=linear_issues,
                short_id="abc123",
                tmux_session="legion-abc123",
                runner=failing_runner,
            )

        assert len(result) == 1
        assert result[0].has_pr is True  # PR attachment exists
        assert result[0].pr_is_draft is None  # Couldn't check status

    @pytest.mark.anyio
    async def test_handles_malformed_linear_issue_data(self) -> None:
        """Handles malformed Linear issue data gracefully."""
        with mock.patch.object(
            fetch.tmux, "list_windows", autospec=True
        ) as mock_windows:
            mock_windows.return_value = []

            # Malformed issues: missing required fields
            linear_issues: list[LinearIssueRaw] = [
                {},  # Empty issue
                {"identifier": "ENG-21"},  # Missing state and labels
                {"state": {"name": "Todo"}},  # Missing identifier
            ]

            result = await fetch.fetch_all_issue_data(
                linear_issues=linear_issues,
                short_id="abc123",
                tmux_session="legion-abc123",
            )

            # Should skip malformed issues gracefully
            # Only ENG-21 should be parsed (with empty state)
            assert len(result) == 1
            assert result[0].issue_id == "ENG-21"


class TestParseLinearIssuesEdgeCases:
    """Test edge cases in Linear issue parsing."""

    def test_handles_deeply_nested_nulls(self) -> None:
        """Handles deeply nested null values gracefully."""
        issues: list[LinearIssueRaw] = [  # pyright: ignore[reportAssignmentType]
            {
                "identifier": "ENG-21",
                "state": {"name": None},  # null name
                "labels": {"nodes": None},  # null nodes
            }
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1
        # When state.name is None, normalize returns "" (empty string)
        assert result[0].status == ""
        assert result[0].labels == []

    def test_handles_labels_with_missing_name(self) -> None:
        """Handles label nodes missing 'name' field."""
        issues: list[LinearIssueRaw] = [
            {
                "identifier": "ENG-21",
                "state": {"name": "Todo"},
                "labels": {"nodes": [{"name": "worker-done"}, {}, {"name": "urgent"}]},  # pyright: ignore[reportAssignmentType]
            }
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1
        # Empty dict in nodes is filtered out (truthy check on node.get("name"))
        assert result[0].labels == ["worker-done", "urgent"]

    def test_handles_attachments_with_invalid_urls(self) -> None:
        """Handles attachments with invalid or non-PR URLs."""
        issues: list[LinearIssueRaw] = [
            {
                "identifier": "ENG-21",
                "state": {"name": "Needs Review"},
                "labels": {"nodes": []},
                "attachments": [
                    {"url": "https://example.com/not-a-pr"},
                    {
                        "url": "https://github.com/owner/repo/issues/123"
                    },  # Issue, not PR
                    {"url": "not-even-a-url"},
                ],
            }
        ]
        result = fetch.parse_linear_issues(issues)
        assert len(result) == 1
        assert result[0].pr_ref is None  # No valid PR URL found


class TestComputeSessionIdEdgeCases:
    """Test edge cases in session ID computation."""

    def test_empty_team_id_raises(self) -> None:
        """Empty team ID raises ValueError."""
        with pytest.raises(ValueError):
            types.compute_session_id("", "ENG-21", "implement")

    def test_malformed_uuid_raises(self) -> None:
        """Malformed UUID raises ValueError."""
        with pytest.raises(ValueError):
            types.compute_session_id("not-a-uuid", "ENG-21", "implement")

    def test_uuid_without_hyphens_accepted(self) -> None:
        """UUID without hyphens is accepted by Python's UUID constructor."""
        # Python's UUID constructor accepts UUIDs with or without hyphens
        result = types.compute_session_id(
            "7b4f0862b7754cb09a6785400c6f44a8", "ENG-21", "implement"
        )
        assert isinstance(result, str)
        uuid.UUID(result)  # Should be valid UUID

    def test_special_characters_in_issue_id(self) -> None:
        """Issue IDs with special characters are handled."""
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        # Should not crash, even with weird issue IDs
        result = types.compute_session_id(team_id, "ENG-21/special", "implement")
        assert isinstance(result, str)
        uuid.UUID(result)  # Should be valid UUID


class TestSuggestActionEdgeCases:
    """Test edge cases in action suggestion."""

    def test_unknown_status_always_skips(self) -> None:
        """Unknown/custom statuses always skip."""
        for status in ["CustomStatus", "WIP", "Blocked", "Random"]:
            action = decision.suggest_action(
                status=status,
                has_worker_done=False,
                has_live_worker=False,
                pr_is_draft=None,
                has_pr=False,
            )
            assert action == "skip"

    def test_done_status_ignores_all_flags(self) -> None:
        """Done status skips regardless of other flags."""
        # Even with worker-done and live worker, should skip
        action = decision.suggest_action(
            status=types.IssueStatus.DONE,
            has_worker_done=True,
            has_live_worker=True,
            pr_is_draft=False,
            has_pr=True,
        )
        assert action == "skip"

    def test_unknown_status_with_worker_done_still_skips(self) -> None:
        """Unknown status skips even with worker-done."""
        action = decision.suggest_action(
            status="SomeUnknownStatus",
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=None,
            has_pr=False,
        )
        assert action == "skip"


class TestActionToModeMapping:
    """Test ACTION_TO_MODE mapping for session ID computation."""

    def test_investigate_no_pr_has_implement_mode(self) -> None:
        """Verify investigate_no_pr action maps to implement mode."""
        assert (
            decision.ACTION_TO_MODE["investigate_no_pr"] == types.WorkerMode.IMPLEMENT
        )


class TestSessionIdComputation:
    """Test session ID computation for various actions."""

    team_id: str = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"

    def test_dispatch_architect_uses_architect_mode(self) -> None:
        """Dispatch architect action uses architect mode for session ID."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.BACKLOG,
            labels=[],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, self.team_id)
        assert state.suggested_action == "dispatch_architect"
        assert state.session_id == types.compute_session_id(
            self.team_id, "ENG-21", "architect"
        )

    def test_dispatch_merger_uses_merge_mode(self) -> None:
        """Dispatch merger action uses merge mode for session ID."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.RETRO,
            labels=["worker-done"],
            has_pr=True,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, self.team_id)
        assert state.suggested_action == "dispatch_merger"
        assert state.session_id == types.compute_session_id(
            self.team_id, "ENG-21", "merge"
        )

    def test_transition_to_needs_review_uses_review_mode(self) -> None:
        """Transition to needs review uses review mode for session ID."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.IN_PROGRESS,
            labels=["worker-done"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, self.team_id)
        assert state.suggested_action == "transition_to_needs_review"
        assert state.session_id == types.compute_session_id(
            self.team_id, "ENG-21", "review"
        )

    def test_transition_to_todo_uses_plan_mode(self) -> None:
        """Transition to todo uses plan mode for session ID."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.BACKLOG,
            labels=["worker-done"],
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )
        state = decision.build_issue_state(data, self.team_id)
        assert state.suggested_action == "transition_to_todo"
        assert state.session_id == types.compute_session_id(
            self.team_id, "ENG-21", "plan"
        )


class TestStuckStateIntegration:
    """Integration tests for stuck state detection with GitHub API failures."""

    def test_github_api_failure_results_in_skip_not_investigate(self) -> None:
        """GitHub API failure: has_pr=True, pr_is_draft=None -> skip (not investigate_no_pr).

        Critical: This distinguishes between "no PR exists" (investigate) and
        "couldn't check PR status" (skip and retry later).
        """
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.NEEDS_REVIEW,
            labels=["worker-done"],
            has_pr=True,  # PR exists
            pr_is_draft=None,  # But couldn't check status (GitHub API down)
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )

        state = decision.build_issue_state(data, team_id)

        # Must skip, not investigate - API failure is transient
        assert state.suggested_action == "skip"

    def test_truly_missing_pr_triggers_investigate(self) -> None:
        """No PR attached: has_pr=False -> investigate_no_pr.

        This is the stuck state that needs human attention.
        """
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.NEEDS_REVIEW,
            labels=["worker-done"],
            has_pr=False,  # No PR attached at all
            pr_is_draft=None,
            has_live_worker=False,
            has_user_feedback=False,
            has_user_input_needed=False,
        )

        state = decision.build_issue_state(data, team_id)

        # Must investigate - worker claimed done but no PR exists
        assert state.suggested_action == "investigate_no_pr"


class TestActionToModeCompleteness:
    """Test that ACTION_TO_MODE mapping is complete and correct."""

    def test_all_actions_have_mode_mapping(self) -> None:
        """Every ActionType value must have an ACTION_TO_MODE entry.

        Missing entries cause fallback to IMPLEMENT mode, which could cause
        session ID collisions if multiple actions for same issue use the fallback.
        """
        # Get all ActionType literals from the type definition
        # ActionType is a Literal, so we need to extract the values
        import typing

        action_type = typing.get_args(types.ActionType)
        mapped_actions = set(decision.ACTION_TO_MODE.keys())

        # All ActionType values should be in ACTION_TO_MODE
        for action in action_type:
            assert action in mapped_actions, (
                f"Action '{action}' missing from ACTION_TO_MODE mapping. "
                f"This would cause fallback to IMPLEMENT mode."
            )


class TestSessionIdUniqueness:
    """Test session ID uniqueness across different actions for same issue."""

    team_id: str = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"

    def test_different_actions_produce_different_session_ids(self) -> None:
        """Different actions on same issue must produce different session IDs.

        Session ID collision would cause workers to overwrite each other's state.
        """
        issue_id = "ENG-21"

        # Test key action pairs that operate on same issue
        plan_session = types.compute_session_id(self.team_id, issue_id, "plan")
        implement_session = types.compute_session_id(
            self.team_id, issue_id, "implement"
        )
        review_session = types.compute_session_id(self.team_id, issue_id, "review")
        merge_session = types.compute_session_id(self.team_id, issue_id, "merge")
        architect_session = types.compute_session_id(
            self.team_id, issue_id, "architect"
        )

        # All must be unique
        session_ids = {
            plan_session,
            implement_session,
            review_session,
            merge_session,
            architect_session,
        }
        assert len(session_ids) == 5, (
            "Session IDs must be unique across different modes"
        )

    def test_same_action_same_issue_produces_same_session_id(self) -> None:
        """Same action on same issue must be deterministic."""
        issue_id = "ENG-21"

        session1 = types.compute_session_id(self.team_id, issue_id, "implement")
        session2 = types.compute_session_id(self.team_id, issue_id, "implement")

        assert session1 == session2, "Session ID must be deterministic"


class TestParsedIssueProperties:
    """Test ParsedIssue property logic."""

    def test_needs_pr_status_requires_all_three_conditions(self) -> None:
        """needs_pr_status should only be True when:
        1. Status is NEEDS_REVIEW
        2. Has worker-done label
        3. Has PR attached

        This controls whether GitHub API is called for PR status.
        """
        # All conditions met - should return True
        issue_all_met = types.ParsedIssue(
            issue_id="ENG-21",
            status=types.IssueStatus.NEEDS_REVIEW,
            labels=["worker-done"],
            pr_ref=types.GitHubPRRef(owner="org", repo="repo", number=1),
        )
        assert issue_all_met.needs_pr_status is True

        # Missing worker-done label
        issue_no_done = types.ParsedIssue(
            issue_id="ENG-21",
            status=types.IssueStatus.NEEDS_REVIEW,
            labels=[],
            pr_ref=types.GitHubPRRef(owner="org", repo="repo", number=1),
        )
        assert issue_no_done.needs_pr_status is False

        # Wrong status
        issue_wrong_status = types.ParsedIssue(
            issue_id="ENG-21",
            status=types.IssueStatus.IN_PROGRESS,
            labels=["worker-done"],
            pr_ref=types.GitHubPRRef(owner="org", repo="repo", number=1),
        )
        assert issue_wrong_status.needs_pr_status is False

        # No PR attached
        issue_no_pr = types.ParsedIssue(
            issue_id="ENG-21",
            status=types.IssueStatus.NEEDS_REVIEW,
            labels=["worker-done"],
            pr_ref=None,
        )
        assert issue_no_pr.needs_pr_status is False


class TestOrphanDetectionPriority:
    """Test priority of orphan detection vs other conditions."""

    def test_orphan_with_worker_done_and_worker_active_removes_active(self) -> None:
        """When both worker-done and worker-active exist but no live worker, orphan detection wins.

        This scenario can happen if worker sets worker-done, then crashes before removing worker-active.
        """
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status=types.IssueStatus.IN_PROGRESS,
            labels=["worker-done", "worker-active"],  # Both labels present
            has_pr=False,
            pr_is_draft=None,
            has_live_worker=False,  # But worker died
            has_user_feedback=False,
            has_user_input_needed=False,
        )

        state = decision.build_issue_state(data, team_id)

        # Orphan detection takes precedence - worker died, need to clean up
        assert state.suggested_action == "remove_worker_active_and_redispatch"


class TestSuggestActionEdgeCasesWithPR:
    """Test that PR status is only checked in NEEDS_REVIEW state."""

    def test_in_progress_ignores_pr_status(self) -> None:
        """IN_PROGRESS with has_pr=True and pr_is_draft=False should not transition to retro.

        Only NEEDS_REVIEW state should check PR draft status.
        """
        action = decision.suggest_action(
            status=types.IssueStatus.IN_PROGRESS,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=False,  # PR is ready
            has_pr=True,
        )
        # Should transition to needs review, not retro
        assert action == "transition_to_needs_review"

    def test_todo_ignores_pr_status(self) -> None:
        """TODO with PR should still follow normal flow."""
        action = decision.suggest_action(
            status=types.IssueStatus.TODO,
            has_worker_done=True,
            has_live_worker=False,
            pr_is_draft=False,
            has_pr=True,
        )
        # Should transition to in progress, ignoring PR
        assert action == "transition_to_in_progress"


class TestInvestigateNoPrWithLiveWorker:
    """Test investigate_no_pr behavior with live worker edge case."""

    def test_needs_review_no_pr_with_live_worker_still_investigates(self) -> None:
        """Even with live worker, investigate_no_pr should trigger when no PR exists.

        Live worker check happens before worker-done checks in decision tree,
        but this combination (worker-done + no PR + live worker) is unusual.
        Current implementation: worker-done bypasses live worker check.
        """
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=True,  # Worker claims done
            has_live_worker=True,  # But somehow still running (stale?)
            pr_is_draft=None,
            has_pr=False,  # No PR exists
        )

        # Worker-done takes precedence, so should still investigate
        assert action == "investigate_no_pr"
