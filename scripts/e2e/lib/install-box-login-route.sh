#!/usr/bin/env bash
# Routes a named OMP profile's model turns to Anthropic on the operator's own Oh My Pi login, the
# one the devbox's own sessions already use: the box-login alternative to
# lib/install-model-gateway.sh's Hawk route and lib/install-key-file-model-route.sh's key file, for
# a devbox whose operator lets Legion's test runs use that login. No key is minted, read or copied,
# and no model gateway is involved.
#
#   scripts/e2e/lib/install-box-login-route.sh --profile <name> --home <dir> --dest <dir> --operator-home <dir>
#
# --home is the HOME Oh My Pi runs under for the profile, made by make_omp_home (lib/omp-home.sh):
# the profile's files go under <home>/.omp/profiles/<name>, and the caller's own HOME is refused.
# --profile must be one plain name, and the agent directory must canonicalise to that very path, so
# the link to the operator's login can never land outside the run's home.
#
# --operator-home is the operator's real home, given explicitly: the caller captures it before it
# moves HOME anywhere, and this script never reads the login's location from its own $HOME.
#
# How it works. Oh My Pi keeps its stored logins in <agentDir>/agent.db (getAgentDbPath,
# @oh-my-pi/pi-utils dirs.ts); a named profile's agentDir is <home>/.omp/profiles/<name>/agent,
# and there is no separate setting that moves the database. So the profile's agent.db is made a
# symlink to <operator-home>/.omp/agent/agent.db. SQLite opens the database through the link and
# writes its -wal and -shm beside the real file, so every pane shares the operator's one database
# rather than a copy of it. This script only checks that the operator's agent.db exists as a
# regular file and links to it: it never opens, reads, copies or hashes it, and runs no `omp`
# command that touches a login.
#
# The database also holds Oh My Pi's usage tables and its memory store; the profile's config.yml
# turns memory off (memory.backend), so the run writes no memory into the operator's database.
#
# The Oh My Pi build. Processes that share one agent.db refresh its OAuth token safely only on a
# build that carries the refresh-race fix, first released as 18.12.2-sami.20261026-004924. A build
# without it can spend the shared refresh token in one process while another still holds the old
# one, which disables the login for every session on the box. So the panes must run the operator's
# own build, the one the operator's sessions already run against this database, not the
# repository's .omp-pin (which also feeds the worker image and CI, and is not moved for this). The
# script reads the operator's global mise pin for github:sjawhar/oh-my-pi with
#   cd / && HOME=<operator-home> mise ls --global --json
# from the filesystem root, so no project's mise config (this checkout's or the caller's working
# directory's) can answer instead of the operator's global one, and refuses it unless it is
# 18.12.2-sami.<stamp> with a build stamp at or after the fix's. Any other base version is refused
# rather than guessed at; raise $fix_base and $fix_stamp below once a later base is known to carry
# the fix.
#
# Stdout is one line, the mise tool spec of that build (github:sjawhar/oh-my-pi@<version>), which
# the caller writes into legion.yaml's omp_invocation. Every refusal goes to stderr. It writes:
#   <dest>/box-login.route   what the run used, for the evidence: the link, the database it names
#                            (a path, never any content) and the build. No secret.
#   the profile's agent/config.yml (see the heredoc below) and the agent/agent.db symlink
# No models.yml is written, so Anthropic keeps Oh My Pi's own endpoint and the stored login.
set -euo pipefail

me=install-box-login-route
model=anthropic/claude-opus-4-8
tool=github:sjawhar/oh-my-pi
fix_base=18.12.2
fix_stamp=20261026-004924

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
operator_home=
have_profile=
have_home=
have_dest=
have_operator_home=
while [ $# -gt 0 ]; do
  case $1 in
  --profile)
    [ $# -ge 2 ] || refuse "--profile needs a value: the OMP profile to route"
    profile=$2 have_profile=1
    shift 2
    ;;
  --home)
    [ $# -ge 2 ] || refuse "--home needs a value: the HOME Oh My Pi runs under for the profile"
    home=$2 have_home=1
    shift 2
    ;;
  --dest)
    [ $# -ge 2 ] || refuse "--dest needs a value: the directory the route's record is written to"
    dest=$2 have_dest=1
    shift 2
    ;;
  --operator-home)
    [ $# -ge 2 ] || refuse "--operator-home needs a value: the operator's real home, captured before HOME moved"
    operator_home=$2 have_operator_home=1
    shift 2
    ;;
  *) refuse "unknown argument $1" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the route's record is written to"
[ -n "$have_operator_home" ] || refuse "--operator-home is required: the operator's real home, captured before HOME moved"

# OMP reads an empty name or "default" as the profile every plain `omp` uses (the same rule as
# install-model-gateway.sh and install-plugin-profile.sh).
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi
# The name becomes one directory under <home>/.omp/profiles, and this script writes a link to the
# operator's login there: one plain path segment, never a path that could leave the run's home.
profile_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
if [[ ! $profile =~ $profile_re ]] || [[ $profile == *..* ]]; then
  refuse "--profile '$profile' is not one plain name (a letter or digit, then letters, digits, '.', '_' or '-', and no '..'); the profile must stay a directory under --home's .omp/profiles"
fi
[ -n "$operator_home" ] || refuse "--operator-home is empty"
operator_home=$(realpath -m -- "$operator_home")
# The profile goes under the run's own home, never the caller's or the operator's.
[ -n "${HOME:-}" ] || refuse "HOME is unset or empty, so --home cannot be checked against it"
home=$(realpath -m -- "$home")
[ "$home" != "$(realpath -m -- "$HOME")" ] || refuse "--home $home is your own HOME; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ "$home" != "$operator_home" ] || refuse "--home $home is --operator-home; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ -d "$home/.omp" ] || refuse "--home $home has no .omp directory; make it with make_omp_home (lib/omp-home.sh)"
agent=$home/.omp/profiles/$profile/agent
# No symlinked directory on the way may carry the profile, and the link with it, out of the run's
# home: the canonical agent directory must be this very path.
[ "$(realpath -m -- "$agent")" = "$agent" ] ||
  refuse "$agent resolves to $(realpath -m -- "$agent"), outside $home/.omp/profiles/$profile; the profile must be a plain directory under --home"
for file in models.yml config.yml; do
  [ ! -e "$agent/$file" ] || refuse "$agent/$file exists; this installs a fresh profile's model route only"
done

# The operator's login: its database must exist as a regular file. Only its type is checked.
operator_db=$operator_home/.omp/agent/agent.db
[ ! -L "$operator_db" ] || fail "$operator_db is a symlink; the box-login route links only to the operator's own database file"
[ -f "$operator_db" ] || fail "$operator_db is not a regular file: the operator has no Oh My Pi login on this box to share (log in with the operator's own omp first)"
link=$agent/agent.db
if [ -L "$link" ]; then
  [ "$(readlink -- "$link")" = "$operator_db" ] || refuse "$link is a symlink to $(readlink -- "$link"), not to $operator_db; this installs a fresh profile's model route only"
elif [ -e "$link" ]; then
  refuse "$link exists and is not the symlink to $operator_db; this installs a fresh profile's model route only, and never replaces a profile's own database"
fi

# The operator's own Oh My Pi build, from the operator's global mise config alone.
listing=$(cd / && HOME=$operator_home mise ls --global --json) ||
  fail "\`mise ls --global --json\` failed under HOME=$operator_home, so the operator's own Oh My Pi build cannot be read"
version=$(jq -r --arg tool "$tool" '(.[$tool] // []) | map(select(.active != false)) | if length == 1 then .[0].version // "" else "" end' <<<"$listing") ||
  fail "mise's global listing is not the JSON \`mise ls --global --json\` prints, so the operator's own Oh My Pi build cannot be read"
[ -n "$version" ] && [ "$version" != null ] ||
  fail "the operator's global mise config pins no single active $tool, so the box-login route has no operator build to run (pin it in the operator's global mise config)"
stamp_re='^([0-9]+\.[0-9]+\.[0-9]+)-sami\.([0-9]{8}-[0-9]{6})$'
[[ $version =~ $stamp_re ]] ||
  fail "the operator's $tool is $version, not <base>-sami.YYYYMMDD-HHMMSS, so it cannot be compared with the refresh-race fix ($fix_base-sami.$fix_stamp)"
base=${BASH_REMATCH[1]}
stamp=${BASH_REMATCH[2]}
[ "$base" = "$fix_base" ] ||
  fail "the operator's $tool is $version, base $base; only base $fix_base is known here, where the refresh-race fix is $fix_base-sami.$fix_stamp; raise fix_base in $0 once base $base is known to carry it"
[[ ! $stamp < $fix_stamp ]] ||
  fail "the operator's $tool is $version, older than $fix_base-sami.$fix_stamp, the first build with the refresh-race fix; panes sharing the operator's login on it could disable that login for every session on the box"
spec=$tool@$version

dest=$(realpath -m -- "$dest")
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
fi
mkdir -p "$dest"
chmod 0700 "$dest"

mkdir -p "$agent"
cat >"$agent/config.yml" <<EOF
# Written by scripts/e2e/lib/install-box-login-route.sh. The operator's own Oh My Pi login, through
# agent.db (a symlink to the operator's database), is the one model route: no models.yml overrides
# Anthropic's endpoint or key, so Oh My Pi uses the login stored there. Oh My Pi falls back from a
# failing provider without a word (install-model-gateway.sh measured a pane answering from Bedrock
# on the devbox's instance role), so enabledModels holds every session's own model to anthropic and
# every provider a pane could use without that login is disabled, as the key-file route does: Bedrock
# twice (the instance role is its key), Google (Stage 2's provider-key path hands panes
# GEMINI_API_KEY), and the local servers OMP uses with no key. Every role is the one model; the
# daemon's boot gate refuses to start without the roles Legion's task agents name. Memory is off,
# so the run writes no memory into the operator's database.
enabledModels:
  - $model
disabledProviders:
  - amazon-bedrock
  - bedrock-mantle
  - google
  - ollama
  - llama.cpp
  - lm-studio
memory:
  backend: "off"
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
[ -L "$link" ] || ln -s -- "$operator_db" "$link"
{
  printf 'route\tbox-login: the operator'"'"'s own Oh My Pi login, never a key\n'
  printf 'link\t%s\n' "$link"
  printf 'database\t%s\n' "$operator_db"
  printf 'omp\t%s\n' "$spec"
} >"$dest/box-login.route"
echo "$me: OMP profile $profile routes $model to Anthropic on the operator's own login ($link -> $operator_db), on the operator's build $spec" >&2
printf '%s\n' "$spec"
