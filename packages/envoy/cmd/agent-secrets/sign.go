// packages/envoy/cmd/agent-secrets/sign.go
//
// cmdSign implements "agent-secrets sign --method M --url U [--enrollment E]": it prints the
// compact JWS this process would put in the Proof header of that one call, without making the
// call. It exists for proofs and doctors, never for tools — a proof is single-use (jti) and
// valid for 60 s (internal/broker/proof) — and, with --enrollment, for the deliberate
// cross-enrollment substitution the broker must refuse: signing with this box's own key for a
// different enrollment id.
package main

import (
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

func cmdSign(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"method": true, "url": true, "enrollment": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets sign: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := flag.NewFlagSet("agent-secrets sign", flag.ContinueOnError)
	flags.SetOutput(stderr)
	method := flags.String("method", "", "HTTP method of the call")
	url := flags.String("url", "", "absolute URL of the call")
	enrollment := flags.String("enrollment", "", "sign for this enrollment id instead of this process's own (file mode only)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if *method == "" || *url == "" {
		fmt.Fprintln(stderr, "agent-secrets sign: --method and --url are required")
		return exitUsageError
	}

	if *enrollment != "" {
		// --enrollment's whole point is to override this box's own enrollment id, so this reads
		// only the key file directly rather than going through buildSigner: that helper also
		// requires a valid enrollment file, which would be both pointless (about to be
		// overridden) and, on a box mid-enrollment with no enrollment file yet, a spurious
		// failure for exactly the substitution this command exists to test.
		key, err := fileModeKeyForSign()
		if err != nil {
			fmt.Fprintln(stderr, "agent-secrets sign:", err)
			return exitUsageError
		}
		compact, err := proof.Sign(key, *enrollment, *method, *url, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets sign: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, compact)
		return 0
	}

	_, signer, err := buildSigner()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets sign: %v\n", err)
		return exitUsageError
	}
	compact, err := signer.Sign(*method, *url)
	if err != nil {
		reportError(stderr, "agent-secrets sign", err)
		return 1
	}
	fmt.Fprintln(stdout, compact)
	return 0
}

// fileModeKeyForSign loads the box's own key.pem for --enrollment's cross-enrollment
// substitution.
func fileModeKeyForSign() (*ecdsa.PrivateKey, error) {
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		return nil, errors.New("--enrollment works only with a key dir; the helper signs only its own enrollment")
	}
	path := filepath.Join(dir, "key.pem")
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("--enrollment works only with a key dir; the helper signs only its own enrollment")
	}
	return loadKey(path)
}
