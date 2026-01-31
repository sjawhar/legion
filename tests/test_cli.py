"""Tests for CLI module."""

from click.testing import CliRunner

from legion import cli


class TestStartCommand:
    def test_start_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli.cli, ["start", "--help"])
        assert result.exit_code == 0
        assert "Start the swarm" in result.output

    def test_start_requires_project_id(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli.cli, ["start"])
        assert result.exit_code != 0
        assert "Missing argument" in result.output


class TestStopCommand:
    def test_stop_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli.cli, ["stop", "--help"])
        assert result.exit_code == 0
        assert "Stop the swarm" in result.output


class TestStatusCommand:
    def test_status_help(self) -> None:
        runner = CliRunner()
        result = runner.invoke(cli.cli, ["status", "--help"])
        assert result.exit_code == 0
        assert "Check swarm status" in result.output
