"""Tests for daemon module."""

import pytest

from legion.daemon import (
    short_id,
    controller_session_name,
    validate_project_id,
)


class TestShortId:
    def test_uuid_returns_short(self) -> None:
        project_id = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        result = short_id(project_id)
        assert len(result) == 8
        assert result != project_id

    def test_uuid_without_hyphens(self) -> None:
        project_id = "7b4f0862b7754cb09a6785400c6f44a8"
        result = short_id(project_id)
        assert len(result) == 8

    def test_non_uuid_returns_as_is(self) -> None:
        project_id = "my-project"
        assert short_id(project_id) == "my-project"

    def test_short_string_returns_as_is(self) -> None:
        project_id = "abc123"
        assert short_id(project_id) == "abc123"


class TestControllerSessionName:
    def test_format(self) -> None:
        assert controller_session_name("abc123") == "legion-abc123-controller"

    def test_with_short_uuid(self) -> None:
        short = short_id("7b4f0862-b775-4cb0-9a67-85400c6f44a8")
        session = controller_session_name(short)
        assert session.startswith("legion-")
        assert session.endswith("-controller")


class TestValidateProjectId:
    def test_valid_alphanumeric(self) -> None:
        validate_project_id("myproject123")  # Should not raise

    def test_valid_with_hyphens(self) -> None:
        validate_project_id("my-project-123")  # Should not raise

    def test_valid_with_underscores(self) -> None:
        validate_project_id("my_project_123")  # Should not raise

    def test_valid_uuid(self) -> None:
        validate_project_id("7b4f0862-b775-4cb0-9a67-85400c6f44a8")  # Should not raise

    def test_invalid_spaces(self) -> None:
        with pytest.raises(ValueError, match="must contain only"):
            validate_project_id("my project")

    def test_invalid_special_chars(self) -> None:
        with pytest.raises(ValueError, match="must contain only"):
            validate_project_id("my;project")

    def test_invalid_shell_injection(self) -> None:
        with pytest.raises(ValueError, match="must contain only"):
            validate_project_id("$(whoami)")

    def test_invalid_path_traversal(self) -> None:
        with pytest.raises(ValueError, match="must contain only"):
            validate_project_id("../etc/passwd")
