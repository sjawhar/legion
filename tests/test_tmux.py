"""Tests for tmux module."""

from unittest import mock

import pytest

from legion import tmux


class TestNewSession:
    """Test creating tmux session with command."""

    @pytest.mark.anyio
    async def test_creates_session_with_command(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.new_session("my-session", "main", "echo hello")
            mock_run.assert_called_once_with(
                [
                    "tmux",
                    "new-session",
                    "-d",
                    "-s",
                    "my-session",
                    "-n",
                    "main",
                    "echo hello",
                ]
            )


class TestListWindows:
    """Test listing windows in a tmux session."""

    @pytest.mark.anyio
    async def test_lists_windows_in_session(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("implement-ENG-21\nplan-ENG-22\nmain", "", 0)
            result = await tmux.list_windows("legion-abc")
            mock_run.assert_called_once_with(
                ["tmux", "list-windows", "-t", "legion-abc", "-F", "#{window_name}"]
            )
            assert result == ["implement-ENG-21", "plan-ENG-22", "main"]

    @pytest.mark.anyio
    async def test_returns_empty_list_on_error(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "session not found", 1)
            result = await tmux.list_windows("nonexistent")
            assert result == []

    @pytest.mark.anyio
    async def test_filters_empty_lines(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("main\n\nworker\n", "", 0)
            result = await tmux.list_windows("legion-abc")
            assert result == ["main", "worker"]

    @pytest.mark.anyio
    async def test_parses_worker_window_names(self) -> None:
        """Verify window names follow {mode}-{issue} convention."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = (
                "architect-ENG-10\nplan-ENG-21\nimplement-ENG-21\nreview-LEG-5\nmerge-LEG-5",
                "",
                0,
            )
            result = await tmux.list_windows("legion-xyz")
            # All window names should be parseable as mode-issue
            for window in result:
                parts = window.split("-", 1)
                assert len(parts) == 2
                mode, issue = parts
                assert mode in ("architect", "plan", "implement", "review", "merge")
                # Issue should match pattern like ENG-21 or LEG-5
                assert "-" in issue


class TestNewWindow:
    """Test creating a new window in a tmux session."""

    @pytest.mark.anyio
    async def test_creates_window_with_name(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.new_window("legion-abc", "implement-ENG-21")
            mock_run.assert_called_once_with(
                [
                    "tmux",
                    "new-window",
                    "-t",
                    "legion-abc",
                    "-n",
                    "implement-ENG-21",
                    "-d",
                ]
            )

    @pytest.mark.anyio
    async def test_creates_window_for_different_modes(self) -> None:
        """Test window creation for various worker modes."""
        modes = ["architect", "plan", "implement", "review", "merge"]
        for mode in modes:
            window_name = f"{mode}-ENG-42"
            with mock.patch.object(
                tmux, "run", new_callable=mock.AsyncMock
            ) as mock_run:
                mock_run.return_value = ("", "", 0)
                await tmux.new_window("legion-xyz", window_name)
                mock_run.assert_called_once_with(
                    ["tmux", "new-window", "-t", "legion-xyz", "-n", window_name, "-d"]
                )


class TestKillWindow:
    """Test killing a window in a tmux session."""

    @pytest.mark.anyio
    async def test_kills_window_by_name(self) -> None:
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.kill_window("legion-abc", "implement-ENG-21")
            mock_run.assert_called_once_with(
                ["tmux", "kill-window", "-t", "legion-abc:implement-ENG-21"]
            )

    @pytest.mark.anyio
    async def test_uses_session_window_target_format(self) -> None:
        """Verify the target format is session:window."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.kill_window("my-session", "my-window")
            # Check the -t argument contains the colon-separated format
            call_args = mock_run.call_args[0][0]
            assert "-t" in call_args
            target_idx = call_args.index("-t") + 1
            assert call_args[target_idx] == "my-session:my-window"


class TestRunErrorHandling:
    """Test error handling in the run function."""

    @pytest.mark.anyio
    async def test_handles_command_not_found(self) -> None:
        """Command not found returns non-zero exit code."""
        stdout, stderr, rc = await tmux.run(["nonexistent-command-12345"])
        assert rc != 0
        assert stderr != "" or stdout == ""

    @pytest.mark.anyio
    async def test_handles_command_with_non_utf8_output(self) -> None:
        """Commands with binary output don't crash."""
        # Create a script that outputs non-UTF8 bytes
        # Use printf to output raw bytes
        stdout, stderr, rc = await tmux.run(["printf", "\\xff\\xfe"])
        # Should handle decode errors gracefully or error appropriately
        # The exact behavior depends on anyio, but it shouldn't crash
        assert isinstance(stdout, str)
        assert isinstance(stderr, str)

    @pytest.mark.anyio
    async def test_returns_stderr_on_failure(self) -> None:
        """Failed commands return stderr output."""
        # Try to list windows in non-existent session
        stdout, stderr, rc = await tmux.run(
            ["tmux", "list-windows", "-t", "nonexistent-session-999"]
        )
        assert rc != 0
        assert stderr != ""  # Should contain error message


class TestSessionExistsEdgeCases:
    """Test edge cases in session_exists."""

    @pytest.mark.anyio
    async def test_session_with_special_characters(self) -> None:
        """Sessions with special characters in name are handled."""
        # Assuming no session with this name exists
        result = await tmux.session_exists("session-with-underscores_123")
        assert isinstance(result, bool)

    @pytest.mark.anyio
    async def test_very_long_session_name(self) -> None:
        """Very long session names are handled."""
        long_name = "a" * 1000
        result = await tmux.session_exists(long_name)
        assert isinstance(result, bool)
        assert result is False  # Unlikely to exist


class TestListWindowsErrorHandling:
    """Test error handling in list_windows."""

    @pytest.mark.anyio
    async def test_handles_session_not_found(self) -> None:
        """Returns empty list when session doesn't exist."""
        result = await tmux.list_windows("nonexistent-session-999")
        assert result == []

    @pytest.mark.anyio
    async def test_handles_empty_session(self) -> None:
        """Handles session with no windows gracefully."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            result = await tmux.list_windows("empty-session")
            assert result == []

    @pytest.mark.anyio
    async def test_filters_whitespace_only_lines(self) -> None:
        """Filters out whitespace-only window names."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("main\n   \n\t\nworker\n", "", 0)
            result = await tmux.list_windows("legion-abc")
            assert result == ["main", "worker"]


class TestNewWindowErrorHandling:
    """Test error handling in new_window."""

    @pytest.mark.anyio
    async def test_handles_session_not_found(self) -> None:
        """new_window on non-existent session completes (tmux handles error)."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "session not found", 1)
            # Should not raise - let tmux handle the error
            await tmux.new_window("nonexistent", "window")
            mock_run.assert_called_once()

    @pytest.mark.anyio
    async def test_handles_duplicate_window_name(self) -> None:
        """Creating window with duplicate name completes (tmux may rename it)."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.new_window("legion-abc", "main")
            # tmux will handle duplicate names by suffixing
            mock_run.assert_called_once()


class TestKillWindowErrorHandling:
    """Test error handling in kill_window."""

    @pytest.mark.anyio
    async def test_handles_window_not_found(self) -> None:
        """kill_window on non-existent window completes."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "can't find window", 1)
            # Should not raise
            await tmux.kill_window("legion-abc", "nonexistent")
            mock_run.assert_called_once()

    @pytest.mark.anyio
    async def test_target_format_is_correct(self) -> None:
        """Verify target format is session:window."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.kill_window("my-session", "my-window")

            call_args = mock_run.call_args[0][0]
            assert "my-session:my-window" in call_args


class TestSendKeysErrorHandling:
    """Test error handling in send_keys."""

    @pytest.mark.anyio
    async def test_handles_non_existent_target(self) -> None:
        """send_keys to non-existent target completes."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "can't find session", 1)
            # Should not raise
            await tmux.send_keys("nonexistent", "window", "echo test")
            mock_run.assert_called_once()

    @pytest.mark.anyio
    async def test_handles_special_characters_in_keys(self) -> None:
        """Keys with special characters are passed through."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            special_keys = "echo 'test'; rm -rf /"
            await tmux.send_keys("session", "window", special_keys)

            call_args = mock_run.call_args[0][0]
            # Should contain the keys as-is (tmux handles escaping)
            assert special_keys in call_args


class TestNewSessionErrorHandling:
    """Test error handling in new_session."""

    @pytest.mark.anyio
    async def test_handles_duplicate_session_name(self) -> None:
        """new_session with duplicate name completes (tmux returns error)."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "duplicate session", 1)
            # Should not raise - let caller handle
            await tmux.new_session("existing-session", "main", "echo test")
            mock_run.assert_called_once()

    @pytest.mark.anyio
    async def test_command_with_shell_metacharacters(self) -> None:
        """Commands with shell metacharacters are passed correctly."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            complex_cmd = "cd /tmp && echo 'test' | grep test"
            await tmux.new_session("session", "main", complex_cmd)

            call_args = mock_run.call_args[0][0]
            # Command should be passed as single argument to tmux
            assert complex_cmd in call_args

    @pytest.mark.anyio
    async def test_empty_command_string(self) -> None:
        """Empty command string is handled."""
        with mock.patch.object(tmux, "run", new_callable=mock.AsyncMock) as mock_run:
            mock_run.return_value = ("", "", 0)
            await tmux.new_session("session", "main", "")
            mock_run.assert_called_once()
