---
title: "feat: Implement Login/Logout Authentication Flow"
type: feat
date: 2026-02-02
issue: LEG-13
depends_on: LEG-12 (session management)
---

# feat: Implement Login/Logout Authentication Flow

## Overview

Implement secure login/logout authentication endpoints for Legion. Users authenticate with email/password, receive a session token on success, and can invalidate their session via logout. Security is prioritized with generic error messages, rate limiting, timing attack prevention, and audit logging.

## Problem Statement

Legion needs user authentication to control access to the system. The session management infrastructure (LEG-12) is complete; this feature adds the authentication layer on top of it.

## Proposed Solution

Create an `auth` module with login/logout functions following security best practices:
- bcrypt for password verification with timing attack prevention
- Rate limiting (sliding window counter) per IP
- Session token generation via LEG-12's session store
- Audit logging of all authentication events

## Technical Approach

### Architecture

```
src/legion/
├── auth.py              # Login/logout logic, rate limiting
└── auth_types.py        # AuthResult, RateLimitResult types

tests/
└── test_auth.py         # TDD tests for all flows
```

### Dependencies

Add to `pyproject.toml`:
```toml
dependencies = [
    # ... existing ...
    "bcrypt>=4.0",
]
```

### Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| bcrypt cost factor | 13 | 2026 minimum, ~250ms per hash, configurable |
| Rate limit algorithm | Sliding window counter | Good balance of precision and memory |
| Rate limit check timing | Before credential validation | Fail-fast, prevents timing oracle |
| Successful login rate limit | Not counted, not reset | Prevents probing then succeeding |
| X-Forwarded-For | Not trusted | Can be spoofed; document limitation |
| Disabled/locked accounts | Treated as non-existent | Prevents account enumeration |
| Session concurrency | Multiple allowed | Check LEG-12 for existing behavior |

### Assumptions (Documented)

1. **Session management (LEG-12)** provides:
   - `create_session(*, config, storage, metadata) -> (token, Session)` - creates session with optional metadata
   - `validate_session(token, *, storage) -> Session | None` - validates token and returns session
   - `revoke_session(token, *, storage) -> bool` - revokes session, returns True if deleted
2. **User lookup** will be injected: `get_user_by_email(email) -> User | None` with `password_hash` field
3. **Rate limit storage** is in-memory initially (can upgrade to Redis later)
4. **Token response format**: JSON body `{"session_token": "..."}`
5. **HTTP status codes**: 200 (success), 401 (invalid credentials), 429 (rate limited), 400 (validation error)

## Acceptance Criteria

### Functional Requirements

- [ ] Login with email/password returns session token on success
- [ ] Login with invalid credentials returns generic error
- [ ] Login with non-existent email returns same error (no enumeration)
- [ ] Rate limit: 5 attempts allowed per 15 minutes per IP
- [ ] Rate limit: 6th attempt returns 429 with Retry-After header
- [ ] Logout invalidates session server-side
- [ ] Logout is idempotent (success even if session already invalid)
- [ ] Login records IP and user_agent in session metadata

### Non-Functional Requirements

- [ ] Timing attack prevention: dummy bcrypt check for non-existent users
- [ ] bcrypt cost factor 13 (configurable via constant)
- [ ] Session token: 32 bytes via `secrets.token_urlsafe(32)`
- [ ] Generic error message: "Invalid email or password"
- [ ] Audit log: all login attempts (success/failure) and logouts

### Quality Gates

- [ ] 100% test coverage for auth module
- [ ] All tests pass with `uv run pytest tests/test_auth.py -v`
- [ ] No security linter warnings

## Implementation Phases

### Phase 1: Types and Rate Limiter

**Files:**
- `src/legion/auth_types.py` - Data types
- `src/legion/auth.py` - Rate limiter class
- `tests/test_auth.py` - Rate limiter tests

**Deliverables:**
- `AuthResult` dataclass
- `SlidingWindowRateLimiter` class with `is_allowed(key: str) -> bool`
- Tests for rate limit scenarios

### Phase 2: Login Function

**Files:**
- `src/legion/auth.py` - Add `login()` function
- `tests/test_auth.py` - Login tests

**Deliverables:**
- `login(email, password, ip, user_agent)` function
- Timing-safe credential verification
- Integration with rate limiter
- Integration with session store (LEG-12)
- Tests for all login scenarios

### Phase 3: Logout Function

**Files:**
- `src/legion/auth.py` - Add `logout()` function
- `tests/test_auth.py` - Logout tests

**Deliverables:**
- `logout(session_token)` function
- Idempotent behavior
- Audit logging
- Tests for logout scenarios

## Test Plan (TDD)

### Rate Limiter Tests

```python
# tests/test_auth.py

class TestSlidingWindowRateLimiter:
    @pytest.mark.anyio
    async def test_allows_first_request(self) -> None:
        """First request for an IP should be allowed."""

    @pytest.mark.anyio
    async def test_allows_up_to_max_requests(self) -> None:
        """Requests 1-5 should all be allowed."""

    @pytest.mark.anyio
    async def test_blocks_after_max_requests(self) -> None:
        """6th request within window should be blocked."""

    @pytest.mark.anyio
    async def test_allows_after_window_expires(self) -> None:
        """Requests allowed again after 15 minutes."""

    @pytest.mark.anyio
    async def test_sliding_window_partial_recovery(self) -> None:
        """Requests recover proportionally as window slides."""

    @pytest.mark.anyio
    async def test_independent_keys(self) -> None:
        """Different IPs have independent rate limits."""
```

### Login Tests

```python
class TestLogin:
    @pytest.mark.anyio
    async def test_successful_login_returns_session_token(self) -> None:
        """Valid credentials return AuthResult with session token."""

    @pytest.mark.anyio
    async def test_invalid_password_returns_generic_error(self) -> None:
        """Wrong password returns 'Invalid email or password'."""

    @pytest.mark.anyio
    async def test_nonexistent_email_returns_same_error(self) -> None:
        """Non-existent email returns same error as wrong password."""

    @pytest.mark.anyio
    async def test_nonexistent_email_has_similar_timing(self) -> None:
        """Non-existent email takes similar time (timing attack prevention)."""

    @pytest.mark.anyio
    async def test_rate_limited_returns_429(self) -> None:
        """6th attempt returns rate limit error."""

    @pytest.mark.anyio
    async def test_records_ip_and_user_agent(self) -> None:
        """Successful login records IP and user_agent in session."""

    @pytest.mark.anyio
    async def test_empty_email_returns_validation_error(self) -> None:
        """Empty email returns 400 validation error."""

    @pytest.mark.anyio
    async def test_empty_password_returns_validation_error(self) -> None:
        """Empty password returns 400 validation error."""
```

### Logout Tests

```python
class TestLogout:
    @pytest.mark.anyio
    async def test_logout_invalidates_session(self) -> None:
        """Logout deletes session from store."""

    @pytest.mark.anyio
    async def test_logout_is_idempotent(self) -> None:
        """Logout on invalid/expired token returns success."""

    @pytest.mark.anyio
    async def test_logout_records_audit_event(self) -> None:
        """Logout logs the event with session info."""
```

## API Contract

### Login

**Request:**
```
POST /api/auth/login
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "secret123"
}
```

**Success Response (200):**
```json
{
  "session_token": "abc123...",
  "message": "Login successful"
}
```

**Error Response (401):**
```json
{
  "error": "Invalid email or password"
}
```

**Rate Limited Response (429):**
```json
{
  "error": "Too many login attempts. Try again later."
}
Headers: Retry-After: 900
```

### Logout

**Request:**
```
POST /api/auth/logout
Authorization: Bearer <session_token>
```

**Success Response (200):**
```json
{
  "message": "Logged out successfully"
}
```

## Security Considerations

1. **Timing Attacks:** Always perform bcrypt check (dummy hash for non-existent users)
2. **Account Enumeration:** Same error message and timing for all failures
3. **Rate Limiting:** Per-IP sliding window prevents brute force
4. **Session Tokens:** 256-bit entropy via `secrets.token_urlsafe(32)`
5. **Password Comparison:** Use `bcrypt.checkpw()` (constant-time internally)
6. **Audit Trail:** Log all authentication events for security monitoring

## Out of Scope

- User registration (separate feature)
- Password reset flow (separate feature)
- Multi-factor authentication (future enhancement)
- Per-account rate limiting (defense-in-depth, future enhancement)
- X-Forwarded-For trust configuration (document as limitation)
- CLI integration (this is the core auth module only)

## References

### Internal

- LEG-12: Session management (dependency)
- `src/legion/state/types.py:302-318` - Example of UUID-based ID generation
- `src/legion/daemon.py:28-33` - Input validation pattern

### External

- [OWASP Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
- [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
- [Python secrets module](https://docs.python.org/3/library/secrets.html)
- [pyca/bcrypt](https://github.com/pyca/bcrypt)
