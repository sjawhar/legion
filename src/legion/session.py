"""Session management infrastructure for Legion."""

from __future__ import annotations

import hashlib
import re
import secrets
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Protocol

# Type alias for JSON-serializable session metadata values
# More specific than Any, captures the actual constraint (must be JSON-serializable)
JsonValue = str | int | float | bool | None | list["JsonValue"] | dict[str, "JsonValue"]
SessionMetadata = dict[str, JsonValue]

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


@dataclass(frozen=True)
class Session:
    """Immutable session record."""

    session_id: str
    token_hash: str
    created_at: float
    last_accessed_at: float
    expires_at: float
    idle_timeout_secs: int
    metadata: SessionMetadata | None = None


@dataclass(frozen=True)
class SessionConfig:
    """Session configuration with sensible defaults."""

    idle_timeout_secs: int = 1800  # 30 minutes
    absolute_timeout_secs: int = 28800  # 8 hours
    token_bytes: int = 32  # 256-bit entropy
    storage_dir: Path | None = None


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


DEFAULT_CONFIG = SessionConfig()


class FileSessionStorage:
    """File-based session storage with atomic writes."""

    def __init__(self, storage_dir: Path) -> None:
        import os

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
        import json

        import aiofiles

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
        import json

        import aiofiles

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


async def create_session(
    *,
    config: SessionConfig = DEFAULT_CONFIG,
    storage: SessionStorage | None = None,
    metadata: SessionMetadata | None = None,
) -> tuple[str, Session]:
    """
    Create a new session.

    Returns:
        Tuple of (raw_token, session_record)

    The raw_token should be given to the client.
    The session_record is stored server-side.
    """
    import time

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
    import time

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
    import time

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
