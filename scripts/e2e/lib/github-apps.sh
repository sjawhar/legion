# shellcheck shell=bash disable=SC2034,SC2154 # require_app_inputs sets <role>_app_* by name; the caller sets work
# Legion's two role Apps as a live proof takes them: from operator inputs, never a literal of this
# public repository, and proved against GitHub before the run writes anything there.
#
# Sourced, never run. The caller defines fail (which exits) and note, and sets work, the run's
# scratch directory. Each App is two required inputs, read by require_app_inputs:
#   LEGION_E2E_<ROLE>_APP_ID        the App's numeric id (ROLE is IMPLEMENT or REVIEW)
#   LEGION_E2E_<ROLE>_APP_KEY_NAME  the name of the secret-store key holding the App's private key,
#                                   base64-encoded PEM, which the `secrets` CLI resolves
# write_github_apps writes the `github_apps` block of legion.yaml from them, each key read by a
# private_key_command that runs `secrets <name> -- …`, and check_github_apps proves each App's key
# is its id's: lib/app-logins.ts signs an App JWT with the key and refuses the run unless GitHub's
# GET /app answers that id. The key never enters this shell, an argv or a file.

github_apps_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

# require_app_inputs ROLE... sets <role>_app_id and <role>_app_key_name for each ROLE (implement,
# review) from its two inputs, or fails naming the input that is unset or malformed. A name is
# letters, digits and underscores, so it is safe in the key command's shell text and as a variable
# name; with both roles read, one App id or one key name for both is refused, since the daemon
# refuses two roles on one App at boot.
require_app_inputs() {
  local role upper id_var key_var id key
  for role in "$@"; do
    case "$role" in implement | review) ;; *) fail "require_app_inputs: no App role '$role'" ;; esac
    upper=${role^^}
    id_var=LEGION_E2E_${upper}_APP_ID
    key_var=LEGION_E2E_${upper}_APP_KEY_NAME
    id=${!id_var:-}
    key=${!key_var:-}
    [ -n "$id" ] || fail "$id_var is unset: name the $role App's numeric id, which GitHub's GET /app must answer for the key $key_var names"
    [[ "$id" =~ ^[1-9][0-9]*$ ]] || fail "$id_var must be the $role App's numeric id, not '$id'"
    [ -n "$key" ] || fail "$key_var is unset: name the secret-store key holding the $role App's private key (base64-encoded PEM), which the secrets CLI resolves"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail "$key_var must be a secret-store key name (letters, digits and underscores), not '$key'"
    printf -v "${role}_app_id" %s "$id"
    printf -v "${role}_app_key_name" %s "$key"
  done
  if [ -n "${implement_app_id:-}" ] && [ -n "${review_app_id:-}" ]; then
    [ "$implement_app_id" != "$review_app_id" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_REVIEW_APP_ID are both $implement_app_id: the workflow needs two Apps"
    [ "$implement_app_key_name" != "$review_app_key_name" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_KEY_NAME and LEGION_E2E_REVIEW_APP_KEY_NAME are both $implement_app_key_name: each App has its own key"
  fi
}

# app_key_command NAME prints the private_key_command that prints the PEM the secret-store key NAME
# holds: `secrets` puts the value in one `sh` child's environment, which turns the store's
# Node-style base64 (URL-safe alphabet, unpadded, maybe one dangling sextet) into POSIX base64 and
# decodes it to stdout, the daemon's pipe.
app_key_command() {
  # shellcheck disable=SC2016 # the expansions are the key command's own, run by its sh
  printf 'secrets %s -- sh -c '\''value=$(printf %%s "${%s}" | tr "_-" "/+"); case $((${#value} %% 4)) in 1) value=${value%%?};; 2) value="${value}==";; 3) value="${value}=";; esac; printf %%s "$value" | base64 -d'\''' "$1" "$1"
}

# write_github_apps ROLE... writes $work/github-apps.yaml, the `github_apps` block of legion.yaml
# for each ROLE require_app_inputs read; a caller's legion.yaml includes the file as it is, so the
# Apps the run proves are the Apps its daemon runs as. The block names commands, never a key.
write_github_apps() {
  local role id_var key_var
  {
    echo "github_apps:"
    for role in "$@"; do
      id_var=${role}_app_id
      key_var=${role}_app_key_name
      [ -n "${!id_var:-}" ] && [ -n "${!key_var:-}" ] ||
        fail "write_github_apps: the $role App's inputs are unread: require_app_inputs $role has not run"
      # jq quotes the command as a JSON string, which YAML reads as a double-quoted scalar.
      printf '  %s:\n    app_id: "%s"\n    private_key_command: %s\n' "$role" "${!id_var}" \
        "$(app_key_command "${!key_var}" | jq -Rs .)"
    done
  } >"$work/github-apps.yaml"
}

# check_github_apps ROLE... proves each ROLE's App of $work/github-apps.yaml against GitHub and sets
# github_app_logins to lib/app-logins.ts's answer, {"<role>":"<slug>[bot]",…}: each key command
# runs as the daemon runs it, its key signs an App JWT with the App's id as issuer, and GET /app
# must answer that id. A key GitHub refuses, an answer naming another id, or a GitHub that never
# answers fails the check, naming the App by its role and id and never the key.
github_app_logins=
check_github_apps() {
  local roles
  roles=$(IFS=, && printf %s "$*")
  github_app_logins=$(bun "$github_apps_lib/app-logins.ts" --roles "$roles" "$work/github-apps.yaml") ||
    fail "lib/app-logins.ts could not prove the Legion role Apps against GitHub's GET /app (its reason is above)"
  note "GET /app answers each App's configured id: $(jq -c . <<<"$github_app_logins")"
}
