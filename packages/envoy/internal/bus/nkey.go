package bus

import (
	"fmt"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// The two variables a process names its NATS user's nkey seed with: a file holding the seed, or
// the seed itself. NkeySeedEnvVar is exported for what writes a process's environment.
const (
	nkeySeedFileVariable = "NATS_NKEY_SEED_FILE"
	NkeySeedEnvVar       = "NATS_NKEY_SEED"
)

// nkeyCredential is the NATS user this process connects as, from its environment: the trimmed
// contents of the file NATS_NKEY_SEED_FILE names, else NATS_NKEY_SEED. A set variable is
// authoritative, and the file pointer wins over the seed: an empty pointer, or a missing,
// unreadable or blank file, is an error naming the variable and the path, never a fallback to
// NATS_NKEY_SEED or to no credential (Legion's X_FILE rule, the Legion daemon's
// config.ReadSecretPointer); so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey
// seed. Neither set is a nil option: the connection carries no credential, as every connection did
// before servers required one. No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func nkeyCredential(lookup func(string) (string, bool)) (nats.Option, error) {
	var seed, source string
	if file, set := lookup(nkeySeedFileVariable); set {
		if file == "" {
			return nil, fmt.Errorf("%s is set but empty", nkeySeedFileVariable)
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s names %s, which could not be read: %w", nkeySeedFileVariable, file, err)
		}
		seed = strings.TrimSpace(string(contents))
		if seed == "" {
			return nil, fmt.Errorf("%s names %s, which is empty", nkeySeedFileVariable, file)
		}
		source = fmt.Sprintf("%s (%s)", nkeySeedFileVariable, file)
	} else if value, set := lookup(NkeySeedEnvVar); set {
		seed = strings.TrimSpace(value)
		if seed == "" {
			return nil, fmt.Errorf("%s is set but empty", NkeySeedEnvVar)
		}
		source = NkeySeedEnvVar
	} else {
		return nil, nil
	}
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	public, err := user.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a valid nkey seed: %w", source, err)
	}
	if !nkeys.IsValidPublicUserKey(public) {
		return nil, fmt.Errorf("%s holds an nkey seed that is not a user's (public key %s)", source, public)
	}
	return nats.Nkey(public, user.Sign), nil
}

// seedTextLength is an nkey seed's length as nkeys spells it: two prefix bytes, the 32-byte
// ed25519 seed and a CRC-16 checksum, 36 bytes, as unpadded base32.
const seedTextLength = 58

// maxPercentDecodes is how many times HoldsSeed percent-decodes a text: a seed encoded once,
// twice or three times is found, as a URL nested in a URL or a configuration value encodes it, and
// one encoded more often is not.
const maxPercentDecodes = 3

// HoldsSeed reports whether text holds an nkey seed of any type nkeys knows, operator, server,
// cluster, account, user or curve, as nkeys.DecodeSeed decides, in any spelling a reader or a
// decoder takes for it. It reads text as runs of base32 letters in either case, in which '.', '-',
// '_' and whitespace continue the run and are dropped from it: so a seed split across DNS labels or
// grouped by a separator is found, and so is one wrapped across lines, which nkeys itself reads.
// It slides a seed-length window across every run, so a seed inside a longer value or a longer run
// is found, and a padded one too. It reads text again percent-decoded, up to maxPercentDecodes
// times, stopping at a decoding that changes nothing. A window counts only if nkeys decodes it as a
// seed: a seed's prefix byte, one of the six types and a matching CRC-16 checksum, which random
// base32 has about one chance in 10^8 of meeting per window, so a value that holds no seed is not
// taken for one.
//
// Its cost is linear in len(text): at most maxPercentDecodes+1 scans and maxPercentDecodes decoded
// copies, each scan doing constant work per byte and, at most once every three letters (a window
// must start 'S', a type letter, then 'A' to 'D'), a checksum over the window's 58 letters. Every
// scan reads the whole text, so a value whose seed-like windows each decoding rescans costs four
// scans' worth of checksums.
//
// It is a pure function: no state, no logging, and no allocation but one decoded copy per decoding
// of a text that percent-encodes something, and nkeys' own decode of a window that is a seed. It
// answers only true or false and never returns or formats text, so a caller refuses naming where
// the text came from, never the text.
func HoldsSeed(text string) bool {
	for decodes := 0; ; decodes++ {
		if seedInRuns(text) {
			return true
		}
		if decodes == maxPercentDecodes {
			return false
		}
		decoded, changed := percentDecoded(text)
		if !changed {
			return false
		}
		text = decoded
	}
}

// seedInRuns reports whether a window of seedTextLength letters of a base32 run of text, read as
// HoldsSeed says, is an nkey seed.
func seedInRuns(text string) bool {
	// The run's letters, uppercase, the last seedTextLength of them ending at n. When it fills,
	// the last seedTextLength-1 move to its start, so each letter is copied at most once more.
	var run [2 * seedTextLength]byte
	n := 0
	found := false
	for i := 0; i < len(text) && !found; i++ {
		c := text[i]
		switch {
		case 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
			fallthrough
		case 'A' <= c && c <= 'Z', '2' <= c && c <= '7':
			if n == len(run) {
				n = copy(run[:], run[n-seedTextLength+1:])
			}
			run[n] = c
			n++
			found = n >= seedTextLength && run[n-seedTextLength] == 'S' &&
				windowIsSeed(run[n-seedTextLength:n])
		case c == '.', c == '-', c == '_', c == ' ', c == '\t', c == '\r', c == '\n':
			// The run continues across it, and it is not one of the run's letters.
		default:
			n = 0
		}
	}
	clear(run[:])
	return found
}

// windowIsSeed reports whether window, seedTextLength uppercase base32 letters, spells an nkey
// seed, as nkeys.DecodeSeed decides. Two necessary conditions come first, so nkeys decodes only a
// window that is a seed. Its first three letters: nkeys reads a seed's first byte's top five bits
// as the seed prefix ('S'), its next five as the type ('O', 'N', 'C', 'A', 'U' or 'X'), and
// requires the three after them to be zero, since every type's prefix byte is a multiple of eight
// (nkeys v0.4.15 strkey.go:26-48, 172-188), so the third letter is 'A' to 'D'. Then its checksum,
// computed here from the letters themselves, without decoding the window or allocating.
func windowIsSeed(window []byte) bool {
	if window[0] != 'S' || strings.IndexByte("ONCAUX", window[1]) < 0 ||
		window[2] < 'A' || window[2] > 'D' {
		return false
	}
	// nkeys appends to a key's bytes their CRC-16/XMODEM, little-endian (nkeys v0.4.15
	// strkey.go:132-152). The CRC reads its input bit by bit, most significant first, as base32
	// spells it, so it takes five bits a letter: 54 letters and the top two bits of the 55th are the
	// 34 checked bytes, the next 16 bits the checksum, and the last two bits are unused. table is
	// the CRC of each five-bit value, the first 32 entries of nkeys' crc16tab (crc16.go:18-22).
	table := [32]uint16{
		0x0000, 0x1021, 0x2042, 0x3063, 0x4084, 0x50a5, 0x60c6, 0x70e7,
		0x8108, 0x9129, 0xa14a, 0xb16b, 0xc18c, 0xd1ad, 0xe1ce, 0xf1ef,
		0x1231, 0x0210, 0x3273, 0x2252, 0x52b5, 0x4294, 0x72f7, 0x62d6,
		0x9339, 0x8318, 0xb37b, 0xa35a, 0xd3bd, 0xc39c, 0xf3ff, 0xe3de,
	}
	var crc uint16
	for _, c := range window[:54] {
		crc = crc<<5 ^ table[crc>>11^uint16(base32Value(c))]
	}
	last := base32Value(window[54])
	for bit := 4; bit >= 3; bit-- {
		crc = crc<<1 ^ 0x1021*(crc>>15^uint16(last>>bit&1))
	}
	stored := uint16(last&7)<<13 | uint16(base32Value(window[55]))<<8 |
		uint16(base32Value(window[56]))<<3 | uint16(base32Value(window[57]))>>2
	if crc != stored>>8|stored<<8 {
		return false
	}
	_, seed, err := nkeys.DecodeSeed(window)
	clear(seed)
	return err == nil
}

// base32Value is the five-bit value of an uppercase base32 letter.
func base32Value(c byte) byte {
	if c < 'A' {
		return c - '2' + 26
	}
	return c - 'A'
}

// percentDecoded decodes each %XX in text, keeping every other byte, a % that encodes nothing
// included, and reports whether it decoded any. It copies text only when it does.
func percentDecoded(text string) (string, bool) {
	first := -1
	for i := 0; i+2 < len(text); i++ {
		if _, ok := unhex(text[i+1], text[i+2]); ok && text[i] == '%' {
			first = i
			break
		}
	}
	if first < 0 {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	b.WriteString(text[:first])
	for i := first; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if v, ok := unhex(text[i+1], text[i+2]); ok {
				b.WriteByte(v)
				i += 2
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return b.String(), true
}

// unhex is the byte two hex digits spell, and whether they are hex digits.
func unhex(hi, lo byte) (byte, bool) {
	h, okH := hexDigit(hi)
	l, okL := hexDigit(lo)
	return h<<4 | l, okH && okL
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
