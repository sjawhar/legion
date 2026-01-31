"""Tests for short_id module."""

from legion.short_id import encode_base62, decode_base62, uuid_to_short


class TestBase62:
    def test_encode_zero(self) -> None:
        assert encode_base62(0) == "0"

    def test_encode_single_digit(self) -> None:
        assert encode_base62(9) == "9"
        assert encode_base62(10) == "A"
        assert encode_base62(35) == "Z"
        assert encode_base62(36) == "a"
        assert encode_base62(61) == "z"

    def test_encode_multi_digit(self) -> None:
        assert encode_base62(62) == "10"
        assert encode_base62(124) == "20"

    def test_decode_single_digit(self) -> None:
        assert decode_base62("0") == 0
        assert decode_base62("9") == 9
        assert decode_base62("A") == 10
        assert decode_base62("Z") == 35
        assert decode_base62("a") == 36
        assert decode_base62("z") == 61

    def test_decode_multi_digit(self) -> None:
        assert decode_base62("10") == 62
        assert decode_base62("20") == 124

    def test_roundtrip(self) -> None:
        for n in [0, 1, 61, 62, 1000, 1000000, 2**64]:
            assert decode_base62(encode_base62(n)) == n


class TestUuidToShort:
    def test_uuid_with_hyphens(self) -> None:
        uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        short = uuid_to_short(uuid)
        assert len(short) == 8
        assert short.isalnum()

    def test_uuid_without_hyphens(self) -> None:
        uuid = "7b4f0862b7754cb09a6785400c6f44a8"
        short = uuid_to_short(uuid)
        assert len(short) == 8
        assert short.isalnum()

    def test_same_uuid_same_short(self) -> None:
        uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        uuid2 = "7b4f0862b7754cb09a6785400c6f44a8"
        assert uuid_to_short(uuid1) == uuid_to_short(uuid2)

    def test_different_uuids_different_shorts(self) -> None:
        uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        uuid2 = "00000000-0000-0000-0000-000000000000"
        assert uuid_to_short(uuid1) != uuid_to_short(uuid2)

    def test_custom_length(self) -> None:
        uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        assert len(uuid_to_short(uuid, length=4)) == 4
        assert len(uuid_to_short(uuid, length=12)) == 12
