package main

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

// A write's marker is "lgm<nonce><counter>x": the run's nonce, markerNonceDigits decimal digits
// with leading zeros kept, then the write's counter in the run (1, 2, ...). The nonce is random
// per run, so a run reads back only its own commit rows even when an earlier run on the same
// database wrote the same counters. commit-trigger.sql extracts the first match of markerPattern
// from each new event's payload.
const markerNonceDigits = 9

// markerPattern matches any run's marker; parseMarker tells the runs apart.
var markerPattern = regexp.MustCompile(`lgm[0-9]+x`)

// newMarkerNonce draws a run's nonce, below 10^markerNonceDigits.
func newMarkerNonce() int64 { return rand.Int64N(1_000_000_000) }

// formatNonce is the nonce as its marker digits.
func formatNonce(nonce int64) string { return fmt.Sprintf("%0*d", markerNonceDigits, nonce) }

func markerTag(nonce, counter int64) string {
	return "lgm" + formatNonce(nonce) + strconv.FormatInt(counter, 10) + "x"
}

// parseMarker reads the counter of marker s, false when s is not a marker of the run with this
// nonce.
func parseMarker(s string, nonce int64) (int64, bool) {
	digits, ok := strings.CutPrefix(s, "lgm")
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, "x")
	if !ok || len(digits) <= markerNonceDigits {
		return 0, false
	}
	if digits[:markerNonceDigits] != formatNonce(nonce) {
		return 0, false
	}
	counter := digits[markerNonceDigits:]
	if counter[0] < '1' || counter[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseInt(counter, 10, 64)
	return n, err == nil
}

// findMarker reads the counter of the first marker of the run with this nonce in b; another
// run's marker is skipped.
func findMarker(b []byte, nonce int64) (int64, bool) {
	for _, m := range markerPattern.FindAll(b, -1) {
		if n, ok := parseMarker(string(m), nonce); ok {
			return n, true
		}
	}
	return 0, false
}
