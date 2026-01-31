"""Legion CLI entry point."""

from pathlib import Path

import anyio
import click

from legion import daemon


@click.group()
def cli() -> None:
    """Legion: Autonomous development swarm using Claude Code agents."""


@cli.command()
@click.argument("project_id")
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
    help="State directory (default: ~/.legion/PROJECT_ID)",
)
def start(project_id: str, workspace: Path, state_dir: Path | None) -> None:
    """Start the swarm for a Linear project."""
    if state_dir is None:
        state_dir = Path.home() / ".legion" / project_id

    try:
        anyio.run(daemon.start, project_id, workspace.resolve(), state_dir)
    except ValueError as e:
        raise click.ClickException(str(e)) from None
    except RuntimeError as e:
        raise click.ClickException(str(e)) from None


@cli.command()
@click.argument("project_id")
@click.option(
    "--state-dir",
    type=click.Path(file_okay=False, path_type=Path),
    default=None,
    help="State directory (default: ~/.legion/PROJECT_ID)",
)
def stop(project_id: str, state_dir: Path | None) -> None:
    """Stop the swarm."""
    if state_dir is None:
        state_dir = Path.home() / ".legion" / project_id

    anyio.run(daemon.stop, project_id, state_dir)


@cli.command()
@click.argument("project_id")
@click.option(
    "--state-dir",
    type=click.Path(file_okay=False, path_type=Path),
    default=None,
    help="State directory (default: ~/.legion/PROJECT_ID)",
)
def status(project_id: str, state_dir: Path | None) -> None:
    """Check swarm status."""
    if state_dir is None:
        state_dir = Path.home() / ".legion" / project_id

    anyio.run(daemon.status, project_id, state_dir)


def main() -> None:
    cli()


if __name__ == "__main__":
    main()
