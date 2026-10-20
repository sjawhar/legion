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

// liveSetupScript records, without any helper text, which credential helpers git runs for
// https://github.com from the setup's own directory, and whether `git ls-remote` of $1 from /tmp,
// outside the issue clone, succeeds with the role's helper and fails without it.
//
// Every git it runs starts under `env -i` with this allowlist alone, so nothing else of the role's
// environment (GIT_CONFIG, GIT_DIR, GIT_COMMON_DIR, GIT_CONFIG_PARAMETERS, an askpass, a token, a
// proxy) steers what git reads or how it authenticates:
//   - PATH, the role's (mainEnvironment): finds git, timeout, and the `legion` that the helper
//     `!legion git-credential` runs;
//   - LEGION_GH_OWNERS_DIR (ghAgentEnv), the one variable `legion git-credential` reads
//     (runGitCredential, cmd/legion/gitcredential.go);
//   - GIT_CONFIG_COUNT and each GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> below it, the role's
//     command-scope entries (gitCredentialEnv);
//   - HOME at an empty temporary directory, GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1,
//     so git reads no global, XDG or system configuration;
//   - GIT_TERMINAL_PROMPT=0, so a missing credential fails rather than prompts.
//
// git builds one helper list for a request from credential.helper and every credential.<url>.helper
// whose URL matches it, in its read order (system, global, local, worktree, command), and an empty
// value under any of those keys empties the list (gitcredentials(7);
// TestGitCombinesBothHelperKeysInReadOrder and TestGitCredentialHelperURLForms hold the local git
// to it). The script reads that list with `git config -z`, so a newline in a value cannot forge a
// record. An empty credential.helper or credential.https://github.com.helper resets the count. A
// record under any other helper key, empty or not, and a helper key with no value at all, may
// change what git runs for github.com, so each counts as one entry. helper_effective is the number
// of entries after the last reset, and the list is `expected` when it is exactly one entry,
// `!legion git-credential` under one of those two keys at command scope, where gitCredentialEnv
// puts it. The negative control runs the same ls-remote with every command-scope helper value
// emptied, gitCredentialEnv's GIT_CONFIG_VALUE_1 among them: legion-smoke is private, so it must
// fail, and git=ok is then the helper's doing.
const liveSetupScript = `set -eu
cwd=$(pwd -P)
uid=$(id -u)
home=$(mktemp -d)
trap 'rm -rf -- "$home"' EXIT
count=${GIT_CONFIG_COUNT:-0}
case $count in ''|*[!0-9]*) count=0 ;; esac
without_helper=
probe_git() {
  i=$count
  while [ "$i" -gt 0 ]; do
    i=$((i - 1))
    eval "key=\${GIT_CONFIG_KEY_$i-} value=\${GIT_CONFIG_VALUE_$i-}"
    if [ -n "$without_helper" ]; then
      case $(printf '%s' "$key" | tr '[:upper:]' '[:lower:]') in
        credential.*helper) value= ;;
      esac
    fi
    set -- "GIT_CONFIG_KEY_$i=$key" "GIT_CONFIG_VALUE_$i=$value" "$@"
  done
  env -i PATH="$PATH" LEGION_GH_OWNERS_DIR="${LEGION_GH_OWNERS_DIR-}" HOME="$home" \
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_COUNT="$count" "$@"
}
soh=$(printf '\001')
records=$(probe_git git config -z --show-scope --get-regexp '^credential\..*helper$' | tr '\n\0' '\001\n')
helper_scope=
helper=
helper_known=
helper_count=0
helper_effective=0
helper_lines=0
state=scope
while IFS= read -r line || [ -n "$line" ]; do
  if [ "$state" = scope ]; then
    scope=$line
    state=entry
    continue
  fi
  state=scope
  helper_lines=$((helper_lines + 1))
  novalue=
  case $line in
    *"$soh"*)
      key=${line%%"$soh"*}
      value=${line#*"$soh"}
      ;;
    *)
      key=$line
      value=
      novalue=1
      ;;
  esac
  known=
  case $key in
    credential.helper|credential.https://github.com.helper) [ -n "$novalue" ] || known=1 ;;
  esac
  if [ -n "$known" ] && [ -z "$value" ]; then
    helper_scope=
    helper=
    helper_known=
    helper_effective=0
    continue
  fi
  [ -z "$value" ] || helper_count=$((helper_count + 1))
  helper_effective=$((helper_effective + 1))
  helper_scope=$scope
  helper=$value
  helper_known=$known
done <<EOF
$records
EOF
helper_class=other
if [ "$helper_effective" -eq 1 ] && [ "$helper_scope" = command ] && [ -n "$helper_known" ] && [ "$helper" = '!legion git-credential' ]; then
  helper_class=expected
fi
if (cd /tmp && probe_git timeout 60 git ls-remote "$1" HEAD >/dev/null); then
  git=ok
else
  git=failed
fi
without_helper=1
if (cd /tmp && probe_git timeout 60 git ls-remote "$1" HEAD >/dev/null 2>&1); then
  git_without_helper=ok
else
  git_without_helper=failed
fi
printf 'setup:%s:%s/%s cwd=%s uid=%s ppid=%s git_from=/tmp helper=%s helper_count=%s helper_effective=%s helper_lines=%s git=%s git_without_helper=%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" "$cwd" "$uid" "$PPID" "$helper_class" "$helper_count" "$helper_effective" "$helper_lines" "$git" "$git_without_helper" >>"$LEGION_E2E_MARKER"
`

type setupMarker struct {
	role, incarnation, cwd, uid, ppid, gitFrom, helper, helperCount, helperEffective, helperLines, git, gitWithoutHelper string
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
	if len(fields) != 11 || !strings.HasPrefix(fields[0], "setup:") {
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
		case "helper_effective":
			if marker.helperEffective != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats helper_effective", line)
			}
			marker.helperEffective = value
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
		case "git_without_helper":
			if marker.gitWithoutHelper != "" {
				return setupMarker{}, fmt.Errorf("setup marker %q repeats git_without_helper", line)
			}
			marker.gitWithoutHelper = value
		default:
			return setupMarker{}, fmt.Errorf("setup marker %q has unknown field %q", line, name)
		}
	}
	if marker.cwd == "" || marker.uid == "" || marker.ppid == "" || marker.gitFrom == "" || marker.helper == "" || marker.helperCount == "" || marker.helperEffective == "" || marker.helperLines == "" {
		return setupMarker{}, fmt.Errorf("setup marker %q is incomplete", line)
	}
	for _, outcome := range []string{marker.git, marker.gitWithoutHelper} {
		if outcome != "ok" && outcome != "failed" {
			return setupMarker{}, fmt.Errorf("setup marker %q has git outcome %q, want ok or failed", line, outcome)
		}
	}
	if marker.helper != "expected" && marker.helper != "other" {
		return setupMarker{}, fmt.Errorf("setup marker %q has helper classification %q", line, marker.helper)
	}
	for _, field := range []struct {
		name, value string
	}{{"helper_count", marker.helperCount}, {"helper_effective", marker.helperEffective}, {"helper_lines", marker.helperLines}} {
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
	return fmt.Errorf("setup helper classification %q with count %s, effective %s and lines %s, want expected", marker.helper, marker.helperCount, marker.helperEffective, marker.helperLines)
}

// setupGitProblem is nil when the ls-remote from /tmp, outside any clone, succeeded with the role's
// helper and the same ls-remote without it failed: then the private repository was read through
// that helper.
func setupGitProblem(marker setupMarker) error {
	if marker.gitFrom == "/tmp" && marker.git == "ok" && marker.gitWithoutHelper == "failed" {
		return nil
	}
	return fmt.Errorf("setup ls-remote from %s: git=%s with the role's helper and git_without_helper=%s without it, want /tmp, ok and failed", marker.gitFrom, marker.git, marker.gitWithoutHelper)
}
