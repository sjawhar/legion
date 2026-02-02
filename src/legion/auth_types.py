"""Authentication types for Legion."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum


class AuthStatus(Enum):
    """Authentication result status codes."""

    SUCCESS = "success"
    INVALID_CREDENTIALS = "invalid_credentials"
    RATE_LIMITED = "rate_limited"
    VALIDATION_ERROR = "validation_error"


@dataclass(frozen=True)
class AuthResult:
    """Result of an authentication attempt."""

    status: AuthStatus
    session_token: str | None = None
    error_message: str | None = None
    retry_after_secs: int | None = None
