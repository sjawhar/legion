# shellcheck shell=bash disable=SC2034,SC2154 # require_app_inputs sets <role>_app_* by name; the caller sets work
# Legion's two role Apps as a live proof takes them: the default pair below, any
# other deployment's through its inputs, and either one proved against GitHub before the run
# writes anything there.
#
# Sourced, never run. The caller defines fail (which exits) and note, and sets work, the run's
# scratch directory. The gated Go proofs (packages/daemon/internal/api/real_github_test.go) are
# callers too: liveProofApps defines fail and reads require_app_inputs's results and
# app_key_command from stdout, so neither of those two writes anything else to stdout, and
# proveLiveProofApps defines fail and note, writes $work/github-apps.yaml itself from the block the
# proofs act with, and runs check_github_apps on it. Each App is an id and the
# name of the secret-store key holding its private key, read by require_app_inputs from two inputs
# that default to the pair below:
#   LEGION_E2E_<ROLE>_APP_ID        the App's numeric id (ROLE is IMPLEMENT or REVIEW)
#   LEGION_E2E_<ROLE>_APP_KEY_NAME  the secret-store key the key runner resolves to the App's
#                                   private key, base64-encoded PEM
# and one input for both, the key runner:
#   LEGION_E2E_APP_KEY_RUNNER       the command that runs `<runner> <NAME> -- <command>` with the
#                                   key NAME holds in that command's environment alone; `secrets`
#                                   by default, or any store's runner of that form (`agent-secrets`
#                                   reads the broker with no local fallback). Legion names no store.
# A deployment with its own Apps sets all four; a run here sets none and acts as it always has.
# write_github_apps writes the `github_apps` block of legion.yaml from them, each key read by a
# private_key_command that runs `<runner> <name> -- …`, and check_github_apps proves each App's key
# is its id's, whether the pair came from a default or an input: lib/app-logins.ts signs an App JWT
# with the key and refuses the run unless GitHub's GET /app answers that id and the write
# permission the role's work needs. The key never enters
# this shell, an argv or a file. Each caller that writes a legion.yaml includes
# $work/github-apps.yaml as it is (`$(<"$work/github-apps.yaml")` in its heredoc), never a copy or
# an edit, so the block the check proved is the block the daemon loads; Stage 4a, which writes no
# legion.yaml, hands its Go harness the same <role>_app_id and <role>_app_key_name the check read.
# app_key_unsets is the `env` argument list that keeps every
# App key out of a process the run starts. It is built when require_app_inputs runs, so a key
# variable a caller exports after that call is not in it.

github_apps_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

# The default pair, which a run that sets no input takes: the App ids and secret-store key names
# the live proofs here have used since they were written. GitHub shows both Apps owned by an
# organization, not by this repository's owner, so a deployment with Apps of its own passes them
# through the inputs; no other deployment's ids or names belong here.
default_implement_app_id=3202636
default_implement_app_key_name=LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64
default_review_app_id=3202653
default_review_app_key_name=GH_REVIEW_APP_PRIVATE_KEY_B64

# require_app_inputs ROLE... sets <role>_app_id and <role>_app_key_name for each ROLE (implement,
# review) from its two inputs, each defaulting to the default pair's value above, and fails
# naming an input that is set but malformed, never echoing its value (an operator who pastes a key
# where its name goes must not see it in the transcript). A name is letters, digits and
# underscores, so it is safe in the key command's shell text and as a variable name; when this call
# reads both roles, one App id or one key name for both is refused, since the daemon refuses two
# roles on one App at boot. It records the roles it read for
# write_github_apps, and sets app_key_unsets: `-u NAME` for each role's key name, for the
# default pair's two, and for every exported variable whose name holds PRIVATE_KEY, so a process
# the run starts inherits no App key, the proof human's included.
github_apps_read=()
app_key_unsets=()
app_key_runner=
require_app_inputs() {
  # The checks below use bracket ranges, which bash's =~ reads in the caller's locale: under a
  # UTF-8 locale [0-9] and [A-Za-z] admit fullwidth digits and accented letters. C keeps them ASCII.
  local LC_ALL=C
  local role upper id_var key_var id_default key_default id key name roles_read=()
  for role in "$@"; do
    case "$role" in implement | review) ;; *) fail "require_app_inputs: no App role '$role'" ;; esac
    upper=${role^^}
    id_var=LEGION_E2E_${upper}_APP_ID
    key_var=LEGION_E2E_${upper}_APP_KEY_NAME
    id_default=default_${role}_app_id
    key_default=default_${role}_app_key_name
    # An input set to an empty value is a command that printed nothing, not a choice of the
    # default: it is refused, where an unset one takes the default.
    id=${!id_var-${!id_default}}
    key=${!key_var-${!key_default}}
    # The bounds keep a pasted credential out of the message: a base64 key is letters and digits
    # too, so only its length tells it from a store key's name, and a refusal prints neither.
    [[ "$id" =~ ^[1-9][0-9]*$ && ${#id} -le 12 ]] || fail "$id_var must be the $role App's numeric id (1 to 12 digits, no leading zero); its value is not printed"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && ${#key} -le 64 ]] || fail "$key_var must be a secret-store key's name (letters, digits and underscores, at most 64), not the key itself; its value is not printed"
    printf -v "${role}_app_id" %s "$id"
    printf -v "${role}_app_key_name" %s "$key"
    roles_read+=("$role")
  done
  # The runner is a command word in the key command's shell text, so it is one word of path
  # characters: a store's runner by name or by path, never a command line.
  app_key_runner=${LEGION_E2E_APP_KEY_RUNNER-secrets}
  [[ "$app_key_runner" =~ ^[A-Za-z0-9_./+-]+$ && ${#app_key_runner} -le 256 ]] ||
    fail "LEGION_E2E_APP_KEY_RUNNER must be one command, by name or path (letters, digits and _ . / + -), that runs '<runner> <NAME> -- <command>'; its value is not printed"
  if [[ " ${roles_read[*]} " == *" implement "* && " ${roles_read[*]} " == *" review "* ]]; then
    [ "$implement_app_id" != "$review_app_id" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_REVIEW_APP_ID are both $implement_app_id: the workflow needs two Apps"
    [ "$implement_app_key_name" != "$review_app_key_name" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_KEY_NAME and LEGION_E2E_REVIEW_APP_KEY_NAME are both $implement_app_key_name: each App has its own key"
  fi
  github_apps_read=("${roles_read[@]}")
  app_key_unsets=()
  for role in "${roles_read[@]}"; do
    key_var=${role}_app_key_name
    app_key_unsets+=(-u "${!key_var}")
  done
  for name in "$default_implement_app_key_name" "$default_review_app_key_name"; do
    app_key_unsets+=(-u "$name")
  done
  while IFS= read -r name; do
    [[ "$name" != *PRIVATE_KEY* ]] || app_key_unsets+=(-u "$name")
  done < <(compgen -e)
}

# app_key_command NAME prints the private_key_command that prints the PEM the secret-store key NAME
# holds: the key runner (`secrets` unless LEGION_E2E_APP_KEY_RUNNER names another) puts the value in
# one `sh` child's environment, which turns the store's
# Node-style base64 (URL-safe alphabet, unpadded, maybe one dangling sextet) into POSIX base64 and
# decodes it to stdout, the daemon's pipe.
app_key_command() {
  # shellcheck disable=SC2016 # the expansions are the key command's own, run by its sh
  printf '%s %s -- sh -c '\''value=$(printf %%s "${%s}" | tr "_-" "/+"); case $((${#value} %% 4)) in 1) value=${value%%?};; 2) value="${value}==";; 3) value="${value}=";; esac; printf %%s "$value" | base64 -d'\''' "$app_key_runner" "$1" "$1"
}

# write_github_apps ROLE... writes $work/github-apps.yaml, the `github_apps` block of legion.yaml
# for each ROLE the last require_app_inputs read, or fails naming a role it did not read, before
# the file is opened. The block names commands, never a key.
write_github_apps() {
  local role id_var key_var block="github_apps:"$'\n'
  for role in "$@"; do
    [[ " ${github_apps_read[*]} " == *" $role "* ]] ||
      fail "write_github_apps: the $role App's inputs are unread: require_app_inputs $role has not run"
    id_var=${role}_app_id
    key_var=${role}_app_key_name
    # jq quotes the command as a JSON string, which YAML reads as a double-quoted scalar.
    block+=$(printf '  %s:\n    app_id: "%s"\n    private_key_command: %s' "$role" "${!id_var}" \
      "$(app_key_command "${!key_var}" | jq -Rs .)")$'\n'
  done
  printf '%s' "$block" >"$work/github-apps.yaml"
}

# check_github_apps ROLE... proves each ROLE's App of $work/github-apps.yaml against GitHub and sets
# github_app_logins to lib/app-logins.ts's answer, {"<role>":"<slug>[bot]",…}: each key command
# runs as the daemon runs it, its key signs an App JWT with the App's id as issuer, and GET /app
# must answer that id and the write permission the role's work needs (the implement App's
# contents, the review App's checks), which also refuses the two Apps swapped whole. A key GitHub
# refuses, an answer naming another id or lacking the permission, or a GitHub that never answers
# fails the check, naming the App by its role and id and never the key.
github_app_logins=
check_github_apps() {
  local roles
  roles=$(IFS=, && printf %s "$*")
  github_app_logins=$(bun "$github_apps_lib/app-logins.ts" --roles "$roles" "$work/github-apps.yaml") ||
    fail "lib/app-logins.ts could not prove the Legion role Apps against GitHub's GET /app (its reason is above)"
  note "GET /app answers each App's configured id: $(jq -c . <<<"$github_app_logins")"
}
