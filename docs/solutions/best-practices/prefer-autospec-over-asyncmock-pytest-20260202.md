---
module: Legion Testing
date: 2026-02-02
problem_type: best_practice
component: testing_framework
symptoms:
  - "Using new_callable=mocker.AsyncMock when autospec=True alone works"
  - "Verbose mocking patterns for async functions"
root_cause: missing_validation
resolution_type: code_fix
severity: medium
tags: [pytest-mock, autospec, asyncmock, testing, mocking, python]
---

# Best Practice: Prefer autospec=True Over AsyncMock in pytest-mock

## Problem
When mocking async functions with pytest-mock, many codebases use `new_callable=mocker.AsyncMock`. However, in modern Python (3.8+), `autospec=True` alone correctly handles async functions AND provides signature validation.

## Environment
- Module: Legion Testing Infrastructure
- Python Version: 3.13
- pytest-mock Version: 3.15.1
- Affected Component: Test mocking patterns in `tests/test_daemon.py`
- Date: 2026-02-02

## Symptoms
- Verbose mocking code using `new_callable=mocker.AsyncMock`
- Missing signature validation that `autospec=True` provides
- Inability to combine `autospec=True` with `new_callable` (ValueError if attempted)

## What Didn't Work

**Attempted Solution 1:** Use both `autospec=True` and `new_callable=mocker.AsyncMock`
- **Why it failed:** Python's `unittest.mock.patch()` raises `ValueError: Cannot use 'autospec' and 'new_callable' together`

## Solution

Use `autospec=True` alone for all mocked functions, including async functions. Modern Python's mock library automatically detects async functions and creates appropriate mocks.

**Code changes:**

```python
# BEFORE - verbose and no signature validation:
mock_func = mocker.patch(
    "module.async_function",
    new_callable=mocker.AsyncMock,
)

# AFTER - cleaner and with signature validation:
mock_func = mocker.patch(
    "module.async_function",
    autospec=True,
)
mock_func.return_value = "mocked_value"

# For sync functions with side_effect:
mocker.patch(
    "module.function",
    side_effect=my_callback,
    autospec=True,
)
```

**Benefits of autospec=True:**
1. Automatically works with both sync and async functions
2. Validates function signatures - catches wrong arguments
3. Cleaner, more consistent code
4. No need to remember `new_callable=mocker.AsyncMock`

## Why This Works

1. **ROOT CAUSE:** In Python 3.8+, `unittest.mock` was updated to properly handle async functions with `autospec=True`. When autospec inspects a coroutine function, it automatically creates a mock that returns a coroutine.

2. **Why autospec is better:**
   - Signature validation catches bugs (wrong argument names/counts)
   - Single consistent pattern for all functions
   - Less verbose code

3. **The misconception:** Many tutorials still recommend `new_callable=mocker.AsyncMock`, but this is unnecessary in modern Python and prevents using `autospec`.

## Prevention

- Always use `autospec=True` for mocked functions
- Avoid `new_callable=mocker.AsyncMock` - it's not needed
- The mock library will automatically handle async functions correctly
- If you see `new_callable=mocker.AsyncMock` in code, replace with `autospec=True`

## Related Issues

No related issues documented yet.
