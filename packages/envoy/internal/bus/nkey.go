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

// HoldsSeed reports whether text holds an nkey seed of any type nkeys knows, operator, server,
// cluster, account, user or curve, as nkeys.DecodeSeed decides, in any spelling a reader or a
// decoder takes for it. It reads text as runs of base32 letters in either case, in which '.', '-',
// '_' and whitespace continue the run and are dropped from it: so a seed split across DNS labels or
// grouped by a separator is found, and so is one wrapped across lines, which nkeys itself reads.
// It slides a seed-length window across every run, so a seed inside a longer value or a longer run
// is found, and a padded one too. It reads text once more percent-decoded, and again for each level
// at which a '%' was itself encoded. A window counts only if nkeys decodes it as a seed: a seed's
// prefix byte, one of the six types and a matching CRC-16 checksum, which random base32 has about
// one chance in 10^8 of meeting per window, so a value that holds no seed is not taken for one.
//
// It is a pure function: no state, no logging, and no allocation but nkeys.DecodeSeed's for the
// rare window with a seed's two leading letters, and a decoded copy of a text holding an encoded
// '%'. It answers only true or false and never returns or formats text, so a caller refuses
// naming where the text came from, never the text.
func HoldsSeed(text string) bool {
	if seedInRuns(text, false) {
		return true
	}
	if strings.IndexByte(text, '%') < 0 {
		return false
	}
	if seedInRuns(text, true) {
		return true
	}
	// A '%' that was itself encoded: decode one level and look again. Each level is shorter than
	// the one before, so this ends.
	if strings.Contains(text, "%25") {
		return HoldsSeed(percentDecoded(text))
	}
	return false
}

// seedInRuns reports whether a window of seedTextLength letters of a base32 run of text, read as
// HoldsSeed says and percent-decoded when decode is set, is an nkey seed.
func seedInRuns(text string, decode bool) bool {
	var run [seedTextLength]byte // the run's last seedTextLength letters, as a ring
	n := 0                       // letters in the current run
	found := false
	for i := 0; i < len(text) && !found; i++ {
		c := text[i]
		if decode && c == '%' && i+2 < len(text) {
			if v, ok := unhex(text[i+1], text[i+2]); ok {
				c = v
				i += 2
			}
		}
		switch {
		case 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
			fallthrough
		case 'A' <= c && c <= 'Z', '2' <= c && c <= '7':
			run[n%seedTextLength] = c
			n++
			found = n >= seedTextLength && windowIsSeed(&run, n)
		case c == '.', c == '-', c == '_', c == ' ', c == '\t', c == '\r', c == '\n':
			// The run continues across it, and it is not one of the run's letters.
		default:
			n = 0
		}
	}
	clear(run[:])
	return found
}

// windowIsSeed reports whether the last seedTextLength letters of a run, held in the ring run after
// n letters, spell an nkey seed. Every seed's first two letters are 'S' and the first letter of its
// type, 'O', 'N', 'C', 'A', 'U' or 'X' (nkeys v0.4.15 strkey.go:26-48), so nearly every window is
// settled before nkeys' decoder.
func windowIsSeed(run *[seedTextLength]byte, n int) bool {
	start := n % seedTextLength
	if run[start] != 'S' || strings.IndexByte("ONCAUX", run[(start+1)%seedTextLength]) < 0 {
		return false
	}
	var window [seedTextLength]byte
	for j := range window {
		window[j] = run[(start+j)%seedTextLength]
	}
	_, raw, err := nkeys.DecodeSeed(window[:])
	clear(window[:])
	clear(raw)
	return err == nil
}

// percentDecoded decodes each %XX in text, keeping every other byte, a % that encodes nothing
// included.
func percentDecoded(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if v, ok := unhex(text[i+1], text[i+2]); ok {
				b.WriteByte(v)
				i += 2
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return b.String()
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
