"""Tests for short_id module."""

from legion import short_id


class TestBase62:
    def test_encode_zero(self) -> None:
        assert short_id.encode_base62(0) == "0"

    def test_encode_single_digit(self) -> None:
        assert short_id.encode_base62(9) == "9"
        assert short_id.encode_base62(10) == "A"
        assert short_id.encode_base62(35) == "Z"
        assert short_id.encode_base62(36) == "a"
        assert short_id.encode_base62(61) == "z"

    def test_encode_multi_digit(self) -> None:
        assert short_id.encode_base62(62) == "10"
        assert short_id.encode_base62(124) == "20"

    def test_decode_single_digit(self) -> None:
        assert short_id.decode_base62("0") == 0
        assert short_id.decode_base62("9") == 9
        assert short_id.decode_base62("A") == 10
        assert short_id.decode_base62("Z") == 35
        assert short_id.decode_base62("a") == 36
        assert short_id.decode_base62("z") == 61

    def test_decode_multi_digit(self) -> None:
        assert short_id.decode_base62("10") == 62
        assert short_id.decode_base62("20") == 124

    def test_roundtrip(self) -> None:
        for n in [0, 1, 61, 62, 1000, 1000000, 2**64]:
            assert short_id.decode_base62(short_id.encode_base62(n)) == n


class TestUuidToShort:
    def test_uuid_with_hyphens(self) -> None:
        uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        short = short_id.uuid_to_short(uuid)
        assert len(short) == 8
        assert short.isalnum()

    def test_uuid_without_hyphens(self) -> None:
        uuid = "7b4f0862b7754cb09a6785400c6f44a8"
        short = short_id.uuid_to_short(uuid)
        assert len(short) == 8
        assert short.isalnum()

    def test_same_uuid_same_short(self) -> None:
        uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        uuid2 = "7b4f0862b7754cb09a6785400c6f44a8"
        assert short_id.uuid_to_short(uuid1) == short_id.uuid_to_short(uuid2)

    def test_different_uuids_different_shorts(self) -> None:
        uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        uuid2 = "00000000-0000-0000-0000-000000000000"
        assert short_id.uuid_to_short(uuid1) != short_id.uuid_to_short(uuid2)

    def test_custom_length(self) -> None:
        uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8"
        assert len(short_id.uuid_to_short(uuid, length=4)) == 4
        assert len(short_id.uuid_to_short(uuid, length=12)) == 12
