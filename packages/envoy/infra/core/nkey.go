package core

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nats-io/nkeys"
)

// An nkey's text is RFC 4648 base32, unpadded, of its prefix, its ed25519 bytes and a
// CRC-16/XMODEM checksum, little-endian. IsUserPublicKey and UserSeedPublicKey take a value only
// as nkeys writes it. nkeys' own decoder reads more: it skips line breaks, takes a key of any
// length and masks a public key's prefix byte.

const (
	// publicKeyLength is a user public key's decoded length: the prefix byte, the 32 bytes of the
	// ed25519 public key and the checksum. Its 56 characters hold exactly these 35 bytes, so a key
	// has one spelling.
	publicKeyLength = 1 + 32 + 2
	// seedLength is a user seed's decoded length: two prefix bytes, the 32 bytes of the ed25519
	// seed and the checksum.
	seedLength = 2 + 32 + 2
)

// IsUserPublicKey reports whether key is a user public key: 56 base32 characters whose 35 bytes are
// the user prefix byte, the key and a valid checksum.
func IsUserPublicKey(key string) bool {
	raw, ok := decodeBase32([]byte(key))
	return ok && len(raw) == publicKeyLength && raw[0] == byte(nkeys.PrefixByteUser) && checksumMatches(raw)
}

// UserSeedPublicKey returns the public key of value, which must be a user seed spelled exactly as
// nkeys writes one. It decides why a value is none in this order: not base32, not 36 bytes once
// decoded, a checksum that does not match, not a seed's prefix byte, a seed of another type than a
// user's, and a spelling nkeys would not write for its bytes. Its errors never quote value.
func UserSeedPublicKey(value []byte) (string, error) {
	raw, ok := decodeBase32(value)
	if !ok {
		return "", errors.New("the value is not base32")
	}
	defer clear(raw)
	if len(raw) != seedLength {
		return "", fmt.Errorf("the value decodes to %d bytes, not the 36 of a seed", len(raw))
	}
	if !checksumMatches(raw) {
		return "", errors.New("the value's checksum does not match")
	}
	if nkeys.PrefixByte(raw[0]&0xF8) != nkeys.PrefixByteSeed {
		return "", errors.New("the value is not a seed (wrong prefix byte)")
	}
	if nkeys.PrefixByte((raw[0]&0x07)<<5|(raw[1]&0xF8)>>3) != nkeys.PrefixByteUser {
		return "", errors.New("the value is a seed, but not a user's")
	}
	private := raw[2 : seedLength-2]
	canonical, err := nkeys.EncodeSeed(nkeys.PrefixByteUser, private)
	if err != nil {
		return "", err
	}
	defer clear(canonical)
	// The last character carries two bits no byte uses, so two spellings decode alike; only the
	// one nkeys writes is taken, which makes every one-character change a refusal.
	if !bytes.Equal(canonical, value) {
		return "", errors.New("the value is not the canonical encoding of its seed")
	}
	user, err := nkeys.FromRawSeed(nkeys.PrefixByteUser, private)
	if err != nil {
		return "", err
	}
	defer user.Wipe()
	return user.PublicKey()
}

// HoldsSeed reports whether text holds a user seed UserSeedPublicKey accepts, anywhere in it:
// whole, inside a longer value (a file path, an error message, any part of a URL, a longer run of
// base32 letters), or percent-encoded. Every run of seedTextLength base32 letters is tried, in the
// text and, when it percent-encodes anything, in the text decoded. It answers only true or false:
// it never returns, formats or logs the value, so a caller can refuse naming where the text came
// from, and never the text.
func HoldsSeed(text string) bool {
	return base32Seed(text) || strings.Contains(text, "%") && base32Seed(percentDecoded(text))
}

// SeedRefusal is the refusal of a value that holds an nkey user seed (HoldsSeed): it names field,
// the value's path, as "users[0]: name" or "natsUrls[1]", and never the value. A layer that takes a
// value no seed belongs in refuses one with it, before the value reaches a message, a log or
// rendered output.
func SeedRefusal(field string) error {
	return errors.New(field + " holds an nkey user seed (not echoed)")
}

// seedTextLength is a user seed's length as nkeys spells it: its seedLength bytes, unpadded base32.
const seedTextLength = 58

// base32Seed reports whether any seedTextLength run of base32 letters in text is a user seed.
func base32Seed(text string) bool {
	run := 0
	for i := range len(text) {
		if c := text[i]; 'A' <= c && c <= 'Z' || '2' <= c && c <= '7' {
			run++
			if run >= seedTextLength {
				if _, err := UserSeedPublicKey([]byte(text[i+1-seedTextLength : i+1])); err == nil {
					return true
				}
			}
			continue
		}
		run = 0
	}
	return false
}

// percentDecoded decodes each %XX in text, keeping every other byte, a % that encodes nothing
// included.
func percentDecoded(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if v, err := strconv.ParseUint(text[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// decodeBase32 decodes value as RFC 4648 base32 after padding it to a multiple of eight
// characters, and reports false for anything else: a byte outside the alphabet and '=' (a line
// break, which Go's decoder alone would skip, or a lowercase letter), or padding out of place.
func decodeBase32(value []byte) ([]byte, bool) {
	for _, b := range value {
		if b != '=' && strings.IndexByte(base32Alphabet, b) < 0 {
			return nil, false
		}
	}
	padded := make([]byte, len(value), len(value)+7)
	copy(padded, value)
	for len(padded)%8 != 0 {
		padded = append(padded, '=')
	}
	raw := make([]byte, base32.StdEncoding.DecodedLen(len(padded)))
	n, err := base32.StdEncoding.Decode(raw, padded)
	clear(padded)
	if err != nil {
		clear(raw)
		return nil, false
	}
	return raw[:n], true
}

// checksumMatches reports whether raw's last two bytes are the checksum of the bytes before them.
// Its callers have checked raw's length.
func checksumMatches(raw []byte) bool {
	n := len(raw) - 2
	return crc16(raw[:n]) == binary.LittleEndian.Uint16(raw[n:])
}

// crc16 is CRC-16/XMODEM, the checksum nkeys appends to a key.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
