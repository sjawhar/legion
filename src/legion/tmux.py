"""tmux operations using anyio."""

import anyio


async def run(cmd: list[str]) -> tuple[str, str, int]:
    """Run a command and return (stdout, stderr, returncode)."""
    result = await anyio.run_process(cmd, check=False)
    return (
        result.stdout.decode().strip(),
        result.stderr.decode().strip(),
        result.returncode,
    )


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
    stdout, _, rc = await run([
        "tmux", "list-windows", "-t", session, "-F", "#{window_name}"
    ])
    if rc != 0:
        return []
    return [w for w in stdout.split("\n") if w]


async def new_window(session: str, name: str) -> None:
    """Create a new window in a session."""
    _ = await run(["tmux", "new-window", "-t", session, "-n", name, "-d"])


async def kill_window(session: str, window: str) -> None:
    """Kill a window in a session."""
    _ = await run(["tmux", "kill-window", "-t", f"{session}:{window}"])


async def send_keys(session: str, window: str, keys: str) -> None:
    """Send keys to a tmux window."""
    _ = await run(["tmux", "send-keys", "-t", f"{session}:{window}", keys, "Enter"])
