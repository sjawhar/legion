package record

import "errors"

// ErrBodyIDMismatch is VerifyBodyReproducesID's error when a stored body parses cleanly but its
// own content-addressed id does not equal the id it was stored under — the one failure ParseBody
// itself cannot detect, since ParseBody has no id to compare against.
var ErrBodyIDMismatch = errors.New("stored body does not reproduce its own id")

// VerifyBodyReproducesID parses canonical and confirms it reproduces recordID's own hash. Every
// caller holding a record id and its stored body shares this exact "parse, then compare id" check
// — ChainVerifier.Verify, requests.Machine's pre-decision check, and machine.Service's own — rather
// than each re-implementing record.ParseBody plus a hand-rolled ID comparison.
func VerifyBodyReproducesID(canonical, recordID string) (Body, error) {
	body, err := ParseBody(canonical)
	if err != nil {
		return Body{}, err
	}
	if body.ID() != recordID {
		return Body{}, ErrBodyIDMismatch
	}
	return body, nil
}
