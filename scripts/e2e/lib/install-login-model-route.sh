#!/usr/bin/env bash
# Routes a named OMP profile's model turns to Anthropic on the operator's own Oh My Pi login: the
# login alternative to lib/install-model-gateway.sh's Hawk route and lib/install-key-file-model-route.sh's
# key file, for a devbox whose Oh My Pi is signed in to Anthropic and that has neither a gateway nor
# a key to hand the run.
#
#   scripts/e2e/lib/install-login-model-route.sh --profile <name> --home <dir> --dest <dir> --omp <absolute executable> --run-minutes <n>
#
# Oh My Pi keeps its logins in its agent.db, beside its settings and caches, and a login's refresh
# token rotates when one process refreshes it. A copy of the login in a second database would
# refresh on its own, and the first refresh in either would leave the other holding a spent refresh
# token: the operator's own sessions signed out. So the profile's agent.db is a symbolic link to the
# operator's own, the one every Oh My Pi session of the operator already shares. SQLite resolves the
# link, so the run's agents and the operator's sessions share one database, its write-ahead log and
# Oh My Pi's refresh leases, as two of the operator's sessions do. The run's agents also write their
# model usage and caches into it. The operator's database is found as Oh My Pi finds it
# (@oh-my-pi/pi-utils dirs.ts, getAgentDbPath): the profile OMP_PROFILE names when it is set at all,
# else PI_PROFILE, else the default one, under $XDG_DATA_HOME/omp when that directory exists, else
# under $HOME/.omp; PI_CONFIG_DIR and PI_CODING_AGENT_DIR, which move it, are refused. Run it under
# the operator's own HOME, before the caller moves an XDG directory of its own, as
# lib/install-model-gateway.sh is run.
#
# Sharing the database has two refusals. An --omp newer than the operator's own omp (the first on
# PATH) would migrate the database's schema under the operator's sessions, so it is refused. And an
# Oh My Pi older than the operator's (the pin, 18.8.9, against 18.12's fix) can lose a login's
# rotated refresh token when its process is killed mid-exchange, as a run's teardown and the
# preflight's timeout kill, which signs every session of the operator out. Oh My Pi refreshes a login
# only within 60 s of its stored expiry or on a 401, so every live anthropic login must have more than
# --run-minutes left; the run's agents then never refresh one for its expiry.
#
# No credential reaches a pane's environment, an argv or the profile's files: the profile holds the
# link, and Oh My Pi reads the login from the database itself. A pane runs as the operator's own uid,
# though, so an agent's own bash can still read that database, as it can read hawk-token's cache or
# the key-file route's key file: the same exposure, not a widening.
#
# The preflight is one `omp -p` under the profile, from a scratch directory, with no extensions, under
# an allow-list modelled on the daemon's pane allow-list (the run's HOME and profile, and nothing else
# of the caller's session; below): it passes when the main session it writes holds an answer from the
# route's model on the anthropic provider. A login Anthropic refuses fails here, before any pane
# exists. --dest, which holds its log, is the caller's to keep or remove.
#
# Stdout is one line, the operator's agent.db the profile now links to; every refusal goes to
# stderr. It writes the profile's agent/config.yml (below) and agent/agent.db (the link), and no
# models.yml: Anthropic's own endpoint is Oh My Pi's default.
set -euo pipefail

me=install-login-model-route
model=anthropic/claude-opus-4-8

# refuse is for arguments (exit 2); fail is for the box the run is on (exit 1).
refuse() {
  echo "$me: $*" >&2
  exit 2
}
fail() {
  echo "$me: $*" >&2
  exit 1
}

profile=
home=
dest=
omp=
run_minutes=
have_profile=
have_home=
have_dest=
have_omp=
while [ $# -gt 0 ]; do
  case $1 in
  --profile)
    profile=$2
    have_profile=1
    shift 2
    ;;
  --home)
    home=$2
    have_home=1
    shift 2
    ;;
  --dest)
    dest=$2
    have_dest=1
    shift 2
    ;;
  --omp)
    omp=$2
    have_omp=1
    shift 2
    ;;
  --run-minutes)
    run_minutes=$2
    shift 2
    ;;
  *) refuse "unknown argument $1" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the preflight's output is written to"
[ -n "$have_omp" ] && [ -n "$omp" ] || refuse "--omp is required: the mise tool spec of the Oh My Pi the panes run"
[[ $run_minutes =~ ^[0-9]+$ ]] || refuse "--run-minutes is required: the longest the caller's run lasts, in whole minutes"

# OMP reads an empty name or "default" as the profile every plain `omp` uses (the same rule as
# install-model-gateway.sh and install-plugin-profile.sh).
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi
# The profile goes under the run's own home, never the caller's (the same rule as
# install-model-gateway.sh and install-plugin-profile.sh).
[ -n "${HOME:-}" ] || refuse "HOME is unset or empty, so --home cannot be checked against it"
home=$(realpath -m -- "$home")
[ "$home" != "$(realpath -m -- "$HOME")" ] || refuse "--home $home is your own HOME; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ -d "$home/.omp" ] || refuse "--home $home has no .omp directory; make it with make_omp_home (lib/omp-home.sh)"
agent=$home/.omp/profiles/$profile/agent
for file in models.yml config.yml agent.db; do
  [ ! -e "$agent/$file" ] && [ ! -L "$agent/$file" ] || refuse "$agent/$file exists; this installs a fresh profile's model route only"
done
command -v bun >/dev/null || fail "bun is not on PATH: it reads the logins in the operator's Oh My Pi database"
command -v mise >/dev/null || fail "mise is not on PATH: the preflight runs --omp through it"

# The operator's own database, where Oh My Pi itself looks for it: OMP_PROFILE names the profile when
# it is set at all (an empty one names the default profile), else PI_PROFILE.
if [ -n "${OMP_PROFILE+set}" ]; then login_profile=$OMP_PROFILE; else login_profile=${PI_PROFILE:-}; fi
login_profile=${login_profile#"${login_profile%%[![:space:]]*}"}
login_profile=${login_profile%"${login_profile##*[![:space:]]}"}
[ "$login_profile" != default ] || login_profile=
[ -z "${PI_CONFIG_DIR:-}" ] && [ -z "${PI_CODING_AGENT_DIR:-}" ] ||
  fail "PI_CONFIG_DIR or PI_CODING_AGENT_DIR is set, which moves the operator's Oh My Pi database somewhere this installer does not look (~/.omp, or \$XDG_DATA_HOME/omp): unset it, or use another model route"
if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp${login_profile:+/profiles/$login_profile}" ]; then
  login_db=$XDG_DATA_HOME/omp${login_profile:+/profiles/$login_profile}/agent.db
else
  login_db=$HOME/.omp${login_profile:+/profiles/$login_profile}/agent/agent.db
fi
[ -f "$login_db" ] || fail "the operator's Oh My Pi has no agent.db at $login_db, so it holds no login: sign in with omp's /login, or use another model route"

# The route shares the operator's database. --omp must therefore be the same release as the
# operator's own omp (the first on PATH).
# Sharing the database is safe only when --omp and the operator's own omp (the first on PATH) report
# the same release, the leading major.minor.patch of `omp --version`: an older client can lose a
# rotated refresh token when a run stops it mid-exchange (a 401 can force an exchange at any expiry),
# and a newer one can migrate the database under the operator's sessions.
version_of() { "$@" --version 2>/dev/null | sed -n 's|^omp/\([0-9][0-9.]*\).*|\1|p' | sed -n 1p; }
operator_version=$(version_of omp) || operator_version=
pin_version=$(version_of mise x "$omp" -- omp) || pin_version=
[ -n "$operator_version" ] || fail "\`omp --version\` printed no version: the operator's own Oh My Pi, which --omp is compared with, must be first on PATH"
[ -n "$pin_version" ] || fail "\`mise x $omp -- omp --version\` printed no version"
[ "$pin_version" = "$operator_version" ] ||
  fail "--omp runs Oh My Pi $pin_version, not the operator's own $operator_version: the login route shares the operator's database, so every process must run the same release; make omp_invocation the operator's executable, or use another model route"

# Every provider the operator's database holds a live login for, and the minutes left on each live
# anthropic OAuth login. Each provider but anthropic is disabled in the profile, since a subagent or
# a retry chooses from every provider it can reach.
# shellcheck disable=SC2016 # the program is JavaScript, whose ${…} is its own template literal
logins=$(AUTH_DB=$login_db bun -e '
  import { Database } from "bun:sqlite";
  const db = new Database(process.env.AUTH_DB, { readonly: true });
  const rows = db.query("SELECT provider, credential_type, data FROM auth_credentials WHERE disabled_cause IS NULL ORDER BY provider").all();
  for (const row of rows) {
    let minutes = "-";
    if (row.provider === "anthropic" && row.credential_type === "oauth") {
      const expires = Number(JSON.parse(row.data).expires);
      minutes = Number.isFinite(expires) ? String(Math.floor((expires - Date.now()) / 60000)) : "unknown";
    }
    console.log(`${row.provider}\t${minutes}`);
  }
  db.close();
') || fail "could not read the logins in $login_db (bun's reason is above)"
grep -q $'^anthropic\t' <<<"$logins" ||
  fail "the operator's Oh My Pi is not signed in to anthropic in $login_db: sign in with omp's /login, or use another model route"
# A refresh rotates the login's refresh token, and an Oh My Pi older than the operator's (the pin
# today) can lose the rotated one when its process is killed mid-exchange, as a run's teardown and the
# preflight's timeout do, signing every session of the operator out. Oh My Pi refreshes a login only
# within 60 s of its stored expiry, or on a 401, so a login with --run-minutes left is never refreshed
# for its expiry by the run's agents.
while IFS=$'\t' read -r provider minutes; do
  [ "$provider" = anthropic ] && [ "$minutes" != - ] || continue
  [[ $minutes =~ ^-?[0-9]+$ ]] && [ "$minutes" -gt "$run_minutes" ] ||
    fail "the operator's anthropic login in $login_db expires in ${minutes} minutes, within the run's $run_minutes: refresh it first with your own omp (\`omp token anthropic --force-refresh\`), then run again"
done <<<"$logins"
disabled=(amazon-bedrock bedrock-mantle google ollama llama.cpp lm-studio)
while IFS=$'\t' read -r provider _; do
  case $provider in
  anthropic | mcp_oauth:* | "") ;;
  *) [[ " ${disabled[*]} " == *" $provider "* ]] || disabled+=("$provider") ;;
  esac
done <<<"$logins"

dest=$(realpath -m -- "$dest")
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
fi
mkdir -p "$dest" "$agent"
chmod 0700 "$dest"
ln -s -- "$login_db" "$agent/agent.db"
{
  cat <<EOF
# Written by scripts/e2e/lib/install-login-model-route.sh. Anthropic's own endpoint on the operator's
# Oh My Pi login (agent.db, linked to the operator's own) is the one model route, and Oh My Pi falls
# back from a failing provider without a word: install-model-gateway.sh measured a pane answering
# from amazon-bedrock/us.anthropic.claude-opus-4-8 on the devbox's instance role, and a Stage 3
# retro's scout subagent on Bedrock's openai.gpt-oss-120b. enabledModels holds every session's own
# model to anthropic. Subagents and retries choose from every enabled provider instead, so each one a
# pane could reach without this login is disabled: Bedrock twice (the instance role is its key),
# Google (Stage 2's provider-key path hands panes GEMINI_API_KEY), the local servers Oh My Pi uses
# with no key, and every other provider the shared database holds a login for. Every role is the one
# model. The daemon's boot gate refuses to start without the roles Legion's task agents name.
enabledModels:
  - $model
disabledProviders:
EOF
  printf '  - %s\n' "${disabled[@]}"
  cat <<EOF
modelRoles:
  default: $model
  smol: $model
  slow: $model
  vision: $model
  plan: $model
  commit: $model
  tiny: $model
  task: $model
  advisor: $model
  review: $model
  oracle: $model
  deep: $model
EOF
} >"$agent/config.yml"

# The preflight: one answer from $model, served by the anthropic provider, in the main session it
# records (a subagent's or a sidecar's session sits a level deeper). It runs under an allow-list of
# its own, modelled on the daemon's pane allow-list (packages/daemon/internal/runtime/tmux/
# environment.go, paneEnvAllowList), with the run's HOME and profile, so no overlay
# (PI_CONFIG_FILES), provider key or other variable of the caller's own session reaches it. Unlike a
# pane's, its XDG base directories are the caller's own, where `mise x` reads which configs it
# trusts; the run's profile name is fresh, so Oh My Pi still finds the profile under --home.
scratch=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT
touch "$scratch/started"
pane_env=()
for name in PATH USER LOGNAME SHELL TERM TZ LANG LANGUAGE LC_ALL LC_CTYPE TMPDIR XDG_CONFIG_HOME XDG_CACHE_HOME \
  XDG_DATA_HOME XDG_STATE_HOME XDG_RUNTIME_DIR MISE_CACHE_DIR MISE_CONFIG_DIR MISE_DATA_DIR MISE_STATE_DIR SECRETSD_SOCK \
  HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy SSL_CERT_DIR SSL_CERT_FILE NODE_EXTRA_CA_CERTS; do
  [ -z "${!name+set}" ] || pane_env+=("$name=${!name}")
done
log=$dest/omp-preflight.log
status=0
(cd "$scratch" && env -i "${pane_env[@]}" HOME="$home" OMP_PROFILE="$profile" \
  timeout 300 mise x "$omp" -- omp -p --no-extensions --model "$model" "Reply with exactly: OK" >"$log" 2>&1) || status=$?
said=$(tail -n 5 "$log" | tr '\n' ' ')
[ "$status" = 0 ] || fail "the preflight \`omp -p\` on the operator's login exited $status: $said(log: $log)"
served=
while IFS= read -r session; do
  served=$(jq -R -r --arg want "${model#anthropic/}" 'fromjson? | select(type == "object" and .type == "message"
    and (.message | type) == "object" and .message.role == "assistant" and .message.provider == "anthropic"
    and .message.model == $want and .message.stopReason != "error" and .message.stopReason != "aborted") | .message.model' \
    "$session" 2>/dev/null | sed -n 1p || true)
  [ -z "$served" ] || break
done < <(find "$agent/sessions" -mindepth 2 -maxdepth 2 -type f -name '*.jsonl' -newer "$scratch/started" 2>/dev/null)
[ -n "$served" ] || fail "the preflight \`omp -p\` exited 0 but its session holds no answer from $model on the anthropic provider: $said(log: $log)"
echo "$me: OMP profile $profile routes $model to Anthropic on the operator's Oh My Pi login in $login_db (Oh My Pi $pin_version beside the operator's $operator_version; preflight answered); disabled: ${disabled[*]}" >&2
printf '%s\n' "$login_db"
