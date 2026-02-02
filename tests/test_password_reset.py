"""Tests for password reset token module."""

from datetime import datetime, timedelta, timezone

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
