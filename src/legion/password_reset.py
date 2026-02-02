"""Password reset token operations."""

import hashlib
import secrets
from dataclasses import dataclass, field, replace
from datetime import datetime, timedelta, timezone


def generate_token() -> str:
    """Generate a cryptographically secure URL-safe token.

    Returns 32 bytes (256 bits) of entropy, URL-safe base64 encoded.
    """
    return secrets.token_urlsafe(32)


def hash_token(token: str) -> str:
    """Hash a token using SHA-256 for secure storage.

    Tokens should be stored hashed, like passwords.
    """
    return hashlib.sha256(token.encode()).hexdigest()


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


def is_expired(token: ResetToken) -> bool:
    """Check if a token has expired.

    Returns True if current time is at or past the expiry time.
    """
    return datetime.now(timezone.utc) >= token.expires_at


def is_used(token: ResetToken) -> bool:
    """Check if a token has already been used.

    Tokens are single-use and cannot be reused.
    """
    return token.used


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


def mark_used(token: ResetToken) -> ResetToken:
    """Return a new token marked as used.

    Since ResetToken is immutable, this returns a copy with used=True.
    """
    return replace(token, used=True)


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
