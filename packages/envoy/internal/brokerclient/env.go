// packages/envoy/internal/brokerclient/env.go
//
// The environment this client is configured from, in one place, because three commands read it:
// agent-secrets, agent-secrets-helper and `legion controller start`. Every variable here is
// unset on a machine that has no identity of its own, and with them unset nothing changes: a
// machine still logs in through the device grant, and a session still registers with the helper.

package brokerclient

import (
	"fmt"
	"os"
	"strings"
)

const (
	// MachineIdentityVar names how this machine proves who it is to the broker's token
	// exchange: "metadata" for the Compute Engine metadata server, "file:<path>" for a
	// Kubernetes projected service-account token the runtime mounts, and "pool" for a Google
	// identity token minted through a workload identity pool. Unset means this machine has no
	// identity of its own.
	MachineIdentityVar = "AGENT_SECRETS_MACHINE_IDENTITY"
	// GoogleAudienceVar is the audience this machine's Google identity token is minted for,
	// which the broker checks against its own setting. The metadata and pool sources need it.
	GoogleAudienceVar = "AGENT_SECRETS_GOOGLE_AUDIENCE"
	// PoolAudienceVar is the workload identity pool provider the machine's own credential is
	// presented to, the full resource name Google's security token service takes.
	PoolAudienceVar = "AGENT_SECRETS_POOL_AUDIENCE"
	// PoolIdentityVar is the managed identity the pool mints the token for: the service
	// account's client id, or its email.
	PoolIdentityVar = "AGENT_SECRETS_POOL_IDENTITY"
	// PoolSubjectTokenFileVar is the file holding the credential the pool's provider trusts.
	PoolSubjectTokenFileVar = "AGENT_SECRETS_POOL_SUBJECT_TOKEN_FILE"
	// SessionNameVar is the session name the trade asks to write under, and is this machine's
	// hostname when unset. A person's sign-in asks under a name of their own derived from it,
	// since the broker holds one session name to one identity.
	SessionNameVar = "AGENT_SECRETS_SESSION_NAME"
	// filePrefix is the one form of MachineIdentityVar that carries a value.
	filePrefix = "file:"
)

// MachineSource is the identity this machine presents to the exchange, as MachineIdentityVar
// names it, and nil when that variable is unset — this machine has none, and every caller then
// works as it did before. A variable that names a source without what that source needs is an
// error naming both, so a misconfigured machine says so at its start rather than at its first
// trade.
func MachineSource(getenv func(string) string) (Source, error) {
	switch kind := strings.TrimSpace(getenv(MachineIdentityVar)); {
	case kind == "":
		return nil, nil
	case kind == "metadata":
		audience := getenv(GoogleAudienceVar)
		if audience == "" {
			return nil, fmt.Errorf("%s=metadata needs %s, the audience this machine's Google identity token is minted for", MachineIdentityVar, GoogleAudienceVar)
		}
		return &MetadataSource{Audience: audience}, nil
	case strings.HasPrefix(kind, filePrefix):
		path := strings.TrimSpace(strings.TrimPrefix(kind, filePrefix))
		if path == "" {
			return nil, fmt.Errorf("%s=file: names no file; it is file:<path>, the projected service-account token this machine's runtime mounts", MachineIdentityVar)
		}
		return &FileSource{Path: path}, nil
	case kind == "pool":
		pool := &PoolSource{
			Audience:     getenv(GoogleAudienceVar),
			PoolAudience: getenv(PoolAudienceVar),
			Identity:     getenv(PoolIdentityVar),
		}
		file := getenv(PoolSubjectTokenFileVar)
		for _, missing := range []struct{ value, name string }{
			{pool.Audience, GoogleAudienceVar}, {pool.PoolAudience, PoolAudienceVar},
			{pool.Identity, PoolIdentityVar}, {file, PoolSubjectTokenFileVar},
		} {
			if missing.value == "" {
				return nil, fmt.Errorf("%s=pool needs %s", MachineIdentityVar, missing.name)
			}
		}
		pool.Subject = &FileSource{Path: file}
		return pool, nil
	default:
		return nil, fmt.Errorf("%s=%q is none of metadata, file:<path> or pool", MachineIdentityVar, kind)
	}
}

// SessionName is the name a trade asks to write under: SessionNameVar when it is set, else this
// machine's hostname. The broker holds one session name to one identity for as long as it
// remembers it, so a person's sign-in on the same machine passes a prefix of their own and the
// two never contend for one name.
func SessionName(getenv func(string) string, prefix string) (string, error) {
	name := strings.TrimSpace(getenv(SessionNameVar))
	if name == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("this machine's hostname is the session name a trade asks under, and it could not be read (%s names one instead): %w", SessionNameVar, err)
		}
		name = hostname
	}
	if prefix != "" {
		return prefix + "-" + name, nil
	}
	return name, nil
}
