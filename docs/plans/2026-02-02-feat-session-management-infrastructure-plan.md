# Session Management Infrastructure Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Create foundational session management infrastructure for Legion with secure token generation, validation, refresh, and revocation capabilities.

**Architecture:** Module-level async functions (not classes) with Protocol-based dependency injection for storage abstraction. Composite tokens (`{session_id}.{secret}`) enable O(1) lookup. Dual timeouts (idle + absolute) per OWASP.

**Tech Stack:** Python 3.13+, anyio, aiofiles, secrets module, SHA-256 hashing

---

## Task 1: Create session types and dataclasses

**Files:**
- Create: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing test for Session dataclass**

```python
# tests/test_session.py
"""Tests for session management infrastructure."""

from __future__ import annotations

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
            sess.session_id = "changed"  # type: ignore[misc]

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
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestSessionDataclass -v`
Expected: FAIL with "ModuleNotFoundError: No module named 'legion.session'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py
"""Session management infrastructure for Legion."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class Session:
    """Immutable session record."""

    session_id: str
    token_hash: str
    created_at: float
    last_accessed_at: float
    expires_at: float
    idle_timeout_secs: int
    metadata: dict[str, Any] | None = None


@dataclass(frozen=True)
class SessionConfig:
    """Session configuration with sensible defaults."""

    idle_timeout_secs: int = 1800  # 30 minutes
    absolute_timeout_secs: int = 28800  # 8 hours
    token_bytes: int = 32  # 256-bit entropy
    storage_dir: Path | None = None


DEFAULT_CONFIG = SessionConfig()
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestSessionDataclass tests/test_session.py::TestSessionConfig -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add Session and SessionConfig dataclasses

- Session: frozen dataclass for immutable session records
- SessionConfig: configuration with OWASP-recommended defaults
- 30min idle timeout, 8hr absolute timeout, 256-bit entropy"
```

---

## Task 2: Add SessionStorage Protocol

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing test for Protocol**

```python
# tests/test_session.py (append to file)

class TestSessionStorageProtocol:
    """Tests for SessionStorage Protocol."""

    def test_protocol_defines_methods(self) -> None:
        """SessionStorage should define save, load, delete methods."""
        # Protocol should be importable and have the expected methods
        assert hasattr(session.SessionStorage, "save")
        assert hasattr(session.SessionStorage, "load")
        assert hasattr(session.SessionStorage, "delete")
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestSessionStorageProtocol -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'SessionStorage'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after SessionConfig, before DEFAULT_CONFIG)

from typing import Protocol


class SessionStorage(Protocol):
    """Protocol for session persistence."""

    async def save(self, session: Session) -> None:
        """Persist a session. Overwrites if exists."""
        ...

    async def load(self, session_id: str) -> Session | None:
        """Load a session by ID. Returns None if not found."""
        ...

    async def delete(self, session_id: str) -> bool:
        """Delete a session. Returns True if deleted, False if not found."""
        ...
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestSessionStorageProtocol -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add SessionStorage Protocol

Protocol-based dependency injection for storage abstraction.
Enables mock injection for testing."
```

---

## Task 3: Implement token utilities with security hardening

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for token utilities**

```python
# tests/test_session.py (append to file)

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
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestTokenUtilities -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute '_generate_token'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after imports, before dataclasses)

import hashlib
import re
import secrets

# Security: Strict validation patterns prevent path traversal and DoS
SESSION_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{8}$")
TOKEN_PATTERN = re.compile(r"^[A-Za-z0-9_-]{8}\.[A-Za-z0-9_-]{43}$")
MAX_TOKEN_LENGTH = 60


def _generate_session_id() -> str:
    """Generate 8-char session ID using base62."""
    from legion import short_id

    return short_id.generate_short_id(length=8)


def _validate_session_id(session_id: str) -> bool:
    """Validate session ID format. Prevents path traversal."""
    return bool(SESSION_ID_PATTERN.match(session_id))


def _generate_token(session_id: str, token_bytes: int = 32) -> str:
    """Generate composite token: {session_id}.{secret}"""
    secret = secrets.token_urlsafe(token_bytes)
    return f"{session_id}.{secret}"


def _parse_token(token: str) -> tuple[str, str] | None:
    """Parse and validate token. Returns None if invalid.

    Security: Validates format and length to prevent DoS and injection.
    """
    if not isinstance(token, str):
        return None
    if len(token) > MAX_TOKEN_LENGTH:
        return None
    if not TOKEN_PATTERN.match(token):
        return None
    session_id, secret = token.split(".", 1)
    return session_id, secret


def _hash_token(token: str) -> str:
    """Hash full token for storage comparison."""
    return hashlib.sha256(token.encode()).hexdigest()


def _validate_token_hash(provided_token: str, stored_hash: str) -> bool:
    """Compare token against stored hash in constant time."""
    provided_hash = _hash_token(provided_token)
    return secrets.compare_digest(provided_hash, stored_hash)
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestTokenUtilities -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add token utilities with security hardening

- Composite token format: {session_id}.{secret}
- 256-bit entropy via secrets.token_urlsafe(32)
- SHA-256 hashing for storage
- Timing-safe comparison via secrets.compare_digest
- Path traversal prevention via regex allowlist
- DoS prevention via token length limit"
```

---

## Task 4: Implement FileSessionStorage

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for FileSessionStorage**

```python
# tests/test_session.py (append to file)

import json
from pathlib import Path


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
    async def test_load_nonexistent(
        self, storage: session.FileSessionStorage
    ) -> None:
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
        storage_dir = tmp_path / "sessions"
        session.FileSessionStorage(storage_dir)
        assert storage_dir.exists()
        # Check permissions (on POSIX systems)
        import os
        if os.name == "posix":
            mode = storage_dir.stat().st_mode & 0o777
            assert mode == 0o700
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestFileSessionStorage -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'FileSessionStorage'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after token utilities, before DEFAULT_CONFIG)

import json
import os

import aiofiles


class FileSessionStorage:
    """File-based session storage with atomic writes."""

    def __init__(self, storage_dir: Path) -> None:
        self._storage_dir = storage_dir
        # Security: Restrict directory permissions
        self._storage_dir.mkdir(parents=True, exist_ok=True)
        if os.name == "posix":
            os.chmod(self._storage_dir, 0o700)

    def _session_path(self, session_id: str) -> Path:
        # Security: Validate session_id before constructing path
        if not _validate_session_id(session_id):
            msg = f"Invalid session ID format: {session_id!r}"
            raise ValueError(msg)
        return self._storage_dir / f"{session_id}.json"

    async def save(self, session: Session) -> None:
        """Atomic write via temp file + rename."""
        path = self._session_path(session.session_id)
        temp_path = path.with_suffix(".tmp")

        data = {
            "session_id": session.session_id,
            "token_hash": session.token_hash,
            "created_at": session.created_at,
            "last_accessed_at": session.last_accessed_at,
            "expires_at": session.expires_at,
            "idle_timeout_secs": session.idle_timeout_secs,
            "metadata": session.metadata,
        }

        async with aiofiles.open(temp_path, "w") as f:
            await f.write(json.dumps(data))

        temp_path.rename(path)  # Atomic on POSIX

    async def load(self, session_id: str) -> Session | None:
        """Load session, returning None on any error.

        Performance: Uses try/except instead of path.exists() to
        eliminate separate syscall.
        """
        try:
            path = self._session_path(session_id)
        except ValueError:
            return None  # Invalid session_id format

        try:
            async with aiofiles.open(path) as f:
                data = json.loads(await f.read())
            return Session(**data)
        except FileNotFoundError:
            return None
        except (json.JSONDecodeError, TypeError, KeyError):
            return None  # Corrupted file treated as missing

    async def delete(self, session_id: str) -> bool:
        """Delete session file."""
        try:
            path = self._session_path(session_id)
        except ValueError:
            return False  # Invalid session_id format

        try:
            path.unlink()
            return True
        except FileNotFoundError:
            return False


def _get_default_storage_dir() -> Path:
    """Get default session storage directory."""
    return Path.home() / ".legion" / "sessions"


def _get_default_storage() -> FileSessionStorage:
    """Get default file storage instance."""
    return FileSessionStorage(_get_default_storage_dir())
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestFileSessionStorage -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add FileSessionStorage with atomic writes

- Atomic writes via temp file + rename
- Path traversal prevention via session ID validation
- Directory permissions restricted to 0o700
- Graceful handling of corrupted/partial files"
```

---

## Task 5: Implement create_session function

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for create_session**

```python
# tests/test_session.py (append to file)

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
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestCreateSession -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'create_session'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after _get_default_storage)

import time


async def create_session(
    *,
    config: SessionConfig = DEFAULT_CONFIG,
    storage: SessionStorage | None = None,
    metadata: dict[str, Any] | None = None,
) -> tuple[str, Session]:
    """
    Create a new session.

    Returns:
        Tuple of (raw_token, session_record)

    The raw_token should be given to the client.
    The session_record is stored server-side.
    """
    if storage is None:
        storage_dir = config.storage_dir or _get_default_storage_dir()
        storage = FileSessionStorage(storage_dir)

    session_id = _generate_session_id()
    token = _generate_token(session_id, config.token_bytes)
    token_hash = _hash_token(token)

    now = time.time()
    sess = Session(
        session_id=session_id,
        token_hash=token_hash,
        created_at=now,
        last_accessed_at=now,
        expires_at=now + config.absolute_timeout_secs,
        idle_timeout_secs=config.idle_timeout_secs,
        metadata=metadata,
    )

    await storage.save(sess)
    return token, sess
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestCreateSession -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add create_session function

- Generates cryptographically secure token
- Configurable idle and absolute timeouts
- Supports optional metadata
- Stores session via injected storage"
```

---

## Task 6: Implement validate_session function

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for validate_session**

```python
# tests/test_session.py (append to file)

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
        config = session.SessionConfig(
            storage_dir=tmp_path,
            idle_timeout_secs=0,  # Immediate expiration
            absolute_timeout_secs=3600,
        )
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        # Small delay to ensure idle timeout
        import asyncio
        await asyncio.sleep(0.01)

        result = await session.validate_session(token, storage=storage)
        assert result is None
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestValidateSession -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'validate_session'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after create_session)

async def validate_session(
    token: str,
    *,
    storage: SessionStorage | None = None,
) -> Session | None:
    """
    Validate a session token.

    Returns:
        Session if valid and not expired, None otherwise.

    Does NOT update last_accessed_at (use refresh for that).
    """
    if storage is None:
        storage = _get_default_storage()

    parsed = _parse_token(token)
    if parsed is None:
        return None

    session_id, _ = parsed
    sess = await storage.load(session_id)
    if sess is None:
        return None

    if not _validate_token_hash(token, sess.token_hash):
        return None

    now = time.time()

    # Check absolute expiration
    if now > sess.expires_at:
        await storage.delete(session_id)
        return None

    # Check idle expiration
    idle_expires_at = sess.last_accessed_at + sess.idle_timeout_secs
    if now > idle_expires_at:
        await storage.delete(session_id)
        return None

    return sess
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestValidateSession -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add validate_session function

- Validates token format and hash
- Checks both idle and absolute timeouts
- Deletes expired sessions automatically
- Timing-safe hash comparison"
```

---

## Task 7: Implement refresh_session function

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for refresh_session**

```python
# tests/test_session.py (append to file)

class TestRefreshSession:
    """Tests for refresh_session function."""

    @pytest.mark.anyio
    async def test_refresh_updates_last_accessed(self, tmp_path: Path) -> None:
        """Refresh should update last_accessed_at."""
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        original_accessed = sess.last_accessed_at

        import asyncio
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
        config = session.SessionConfig(storage_dir=tmp_path)
        storage = session.FileSessionStorage(tmp_path)
        token, sess = await session.create_session(config=config, storage=storage)

        import asyncio
        await asyncio.sleep(0.05)

        await session.refresh_session(token, storage=storage)

        # Load directly from storage to verify persistence
        loaded = await storage.load(sess.session_id)
        assert loaded is not None
        assert loaded.last_accessed_at > sess.last_accessed_at
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestRefreshSession -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'refresh_session'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after validate_session)

from dataclasses import replace


async def refresh_session(
    token: str,
    *,
    storage: SessionStorage | None = None,
) -> Session | None:
    """
    Refresh a session, extending its idle timeout.

    Returns:
        Updated Session if valid, None if invalid/expired.

    Updates last_accessed_at. Does NOT extend absolute timeout.
    """
    if storage is None:
        storage = _get_default_storage()

    sess = await validate_session(token, storage=storage)
    if sess is None:
        return None

    now = time.time()

    # Create updated session with new last_accessed_at
    updated = replace(sess, last_accessed_at=now)

    await storage.save(updated)
    return updated
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestRefreshSession -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add refresh_session function

- Updates last_accessed_at to extend idle timeout
- Preserves absolute expiration (no extension)
- Uses dataclasses.replace for immutable update"
```

---

## Task 8: Implement revoke_session function

**Files:**
- Modify: `src/legion/session.py`
- Test: `tests/test_session.py`

**Step 1: Write failing tests for revoke_session**

```python
# tests/test_session.py (append to file)

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
```

**Step 2: Run test to verify it fails**

Run: `uv run pytest tests/test_session.py::TestRevokeSession -v`
Expected: FAIL with "AttributeError: module 'legion.session' has no attribute 'revoke_session'"

**Step 3: Write minimal implementation**

```python
# src/legion/session.py (add after refresh_session)

async def revoke_session(
    token: str,
    *,
    storage: SessionStorage | None = None,
) -> bool:
    """
    Revoke a session immediately.

    Returns:
        True if session was revoked, False if not found.

    Idempotent: revoking non-existent session returns False.
    """
    if storage is None:
        storage = _get_default_storage()

    parsed = _parse_token(token)
    if parsed is None:
        return False

    session_id, _ = parsed
    return await storage.delete(session_id)
```

**Step 4: Run test to verify it passes**

Run: `uv run pytest tests/test_session.py::TestRevokeSession -v`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat(session): add revoke_session function

- Immediately invalidates session
- Idempotent operation (safe to call multiple times)
- Returns True if revoked, False if not found"
```

---

## Task 9: Run full test suite and verify

**Step 1: Run all session tests**

Run: `uv run pytest tests/test_session.py -v`
Expected: All tests pass

**Step 2: Run full project test suite**

Run: `uv run pytest -v`
Expected: All tests pass (including existing tests)

**Step 3: Final commit**

```bash
jj describe -m "feat: add session management infrastructure (LEG-12)

Complete implementation of session management for Legion:
- Session creation with cryptographically secure tokens
- Session validation with dual timeouts (idle + absolute)
- Session refresh (extends idle, not absolute)
- Session revocation

Security features:
- 256-bit entropy tokens via secrets.token_urlsafe
- SHA-256 hashing (never store raw tokens)
- Timing-safe comparison via secrets.compare_digest
- Path traversal prevention via regex allowlist
- File permissions restricted to 0o700

Architecture:
- Module-level async functions (Legion pattern)
- Protocol-based DI for storage abstraction
- Frozen dataclasses for immutability
- Atomic file writes via temp + rename"
```

---

## References

### Internal References
- Existing patterns: `src/legion/state/types.py:1` (dataclass patterns)
- Short ID generation: `src/legion/short_id.py:1` (base62 encoding)
- Async patterns: `src/legion/state/fetch.py:1` (Protocol injection, aiofiles)
- Testing patterns: `tests/test_state.py:1` (pytest-anyio, mock runners)

### External References
- [Python secrets documentation](https://docs.python.org/3/library/secrets.html)
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
- [aiofiles documentation](https://github.com/tinche/aiofiles)
