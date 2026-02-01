# Simple Calculator Module Pattern

## Problem
Create a clean, minimal calculator module that demonstrates best practices for module structure, testing, and documentation within the Legion project.

## Solution

### 1. Module Structure

**File:** `src/legion/calculator.py`

The calculator module uses a straightforward, function-based approach with clear responsibilities:

```python
"""Simple calculator operations."""


def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b


def subtract(a: int, b: int) -> int:
    """Subtract b from a."""
    return a - b
```

**Key patterns:**
- **Module docstring first** - Immediately describes the module's purpose
- **Type hints on public functions** - Both parameters and return types are annotated (`int` → `int`)
- **Clear parameter naming** - Operations use conventional `a` and `b` rather than ambiguous names
- **Explicit docstrings** - Each function documents its behavior, including operation order (subtract "b from a")
- **No classes for simple operations** - Functions are preferred over stateless classes, keeping the API simple and direct

### 2. Absolute Import Convention

Tests demonstrate the import pattern used throughout Legion:

```python
from legion import calculator

# Usage:
calculator.add(2, 3)
calculator.subtract(5, 3)
```

**Key patterns:**
- **Module-level imports** - Import the module itself, not functions
- **Qualified calls** - Use `calculator.add()` rather than `from legion.calculator import add`
- **Follows Google Python Style Guide** - Consistent with Legion's conventions (no relative imports)

This approach keeps namespaces clear and makes it easy to understand which module a function comes from.

### 3. Comprehensive Test Coverage

**File:** `tests/test_calculator.py`

Tests are organized in `TestAdd` and `TestSubtract` classes with 13 focused test cases:

```python
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
```

**Key patterns:**
- **Class-based organization** - Tests grouped by function being tested (TestAdd, TestSubtract)
- **Type hints on test methods** - `-> None` indicates these are pure tests with no return value
- **Simple assertions** - Direct `assert` statements, no complex testing frameworks
- **Boundary testing** - Covers edge cases:
  - Identity properties (adding zero)
  - Zero handling (both arguments, one argument, result)
  - Sign combinations (positive, negative, mixed)
  - Large values (leverages Python's arbitrary precision)
- **Descriptive names** - Test names clearly state what's being tested
- **Docstrings when helpful** - Added for mathematical properties like "identity property"
- **Comments for rationale** - Explains why large integers matter (arbitrary precision)

### 4. What Makes This Clean

1. **Minimal but complete** - Only essential functionality, no over-engineering
2. **Self-documenting** - Type hints and docstrings eliminate ambiguity
3. **Test-driven coverage** - 13 tests ensure reliability with edge cases
4. **Follows project conventions** - Consistent with Legion's style guide and patterns
5. **Clear responsibilities** - Each function does one thing well
6. **Easy to import and use** - Module-level imports keep code readable

## When to Use This Pattern

- **Simple utility functions** - When business logic doesn't require state
- **Pure functions** - Operations with no side effects
- **Foundational modules** - Base utilities other modules depend on

## Related Patterns

- **Module layout** - Consistent with other Legion modules like `short_id.py`
- **Type hints** - Follows Legion's approach to Python 3.13+ features
- **Testing** - Mirrors test structure used elsewhere in the project
