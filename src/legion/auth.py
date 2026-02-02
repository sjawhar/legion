"""Authentication module for Legion."""

from __future__ import annotations

import time
from collections import OrderedDict
from collections.abc import Awaitable, Callable
from typing import Protocol

import bcrypt

from legion.auth_types import AuthResult, AuthStatus
from legion import session

# Security: bcrypt cost factor 13 (~250ms per hash in 2026)
BCRYPT_COST_FACTOR = 13

# Dummy hash for timing attack prevention (pre-computed with cost factor 13)
# This ensures non-existent users take similar time as existing users
_DUMMY_HASH = bcrypt.hashpw(b"dummy", bcrypt.gensalt(rounds=BCRYPT_COST_FACTOR))

# Default max keys for rate limiter to prevent memory exhaustion under DDoS
DEFAULT_MAX_KEYS = 100_000


class UserProtocol(Protocol):
    """Protocol for user objects."""

    email: str
    password_hash: bytes


class SlidingWindowRateLimiter:
    """Sliding window rate limiter for login attempts.

    Uses a sliding window counter algorithm that tracks request timestamps
    and calculates the weighted count based on window position.

    Security: Implements max_keys limit with LRU eviction to prevent memory
    exhaustion under DDoS attacks with many unique IPs.
    """

    def __init__(
        self,
        max_requests: int = 5,
        window_secs: int = 900,
        max_keys: int = DEFAULT_MAX_KEYS,
    ) -> None:
        self._max_requests = max_requests
        self._window_secs = window_secs
        self._max_keys = max_keys
        # OrderedDict for LRU eviction - most recently used keys are at the end
        self._requests: OrderedDict[str, list[float]] = OrderedDict()

    def is_allowed(self, key: str) -> bool:
        """Check if a request is allowed for the given key.

        Returns True and records the request if under limit.
        Returns False if rate limited.

        Uses a sliding window: requests expire proportionally as time passes,
        rather than all at once when the window resets.
        """
        now = time.time()
        window_start = now - self._window_secs

        # Get existing timestamps for this key
        timestamps = self._requests.get(key, [])

        # Remove timestamps completely outside the window
        timestamps = [ts for ts in timestamps if ts > window_start]

        # Clean up key entirely if no timestamps remain (prevents memory leak)
        if not timestamps:
            self._requests.pop(key, None)
            timestamps = []

        # Count requests still in window
        # Sliding behavior: older requests "decay" by being filtered out
        if len(timestamps) >= self._max_requests:
            self._requests[key] = timestamps
            # Move to end for LRU tracking
            self._requests.move_to_end(key)
            return False

        # Record this request
        timestamps.append(now)
        self._requests[key] = timestamps
        # Move to end for LRU tracking
        self._requests.move_to_end(key)

        # Evict oldest keys if we exceed max_keys (DDoS protection)
        self._evict_if_needed()

        return True

    def _evict_if_needed(self) -> None:
        """Evict oldest (least recently used) keys if over capacity."""
        while len(self._requests) > self._max_keys:
            # popitem(last=False) removes the oldest (first) item
            self._requests.popitem(last=False)

    def reset(self, key: str) -> None:
        """Reset the rate limit counter for a key.

        Called on successful login to avoid penalizing legitimate users.
        """
        self._requests.pop(key, None)

    @property
    def window_secs(self) -> int:
        """Return the window size in seconds."""
        return self._window_secs

    @property
    def max_keys(self) -> int:
        """Return the maximum number of keys before LRU eviction."""
        return self._max_keys


async def login(
    *,
    email: str,
    password: str,
    ip: str,
    user_agent: str,
    get_user: Callable[[str], Awaitable[UserProtocol | None]],
    rate_limiter: SlidingWindowRateLimiter,
    storage: session.SessionStorage,
) -> AuthResult:
    """Authenticate a user with email and password.

    Security features:
    - Rate limiting per IP (fail-fast before credential check)
    - Timing attack prevention (dummy bcrypt check for non-existent users)
    - Generic error messages (no account enumeration)
    - Session metadata recording (IP and user_agent)

    Args:
        email: User's email address
        password: User's password (plaintext)
        ip: Client IP address for rate limiting
        user_agent: Client user agent string
        get_user: Async function to lookup user by email
        rate_limiter: Rate limiter instance
        storage: Session storage instance

    Returns:
        AuthResult with status and session_token (on success) or error_message
    """
    # Validation: Check required fields
    if not email or not email.strip():
        return AuthResult(
            status=AuthStatus.VALIDATION_ERROR,
            error_message="Email is required",
        )
    if not password or not password.strip():
        return AuthResult(
            status=AuthStatus.VALIDATION_ERROR,
            error_message="Password is required",
        )

    # Normalize email: strip whitespace and lowercase for consistent lookup
    email = email.strip().lower()

    # Rate limit check BEFORE credential validation (fail-fast)
    if not rate_limiter.is_allowed(ip):
        return AuthResult(
            status=AuthStatus.RATE_LIMITED,
            error_message="Too many login attempts. Try again later.",
            retry_after_secs=rate_limiter.window_secs,
        )

    # Lookup user
    user = await get_user(email)

    # Timing attack prevention: always perform bcrypt check
    if user is not None:
        password_valid = bcrypt.checkpw(password.encode(), user.password_hash)
    else:
        # Dummy check to maintain consistent timing
        bcrypt.checkpw(password.encode(), _DUMMY_HASH)
        password_valid = False

    if not password_valid:
        return AuthResult(
            status=AuthStatus.INVALID_CREDENTIALS,
            error_message="Invalid email or password",
        )

    # Successful login: reset rate limit so legitimate users aren't penalized
    rate_limiter.reset(ip)

    # Create session with metadata
    token, _ = await session.create_session(
        storage=storage,
        metadata={
            "ip": ip,
            "user_agent": user_agent,
            "email": email,
        },
    )

    return AuthResult(
        status=AuthStatus.SUCCESS,
        session_token=token,
    )


async def logout(
    *,
    token: str,
    storage: session.SessionStorage,
) -> bool:
    """Invalidate a session.

    Idempotent: always returns True, even if the session was already
    invalid or the token format is malformed.

    Args:
        token: Session token to invalidate
        storage: Session storage instance

    Returns:
        True (always, for idempotent behavior)
    """
    # Delegate to session.revoke_session which handles all validation
    # We ignore the return value to maintain idempotent behavior
    await session.revoke_session(token, storage=storage)
    return True
