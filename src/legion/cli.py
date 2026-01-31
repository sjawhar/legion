"""Legion CLI entry point."""

from pathlib import Path

import anyio
import click

from . import daemon


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


@cli.command()
@click.option(
    "--workspace",
    "-w",
    type=click.Path(exists=True, file_okay=False, path_type=Path),
    default=None,
    help="Workspace to install hooks into (optional)",
)
@click.option(
    "--skills-only",
    is_flag=True,
    help="Only install skills, skip hooks",
)
def setup(workspace: Path | None, skills_only: bool) -> None:
    """Install Legion skills and hooks.

    Skills are installed to ~/.claude/skills/.
    Hooks are installed to WORKSPACE/.claude/hooks/ if --workspace is provided.
    """
    from . import setup as setup_module

    # Install skills
    installed_skills = setup_module.install_skills()
    for skill in installed_skills:
        click.echo(f"Installed skill: {skill}")

    # Install hooks if workspace provided
    if workspace and not skills_only:
        installed_hooks = setup_module.install_hooks(workspace)
        for hook in installed_hooks:
            click.echo(f"Installed hook: {hook}")

        setup_module.install_settings(workspace)
        click.echo(f"Updated settings: {workspace}/.claude/settings.json")

    click.echo("\nSetup complete!")


def main() -> None:
    cli()


if __name__ == "__main__":
    main()
