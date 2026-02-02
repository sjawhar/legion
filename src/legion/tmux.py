"""tmux operations using anyio."""

import anyio


async def run(cmd: list[str]) -> tuple[str, str, int]:
    """Run a command and return (stdout, stderr, returncode).

    Handles edge cases gracefully:
    - Command not found: returns ("", error message, 127)
    - Non-UTF8 output: uses errors='replace' to substitute invalid bytes
    """
    try:
        result = await anyio.run_process(cmd, check=False)
        return (
            result.stdout.decode(errors="replace").strip(),
            result.stderr.decode(errors="replace").strip(),
            result.returncode,
        )
    except FileNotFoundError as e:
        # Command not found - return standard shell exit code 127
        return ("", str(e), 127)
    except OSError as e:
        # Other OS errors (permission denied, etc.)
        return ("", str(e), 126)


async def list_sessions() -> list[str]:
    """List all tmux session names."""
    stdout, _, rc = await run(["tmux", "list-sessions", "-F", "#{session_name}"])
    if rc != 0:
        return []
    return [s for s in stdout.split("\n") if s.strip()]


async def session_exists(session: str) -> bool:
    """Check if a tmux session exists."""
    _, _, rc = await run(["tmux", "has-session", "-t", session])
    return rc == 0


async def create_session(session: str, window: str) -> None:
    """Create a new tmux session with a named window."""
    _ = await run(["tmux", "new-session", "-d", "-s", session, "-n", window])


async def kill_session(session: str) -> None:
    """Kill a tmux session."""
    _ = await run(["tmux", "kill-session", "-t", session])


async def list_windows(session: str) -> list[str]:
    """List window names in a session."""
    stdout, _, rc = await run(
        ["tmux", "list-windows", "-t", session, "-F", "#{window_name}"]
    )
    if rc != 0:
        return []
    return [w for w in stdout.split("\n") if w.strip()]


async def new_window(session: str, name: str) -> None:
    """Create a new window in a session."""
    _ = await run(["tmux", "new-window", "-t", session, "-n", name, "-d"])


async def kill_window(session: str, window: str) -> None:
    """Kill a window in a session."""
    _ = await run(["tmux", "kill-window", "-t", f"{session}:{window}"])


async def send_keys(session: str, window: str, keys: str) -> None:
    """Send keys to a tmux window."""
    _ = await run(["tmux", "send-keys", "-t", f"{session}:{window}", keys, "Enter"])


async def new_session(session: str, window: str, command: str) -> None:
    """Create new tmux session with command.

    Unlike create_session which just creates an empty session,
    this runs a command directly in the new session.

    Args:
        session: Session name
        window: Window name
        command: Command to run in the session
    """
    _ = await run(
        [
            "tmux",
            "new-session",
            "-d",
            "-s",
            session,
            "-n",
            window,
            command,
        ]
    )
