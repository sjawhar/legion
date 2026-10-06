#!/usr/bin/env bash
# The proof pod's identities and GitHub credentials: what a stage proof run from a pod in the cluster
# it drives (scripts/e2e/README.md, "From a pod") has in place of the devbox's kubeconfig, its
# `secrets` CLI and its routed gh. Run, never sourced:
#
#   bash scripts/e2e/lib/proof-pod.sh run <stage script> [<arg>...]
#   bash scripts/e2e/lib/proof-pod.sh kubeconfig <file>
#   bash scripts/e2e/lib/proof-pod.sh app-token <app id> <key file> <owner/repo> [<cache file>]
#   bash scripts/e2e/lib/proof-pod.sh human <dir> <app id> <key file> <owner/repo>
#
# kubeconfig writes a kubeconfig with two contexts on the pod's own cluster, reached at the address
# every pod is given (KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT) and checked against the
# pod's CA:
#   operator  the pod's own ServiceAccount, by its token file, which the client re-reads as the
#             kubelet rotates it;
#   runtime   the Legion daemon's ServiceAccount, LEGION_PROOF_DAEMON_SERVICE_ACCOUNT
#             (<namespace>/<name>, required): an exec credential plugin (runtime-token, below) asks
#             the TokenRequest API for a one-hour token of that account, as the operator, and the
#             client asks again once it expires. No token of it is written anywhere.
# The pod's ServiceAccount directory is /var/run/secrets/kubernetes.io/serviceaccount, or
# LEGION_PROOF_SERVICE_ACCOUNT_DIR.
#
# run writes that kubeconfig into a fresh directory and runs the stage script with the inputs it
# answers: KUBECONFIG (so the operator context resolves as it does on any host),
# LEGION_E2E_RUNTIME_KUBECONFIG, LEGION_E2E_RUNTIME_CONTEXT=runtime,
# LEGION_E2E_OPERATOR_CONTEXT=operator, and LEGION_E2E_RUNTIME_SERVICE_ACCOUNT, the account the
# harness's identity check then requires. It refuses to start when any of them is already set,
# rather than override the caller's.
#
# app-token prints an installation token of a GitHub App for its installation on a repository,
# minted from the App's private key file: a PEM block in a file only its owner may read (no group or
# other permission bits). Given a cache file, it reuses the token there while more than ten minutes
# of it are left, and writes a new one there (0600) otherwise. The key, the App's JWT and the token
# never enter an argv: curl reads the bearer from its stdin.
#
# human writes the proof human's GitHub credentials into <dir> (0700): `gh`, which runs the gh that
# was first on PATH with GH_TOKEN set to an installation token of the human's App on the repository,
# and `gitconfig`, whose credential helper answers git with the same token for https://github.com.
# Both mint through app-token with <dir>/token.json as the cache, so a run longer than a token's
# hour keeps working. The caller puts <dir> first on PATH and sets GIT_CONFIG_GLOBAL=<dir>/gitconfig.
#
# runtime-token <kubeconfig> <namespace> <name> and git-credential <app id> <key file> <owner/repo>
# <operation> [<cache file>] are what the kubeconfig and the gitconfig call; nothing else does.
#
# Every refusal names what it refused on stderr and exits 1.
set -euo pipefail

self=$(realpath -- "${BASH_SOURCE[0]}")
github_api=https://api.github.com

fail() {
  echo "proof-pod: $*" >&2
  exit 1
}

# ---- the pod's two identities ----------------------------------------------------------------------

kubeconfig() {
  [ "$#" = 1 ] || fail "usage: proof-pod.sh kubeconfig <file>"
  local file account namespace name host port sa_dir
  file=$(realpath -m -- "$1")
  account=${LEGION_PROOF_DAEMON_SERVICE_ACCOUNT:-}
  sa_dir=${LEGION_PROOF_SERVICE_ACCOUNT_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}
  host=${KUBERNETES_SERVICE_HOST:-}
  port=${KUBERNETES_SERVICE_PORT:-}
  [ -n "$account" ] ||
    fail "LEGION_PROOF_DAEMON_SERVICE_ACCOUNT is unset: set it to the Legion daemon's ServiceAccount, <namespace>/<name>, whose tokens the runtime context mints"
  [[ $account =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$ ]] ||
    fail "LEGION_PROOF_DAEMON_SERVICE_ACCOUNT=$account is not <namespace>/<name>"
  namespace=${account%/*}
  name=${account#*/}
  [ -n "$host" ] && [ -n "$port" ] ||
    fail "KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset: kubeconfig runs in a pod, which the cluster gives both"
  [ -r "$sa_dir/token" ] && [ -r "$sa_dir/ca.crt" ] ||
    fail "$sa_dir holds no readable token and ca.crt: the pod needs its ServiceAccount token mounted"
  case $host in *:*) host="[$host]" ;; esac
  (
    umask 077
    # shellcheck disable=SC2094  # $file is the exec plugin's argument, not a file jq reads
    jq -n --arg server "https://$host:$port" --arg ca "$sa_dir/ca.crt" --arg token "$sa_dir/token" \
      --arg bash "$(command -v bash)" --arg self "$self" --arg file "$file" \
      --arg namespace "$namespace" --arg name "$name" '{
        apiVersion: "v1",
        kind: "Config",
        clusters: [{name: "pod", cluster: {server: $server, "certificate-authority": $ca}}],
        users: [
          {name: "operator", user: {tokenFile: $token}},
          {name: "runtime", user: {exec: {
            apiVersion: "client.authentication.k8s.io/v1",
            command: $bash,
            args: [$self, "runtime-token", $file, $namespace, $name],
            interactiveMode: "Never"
          }}}
        ],
        contexts: [
          {name: "operator", context: {cluster: "pod", user: "operator"}},
          {name: "runtime", context: {cluster: "pod", user: "runtime"}}
        ],
        "current-context": "operator"
      }' >"$file"
  )
}

# runtime-token: the exec credential plugin of the runtime context. The operator context asks the
# TokenRequest API for a token of the daemon's ServiceAccount; the answer goes back as an
# ExecCredential with its expiry, so the client reuses it until then and runs this again after.
runtime_token() {
  [ "$#" = 3 ] || fail "usage: proof-pod.sh runtime-token <kubeconfig> <namespace> <name>"
  local answer
  answer=$(kubectl --kubeconfig "$1" --context operator -n "$2" create token "$3" --duration 1h -o json) ||
    fail "the operator context could not mint a token of ServiceAccount $2/$3 (kubectl's reason is above)"
  jq -e '.status.token and .status.expirationTimestamp' <<<"$answer" >/dev/null ||
    fail "the TokenRequest for $2/$3 answered no token and expiry"
  jq -c '{apiVersion: "client.authentication.k8s.io/v1", kind: "ExecCredential",
    status: {token: .status.token, expirationTimestamp: .status.expirationTimestamp}}' <<<"$answer"
}

run_stage() {
  [ "$#" -ge 1 ] || fail "usage: proof-pod.sh run <stage script> [<arg>...]"
  local script=$1 name dir
  shift
  [ -f "$script" ] || fail "$script is not a file"
  for name in KUBECONFIG LEGION_E2E_RUNTIME_KUBECONFIG LEGION_E2E_RUNTIME_CONTEXT LEGION_E2E_OPERATOR_CONTEXT \
    LEGION_E2E_RUNTIME_SERVICE_ACCOUNT; do
    [ -z "${!name:-}" ] || fail "$name is set: run sets it from the pod's own identities, so unset it"
  done
  dir=$(mktemp -d "${TMPDIR:-/tmp}/legion-proof-pod.XXXXXXXX")
  kubeconfig "$dir/kubeconfig"
  export KUBECONFIG=$dir/kubeconfig LEGION_E2E_RUNTIME_KUBECONFIG=$dir/kubeconfig \
    LEGION_E2E_RUNTIME_CONTEXT=runtime LEGION_E2E_OPERATOR_CONTEXT=operator \
    LEGION_E2E_RUNTIME_SERVICE_ACCOUNT=$LEGION_PROOF_DAEMON_SERVICE_ACCOUNT
  echo "proof-pod: $dir/kubeconfig: context operator is this pod's ServiceAccount, context runtime is $LEGION_PROOF_DAEMON_SERVICE_ACCOUNT through the TokenRequest API" >&2
  exec bash "$script" "$@"
}

# ---- GitHub App installation tokens ----------------------------------------------------------------

# app_inputs APP KEY REPO: refuses an App id that is not a number, a repository that is not
# owner/name, and a key file its group or others can read or that holds no PEM block.
app_inputs() {
  local app=$1 key=$2 repo=$3 mode first=''
  [[ $app =~ ^[0-9]+$ ]] || fail "App id '$app' is not a number"
  [[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "repository '$repo' is not <owner>/<name>"
  [ -f "$key" ] && [ -r "$key" ] || fail "App $app's key file $key is not a readable file"
  # The mode is the file openssl reads, so a symlink is followed: a link's own mode is always 777.
  mode=$(stat -L -c %a -- "$key")
  case $mode in
    *00) ;;
    *) fail "App $app's key file $key lets its group or others in (mode $mode): make it 0600" ;;
  esac
  IFS= read -r first <"$key" || true
  case $first in
    -----BEGIN*) ;;
    *) fail "App $app's key file $key holds no PEM block (its first line is not -----BEGIN …)" ;;
  esac
}

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# app_jwt APP KEY: the App's RS256 JWT, as the daemon mints it (internal/appauth/jwt.go): issued a
# minute back for clock skew, ten minutes in all.
app_jwt() {
  local now header claims signature
  now=$(date +%s)
  header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
  claims=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 540))" "$1" | b64url)
  signature=$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign "$2" -binary | b64url) ||
    fail "openssl could not sign App $1's JWT with $2"
  [ -n "$signature" ] || fail "openssl could not sign App $1's JWT with $2"
  printf '%s.%s.%s' "$header" "$claims" "$signature"
}

# github METHOD PATH BEARER: GitHub's answer on stdout. The bearer reaches curl on stdin, and an error
# answer's body is printed too, for the caller's message.
github() {
  printf 'header = "Authorization: Bearer %s"\n' "$3" |
    curl -sS --fail-with-body --max-time 30 --config - -X "$1" \
      -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "$github_api$2"
}

# mint APP KEY REPO: GitHub's access_tokens answer for the App's installation on the repository.
mint() {
  local jwt answer installation
  jwt=$(app_jwt "$1" "$2")
  answer=$(github GET "/repos/$3/installation" "$jwt") ||
    fail "GitHub refused App $1's installation lookup on $3: $(jq -r '.message // empty' <<<"$answer" 2>/dev/null)"
  installation=$(jq -r '.id // empty' <<<"$answer")
  [ -n "$installation" ] || fail "GitHub named no installation of App $1 on $3"
  answer=$(github POST "/app/installations/$installation/access_tokens" "$jwt") ||
    fail "GitHub refused a token for App $1's installation $installation: $(jq -r '.message // empty' <<<"$answer" 2>/dev/null)"
  jq -e '(.token | type == "string") and (.expires_at | type == "string")' <<<"$answer" >/dev/null ||
    fail "GitHub's answer for App $1's installation $installation carries no token and expiry"
  printf '%s\n' "$answer"
}

app_token() {
  [ "$#" = 3 ] || [ "$#" = 4 ] || fail "usage: proof-pod.sh app-token <app id> <key file> <owner/repo> [<cache file>]"
  local cache=${4:-} expires answer
  app_inputs "$1" "$2" "$3"
  if [ -n "$cache" ] && [ -s "$cache" ]; then
    expires=$(jq -r '.expires_at // empty' "$cache" 2>/dev/null) || expires=
    if [ -n "$expires" ] && [ "$(date -d "$expires" +%s)" -gt "$(($(date +%s) + 600))" ]; then
      jq -r .token "$cache"
      return 0
    fi
  fi
  answer=$(mint "$1" "$2" "$3")
  if [ -n "$cache" ]; then
    (umask 077 && jq '{token, expires_at}' <<<"$answer" >"$cache.$$" && mv -f -- "$cache.$$" "$cache")
  fi
  jq -r .token <<<"$answer"
}

# git-credential: the gitconfig's helper. It answers `get` for https://github.com with the App's
# token, and ignores `store` and `erase`: the token is minted, never kept by git.
git_credential() {
  [ "$#" = 4 ] || [ "$#" = 5 ] ||
    fail "usage: proof-pod.sh git-credential <app id> <key file> <owner/repo> <operation> [<cache file>]"
  local line protocol='' host='' token
  [ "$4" = get ] || return 0
  while IFS= read -r line && [ -n "$line" ]; do
    case $line in
      protocol=*) protocol=${line#protocol=} ;;
      host=*) host=${line#host=} ;;
    esac
  done
  [ "$protocol" = https ] && [ "$host" = github.com ] || return 0
  token=$(app_token "$1" "$2" "$3" ${5:+"$5"})
  printf 'username=x-access-token\npassword=%s\n' "$token"
}

human() {
  [ "$#" = 4 ] || fail "usage: proof-pod.sh human <dir> <app id> <key file> <owner/repo>"
  local dir app=$2 key repo=$4 gh
  app_inputs "$app" "$3" "$repo"
  key=$(realpath -- "$3")
  mkdir -p -- "$1"
  chmod 0700 -- "$1"
  dir=$(realpath -- "$1")
  gh=$(command -v gh) || fail "gh is not on PATH"
  gh=$(realpath -- "$gh")
  [ "${gh%/*}" != "$dir" ] || fail "the gh on PATH is $gh, this directory's own: take $dir off PATH first"
  # The key mints now, so a run learns of a wrong App or key before it relies on either.
  app_token "$app" "$key" "$repo" "$dir/token.json" >/dev/null
  {
    echo '#!/usr/bin/env bash'
    echo '# The proof human'"'"'s gh, written by scripts/e2e/lib/proof-pod.sh human.'
    # shellcheck disable=SC2016  # $token and $@ are the wrapper's own, expanded when it runs
    printf 'token=$(bash %q app-token %q %q %q %q) || exit 1\n' "$self" "$app" "$key" "$repo" "$dir/token.json"
    # shellcheck disable=SC2016
    printf 'GH_TOKEN=$token exec %q "$@"\n' "$gh"
  } >"$dir/gh"
  {
    echo '#!/usr/bin/env bash'
    echo '# The proof human'"'"'s git credential helper, written by scripts/e2e/lib/proof-pod.sh human.'
    # shellcheck disable=SC2016  # $1 is the operation git passes the helper
    printf 'exec bash %q git-credential %q %q %q "$1" %q\n' "$self" "$app" "$key" "$repo" "$dir/token.json"
  } >"$dir/git-credential"
  chmod 0700 -- "$dir/gh" "$dir/git-credential"
  printf '[credential "https://github.com"]\n\thelper = %s\n' "$dir/git-credential" >"$dir/gitconfig"
  echo "proof-pod: the proof human's gh and git credential helper, App $app on $repo, are in $dir" >&2
}

[ "$#" -ge 1 ] || fail "usage: proof-pod.sh run|kubeconfig|app-token|human … (the header names each)"
subcommand=$1
shift
case $subcommand in
  run) run_stage "$@" ;;
  kubeconfig) kubeconfig "$@" ;;
  runtime-token) runtime_token "$@" ;;
  app-token) app_token "$@" ;;
  git-credential) git_credential "$@" ;;
  human) human "$@" ;;
  *) fail "unknown command '$subcommand': run, kubeconfig, app-token or human" ;;
esac
