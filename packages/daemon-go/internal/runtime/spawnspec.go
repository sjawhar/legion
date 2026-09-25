package runtime

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// envName is a name a shell accepts as a variable, which is also a valid Kubernetes Secret key.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsEnvName reports whether name is one a shell accepts as an environment variable's.
func IsEnvName(name string) bool { return envName.MatchString(name) }

// ValidateSpawnSpec is the refusal every runtime makes before anything touches its disk, its
// server, or its cluster: a spec it could not honour exactly. runtimeOwned is the runtime's own
// set of the variables it sets in the agent's environment itself — each runtime ties it by a test
// to the environment it builds — so a spec that sets one, in Env or as a secret's `<NAME>_FILE`
// pointer, is refused rather than one silently winning. Above all, nothing reaches the agent as a
// plain value that is shaped like a credential: a secret travels in Secrets, and reaches the
// process as a file. A runtime adds its own refusals after this one.
func ValidateSpawnSpec(spec SpawnSpec, runtimeOwned map[string]bool) error {
	if spec.Claim == "" {
		return errors.New("spawn: no claim token")
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("spawn %s: "+format, append([]any{spec.Claim}, args...)...)
	}
	for _, field := range []struct{ name, value string }{
		{"project", spec.Project}, {"tree", spec.Tree}, {"issue", spec.Issue},
		{"role", string(spec.Role)}, {"boot token", spec.BootToken},
	} {
		if field.value == "" {
			return refuse("no %s", field.name)
		}
	}
	// Each runtime builds the workspace's path from the issue, so the issue is the one shape a
	// Dispatch key has, never a path.
	for _, key := range []struct{ name, value string }{{"tree", spec.Tree}, {"issue", spec.Issue}} {
		if !claim.IsIssueKey(key.value) {
			return refuse("%s %q is not an issue key", key.name, key.value)
		}
	}
	if !claim.IsRole(spec.Role) {
		return refuse("%q is not a role", spec.Role)
	}
	if len(spec.Prompt.RolePromptPaths) == 0 {
		return refuse("no role prompt")
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		switch {
		case !IsEnvName(name):
			return refuse("Env name %q is not an environment variable name", name)
		case runtimeOwned[name]:
			return refuse("Env sets %s, which the runtime sets itself", name)
		case IsSecretLikeName(name) && !strings.HasSuffix(name, "_FILE"):
			return refuse("Env carries %s, a credential-shaped name; a secret travels in Secrets, as a file", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Secrets)) {
		pointer := name + "_FILE"
		switch {
		case !IsEnvName(name):
			return refuse("secret %q is not an environment variable name", name)
		case runtimeOwned[pointer]:
			return refuse("secret %s's pointer %s is a variable the runtime sets itself", name, pointer)
		case spec.Env[pointer] != "":
			return refuse("secret %s's pointer %s is also set in Env", name, pointer)
		}
	}
	return nil
}
