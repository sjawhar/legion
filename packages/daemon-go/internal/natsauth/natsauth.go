// Package natsauth resolves the NATS nkey user a Legion process connects to Envoy's NATS as, and
// connects as it.
package natsauth

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// SeedFileKey is the configuration key naming the seed's file, in `legion.yaml` and the operator's
// controller configuration alike. SeedFileVariable and SeedVariable are the two variables a process
// names it with otherwise: a file holding the seed, or the seed itself. The Envoy clients read the
// same two (packages/envoy/internal/bus/nkey.go), and SeedVariable is also the name of the launch
// secret every pane receives the seed as, behind a SeedFileVariable pointer.
const (
	SeedFileKey      = "nats_nkey_seed_file"
	SeedFileVariable = "NATS_NKEY_SEED_FILE"
	SeedVariable     = "NATS_NKEY_SEED"
)

// DaemonSeedFileKey, DaemonSeedFileVariable and DaemonSeedVariable name the seed the daemon's own
// connection authenticates with, the same three ways: a `legion.yaml` key, a variable naming a file,
// and a variable holding the seed. No pane is ever handed it; unset, the daemon connects as the pane
// seed.
const (
	DaemonSeedFileKey      = "nats_daemon_nkey_seed_file"
	DaemonSeedFileVariable = "NATS_DAEMON_NKEY_SEED_FILE"
	DaemonSeedVariable     = "NATS_DAEMON_NKEY_SEED"
)

// names are the three names one seed is read under: the configuration key naming its file, the
// variable naming its file, and the variable holding it.
type names struct{ fileKey, fileVariable, variable string }

var (
	paneSeed   = names{SeedFileKey, SeedFileVariable, SeedVariable}
	daemonSeed = names{DaemonSeedFileKey, DaemonSeedFileVariable, DaemonSeedVariable}
)

// Configured reports whether Seed reads a seed at all: file (the SeedFileKey's, "" when the
// configuration names none) is set, or either variable is, whatever it holds.
func Configured(file string, lookup func(string) (string, bool)) bool {
	_, ok := resolve(paneSeed, file, lookup, nil)
	return ok
}

// Seed is the nkey seed of the NATS user a process connects as: the trimmed contents of file, the
// file the configuration's SeedFileKey names ("" when it names none); else those of the file
// NATS_NKEY_SEED_FILE names; else NATS_NKEY_SEED. The first source set is authoritative: an empty
// NATS_NKEY_SEED_FILE, or a missing, unreadable, or blank file, or one whose mode lets more than
// its owner read it, is an error naming the key or variable and the path, never a fallback to the
// next source or to no credential; so is a blank NATS_NKEY_SEED, and a seed that is not a user nkey
// seed. The file's group may read it only when root owns it and the reader is not root
// (config.ReadGroupSecretPointer): a non-root process in a pod — the daemon, or `legion
// probe-image` in the probe pod — reads a kubelet-mounted Secret file, root's, through the pod's
// fsGroup. None set is "": the connection carries no credential, as every connection did before
// servers required one. No error carries the seed.
// Deploy order: a process gets a seed only after its server has nkey users (the SRE's stage 1, with
// the no_auth_user fallback); a server with no users sends no nonce, and nats.go then refuses the
// nkey ("nats: nkeys not supported by the server") rather than connecting without it.
func Seed(file string, lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve(paneSeed, file, lookup, config.ReadGroupSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// DaemonSeed is Seed of the daemon's own seed, under its own names: the file the configuration's
// DaemonSeedFileKey names ("" when it names none), else the file NATS_DAEMON_NKEY_SEED_FILE names,
// else NATS_DAEMON_NKEY_SEED, with Seed's refusals and file mode rule. None set is "": the daemon
// then connects as the pane seed. It never falls back to the pane seed's names, nor they to it.
func DaemonSeed(file string, lookup func(string) (string, bool)) (string, error) {
	src, ok := resolve(daemonSeed, file, lookup, config.ReadGroupSecretPointer)
	if !ok {
		return "", nil
	}
	return src.seed()
}

// SeedFile is Seed of the file the configuration's SeedFileKey names, alone, refused when its group
// or others can read it (config.ReadPrivateSecretPointer): what `legion controller start` checks,
// on the operator's own machine, before handing its Oh My Pi that file.
func SeedFile(file string) (string, error) {
	return fileSource(paneSeed.fileKey, file, config.ReadPrivateSecretPointer).seed()
}

// source is one place a seed is read from: its name, which every refusal carries, and read, which
// answers the seed or why the source holds none.
type source struct {
	name string
	read func() (string, error)
}

// resolve is the one precedence every reader of a seed shares, under n's names: file when the
// configuration names one, else the file variable when set, else the seed variable when set,
// whatever each holds; ok is false when none is. read reads a file source (the key or variable
// naming it, and its path).
func resolve(n names, file string, lookup func(string) (string, bool), read func(key, path string) (string, error)) (source, bool) {
	if file != "" {
		return fileSource(n.fileKey, file, read), true
	}
	if path, set := lookup(n.fileVariable); set {
		if path == "" {
			return source{n.fileVariable, func() (string, error) {
				return "", fmt.Errorf("%s is set but empty", n.fileVariable)
			}}, true
		}
		return fileSource(n.fileVariable, path, read), true
	}
	if value, set := lookup(n.variable); set {
		return source{n.variable, func() (string, error) {
			if seed := strings.TrimSpace(value); seed != "" {
				return seed, nil
			}
			return "", fmt.Errorf("%s is set but empty", n.variable)
		}}, true
	}
	return source{}, false
}

// fileSource is the file path that key (a configuration key or a variable) names, which read reads.
func fileSource(key, path string, read func(key, path string) (string, error)) source {
	return source{fmt.Sprintf("%s (%s)", key, path), func() (string, error) { return read(key, path) }}
}

// seed is the source's seed, refused unless it is a user's nkey seed.
func (s source) seed() (string, error) {
	seed, err := s.read()
	if err != nil {
		return "", err
	}
	if _, err := userKey(seed, s.name); err != nil {
		return "", err
	}
	return seed, nil
}

// Connect dials urls as the nkey user seed is the seed of, a seed Seed answered; "" connects
// without a credential.
func Connect(urls []string, seed string, options ...nats.Option) (*nats.Conn, error) {
	if seed != "" {
		user, err := userKey(seed, "the NATS nkey seed")
		if err != nil {
			return nil, err
		}
		public, err := user.PublicKey()
		if err != nil {
			return nil, err
		}
		options = append(options, nats.Nkey(public, user.Sign))
	}
	return nats.Connect(strings.Join(urls, ","), options...)
}

// permissionRefusal is the operation and subject a server's permissions violation names
// (`Permissions Violation for Subscription to "<subject>"`, or `… Publish to …`).
var permissionRefusal = regexp.MustCompile(`(?i)(publish|subscription) to "([^"]+)"`)

// LogEvents is the connection option that logs what the server reports about the connection
// asynchronously: at error, every permission the server refuses it, a subscription or a publish,
// the JetStream API requests a consumer makes included, and a terminal close (a fatal server -ERR,
// reconnects exhausted), once, with its cause; at warn, every other asynchronous error, with the
// subject of the subscription it names, and every disconnect, with its cause; at info, every
// reconnect, with the server. nats.go's default handler writes a refusal to stderr unlabelled and
// nothing for the rest. It owns the connection's AsyncErrorCB, DisconnectedErrCB, ReconnectedCB and
// ClosedCB: an option after it that sets one replaces its handler.
func LogEvents(log *slog.Logger) nats.Option {
	return func(o *nats.Options) error {
		o.AsyncErrorCB = func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if !errors.Is(err, nats.ErrPermissionViolation) {
				subject := ""
				if sub != nil {
					subject = sub.Subject
				}
				log.Warn("NATS reported an asynchronous error", "subject", subject, "error", err)
				return
			}
			operation, subject := "", ""
			if m := permissionRefusal.FindStringSubmatch(err.Error()); m != nil {
				operation, subject = strings.ToLower(m[1]), m[2]
			}
			log.Error("NATS refused the daemon a permission: its NATS user lacks that grant", "operation", operation, "subject", subject, "error", err)
		}
		// A nil cause is the connection closing: the daemon's own Close, or a terminal close, which
		// ClosedCB logs with its cause.
		o.DisconnectedErrCB = func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("NATS connection lost", "error", err)
			}
		}
		o.ReconnectedCB = func(c *nats.Conn) {
			log.Info("NATS connection restored", "server", c.ConnectedUrlRedacted())
		}
		// The connection's LastError is a terminal close's cause, and nil after the daemon's own Close,
		// which is no error to report.
		o.ClosedCB = func(c *nats.Conn) {
			if err := c.LastError(); err != nil {
				log.Error("NATS connection closed", "error", err)
			}
		}
		return nil
	}
}

// WithLastError is err naming conn's last error, the cause of a terminal close (a fatal server
// -ERR, reconnects exhausted) that nats.go hands no handler; err itself when conn has none.
func WithLastError(err error, conn *nats.Conn) error {
	if last := conn.LastError(); last != nil {
		return fmt.Errorf("%w (the NATS connection's last error: %v)", err, last)
	}
	return err
}

// PublicKey is the public key of the nkey user seed is the seed of, a seed Seed answered: what a
// process may say about the seed it holds without the seed leaving it.
func PublicKey(seed string) (string, error) {
	user, err := userKey(seed, "the NATS nkey seed")
	if err != nil {
		return "", err
	}
	return user.PublicKey()
}

// userKey is seed's key pair, refused, naming source, when seed is no nkey seed or the seed of
// something other than a user.
func userKey(seed, source string) (nkeys.KeyPair, error) {
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
	return user, nil
}
