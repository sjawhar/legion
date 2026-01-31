"""Short ID generation using base62 encoding."""

import uuid

# Base62 alphabet (0-9, A-Z, a-z)
ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
BASE = len(ALPHABET)


def encode_base62(num: int) -> str:
    """Encode an integer to base62."""
    if num == 0:
        return ALPHABET[0]

    result = []
    while num:
        num, rem = divmod(num, BASE)
        result.append(ALPHABET[rem])
    return "".join(reversed(result))


def decode_base62(s: str) -> int:
    """Decode a base62 string to an integer."""
    num = 0
    for char in s:
        num = num * BASE + ALPHABET.index(char)
    return num


def uuid_to_short(uuid_str: str, length: int = 8) -> str:
    """Convert a UUID string to a short base62 ID.

    Takes first `length` characters of the base62 encoding.
    8 chars of base62 = ~47 bits of entropy (62^8 ≈ 2^47).
    """
    # Remove hyphens and convert to int
    clean = uuid_str.replace("-", "")
    num = int(clean, 16)
    encoded = encode_base62(num)
    return encoded[:length]


def generate_short_id(length: int = 8) -> str:
    """Generate a new short ID."""
    return uuid_to_short(str(uuid.uuid4()), length)
