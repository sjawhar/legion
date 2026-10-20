// The Stage 4a setup checks' pure logic: the deployment setup argv and its non-secret marker.
// The live checks use these through the real Kubernetes launcher path; these tests keep parsing
// failures visible to ordinary go test without requiring a cluster.
package sandbox

import (
	"fmt"
	"strconv"
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
    -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_PARAMETERS \
    GIT_CONFIG_NOSYSTEM=1 HOME="$home" GIT_TERMINAL_PROMPT=0 \
    git config --show-scope --get-all credential.https://github.com.helper
) || helpers=
tab='	'
helper_scope=
helper=
helper_count=0
helper_lines=0
while IFS= read -r line || [ -n "$line" ]; do
  [ -n "$line" ] || continue
  helper_lines=$((helper_lines + 1))
  case $line in
    *"$tab"*)
      scope=${line%%"$tab"*}
      value=${line#*"$tab"}
      ;;
    *) continue ;;
  esac
  [ -n "$value" ] || continue
  helper_scope=$scope
  helper=$value
  helper_count=$((helper_count + 1))
done <<EOF
$helpers
EOF
helper_class=other
if [ "$helper_count" -eq 1 ] && [ "$helper_scope" = command ] && [ "$helper" = '!legion git-credential' ]; then
  helper_class=expected
fi
if (
  cd /tmp
  env -u GIT_ASKPASS -u SSH_ASKPASS -u GH_TOKEN -u GITHUB_TOKEN \
    -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_PARAMETERS \
    GIT_CONFIG_NOSYSTEM=1 HOME="$home" GIT_TERMINAL_PROMPT=0 \
    timeout 60 git ls-remote "$1" HEAD >/dev/null
); then
  git=ok
else
  git=failed
fi
printf 'setup:%s:%s/%s cwd=%s uid=%s ppid=%s git_from=/tmp helper=%s helper_count=%s helper_lines=%s git=%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" "$cwd" "$uid" "$PPID" "$helper_class" "$helper_count" "$helper_lines" "$git" >>"$LEGION_E2E_MARKER"
`

type setupMarker struct {
	role, incarnation, cwd, uid, ppid, gitFrom, helper, helperCount, helperLines, git string
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
		case "helper":
			if marker.helper != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper", line)
			}
			marker.helper = value
		case "helper_count":
			if marker.helperCount != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_count", line)
			}
			marker.helperCount = value
		case "helper_lines":
			if marker.helperLines != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_lines", line)
			}
			marker.helperLines = value
		case "git":
			if marker.git != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats git", line)
			}
			marker.git = value
		default:
			return setupMarker{}, fmt.Errorf("setup marker %q has unknown field %q", line, name)
		}
	}
	if marker.cwd == "" || marker.uid == "" || marker.ppid == "" || marker.gitFrom == "" || marker.helper == "" || marker.helperCount == "" || marker.helperLines == "" || (marker.git != "ok" && marker.git != "failed") {
		return setupMarker{}, fmt.Errorf("setup marker %q is incomplete", line)
	}
	if marker.helper != "expected" && marker.helper != "other" {
		return setupMarker{}, fmt.Errorf("setup marker %q has helper classification %q", line, marker.helper)
	}
	for _, field := range []struct {
		name, value string
	}{{"helper_count", marker.helperCount}, {"helper_lines", marker.helperLines}} {
		if _, err := strconv.ParseUint(field.value, 10, 64); err != nil {
			return setupMarker{}, fmt.Errorf("setup marker %q has non-numeric %s %q", line, field.name, field.value)
		}
	}
	return marker, nil
}

func setupHelperProblem(marker setupMarker) error {
	if marker.helper == "expected" {
		return nil
	}
	return fmt.Errorf("setup helper classification %q with count %s and lines %s, want expected", marker.helper, marker.helperCount, marker.helperLines)
}
