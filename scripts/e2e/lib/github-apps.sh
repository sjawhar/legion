# shellcheck shell=bash disable=SC2034,SC2154 # require_app_inputs sets <role>_app_* by name; the caller sets work
# Legion's two role Apps as a live proof takes them: the default pair below, any
# other deployment's through its inputs, and either one proved against GitHub before the run
# writes anything there.
#
# Sourced, never run. The caller defines fail (which exits) and note, and sets work, the run's
# scratch directory. The gated Go proofs (packages/daemon/internal/api/real_github_test.go) are
# callers too: liveProofApps defines fail and reads require_app_inputs's results and
# app_token_command from stdout, so neither of those two writes anything else to stdout, and
# proveLiveProofApps defines fail and note, writes $work/github-apps.yaml itself from the block the
# proofs act with, and runs check_github_apps on it. Each App is an id and the name of the secrets
# broker's github-app secret for it, read by require_app_inputs from two inputs that default to the
# pair below:
#   LEGION_E2E_<ROLE>_APP_ID      the App's numeric id (ROLE is IMPLEMENT or REVIEW)
#   LEGION_E2E_<ROLE>_APP_SECRET  the name of the secrets broker's github-app secret for the App,
#                                 which `agent-secrets` turns into a release of the App's
#                                 installation tokens, never its key
# A deployment with its own Apps stores each as a broker github-app secret and sets all four; a
# run here sets none and takes the default pair. The lib takes no key input: it proves an App from
# its token, and a key file or a key runner was the only reason for one.
# write_github_apps writes the `github_apps` block of legion.yaml from them, each App's tokens read
# by a token_command that runs `agent-secrets <secret> … -- …` (or reads the release file the
# refresher below wrote, when its caller names that directory), and check_github_apps proves each
# App from its token, whether the pair came from a default or an input: lib/app-logins.ts reads the
# token's bot login from GraphQL's viewer and refuses the run unless GitHub's GET /apps/<slug>
# answers the configured id and the write permission the role's work needs. A token never enters
# this shell or an argv, and a file only through the refresher (mint_app_tokens,
# start_app_token_refresher), for a daemon that cannot run agent-secrets itself: it writes each
# release to an owner-only file of a directory its caller names (the sandbox proof's, on the devbox
# user's tmpfs under bridge) and removes at teardown, and the caller passes that directory to
# write_github_apps, whose token commands then read those files. Each caller that writes a
# legion.yaml includes $work/github-apps.yaml as it is (`$(<"$work/github-apps.yaml")` in its
# heredoc), never a copy or an edit, so the block the check proved is the block the daemon loads;
# Stage 4a, which writes no legion.yaml, hands its Go harness the same <role>_app_id,
# <role>_app_secret and app_token_command the check read. app_key_unsets is the `env` argument
# list that keeps every App credential variable out of a process the run starts. It is built when
# require_app_inputs runs, so a variable a caller exports after that call is not in it.

github_apps_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

# The default pair, which a run that sets no input takes: the App ids and secrets broker github-app
# secret names of the legion-implementer and legion-reviewer Apps, both owned by the sjawhar
# account. The implement secret is agent tier; the review secret is human tier, so its first grant
# waits for a person's approval. A deployment with Apps of its own passes them through the inputs;
# no other deployment's ids or names belong here.
default_implement_app_id=5341240
default_implement_app_secret=LEGION_IMPLEMENT_APP
default_review_app_id=5341243
default_review_app_secret=LEGION_REVIEW_APP

# require_app_inputs ROLE... sets <role>_app_id and <role>_app_secret for each ROLE (implement,
# review) from its two inputs, each defaulting to the default pair's value above, and fails
# naming an input that is set but malformed, never echoing its value (an operator who pastes a
# token where its name goes must not see it in the transcript). A name is letters, digits and
# underscores, so it is safe in the token command's shell text and as a variable name; when this
# call reads both roles, one App id or one secret for both is refused, since the daemon refuses two
# roles on one App at boot. It records the roles it read for write_github_apps, and sets
# app_key_unsets: `-u NAME` for each role's secret name, for the default pair's two, and for every
# exported variable whose name holds PRIVATE_KEY, so a process the run starts inherits no App
# credential, the proof human's included.
github_apps_read=()
app_key_unsets=()
require_app_inputs() {
  # The checks below use bracket ranges, which bash's =~ reads in the caller's locale: under a
  # UTF-8 locale [0-9] and [A-Za-z] admit fullwidth digits and accented letters. C keeps them ASCII.
  local LC_ALL=C
  local role upper id_var secret_var id_default secret_default id secret name roles_read=()
  for role in "$@"; do
    case "$role" in implement | review) ;; *) fail "require_app_inputs: no App role '$role'" ;; esac
    upper=${role^^}
    id_var=LEGION_E2E_${upper}_APP_ID
    secret_var=LEGION_E2E_${upper}_APP_SECRET
    id_default=default_${role}_app_id
    secret_default=default_${role}_app_secret
    # An input set to an empty value is a command that printed nothing, not a choice of the
    # default: it is refused, where an unset one takes the default.
    id=${!id_var-${!id_default}}
    secret=${!secret_var-${!secret_default}}
    # The bounds keep a pasted credential out of the message: a token is letters and digits too, so
    # only its length tells it from a secret's name, and a refusal prints neither.
    [[ "$id" =~ ^[1-9][0-9]*$ && ${#id} -le 12 ]] || fail "$id_var must be the $role App's numeric id (1 to 12 digits, no leading zero); its value is not printed"
    [[ "$secret" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && ${#secret} -le 64 ]] || fail "$secret_var must be the name of a secrets broker github-app secret (letters, digits and underscores, at most 64), not a credential; its value is not printed"
    printf -v "${role}_app_id" %s "$id"
    printf -v "${role}_app_secret" %s "$secret"
    roles_read+=("$role")
  done
  if [[ " ${roles_read[*]} " == *" implement "* && " ${roles_read[*]} " == *" review "* ]]; then
    [ "$implement_app_id" != "$review_app_id" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_ID and LEGION_E2E_REVIEW_APP_ID are both $implement_app_id: the workflow needs two Apps"
    [ "$implement_app_secret" != "$review_app_secret" ] ||
      fail "LEGION_E2E_IMPLEMENT_APP_SECRET and LEGION_E2E_REVIEW_APP_SECRET are both $implement_app_secret: each App has its own secret"
  fi
  github_apps_read=("${roles_read[@]}")
  app_key_unsets=()
  for role in "${roles_read[@]}"; do
    secret_var=${role}_app_secret
    app_key_unsets+=(-u "${!secret_var}")
  done
  for name in "$default_implement_app_secret" "$default_review_app_secret"; do
    app_key_unsets+=(-u "$name")
  done
  while IFS= read -r name; do
    [[ "$name" != *PRIVATE_KEY* ]] || app_key_unsets+=(-u "$name")
  done < <(compgen -e)
}

# app_token_command NAME prints the token command that prints the release the secrets broker's
# github-app secret NAME gives: `agent-secrets` puts the release in one `sh` child's environment
# as NAME, which prints it to stdout, the daemon's pipe. The two `${…}` expansions are the command's
# own, expanded by the sh the daemon runs it with: LEGION_GITHUB_OWNER, when set, narrows the
# release to that owner's installation (--github-owner), and LEGION_GITHUB_READ_ONLY, when set,
# to contents and metadata read (--github-read-only). It depends on nothing but NAME.
app_token_command() {
  # shellcheck disable=SC2016 # the expansions are the token command's own, run by its sh
  printf 'agent-secrets %s --reason=legion-live-proof-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c '\''printf %%s "$%s"'\''\n' "$1" "$1"
}

# app_token_dir_ok DIR fails unless DIR is a path of letters, digits and `_ . / -` alone, since the
# token commands carry it in their shell text.
app_token_dir_ok() {
  local LC_ALL=C
  [[ "$2" =~ ^[A-Za-z0-9_./-]+$ ]] ||
    fail "$1: the token directory '$2' holds a character other than letters, digits and _ . / -, which the token commands could not carry in their shell text"
}

# The refresher, for a daemon that cannot run agent-secrets itself (the Claude Code sandbox proof's
# container under bridge). It mints, here, each ROLE's releases into DIR, one directory a role:
#   DIR/<role>/_all.json           the release of every installation (no owner)
#   DIR/<role>/<owner>.json        OWNER's installation alone
#   DIR/<role>/<owner>.read.json   OWNER's installation, read-only
# owner lowercased, each file 0600 and each directory 0700, which is the layout the token commands
# write_github_apps --token-dir DIR writes read. A release passes from agent-secrets' `sh` child to
# a temporary file of the same directory, then mv renames it over the file, so a reader sees the
# whole old release or the whole new one, and a mint that fails or prints nothing leaves no
# temporary file and the last good release as it was. No token passes through this shell.
#
# app_token_mint DIR ROLE... mints every file once, for the owners app_token_owners lists, and
# returns 1 at the first failure with app_token_mint_failed naming the App's role, id and secret,
# the file and agent-secrets' exit status, never a token. agent-secrets' own stderr (the request a
# grant waits on) reaches this shell's stderr as it is written.
app_token_owners=()
app_token_mint_failed=
app_token_mint() {
  local dir=$1 role id_var secret_var owner ro file status
  shift
  app_token_mint_failed=
  for role in "$@"; do
    id_var=${role}_app_id
    secret_var=${role}_app_secret
    if ! (umask 077 && mkdir -p -- "$dir/$role" && chmod 0700 -- "$dir/$role"); then
      app_token_mint_failed="the $role App's token directory $dir/$role could not be made owner-only"
      return 1
    fi
    for owner in "" "${app_token_owners[@]}"; do
      owner=${owner,,}
      for ro in "" 1; do
        # The release of every installation is never asked read-only.
        [ -n "$owner" ] || [ -z "$ro" ] || continue
        file=$dir/$role/${owner:-_all}${ro:+.read}.json
        status=0
        app_token_mint_file "$file" "$owner" "$ro" "${!secret_var}" || status=$?
        case "$status" in
          0) continue ;;
          125) app_token_mint_failed="agent-secrets printed nothing" ;;
          *) app_token_mint_failed="agent-secrets exited $status; no token is printed" ;;
        esac
        app_token_mint_failed="the $role App (App ${!id_var}, github-app secret ${!secret_var}) gave no release for $file: $app_token_mint_failed"
        return 1
      done
    done
  done
}
# app_token_mint_file FILE OWNER READ_ONLY SECRET writes one release atomically. It returns the
# token command's non-zero status, or 125 when the command succeeded and printed nothing; either way
# it leaves no temporary file and FILE as it was.
app_token_mint_file() {
  local file=$1 owner=$2 ro=$3 secret=$4 tmp status=0
  tmp=$(umask 077 && mktemp "$(dirname -- "$file")/.mint.XXXXXXXX") || return 1
  (umask 077 && LEGION_GITHUB_OWNER=$owner LEGION_GITHUB_READ_ONLY=$ro sh -c "$(app_token_command "$secret")" >"$tmp" </dev/null) || status=$?
  if [ "$status" != 0 ]; then
    rm -f -- "$tmp"
    return "$status"
  fi
  if [ ! -s "$tmp" ]; then
    rm -f -- "$tmp"
    return 125
  fi
  mv -f -- "$tmp" "$file"
}

# mint_app_tokens DIR OWNER... records the OWNERs the run serves (app_token_owners) and mints,
# synchronously, every release file of each role the last require_app_inputs read, failing, naming
# the App by its role and id, at the first mint that fails or prints nothing. A secret whose grant
# waits on a person's approval (the review App's, at its first grant) waits here, as long as
# agent-secrets does. DIR must exist; the caller owns it and removes it, the files with it.
mint_app_tokens() {
  local dir=$1 owner
  shift
  app_token_dir_ok mint_app_tokens "$dir"
  [ -d "$dir" ] || fail "mint_app_tokens: the token directory $dir is not a directory"
  [ "${#github_apps_read[@]}" -gt 0 ] || fail "mint_app_tokens: require_app_inputs has not run"
  for owner in "$@"; do
    [[ "$owner" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]] ||
      fail "mint_app_tokens: '$owner' is not a GitHub account login"
  done
  app_token_owners=("$@")
  app_token_mint "$dir" "${github_apps_read[@]}" || fail "$app_token_mint_failed"
}

# start_app_token_refresher DIR starts the background refresher, which mints every release file of
# the last mint_app_tokens (its roles and owners) again every app_token_refresh_seconds (15
# minutes: an installation token lasts an hour, and the daemon runs its command again once fewer
# than 20 minutes remain on a lease), and sets app_token_refresher to its pid. A failed mint prints
# one line naming the App and keeps the last good files; the next round tries again. Run
# mint_app_tokens first, so the first releases and their failures are the prerequisites'.
# stop_app_token_refresher kills it.
app_token_refresh_seconds=900
app_token_refresher=
start_app_token_refresher() {
  local dir=$1
  (
    sleeper=
    trap '[ -z "$sleeper" ] || kill "$sleeper" 2>/dev/null; exit 0' TERM
    while :; do
      sleep "$app_token_refresh_seconds" &
      sleeper=$!
      wait "$sleeper" || true
      sleeper=
      app_token_mint "$dir" "${github_apps_read[@]}" ||
        printf '   [refresher] could not refresh: %s; the last releases stay\n' "$app_token_mint_failed" >&2
    done
  ) &
  app_token_refresher=$!
}
# stop_app_token_refresher stops the refresher, when one runs, and waits for it.
stop_app_token_refresher() {
  [ -n "$app_token_refresher" ] || return 0
  kill -TERM "$app_token_refresher" 2>/dev/null || true
  wait "$app_token_refresher" 2>/dev/null || true
  app_token_refresher=
}

# write_github_apps [--token-dir DIR] ROLE... writes $work/github-apps.yaml, the `github_apps` block
# of legion.yaml for each ROLE the last require_app_inputs read, or fails naming a role it did not
# read, before the file is opened. Each token_command is app_token_command's, or, given --token-dir
# DIR (the directory the refresher writes), a command that reads the release file of the owner and
# read-only flag the daemon asks for, `DIR/<role>/${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json`
# with the owner lowercased, with `cat --`, in any POSIX sh (busybox's included). The block names
# commands, never a token.
write_github_apps() {
  local LC_ALL=C
  local role id_var secret_var token_dir='' token_command block="github_apps:"$'\n'
  if [ "${1-}" = --token-dir ]; then
    [ "$#" -ge 2 ] || fail "write_github_apps: --token-dir names no directory"
    token_dir=$2
    shift 2
    app_token_dir_ok write_github_apps "$token_dir"
  fi
  for role in "$@"; do
    [[ " ${github_apps_read[*]} " == *" $role "* ]] ||
      fail "write_github_apps: the $role App's inputs are unread: require_app_inputs $role has not run"
    id_var=${role}_app_id
    secret_var=${role}_app_secret
    if [ -n "$token_dir" ]; then
      # shellcheck disable=SC2016 # the expansions are the token command's own, run by its sh
      token_command=$(printf 'owner=$(printf %%s "$LEGION_GITHUB_OWNER" | tr "[:upper:]" "[:lower:]"); case $owner in *[!a-z0-9-]*) echo "no release file for owner $owner" >&2; exit 2;; esac; cat -- '\''%s/%s/'\''"${owner:-_all}${LEGION_GITHUB_READ_ONLY:+.read}.json"' "$token_dir" "$role")
    else
      token_command=$(app_token_command "${!secret_var}")
    fi
    # jq quotes the command as a JSON string, which YAML reads as a double-quoted scalar.
    block+=$(printf '  %s:\n    app_id: "%s"\n    token_command: %s' "$role" "${!id_var}" \
      "$(printf '%s' "$token_command" | jq -Rs .)")$'\n'
  done
  printf '%s' "$block" >"$work/github-apps.yaml"
}

# check_github_apps ROLE... proves each ROLE's App of $work/github-apps.yaml against GitHub and sets
# github_app_logins to lib/app-logins.ts's answer, {"<role>":"<slug>[bot]",…}: each token command
# runs as the daemon runs it, GraphQL's viewer under the first installation's token names the bot,
# and GET /apps/<slug> must answer the App's id and the write permission the role's work needs (the
# implement App's contents, the review App's checks), which also refuses the two Apps swapped whole.
# A command that fails, a release GitHub refuses, an answer naming another id or lacking the
# permission, or a GitHub that never answers fails the check, naming the App by its role and id and
# never a token.
github_app_logins=
check_github_apps() {
  local roles
  roles=$(IFS=, && printf %s "$*")
  github_app_logins=$(bun "$github_apps_lib/app-logins.ts" --roles "$roles" "$work/github-apps.yaml") ||
    fail "lib/app-logins.ts could not prove the Legion role Apps against GitHub from their tokens (its reason is above)"
  note "GET /apps/<slug> answers each App's configured id under its own token: $(jq -c . <<<"$github_app_logins")"
}
