"""Tests for calculator module."""

from legion import calculator


class TestAdd:
    def test_add_positive_numbers(self) -> None:
        assert calculator.add(2, 3) == 5

    def test_add_negative_and_positive(self) -> None:
        assert calculator.add(-1, 1) == 0

    def test_add_zeros(self) -> None:
        assert calculator.add(0, 0) == 0

    def test_add_zero_identity(self) -> None:
        """Test that adding zero preserves the value (identity property)."""
        assert calculator.add(5, 0) == 5
        assert calculator.add(0, 5) == 5

    def test_add_both_negative(self) -> None:
        assert calculator.add(-5, -3) == -8

    def test_add_large_integers(self) -> None:
        # Python handles arbitrary precision integers
        large = 10**100
        assert calculator.add(large, large) == 2 * large


class TestSubtract:
    def test_subtract_positive_numbers(self) -> None:
        assert calculator.subtract(5, 3) == 2

    def test_subtract_to_zero(self) -> None:
        assert calculator.subtract(3, 3) == 0

    def test_subtract_negative_result(self) -> None:
        assert calculator.subtract(2, 5) == -3

    def test_subtract_from_zero(self) -> None:
        assert calculator.subtract(0, 5) == -5

    def test_subtract_zero(self) -> None:
        assert calculator.subtract(5, 0) == 5

    def test_subtract_both_negative(self) -> None:
        assert calculator.subtract(-5, -3) == -2

    def test_subtract_large_integers(self) -> None:
        # Python handles arbitrary precision integers
        large = 10**100
        assert calculator.subtract(large, 1) == large - 1
