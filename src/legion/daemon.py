"""Legion daemon: manages tmux session and controller lifecycle."""

import logging
import re
import shlex
import time
from pathlib import Path
from typing import TypeGuard

import anyio
import click

from legion import short_id as short_id_mod
from legion import tmux
from legion.state import types
from legion.state.types import WorkerModeLiteral

logger = logging.getLogger(__name__)

# Default staleness threshold for controller and worker health checks (10 minutes)
DEFAULT_STALENESS_THRESHOLD = 600


def is_valid_worker_mode(mode: str) -> TypeGuard[WorkerModeLiteral]:
    """Check if a string is a valid worker mode.

    This provides type narrowing - after calling this function, the type
    checker knows that the mode is a WorkerModeLiteral if True is returned.
    """
    return mode in ("architect", "plan", "implement", "review", "merge")


def get_short_id(project_id: str) -> str:
    """Get short ID from project ID.

    If project_id looks like a UUID, shorten it. Otherwise use as-is.
    """
    clean = project_id.replace("-", "")
    try:
        # If it parses as a hex UUID (32 hex chars), shorten it
        if len(clean) == 32:
            int(clean, 16)
            return short_id_mod.uuid_to_short(project_id)
    except ValueError:
        pass
    return project_id


def session_name(short: str) -> str:
    """Get tmux session name for Legion instance."""
    return f"legion-{short}"


def validate_project_id(project_id: str) -> None:
    """Validate project ID to prevent injection."""
    if not re.match(r"^[a-zA-Z0-9_-]+$", project_id):
        raise ValueError(
            "PROJECT_ID must contain only letters, numbers, hyphens, and underscores"
        )


def get_session_file_path(workspace: Path, session_id: str) -> Path:
    """Get path to Claude Code session file.

    Claude Code encodes workspace paths by replacing all non-alphanumeric
    characters with dashes. Examples:
    - /home/sami/legion/default -> -home-sami-legion-default
    - /home/sami/.dotfiles -> -home-sami--dotfiles
    """
    encoded = re.sub(r"[^a-zA-Z0-9]", "-", str(workspace))
    return Path.home() / ".claude" / "projects" / encoded / f"{session_id}.jsonl"


async def get_newest_mtime(session_file: Path) -> float | None:
    """Get newest mtime from session file and any subagent files.

    Returns None if session file doesn't exist or is inaccessible.
    """
    session_path = anyio.Path(session_file)
    if not await session_path.exists():
        return None

    try:
        newest = (await session_path.stat()).st_mtime
    except (PermissionError, OSError):
        # Session file exists but is inaccessible
        return None

    # Check subagent files
    subagents_dir = session_file.parent / session_file.stem / "subagents"
    subagents_path = anyio.Path(subagents_dir)
    try:
        if await subagents_path.exists():
            async for entry in subagents_path.iterdir():
                if entry.suffix == ".jsonl":
                    try:
                        stat = await entry.stat()
                        newest = max(newest, stat.st_mtime)
                    except (PermissionError, OSError):
                        # Skip inaccessible subagent files
                        continue
    except (PermissionError, OSError):
        # Subagents directory inaccessible, use session file mtime only
        pass

    return newest


async def check_worker_health(
    tmux_session: str,
    team_id: str,
    workspaces_dir: Path,
    staleness_threshold: int = DEFAULT_STALENESS_THRESHOLD,
) -> None:
    """Check worker health and kill stale windows.

    Workers run as windows within the Legion tmux session.
    Window names follow the format: {issue}-{mode} (e.g., "leg-18-architect").
    Worker workspaces are sibling directories to the default workspace.

    Args:
        tmux_session: Name of the tmux session containing workers
        team_id: Linear team UUID for computing session IDs
        workspaces_dir: Parent directory containing worker workspaces as siblings
        staleness_threshold: Seconds of inactivity before killing (default 10 min)
    """
    windows = await tmux.list_windows(tmux_session)

    for window in windows:
        if window == "main":
            continue  # Skip controller window

        # Parse {issue}-{mode} from window name (e.g., "leg-18-architect")
        # Split from right since issue identifier contains a hyphen
        parts = window.rsplit("-", 1)
        if len(parts) != 2:
            continue  # Invalid format, skip

        issue_lower, mode = parts
        if not is_valid_worker_mode(mode):
            continue  # Not a valid worker mode

        issue_id = issue_lower.upper()  # Normalize for session ID computation

        # Compute session file path
        session_id = types.compute_session_id(team_id, issue_id, mode)
        workspace = workspaces_dir / issue_lower
        session_file = get_session_file_path(workspace, session_id)

        # Check staleness
        mtime = await get_newest_mtime(session_file)
        if mtime is None:
            # No session file yet - worker just started
            continue

        age = time.time() - mtime
        if age > staleness_threshold:
            logger.info("Killing stale worker: %s (age: %.0fs)", window, age)
            try:
                await tmux.kill_window(tmux_session, window)
            except Exception as e:
                logger.warning("Failed to kill window %s: %s", window, e)


async def validate_workspace(workspace: Path) -> None:
    """Validate the workspace directory."""
    workspace_path = anyio.Path(workspace)
    if not await workspace_path.exists():
        raise ValueError(f"Workspace does not exist: {workspace}")

    if not await (workspace_path / ".jj").exists():
        raise ValueError(f"Not a jj repository: {workspace}")

    # Check for default workspace
    stdout, _, _ = await tmux.run(["jj", "workspace", "list", "-R", str(workspace)])
    if "default:" not in stdout:
        raise ValueError("Must run from the default jj workspace")

    # Check for git remote (warn if missing)
    stdout, _, _ = await tmux.run(["jj", "git", "remote", "list", "-R", str(workspace)])
    if not stdout.strip():
        click.echo(
            click.style(
                "WARNING: No git remote configured. Push/PR steps will fail.",
                fg="yellow",
            )
        )
        click.echo(
            click.style("  Add one with: jj git remote add origin <url>", fg="yellow")
        )


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

    # Start interactive Claude with initial prompt as positional argument
    prompt = f"/legion-controller Project: {project_id}"
    cmd = (
        f"cd {shlex.quote(str(workspace))} && "
        f"LEGION_DIR={shlex.quote(str(workspace))} "
        f"LINEAR_TEAM_ID={shlex.quote(project_id)} "
        f"LEGION_SHORT_ID={shlex.quote(short)} "
        f"claude --dangerously-skip-permissions {session_flag} "
        f"{shlex.quote(prompt)}"
    )

    await tmux.new_session(tmux_session, "main", cmd)


async def controller_needs_restart(
    tmux_session: str,
    session_file: Path,
    threshold: int = DEFAULT_STALENESS_THRESHOLD,
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


async def health_loop(
    tmux_session: str,
    project_id: str,
    short: str,
    workspace: Path,
    session_id: str,
    check_interval: float = 60.0,
    staleness_threshold: int = DEFAULT_STALENESS_THRESHOLD,
    restart_cooldown: float = 60.0,
) -> None:
    """Monitor controller health and restart if needed.

    Never exits - always restarts controller on failure.
    """
    last_restart: float = 0.0
    session_file = get_session_file_path(workspace, session_id)

    while True:
        await anyio.sleep(check_interval)

        # Check worker health first (kill stale worker windows)
        # Wrapped in try/except to prevent daemon crash if worker health check fails
        try:
            await check_worker_health(
                tmux_session, project_id, workspace.parent, staleness_threshold
            )
        except Exception as e:
            logger.error("Worker health check failed: %s", e)

        needs_restart = await controller_needs_restart(
            tmux_session, session_file, staleness_threshold
        )

        if needs_restart:
            # Enforce cooldown
            elapsed = time.time() - last_restart
            if elapsed < restart_cooldown:
                wait_time = restart_cooldown - elapsed
                logger.info("Restart cooldown: waiting %.0fs", wait_time)
                await anyio.sleep(wait_time)

            logger.info("Restarting controller...")

            # Kill existing session if it exists
            if await tmux.session_exists(tmux_session):
                await tmux.kill_session(tmux_session)

            try:
                await start_controller(
                    tmux_session, project_id, short, workspace, session_id
                )
                last_restart = time.time()
            except Exception as e:
                logger.error("Failed to start controller: %s", e)
                # Don't update last_restart - next iteration will try again


async def start(project_id: str, workspace: Path, state_dir: Path) -> None:
    """Start the Legion swarm."""
    validate_project_id(project_id)
    await validate_workspace(workspace)

    short = get_short_id(project_id)
    session = session_name(short)
    session_id = types.compute_controller_session_id(project_id)

    if await tmux.session_exists(session):
        raise RuntimeError(
            f"Swarm already running for {project_id}. "
            f"Use 'legion stop {project_id}' first."
        )

    click.echo(f"Starting Legion for project: {project_id}")
    click.echo(f"Session ID: {short}")
    click.echo(f"Workspace: {workspace}")

    # Create state directory
    state_dir.mkdir(parents=True, exist_ok=True)

    # Start controller directly (no need to create empty session first)
    await start_controller(session, project_id, short, workspace, session_id)
    click.echo(f"Started controller in tmux session: {session}")

    click.echo()
    click.echo(f"To attach: tmux attach -t {session}")
    click.echo(f"To view:   tmux capture-pane -t {session}:main -p")
    click.echo()

    # Run health loop
    await health_loop(
        tmux_session=session,
        project_id=project_id,
        short=short,
        workspace=workspace,
        session_id=session_id,
    )


async def stop(project_id: str, state_dir: Path) -> None:  # noqa: ARG001
    """Stop the Legion swarm.

    Args:
        project_id: Linear project/team UUID
        state_dir: State directory (unused, kept for API compatibility)
    """
    _ = state_dir  # Explicitly mark as intentionally unused
    validate_project_id(project_id)
    short = get_short_id(project_id)
    session = session_name(short)

    click.echo(f"Stopping Legion for project: {project_id}")

    # Kill controller session (this also kills all worker windows within it)
    if await tmux.session_exists(session):
        await tmux.kill_session(session)
        click.echo(f"Killed controller session: {session}")
    else:
        click.echo(f"No controller session found: {session}")


async def status(project_id: str, state_dir: Path) -> None:
    """Show Legion swarm status."""
    validate_project_id(project_id)
    short = get_short_id(project_id)
    session = session_name(short)

    click.echo(f"Legion Status: {project_id}")
    click.echo(f"Session ID: {short}")
    click.echo("=" * 40)

    # Check controller session
    if await tmux.session_exists(session):
        click.echo(f"Controller ({session}): RUNNING")
    else:
        click.echo(f"Controller ({session}): NOT RUNNING")

    # Check worker windows within controller session
    if await tmux.session_exists(session):
        windows = await tmux.list_windows(session)
        workers = [w for w in windows if w != "main"]
        if workers:
            click.echo(f"Workers: {len(workers)}")
            for w in workers:
                click.echo(f"  - {w}")
        else:
            click.echo("Workers: none")
    else:
        click.echo("Workers: N/A (controller not running)")

    click.echo()

    heartbeat_path = anyio.Path(state_dir / "heartbeat")
    if await heartbeat_path.exists():
        stat_result = await heartbeat_path.stat()
        age = time.time() - stat_result.st_mtime
        click.echo(f"Heartbeat: {age:.0f}s ago")
    else:
        click.echo("Heartbeat: NO FILE")
