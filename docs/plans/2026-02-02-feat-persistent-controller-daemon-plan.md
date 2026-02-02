# Persistent Controller Daemon Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Update Legion daemon to supervise persistent controller with deterministic session IDs, session file-based liveness detection, and auto-restart.

**Architecture:** Daemon monitors controller health by checking Claude Code session file mtime. Uses deterministic UUIDv5 session IDs so controller can resume context after restart. Controller runs in tmux, daemon restarts it if stale or crashed.

**Tech Stack:** Python 3.13+, anyio, pytest, tmux, Claude Code CLI

---

## Task 1: Add `compute_controller_session_id` to types.py

**Files:**
- Modify: `src/legion/state/types.py:310-327` (after `compute_session_id`)
- Test: `tests/test_state.py`

**Step 1: Write the failing test**

```python
# Add to tests/test_state.py after TestComputeSessionId class

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
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_state.py::TestComputeControllerSessionId -v`
Expected: FAIL with "AttributeError: module 'legion.state.types' has no attribute 'compute_controller_session_id'"

**Step 3: Write minimal implementation**

Add to `src/legion/state/types.py` after `compute_session_id`:

```python
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
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_state.py::TestComputeControllerSessionId -v`
Expected: PASS (4 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(types): add compute_controller_session_id"
```

---

## Task 2: Add `get_session_file_path` to daemon.py

**Files:**
- Modify: `src/legion/daemon.py` (add import and function)
- Test: `tests/test_daemon.py` (create new file)

**Step 1: Write the failing test**

Create `tests/test_daemon.py`:

```python
"""Tests for daemon module."""

from pathlib import Path

from legion import daemon


class TestGetSessionFilePath:
    """Test Claude session file path computation."""

    def test_encodes_simple_path(self) -> None:
        workspace = Path("/home/sami/legion/default")
        session_id = "abc-123"
        result = daemon.get_session_file_path(workspace, session_id)
        assert result == Path.home() / ".claude/projects/-home-sami-legion-default/abc-123.jsonl"

    def test_encodes_dots_as_dashes(self) -> None:
        workspace = Path("/home/sami/.dotfiles")
        session_id = "abc-123"
        result = daemon.get_session_file_path(workspace, session_id)
        assert result == Path.home() / ".claude/projects/-home-sami--dotfiles/abc-123.jsonl"

    def test_handles_trailing_slash(self) -> None:
        workspace = Path("/home/sami/legion/default/")
        session_id = "abc-123"
        result = daemon.get_session_file_path(workspace, session_id)
        # Path normalizes trailing slash
        assert "-default-" not in str(result) or result.name == "abc-123.jsonl"
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestGetSessionFilePath -v`
Expected: FAIL with "AttributeError: module 'legion.daemon' has no attribute 'get_session_file_path'"

**Step 3: Write minimal implementation**

Add import at top of `src/legion/daemon.py` (after existing imports):

```python
import re
```

Add function after `validate_project_id`:

```python
def get_session_file_path(workspace: Path, session_id: str) -> Path:
    """Get path to Claude Code session file.

    Claude Code encodes workspace paths by replacing all non-alphanumeric
    characters with dashes. Examples:
    - /home/sami/legion/default -> -home-sami-legion-default
    - /home/sami/.dotfiles -> -home-sami--dotfiles
    """
    encoded = re.sub(r"[^a-zA-Z0-9]", "-", str(workspace))
    return Path.home() / ".claude" / "projects" / encoded / f"{session_id}.jsonl"
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestGetSessionFilePath -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(daemon): add get_session_file_path helper"
```

---

## Task 3: Add `get_newest_mtime` to daemon.py

**Files:**
- Modify: `src/legion/daemon.py`
- Test: `tests/test_daemon.py`

**Step 1: Write the failing test**

Add to `tests/test_daemon.py`:

```python
import tempfile
import time

import pytest


class TestGetNewestMtime:
    """Test session file mtime detection."""

    @pytest.mark.anyio
    async def test_returns_none_when_file_missing(self, tmp_path: Path) -> None:
        session_file = tmp_path / "missing.jsonl"
        result = await daemon.get_newest_mtime(session_file)
        assert result is None

    @pytest.mark.anyio
    async def test_returns_mtime_of_session_file(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.touch()
        result = await daemon.get_newest_mtime(session_file)
        assert result is not None
        assert abs(result - time.time()) < 2  # Within 2 seconds

    @pytest.mark.anyio
    async def test_returns_newest_from_subagents(self, tmp_path: Path) -> None:
        # Create session file
        session_file = tmp_path / "session.jsonl"
        session_file.touch()
        old_mtime = session_file.stat().st_mtime

        # Create subagent dir with newer file
        subagent_dir = tmp_path / "session" / "subagents"
        subagent_dir.mkdir(parents=True)
        time.sleep(0.1)  # Ensure different mtime
        subagent_file = subagent_dir / "agent-1.jsonl"
        subagent_file.touch()

        result = await daemon.get_newest_mtime(session_file)
        assert result is not None
        assert result > old_mtime

    @pytest.mark.anyio
    async def test_ignores_non_jsonl_subagent_files(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.touch()
        session_mtime = session_file.stat().st_mtime

        subagent_dir = tmp_path / "session" / "subagents"
        subagent_dir.mkdir(parents=True)
        time.sleep(0.1)
        (subagent_dir / "not-a-jsonl.txt").touch()

        result = await daemon.get_newest_mtime(session_file)
        assert result == session_mtime
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestGetNewestMtime -v`
Expected: FAIL with "AttributeError: module 'legion.daemon' has no attribute 'get_newest_mtime'"

**Step 3: Write minimal implementation**

Add to `src/legion/daemon.py`:

```python
async def get_newest_mtime(session_file: Path) -> float | None:
    """Get newest mtime from session file and any subagent files.

    Returns None if session file doesn't exist.
    """
    session_path = anyio.Path(session_file)
    if not await session_path.exists():
        return None

    newest = (await session_path.stat()).st_mtime

    # Check subagent files
    subagents_dir = session_file.parent / session_file.stem / "subagents"
    subagents_path = anyio.Path(subagents_dir)
    if await subagents_path.exists():
        async for entry in subagents_path.iterdir():
            if entry.suffix == ".jsonl":
                stat = await entry.stat()
                newest = max(newest, stat.st_mtime)

    return newest
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestGetNewestMtime -v`
Expected: PASS (4 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(daemon): add get_newest_mtime for session liveness"
```

---

## Task 4: Add `new_session` to tmux.py

**Files:**
- Modify: `src/legion/tmux.py`
- Test: `tests/test_tmux.py` (create new file)

**Step 1: Write the failing test**

Create `tests/test_tmux.py`:

```python
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
            mock_run.assert_called_once_with([
                "tmux", "new-session", "-d",
                "-s", "my-session",
                "-n", "main",
                "echo hello",
            ])
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_tmux.py::TestNewSession -v`
Expected: FAIL with "AttributeError: module 'legion.tmux' has no attribute 'new_session'"

**Step 3: Write minimal implementation**

Add to `src/legion/tmux.py`:

```python
async def new_session(session: str, window: str, command: str) -> None:
    """Create new tmux session with command.

    Unlike create_session which just creates an empty session,
    this runs a command directly in the new session.

    Args:
        session: Session name
        window: Window name
        command: Command to run in the session
    """
    await run([
        "tmux", "new-session", "-d",
        "-s", session,
        "-n", window,
        command,
    ])
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_tmux.py::TestNewSession -v`
Expected: PASS

**Step 5: Commit**

```bash
jj desc -m "feat(tmux): add new_session with command support"
```

---

## Task 5: Update `start_controller` with session ID and shlex

**Files:**
- Modify: `src/legion/daemon.py:62-72`
- Test: `tests/test_daemon.py`

**Step 1: Write the failing test**

Add to `tests/test_daemon.py`:

```python
from unittest import mock


class TestStartController:
    """Test controller startup."""

    @pytest.mark.anyio
    async def test_uses_session_id_for_new_session(self, tmp_path: Path) -> None:
        """When no session file exists, uses --session-id."""
        with mock.patch("legion.daemon.tmux.new_session", new_callable=mock.AsyncMock) as mock_new:
            await daemon.start_controller(
                tmux_session="legion-abc-controller",
                project_id="proj-123",
                short="abc",
                workspace=tmp_path,
                session_id="session-uuid",
            )
            mock_new.assert_called_once()
            cmd = mock_new.call_args[0][2]
            assert "--session-id 'session-uuid'" in cmd

    @pytest.mark.anyio
    async def test_uses_resume_for_existing_session(self, tmp_path: Path) -> None:
        """When session file exists, uses --resume."""
        # Create fake session file
        session_file = daemon.get_session_file_path(tmp_path, "session-uuid")
        session_file.parent.mkdir(parents=True, exist_ok=True)
        session_file.touch()

        with mock.patch("legion.daemon.tmux.new_session", new_callable=mock.AsyncMock) as mock_new:
            await daemon.start_controller(
                tmux_session="legion-abc-controller",
                project_id="proj-123",
                short="abc",
                workspace=tmp_path,
                session_id="session-uuid",
            )
            cmd = mock_new.call_args[0][2]
            assert "--resume 'session-uuid'" in cmd

    @pytest.mark.anyio
    async def test_escapes_paths_with_shlex(self, tmp_path: Path) -> None:
        """Paths are properly escaped with shlex.quote."""
        # Create workspace with space in name
        weird_workspace = tmp_path / "my workspace"
        weird_workspace.mkdir()

        with mock.patch("legion.daemon.tmux.new_session", new_callable=mock.AsyncMock) as mock_new:
            await daemon.start_controller(
                tmux_session="legion-abc-controller",
                project_id="proj-123",
                short="abc",
                workspace=weird_workspace,
                session_id="session-uuid",
            )
            cmd = mock_new.call_args[0][2]
            # shlex.quote wraps in single quotes for paths with spaces
            assert "'my workspace'" in cmd or '"my workspace"' in cmd
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestStartController -v`
Expected: FAIL (signature mismatch - current function doesn't take session_id)

**Step 3: Write minimal implementation**

Replace `start_controller` in `src/legion/daemon.py`:

```python
import shlex  # Add to imports at top

async def start_controller(
    tmux_session: str,
    project_id: str,
    short: str,
    workspace: Path,
    session_id: str,
) -> None:
    """Start controller in tmux session.

    Uses --session-id for new sessions, --resume for existing ones.
    Passes command directly to tmux new-session (not send_keys).
    """
    session_file = get_session_file_path(workspace, session_id)

    # Decide whether to create new or resume
    if await anyio.Path(session_file).exists():
        session_flag = f"--resume {shlex.quote(session_id)}"
    else:
        session_flag = f"--session-id {shlex.quote(session_id)}"

    cmd = (
        f"cd {shlex.quote(str(workspace))} && "
        f"LEGION_DIR={shlex.quote(str(workspace))} "
        f"LINEAR_TEAM_ID={shlex.quote(project_id)} "
        f"LEGION_SHORT_ID={shlex.quote(short)} "
        f"claude --dangerously-skip-permissions {session_flag} "
        f"-p 'Use the legion-controller skill. Project: {project_id}'"
    )

    await tmux.new_session(tmux_session, "main", cmd)
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestStartController -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(daemon): update start_controller with session ID support"
```

---

## Task 6: Add `controller_needs_restart` to daemon.py

**Files:**
- Modify: `src/legion/daemon.py`
- Test: `tests/test_daemon.py`

**Step 1: Write the failing test**

Add to `tests/test_daemon.py`:

```python
class TestControllerNeedsRestart:
    """Test restart decision logic."""

    @pytest.mark.anyio
    async def test_returns_true_when_tmux_session_missing(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.touch()

        with mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock) as mock_exists:
            mock_exists.return_value = False
            result = await daemon.controller_needs_restart(
                tmux_session="legion-abc-controller",
                session_file=session_file,
                threshold=600,
            )
            assert result is True

    @pytest.mark.anyio
    async def test_returns_true_when_session_file_missing(self, tmp_path: Path) -> None:
        session_file = tmp_path / "missing.jsonl"

        with mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock) as mock_exists:
            mock_exists.return_value = True
            result = await daemon.controller_needs_restart(
                tmux_session="legion-abc-controller",
                session_file=session_file,
                threshold=600,
            )
            assert result is True

    @pytest.mark.anyio
    async def test_returns_true_when_file_stale(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.touch()
        # Make file old
        import os
        old_time = time.time() - 1000
        os.utime(session_file, (old_time, old_time))

        with mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock) as mock_exists:
            mock_exists.return_value = True
            result = await daemon.controller_needs_restart(
                tmux_session="legion-abc-controller",
                session_file=session_file,
                threshold=600,
            )
            assert result is True

    @pytest.mark.anyio
    async def test_returns_false_when_healthy(self, tmp_path: Path) -> None:
        session_file = tmp_path / "session.jsonl"
        session_file.touch()  # Fresh file

        with mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock) as mock_exists:
            mock_exists.return_value = True
            result = await daemon.controller_needs_restart(
                tmux_session="legion-abc-controller",
                session_file=session_file,
                threshold=600,
            )
            assert result is False

    @pytest.mark.anyio
    async def test_considers_subagent_activity(self, tmp_path: Path) -> None:
        """Active subagent keeps controller alive even if main file is stale."""
        session_file = tmp_path / "session.jsonl"
        session_file.touch()
        # Make session file old
        import os
        old_time = time.time() - 1000
        os.utime(session_file, (old_time, old_time))

        # But subagent is fresh
        subagent_dir = tmp_path / "session" / "subagents"
        subagent_dir.mkdir(parents=True)
        (subagent_dir / "agent-1.jsonl").touch()

        with mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock) as mock_exists:
            mock_exists.return_value = True
            result = await daemon.controller_needs_restart(
                tmux_session="legion-abc-controller",
                session_file=session_file,
                threshold=600,
            )
            assert result is False  # Subagent activity keeps it alive
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestControllerNeedsRestart -v`
Expected: FAIL with "AttributeError: module 'legion.daemon' has no attribute 'controller_needs_restart'"

**Step 3: Write minimal implementation**

Add to `src/legion/daemon.py`:

```python
async def controller_needs_restart(
    tmux_session: str,
    session_file: Path,
    threshold: int = 600,
) -> bool:
    """Check if controller needs restart.

    Args:
        tmux_session: tmux session name
        session_file: Path to Claude Code session file
        threshold: Staleness threshold in seconds (default 10 min)

    Returns:
        True if controller should be restarted
    """
    if not await tmux.session_exists(tmux_session):
        return True

    newest_mtime = await get_newest_mtime(session_file)
    if newest_mtime is None:
        return True

    age = time.time() - newest_mtime
    return age > threshold
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestControllerNeedsRestart -v`
Expected: PASS (5 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(daemon): add controller_needs_restart logic"
```

---

## Task 7: Update `health_loop` with restart logic

**Files:**
- Modify: `src/legion/daemon.py:75-100`
- Test: `tests/test_daemon.py`

**Step 1: Write the failing test**

Add to `tests/test_daemon.py`:

```python
class TestHealthLoop:
    """Test health monitoring loop."""

    @pytest.mark.anyio
    async def test_restarts_controller_when_needed(self, tmp_path: Path) -> None:
        """Health loop restarts controller when needs_restart is True."""
        restart_count = 0
        check_count = 0

        async def mock_needs_restart(*args, **kwargs) -> bool:
            nonlocal check_count
            check_count += 1
            return check_count == 1  # Need restart only on first check

        async def mock_start_controller(*args, **kwargs) -> None:
            nonlocal restart_count
            restart_count += 1

        with (
            mock.patch("legion.daemon.controller_needs_restart", side_effect=mock_needs_restart),
            mock.patch("legion.daemon.start_controller", side_effect=mock_start_controller),
            mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock, return_value=False),
            mock.patch("legion.daemon.tmux.kill_session", new_callable=mock.AsyncMock),
            mock.patch("anyio.sleep", new_callable=mock.AsyncMock) as mock_sleep,
        ):
            # Stop after 2 iterations
            mock_sleep.side_effect = [None, None, Exception("stop")]

            with pytest.raises(Exception, match="stop"):
                await daemon.health_loop(
                    tmux_session="legion-abc-controller",
                    project_id="proj-123",
                    short="abc",
                    workspace=tmp_path,
                    session_id="session-uuid",
                    check_interval=1.0,
                    staleness_threshold=600,
                    restart_cooldown=0.0,
                )

            assert restart_count == 1

    @pytest.mark.anyio
    async def test_enforces_restart_cooldown(self, tmp_path: Path) -> None:
        """Cooldown prevents rapid restarts."""
        restart_times: list[float] = []

        async def mock_start_controller(*args, **kwargs) -> None:
            restart_times.append(time.time())

        with (
            mock.patch("legion.daemon.controller_needs_restart", new_callable=mock.AsyncMock, return_value=True),
            mock.patch("legion.daemon.start_controller", side_effect=mock_start_controller),
            mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock, return_value=False),
            mock.patch("legion.daemon.tmux.kill_session", new_callable=mock.AsyncMock),
            mock.patch("anyio.sleep", new_callable=mock.AsyncMock) as mock_sleep,
        ):
            # Stop after 2 restarts
            call_count = 0
            async def counting_sleep(duration: float) -> None:
                nonlocal call_count
                call_count += 1
                if call_count > 3:
                    raise Exception("stop")

            mock_sleep.side_effect = counting_sleep

            with pytest.raises(Exception, match="stop"):
                await daemon.health_loop(
                    tmux_session="legion-abc-controller",
                    project_id="proj-123",
                    short="abc",
                    workspace=tmp_path,
                    session_id="session-uuid",
                    check_interval=0.0,
                    staleness_threshold=600,
                    restart_cooldown=60.0,  # 60s cooldown
                )

            # Should have been called with cooldown wait
            cooldown_calls = [c for c in mock_sleep.call_args_list if c[0][0] > 0]
            assert len(cooldown_calls) >= 1

    @pytest.mark.anyio
    async def test_handles_start_controller_failure(self, tmp_path: Path) -> None:
        """Failed start_controller doesn't crash the loop."""
        start_attempts = 0

        async def failing_start(*args, **kwargs) -> None:
            nonlocal start_attempts
            start_attempts += 1
            if start_attempts == 1:
                raise RuntimeError("tmux failed")
            # Second attempt succeeds

        with (
            mock.patch("legion.daemon.controller_needs_restart", new_callable=mock.AsyncMock, return_value=True),
            mock.patch("legion.daemon.start_controller", side_effect=failing_start),
            mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock, return_value=False),
            mock.patch("legion.daemon.tmux.kill_session", new_callable=mock.AsyncMock),
            mock.patch("anyio.sleep", new_callable=mock.AsyncMock) as mock_sleep,
        ):
            call_count = 0
            async def counting_sleep(duration: float) -> None:
                nonlocal call_count
                call_count += 1
                if call_count > 2:
                    raise Exception("stop")

            mock_sleep.side_effect = counting_sleep

            with pytest.raises(Exception, match="stop"):
                await daemon.health_loop(
                    tmux_session="legion-abc-controller",
                    project_id="proj-123",
                    short="abc",
                    workspace=tmp_path,
                    session_id="session-uuid",
                    check_interval=0.0,
                    staleness_threshold=600,
                    restart_cooldown=0.0,
                )

            # Should have attempted twice despite first failure
            assert start_attempts >= 2
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestHealthLoop -v`
Expected: FAIL (signature mismatch or behavior difference)

**Step 3: Write minimal implementation**

Replace `health_loop` in `src/legion/daemon.py`:

```python
async def health_loop(
    tmux_session: str,
    project_id: str,
    short: str,
    workspace: Path,
    session_id: str,
    check_interval: float = 60.0,
    staleness_threshold: int = 600,
    restart_cooldown: float = 60.0,
) -> None:
    """Monitor controller health and restart if needed.

    Never exits - always restarts controller on failure.
    """
    last_restart: float = 0.0
    session_file = get_session_file_path(workspace, session_id)

    while True:
        await anyio.sleep(check_interval)

        needs_restart = await controller_needs_restart(
            tmux_session, session_file, staleness_threshold
        )

        if needs_restart:
            # Enforce cooldown
            elapsed = time.time() - last_restart
            if elapsed < restart_cooldown:
                wait_time = restart_cooldown - elapsed
                print(f"Restart cooldown: waiting {wait_time:.0f}s")
                await anyio.sleep(wait_time)

            print("Restarting controller...")

            # Kill existing session if it exists
            if await tmux.session_exists(tmux_session):
                await tmux.kill_session(tmux_session)

            try:
                await start_controller(
                    tmux_session, project_id, short, workspace, session_id
                )
                last_restart = time.time()
            except Exception as e:
                print(f"Failed to start controller: {e}")
                # Don't update last_restart - next iteration will try again
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestHealthLoop -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj desc -m "feat(daemon): update health_loop with restart logic"
```

---

## Task 8: Update `start` function to use session IDs

**Files:**
- Modify: `src/legion/daemon.py:102-138`
- Test: `tests/test_daemon.py`

**Step 1: Write the failing test**

Add to `tests/test_daemon.py`:

```python
class TestStart:
    """Test daemon start function."""

    @pytest.mark.anyio
    async def test_computes_and_uses_session_id(self, tmp_path: Path) -> None:
        """Start computes session ID and passes to start_controller."""
        workspace = tmp_path / "workspace"
        workspace.mkdir()
        (workspace / ".jj").mkdir()

        captured_session_id = None

        async def mock_start_controller(tmux_session, project_id, short, workspace, session_id):
            nonlocal captured_session_id
            captured_session_id = session_id

        with (
            mock.patch("legion.daemon.validate_workspace", new_callable=mock.AsyncMock),
            mock.patch("legion.daemon.tmux.session_exists", new_callable=mock.AsyncMock, return_value=False),
            mock.patch("legion.daemon.start_controller", side_effect=mock_start_controller),
            mock.patch("legion.daemon.health_loop", new_callable=mock.AsyncMock),
        ):
            await daemon.start(
                project_id="7b4f0862-b775-4cb0-9a67-85400c6f44a8",
                workspace=workspace,
                state_dir=tmp_path / "state",
            )

            # Session ID should be a valid UUID computed from project_id
            assert captured_session_id is not None
            import uuid
            uuid.UUID(captured_session_id)  # Should not raise
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_daemon.py::TestStart -v`
Expected: FAIL (start_controller called with wrong signature)

**Step 3: Write minimal implementation**

Update the import and `start` function in `src/legion/daemon.py`:

```python
from legion.state.types import compute_controller_session_id  # Add to imports

async def start(project_id: str, workspace: Path, state_dir: Path) -> None:
    """Start the Legion swarm."""
    validate_project_id(project_id)
    await validate_workspace(workspace)

    short = get_short_id(project_id)
    session = controller_session_name(short)
    session_id = compute_controller_session_id(project_id)

    if await tmux.session_exists(session):
        raise RuntimeError(
            f"Swarm already running for {project_id}. "
            f"Use 'legion stop {project_id}' first."
        )

    print(f"Starting Legion for project: {project_id}")
    print(f"Session ID: {short}")
    print(f"Workspace: {workspace}")

    # Create state directory
    state_dir.mkdir(parents=True, exist_ok=True)

    # Start controller directly (no need to create empty session first)
    await start_controller(session, project_id, short, workspace, session_id)
    print(f"Started controller in tmux session: {session}")

    print()
    print(f"To attach: tmux attach -t {session}")
    print(f"To view:   tmux capture-pane -t {session}:main -p")
    print()

    # Run health loop
    await health_loop(
        tmux_session=session,
        project_id=project_id,
        short=short,
        workspace=workspace,
        session_id=session_id,
    )
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_daemon.py::TestStart -v`
Expected: PASS

**Step 5: Run all tests**

Run: `uv run pytest -v`
Expected: All tests PASS

**Step 6: Commit**

```bash
jj desc -m "feat(daemon): update start to use session IDs and new health_loop"
```

---

## Task 9: Manual Integration Test

**Step 1: Start daemon with test project**

```bash
# Create test directory
mkdir -p /tmp/legion-test
cd /tmp/legion-test
jj git init

# Start daemon
cd /home/sami/legion/default
uv run legion start 7b4f0862-b775-4cb0-9a67-85400c6f44a8
```

**Step 2: Verify tmux session created**

```bash
tmux list-sessions | grep legion
```

Expected: Session exists with name like `legion-XXXXX-controller`

**Step 3: Kill controller and verify restart**

```bash
# Find and kill the controller session
tmux kill-session -t legion-XXXXX-controller

# Wait 60-120 seconds, verify it restarts
sleep 120
tmux list-sessions | grep legion
```

Expected: Session reappears after restart

**Step 4: Stop daemon**

```bash
# Stop cleanly
uv run legion stop 7b4f0862-b775-4cb0-9a67-85400c6f44a8
```

**Step 5: Final commit**

```bash
jj desc -m "feat: persistent controller daemon with session-based liveness

- Add compute_controller_session_id for deterministic session IDs
- Add session file path encoding matching Claude Code
- Add get_newest_mtime for session + subagent liveness detection
- Add controller_needs_restart logic
- Update health_loop to restart stale/crashed controllers
- Update start_controller to use --session-id or --resume
- Add tmux.new_session for direct command execution
- Add shlex escaping for all paths"
```

---

Plan complete and saved to `docs/plans/2026-02-02-feat-persistent-controller-daemon-plan.md`. Two execution options:

**1. Subagent-Driven (this session)** - I dispatch fresh subagent per task, review between tasks, fast iteration

**2. Parallel Session (separate)** - Open new session with executing-plans, batch execution with checkpoints

Which approach?
