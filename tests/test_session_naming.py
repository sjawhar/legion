"""Tests for session naming conventions."""

from legion.daemon import short_id, controller_session_name


def worker_session_name(short: str, issue_id: str) -> str:
    """Get tmux session name for a worker.

    This mirrors the logic in the controller skill.
    """
    return f"legion-{short}-worker-{issue_id.lower()}"


class TestSessionNaming:
    def test_controller_session_format(self) -> None:
        session = controller_session_name("abc123")
        assert session == "legion-abc123-controller"

    def test_worker_session_format(self) -> None:
        session = worker_session_name("abc123", "ENG-523")
        assert session == "legion-abc123-worker-eng-523"

    def test_worker_session_lowercase(self) -> None:
        # Issue IDs from Linear are uppercase, but sessions should be lowercase
        session = worker_session_name("abc123", "ENG-523")
        assert "ENG" not in session
        assert "eng-523" in session

    def test_worker_session_already_lowercase(self) -> None:
        session = worker_session_name("abc123", "eng-523")
        assert session == "legion-abc123-worker-eng-523"

    def test_full_workflow(self) -> None:
        # Simulate full workflow from UUID to session names
        project_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        short = short_id(project_id)

        controller = controller_session_name(short)
        worker = worker_session_name(short, "ENG-523")

        # Both should start with the same prefix
        assert controller.startswith(f"legion-{short}-")
        assert worker.startswith(f"legion-{short}-")

        # Controller ends with -controller
        assert controller.endswith("-controller")

        # Worker ends with lowercase issue ID
        assert worker.endswith("-eng-523")
