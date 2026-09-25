package runtime

import (
	"regexp"
	"strings"
)

// secretLikeName is a trailing credential segment, optionally followed by _FILE, or PRIVATE_KEY
// anywhere, in any case (packages/daemon/src/daemon/environment.ts:247-260). The segment must end
// the name: TOKENIZER, X_PATH, and X_KEYBOARD are not credentials.
var secretLikeName = regexp.MustCompile(`(?i)(?:_SECRET|_TOKEN|_GRANT|_KEY|_PASSWORD|_PASSWD|_PAT|_CREDENTIALS)(?:_FILE)?$|PRIVATE_KEY`)

// IsSecretLikeName reports whether an environment variable's name is shaped like a credential's.
// Every runtime refuses such a name in a spec's plain Env, since a secret travels in Secrets and
// reaches the process as a file; tmux also drops such names from the environment it passes panes.
func IsSecretLikeName(name string) bool { return secretLikeName.MatchString(name) }

// HoldsSecretValue reports whether a variable of that name would hold a secret's value: shaped like
// a credential's, and not a `<NAME>_FILE` pointer to the file holding one. A spec's Env and an
// operator's pod env refuse such a name, since a secret reaches the process as a file.
func HoldsSecretValue(name string) bool {
	return IsSecretLikeName(name) && !strings.HasSuffix(name, "_FILE")
}
