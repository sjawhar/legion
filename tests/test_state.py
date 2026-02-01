"""Tests for state module."""

import json
import uuid
from pathlib import Path
from unittest import mock

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
        with mock.patch("legion.state.fetch.get_tmux_sessions", new_callable=mock.AsyncMock) as mock_sessions:
            mock_sessions.return_value = [
                "legion-abc123-worker-ENG-21",
                "legion-abc123-worker-ENG-22",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21", "ENG-22"}

    @pytest.mark.anyio
    async def test_ignores_other_sessions(self) -> None:
        with mock.patch("legion.state.fetch.get_tmux_sessions", new_callable=mock.AsyncMock) as mock_sessions:
            mock_sessions.return_value = [
                "legion-abc123-controller",
                "legion-abc123-worker-ENG-21",
                "other-session",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21"}

    @pytest.mark.anyio
    async def test_normalizes_lowercase_to_uppercase(self) -> None:
        with mock.patch("legion.state.fetch.get_tmux_sessions", new_callable=mock.AsyncMock) as mock_sessions:
            mock_sessions.return_value = [
                "legion-abc123-worker-eng-21",
                "legion-abc123-worker-Eng-22",
            ]
            result = await fetch.get_live_workers("abc123")
            assert result == {"ENG-21", "ENG-22"}


class TestGetPrDraftStatusBatch:
    """Test GitHub PR draft status batch fetching."""

    @pytest.mark.anyio
    async def test_returns_draft_status_for_multiple_issues(self) -> None:
        """Multiple PRs in same repo are fetched in single query."""
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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
        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
            nonlocal call_count
            call_count += 1
            return "not valid json {[", "", 0  # Success exit but bad JSON

        with pytest.raises(fetch.GitHubAPIError, match="Failed to parse GraphQL response"):
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

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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

        async def mock_runner(cmd: list[str]) -> tuple[str, str, int]:
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

    def test_needs_review_no_worker_done_dispatches_reviewer(self) -> None:
        """Issue in Needs Review without worker-done = dispatch reviewer."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=False,
            has_live_worker=False,
            pr_is_draft=None,
            is_blocked=False,
        )
        assert action == "dispatch_reviewer"

    def test_needs_review_with_live_worker_no_done_skips(self) -> None:
        """Active reviewer session without worker-done = skip."""
        action = decision.suggest_action(
            status=types.IssueStatus.NEEDS_REVIEW,
            has_worker_done=False,
            has_live_worker=True,
            pr_is_draft=None,
            is_blocked=False,
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
            is_blocked=False,
        )
        assert action == "transition_to_retro"

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
    def test_relay_user_feedback_when_blocked_with_feedback(self) -> None:
        """When worker is blocked and user gave feedback, relay it."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Which approach should I use?",
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        assert state.suggested_action == "relay_user_feedback"

    def test_blocked_without_feedback_skips(self) -> None:
        """Blocked worker without user feedback still skips."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed"],
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Which approach?",
            has_user_feedback=False,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        assert state.suggested_action == "skip"

    def test_blocked_with_feedback_but_no_live_worker_redispatches(self) -> None:
        """When worker died but feedback was given, redispatch rather than relay."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            pr_is_draft=None,
            has_live_worker=False,  # Worker crashed/terminated
            is_blocked=True,  # Session file still shows blocked state
            blocked_question="Which approach should I use?",
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        # Should dispatch a new implementer, not try to relay to dead worker
        assert state.suggested_action == "dispatch_implementer"

    def test_feedback_but_not_blocked_follows_normal_flow(self) -> None:
        """Feedback given but worker not blocked follows normal transition logic."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-feedback-given", "worker-done"],
            pr_is_draft=None,
            has_live_worker=False,
            is_blocked=False,  # Not blocked
            blocked_question=None,
            has_user_feedback=True,
            has_user_input_needed=False,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        # Should follow normal flow: In Progress + worker-done → dispatch_reviewer
        assert state.suggested_action == "dispatch_reviewer"

    def test_relay_feedback_computes_correct_session_id(self) -> None:
        """relay_user_feedback action computes session ID for implement mode."""
        team_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Which approach?",
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
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Should I start?",
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
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Should I merge?",
            has_user_feedback=True,
            has_user_input_needed=True,
        )
        state_review = decision.build_issue_state(data_review, team_id)
        assert state_review.suggested_action == "relay_user_feedback"

    def test_relay_feedback_preserves_blocked_question(self) -> None:
        """Blocked question is preserved in state when relaying feedback."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["user-input-needed", "user-feedback-given"],
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Should I use approach A or B?",
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        assert state.suggested_action == "relay_user_feedback"
        assert state.blocked_question == "Should I use approach A or B?"
        assert state.has_user_feedback is True

    def test_all_labels_present_relay_takes_precedence(self) -> None:
        """When worker-done, user-input-needed, and user-feedback-given all present, relay wins."""
        data = types.FetchedIssueData(
            issue_id="ENG-21",
            status="In Progress",
            labels=["worker-done", "user-input-needed", "user-feedback-given"],
            pr_is_draft=None,
            has_live_worker=True,
            is_blocked=True,
            blocked_question="Final question before completion?",
            has_user_feedback=True,
            has_user_input_needed=True,
        )

        state = decision.build_issue_state(data, "00000000-0000-0000-0000-000000000000")

        # relay_user_feedback takes precedence over normal state transitions
        assert state.suggested_action == "relay_user_feedback"




class TestFetchAllIssueData:
    """Test full data fetching."""

    @pytest.mark.anyio
    async def test_fetches_data_for_issues(self) -> None:
        with (
            mock.patch("legion.state.fetch.get_live_workers", new_callable=mock.AsyncMock) as mock_workers,
            mock.patch("legion.state.fetch.get_pr_draft_status_batch", new_callable=mock.AsyncMock) as mock_draft,
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

    def test_relay_feedback_with_multiple_issues(self) -> None:
        """Integration test: relay_user_feedback works with multiple issues."""
        team_id = "00000000-0000-0000-0000-000000000000"
        issues_data = [
            # Normal issue - dispatch planner
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
            # Blocked with feedback - relay
            types.FetchedIssueData(
                issue_id="ENG-22",
                status="In Progress",
                labels=["user-input-needed", "user-feedback-given"],
                pr_is_draft=None,
                has_live_worker=True,
                is_blocked=True,
                blocked_question="How should I proceed?",
                has_user_feedback=True,
                has_user_input_needed=True,
            ),
            # Blocked without feedback - skip
            types.FetchedIssueData(
                issue_id="ENG-23",
                status="In Progress",
                labels=["user-input-needed"],
                pr_is_draft=None,
                has_live_worker=True,
                is_blocked=True,
                blocked_question="Waiting for input",
                has_user_feedback=False,
                has_user_input_needed=True,
            ),
        ]

        state = decision.build_collected_state(issues_data, team_id)

        assert len(state.issues) == 3
        assert state.issues["ENG-21"].suggested_action == "dispatch_planner"
        assert state.issues["ENG-22"].suggested_action == "relay_user_feedback"
        assert state.issues["ENG-22"].blocked_question == "How should I proceed?"
        assert state.issues["ENG-23"].suggested_action == "skip"


