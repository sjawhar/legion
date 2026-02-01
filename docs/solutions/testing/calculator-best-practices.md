# Best Practices: Module Implementation & Testing

**Source Issue:** LEG-10 - Add calculator module
**Date:** 2026-02-01

## Overview

The calculator module demonstrates a reusable template for implementing pure utility modules with comprehensive test coverage. This document captures patterns and principles that should be applied to future similar implementations.

---

## Code Organization Principles

### 1. Module Structure

**Pattern Applied:**
- Single-responsibility functions with clear names
- Module-level docstring explaining purpose
- Type hints on all function signatures and returns

**Why This Works:**
- Makes functions discoverable and self-documenting
- Clear contracts prevent integration issues
- Enables IDE autocompletion and static analysis

**Template for Future Modules:**
```python
"""Module purpose (one line)."""

from typing import TypeVar, Callable

T = TypeVar("T")

def operation(arg1: Type, arg2: Type) -> ReturnType:
    """Brief description of what the function does."""
    # Implementation
    return result
```

**Reuse Guidance:**
- Always include module docstring
- Add type hints even for simple functions
- Use descriptive names that clearly indicate behavior

---

### 2. Function Documentation

**Pattern Applied:**
```python
def subtract(a: int, b: int) -> int:
    """Subtract b from a."""
    return a - b
```

**Why This Works:**
- One-line docstring sufficient for simple functions
- Parameter order is unambiguous in the description
- Avoids over-documentation for straightforward operations

**When to Extend Docstrings:**
- If there are non-obvious edge cases
- If the function performs side effects
- If parameters require explanation beyond their names

---

## Testing Patterns

### 1. Class-Based Test Organization

**Pattern Applied:**
```python
class TestAdd:
    def test_add_positive_numbers(self) -> None:
        assert calculator.add(2, 3) == 5
```

**Benefits:**
- Groups related tests logically (one class per function)
- Allows shared setup/teardown via fixtures if needed
- Improves test readability and navigation
- Clear naming convention: `Test<FunctionName>`

**Reuse Guidance:**
- Always use class-based organization for module test suites
- Name each test class after the function it tests: `Test<function_name>`
- Name test methods: `test_<scenario>` (verb_noun pattern)

### 2. Comprehensive Edge Case Coverage

**Categories Tested:**

| Category | Examples | Why Important |
|----------|----------|---------------|
| **Identity Cases** | Adding/subtracting zero | Validates fundamental mathematical properties |
| **Boundary Values** | Zero, negative numbers | Often reveal off-by-one or sign errors |
| **Combined Cases** | Negative + negative, large integers | Tests behavior across value ranges |
| **Extreme Values** | `10**100` (arbitrary precision) | Verifies no silent overflow or precision loss |

**Pattern Applied:**
```python
class TestAdd:
    def test_add_zero_identity(self) -> None:
        """Test that adding zero preserves the value (identity property)."""
        assert calculator.add(5, 0) == 5
        assert calculator.add(0, 5) == 5

    def test_add_large_integers(self) -> None:
        large = 10**100
        assert calculator.add(large, large) == 2 * large
```

**Reuse Guidance:**
- Include at least one "normal" case per function
- Add one identity/neutral operation test
- Test boundary values (zero, one, negatives)
- If working with numbers, include large value tests
- Document non-obvious test intent with docstring

### 3. Type Hints in Tests

**Pattern Applied:**
```python
def test_add_positive_numbers(self) -> None:
    assert calculator.add(2, 3) == 5
```

**Benefits:**
- Enables static analysis to catch test code bugs
- Signals intent: test returns nothing
- Aligns with module-level type discipline

**Reuse Guidance:**
- Add `-> None` return type to all test methods
- Test method parameters should not use type hints (pytest convention)

### 4. Test Independence

**Pattern Applied:**
- Each test is self-contained and can run independently
- No shared state between tests
- Each test validates one assertion or logical grouping

**Why This Matters:**
- Tests can run in any order
- Failure in one test doesn't mask others
- Easier to debug when tests are isolated

**Reuse Guidance:**
- Avoid test interdependencies
- Don't share state via class variables
- Each test should be runnable alone

---

## Code Quality Attributes

### 1. Simplicity

**Observed:**
- Functions are 1-3 lines of implementation
- No defensive programming overhead
- Direct delegation to language primitives

**When to Apply:**
- For utility modules with single responsibility
- When using type hints ensures correctness
- Pure functions with no side effects

**When NOT to Apply:**
- Functions with significant business logic
- Operations requiring error handling
- Interactions with external systems

### 2. Type Safety Without Verbosity

**Observed:**
```python
def add(a: int, b: int) -> int:
    """Add two integers."""
    return a + b
```

**Benefits:**
- Type checker catches misuse immediately
- No `isinstance()` checks needed
- Self-documenting contract

**Reuse Guidance:**
- Use language type hints for validation
- Avoid defensive checks when types are enforced
- Rely on type checker (mypy, pyright) in CI

---

## Testing Strategy Summary

### Minimum Test Coverage

For a new module function, include:

1. **Normal Case** (2-3 tests)
   - Basic positive path
   - Confirm expected behavior

2. **Boundary Cases** (1-2 tests per boundary)
   - Zero, negative, empty values
   - Operations involving identity elements

3. **Edge Cases** (1-2 tests)
   - Extreme values (large numbers, min/max)
   - Unusual but valid combinations
   - Language-specific quirks (e.g., Python's arbitrary precision)

4. **Logical Groups** (1 test)
   - Combined cases (e.g., both negative)
   - Verify commutativity/associativity if applicable

### Test Method Naming

```
test_<operation>_<scenario>

Examples:
- test_add_positive_numbers
- test_add_zero_identity
- test_subtract_negative_result
```

### Documentation in Tests

- Add docstrings only for non-obvious tests
- Include comments explaining "why" for edge cases
- Let test name explain "what"

---

## Module Template for Future Use

```python
"""Module description."""


def function_name(arg1: Type, arg2: Type) -> ReturnType:
    """One-line description of function behavior."""
    return computed_result
```

```python
"""Tests for module."""

from legion import module_name


class TestFunctionName:
    def test_function_name_normal_case(self) -> None:
        """Test basic happy path."""
        assert module_name.function_name(input1, input2) == expected

    def test_function_name_boundary_case(self) -> None:
        """Test boundary/identity case."""
        assert module_name.function_name(0, value) == value

    def test_function_name_edge_case(self) -> None:
        """Test extreme or unusual values."""
        assert module_name.function_name(large_value, large_value) == result
```

---

## Checklist for Future Implementations

- [ ] Module has docstring explaining purpose
- [ ] All functions have type hints (parameters and return)
- [ ] All functions have docstrings (at least one line)
- [ ] Tests use class-based organization
- [ ] Test class names follow `Test<FunctionName>` pattern
- [ ] Test method names follow `test_<scenario>` pattern
- [ ] Coverage includes: normal, boundary, and edge cases
- [ ] Edge cases have comments explaining the test
- [ ] All test methods have `-> None` type hint
- [ ] No test interdependencies
- [ ] Tests can run independently and in any order

---

## Why This Approach Works

1. **Discoverability:** Type hints + docstrings make code self-documenting
2. **Maintainability:** Class-based tests group logic clearly
3. **Reliability:** Comprehensive edge case testing catches regressions
4. **Consistency:** Reusable patterns across the codebase
5. **Velocity:** Clear template reduces decision-making for new modules

This pattern is suitable for implementing utility modules, domain logic with pure functions, and any module where behavior can be fully described by input/output contracts.

