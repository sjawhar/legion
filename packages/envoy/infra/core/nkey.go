package core

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
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

// SeedRefusal is the refusal of a value that holds an nkey seed of any type (bus.HoldsSeed): it
// names field, the value's path, as "users[0]: name" or "natsUrls[1]", and never the value. A layer
// that takes a value no seed belongs in refuses one with it, before the value reaches a message, a
// log or rendered output.
func SeedRefusal(field string) error {
	return errors.New(field + " holds an nkey seed (not echoed)")
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
