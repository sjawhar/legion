package brokertoken

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// AccountTokenCacheTTL is how long an AccountToken reuses the bytes it last read. Opening the file
// on every call would cost a read per request for a credential that changes a few times a day: the
// kubelet rewrites a projected token in place well before it expires, and the broker refuses one
// that lives longer than an hour from its issue, so bytes this fresh are never near expiry and a
// rewrite reaches the broker within this window of it.
const AccountTokenCacheTTL = 30 * time.Second

// AccountToken reads Dispatch's own projected Kubernetes ServiceAccount token
// (DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE), the credential every call Dispatch makes to the
// secrets broker presents as its bearer: the broker resolves the calling service from it
// (BROKER_API_ACCOUNTS). One reader serves every such call — the credential-request UI's relay
// (internal/dispatch/agentsecrets) and this package's introspection — so a projected token is
// cached once rather than once per caller.
//
// A projected token rotates, so the path is read at call time and never once at boot.
type AccountToken struct {
	// Path is the file the token is read from.
	Path string
	// TTL is how long Read reuses what it read, AccountTokenCacheTTL for every reader
	// NewAccountToken builds. A test that proves a rewritten file is picked up lapses the cache
	// by zeroing it.
	TTL time.Duration

	mu     sync.Mutex
	cached string
	readAt time.Time
}

// NewAccountToken is a reader of the projected ServiceAccount token at path, which Read opens at
// call time, caching what it reads for AccountTokenCacheTTL.
func NewAccountToken(path string) *AccountToken {
	return &AccountToken{Path: path, TTL: AccountTokenCacheTTL}
}

// AccountTokenError is a call that never reached the broker because Dispatch could not read its
// own ServiceAccount token from Path: the file is missing or unreadable, or holds nothing. It is
// its own type because what is wrong is Dispatch's deployment, not the broker, and a route must
// say so rather than send the operator to a broker that was never asked.
type AccountTokenError struct {
	// Path is the token file Dispatch tried to read.
	Path string
	// Err is why it could not: the read's own error, or errEmptyAccountToken.
	Err error
}

func (e *AccountTokenError) Error() string {
	return fmt.Sprintf("brokertoken: read the ServiceAccount token %s: %v", e.Path, e.Err)
}

func (e *AccountTokenError) Unwrap() error { return e.Err }

// errEmptyAccountToken is an AccountTokenError's reason when the file reads but holds nothing.
var errEmptyAccountToken = errors.New("the file is empty")

// Read is the bearer a call presents: Path's trimmed contents, read again once the copy it cached
// is older than TTL. A file that cannot be read, or that holds nothing, is an *AccountTokenError
// naming the path and no request at all, never an empty bearer for the broker to refuse: a
// projected token Dispatch cannot read is Dispatch's own misconfiguration.
func (a *AccountToken) Read() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != "" && time.Since(a.readAt) < a.TTL {
		return a.cached, nil
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", &AccountTokenError{Path: a.Path, Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &AccountTokenError{Path: a.Path, Err: errEmptyAccountToken}
	}
	a.cached, a.readAt = token, time.Now()
	return token, nil
}
