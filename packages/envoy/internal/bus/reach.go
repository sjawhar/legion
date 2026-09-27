package bus

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// AllowRemoteEnvVar names the variable a run sets to "1" to let Connect reach a NATS server that
// is not this machine's.
const AllowRemoteEnvVar = "ENVOY_ALLOW_REMOTE_NATS"

// ErrRemoteNATS is what Connect refuses a NATS server this machine does not run with. Only the
// deployed services (ConnectOwningStream) reach another machine's bus without saying so.
var ErrRemoteNATS = errors.New("bus: refusing a NATS server that is not this machine's")

// refuseRemoteNATS reports the first url naming another machine, unless the run opted in. An
// empty urls is nats.go's own default, this machine's server, and passes.
func refuseRemoteNATS(urls []string) error {
	if os.Getenv(AllowRemoteEnvVar) == "1" {
		return nil
	}
	for _, raw := range urls {
		if localNATSURL(raw) {
			continue
		}
		return fmt.Errorf("%w: %s (set %s=1 to reach it)", ErrRemoteNATS, strings.TrimSpace(raw), AllowRemoteEnvVar)
	}
	return nil
}

// localNATSURL reports whether raw names a server on this machine. It reads the URL and never
// resolves it: a lookup would reach the network the refusal is about, and a name that resolves to
// a loopback address on one machine resolves to a shared server on another - the devbox's own
// envoy-nats is exactly such a name. A URL it cannot read names no server it can vouch for.
func localNATSURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if !strings.Contains(trimmed, "://") {
		trimmed = "nats://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
