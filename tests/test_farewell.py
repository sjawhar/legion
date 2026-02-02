"""Tests for farewell module."""

from legion import farewell


class TestGoodbye:
    def test_goodbye_with_name(self) -> None:
        assert farewell.goodbye("Alice") == "Goodbye, Alice!"

    def test_goodbye_with_empty_name(self) -> None:
        assert farewell.goodbye("") == "Goodbye!"

    def test_goodbye_with_whitespace_only(self) -> None:
        assert farewell.goodbye("   ") == "Goodbye!"

    def test_goodbye_with_tabs_and_newlines(self) -> None:
        assert farewell.goodbye("\t\n") == "Goodbye!"

    def test_goodbye_with_none(self) -> None:
        assert farewell.goodbye(None) == "Goodbye!"

    def test_goodbye_with_no_args(self) -> None:
        assert farewell.goodbye() == "Goodbye!"

    def test_goodbye_preserves_leading_trailing_whitespace(self) -> None:
        """Verify that whitespace in names is preserved in output."""
        assert farewell.goodbye("  Alice  ") == "Goodbye,   Alice  !"

    def test_goodbye_with_unicode_name(self) -> None:
        """Verify that unicode characters work correctly."""
        assert farewell.goodbye("José") == "Goodbye, José!"
        assert farewell.goodbye("李明") == "Goodbye, 李明!"

    def test_goodbye_with_emoji(self) -> None:
        """Verify that emoji characters work correctly."""
        assert farewell.goodbye("Alice 👋") == "Goodbye, Alice 👋!"

    def test_goodbye_with_very_long_name(self) -> None:
        """Verify that very long names don't cause issues."""
        long_name = "A" * 1000
        result = farewell.goodbye(long_name)
        assert result == f"Goodbye, {long_name}!"
        assert len(result) == 1010  # "Goodbye, " (9) + name (1000) + "!" (1)


class TestFarewell:
    def test_farewell_with_name(self) -> None:
        assert farewell.farewell("Alice") == "Farewell, Alice!"

    def test_farewell_with_empty_name(self) -> None:
        assert farewell.farewell("") == "Farewell!"

    def test_farewell_with_whitespace_only(self) -> None:
        assert farewell.farewell("   ") == "Farewell!"

    def test_farewell_with_tabs_and_newlines(self) -> None:
        assert farewell.farewell("\t\n") == "Farewell!"

    def test_farewell_with_none(self) -> None:
        assert farewell.farewell(None) == "Farewell!"

    def test_farewell_with_no_args(self) -> None:
        assert farewell.farewell() == "Farewell!"

    def test_farewell_preserves_leading_trailing_whitespace(self) -> None:
        """Verify that whitespace in names is preserved in output."""
        assert farewell.farewell("  Alice  ") == "Farewell,   Alice  !"

    def test_farewell_with_unicode_name(self) -> None:
        """Verify that unicode characters work correctly."""
        assert farewell.farewell("José") == "Farewell, José!"
        assert farewell.farewell("李明") == "Farewell, 李明!"

    def test_farewell_with_emoji(self) -> None:
        """Verify that emoji characters work correctly."""
        assert farewell.farewell("Alice 👋") == "Farewell, Alice 👋!"

    def test_farewell_with_very_long_name(self) -> None:
        """Verify that very long names don't cause issues."""
        long_name = "A" * 1000
        result = farewell.farewell(long_name)
        assert result == f"Farewell, {long_name}!"
        assert len(result) == 1011  # "Farewell, " (10) + name (1000) + "!" (1)
