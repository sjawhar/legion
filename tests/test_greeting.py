"""Tests for the greeting module."""

from legion.greeting import greet


class TestGreet:
    """Tests for the greet function."""

    def test_greet_with_name(self) -> None:
        """greet returns a greeting with the provided name."""
        assert greet("World") == "Hello, World!"

    def test_greet_with_empty_string(self) -> None:
        """greet handles empty string input."""
        assert greet("") == "Hello, !"
