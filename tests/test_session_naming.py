"""Tests for session and window naming conventions."""

from legion import daemon


def worker_window_name(issue_id: str, mode: str) -> str:
    """Get tmux window name for a worker.

    This mirrors the logic in the controller skill.
    Window format: {issue_lower}-{mode} (e.g., leg-18-architect)
    """
    return f"{issue_id.lower()}-{mode}"


def workspace_name(issue_id: str) -> str:
    """Get jj workspace name for an issue.

    This mirrors the logic in the controller skill.
    Workspace format: lowercase issue identifier (e.g., leg-18)
    """
    return issue_id.lower()


class TestSessionNaming:
    def test_session_format(self) -> None:
        session = daemon.session_name("abc123")
        assert session == "legion-abc123"

    def test_worker_window_format(self) -> None:
        window = worker_window_name("ENG-523", "architect")
        assert window == "eng-523-architect"

    def test_worker_window_lowercase(self) -> None:
        # Issue IDs from Linear are uppercase, but window names should be lowercase
        window = worker_window_name("ENG-523", "implement")
        assert "ENG" not in window
        assert "eng-523" in window

    def test_worker_window_modes(self) -> None:
        assert worker_window_name("LEG-18", "architect") == "leg-18-architect"
        assert worker_window_name("LEG-18", "plan") == "leg-18-plan"
        assert worker_window_name("LEG-18", "implement") == "leg-18-implement"
        assert worker_window_name("LEG-18", "review") == "leg-18-review"
        assert worker_window_name("LEG-18", "retro") == "leg-18-retro"
        assert worker_window_name("LEG-18", "merge") == "leg-18-merge"

    def test_workspace_naming(self) -> None:
        assert workspace_name("LEG-18") == "leg-18"
        assert workspace_name("ENG-523") == "eng-523"

    def test_full_workflow(self) -> None:
        # Simulate full workflow from UUID to session/window names
        project_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        short = daemon.get_short_id(project_id)

        session = daemon.session_name(short)
        window = worker_window_name("ENG-523", "implement")
        workspace = workspace_name("ENG-523")

        # Session is just legion-{short}
        assert session == f"legion-{short}"

        # Window is issue-mode
        assert window == "eng-523-implement"

        # Workspace is lowercase issue identifier
        assert workspace == "eng-523"
