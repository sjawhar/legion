# Password Reset Token Model Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Create a password reset token module with secure generation, hashing, expiry, and single-use validation.

**Architecture:** Pure Python module using `secrets` for cryptographically secure token generation and `hashlib` for SHA-256 hashing. Tokens are stored hashed (like passwords). The module provides functions for token generation, hashing, and validation. Storage is abstracted via a simple protocol - actual persistence is out of scope.

**Tech Stack:** Python 3.13+, pytest, secrets, hashlib, dataclasses

---

## Task 1: Token Generation

Create secure token generation using Python's `secrets` module.

**Files:**
- Create: `src/legion/password_reset.py`
- Create: `tests/test_password_reset.py`

**Step 1: Write the failing test for token generation**

```python
"""Tests for password reset token module."""

from legion import password_reset


class TestGenerateToken:
    def test_generates_url_safe_token(self) -> None:
        token = password_reset.generate_token()
        # URL-safe base64 characters only
        assert all(c.isalnum() or c in "-_" for c in token)

    def test_generates_sufficient_entropy(self) -> None:
        token = password_reset.generate_token()
        # 32 bytes = 256 bits of entropy, base64 encoded ~ 43 chars
        assert len(token) >= 32

    def test_generates_unique_tokens(self) -> None:
        tokens = [password_reset.generate_token() for _ in range(100)]
        assert len(set(tokens)) == 100
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestGenerateToken -v`
Expected: FAIL with "ModuleNotFoundError: No module named 'legion.password_reset'"

**Step 3: Write minimal implementation**

```python
"""Password reset token operations."""

import secrets


def generate_token() -> str:
    """Generate a cryptographically secure URL-safe token.

    Returns 32 bytes (256 bits) of entropy, URL-safe base64 encoded.
    """
    return secrets.token_urlsafe(32)
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestGenerateToken -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add secure token generation"
```

---

## Task 2: Token Hashing

Hash tokens for secure storage (never store raw tokens).

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for token hashing**

Add to `tests/test_password_reset.py`:

```python
class TestHashToken:
    def test_returns_hex_string(self) -> None:
        token = "test-token-abc123"
        hashed = password_reset.hash_token(token)
        # SHA-256 produces 64 hex characters
        assert len(hashed) == 64
        assert all(c in "0123456789abcdef" for c in hashed)

    def test_same_input_same_hash(self) -> None:
        token = "consistent-token"
        hash1 = password_reset.hash_token(token)
        hash2 = password_reset.hash_token(token)
        assert hash1 == hash2

    def test_different_input_different_hash(self) -> None:
        hash1 = password_reset.hash_token("token-a")
        hash2 = password_reset.hash_token("token-b")
        assert hash1 != hash2
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestHashToken -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'hash_token'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
import hashlib


def hash_token(token: str) -> str:
    """Hash a token using SHA-256 for secure storage.

    Tokens should be stored hashed, like passwords.
    """
    return hashlib.sha256(token.encode()).hexdigest()
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestHashToken -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add token hashing with SHA-256"
```

---

## Task 3: Token Data Model

Create a dataclass to hold token metadata (email, expiry, used status).

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for token data model**

Add to `tests/test_password_reset.py`:

```python
from datetime import datetime, timedelta, timezone


class TestResetToken:
    def test_create_token_with_defaults(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc123",
            email="user@example.com",
        )
        assert token.token_hash == "abc123"
        assert token.email == "user@example.com"
        assert token.used is False
        assert token.expires_at is not None

    def test_default_expiry_is_one_hour(self) -> None:
        before = datetime.now(timezone.utc)
        token = password_reset.ResetToken(
            token_hash="abc123",
            email="user@example.com",
        )
        after = datetime.now(timezone.utc)

        # Expiry should be ~1 hour from now
        expected_min = before + timedelta(hours=1) - timedelta(seconds=1)
        expected_max = after + timedelta(hours=1) + timedelta(seconds=1)
        assert expected_min <= token.expires_at <= expected_max

    def test_custom_expiry(self) -> None:
        custom_expiry = datetime(2026, 12, 31, 23, 59, 59, tzinfo=timezone.utc)
        token = password_reset.ResetToken(
            token_hash="abc123",
            email="user@example.com",
            expires_at=custom_expiry,
        )
        assert token.expires_at == custom_expiry

    def test_token_is_immutable(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc123",
            email="user@example.com",
        )
        # Frozen dataclass should raise on mutation
        try:
            token.used = True  # type: ignore[misc]
            assert False, "Should have raised"
        except AttributeError:
            pass
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestResetToken -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'ResetToken'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone


def _default_expiry() -> datetime:
    """Default expiry is 1 hour from now."""
    return datetime.now(timezone.utc) + timedelta(hours=1)


@dataclass(frozen=True)
class ResetToken:
    """Immutable password reset token data.

    Attributes:
        token_hash: SHA-256 hash of the raw token (for storage lookup).
        email: Email address this token was issued for.
        expires_at: When this token expires (default: 1 hour from creation).
        used: Whether this token has been used (single-use enforcement).
    """
    token_hash: str
    email: str
    expires_at: datetime = field(default_factory=_default_expiry)
    used: bool = False
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestResetToken -v`
Expected: PASS (4 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add ResetToken dataclass with expiry"
```

---

## Task 4: Token Validation - Expiry Check

Add validation for expired tokens.

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for expiry validation**

Add to `tests/test_password_reset.py`:

```python
class TestIsExpired:
    def test_not_expired_when_before_expiry(self) -> None:
        future = datetime.now(timezone.utc) + timedelta(hours=1)
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=future,
        )
        assert password_reset.is_expired(token) is False

    def test_expired_when_past_expiry(self) -> None:
        past = datetime.now(timezone.utc) - timedelta(seconds=1)
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=past,
        )
        assert password_reset.is_expired(token) is True

    def test_expired_at_exact_expiry_time(self) -> None:
        # Edge case: exactly at expiry moment
        now = datetime.now(timezone.utc)
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=now,
        )
        # At exact expiry, consider it expired
        assert password_reset.is_expired(token) is True
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestIsExpired -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'is_expired'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
def is_expired(token: ResetToken) -> bool:
    """Check if a token has expired.

    Returns True if current time is at or past the expiry time.
    """
    return datetime.now(timezone.utc) >= token.expires_at
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestIsExpired -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add token expiry validation"
```

---

## Task 5: Token Validation - Single-Use Check

Add validation for already-used tokens.

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for single-use validation**

Add to `tests/test_password_reset.py`:

```python
class TestIsUsed:
    def test_not_used_by_default(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
        )
        assert password_reset.is_used(token) is False

    def test_used_when_marked(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            used=True,
        )
        assert password_reset.is_used(token) is True
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestIsUsed -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'is_used'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
def is_used(token: ResetToken) -> bool:
    """Check if a token has already been used.

    Tokens are single-use and cannot be reused.
    """
    return token.used
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestIsUsed -v`
Expected: PASS (2 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add single-use token validation"
```

---

## Task 6: Combined Token Validation

Create a function that validates a token is both valid (not expired, not used).

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for combined validation**

Add to `tests/test_password_reset.py`:

```python
class TestValidateToken:
    def test_valid_token_returns_none(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=datetime.now(timezone.utc) + timedelta(hours=1),
            used=False,
        )
        result = password_reset.validate_token(token)
        assert result is None

    def test_expired_token_returns_error(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=datetime.now(timezone.utc) - timedelta(seconds=1),
            used=False,
        )
        result = password_reset.validate_token(token)
        assert result == "token_expired"

    def test_used_token_returns_error(self) -> None:
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=datetime.now(timezone.utc) + timedelta(hours=1),
            used=True,
        )
        result = password_reset.validate_token(token)
        assert result == "token_used"

    def test_expired_takes_precedence_over_used(self) -> None:
        # If both expired and used, report expired first
        token = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            expires_at=datetime.now(timezone.utc) - timedelta(seconds=1),
            used=True,
        )
        result = password_reset.validate_token(token)
        assert result == "token_expired"
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestValidateToken -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'validate_token'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
def validate_token(token: ResetToken) -> str | None:
    """Validate a token for use.

    Returns:
        None if token is valid.
        "token_expired" if token has expired.
        "token_used" if token has already been used.
    """
    if is_expired(token):
        return "token_expired"
    if is_used(token):
        return "token_used"
    return None
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestValidateToken -v`
Expected: PASS (4 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add combined token validation"
```

---

## Task 7: Mark Token as Used

Create a function to produce a new token marked as used (immutable pattern).

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for marking token used**

Add to `tests/test_password_reset.py`:

```python
class TestMarkUsed:
    def test_returns_new_token_with_used_true(self) -> None:
        original = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            used=False,
        )
        marked = password_reset.mark_used(original)
        assert marked.used is True

    def test_preserves_other_fields(self) -> None:
        expiry = datetime(2026, 12, 31, tzinfo=timezone.utc)
        original = password_reset.ResetToken(
            token_hash="abc123",
            email="test@example.com",
            expires_at=expiry,
            used=False,
        )
        marked = password_reset.mark_used(original)
        assert marked.token_hash == "abc123"
        assert marked.email == "test@example.com"
        assert marked.expires_at == expiry

    def test_original_unchanged(self) -> None:
        original = password_reset.ResetToken(
            token_hash="abc",
            email="user@example.com",
            used=False,
        )
        password_reset.mark_used(original)
        assert original.used is False
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestMarkUsed -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'mark_used'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
from dataclasses import replace


def mark_used(token: ResetToken) -> ResetToken:
    """Return a new token marked as used.

    Since ResetToken is immutable, this returns a copy with used=True.
    """
    return replace(token, used=True)
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestMarkUsed -v`
Expected: PASS (3 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add mark_used for immutable token update"
```

---

## Task 8: Create Token Helper

Create a convenience function that generates a token, hashes it, and returns both.

**Files:**
- Modify: `src/legion/password_reset.py`
- Modify: `tests/test_password_reset.py`

**Step 1: Write the failing test for create_token helper**

Add to `tests/test_password_reset.py`:

```python
class TestCreateToken:
    def test_returns_raw_token_and_reset_token(self) -> None:
        raw, reset_token = password_reset.create_token("user@example.com")
        assert isinstance(raw, str)
        assert isinstance(reset_token, password_reset.ResetToken)

    def test_raw_token_hashes_to_token_hash(self) -> None:
        raw, reset_token = password_reset.create_token("user@example.com")
        assert password_reset.hash_token(raw) == reset_token.token_hash

    def test_email_is_set(self) -> None:
        raw, reset_token = password_reset.create_token("test@example.com")
        assert reset_token.email == "test@example.com"

    def test_token_is_not_used(self) -> None:
        raw, reset_token = password_reset.create_token("user@example.com")
        assert reset_token.used is False

    def test_token_expires_in_one_hour(self) -> None:
        before = datetime.now(timezone.utc)
        raw, reset_token = password_reset.create_token("user@example.com")
        after = datetime.now(timezone.utc)

        expected_min = before + timedelta(hours=1) - timedelta(seconds=1)
        expected_max = after + timedelta(hours=1) + timedelta(seconds=1)
        assert expected_min <= reset_token.expires_at <= expected_max
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_password_reset.py::TestCreateToken -v`
Expected: FAIL with "AttributeError: module 'legion.password_reset' has no attribute 'create_token'"

**Step 3: Write minimal implementation**

Add to `src/legion/password_reset.py`:

```python
def create_token(email: str) -> tuple[str, ResetToken]:
    """Create a new password reset token for an email.

    Returns:
        A tuple of (raw_token, reset_token).
        - raw_token: Send this to the user (in the reset link).
        - reset_token: Store this in the database (hashed).
    """
    raw = generate_token()
    token_hash = hash_token(raw)
    reset_token = ResetToken(token_hash=token_hash, email=email)
    return raw, reset_token
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_password_reset.py::TestCreateToken -v`
Expected: PASS (5 tests)

**Step 5: Commit**

```bash
jj describe -m "feat(password-reset): add create_token convenience function"
```

---

## Task 9: Final Test Run and Module Export

Ensure all tests pass and module exports are correct.

**Files:**
- Verify: `src/legion/password_reset.py`
- Verify: `tests/test_password_reset.py`

**Step 1: Run full test suite**

Run: `uv run pytest tests/test_password_reset.py -v`
Expected: PASS (27 tests total)

**Step 2: Run full project tests to ensure no regressions**

Run: `uv run pytest -v`
Expected: All existing tests still pass

**Step 3: Verify module can be imported**

Run: `uv run python -c "from legion import password_reset; print(dir(password_reset))"`
Expected: Lists `ResetToken`, `create_token`, `generate_token`, `hash_token`, `is_expired`, `is_used`, `mark_used`, `validate_token`

**Step 4: Final commit**

```bash
jj describe -m "feat(password-reset): complete token model with generation, hashing, and validation

Implements:
- Secure token generation (256-bit entropy, URL-safe)
- SHA-256 token hashing for storage
- Immutable ResetToken dataclass with 1-hour default expiry
- Validation: expiry check, single-use enforcement
- Helper functions: create_token, mark_used

Part of LEG-14."
```

---

## Summary

| Task | Tests | Functions/Classes Added |
|------|-------|------------------------|
| 1. Token Generation | 3 | `generate_token()` |
| 2. Token Hashing | 3 | `hash_token()` |
| 3. Token Data Model | 4 | `ResetToken` dataclass |
| 4. Expiry Check | 3 | `is_expired()` |
| 5. Single-Use Check | 2 | `is_used()` |
| 6. Combined Validation | 4 | `validate_token()` |
| 7. Mark Used | 3 | `mark_used()` |
| 8. Create Token Helper | 5 | `create_token()` |
| 9. Final Verification | — | — |
| **Total** | **27** | **8 exports** |

## Password Complexity (Deferred)

Password complexity validation is NOT part of this sub-issue. It belongs in the "Reset password endpoint" sub-issue since complexity is enforced when setting the new password, not when generating/validating tokens.

Standard rules when implemented:
- Minimum 8 characters
- At least one uppercase letter
- At least one lowercase letter
- At least one number
- At least one special character

## Not In Scope

The following are explicitly out of scope for this sub-issue:
- Database/storage implementation (tokens are pure data)
- HTTP endpoints
- Email sending
- Session invalidation
- Rate limiting
