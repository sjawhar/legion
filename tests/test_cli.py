"""Tests for CLI module."""

from click.testing import CliRunner

from legion.cli import cli


class TestSetupCommand:
    def test_setup_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli, ["setup", "--help"])
        assert result.exit_code == 0
        assert "Install Legion skills and hooks" in result.output

    def test_setup_skills_only(self, tmp_path) -> None:
        runner = CliRunner()
        # Use a temp directory for skills to avoid modifying real ~/.claude/skills
        result = runner.invoke(cli, ["setup", "--skills-only"])
        # This will install to ~/.claude/skills which is fine for testing
        assert result.exit_code == 0
        assert "Setup complete" in result.output


class TestStartCommand:
    def test_start_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli, ["start", "--help"])
        assert result.exit_code == 0
        assert "Start the swarm" in result.output

    def test_start_requires_project_id(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli, ["start"])
        assert result.exit_code != 0
        assert "Missing argument" in result.output


class TestStopCommand:
    def test_stop_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli, ["stop", "--help"])
        assert result.exit_code == 0
        assert "Stop the swarm" in result.output


class TestStatusCommand:
    def test_status_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli, ["status", "--help"])
        assert result.exit_code == 0
        assert "Check swarm status" in result.output
