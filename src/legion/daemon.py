"""Legion daemon: manages tmux session and controller lifecycle."""

import re
import time
from pathlib import Path

import anyio

from legion import short_id as short_id_mod
from legion import tmux


def get_short_id(project_id: str) -> str:
    """Get short ID from project ID.

    If project_id looks like a UUID, shorten it. Otherwise use as-is.
    """
    clean = project_id.replace("-", "")
    if len(clean) == 32 and all(c in "0123456789abcdefABCDEF" for c in clean):
        return short_id_mod.uuid_to_short(project_id)
    return project_id


def controller_session_name(short: str) -> str:
    """Get tmux session name for controller."""
    return f"legion-{short}-controller"


def worker_session_prefix(short: str) -> str:
    """Get prefix for worker session names."""
    return f"legion-{short}-worker-"


def validate_project_id(project_id: str) -> None:
    """Validate project ID to prevent injection."""
    if not re.match(r"^[a-zA-Z0-9_-]+$", project_id):
        raise ValueError(
            "PROJECT_ID must contain only letters, numbers, hyphens, and underscores"
        )


async def validate_workspace(workspace: Path) -> None:
    """Validate the workspace directory."""
    if not workspace.exists():
        raise ValueError(f"Workspace does not exist: {workspace}")

    if not (workspace / ".jj").exists():
        raise ValueError(f"Not a jj repository: {workspace}")

    # Check for default workspace
    stdout, _, _ = await tmux.run(["jj", "workspace", "list", "-R", str(workspace)])
    if "default:" not in stdout:
        raise ValueError("Must run from the default jj workspace")

    # Check for git remote (warn if missing)
    stdout, _, _ = await tmux.run(["jj", "git", "remote", "list", "-R", str(workspace)])
    if not stdout.strip():
        print("WARNING: No git remote configured. Push/PR steps will fail.")
        print("  Add one with: jj git remote add origin <url>")


async def start_controller(
    session: str, project_id: str, short: str, workspace: Path
) -> None:
    """Start the controller in the tmux session."""
    cmd = (
        f"cd '{workspace}' && "
        f"LEGION_DIR='{workspace}' LINEAR_TEAM_ID={project_id} LEGION_SHORT_ID={short} "
        f"claude --dangerously-skip-permissions -p "
        f"'Use the legion-controller skill. Project: {project_id}'"
    )
    await tmux.send_keys(session, "main", cmd)


async def health_loop(
    short: str,
    state_dir: Path,
    interval_seconds: int = 180,
) -> None:
    """Monitor controller heartbeat."""
    session = controller_session_name(short)
    heartbeat_file = state_dir / "heartbeat"

    while True:
        await anyio.sleep(interval_seconds)

        if not await tmux.session_exists(session):
            print(f"Session {session} no longer exists, exiting health loop")
            break

        if not heartbeat_file.exists():
            print(f"WARNING: No heartbeat file for {short}")
            continue

        age = time.time() - heartbeat_file.stat().st_mtime
        if age > interval_seconds:
            print(f"WARNING: Controller heartbeat stale ({age:.0f}s) for {short}")
        else:
            print(f"Controller heartbeat OK for {short}")


async def start(project_id: str, workspace: Path, state_dir: Path) -> None:
    """Start the Legion swarm."""
    validate_project_id(project_id)
    await validate_workspace(workspace)

    short = get_short_id(project_id)
    session = controller_session_name(short)

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

    # Create tmux session with window named "main"
    await tmux.create_session(session, "main")
    print(f"Created tmux session: {session}")

    # Start controller
    await start_controller(session, project_id, short, workspace)
    print("Started controller")

    print()
    print(f"To attach: tmux attach -t {session}")
    print(f"To view:   tmux capture-pane -t {session}:main -p")
    print()

    # Run health loop
    await health_loop(short, state_dir)


async def stop(project_id: str, state_dir: Path) -> None:
    """Stop the Legion swarm."""
    validate_project_id(project_id)
    short = get_short_id(project_id)
    session = controller_session_name(short)

    print(f"Stopping Legion for project: {project_id}")

    # Kill controller session
    if await tmux.session_exists(session):
        await tmux.kill_session(session)
        print(f"Killed controller session: {session}")
    else:
        print(f"No controller session found: {session}")

    # Kill any worker sessions matching this short ID
    prefix = worker_session_prefix(short)
    for sess in await tmux.list_sessions():
        if sess.startswith(prefix):
            await tmux.kill_session(sess)
            print(f"Killed worker session: {sess}")


async def status(project_id: str, state_dir: Path) -> None:
    """Show Legion swarm status."""
    validate_project_id(project_id)
    short = get_short_id(project_id)
    session = controller_session_name(short)

    print(f"Legion Status: {project_id}")
    print(f"Session ID: {short}")
    print("=" * 40)

    # Check controller session
    if await tmux.session_exists(session):
        print(f"Controller ({session}): RUNNING")
    else:
        print(f"Controller ({session}): NOT RUNNING")

    # Check worker sessions
    prefix = worker_session_prefix(short)
    workers = [s for s in await tmux.list_sessions() if s.startswith(prefix)]
    if workers:
        print(f"Workers: {len(workers)}")
        for w in workers:
            issue_id = w.removeprefix(prefix)
            print(f"  - {issue_id}")
    else:
        print("Workers: none")

    print()

    heartbeat_file = state_dir / "heartbeat"
    if heartbeat_file.exists():
        age = time.time() - heartbeat_file.stat().st_mtime
        print(f"Heartbeat: {age:.0f}s ago")
    else:
        print("Heartbeat: NO FILE")
