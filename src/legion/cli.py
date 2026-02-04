"""Legion CLI entry point."""

import json
import os
import re
import subprocess
import urllib.request
from pathlib import Path

import anyio
import click

from legion import daemon

# Required Claude Code plugins
REQUIRED_PLUGINS = [
    ("superpowers@claude-plugins-official", "TDD, debugging, workflows"),
    ("compound-engineering@every-marketplace", "Research agents, review agents"),
]

# UUID regex pattern
UUID_PATTERN = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", re.IGNORECASE
)


def resolve_team_id(team_ref: str) -> str:
    """Resolve a team reference to a UUID.

    Args:
        team_ref: Either a UUID or a team key (e.g., "LEG")

    Returns:
        The team UUID

    Raises:
        click.ClickException: If team cannot be resolved
    """
    # If it's already a UUID, return it
    if UUID_PATTERN.match(team_ref):
        return team_ref

    # Check for cached team mapping
    cache_file = Path.home() / ".legion" / "teams.json"
    if cache_file.exists():
        with open(cache_file) as f:
            teams = json.load(f)
            key_upper = team_ref.upper()
            if key_upper in teams:
                team = teams[key_upper]
                click.echo(f"Using cached: {team_ref} → {team['name']} ({team['id']})")
                return team["id"]

    # Try LINEAR_API_KEY if available
    api_key = os.environ.get("LINEAR_API_KEY")
    if api_key:
        return _lookup_team_via_api(team_ref, api_key)

    # No cache, no API key
    raise click.ClickException(
        f"'{team_ref}' is not a UUID.\n"
        f"Run 'legion teams' to cache team mappings, or set LINEAR_API_KEY."
    )


def _lookup_team_via_api(team_ref: str, api_key: str) -> str:
    """Look up team via Linear GraphQL API."""
    query = """
    query GetTeam($key: String!) {
        team(key: $key) {
            id
            name
        }
    }
    """

    payload = json.dumps({"query": query, "variables": {"key": team_ref.upper()}})
    req = urllib.request.Request(
        "https://api.linear.app/graphql",
        data=payload.encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Authorization": api_key,
        },
    )

    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        raise click.ClickException(f"Failed to look up team '{team_ref}': {e}") from e

    team = data.get("data", {}).get("team")
    if not team:
        raise click.ClickException(f"Team '{team_ref}' not found in Linear")

    click.echo(f"Resolved: {team_ref} → {team['name']} ({team['id']})")
    return team["id"]


@click.group()
def cli() -> None:
    """Legion: Autonomous development swarm using Claude Code agents."""


@cli.command()
@click.argument("team")
@click.option(
    "--workspace",
    "-w",
    type=click.Path(exists=True, file_okay=False, path_type=Path),
    default=Path.cwd(),
    help="Path to the workspace (default: current directory)",
)
@click.option(
    "--state-dir",
    type=click.Path(file_okay=False, path_type=Path),
    default=None,
    help="State directory (default: ~/.legion/TEAM_ID)",
)
def start(team: str, workspace: Path, state_dir: Path | None) -> None:
    """Start the swarm for a Linear team.

    TEAM can be a team key (e.g., LEG) or a UUID.
    """
    team_id = resolve_team_id(team)

    if state_dir is None:
        state_dir = Path.home() / ".legion" / team_id

    try:
        anyio.run(daemon.start, team_id, workspace.resolve(), state_dir)
    except ValueError as e:
        raise click.ClickException(str(e)) from None
    except RuntimeError as e:
        raise click.ClickException(str(e)) from None


@cli.command()
@click.argument("team")
@click.option(
    "--state-dir",
    type=click.Path(file_okay=False, path_type=Path),
    default=None,
    help="State directory (default: ~/.legion/TEAM_ID)",
)
def stop(team: str, state_dir: Path | None) -> None:
    """Stop the swarm.

    TEAM can be a team key (e.g., LEG) or a UUID.
    """
    team_id = resolve_team_id(team)

    if state_dir is None:
        state_dir = Path.home() / ".legion" / team_id

    anyio.run(daemon.stop, team_id, state_dir)


@cli.command()
@click.argument("team")
@click.option(
    "--state-dir",
    type=click.Path(file_okay=False, path_type=Path),
    default=None,
    help="State directory (default: ~/.legion/TEAM_ID)",
)
def status(team: str, state_dir: Path | None) -> None:
    """Check swarm status.

    TEAM can be a team key (e.g., LEG) or a UUID.
    """
    team_id = resolve_team_id(team)

    if state_dir is None:
        state_dir = Path.home() / ".legion" / team_id

    anyio.run(daemon.status, team_id, state_dir)


@cli.command()
def teams() -> None:
    """List and cache Linear teams.

    Uses Claude with Linear MCP to fetch teams and saves them to ~/.legion/teams.json.
    After running this, you can use team keys (e.g., LEG) instead of UUIDs.
    """
    click.echo("Fetching teams from Linear via Claude...")

    # Use Claude to get teams via Linear MCP
    result = subprocess.run(
        [
            "claude",
            "--dangerously-skip-permissions",
            "--output-format",
            "json",
            "--max-turns",
            "3",
            "Use mcp__linear__list_teams to list all teams. "
            "Then output ONLY a JSON object where each key is the team's key "
            "(uppercase letters like LEG, ENG) and the value is {id, name}. "
            "Find the team key from the response - it's typically uppercase letters. "
            'Example format: {"LEG": {"id": "uuid-here", "name": "Legion"}}. '
            "No markdown, just raw JSON.",
        ],
        capture_output=True,
        text=True,
    )

    if result.returncode != 0:
        raise click.ClickException(f"Failed to fetch teams: {result.stderr}")

    # Parse Claude's response - it outputs JSON with result field
    try:
        response = json.loads(result.stdout)
        content = response.get("result", "")

        # Find JSON object - might be nested or contain multiple objects
        # Find the outermost { } pair
        start = content.find("{")
        if start == -1:
            raise click.ClickException(f"No JSON found in: {content[:200]}")

        # Find matching closing brace
        depth = 0
        end = start
        for i, char in enumerate(content[start:], start):
            if char == "{":
                depth += 1
            elif char == "}":
                depth -= 1
                if depth == 0:
                    end = i + 1
                    break

        teams_json = content[start:end]
        teams = json.loads(teams_json)

        # Validate structure - should be {KEY: {id, name}, ...}
        if not teams or not isinstance(teams, dict):
            raise click.ClickException(f"Invalid teams format: {teams_json[:200]}")

        # Check if it's a single team (missing key wrapper)
        if "id" in teams and "name" in teams and len(teams) <= 3:
            # It's a single team object, not keyed by team key
            # Try to extract the key from the response or ask user
            raise click.ClickException(
                f"Claude returned a single team without the key. "
                f"Please manually create ~/.legion/teams.json with format:\n"
                f'{{"TEAMKEY": {teams_json}}}'
            )

    except json.JSONDecodeError as e:
        raise click.ClickException(f"Invalid JSON response: {e}") from e

    # Save to cache
    cache_dir = Path.home() / ".legion"
    cache_dir.mkdir(parents=True, exist_ok=True)
    cache_file = cache_dir / "teams.json"

    with open(cache_file, "w") as f:
        json.dump(teams, f, indent=2)

    click.echo(f"\nCached {len(teams)} teams to {cache_file}:\n")
    for key, team in sorted(teams.items()):
        if isinstance(team, dict) and "name" in team and "id" in team:
            click.echo(f"  {key}: {team['name']} ({team['id']})")
        else:
            click.echo(f"  {key}: {team}")


@cli.command()
def install() -> None:
    """Install required Claude Code plugins."""
    click.echo("Installing required Claude Code plugins...\n")

    for plugin, description in REQUIRED_PLUGINS:
        click.echo(f"  {plugin}")
        click.echo(f"    {description}")
        result = subprocess.run(
            ["claude", "plugin", "install", plugin, "--scope", "user"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            click.echo(click.style("    ✓ installed", fg="green"))
        elif "already installed" in result.stderr.lower():
            click.echo(click.style("    ✓ already installed", fg="yellow"))
        else:
            click.echo(click.style(f"    ✗ failed: {result.stderr.strip()}", fg="red"))
        click.echo()

    click.echo("Done! Verify with: claude plugin list")


def main() -> None:
    cli()


if __name__ == "__main__":
    main()
