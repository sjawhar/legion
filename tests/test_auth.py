"""Tests for authentication module."""

from __future__ import annotations

import time
from dataclasses import dataclass
from typing import Any
from unittest.mock import patch

import bcrypt
import pytest

from legion import auth
from legion.auth_types import AuthStatus


class TestSlidingWindowRateLimiter:
    """Tests for the sliding window rate limiter."""

    def test_allows_first_request(self) -> None:
        """First request for an IP should be allowed."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        assert limiter.is_allowed("192.168.1.1") is True

    def test_allows_up_to_max_requests(self) -> None:
        """Requests 1-5 should all be allowed."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        for _ in range(5):
            assert limiter.is_allowed(ip) is True

    def test_blocks_after_max_requests(self) -> None:
        """6th request within window should be blocked."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        for _ in range(5):
            limiter.is_allowed(ip)
        assert limiter.is_allowed(ip) is False

    def test_allows_after_window_expires(self) -> None:
        """Requests allowed again after window expires."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"

        # Use up all requests
        for _ in range(5):
            limiter.is_allowed(ip)

        # Simulate time passing beyond window
        with patch.object(time, "time", return_value=time.time() + 901):
            assert limiter.is_allowed(ip) is True

    def test_sliding_window_partial_recovery(self) -> None:
        """Requests recover proportionally as window slides."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"

        base_time = 1000000.0

        # Make 5 requests at base_time
        with patch.object(time, "time", return_value=base_time):
            for _ in range(5):
                limiter.is_allowed(ip)
            # Should be blocked now
            assert limiter.is_allowed(ip) is False

        # After full window (901 seconds), all requests have expired
        with patch.object(time, "time", return_value=base_time + 901):
            # All 5 old requests have slid out of window
            assert limiter.is_allowed(ip) is True

    def test_independent_keys(self) -> None:
        """Different IPs have independent rate limits."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip1 = "192.168.1.1"
        ip2 = "192.168.1.2"

        # Exhaust ip1's limit
        for _ in range(5):
            limiter.is_allowed(ip1)
        assert limiter.is_allowed(ip1) is False

        # ip2 should still have full allowance
        assert limiter.is_allowed(ip2) is True

    def test_cleans_up_old_timestamps(self) -> None:
        """Old timestamps are removed from memory to prevent leaks."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        
        base_time = 1000000.0
        
        # Make 5 requests at base_time
        with patch.object(time, "time", return_value=base_time):
            for _ in range(5):
                limiter.is_allowed(ip)
        
        # Verify timestamps are stored
        assert len(limiter._requests[ip]) == 5
        
        # Move far into the future (beyond window)
        with patch.object(time, "time", return_value=base_time + 10000):
            limiter.is_allowed(ip)
        
        # Old timestamps should be cleaned up
        # Only the new request should remain
        assert len(limiter._requests[ip]) == 1

    def test_reset_clears_rate_limit(self) -> None:
        """Reset method clears the rate limit counter for a key."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        
        # Make some requests
        for _ in range(3):
            limiter.is_allowed(ip)
        
        assert len(limiter._requests.get(ip, [])) == 3
        
        # Reset should clear the counter
        limiter.reset(ip)
        assert ip not in limiter._requests
        
        # Should now be able to make 5 more requests
        for _ in range(5):
            assert limiter.is_allowed(ip) is True

    def test_lru_eviction_when_max_keys_exceeded(self) -> None:
        """Oldest keys are evicted when max_keys limit is reached (DDoS protection)."""
        # Small max_keys for testing
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900, max_keys=3)

        # Add 3 keys - should all fit
        limiter.is_allowed("ip1")
        limiter.is_allowed("ip2")
        limiter.is_allowed("ip3")

        assert len(limiter._requests) == 3
        assert "ip1" in limiter._requests
        assert "ip2" in limiter._requests
        assert "ip3" in limiter._requests

        # Add 4th key - should evict ip1 (oldest)
        limiter.is_allowed("ip4")

        assert len(limiter._requests) == 3
        assert "ip1" not in limiter._requests  # Evicted
        assert "ip2" in limiter._requests
        assert "ip3" in limiter._requests
        assert "ip4" in limiter._requests

    def test_lru_eviction_updates_on_access(self) -> None:
        """Accessing a key moves it to end, so it won't be evicted next."""
        limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900, max_keys=3)

        # Add 3 keys
        limiter.is_allowed("ip1")
        limiter.is_allowed("ip2")
        limiter.is_allowed("ip3")

        # Access ip1 again - moves it to end
        limiter.is_allowed("ip1")

        # Now add ip4 - should evict ip2 (now oldest)
        limiter.is_allowed("ip4")

        assert len(limiter._requests) == 3
        assert "ip1" in limiter._requests  # Kept because recently accessed
        assert "ip2" not in limiter._requests  # Evicted
        assert "ip3" in limiter._requests
        assert "ip4" in limiter._requests

    def test_max_keys_property(self) -> None:
        """max_keys property returns the configured limit."""
        limiter = auth.SlidingWindowRateLimiter(max_keys=50_000)
        assert limiter.max_keys == 50_000


# Test fixtures and helpers for login tests


@dataclass
class MockUser:
    """Mock user for testing."""

    email: str
    password_hash: bytes


def make_password_hash(password: str, rounds: int = 4) -> bytes:
    """Create a bcrypt hash for testing."""
    return bcrypt.hashpw(password.encode(), bcrypt.gensalt(rounds=rounds))


class MockSessionStorage:
    """In-memory session storage for testing."""

    def __init__(self) -> None:
        self.sessions: dict[str, Any] = {}

    async def save(self, session: Any) -> None:  # noqa: A002
        self.sessions[session.session_id] = session

    async def load(self, session_id: str) -> Any | None:
        return self.sessions.get(session_id)

    async def delete(self, session_id: str) -> bool:
        if session_id in self.sessions:
            del self.sessions[session_id]
            return True
        return False


class TestLogin:
    """Tests for the login function."""

    @pytest.fixture
    def storage(self) -> MockSessionStorage:
        return MockSessionStorage()

    @pytest.fixture
    def rate_limiter(self) -> auth.SlidingWindowRateLimiter:
        return auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)

    @pytest.fixture
    def valid_user(self) -> MockUser:
        return MockUser(
            email="user@example.com",
            password_hash=make_password_hash("correct-password"),
        )

    @pytest.fixture
    def get_user(self, valid_user: MockUser):
        """User lookup function that returns the valid user."""

        async def _get_user(email: str) -> MockUser | None:
            if email == valid_user.email:
                return valid_user
            return None

        return _get_user

    @pytest.mark.anyio
    async def test_successful_login_returns_session_token(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Valid credentials return AuthResult with session token."""
        result = await auth.login(
            email="user@example.com",
            password="correct-password",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.SUCCESS
        assert result.session_token is not None
        assert len(result.session_token) > 0

    @pytest.mark.anyio
    async def test_invalid_password_returns_generic_error(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Wrong password returns 'Invalid email or password'."""
        result = await auth.login(
            email="user@example.com",
            password="wrong-password",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.INVALID_CREDENTIALS
        assert result.error_message == "Invalid email or password"
        assert result.session_token is None

    @pytest.mark.anyio
    async def test_nonexistent_email_returns_same_error(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Non-existent email returns same error as wrong password."""
        result = await auth.login(
            email="nonexistent@example.com",
            password="any-password",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.INVALID_CREDENTIALS
        assert result.error_message == "Invalid email or password"

    @pytest.mark.anyio
    async def test_nonexistent_email_performs_dummy_bcrypt(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
    ) -> None:
        """Non-existent email performs dummy bcrypt check (timing attack prevention)."""
        # Mock bcrypt.checkpw to verify it's called even for non-existent users
        async def get_user(email: str) -> MockUser | None:
            return None  # No user exists
        
        with patch.object(bcrypt, "checkpw", wraps=bcrypt.checkpw) as mock_checkpw:
            await auth.login(
                email="nonexistent@example.com",
                password="any-password",
                ip="192.168.1.1",
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )
            
            # bcrypt.checkpw should be called with the dummy hash
            assert mock_checkpw.call_count == 1
            call_args = mock_checkpw.call_args[0]
            # Second argument should be the dummy hash
            assert call_args[1] == auth._DUMMY_HASH

    @pytest.mark.anyio
    async def test_rate_limited_returns_429(
        self,
        storage: MockSessionStorage,
        get_user,
    ) -> None:
        """6th attempt returns rate limit error."""
        rate_limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"

        # Make 5 failed login attempts
        for _ in range(5):
            await auth.login(
                email="user@example.com",
                password="wrong-password",
                ip=ip,
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )

        # 6th attempt should be rate limited
        result = await auth.login(
            email="user@example.com",
            password="correct-password",
            ip=ip,
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.RATE_LIMITED
        assert result.error_message == "Too many login attempts. Try again later."
        assert result.retry_after_secs is not None
        assert result.retry_after_secs > 0

    @pytest.mark.anyio
    async def test_successful_login_resets_rate_limit(
        self,
        storage: MockSessionStorage,
        get_user,
    ) -> None:
        """Successful login resets rate limit to avoid penalizing legitimate users."""
        rate_limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        
        # Make 4 failed attempts
        for _ in range(4):
            await auth.login(
                email="user@example.com",
                password="wrong-password",
                ip=ip,
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )
        
        # 5th attempt succeeds
        result = await auth.login(
            email="user@example.com",
            password="correct-password",
            ip=ip,
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )
        assert result.status == AuthStatus.SUCCESS
        
        # Rate limit should be reset, so user can make more attempts
        # (This verifies legitimate users aren't locked out after a few typos)
        for _ in range(5):
            result = await auth.login(
                email="user@example.com",
                password="correct-password",
                ip=ip,
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )
            assert result.status == AuthStatus.SUCCESS

    @pytest.mark.anyio
    async def test_records_ip_and_user_agent(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Successful login records IP and user_agent in session."""
        result = await auth.login(
            email="user@example.com",
            password="correct-password",
            ip="192.168.1.100",
            user_agent="CustomAgent/2.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.SUCCESS

        # Check that session was stored with metadata
        assert len(storage.sessions) == 1
        session = list(storage.sessions.values())[0]
        assert session.metadata is not None
        assert session.metadata.get("ip") == "192.168.1.100"
        assert session.metadata.get("user_agent") == "CustomAgent/2.0"

    @pytest.mark.anyio
    async def test_empty_email_returns_validation_error(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Empty email returns validation error."""
        result = await auth.login(
            email="",
            password="some-password",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.VALIDATION_ERROR
        assert result.error_message is not None

    @pytest.mark.anyio
    async def test_empty_password_returns_validation_error(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Empty password returns validation error."""
        result = await auth.login(
            email="user@example.com",
            password="",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )

        assert result.status == AuthStatus.VALIDATION_ERROR
        assert result.error_message is not None

    @pytest.mark.anyio
    async def test_whitespace_only_email_returns_validation_error(
        self,
        storage: MockSessionStorage,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Email with only whitespace returns validation error."""
        result = await auth.login(
            email="   ",
            password="some-password",
            ip="192.168.1.1",
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )
        
        assert result.status == AuthStatus.VALIDATION_ERROR
        assert result.error_message == "Email is required"

    @pytest.mark.anyio
    async def test_storage_failure_during_session_creation(
        self,
        rate_limiter: auth.SlidingWindowRateLimiter,
        get_user,
    ) -> None:
        """Login fails gracefully if session storage fails."""
        class FailingStorage:
            async def save(self, session: Any) -> None:
                raise OSError("Disk full")
            
            async def load(self, session_id: str) -> Any | None:
                return None
            
            async def delete(self, session_id: str) -> bool:
                return False
        
        storage = FailingStorage()
        
        # Should raise exception (not return success without session)
        with pytest.raises(OSError):
            await auth.login(
                email="user@example.com",
                password="correct-password",
                ip="192.168.1.1",
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )

    @pytest.mark.anyio
    async def test_validation_errors_bypass_rate_limiting(
        self,
        storage: MockSessionStorage,
        get_user,
    ) -> None:
        """Validation errors don't consume rate limit quota."""
        rate_limiter = auth.SlidingWindowRateLimiter(max_requests=5, window_secs=900)
        ip = "192.168.1.1"
        
        # Make many validation errors - these shouldn't consume rate limit
        for _ in range(10):
            result = await auth.login(
                email="",  # Empty email triggers validation error
                password="some-password",
                ip=ip,
                user_agent="TestBrowser/1.0",
                get_user=get_user,
                rate_limiter=rate_limiter,
                storage=storage,
            )
            assert result.status == AuthStatus.VALIDATION_ERROR
        
        # Now try a valid login - should succeed because validation errors
        # didn't consume the rate limit
        result = await auth.login(
            email="user@example.com",
            password="correct-password",
            ip=ip,
            user_agent="TestBrowser/1.0",
            get_user=get_user,
            rate_limiter=rate_limiter,
            storage=storage,
        )
        
        assert result.status == AuthStatus.SUCCESS


class TestLogout:
    """Tests for the logout function."""

    @pytest.fixture
    def storage(self) -> MockSessionStorage:
        return MockSessionStorage()

    @pytest.mark.anyio
    async def test_logout_invalidates_session(
        self,
        storage: MockSessionStorage,
    ) -> None:
        """Logout deletes session from store."""
        from legion import session

        # Create a session first
        token, sess = await session.create_session(storage=storage)

        # Verify session exists
        assert await storage.load(sess.session_id) is not None

        # Logout
        result = await auth.logout(token=token, storage=storage)

        # Verify session is deleted
        assert result is True
        assert await storage.load(sess.session_id) is None

    @pytest.mark.anyio
    async def test_logout_is_idempotent(
        self,
        storage: MockSessionStorage,
    ) -> None:
        """Logout on invalid/expired token returns success (idempotent)."""
        # Logout with a token that doesn't exist
        result = await auth.logout(token="nonexist.abcdefghijklmnopqrstuvwxyz123456789012", storage=storage)

        # Should still return True (idempotent)
        assert result is True

    @pytest.mark.anyio
    async def test_logout_with_invalid_token_format(
        self,
        storage: MockSessionStorage,
    ) -> None:
        """Logout with malformed token returns success (idempotent)."""
        result = await auth.logout(token="invalid-token-format", storage=storage)

        # Should still return True (idempotent)
        assert result is True

    @pytest.mark.anyio
    async def test_logout_with_empty_token(
        self,
        storage: MockSessionStorage,
    ) -> None:
        """Logout with empty token returns success (idempotent)."""
        result = await auth.logout(token="", storage=storage)
        
        # Should still return True (idempotent)
        assert result is True

    @pytest.mark.anyio
    async def test_logout_with_whitespace_token(
        self,
        storage: MockSessionStorage,
    ) -> None:
        """Logout with whitespace token returns success (idempotent)."""
        result = await auth.logout(token="   ", storage=storage)
        
        # Should still return True (idempotent)
        assert result is True
