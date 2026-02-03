"""Tests for session management infrastructure."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from legion import session


class TestSessionDataclass:
    """Tests for Session dataclass."""

    def test_session_is_frozen(self) -> None:
        """Session should be immutable (frozen dataclass)."""
        sess = session.Session(
            session_id="test1234",
            token_hash="fakehash",
            created_at=1000.0,
            last_accessed_at=1000.0,
            expires_at=2000.0,
            idle_timeout_secs=1800,
            metadata=None,
        )
        with pytest.raises(AttributeError):
            sess.session_id = "changed"  # pyright: ignore[reportAttributeAccessIssue]

    def test_session_fields(self) -> None:
        """Session should have all required fields."""
        sess = session.Session(
            session_id="test1234",
            token_hash="fakehash",
            created_at=1000.0,
            last_accessed_at=1000.0,
            expires_at=2000.0,
            idle_timeout_secs=1800,
            metadata={"user": "test"},
        )
        assert sess.session_id == "test1234"
        assert sess.token_hash == "fakehash"
        assert sess.created_at == 1000.0
        assert sess.last_accessed_at == 1000.0
        assert sess.expires_at == 2000.0
        assert sess.idle_timeout_secs == 1800
        assert sess.metadata == {"user": "test"}


class TestSessionConfig:
    """Tests for SessionConfig dataclass."""

    def test_default_values(self) -> None:
        """SessionConfig should have sensible defaults."""
        config = session.SessionConfig()
        assert config.idle_timeout_secs == 1800  # 30 minutes
        assert config.absolute_timeout_secs == 28800  # 8 hours
        assert config.token_bytes == 32  # 256-bit entropy
        assert config.storage_dir is None


class TestSessionStorageProtocol:
    """Tests for SessionStorage Protocol."""

    def test_protocol_defines_methods(self) -> None:
        """SessionStorage should define save, load, delete methods."""
        # Protocol should be importable and have the expected methods
        assert hasattr(session.SessionStorage, "save")
        assert hasattr(session.SessionStorage, "load")
        assert hasattr(session.SessionStorage, "delete")


class TestTokenUtilities:
    """Tests for token generation and parsing."""

    def test_generate_token_format(self) -> None:
        """Token should be session_id.secret format."""
        token = session._generate_token("abc12345", token_bytes=32)
        assert "." in token
        parts = token.split(".", 1)
        assert parts[0] == "abc12345"
        assert len(parts[1]) == 43  # Base64 encoding of 32 bytes

    def test_parse_token_valid(self) -> None:
        """Valid token should parse into session_id and secret."""
        # Generate a valid token first
        token = session._generate_token("abc12345", token_bytes=32)
        result = session._parse_token(token)
        assert result is not None
        assert result[0] == "abc12345"

    def test_parse_token_invalid_no_dot(self) -> None:
        """Token without dot should return None."""
        assert session._parse_token("nodothere") is None

    def test_parse_token_invalid_empty(self) -> None:
        """Empty token should return None."""
        assert session._parse_token("") is None

    def test_parse_token_invalid_too_long(self) -> None:
        """Oversized token should return None (DoS prevention)."""
        long_token = "a" * 100
        assert session._parse_token(long_token) is None

    def test_parse_token_invalid_format(self) -> None:
        """Token with invalid characters should return None."""
        assert session._parse_token("../../../etc.passwd") is None
        assert session._parse_token("abc12345.secret with spaces") is None

    def test_validate_session_id_valid(self) -> None:
        """Valid session ID should pass validation."""
        assert session._validate_session_id("abc12345") is True
        assert session._validate_session_id("ABCD1234") is True
        assert session._validate_session_id("a1b2c3d4") is True

    def test_validate_session_id_invalid(self) -> None:
        """Invalid session ID should fail validation."""
        assert session._validate_session_id("") is False
        assert session._validate_session_id("short") is False  # Too short
        assert session._validate_session_id("toolongid") is False  # Too long
        assert session._validate_session_id("../../../") is False  # Path traversal
        assert session._validate_session_id("abc.1234") is False  # Invalid char

    def test_hash_token_deterministic(self) -> None:
        """Same token should produce same hash."""
        token = "abc12345.secretpart"
        hash1 = session._hash_token(token)
        hash2 = session._hash_token(token)
        assert hash1 == hash2

    def test_hash_token_different_inputs(self) -> None:
        """Different tokens should produce different hashes."""
        hash1 = session._hash_token("token1")
        hash2 = session._hash_token("token2")
        assert hash1 != hash2

    def test_validate_token_hash_correct(self) -> None:
        """Correct token should validate against its hash."""
        token = "abc12345.secretpartsecretpartsecretpartsecr"
        stored_hash = session._hash_token(token)
        assert session._validate_token_hash(token, stored_hash) is True

    def test_validate_token_hash_incorrect(self) -> None:
        """Incorrect token should not validate."""
        stored_hash = session._hash_token("abc12345.correct")
        assert session._validate_token_hash("abc12345.wrong", stored_hash) is False


class TestFileSessionStorage:
    """Tests for file-based session storage."""

    @pytest.fixture
    def storage(self, tmp_path: Path) -> session.FileSessionStorage:
        return session.FileSessionStorage(tmp_path)

    @pytest.fixture
    def sample_session(self) -> session.Session:
        return session.Session(
            session_id="test1234",
            token_hash="fakehash",
            created_at=1000.0,
            last_accessed_at=1000.0,
            expires_at=2000.0,
            idle_timeout_secs=1800,
            metadata={"user": "test"},
        )

    @pytest.mark.anyio
    async def test_save_and_load(
        self, storage: session.FileSessionStorage, sample_session: session.Session
    ) -> None:
        """Session should round-trip through save and load."""
        await storage.save(sample_session)
        loaded = await storage.load(sample_session.session_id)

        assert loaded is not None
        assert loaded.session_id == sample_session.session_id
        assert loaded.token_hash == sample_session.token_hash
        assert loaded.metadata == sample_session.metadata

    @pytest.mark.anyio
    async def test_load_nonexistent(self, storage: session.FileSessionStorage) -> None:
        """Loading nonexistent session should return None."""
        loaded = await storage.load("nonexist")
        assert loaded is None

    @pytest.mark.anyio
    async def test_load_invalid_session_id(
        self, storage: session.FileSessionStorage
    ) -> None:
        """Loading with invalid session ID should return None."""
        loaded = await storage.load("../../../etc")
        assert loaded is None

    @pytest.mark.anyio
    async def test_delete_existing(
        self, storage: session.FileSessionStorage, sample_session: session.Session
    ) -> None:
        """Deleting existing session should return True."""
        await storage.save(sample_session)
        result = await storage.delete(sample_session.session_id)
        assert result is True

        # Verify deleted
        loaded = await storage.load(sample_session.session_id)
        assert loaded is None

    @pytest.mark.anyio
    async def test_delete_nonexistent(
        self, storage: session.FileSessionStorage
    ) -> None:
        """Deleting nonexistent session should return False."""
        result = await storage.delete("nonexist")
        assert result is False

    @pytest.mark.anyio
    async def test_load_corrupted_json(
        self, storage: session.FileSessionStorage, tmp_path: Path
    ) -> None:
        """Corrupted JSON should return None, not raise."""
        # Write invalid JSON
        path = tmp_path / "corrupt1.json"
        path.write_text("not valid json")

        loaded = await storage.load("corrupt1")
        assert loaded is None

    @pytest.mark.anyio
    async def test_load_missing_fields(
        self, storage: session.FileSessionStorage, tmp_path: Path
    ) -> None:
        """Missing fields should return None, not raise."""
        path = tmp_path / "partial1.json"
        path.write_text(json.dumps({"session_id": "partial1"}))

        loaded = await storage.load("partial1")
        assert loaded is None

    def test_storage_dir_created_with_restricted_permissions(
        self, tmp_path: Path
    ) -> None:
        """Storage directory should be created with mode 0o700."""
        import os

        storage_dir = tmp_path / "sessions"
        session.FileSessionStorage(storage_dir)
        assert storage_dir.exists()
        # Check permissions (on POSIX systems)
        if os.name == "posix":
            mode = storage_dir.stat().st_mode & 0o777
            assert mode == 0o700


class TestCreateSession:
    """Tests for create_session function."""

    @pytest.mark.anyio
    async def test_creates_valid_session(self, tmp_path: Path) -> None:
        """Should create session with valid token and stored record."""
        config = session.SessionConfig(storage_dir=tmp_path)
        token, sess = await session.create_session(config=config)

        assert "." in token
        assert sess.session_id in token
        assert sess.token_hash == session._hash_token(token)

    @pytest.mark.anyio
    async def test_session_stored(self, tmp_path: Path) -> None:
        """Session should be persisted to storage."""
        config = session.SessionConfig(storage_dir=tmp_path)
        token, sess = await session.create_session(config=config)

        # Verify file exists
        session_file = tmp_path / f"{sess.session_id}.json"
        assert session_file.exists()

    @pytest.mark.anyio
    async def test_metadata_stored(self, tmp_path: Path) -> None:
        """Metadata should be stored with session."""
        config = session.SessionConfig(storage_dir=tmp_path)
        metadata = {"user_id": "u123", "role": "worker"}
        token, sess = await session.create_session(config=config, metadata=metadata)

        assert sess.metadata == metadata

    @pytest.mark.anyio
    async def test_expiration_configured(self, tmp_path: Path) -> None:
        """Session should have configured expiration."""
        config = session.SessionConfig(
            storage_dir=tmp_path,
            idle_timeout_secs=60,
            absolute_timeout_secs=3600,
        )
        token, sess = await session.create_session(config=config)

        assert sess.idle_timeout_secs == 60
        assert sess.expires_at - sess.created_at == pytest.approx(3600, abs=1)

    @pytest.mark.anyio
    async def test_unique_tokens(self, tmp_path: Path) -> None:
        """Each session should have a unique token."""
        config = session.SessionConfig(storage_dir=tmp_path)
        token1, _ = await session.create_session(config=config)
        token2, _ = await session.create_session(config=config)

        assert token1 != token2


class TestValidateSession:
    """Tests for validate_session function."""

    @pytest.mark.anyio
    async def test_valid_session(self, tmp_path: Path) -> None:
        """Valid token should return session."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, _ = await session.create_session(config=config, storage=storage)

        result = await session.validate_session(token, storage=storage)
        assert result is not None

    @pytest.mark.anyio
    async def test_invalid_token_format(self, tmp_path: Path) -> None:
        """Invalid token format should return None."""
        storage = session.FileSessionStorage(tmp_path)

        result = await session.validate_session("invalidtoken", storage=storage)
        assert result is None

    @pytest.mark.anyio
    async def test_wrong_token_secret(self, tmp_path: Path) -> None:
        """Wrong secret should return None."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        # Create token with same session_id but wrong secret (must match format)
        wrong_token = f"{sess.session_id}.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
        result = await session.validate_session(wrong_token, storage=storage)
        assert result is None

    @pytest.mark.anyio
    async def test_nonexistent_session(self, tmp_path: Path) -> None:
        """Token for nonexistent session should return None."""
        storage = session.FileSessionStorage(tmp_path)

        # Valid format but session doesn't exist
        fake_token = "fake1234.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
        result = await session.validate_session(fake_token, storage=storage)
        assert result is None

    @pytest.mark.anyio
    async def test_expired_absolute(self, tmp_path: Path) -> None:
        """Expired session should return None and be deleted."""
        config = session.SessionConfig(
            storage_dir=tmp_path,
            absolute_timeout_secs=-1,  # Already expired
        )
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        result = await session.validate_session(token, storage=storage)
        assert result is None

        # Verify deleted
        loaded = await storage.load(sess.session_id)
        assert loaded is None

    @pytest.mark.anyio
    async def test_expired_idle(self, tmp_path: Path) -> None:
        """Idle-expired session should return None."""
        import asyncio

        config = session.SessionConfig(
            storage_dir=tmp_path,
            idle_timeout_secs=0,  # Immediate expiration
            absolute_timeout_secs=3600,
        )
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        # Small delay to ensure idle timeout
        await asyncio.sleep(0.01)

        result = await session.validate_session(token, storage=storage)
        assert result is None


class TestRefreshSession:
    """Tests for refresh_session function."""

    @pytest.mark.anyio
    async def test_refresh_updates_last_accessed(self, tmp_path: Path) -> None:
        """Refresh should update last_accessed_at."""
        import asyncio

        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        original_accessed = sess.last_accessed_at

        await asyncio.sleep(0.05)

        refreshed = await session.refresh_session(token, storage=storage)
        assert refreshed is not None
        assert refreshed.last_accessed_at > original_accessed

    @pytest.mark.anyio
    async def test_refresh_preserves_expires_at(self, tmp_path: Path) -> None:
        """Refresh should NOT extend absolute expiration."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        refreshed = await session.refresh_session(token, storage=storage)
        assert refreshed is not None
        assert refreshed.expires_at == sess.expires_at

    @pytest.mark.anyio
    async def test_refresh_invalid_token(self, tmp_path: Path) -> None:
        """Refresh with invalid token should return None."""
        storage = session.FileSessionStorage(tmp_path)

        result = await session.refresh_session("invalid.token", storage=storage)
        assert result is None

    @pytest.mark.anyio
    async def test_refresh_persists_update(self, tmp_path: Path) -> None:
        """Refresh should persist the updated session."""
        import asyncio

        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        await asyncio.sleep(0.05)

        await session.refresh_session(token, storage=storage)

        # Load directly from storage to verify persistence
        loaded = await storage.load(sess.session_id)
        assert loaded is not None
        assert loaded.last_accessed_at > sess.last_accessed_at


class TestRevokeSession:
    """Tests for revoke_session function."""

    @pytest.mark.anyio
    async def test_revoke_existing(self, tmp_path: Path) -> None:
        """Revoking existing session should return True."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, _ = await session.create_session(config=config, storage=storage)

        result = await session.revoke_session(token, storage=storage)
        assert result is True

        # Verify session is gone
        validated = await session.validate_session(token, storage=storage)
        assert validated is None

    @pytest.mark.anyio
    async def test_revoke_nonexistent(self, tmp_path: Path) -> None:
        """Revoking nonexistent session should return False."""
        storage = session.FileSessionStorage(tmp_path)

        # Valid format but doesn't exist
        result = await session.revoke_session(
            "fake1234.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            storage=storage,
        )
        assert result is False

    @pytest.mark.anyio
    async def test_revoke_invalid_token(self, tmp_path: Path) -> None:
        """Revoking with invalid token format should return False."""
        storage = session.FileSessionStorage(tmp_path)

        result = await session.revoke_session("invalidformat", storage=storage)
        assert result is False

    @pytest.mark.anyio
    async def test_revoke_idempotent(self, tmp_path: Path) -> None:
        """Revoking same session twice should be safe."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, _ = await session.create_session(config=config, storage=storage)

        result1 = await session.revoke_session(token, storage=storage)
        result2 = await session.revoke_session(token, storage=storage)

        assert result1 is True
        assert result2 is False  # Already gone
