// The Stage 4a setup checks' pure logic: the deployment setup argv and its non-secret marker.
// The live checks use these through the real Kubernetes launcher path; these tests keep parsing
// failures visible to ordinary go test without requiring a cluster.
package sandbox

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const liveSetupScript = `set -eu
cwd=$(pwd -P)
uid=$(id -u)
home=$(mktemp -d)
trap 'rm -rf -- "$home"' EXIT
helpers=$(
  env -u GIT_ASKPASS -u SSH_ASKPASS -u GH_TOKEN -u GITHUB_TOKEN \
    GIT_CONFIG_NOSYSTEM=1 HOME="$home" GIT_TERMINAL_PROMPT=0 \
    git config --show-scope --get-regexp '^credential\..*helper$'
) || helpers=
helper_scope=
helper=
helper_count=0
while IFS="$(printf '\t')" read -r scope key value; do
  [ "$key" = credential.https://github.com.helper ] || continue
  [ -n "$value" ] || continue
  helper_scope=$scope
  helper=$value
  helper_count=$((helper_count + 1))
done <<EOF
$helpers
EOF
helper_b64=$(printf %s "$helper" | base64 | tr -d '\n')
[ -n "$helper_b64" ] || helper_b64=none
if (
  cd /tmp
  env -u GIT_ASKPASS -u SSH_ASKPASS -u GH_TOKEN -u GITHUB_TOKEN \
    GIT_CONFIG_NOSYSTEM=1 HOME="$home" GIT_TERMINAL_PROMPT=0 \
    timeout 60 git ls-remote "$1" HEAD >/dev/null
); then
  git=ok
else
  git=failed
fi
printf 'setup:%s:%s/%s cwd=%s uid=%s ppid=%s git_from=/tmp helper_scope=%s helper_b64=%s helper_count=%s git=%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" "$cwd" "$uid" "$PPID" "$helper_scope" "$helper_b64" "$helper_count" "$git" >>"$LEGION_E2E_MARKER"
`

type setupMarker struct {
	role, incarnation, cwd, uid, ppid, gitFrom, helperScope, helperBase64, helperCount, git string
}

// liveSetup is the argv each Stage 4a role's start wrapper runs before its stub agent. The
// repository travels as the shell's positional argument, never interpolated into its script.
func liveSetup(repo ghrepo.Repository) []string {
	return []string{"/bin/sh", "-c", liveSetupScript, "stage4a-setup", "https://github.com/" + repo.String()}
}

// parseSetupMarker parses the one setup line a role appends to the marker the stub agent later
// appends its role:incarnation line to. It contains no credential or raw command output.
func parseSetupMarker(line string) (setupMarker, error) {
	fields := strings.Fields(line)
	if len(fields) != 9 || !strings.HasPrefix(fields[0], "setup:") {
		return setupMarker{}, fmt.Errorf("setup marker %q is not one setup record", line)
	}
	role, incarnation, found := strings.Cut(strings.TrimPrefix(fields[0], "setup:"), ":")
	if !found || role == "" || incarnation == "" {
		return setupMarker{}, fmt.Errorf("setup marker %q has no role and incarnation", line)
	}
	marker := setupMarker{role: role, incarnation: incarnation}
	for _, field := range fields[1:] {
		name, value, found := strings.Cut(field, "=")
		if !found || value == "" {
			return setupMarker{}, fmt.Errorf("setup marker %q has malformed field %q", line, field)
		}
		switch name {
		case "cwd":
			if marker.cwd != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats cwd", line)
			}
			marker.cwd = value
		case "uid":
			if marker.uid != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats uid", line)
			}
			marker.uid = value
		case "ppid":
			if marker.ppid != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats ppid", line)
			}
			marker.ppid = value
		case "git_from":
			if marker.gitFrom != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats git_from", line)
			}
			marker.gitFrom = value
		case "helper_scope":
			if marker.helperScope != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_scope", line)
			}
			marker.helperScope = value
		case "helper_b64":
			if marker.helperBase64 != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_b64", line)
			}
			marker.helperBase64 = value
		case "helper_count":
			if marker.helperCount != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_count", line)
			}
			marker.helperCount = value
		case "git":
			if marker.git != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats git", line)
			}
			marker.git = value
		default:
			return setupMarker{}, fmt.Errorf("setup marker %q has unknown field %q", line, name)
		}
	}
	if marker.cwd == "" || marker.uid == "" || marker.ppid == "" || marker.gitFrom == "" || marker.helperScope == "" || marker.helperBase64 == "" || marker.helperCount == "" || (marker.git != "ok" && marker.git != "failed") {
		return setupMarker{}, fmt.Errorf("setup marker %q is incomplete", line)
	}
	return marker, nil
}
