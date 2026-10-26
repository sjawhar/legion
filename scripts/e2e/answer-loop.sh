#!/usr/bin/env bash
# The answer loop's live proof (INFRA-204; scripts/e2e/README.md, "answer-loop.sh"): the branch's
# daemon, Envoy listener and Dispatch, run on this host with real Anthropic models, under both of a
# tmux daemon's conversation stores (runtime.tmux.session_store files, then postgres), with stopped
# agents whose real conversations hold the decisions scripts/e2e/answer-loop/questions.json names.
# Every acceptance criterion of the plan's table (A1-A10, A12-A19) is one check that prints its
# evidence and a negative control; the summary lists each as PASS, FAIL or WAITING with what it
# waits on. It runs in a Legion issue pod (no docker, no root, no psql, no tmux) and on a devbox.
#
#   bash scripts/e2e/answer-loop.sh
#
# Inputs (environment):
#   ANTHROPIC_API_KEY            required: the key every model call of the run carries, written once
#                                to a 0600 file in the run's work directory and never echoed; the
#                                agents' key command, Claude Code's apiKeyHelper and the daemon's
#                                answers.providers.anthropic.api_key_file all read that file.
#   LEGION_E2E_IMPLEMENT_APP_ID, LEGION_E2E_IMPLEMENT_APP_KEY_NAME
#   LEGION_E2E_REVIEW_APP_ID, LEGION_E2E_REVIEW_APP_KEY_NAME
#                                Legion's two role Apps (lib/github-apps.sh; defaults are this
#                                repository's own deployment's). The implement App is proved
#                                installed on the smoke repository before anything else.
#   LEGION_E2E_REVIEW_APP_GRANTED=1
#                                says a person has granted the review App's key to this session. On
#                                a host whose secrets CLI is `agent-secrets` the run never asks for
#                                that key otherwise (asking files a request in a person's inbox, and
#                                agent-secrets cannot tell a grant without asking), and the daemon
#                                then runs without its workflow: the answer loop is part of the
#                                workflow, which boots only with two distinct Apps, so every
#                                answering criterion waits on the grant. Told so and refused, the
#                                run cancels its request and stops. On a devbox (`secrets`), the
#                                key's tier decides, as Stage 3's prerequisites read it.
#   LEGION_E2E_REPO_SECOND_OWNER  optional: owner/name of a repository of another owner both Apps are
#                                installed on (A17's refusal); never written in this tree.
#   LEGION_E2E_BASE_REV           the base commit A13 compares against (default main@origin).
#   LEGION_E2E_OMP_BIN            the Oh My Pi binary (default /opt/omp/bin/omp, else the .omp-pin
#                                mise resolves).
#   LEGION_E2E_ANSWER_LOOP_EVIDENCE_DIR, LEGION_E2E_ANSWER_LOOP_CACHE
#                                where the evidence goes (default a new directory under $TMPDIR) and
#                                where the pinned downloads and the base commit's build are kept
#                                between runs (default $TMPDIR/legion-e2eal-cache).
#   LEGION_E2E_ANSWER_LOOP_ONLY, LEGION_E2E_ANSWER_LOOP_STORES
#                                narrow a run while a check is fixed: the criteria to run (e.g.
#                                "boot A6"; default all) and the stores (default "files postgres").
#                                The bring-up and the stopped agents are made as always.
#
# Everything the run starts is its own and stopped by exact pid on exit: Postgres (the zonky
# embedded-postgres binaries with Debian's pgvector), nats-server, the recording proxy, the Envoy
# inbox, the listener, Dispatch, each daemon and its private tmux server. The run's GitHub writes
# are on the smoke repository (A17, A18, A19's pull requests, comments and reviews, each recorded by
# id and closed or deleted on exit) and A17's refused write on the second owner's repository.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tmp_root=${TMPDIR:-/tmp}
tmp_root=${tmp_root%/}
work=$(mktemp -d "$tmp_root/legion-e2eal.$$.XXXXXXXX")
evidence=${LEGION_E2E_ANSWER_LOOP_EVIDENCE_DIR:-$(mktemp -d "$tmp_root/legion-e2eal-evidence.XXXXXXXX")}
cache=${LEGION_E2E_ANSWER_LOOP_CACHE:-$tmp_root/legion-e2eal-cache}
mkdir -p "$evidence/logs" "$evidence/a13" "$evidence/sessions" "$cache"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

# ---- pins ------------------------------------------------------------------------------------------
# Postgres 17 from the zonky embedded-postgres jar on Maven Central, with Debian trixie's pgvector
# for 17 copied into it (Dispatch's migrations create the vector extension).
pg_version=17.6.0
pg_jar_url=https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/$pg_version/embedded-postgres-binaries-linux-amd64-$pg_version.jar
pg_jar_sha256=23da5a044b4fb7a5a081a45008c95749c873305328d73f86aefd56922ce1d29d
debian_pool=https://deb.debian.org/debian/pool/main
pgvector_deb=p/pgvector/postgresql-17-pgvector_0.8.0-1_amd64.deb
pgvector_sha256=6e8a457eb252617b66cf7b2908c736127eae43703fffec94492ac131ca921515
# tmux and the libraries it links, unpacked with dpkg-deb when the host has no tmux.
tmux_debs=(
  "t/tmux/tmux_3.5a-3_amd64.deb e6b6aa51780044e2c4a0e618c33f832b70ec80caa30303e6c254a0c851b5af78"
  "libe/libevent/libevent-core-2.1-7t64_2.1.13-stable-1~deb13u1_amd64.deb faa7d20cc8fa05ac3b0497065fa60bc920eacd3711ff039649d43a7aa34421ce"
  "libu/libutempter/libutempter0_1.2.1-5_amd64.deb 92872367e010acf25765c18006f40317606628739ace4187c37e8230419b8d20"
  "j/jemalloc/libjemalloc2_5.3.1-2_amd64.deb 115b797f5a25d70cbe81373c9f1cce78536ed2a8a6d9dd5a061682a2340a25df"
)
# nats-server at packages/envoy/go.mod's own pin, installed with `go install` so its pid is its own.
nats_version=v2.10.29
# Claude Code at .github/actions/install-claude-code's pin.
claude_code_version=2.1.310
# Anthropic's list prices, USD per million tokens: input, output, cache write (5 minutes), cache read.
answers_prices='    anthropic/claude-opus-5-5: { input: 5, output: 25, cache_write: 6.25, cache_read: 0.5 }
    anthropic/claude-sonnet-5-5: { input: 3, output: 15, cache_write: 3.75, cache_read: 0.3 }'
repo=sjawhar/legion-smoke
questions_file=$root/scripts/e2e/answer-loop/questions.json

# ---- run state -------------------------------------------------------------------------------------
ok=
check=setup
timeout_hook=
mode=claims-only
base_rev=${LEGION_E2E_BASE_REV:-main@origin}
second_repo=${LEGION_E2E_REPO_SECOND_OWNER:-}
# LEGION_E2E_ANSWER_LOOP_ONLY (criteria, e.g. "boot A1 A6") and LEGION_E2E_ANSWER_LOOP_STORES
# (default "files postgres") narrow a run while a check is being fixed: the bring-up and the stopped
# agents are made as always, and only the named checks run. A run that proves the criteria sets
# neither.
only=${LEGION_E2E_ANSWER_LOOP_ONLY:-}
only=" ${only//,/ } "
stores=${LEGION_E2E_ANSWER_LOOP_STORES:-files postgres}
wanted() { [ "$only" = "  " ] || [[ "$only" == *" $1 "* ]]; }
run_tag=$(((($$ + $(date +%s)) % 90000) + 10000))
proof_human_user=smoke
dispatch_actor=
dispatch_base=
project=
pg_pid=
nats_pid=
proxy_pid=
inbox_pid=
asker_heartbeat_pid=
listener_pid=
dispatch_pid=
daemon_pid=
port_pg=
port_nats=
port_proxy=
port_inbox=
port_listener=
port_dispatch=
port_daemon=
store=
# The daemon under test in the current step: its binary, config, log, HOME and profile.
daemon_bin=
daemon_config=
daemon_log=
daemon_home=
daemon_profile=
daemon_plugin_dir=
github_writes=()
results=()
declare -A agent=()

begin() { check=$1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# Nothing the run starts inherits the operator's Dispatch bearer.
unset DISPATCH_TOKEN

# pick_port VAR replaces lib/rig.sh's: lib/free-port.sh asks `ss` for listeners and needs the kernel's
# ephemeral range to start above 21000, and a Legion pod has no ss and a range from 16000. This picks
# below the range as free-port.sh does, from 10000, and reads the listeners from /proc/net/tcp and
# tcp6 (state 0A), never one this run already picked.
picked_ports=
pick_port() {
  local _low _port _hex _busy
  read -r _low _ </proc/sys/net/ipv4/ip_local_port_range
  [ "$_low" -gt 11000 ] || fail "the ephemeral port range starts at $_low, leaving no room above 10000"
  for _ in $(seq 1 100); do
    _port=$((10000 + RANDOM % (_low - 10000)))
    case " $picked_ports " in *" $_port "*) continue ;; esac
    printf -v _hex '%04X' "$_port"
    _busy=$(awk -v port=":$_hex" '$4 == "0A" && substr($2, length($2) - 4) == port' /proc/net/tcp /proc/net/tcp6 2>/dev/null || true)
    [ -z "$_busy" ] || continue
    picked_ports="$picked_ports $_port"
    printf -v "$1" '%s' "$_port"
    return 0
  done
  fail "no free port for $1"
}

# record_result CRITERION VERDICT DETAIL: one summary row, for the current store ("-" for a
# criterion that does not run per store). VERDICT is PASS, FAIL or WAITING; DETAIL says why.
record_result() {
  results+=("${store:--}|$1|$2|$3")
  printf '%s %s (%s): %s\n' "$2" "$1" "${store:--}" "$3"
}

# ---- cleanup ---------------------------------------------------------------------------------------
# github_cleanup closes each pull request (and deletes its branch) and deletes each comment a check
# made on the smoke repository, by the id it recorded (`pr:<number>:<branch>`, `comment:<id>`), as
# the implement App that made them (lib/smoke-pull-request.ts), so nothing the run made outlives it.
github_cleanup() {
  local entry kind id
  for entry in "${github_writes[@]}"; do
    kind=${entry%%:*}
    id=${entry#*:}
    case "$kind" in
      pr) timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" close "$work/github-apps.yaml" "$repo" "${id%%:*}" "${id#*:}" >&2 ||
        printf 'could not close %s pull request #%s or delete its branch\n' "$repo" "${id%%:*}" >&2 ;;
      comment) timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" delete-comment "$work/github-apps.yaml" "$repo" "$id" >&2 ||
        printf 'could not delete %s comment %s\n' "$repo" "$id" >&2 ;;
    esac
  done
}

kill_tmux_servers() {
  local socket
  for socket in "$work"/tmux/tmux-*/*; do
    [ -S "$socket" ] || continue
    TMUX_TMPDIR="$work/tmux" "$tmux_bin" -S "$socket" kill-server >/dev/null 2>&1 || true
  done
}

cleanup() {
  local status=$? p
  set +e
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  rm -f "$work/anthropic-key" "$work/anthropic-key-header" "$work/provision-token"
  stop_pid "$daemon_pid"
  [ -z "${tmux_bin:-}" ] || kill_tmux_servers
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_tree "$asker_heartbeat_pid"
  stop_pid "$inbox_pid"
  stop_pid "$proxy_pid"
  stop_pid "$nats_pid"
  stop_pid "$pg_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  [ "${#github_writes[@]}" = 0 ] || github_cleanup
  # The key file's path is in the agents' key command and Claude Code's apiKeyHelper, both under the
  # work directory: the directory goes on every exit, and the evidence stays.
  rm -rf "$work"
  printf "the run's evidence is %s\n" "$evidence" >&2
  [ -n "${ok:-}" ] || [ "$status" != 0 ] || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- provisioning ----------------------------------------------------------------------------------
# fetch_pinned URL SHA256 FILE downloads URL to FILE once, kept in the cache, and refuses a file whose
# sha256 is not the pinned one.
fetch_pinned() {
  local url=$1 want=$2 file=$3 got
  if [ ! -f "$file" ] || [ "$(sha256sum "$file" | cut -d' ' -f1)" != "$want" ]; then
    curl -fsSL --max-time 600 -o "$file.part" "$url" || fail "could not download $url"
    mv "$file.part" "$file"
  fi
  got=$(sha256sum "$file" | cut -d' ' -f1)
  [ "$got" = "$want" ] || fail "$url has sha256 $got, not the pinned $want"
}

# provision_postgres unpacks the zonky jar's postgres-linux-x86_64.txz (Python's lzma and tarfile:
# a pod has no xz) and copies pgvector's library and extension files into it, once per cache.
provision_postgres() {
  pg_root=$cache/postgres-$pg_version
  [ -x "$pg_root/bin/postgres" ] && [ -f "$pg_root/lib/postgresql/vector.so" ] && return 0
  fetch_pinned "$pg_jar_url" "$pg_jar_sha256" "$cache/postgres-$pg_version.jar"
  fetch_pinned "$debian_pool/$pgvector_deb" "$pgvector_sha256" "$cache/$(basename "$pgvector_deb")"
  rm -rf "$pg_root.part" "$work/pgvector"
  python3 - "$cache/postgres-$pg_version.jar" "$pg_root.part" <<'PY'
import io, lzma, sys, tarfile, zipfile
jar = zipfile.ZipFile(sys.argv[1])
name = next(n for n in jar.namelist() if n.endswith(".txz"))
tarfile.open(fileobj=io.BytesIO(lzma.decompress(jar.read(name)))).extractall(sys.argv[2], filter="tar")
PY
  dpkg-deb -x "$cache/$(basename "$pgvector_deb")" "$work/pgvector"
  cp "$work/pgvector/usr/lib/postgresql/17/lib/vector.so" "$pg_root.part/lib/postgresql/"
  cp "$work/pgvector/usr/share/postgresql/17/extension/"vector* "$pg_root.part/share/postgresql/extension/"
  mv "$pg_root.part" "$pg_root"
}

# pg_sql DATABASE SQL prints the statement's rows as one JSON array, through Bun's Postgres client
# (the host has no psql).
pg_sql() {
  bun "$work/pg-sql.ts" "postgres://postgres@127.0.0.1:$port_pg/$1?sslmode=disable" "$2"
}

start_postgres() {
  local attempt
  cat >"$work/pg-sql.ts" <<'TS'
const [dsn, query] = process.argv.slice(2);
const sql = new Bun.SQL(dsn as string);
const rows = await sql.unsafe(query as string);
console.log(JSON.stringify([...rows]));
await sql.close();
TS
  LD_LIBRARY_PATH="$pg_root/lib" "$pg_root/bin/initdb" -D "$work/pg-data" -U postgres --auth=trust -E UTF8 --locale=C \
    >>"$evidence/logs/postgres.log" 2>&1 || fail "initdb failed; see $evidence/logs/postgres.log"
  mkdir -p "$work/pg-sock"
  for attempt in 1 2 3 4 5; do
    pick_port port_pg
    if LD_LIBRARY_PATH="$pg_root/lib" "$pg_root/bin/pg_ctl" -D "$work/pg-data" -l "$evidence/logs/postgres.log" -w -t 60 \
      -o "-p $port_pg -h 127.0.0.1 -k $work/pg-sock" start >/dev/null; then
      pg_pid=$(head -1 "$work/pg-data/postmaster.pid")
      return 0
    fi
    note "Postgres did not start on port $port_pg (attempt $attempt); picking another"
  done
  fail "Postgres did not start; see $evidence/logs/postgres.log"
}

# provision_tmux sets tmux_bin: the host's tmux, else tmux unpacked from Debian with a wrapper that
# binds its libraries (docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-…): the tmux runtime
# starts its server with the pane allow-list, which passes PATH and drops LD_LIBRARY_PATH.
provision_tmux() {
  local entry deb sha root_dir=$cache/tmux-root
  mkdir -p "$work/bin"
  if command -v tmux >/dev/null 2>&1; then
    ln -sfn "$(command -v tmux)" "$work/bin/tmux"
  else
    if [ ! -x "$root_dir/usr/bin/tmux" ]; then
      rm -rf "$root_dir.part"
      for entry in "${tmux_debs[@]}"; do
        deb=${entry% *}
        sha=${entry#* }
        fetch_pinned "$debian_pool/$deb" "$sha" "$cache/$(basename "$deb")"
        dpkg-deb -x "$cache/$(basename "$deb")" "$root_dir.part"
      done
      mv "$root_dir.part" "$root_dir"
    fi
    printf '#!/bin/sh\nLD_LIBRARY_PATH=%s/usr/lib/x86_64-linux-gnu exec %s/usr/bin/tmux "$@"\n' "$root_dir" "$root_dir" >"$work/bin/tmux"
    chmod 0755 "$work/bin/tmux"
  fi
  tmux_bin=$work/bin/tmux
  note "tmux: $("$tmux_bin" -V) ($(readlink -f "$work/bin/tmux"))"
}

provision_nats() {
  nats_bin=$cache/gobin/nats-server
  if [ ! -x "$nats_bin" ] || [ "$("$nats_bin" --version 2>&1)" != "nats-server: $nats_version" ]; then
    (cd "$root/packages/envoy" && GOBIN="$cache/gobin" GOFLAGS=-mod=mod go install "github.com/nats-io/nats-server/v2@$nats_version") ||
      fail "go install github.com/nats-io/nats-server/v2@$nats_version failed"
  fi
  note "nats-server: $("$nats_bin" --version)"
}

provision_claude() {
  local dir=$cache/claude-code-$claude_code_version
  if [ ! -e "$dir/node_modules/.bin/claude" ]; then
    mkdir -p "$dir"
    bun add --cwd "$dir" --exact "@anthropic-ai/claude-code@$claude_code_version" >&2 ||
      fail "bun add @anthropic-ai/claude-code@$claude_code_version failed"
  fi
  claude_bin=$(readlink -f "$dir/node_modules/.bin/claude")
  local version
  version=$("$claude_bin" --version 2>&1) || fail "$claude_bin --version failed: $version"
  [[ "$version" == "$claude_code_version"* ]] || fail "Claude Code reports $version, not the pinned $claude_code_version"
  note "Claude Code: $claude_bin ($version)"
}

resolve_omp() {
  local pin
  if [ -n "${LEGION_E2E_OMP_BIN:-}" ]; then
    omp_bin=$LEGION_E2E_OMP_BIN
  elif [ -x /opt/omp/bin/omp ]; then
    omp_bin=/opt/omp/bin/omp
  else
    pin=$(<"$root/.omp-pin")
    omp_bin=$(mise where "$pin")/bin/omp || fail "no Oh My Pi: set LEGION_E2E_OMP_BIN, or install the .omp-pin with mise"
  fi
  [ -x "$omp_bin" ] || fail "the Oh My Pi binary $omp_bin is not executable"
  note "Oh My Pi: $omp_bin ($("$omp_bin" --version 2>&1 | head -1)); .omp-pin names $(<"$root/.omp-pin")"
}

# real_tools writes $work/realbin, the daemon's PATH ahead of the system's: the first gh on PATH that
# is a compiled program (a Legion pane's first gh is the `legion gh` wrapper script, which the daemon
# refuses), and the host's jj, git and bun.
real_tools() {
  local dir gh=
  mkdir -p "$work/realbin"
  local IFS=:
  for dir in $PATH; do
    [ -x "$dir/gh" ] && [ ! -d "$dir/gh" ] || continue
    [ "$(head -c 2 "$dir/gh")" = '#!' ] && continue
    gh=$dir/gh
    break
  done
  unset IFS
  [ -n "$gh" ] || fail "no compiled gh on PATH (the daemon refuses a gh wrapper script)"
  ln -sfn "$gh" "$work/realbin/gh"
  for tool in jj git bun; do ln -sfn "$(command -v "$tool")" "$work/realbin/$tool"; done
  daemon_path="$work/bin:$work/realbin:/usr/local/bin:/usr/bin:/bin"
}

# ---- builds ----------------------------------------------------------------------------------------
build_branch() {
  mkdir -p "$work/branch"
  (cd "$root/packages/daemon" && go build -o "$work/branch/legion" ./cmd/legion)
  (cd "$root/packages/envoy" && go build -o "$work/branch/envoy-listener" ./cmd/listener && go build -o "$work/branch/envoy-dispatch" ./cmd/dispatch)
  built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/branch/legion" "$work/branch/envoy-listener" "$work/branch/envoy-dispatch") ||
    fail "lib/built-from.sh could not say what the run built"
  while IFS= read -r line; do note "branch $line"; done <<<"$built"
}

# build_base builds the base commit's three binaries from a `git archive` of it (the checkout is
# read, never changed) and installs its workspace, whose plugins the base daemon loads.
build_base() {
  base_commit=$(jj -R "$root" log -r "$base_rev" --no-graph -T 'commit_id' 2>/dev/null || git -C "$root" rev-parse "$base_rev") ||
    fail "the base revision $base_rev does not resolve"
  # The base commit's tree, its three binaries and its installed workspace are kept in the cache by
  # commit: a commit's build is the same every run.
  base_src=$cache/base-$base_commit/src
  if [ ! -x "$cache/base-$base_commit/bin/envoy-dispatch" ]; then
    rm -rf "$cache/base-$base_commit"
    mkdir -p "$base_src" "$cache/base-$base_commit/bin"
    git -C "$root" archive "$base_commit" | tar -x -C "$base_src"
    (cd "$base_src/packages/daemon" && GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/legion" ./cmd/legion)
    (cd "$base_src/packages/envoy" && GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/envoy-listener" ./cmd/listener &&
      GOFLAGS=-buildvcs=false go build -o "$cache/base-$base_commit/bin/envoy-dispatch" ./cmd/dispatch)
    (cd "$base_src" && bun install --frozen-lockfile >/dev/null)
  fi
  ln -sfn "$cache/base-$base_commit/bin" "$work/base"
  local b
  for b in legion envoy-listener envoy-dispatch; do
    note "base $b: sha256 $(sha256sum "$work/base/$b" | cut -d' ' -f1), built from git archive $base_commit ($base_rev)"
  done
}

# install_plugins SOURCE HOME PROFILE DEST installs SOURCE's pi-envoy and pi-legion into the profile,
# each packed by SOURCE's own lib/pack-plugin.sh as the release packs it, then `omp plugin install`
# with the run's Oh My Pi, as lib/install-plugin-profile.sh does with the mise-pinned one (a pod has
# no mise).
install_plugins() {
  local source=$1 home=$2 profile=$3 dest=$4 package tarball listing name version
  for package in pi-envoy pi-legion; do
    tarball=$(bash "$source/scripts/e2e/lib/pack-plugin.sh" "$package" "$dest/$package-pack")
    mkdir -p "$dest/$package"
    tar xzf "$tarball" -C "$dest/$package" --strip-components=1
    (cd "$dest/$package" && HOME=$home OMP_PROFILE=$profile "$omp_bin" plugin install "$dest/$package" >&2)
    name=$(jq -r .name "$dest/$package/package.json")
    version=$(jq -r .version "$dest/$package/package.json")
    listing=$(cd "$dest/$package" && HOME=$home OMP_PROFILE=$profile "$omp_bin" plugin list --json)
    jq -e --arg name "$name" --arg v "$version" '[.npm[]? | select(.name == $name and .version == $v and .enabled == true)] | length == 1' \
      <<<"$listing" >/dev/null || fail "profile $profile under $home does not list $name@$version enabled"
    note "$name@$version enabled in profile $profile ($dest/$package)"
  done
}

# install_model_route HOME PROFILE LABEL: lib/install-key-file-model-route.sh's route, then pointed at
# the recording proxy under LABEL and at the questions file's model, so every agent turn is recorded.
install_model_route() {
  local home=$1 profile=$2 label=$3 agent_dir model
  model=$(jq -r .model "$questions_file")
  bash "$root/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$home" \
    --dest "$evidence/model-route-$label" --key-dir "$work/key-$label" --key-file "$work/anthropic-key" >/dev/null ||
    fail "the agents' model route could not be installed (the reason is above)"
  agent_dir=$home/.omp/profiles/$profile/agent
  sed -i "s#baseUrl: https://api.anthropic.com#baseUrl: http://127.0.0.1:$port_proxy/tag/$label#" "$agent_dir/models.yml"
  sed -i "s#anthropic/claude-opus-4-8#anthropic/$model#g" "$agent_dir/config.yml"
  grep -q "tag/$label" "$agent_dir/models.yml" || fail "the profile's models.yml does not name the proxy"
  note "profile $profile: anthropic/$model through the recording proxy as $label"
}

# ---- services --------------------------------------------------------------------------------------
service_env=(env -i "PATH=$PATH" "LANG=C.UTF-8" "TZ=UTC")

start_nats() {
  local attempt offset result
  mkdir -p "$work/nats-store"
  for attempt in 1 2 3 4 5; do
    [ -n "$port_nats" ] || pick_port port_nats
    offset=$(log_size nats)
    start_process nats "${service_env[@]}" "$nats_bin" -js -a 127.0.0.1 -p "$port_nats" -sd "$work/nats-store"
    result=0
    await_start nats "$nats_pid" "$offset" 60 "nats-server to be ready" \
      sh -c "tail -c +$((offset + 1)) '$evidence/logs/nats.log' | grep -q 'Server is ready'" || result=$?
    [ "$result" != 0 ] || return 0
    port_nats=
    note "nats-server lost its port (attempt $attempt); picking another"
  done
  fail "nats-server lost its picked port five times"
}

start_proxy() {
  local offset
  pick_port port_proxy
  mkdir -p "$work/proxy-hold" "$work/proxy-stand-in"
  offset=$(log_size proxy)
  start_process proxy "${service_env[@]}" "HOME=$work" bun "$root/scripts/e2e/lib/recording-proxy.ts" --port "$port_proxy" \
    --upstream https://api.anthropic.com --record "$evidence/model-calls.jsonl" \
    --hold-dir "$work/proxy-hold" --stand-in-dir "$work/proxy-stand-in"
  await_start proxy "$proxy_pid" "$offset" 30 "the recording proxy" \
    sh -c "tail -c +$((offset + 1)) '$evidence/logs/proxy.log' | grep -q 'recording-proxy listening'"
}

start_inbox() {
  local offset
  pick_port port_inbox
  offset=$(log_size inbox)
  start_process inbox "${service_env[@]}" "HOME=$work" bun "$root/scripts/e2e/lib/envoy-inbox.ts" --port "$port_inbox" \
    --record "$evidence/inbox.jsonl"
  await_start inbox "$inbox_pid" "$offset" 30 "the Envoy inbox" \
    sh -c "tail -c +$((offset + 1)) '$evidence/logs/inbox.log' | grep -q 'envoy-inbox listening'"
}

# start_listener BINARY_DIR and start_dispatch BINARY_DIR DATABASE are claude-backend-tmux.sh's, with
# every variable named (env -i): a pod's own ENVOY_*, DISPATCH_* and LEGION_* never reach them.
start_listener() {
  local bin=$1 attempt offset result
  for attempt in 1 2 3 4 5; do
    [ -n "$port_listener" ] || pick_port port_listener
    offset=$(log_size listener)
    start_process listener "${service_env[@]}" "HOME=$work/listener-home" "ENVOY_API_TOKEN=$(cat "$work/envoy-token")" \
      "PORT=$port_listener" ENVOY_LISTEN_HOST=127.0.0.1 "ENVOY_MACHINE_ID=legion-e2eal-$$" \
      "NATS_URLS=nats://127.0.0.1:$port_nats" "$bin/envoy-listener"
    result=0
    await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
      curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions" || result=$?
    [ "$result" != 0 ] || return 0
    port_listener=
    note "the Envoy listener lost its port (attempt $attempt); picking another"
  done
  fail "the Envoy listener lost its picked port five times"
}

start_dispatch() {
  local bin=$1 database=$2 attempt offset result
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  for attempt in 1 2 3 4 5; do
    [ -n "$port_dispatch" ] || pick_port port_dispatch
    offset=$(log_size dispatch)
    start_process dispatch "${service_env[@]}" "HOME=$work/dispatch-home" "XDG_CONFIG_HOME=$work/dispatch-home/.config" \
      "DATABASE_URL=postgres://postgres@127.0.0.1:$port_pg/$database?sslmode=disable" \
      "DISPATCH_AGENT_TOKEN=$(cat "$work/dispatch-token")" "ENVOY_TOKEN=$(cat "$work/envoy-token")" \
      DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 \
      "DISPATCH_PORT=$port_dispatch" "DISPATCH_SERVER_URL=http://127.0.0.1:$port_dispatch" \
      "NATS_URLS=nats://127.0.0.1:$port_nats" "ENVOY_URL=http://127.0.0.1:$port_listener" "$bin/envoy-dispatch"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 120 "the scratch Dispatch server" \
      curl -fsS "http://127.0.0.1:$port_dispatch/api/v1" || result=$?
    dispatch_base="http://127.0.0.1:$port_dispatch"
    [ "$result" != 0 ] || return 0
    port_dispatch=
    note "the scratch Dispatch lost its port (attempt $attempt); picking another"
  done
  fail "the scratch Dispatch lost its picked port five times"
}

stop_service() {
  local name=$1 pid_var="${1}_pid"
  stop_pid "${!pid_var}"
  printf -v "$pid_var" '%s' ""
  note "stopped $name"
}

# ---- the daemon ------------------------------------------------------------------------------------
# write_legion_config FILE ANSWERS [BOUNDS] writes the daemon's legion.yaml for the current store:
# claims-only (no dispatch_url, no github_apps) unless the review App is granted, the agents'
# reviewer on Claude Code, the store's runtime, and with ANSWERS=yes the answers block: Anthropic
# through the recording proxy, the list prices, and BOUNDS (a YAML flow mapping) over the defaults.
# ANSWERS=base is the base commit's daemon's file, in the same mode, on runtime tmux's files store
# (it has neither session_store nor answers): an agent it starts works in the same workspace the
# branch daemon later resumes it in, as an upgrade leaves it.
write_legion_config() {
  local file=$1 answers=$2 bounds=${3:-}
  {
    printf 'project: %s\nport: %s\npostgres_dsn: postgres://postgres@127.0.0.1:%s/%s?sslmode=disable\n' \
      "$project" "$port_daemon" "$port_pg" "$daemon_db"
    printf 'state_dir: %s\noperator_token_file: %s\n' "$state" "$work/operator-token"
    printf 'envoy_url: http://127.0.0.1:%s\nenvoy_token_file: %s\nnats_urls:\n  - nats://127.0.0.1:%s\n' \
      "$port_listener" "$work/envoy-token" "$port_nats"
    printf 'probe_interval_seconds: 5\nworker_backends:\n  reviewer: claude\n'
    if [ "$store" = postgres ] && [ "$answers" != base ]; then
      printf 'runtime:\n  tmux:\n    session_store: postgres\n    session_dsn_file: %s\n' "$work/sessions-dsn-$store"
    fi
    if [ "$mode" = workflow ]; then
      printf 'dispatch_url: http://127.0.0.1:%s\ndispatch_token_file: %s\nprojects:\n  %s: {}\ngates:\n  design: off\n' \
        "$port_dispatch" "$work/dispatch-token" "$project"
      cat "$work/github-apps.yaml"
    fi
    if [ "$answers" = yes ]; then
      printf 'answers:\n  providers:\n    anthropic:\n      base_url: http://127.0.0.1:%s/tag/daemon-%s\n      api_key_file: %s\n' \
        "$port_proxy" "$store" "$work/anthropic-key"
      printf '  prices:\n%s\n' "$answers_prices"
      [ -z "$bounds" ] || printf '  bounds: %s\n' "$bounds"
    fi
  } >"$file"
}

# start_daemon BINARY CONFIG LOG HOME PROFILE PLUGIN_DIR boots a daemon with every variable named: the
# run's HOME and profile, the run's tools first on PATH, the run's Oh My Pi (LEGION_OMP_PATH: the
# daemon takes omp_invocation only as `mise x <tool> -- omp`), the run's tmux directory, and the
# Claude Code launch. It waits for /healthz and records the log offset this boot starts at; a daemon
# that exits first fails the check with its log's last lines.
start_daemon() {
  daemon_bin=$1 daemon_config=$2 daemon_log=$3 daemon_home=$4 daemon_profile=$5 daemon_plugin_dir=$6
  local started=$SECONDS
  printf '=== boot at %s (%s)\n' "$(date -u +%FT%TZ)" "$daemon_config" >>"$daemon_log"
  boot_offset=$(stat -c %s "$daemon_log")
  env -i "HOME=$daemon_home" "OMP_PROFILE=$daemon_profile" "PATH=$daemon_path" "TMPDIR=$work/tmp" \
    "TMUX_TMPDIR=$work/tmux" "XDG_STATE_HOME=$work/xdg" "XDG_CONFIG_HOME=$XDG_CONFIG_HOME" "MISE_DATA_DIR=$MISE_DATA_DIR" \
    "JJ_CONFIG=$JJ_CONFIG" LANG=C.UTF-8 TZ=UTC "USER=$(id -un)" "LOGNAME=$(id -un)" SHELL=/bin/bash TERM=xterm-256color \
    "LEGION_OMP_PATH=$omp_bin" "LEGION_CLAUDE_PATH=$claude_bin" "LEGION_BUN_PATH=$work/realbin/bun" \
    "LEGION_CLAUDE_PLUGIN_DIR=$daemon_plugin_dir" \
    "$daemon_bin" start --config "$daemon_config" >>"$daemon_log" 2>&1 &
  daemon_pid=$!
  note "waiting up to 300s for the daemon ($daemon_config) to answer /healthz"
  until curl -fsS --max-time 5 "http://127.0.0.1:$port_daemon/healthz" >/dev/null 2>&1; do
    kill -0 "$daemon_pid" 2>/dev/null ||
      fail "the daemon exited at boot; its log $daemon_log says: $(tail -c +$((boot_offset + 1)) "$daemon_log" | grep -v '^{' | tail -3 | tr '\n' ' ')"
    ((SECONDS - started < 300)) || fail "the daemon did not answer /healthz within 300 s; see $daemon_log"
    sleep 0.5
  done
}
stop_daemon() {
  [ -n "$daemon_pid" ] || return 0
  stop_pid "$daemon_pid"
  daemon_pid=
  [ -z "$tmux_bin" ] || kill_tmux_servers
}
# boot_log prints the current boot's daemon log lines, JSON only.
boot_log() { tail -c "+$((boot_offset + 1))" "$daemon_log" | jq -c -R 'fromjson? // empty'; }

legion_cmd() { "$daemon_bin" "$@" --config "$daemon_config"; }
claims_cmd() {
  local sub=$1
  shift
  "$daemon_bin" claims "$sub" --config "$daemon_config" --operator-token-file "$work/operator-token" "$@"
}
claim_json() { claims_cmd list --json | jq -ce --arg token "$1" '.claims[] | select(.token == $token)'; }
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; }
claim_token_of() { printf 'legion-%s-%s-%s' "${project,,}" "${1,,}" "$2"; }

# ---- Envoy -----------------------------------------------------------------------------------------
envoy() { curl -sS --max-time 30 -H "@$work/envoy-auth-header" -H 'content-type: application/json' "$@"; }
listener_url() { printf 'http://127.0.0.1:%s%s' "$port_listener" "$1"; }
# session_gone SESSION: the listener's session bucket no longer holds SESSION. A stopped pane does
# not unregister its session; the entry lapses with the bucket's 5-minute TTL, so a wait for it is
# session_ttl_wait long.
session_gone() { [ "$(envoy -o /dev/null -w '%{http_code}' "$(listener_url "/v1/registry/$1")")" = 404 ]; }
session_live() { [ "$(envoy -o /dev/null -w '%{http_code}' "$(listener_url "/v1/registry/$1")")" = 200 ]; }
session_ttl_wait=420
# more_than N COMMAND...: COMMAND prints a number above N.
more_than() {
  local n=$1
  shift
  [ "$("$@")" -gt "$n" ]
}
# register_asker makes the run's own sender session live in the listener's registry, served by the
# Envoy inbox: what comes back to it on its agent topic lands in $evidence/inbox.jsonl. With it, a
# stand-in for the project's controller (the operator launches none under tmux) subscribes to both
# passes' controller topics, `notifications.legion.<project token>.controller`, so every controller
# notice the daemons publish lands there too, under its session.
asker_session=legion-e2eal-asker-$$
controller_session=legion-e2eal-controller-$$
register_asker() {
  envoy -f -o /dev/null --data "$(jq -cn --arg s "$asker_session" --argjson port "$port_inbox" --arg dir "$work" \
    '{session_id: $s, topics: [], port: $port, host: "127.0.0.1", dir: $dir, title: "answer-loop rig asker", capabilities: ["steer"], driving: true, self_subscribed: true}')" \
    "$(listener_url /v1/interests/subscribe)"
  envoy -f -o /dev/null --data "$(jq -cn --arg s "$controller_session" --argjson port "$port_inbox" --arg dir "$work" --arg tag "$run_tag" \
    '{session_id: $s, topics: ["notifications.legion.alf\($tag).controller", "notifications.legion.alp\($tag).controller"], port: $port, host: "127.0.0.1", dir: $dir, title: "answer-loop rig controller", capabilities: ["steer"], driving: true, self_subscribed: true}')" \
    "$(listener_url /v1/interests/subscribe)"
}
# controller_notices KIND NEEDLE: how many notices of KIND the controller stand-in received whose
# text holds NEEDLE.
controller_notices() {
  if [ -f "$evidence/inbox.jsonl" ]; then
    jq -s --arg s "$controller_session" --arg k "\"kind\":\"$1\"" --arg n "$2" \
      '[.[] | select(.session == $s and (.text | contains($k)) and (.text | contains($n)))] | length' "$evidence/inbox.jsonl"
  else
    printf '0\n'
  fi
}

# ---- questions -------------------------------------------------------------------------------------
question() { jq -ce --arg id "$1" '.questions[] | select(.id == $id)' "$questions_file"; }
question_text() { question "$1" | jq -r .text; }
# answer_holds ID TEXT: TEXT holds every must_contain string of question ID, case-insensitively, and
# prints the first one missing when it does not.
answer_holds() {
  local key
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    grep -qiF -- "$key" <<<"$2" || {
      printf '%s\n' "$key"
      return 1
    }
  done < <(question "$1" | jq -r '.must_contain[]?')
}

# ---- producing the stopped agents ------------------------------------------------------------------
# A Dispatch issue of the pass's project, as a person writes one (backlog, Dispatch's default): no
# `legion` label, so a workflow daemon admits nothing of it; its claims are the operator's (legion
# claims spawn).
new_rig_issue() {
  local title=$1 parent=${2:-} payload
  payload=$(jq -cn --arg project "$project" --arg title "$title" --arg parent "$parent" \
    '{project: $project, title: $title, force: true} + (if $parent == "" then {} else {parent: $parent} end)')
  dispatch_human POST issues "$payload" | jq -er .key
}

write_prompt() {
  printf 'You are the %s of the Legion issue %s in a proof run of Legion'"'"'s answer loop. Do exactly what each message asks, in one short reply. Run no tool unless a message asks you to.\n' \
    "$1" "$2" >"$work/prompt-$1-$2.md"
  printf '%s' "$work/prompt-$1-$2.md"
}

# spawn_agent NAME ROLE TREE ISSUE TASK spawns the claim through the daemon's operator route and
# records its token; with the workflow on, the issue's workspace is provisioned first.
spawn_agent() {
  local name=$1 role=$2 tree=$3 issue=$4 task=$5 token
  token=$(claim_token_of "$issue" "$role")
  provision_workspace "$issue"
  claims_cmd spawn --json --tree "$tree" --issue "$issue" --role "$role" --prompt-file "$(write_prompt "$role" "$issue")" \
    --task "$task" >/dev/null
  agent[$store:$name.token]=$token
  agent[$store:$name.issue]=$issue
  agent[$store:$name.role]=$role
  note "spawned $name: $role of $issue (tree $tree), claim $token"
}

# await_turn NAME waits for the agent's task to run and end, then records its session and file.
await_turn() {
  local name=$1 token json
  token=${agent[$store:$name.token]}
  until_true 900 "$name ($token) to finish its task" claim_is "$token" '.state == "idle" and .pending == null'
  json=$(claim_json "$token")
  agent[$store:$name.session]=$(jq -r .session <<<"$json")
  agent[$store:$name.file]=$(jq -r .sessionFile <<<"$json")
  note "$name: session ${agent[$store:$name.session]}, conversation ${agent[$store:$name.file]}"
}

# write_claude_route TOKEN LABEL gives a Claude Code claim its model before the claim exists:
# settings.json in its claim directory names the model, the key command printing the run's key, and
# ANTHROPIC_BASE_URL at the recording proxy.
write_claude_route() {
  local dir=$state/claude/$1
  (umask 077 && mkdir -p "$dir" &&
    printf '#!/bin/sh\nexec cat %s\n' "$work/anthropic-key" >"$work/claude-key-command" &&
    jq -n --arg helper "$work/claude-key-command" --arg model "$(jq -r .model "$questions_file")" \
      --arg base "http://127.0.0.1:$port_proxy/tag/$2" \
      '{apiKeyHelper: $helper, model: $model, env: {ANTHROPIC_BASE_URL: $base}}' >"$dir/settings.json")
  chmod 0700 "$work/claude-key-command"
}

# ---- the issues' workspaces ------------------------------------------------------------------------
# With the workflow on, an operator's spawn spec carries the repository Dispatch's repo-projects
# mapping gives the issue (the smoke repository), and the tmux runtime then works in the issue's jj
# workspace at workspace.Location's <state>/workspaces/<owner>/<repo>/<issue lower-cased>, which
# must exist already (internal/runtime/tmux/spawn.go, workspaceDir): the workflow's outbox
# provisions it for an issue it admits, and it admits no rig issue (none has the `legion` label).
# provision_workspace ISSUE provisions it as an issue pod's two init containers do, with the branch
# daemon's own `legion workspace-init`: `fetch` fills the store pass's feed from GitHub once (no
# repository cache), under an installation token of the implement App for the smoke repository
# alone, `contents: read`, in a 0600 file removed as soon as the fetch ends; `provision` then makes
# the shared clone at <state>/repos/github.com/<owner>/<repo> and the issue's workspace on
# legion/<ISSUE> from main, its clone's credential helper the one a tmux daemon gives every clone it
# provisions and rewrites at boot (workspace.GitHubCredentialHelper, `!legion git-credential`).
# Claims-only, a spec has no repository and the runtime makes <state>/workspaces/<issue> itself.
# The fetch and the provisioning log to $evidence/logs/workspaces-<store>.log.
provision_workspace() {
  local issue=$1 feed=$work/feed-$store dir log=$evidence/logs/workspaces-$store.log out code
  [ "$mode" = workflow ] || return 0
  dir=$state/workspaces/$repo/${issue,,}
  [ ! -d "$dir" ] || return 0
  if [ ! -d "$feed/$repo.git" ]; then
    rm -f "$work/provision-token"
    mint_provision_token "$work/provision-token" ||
      fail "could not mint the implement App's installation token on $repo for the workspaces' fetch"
    code=0
    env -u LEGION_REPOSITORY_CACHE_URL -u LEGION_REPOSITORY_CACHE_TOKEN_FILE "PATH=$daemon_path" "TMPDIR=$work/tmp" \
      "LEGION_PROVISION_TOKEN_FILE=$work/provision-token" \
      "$work/branch/legion" workspace-init fetch --repo "$repo" --feed "$feed" >>"$log" 2>&1 || code=$?
    rm -f "$work/provision-token"
    [ "$code" = 0 ] || fail "legion workspace-init fetch --repo $repo exited $code; see $log"
    note "fetched $repo into the $store pass's feed $feed: $(grep '^workspace-init fetch:' "$log" | tail -1)"
  fi
  code=0
  out=$(env -u LEGION_PROVISION_TOKEN_FILE -u LEGION_REPOSITORY_CACHE_URL -u LEGION_REPOSITORY_CACHE_TOKEN_FILE \
    "PATH=$daemon_path" "TMPDIR=$work/tmp" \
    "$work/branch/legion" workspace-init provision --issue "$issue" --repo "$repo" --default-branch main --root "$state" \
    --credential-helper '!legion git-credential' --feed "$feed" 2>&1) || code=$?
  printf '%s\n' "$out" >>"$log"
  [ "$code" = 0 ] || fail "legion workspace-init provision --issue $issue exited $code: $(tail -1 <<<"$out")"
  # Its last line names the directory it provisioned, which must be the one spawn.go looks in.
  [ "$(tail -1 <<<"$out")" = "workspace-init: $dir on legion/$issue from main" ] ||
    fail "legion workspace-init provision --issue $issue provisioned \"$(tail -1 <<<"$out")\", not $dir"
  note "provisioned $issue's workspace $dir (legion/$issue from main)"
}

# mint_provision_token FILE writes an installation token of github_apps.implement in
# $work/github-apps.yaml on the smoke repository alone, `contents: read`, to the new 0600 FILE
# (lib/app-logins.ts reads the key, lib/app-installation.ts finds the installation); the key and the
# token are printed nowhere.
mint_provision_token() {
  cat >"$work/mint-provision-token.ts" <<'TS'
import { readFileSync, writeFileSync } from "node:fs";
const [lib, config, repo, out] = process.argv.slice(2) as [string, string, string, string];
const { appJWT, appKeySources, runKeyCommand } = await import(`${lib}/app-logins.ts`);
const { installationOn } = await import(`${lib}/app-installation.ts`);
const api = "https://api.github.com";
const source = appKeySources(readFileSync(config, "utf8"), ["implement"]).implement;
if (source === undefined) throw new Error("github_apps.implement is not configured");
const key = runKeyCommand("implement", source.privateKeyCommand);
const installation = await installationOn(source.appId, key, repo, api);
if (!installation.installed) throw new Error(`the implement App is not installed on ${repo}`);
const path = `/app/installations/${installation.installation}/access_tokens`;
const minted = await fetch(`${api}${path}`, {
  method: "POST",
  headers: {
    authorization: `Bearer ${appJWT(source.appId, key, new Date())}`,
    accept: "application/vnd.github+json",
    "x-github-api-version": "2022-11-28",
    "content-type": "application/json",
  },
  body: JSON.stringify({ repositories: [repo.split("/")[1]], permissions: { contents: "read" } }),
  signal: AbortSignal.timeout(30_000),
});
if (!minted.ok) throw new Error(`POST ${path} answered ${minted.status}`);
const token = ((await minted.json()) as { token?: unknown }).token;
if (typeof token !== "string" || token === "") throw new Error(`POST ${path} named no token`);
writeFileSync(out, token, { mode: 0o600, flag: "wx" });
TS
  (umask 077 && bun "$work/mint-provision-token.ts" "$root/scripts/e2e/lib" "$work/github-apps.yaml" "$repo" "$1")
}

# produce_pre_change runs the base commit's daemon on the pass's database and state directory, as the
# release before this change: one Claude Code reviewer, on a child of a tree of its own, has its
# conversation, then is suspended and the base daemon stopped. The tree's architect is spawned with
# no task first, since an operator's spawn opens a tree's lifecycle only for its root architect, and
# is suspended with it. The branch daemon then boots on the same database and directory, as an
# upgrade does, and finds the conversation only by its title (A2's conversation from before the change).
# The base daemon runs in the run's mode, so with the workflow on both issues' workspaces are
# provisioned first (provision_workspace): the base daemon starts the reviewer there, and the branch
# daemon resumes it there (A10).
produce_pre_change() {
  local token architect tree=${agent[$store:pre_change.tree]}
  write_legion_config "$work/base-$store.yaml" base
  token=$(claim_token_of "${agent[$store:pre_change.issue]}" reviewer)
  architect=$(claim_token_of "$tree" architect)
  write_claude_route "$token" "claude-$store"
  start_daemon "$work/base/legion" "$work/base-$store.yaml" "$evidence/logs/base-daemon-$store.log" "$base_home" "$base_profile" \
    "$base_src/packages/claude-envoy"
  provision_workspace "$tree"
  claims_cmd spawn --json --tree "$tree" --issue "$tree" --role architect --prompt-file "$(write_prompt architect "$tree")" >/dev/null
  spawn_agent pre_change reviewer "$tree" "${agent[$store:pre_change.issue]}" "$(jq -r .agents.pre_change.task "$questions_file")"
  await_turn pre_change
  claims_cmd suspend --claim "$token" >/dev/null
  until_true 120 "the pre-change reviewer to be suspended" claim_is "$token" '.state == "suspended"'
  claims_cmd suspend --claim "$architect" >/dev/null
  until_true 120 "the pre-change tree's architect to be suspended" claim_is "$architect" '.state == "suspended"'
  stop_daemon
  note "the base daemon ran the Claude Code reviewer ${agent[$store:pre_change.session]} and stopped"
}

# produce_agents runs the branch daemon's own agents and stops each the way its kind is made:
#   architect, the tree's root: suspended, as a person parking the tree leaves it;
#   planner, on a child: stopped (retired), as a closed child's claim ends;
#   tester, on a child: suspended, its claim's session forgotten as the workflow's issue close
#   forgets it (claims-only mode has no issue close, so the run clears the one claims row the close
#   writes, with the daemon stopped), resumed with a new session, given its second task, and
#   suspended again: the first session is the one the second replaced.
produce_agents() {
  local tester old task replacement
  task=$(jq -r .agents.architect.task "$questions_file")
  replacement=$(jq -r .agents.tester.replacement_task "$questions_file")
  # With the workflow on, a pane has Dispatch: the architect also opens the ask A1 replies on, and
  # the replacing tester the ask A6's first leg replies on.
  if [ "$mode" = workflow ]; then
    task+=" Then make exactly one tool call: open one Dispatch ask on ${agent[$store:root]} asking a person whether to keep the 7-minute TTL, and wait."
    replacement+=" Then make exactly one tool call: open one Dispatch ask on ${agent[$store:tester.issue]} asking a person whether to keep the 20-run rerun as the release gate, and wait."
  fi
  spawn_agent architect architect "${agent[$store:root]}" "${agent[$store:root]}" "$task"
  spawn_agent planner planner "${agent[$store:root]}" "${agent[$store:planner.issue]}" "$(jq -r .agents.planner.task "$questions_file")"
  spawn_agent tester tester "${agent[$store:root]}" "${agent[$store:tester.issue]}" "$(jq -r .agents.tester.task "$questions_file")"
  await_turn architect
  await_turn planner
  await_turn tester
  claims_cmd suspend --claim "${agent[$store:architect.token]}" >/dev/null
  claims_cmd stop --claim "${agent[$store:planner.token]}" >/dev/null
  claims_cmd suspend --claim "${agent[$store:tester.token]}" >/dev/null
  until_true 120 "the architect to be suspended" claim_is "${agent[$store:architect.token]}" '.state == "suspended"'
  until_true 120 "the planner to be retired" claim_is "${agent[$store:planner.token]}" '.state == "retired"'
  until_true 120 "the tester to be suspended" claim_is "${agent[$store:tester.token]}" '.state == "suspended"'
  tester=${agent[$store:tester.token]}
  old=${agent[$store:tester.session]}
  agent[$store:tester_old.session]=$old
  agent[$store:tester_old.issue]=${agent[$store:tester.issue]}
  agent[$store:tester_old.file]=${agent[$store:tester.file]}
  stop_daemon
  pg_sql "$daemon_db" "update claims set session = '', session_file = '' where token = '$tester'" >/dev/null
  note "the tester's claim forgets session $old (claims.session and session_file cleared, the daemon stopped)"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
    "$root/packages/claude-envoy"
  claims_cmd resume --claim "$tester" >/dev/null
  until_true 600 "the tester to register a new session" claim_is "$tester" \
    "(.state | IN(\"ready\", \"idle\")) and .session != \"\" and .session != \"$old\""
  claims_cmd deliver --claim "$tester" --task "$replacement" >/dev/null
  await_turn tester
  claims_cmd suspend --claim "$tester" >/dev/null
  until_true 120 "the replacing tester to be suspended" claim_is "$tester" '.state == "suspended"'
  local name
  for name in architect planner tester_old tester pre_change; do
    until_true "$session_ttl_wait" "$name's session to leave the listener's registry" session_gone "${agent[$store:$name.session]}"
  done
  note "stopped: architect ${agent[$store:architect.session]} (parked), planner ${agent[$store:planner.session]} (closed child), tester $old replaced by ${agent[$store:tester.session]}, pre-change reviewer ${agent[$store:pre_change.session]}"
}

# conversation_text NAME prints the agent's conversation as its store holds it: the session file
# under the files store, the session table's row under postgres (Oh My Pi's sessions), and a Claude
# Code transcript as a file under either.
conversation_text() {
  local file=${agent[$store:$1.file]}
  if [ "$store" = postgres ] && [ "$1" != pre_change ]; then
    pg_sql "omp_sessions_$run_tag" "select coalesce(string_agg(content, '' order by start_offset), '') as text from (select 0 as start_offset, content from omp_session_files where path = '$file' union all select start_offset, content from omp_session_files_parts where path = '$file') p" |
      jq -r '.[0].text'
  else
    cat -- "$file"
  fi
}

save_conversations() {
  local name
  mkdir -p "$evidence/sessions/$store"
  for name in architect planner tester_old tester pre_change; do
    conversation_text "$name" >"$evidence/sessions/$store/$name.jsonl" 2>/dev/null || true
  done
}

# ---- prerequisites ---------------------------------------------------------------------------------
begin prerequisites
for tool in go bun jq curl python3 dpkg-deb tar flock pgrep git jj gh sha256sum; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
[ -n "${ANTHROPIC_API_KEY:-}" ] || fail "ANTHROPIC_API_KEY is unset: the run's model calls need it"
(umask 077 && printf '%s' "$ANTHROPIC_API_KEY" | tr -d '[:space:]' >"$work/anthropic-key" &&
  printf 'x-api-key: %s\n' "$(cat "$work/anthropic-key")" >"$work/anthropic-key-header")
unset ANTHROPIC_API_KEY
code=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -H "@$work/anthropic-key-header" -H 'anthropic-version: 2023-06-01' \
  https://api.anthropic.com/v1/models) || code="unreachable (curl exited $?)"
rm -f "$work/anthropic-key-header"
[ "$code" = 200 ] || fail "Anthropic refuses ANTHROPIC_API_KEY: GET /v1/models answered $code"
note "Anthropic accepts the run's key (GET /v1/models 200); it is in $work/anthropic-key (0600) and nowhere else"
note "OpenAI: not used (A10's GPT-6.1 leg is not run: the OpenAI API is paused, dispatch://OSSV-201/ask/71cd9fe8-913a-409d-a6de-1adae3f6dd19)"
# The questions and their answer keys, read before any model call.
jq -e '.agents.architect.task and .agents.planner.task and .agents.tester.task and .agents.tester.replacement_task and .agents.pre_change.task and (.questions | length > 0)' \
  "$questions_file" >/dev/null || fail "$questions_file is not the questions file this run reads"
note "questions: $questions_file, sha256 $(sha256sum "$questions_file" | cut -d' ' -f1), $(jq '.questions | length' "$questions_file") questions"
# The role Apps: the implement App is proved against GitHub and on the smoke repository; the review
# App's key is resolved only when a person granted it (agent-secrets, LEGION_E2E_REVIEW_APP_GRANTED=1)
# or its tier is agent (secrets). agent-secrets cannot say whether a key is granted without asking
# for it, and asking files a request in a person's inbox, so a run never asks unless told the grant
# exists; told so and refused, it cancels the request it made, by its id, and stops.
if ! command -v secrets >/dev/null && command -v agent-secrets >/dev/null; then
  # The daemon runs under `env -i`: its key commands' `secrets` names the broker's binary by path and
  # carries this session's broker address and key directory (paths, never a secret). A broker call
  # that fails (exit 1: the broker's gateway answered 502 in a run) is tried twice more.
  mkdir -p "$work/bin"
  {
    printf '#!/bin/sh\n'
    for var in AGENT_SECRETS_URL AGENT_SECRETS_KEY_DIR AGENT_SECRETS_HELPER_SOCK XDG_RUNTIME_DIR; do
      [ -z "${!var:-}" ] || printf 'export %s=%q\n' "$var" "${!var}"
    done
    # shellcheck disable=SC2016 # the expansions are the wrapper's own, run by its sh
    printf 'for attempt in 1 2 3; do\n  %q "$@" && exit 0\n  status=$?\n  [ "$status" = 1 ] || exit "$status"\n  sleep 3\ndone\nexit 1\n' \
      "$(command -v agent-secrets)"
  } >"$work/bin/secrets"
  chmod 0755 "$work/bin/secrets"
  export PATH="$work/bin:$PATH"
  review_granted=
  if [ "${LEGION_E2E_REVIEW_APP_GRANTED:-}" = 1 ]; then
    require_app_inputs review
    code=0
    status=$(secrets "$review_app_key_name" --wait 5s -- true 2>&1) || code=$?
    if [ "$code" != 0 ]; then
      request=$(grep -oE 'request [0-9a-f-]{36}' <<<"$status" | head -1 | cut -d' ' -f2 || true)
      [ -z "$request" ] || agent-secrets cancel "$request" >/dev/null 2>&1 || true
      fail "LEGION_E2E_REVIEW_APP_GRANTED=1, but the broker does not give this session the review App's key ($review_app_key_name, exit $code${request:+; request $request cancelled}); its message is not printed"
    fi
    review_granted=1
  fi
  note "secrets CLI: agent-secrets; the review App's key is $([ "$review_granted" = 1 ] && printf 'granted' || printf 'not read (LEGION_E2E_REVIEW_APP_GRANTED is not 1)')"
else
  review_granted=
  require_app_inputs review
  if status=$(secrets get "$review_app_key_name" --no-request 2>&1) && jq -e '.tier == "agent"' <<<"$status" >/dev/null 2>&1; then
    review_granted=1
  fi
fi
if [ "$review_granted" = 1 ]; then
  require_app_inputs implement review
  write_github_apps implement review
  read_app_logins
  installed=$(bun "$root/scripts/e2e/lib/app-installation.ts" --roles implement,review "$work/github-apps.yaml" "$repo") ||
    fail "lib/app-installation.ts could not read the Apps' installations on $repo"
  mode=workflow
else
  require_app_inputs implement
  write_github_apps implement
  check_github_apps implement
  installed=$(bun "$root/scripts/e2e/lib/app-installation.ts" --roles implement "$work/github-apps.yaml" "$repo") ||
    fail "lib/app-installation.ts could not read the implement App's installation on $repo"
fi
note "installations on $repo (GET /repos/$repo/installation under each App's JWT): $installed"
jq -e '.implement.installed == true' <<<"$installed" >/dev/null ||
  fail "the implement App ($implement_app_id) is not installed on $repo: the GitHub checks cannot run there"
[ "$mode" != workflow ] || jq -e '.review.installed == true' <<<"$installed" >/dev/null ||
  fail "the review App ($review_app_id) is not installed on $repo"
if [ -n "$second_repo" ]; then
  [[ "$second_repo" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || fail "LEGION_E2E_REPO_SECOND_OWNER must be owner/name"
  second_installed=$(bun "$root/scripts/e2e/lib/app-installation.ts" --roles implement "$work/github-apps.yaml" "$second_repo") ||
    fail "lib/app-installation.ts could not read the implement App's installation on the second owner's repository"
  note "the second owner's repository: $second_installed"
fi
note "mode: $mode$([ "$mode" = workflow ] || printf ' (the review App key is not granted: the daemon runs without dispatch_url and github_apps, so its workflow, and the answer loop with it, is off)')"
pass

begin provision
started_at=$(date +%s)
provision_postgres
provision_tmux
provision_nats
provision_claude
resolve_omp
real_tools
mkdir -p "$work/tmp" "$work/tmux" "$work/xdg" "$work/listener-home"
chmod 0700 "$work/tmux" "$work/xdg"
pass

begin build
build_branch
build_base
pass

begin rig
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
  printf 'X-Dispatch-User: %s\n' "$proof_human_user" >"$work/dispatch-human-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token")
start_postgres
for db in dispatch dispatch_base "legion_files_$run_tag" "legion_postgres_$run_tag" "legion_base_$run_tag" "omp_sessions_$run_tag"; do
  pg_sql postgres "create database $db" >/dev/null
done
(umask 077 && printf 'postgres://postgres@127.0.0.1:%s/omp_sessions_%s?sslmode=disable\n' "$port_pg" "$run_tag" >"$work/sessions-dsn-postgres")
note "Postgres $pg_version (pid $pg_pid, port $port_pg) with pgvector $(pg_sql dispatch 'create extension if not exists vector' >/dev/null && pg_sql dispatch "select extversion from pg_extension where extname = 'vector'" | jq -r '.[0].extversion')"
start_nats
start_proxy
start_inbox
start_listener "$work/branch"
start_dispatch "$work/branch" dispatch
note "NATS $port_nats (pid $nats_pid), proxy $port_proxy (pid $proxy_pid), inbox $port_inbox (pid $inbox_pid), listener $port_listener (pid $listener_pid), Dispatch $port_dispatch (pid $dispatch_pid)"
# The asker's session lapses with the session bucket's TTL like any other: a loop renews it every
# minute, so an answer that comes late still finds it live.
(while :; do
  register_asker >/dev/null 2>&1 || true
  sleep 60
done) &
asker_heartbeat_pid=$!
# The base daemon's Oh My Pi home: the base commit's plugins (its daemon refuses another contract).
base_home=$work/base-omp-home
base_profile=legion-e2eal-base-$$
make_omp_home "$base_home"
install_plugins "$base_src" "$base_home" "$base_profile" "$work/base-plugins"
install_model_route "$base_home" "$base_profile" agents-base
branch_home=$work/omp-home
branch_profile=legion-e2eal-$$
make_omp_home "$branch_home"
install_plugins "$root" "$branch_home" "$branch_profile" "$work/branch-plugins"
install_model_route "$branch_home" "$branch_profile" agents
note "bring-up took $(($(date +%s) - started_at)) s"
pass

# ---- the criteria ----------------------------------------------------------------------------------
# answering_blockers prints, one per line, what the answering criteria wait on in this run: the
# daemon's workflow (the answer loop runs in it, and it boots only with both role Apps), and the
# listener's hand-off (plan task 6), read from this boot's log: the daemon writes the project's
# answering heartbeat at boot, and a listener without the hand-off answers its route 404.
answering_blockers() {
  [ "$mode" = workflow ] ||
    printf 'WAITING (needs the review App key grant): the daemon runs without its workflow, which carries the answer loop and boots only with both role Apps\n'
  if boot_log | jq -e -s 'any(.[]; .msg | startswith("the Envoy listener answers 404 on PUT /v1/legion/projects/{project}"))' >/dev/null; then
    printf 'WAITING (needs the listener hand-off, task 6): the listener answers the heartbeat route PUT /v1/legion/projects/{project} 404, so it hands nothing off\n'
  fi
}

# criterion ID DESCRIPTION PREREQ... records WAITING for each blocker that applies (and returns 1),
# or returns 0 so the caller runs the check. A PREREQ names a prerequisite beyond the answering ones,
# each read from what the run has: infra448 (the listener's registry routes in this checkout, A3),
# openai (the OpenAI API, paused for this phase), second-owner (LEGION_E2E_REPO_SECOND_OWNER).
criterion() {
  local id=$1 description=$2 blockers line
  shift 2
  printf '== %s (%s): %s\n' "$id" "$store" "$description"
  blockers=$(answering_blockers)
  for line in "$@"; do
    case "$line" in
      infra448) grep -rqF '/v1/legion/agents' "$root/packages/envoy/cmd/listener" ||
        blockers+=$'\n'"WAITING (needs INFRA-448): the listener in this checkout has no /v1/legion/agents routes, and Dispatch no Collaborators view" ;;
      openai) blockers+=$'\n'"WAITING (needs the OpenAI API, paused: dispatch://OSSV-201/ask/71cd9fe8-913a-409d-a6de-1adae3f6dd19): no GPT-6.1 conversation is made" ;;
      second-owner) [ -n "$second_repo" ] || blockers+=$'\n'"WAITING (needs LEGION_E2E_REPO_SECOND_OWNER): no second owner's repository was given" ;;
    esac
  done
  blockers=$(sed '/^$/d' <<<"$blockers")
  [ -n "$blockers" ] || return 0
  while IFS= read -r line; do record_result "$id" WAITING "${line#WAITING }"; done <<<"$blockers"
  return 1
}

# envoy_ask TARGET_KIND TARGET QUESTION_ID [KEY]: the rig's asker sends question ID to a stopped
# agent's session (TARGET_KIND session) or role token (role), and prints `<status> <event id>`.
envoy_ask() { envoy_ask_text "$1" "$2" "$(question_text "$3") (question $3 of run $run_tag)" "${4:-}"; }
# envoy_ask_text TARGET_KIND TARGET TEXT [KEY] is envoy_ask with a text of the check's own; KEY is the
# send's idempotency key, so the same KEY is the same question.
envoy_ask_text() {
  local kind=$1 target=$2 text=$3 key=${4:-} body out code
  register_asker
  out=$work/ask-$RANDOM$RANDOM.json
  if [ "$kind" = session ]; then
    body=$(jq -cn --arg s "$target" --arg m "$text" --arg from "$asker_session" --arg key "$key" \
      '{target_session: $s, source: "agent", source_session: $from, message: $m} + (if $key == "" then {} else {idempotency_key: $key} end)')
    code=$(envoy -o "$out" -w '%{http_code}' --data "$body" "$(listener_url /v1/messages/send)")
  else
    body=$(jq -cn --arg t "notifications.role.$target" --arg m "$text" --arg from "$asker_session" --arg key "$key" \
      '{topic: $t, source: "agent", source_session: $from, message: $m} + (if $key == "" then {} else {idempotency_key: $key} end)')
    code=$(envoy -o "$out" -w '%{http_code}' --data "$body" "$(listener_url /v1/messages/publish)")
  fi
  printf '%s %s\n' "$code" "$(jq -r '.event_id // empty' "$out" 2>/dev/null)"
}
# inbox_reply EVENT prints the text the asker received in reply to EVENT, or fails; inbox_replies
# EVENT counts them.
inbox_reply() {
  [ -f "$evidence/inbox.jsonl" ] || return 1
  jq -s -e -r --arg e "In Reply To: $1" '[.[] | select(.text | contains($e)) | .text] | .[0]' "$evidence/inbox.jsonl"
}
# inbox_message EVENT prints only the message of that reply: the listener renders a delivery as
# header lines, `Message:`, the message, a blank line and `Topic: …` (internal/session.Text).
inbox_message() {
  inbox_reply "$1" | jq -R -s -r 'split("\nMessage:\n") | (.[1] // .[0]) | sub("\n\nTopic: [^\n]*\n?$"; "")'
}
inbox_replies() {
  if [ -f "$evidence/inbox.jsonl" ]; then
    jq -s --arg e "In Reply To: $1" '[.[] | select(.text | contains($e))] | length' "$evidence/inbox.jsonl"
  else
    printf '0\n'
  fi
}

# ask_and_expect NAME KIND TARGET QUESTION_ID: the Envoy half every answering criterion shares: a 200
# instead of a 404, then a reply on the asker's topic within four minutes holding the answer key.
ask_and_expect() {
  local label=$1 kind=$2 target=$3 id=$4 sent code event reply missing
  sent=$(envoy_ask "$kind" "$target" "$id")
  code=${sent%% *}
  event=${sent#* }
  note "$label: the listener answered $code (event $event)"
  [ "$code" = 200 ] || return 1
  soft_wait 240 "the answer to $id on the asker's topic" inbox_reply "$event" || return 1
  reply=$(inbox_reply "$event")
  note "$label: reply: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  if missing=$(answer_holds "$id" "$reply"); then
    return 0
  fi
  note "$label: the reply lacks the answer key's \"$missing\""
  return 1
}

# check_boot: the daemon with answers configured records every stopped agent's conversation, the
# pre-change Claude Code one by its title (a scan row), and mirrors each to the listener's Legion
# agent registry: each session's entry names its issue and role, and the replaced tester's names
# the session that replaced it. Control: no registry route answered the daemon 404 this boot.
check_boot() {
  local log routes rows scanned name entry entries=() ok=1
  printf '== boot (%s): the daemon boots with answers configured\n' "$store"
  if [ "$mode" != workflow ]; then
    record_result boot WAITING "(needs the review App key grant): the daemon runs without its workflow, which carries the answer loop and its registry mirror"
    return 0
  fi
  log=$(boot_log)
  routes=$(jq -s -c '[.[] | select(.msg | startswith("the Envoy listener answers 404 on")) | .route] | group_by(.) | map({route: .[0], lines: length})' <<<"$log")
  note "registry 404 lines this boot, by route (want none): $routes"
  rows=$(pg_sql "$daemon_db" "select session, issue, role, harness, source from agent_conversations where project = '${project,,}' order by recorded_at")
  note "agent_conversations: $rows"
  scanned=$(jq -c --arg s "${agent[$store:pre_change.session]}" '[.[] | select(.session == $s and .source == "scan" and .harness == "claude")] | length' <<<"$rows")
  for name in architect planner tester_old tester pre_change; do
    entry=$(envoy "$(listener_url "/v1/legion/agents/sessions/${agent[$store:$name.session]}")" | jq -c '(.agent // .) | {session_id, issue, role, harness, replaced_by, live}' 2>/dev/null) || entry=null
    entries+=("$name $entry")
    jq -e --arg i "${agent[$store:$name.issue]:-${agent[$store:root]}}" '.issue == $i and .live == false' <<<"$entry" >/dev/null 2>&1 || ok=0
  done
  printf '   registry: %s\n' "${entries[@]}"
  jq -e --arg s "${agent[$store:tester.session]}" '.replaced_by == $s' <<<"${entries[2]#* }" >/dev/null 2>&1 || ok=0
  if [ "$routes" = "[]" ] && [ "$scanned" = 1 ] && [ "$ok" = 1 ]; then
    record_result boot PASS "the pre-change Claude Code conversation recorded by its title (scan); every stopped agent in the listener's registry, not live, the replaced tester naming its replacement; no registry route answered 404"
  else
    record_result boot FAIL "404 routes $routes, pre-change scan rows $scanned, registry entries as listed $ok"
  fi
}

# check_boot_without_answers: the negative control for the boot and A13's daemon half: the same
# daemon with `answers` removed writes nothing to the registry and logs no registry line.
check_boot_without_answers() {
  local lines
  lines=$(boot_log | jq -s -c '[.[] | select((.msg // "") | test("agent registry|answers 404 on|answers:"))] | length')
  note "with answers removed: $lines registry or answers lines this boot"
  [ "$lines" = 0 ]
}

check_postgres_store() {
  local rows files
  printf '== store (%s): the panes keep their conversations in the session table\n' "$store"
  rows=$(pg_sql "omp_sessions_$run_tag" "select path, byte_len from omp_session_files order by path")
  files=0
  for name in architect planner tester_old tester; do
    [ ! -e "${agent[$store:$name.file]}" ] || files=$((files + 1))
  done
  note "omp_session_files: $rows"
  note "session files on disk at the claims' paths: $files (want 0)"
  if [ "$files" = 0 ] && jq -e --arg a "${agent[$store:architect.file]}" --arg t "${agent[$store:tester.file]}" \
    'any(.[]; .path == $a) and any(.[]; .path == $t)' <<<"$rows" >/dev/null; then
    record_result store PASS "every pane's Oh My Pi session is a row of omp_session_files, none a file"
  else
    record_result store FAIL "the session table or the disk disagrees with session_store postgres"
  fi
}

# soft_wait SECONDS DESCRIPTION COMMAND... is lib/rig.sh's until_true for a criterion's step: a
# timeout returns 1, and the check records its FAIL, instead of ending the run.
soft_wait() {
  local limit=$1 what=$2 started=$SECONDS
  shift 2
  note "waiting up to ${limit}s for $what"
  while ((SECONDS - started < limit)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  note "gave up after ${limit}s waiting for $what"
  return 1
}

# proxy_count LABEL: how many model calls of LABEL the recording proxy has finished.
proxy_count() { jq -s --arg l "$1" '[.[] | select(.label == $l)] | length' "$evidence/model-calls.jsonl" 2>/dev/null || printf '0\n'; }
# hold_at LABEL N lets LABEL's calls through up to ordinal N and holds the next (recording proxy);
# held LABEL N: the call of ordinal N is waiting.
hold_at() { printf '%s\n' "$2" >"$work/proxy-hold/$1"; }
held() { grep -qx "$2" "$work/proxy-hold/$1.held" 2>/dev/null; }
release() { rm -f "$work/proxy-hold/$1"; }

# pr_comments NUMBER MARKER prints the smoke repository's comments on the pull request that hold
# MARKER, as {id, user, body}.
pr_comments() {
  timeout 60 gh api "repos/$repo/issues/$1/comments?per_page=100" |
    jq -c --arg m "$2" '[.[] | select(.body | contains($m)) | {id, user: .user.login, body}]'
}

check_a1() {
  criterion A1 "a question on a parked tree's architect's ask is answered in the thread within four minutes, nothing of the tree starting" || return 0
  local asks ask before panes_before panes_after reply
  asks=$(dispatch_get "issues/${agent[$store:root]}/asks?thread=1")
  ask=$(jq -r --arg s "${agent[$store:architect.session]}" '[(.asks // .)[] | select(.author.id == $s)] | last | .id // empty' <<<"$asks")
  [ -n "$ask" ] || {
    record_result A1 FAIL "the parked architect opened no ask on ${agent[$store:root]}"
    return 0
  }
  panes_before=$(TMUX_TMPDIR="$work/tmux" "$tmux_bin" -L "legion-${project,,}" list-panes -a -F '#{pane_id}' 2>/dev/null | wc -l)
  before=$(date +%s)
  dispatch_human POST "issues/${agent[$store:root]}/comments" "$(jq -cn --arg ask "$ask" --arg body "$(question_text architect-decision)" \
    '{body: $body, ask_id: $ask}')" >/dev/null
  thread() { dispatch_get "asks/$ask" | jq -e --arg s "${agent[$store:architect.session]}" '[.replies[] | select(.author.id == $s)] | length > 0'; }
  if soft_wait 240 "the architect's answer in ask $ask's thread" thread; then
    reply=$(dispatch_get "asks/$ask" | jq -r --arg s "${agent[$store:architect.session]}" '[.replies[] | select(.author.id == $s)] | last | .body')
    panes_after=$(TMUX_TMPDIR="$work/tmux" "$tmux_bin" -L "legion-${project,,}" list-panes -a -F '#{pane_id}' 2>/dev/null | wc -l)
    note "answered after $(($(date +%s) - before)) s: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    note "claims of the tree: $(claims_cmd list --json | jq -c --arg t "${agent[$store:root]}" '[.claims[] | select(.tree == $t) | {role, state}]'); panes $panes_before → $panes_after"
    if answer_holds architect-decision "$reply" >/dev/null && [ "$panes_after" -le "$panes_before" ] &&
      claims_cmd list --json | jq -e --arg t "${agent[$store:root]}" 'all(.claims[] | select(.tree == $t); .state | IN("suspended", "retired"))' >/dev/null; then
      record_result A1 PASS "answered in the thread as ${agent[$store:architect.session]} within four minutes; no claim or pane of the tree started"
    else
      record_result A1 FAIL "the answer lacks its key, or a claim or pane of the tree started"
    fi
  else
    record_result A1 FAIL "no answer in ask $ask's thread within four minutes"
  fi
}

check_a2() {
  criterion A2 "a finished tester's role topic, its replaced session and a conversation known only from the title scan each answer instead of a 404" || return 0
  local passed=0 code
  ask_and_expect "role ${agent[$store:tester.token]}" role "${agent[$store:tester.token]}" tester-role && passed=$((passed + 1))
  ask_and_expect "replaced session ${agent[$store:tester_old.session]}" session "${agent[$store:tester_old.session]}" tester-old-session && passed=$((passed + 1))
  ask_and_expect "pre-change session ${agent[$store:pre_change.session]}" session "${agent[$store:pre_change.session]}" pre-change-scan && passed=$((passed + 1))
  code=$(envoy -o "$work/a2-control.json" -w '%{http_code}' --data "$(jq -cn --arg s "legion-e2eal-unregistered-$$" \
    '{target_session: $s, source: "agent", message: "control: nobody registered this session"}')" "$(listener_url /v1/messages/send)")
  note "control: an unregistered session answers $code: $(cat "$work/a2-control.json")"
  if [ "$passed" = 3 ] && [ "$code" = 404 ]; then
    record_result A2 PASS "role, replaced session and scanned conversation answered on the asker's topic; an unregistered session still 404"
  else
    record_result A2 FAIL "$passed of 3 answered with their keys; the unregistered control answered $code"
  fi
}

# check_a3: a person closes the tester's issue, finds its tester through the issue's collaborators
# (the Legion agent registry's entries for the issue, the list the mention picker offers stopped
# agents from), and asks on the closed issue's own thread with a role mention of that tester; the
# answer is the tester's reply to that comment, in its own name, holding what only the replacing
# tester's conversation holds. Control: a role mention of a token no registry entry names, on the
# same closed issue, fails its delivery as today. The second leg, a message from the tester's
# conversation page reached through the Collaborators view, waits on INFRA-470.
check_a3() {
  criterion A3 "a question on a closed issue's own thread, by a role mention of its stopped tester, is answered there" infra448 || return 0
  local issue=${agent[$store:tester.issue]} session=${agent[$store:tester.session]} collaborators token comment id reply='' control before
  dispatch_human PATCH "issues/$issue" '{"status":"done"}' >/dev/null
  note "closed $issue: status $(dispatch_get "issues/$issue" | jq -r .status)"
  collaborators=$(dispatch_get "issues/$issue/collaborators")
  note "collaborators of $issue: registry $(jq -r .legion_registry <<<"$collaborators"); agents $(jq -c '[.agents[] | {session_id, legion: (.legion | if . then {role, role_token, replaced_by} else null end)}]' <<<"$collaborators")"
  token=$(jq -r --arg s "$session" '[.agents[] | select(.session_id == $s and .legion.role == "tester" and .legion.replaced_by == null) | .legion.role_token] | first // empty' <<<"$collaborators")
  if [ -z "$token" ]; then
    record_result A3 FAIL "the collaborators of closed $issue name no current tester $session with its role token"
    record_result A3 WAITING "(the conversation-page leg through the Collaborators view) needs INFRA-470"
    return 0
  fi
  before=$(date +%s)
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg body "@tester $(question_text tester-closed-thread) (question tester-closed-thread of run $run_tag)" \
    --arg t "role:$token" '{body: $body, mentions: [{target: $t}]}')")
  id=$(jq -r .id <<<"$comment")
  agent[$store:a3.comment]=$id
  note "the person's comment $id on closed $issue mentions role:$token; deliveries $(jq -c '[.deliveries[]? | {target, state, session_id, error}]' <<<"$comment")"
  answered() { dispatch_get "issues/$issue/comments" | jq -e --arg id "$id" --arg s "$session" 'any(.[]; .reply_to == $id and .author.id == $s)'; }
  if soft_wait 240 "the tester's reply to comment $id" answered; then
    reply=$(dispatch_get "issues/$issue/comments" | jq -r --arg id "$id" --arg s "$session" '[.[] | select(.reply_to == $id and .author.id == $s)] | last | .body')
    note "answered after $(($(date +%s) - before)) s as $session: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  fi
  control=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg t "role:legion-e2eal-nobody-$run_tag-tester" \
    '{body: "control: a role nobody registered", mentions: [{target: $t}]}')" | jq -c '[.deliveries[]? | {state, error}]')
  note "control: a mention of an unregistered role on closed $issue: $control"
  if [ -n "$reply" ] && answer_holds tester-closed-thread "$reply" >/dev/null &&
    jq -e --arg s "$session" '.deliveries[0].state == "sent" and .deliveries[0].session_id == $s' <<<"$comment" >/dev/null &&
    jq -e 'length == 1 and .[0].state == "failed"' <<<"$control" >/dev/null; then
    record_result A3 PASS "the role mention on closed $issue reached tester $session, which answered in the thread from its conversation; an unregistered role's mention failed as today"
  else
    record_result A3 FAIL "no reply with the answer key from $session to comment $id, or its delivery was not sent to it, or the control did not fail"
  fi
  record_result A3 WAITING "(the conversation-page leg through the Collaborators view) needs INFRA-470"
}

# passed_to_controller EVENT REASON: legion state lists a question passed on for REASON whose id or
# reference names EVENT.
passed_to_controller() {
  legion_cmd state --json | jq -e --arg q "$1" --arg r "$2" 'any(.questionsForController[]?; ((.id + " " + .ref) | contains($q)) and .reason == $r)' >/dev/null
}
check_passed_on() {
  local id=$1 crit=$2 kind expect sent code event line reply
  expect=$(question "$id" | jq -r .expect_kind)
  criterion "$crit" "$(question_text "$id")" || return 0
  sent=$(envoy_ask session "${agent[$store:planner.session]}" "$id")
  code=${sent%% *}
  event=${sent#* }
  if [ "$code" != 200 ] || ! soft_wait 240 "the planner's reply to $id" inbox_reply "$event"; then
    record_result "$crit" FAIL "the listener answered $code, or no reply reached the asker"
    return 0
  fi
  reply=$(inbox_reply "$event")
  line=$(boot_log | jq -c 'select(.msg == "answers: answered")' | tail -1)
  kind=$(jq -r .kind <<<"$line")
  note "reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
  note "answered line: $line"
  note "questionsForController: $(legion_cmd state --json | jq -c '.questionsForController')"
  if [ "$kind" = "$expect" ] && soft_wait 60 "the question in questionsForController" passed_to_controller "$event" "$expect" &&
    [ "$(claim_json "${agent[$store:planner.token]}" | jq -r .state)" = retired ]; then
    record_result "$crit" PASS "replied as $expect, passed to the controller with the asker; the planner's claim stayed retired"
  else
    record_result "$crit" FAIL "kind $kind (want $expect), or not listed in questionsForController, or the claim moved"
  fi
}

# ask_of SESSION ISSUE prints the id of the newest ask SESSION opened on ISSUE, empty for none;
# ask_reply_by ASK SESSION prints SESSION's newest reply on ASK's thread, or fails.
ask_of() {
  dispatch_get "issues/$2/asks?thread=1" | jq -r --arg s "$1" '[(.asks // .)[] | select(.author.id == $s)] | last | .id // empty'
}
ask_reply_by() { dispatch_get "asks/$1" | jq -e -r --arg s "$2" '[.replies[] | select(.author.id == $s)] | last | .body'; }
# notices_in_conversation NAME NEEDLE: how many entries of NAME's conversation that are not its own
# replies hold NEEDLE (what reached it, as prompts and notices).
notices_in_conversation() {
  conversation_text "$1" | jq -R -s --arg n "$2" '[split("\n")[] | fromjson? | select((.message.role // "") != "assistant") | tojson | select(contains($n))] | length'
}

# decide_and_ask LEG NAME QUESTION_ID ROUTE: a person replies on the ask agent NAME opened, with a
# decision that also asks something (question QUESTION_ID). The agent's answer must come on the ask's
# thread in its name, holding the answer key, and the daemon must pass the decision on once, to ROUTE
# ("the controller", read on the controller stand-in's topic, or an architect NAME, read in its
# conversation), and to nobody else, a minute later still once. It prints the leg's evidence and
# returns 0 when all of that holds.
decide_and_ask() {
  local leg=$1 name=$2 id=$3 route=$4 session issue ask comment reply lines delivered want_to ok=0
  session=${agent[$store:$name.session]}
  issue=${agent[$store:$name.issue]}
  ask=$(ask_of "$session" "$issue")
  if [ -z "$ask" ]; then
    note "$leg: $name ($session) opened no ask on $issue"
    return 1
  fi
  comment=$(dispatch_human POST "issues/$issue/comments" "$(jq -cn --arg ask "$ask" --arg body "$(question_text "$id") (question $id of run $run_tag)" \
    '{body: $body, ask_id: $ask}')" | jq -r .id)
  note "$leg: the person's reply $comment on $name's ask $ask on $issue decides and asks"
  soft_wait 240 "$name's answer on ask $ask's thread" ask_reply_by "$ask" "$session" || true
  reply=$(ask_reply_by "$ask" "$session" 2>/dev/null || true)
  note "$leg: $name's answer as $session: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
  if [ "$route" = "the controller" ]; then
    delivered() { more_than 0 controller_notices agent-decision "$comment"; }
  else
    delivered() { more_than 0 notices_in_conversation "$route" "$comment"; }
  fi
  soft_wait 240 "the decision to reach $route" delivered || true
  sleep 60
  lines=$(boot_log | jq -s -c --arg d "dispatch:$comment" '[.[] | select(.msg == "answers: decision passed on" and .decision == $d) | {decision, to, issue, role, session, asker, ref}]')
  if [ "$route" = "the controller" ]; then
    delivered=$(controller_notices agent-decision "$comment")
  else
    delivered=$(notices_in_conversation "$route" "$comment")
  fi
  note "$leg: the daemon's decision lines: $lines"
  note "$leg: agent-decision notices holding $comment at $route: $delivered; at the controller stand-in: $(controller_notices agent-decision "$comment")"
  want_to=$route
  [ "$route" = "the controller" ] || want_to=${agent[$store:$route.token]}
  if [ -n "$reply" ] && answer_holds "$id" "$reply" >/dev/null && [ "$delivered" = 1 ] &&
    jq -e --arg to "$want_to" 'length == 1 and .[0].to == $to' <<<"$lines" >/dev/null &&
    { [ "$route" = "the controller" ] || [ "$(controller_notices agent-decision "$comment")" = 0 ]; }; then
    ok=1
  fi
  [ "$ok" = 1 ]
}

# check_a6: A6's two legs. The first: a person's reply on the replacing tester's ask (a child of the
# parked tree, its architect suspended) decides and asks; the decision reaches the controller once,
# since nobody in that tree runs. The second: a tree whose root architect runs, with a child parked
# (its tester opened an ask and was suspended); a person's reply there reaches that architect once,
# as an agent-decision notice, and not the controller. The daemon routes a notice only through the
# trees it records, and the rig's issues are a person's (no `legion` label, so the workflow records
# none of them): the running tree's two issues are written into the daemon's record with the daemon
# stopped, as admission writes a root and the child it records under it (status in_progress, the
# event sequence Dispatch is at, so the boot listing changes nothing), a departure like A19's. Each
# leg's question gets its own answer on the ask's thread.
check_a6() {
  criterion A6 "a decision on a parked tree's child and on a parked child of a running tree reaches the controller or architect once, and its question is answered" || return 0
  local root6 child6 issue seq architect ok1=0 ok2=0
  decide_and_ask "parked tree's child tester" tester a6-tester-decision "the controller" && ok1=1
  root6=$(new_rig_issue "Answer loop proof: a running tree (A6, $store)")
  child6=$(new_rig_issue "Answer loop proof child: parked under a running tree (A6)" "$root6")
  stop_daemon
  for issue in "$root6" "$child6"; do
    seq=$(dispatch_events "$issue" | jq -s '[.[].seq] | max // 0')
    pg_sql "$daemon_db" "insert into issues (key, tree, project, title, parent, phase, generation, status, rank, handed_over, last_dispatch_seq, dispatch_status, repo, default_branch, github_repo)
      values ('$issue', '$root6', '$project', 'A6 $issue', $([ "$issue" = "$root6" ] && printf null || printf "'%s'" "$root6"), 'implementing', 1, 'in_progress', 'a', true, $seq, 'in_progress', '$repo', 'main', '$repo')" >/dev/null
  done
  note "recorded the running tree $root6 and its child $child6 in the daemon's issues table"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$root/packages/claude-envoy"
  architect=$(claim_token_of "$root6" architect)
  provision_workspace "$root6"
  claims_cmd spawn --json --tree "$root6" --issue "$root6" --role architect --prompt-file "$(write_prompt architect "$root6")" >/dev/null
  agent[$store:a6_architect.token]=$architect
  agent[$store:a6_architect.issue]=$root6
  soft_wait 600 "the running tree's architect to hold its role" claim_is "$architect" '.state | IN("ready", "idle")' || true
  agent[$store:a6_architect.session]=$(claim_json "$architect" | jq -r .session)
  agent[$store:a6_architect.file]=$(claim_json "$architect" | jq -r .sessionFile)
  spawn_agent a6_child tester "$root6" "$child6" \
    "$(jq -r .agents.a6_child.task "$questions_file") Then make exactly one tool call: open one Dispatch ask on $child6 asking a person whether to quarantine TestGarnetFalcon, and wait."
  await_turn a6_child
  if suspend_agent a6_child; then
    note "the parked child's tester ${agent[$store:a6_child.session]} is suspended; the tree's architect $architect is $(claim_json "$architect" | jq -c '{state, session}')"
    decide_and_ask "parked child of a running tree" a6_child a6-child-decision a6_architect && ok2=1
  else
    note "the parked child's tester was not suspended, or its session stayed live"
  fi
  claims_cmd suspend --claim "$architect" >/dev/null || true
  if [ "$ok1$ok2" = 11 ]; then
    record_result A6 PASS "each decision reached its route once (the parked tree's: the controller; the running tree's parked child's: its architect $architect, not the controller), and each question was answered on its ask's thread"
  else
    record_result A6 FAIL "parked tree's child $ok1, parked child of a running tree $ok2 (1 = answered, and the decision passed once to its route)"
  fi
}

# suspend_agent NAME suspends NAME's claim and waits until its session has left the listener's
# session bucket (session_gone), so the next message to it is handed to the daemon rather than sent
# to the pane that is gone; it fails (returns 1) when either does not happen in time.
suspend_agent() {
  local token=${agent[$store:$1.token]}
  claims_cmd suspend --claim "$token" >/dev/null || return 1
  soft_wait 300 "$1 to be suspended" claim_is "$token" '.state == "suspended"' &&
    soft_wait "$session_ttl_wait" "$1's session to leave the listener's registry" session_gone "${agent[$store:$1.session]}"
}
# resume_agent NAME resumes NAME's claim on its kept conversation and waits until it holds its
# session in the listener again.
resume_agent() {
  local token=${agent[$store:$1.token]}
  claims_cmd resume --claim "$token" >/dev/null || return 1
  soft_wait 300 "$1 to run again" claim_is "$token" '.state | IN("ready", "idle")' &&
    soft_wait 120 "$1's session to be live in the listener" session_live "${agent[$store:$1.session]}"
}

check_a7() {
  criterion A7 "a question to a running agent reaches it alone; a resume during generation appends nothing and forwards the question" || return 0
  local token=${agent[$store:architect.token]} label=daemon-$store before after sent event n first_ok=0 second_ok=0
  if ! resume_agent architect; then
    record_result A7 FAIL "the architect did not run again"
    return 0
  fi
  before=$(boot_log | jq -s '[.[] | select(.msg | startswith("answers:"))] | length')
  sent=$(envoy_ask session "${agent[$store:architect.session]}" in-order-1)
  holds_first() { conversation_text architect | grep -qF "question in-order-1 of run $run_tag"; }
  soft_wait 180 "the running architect's conversation to hold the question" holds_first || true
  sleep 20
  after=$(boot_log | jq -s '[.[] | select(.msg | startswith("answers:"))] | length')
  note "to the running architect: listener $sent; the daemon's answers lines $before → $after; its conversation holds the question: $(holds_first && echo yes || echo no)"
  [ "$before" = "$after" ] && [ "${sent%% *}" = 200 ] && holds_first && first_ok=1
  if ! suspend_agent architect; then
    record_result A7 FAIL "the architect was not suspended, or its session stayed live"
    return 0
  fi
  n=$(proxy_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask session "${agent[$store:architect.session]}" in-order-2)
  event=${sent#* }
  if ! soft_wait 240 "the loop's model call to be held at the proxy" held "$label" "$((n + 1))"; then
    release "$label"
    record_result A7 FAIL "the loop made no model call for the question to the stopped architect (listener $sent)"
    return 0
  fi
  claims_cmd resume --claim "$token" >/dev/null
  soft_wait 300 "the architect to start" claim_is "$token" '.state | IN("launching", "shim_connected", "registered", "ready", "working", "idle")' || true
  release "$label"
  dropped() { boot_log | jq -e -s 'any(.[]; .msg == "answers: the agent'"'"'s conversation is running or moved; the answer is dropped and the question forwarded" and .started == true)'; }
  delivered() { conversation_text architect | grep -qF "question in-order-2 of run $run_tag"; }
  soft_wait 300 "the answer to be dropped and the question forwarded" dropped || true
  soft_wait 300 "the running architect to receive the forwarded question" delivered || true
  note "the loop: $(boot_log | jq -s -c '[.[] | select((.msg | startswith("answers:")) and (.session // "") == "'"${agent[$store:architect.session]}"'") | {msg, state, started}] | .[-6:]')"
  note "the running architect holds the forwarded question: $(delivered && echo yes || echo no); legion-answer entries: $(conversation_text architect | grep -c '"customType":"legion-answer"' || true); replies on the asker's topic: $(inbox_replies "$event")"
  dropped && delivered && [ "$(inbox_replies "$event")" = 0 ] && ! conversation_text architect | grep -qF '"customType":"legion-answer"' && second_ok=1
  if [ "$first_ok$second_ok" = 11 ]; then
    record_result A7 PASS "the running architect alone took the first question (no answers line); the second, its claim started mid-answer, was dropped and forwarded with nothing appended or posted"
  else
    record_result A7 FAIL "running agent alone $first_ok, started mid-answer forwarded with nothing appended $second_ok (1 = shown)"
  fi
  suspend_agent architect || true
}

# answers_lines MSG [SESSION [STATE]]: how many lines of this boot's daemon log carry MSG, for
# SESSION and in STATE when given.
answers_lines() {
  boot_log | jq -s --arg m "$1" --arg s "${2:-}" --arg st "${3:-}" \
    '[.[] | select(.msg == $m and ($s == "" or .session == $s) and ($st == "" or .state == $st))] | length'
}

check_a8() {
  criterion A8 "one question delivered twice, across a restart and across a kill just after its answer posted, gets one answer; the loop's own answer and an edit are not answered" || return 0
  local planner=${agent[$store:planner.session]} key=a8-$run_tag key3=a8-kill-$run_tag sent event event3 again posted_before own_before edit_before ok_twice=0 ok_kill=0 ok_own=0 ok_edit=0
  sent=$(envoy_ask session "$planner" unanswerable "$key")
  event=${sent#* }
  envoy_ask session "$planner" unanswerable "$key" >/dev/null
  soft_wait 240 "the answer" inbox_reply "$event" || true
  stop_daemon
  start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  envoy_ask session "$planner" unanswerable "$key" >/dev/null
  soft_wait 60 "the restarted daemon to take the hand-off again" more_than 0 answers_lines "answers: question handed off again; not answered again" "$planner" || true
  sleep 20
  again=$(answers_lines "answers: question handed off again; not answered again" "$planner")
  note "one envelope three times, the third across a restart: replies $(inbox_replies "$event"); handed off again after the restart: $again"
  [ "$(inbox_replies "$event")" = 1 ] && [ "$again" -ge 1 ] && ok_twice=1
  # A kill just after the answer posted: the asker's inbox is polled every tenth of a second and the
  # daemon killed the moment the reply lands, then the same envelope is delivered again.
  sent=$(envoy_ask session "$planner" unanswerable "$key3")
  event3=${sent#* }
  for _ in $(seq 1 3000); do
    [ "$(inbox_replies "$event3")" = 0 ] || break
    sleep 0.1
  done
  kill -KILL "$daemon_pid"
  daemon_pid=
  note "killed the daemon $(inbox_replies "$event3") reply after its post; the question's last state before the kill: $(boot_log | jq -s -c --arg s "$planner" '[.[] | select(.msg == "answers: question" and .session == $s) | .state] | last')"
  start_daemon "$work/branch/legion" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
  envoy_ask session "$planner" unanswerable "$key3" >/dev/null
  sleep 60
  note "after the kill and restart: replies $(inbox_replies "$event3"); this boot's lines for the planner: $(boot_log | jq -s -c --arg s "$planner" '[.[] | select((.msg | startswith("answers:")) and .session == $s) | {msg, state}]')"
  [ "$(inbox_replies "$event3")" = 1 ] && ok_kill=1
  # The loop's own answer: a question to the planner sent as the stopped, replaced tester session;
  # the planner's answer goes to that session in the planner's name, and is never answered.
  posted_before=$(answers_lines "answers: question" "$planner" posted)
  own_before=$(answers_lines "answers: question" "${agent[$store:tester_old.session]}")
  envoy -o /dev/null --data "$(jq -cn --arg s "$planner" --arg from "${agent[$store:tester_old.session]}" --arg m "$(question_text repository-file) (question own-answer of run $run_tag)" \
    '{target_session: $s, source: "agent", source_session: $from, message: $m}')" "$(listener_url /v1/messages/send)"
  soft_wait 300 "the planner's answer to the tester's session to post" more_than "$posted_before" answers_lines "answers: question" "$planner" posted || true
  sleep 60
  note "the planner's answers posted $posted_before → $(answers_lines "answers: question" "$planner" posted); questions for the replaced tester $own_before → $(answers_lines "answers: question" "${agent[$store:tester_old.session]}")"
  [ "$(answers_lines "answers: question" "$planner" posted)" -gt "$posted_before" ] &&
    [ "$(answers_lines "answers: question" "${agent[$store:tester_old.session]}")" = "$own_before" ] && ok_own=1
  # An edit: the person edits the comment A3's question was, on the closed issue.
  if [ -n "${agent[$store:a3.comment]:-}" ]; then
    edit_before=$(answers_lines "answers: question" "${agent[$store:tester.session]}")
    dispatch_human PATCH "comments/${agent[$store:a3.comment]}" "$(jq -cn --arg b "@tester $(question_text tester-closed-thread) (edited)" '{body: $b}')" >/dev/null
    sleep 60
    note "after the person edited comment ${agent[$store:a3.comment]}: questions for the tester $edit_before → $(answers_lines "answers: question" "${agent[$store:tester.session]}")"
    [ "$(answers_lines "answers: question" "${agent[$store:tester.session]}")" = "$edit_before" ] && ok_edit=1
  fi
  if [ "$ok_twice$ok_kill$ok_own$ok_edit" = 1111 ]; then
    record_result A8 PASS "one envelope delivered three times across a restart: one answer; killed just after the post and delivered again: one answer; the loop's own answer and an edit were not answered"
  else
    record_result A8 FAIL "twice and restart $ok_twice, kill after post $ok_kill, own answer unanswered $ok_own, edit unanswered $ok_edit (1 = shown)"
  fi
}

# first_request_after LABEL N prints the request body of LABEL's first model call after ordinal N.
first_request_after() {
  jq -s -c --arg l "$1" --argjson n "$2" '[.[] | select(.label == $l and .ordinal > $n)] | sort_by(.ordinal) | (.[0].request_body // null)' "$evidence/model-calls.jsonl"
}
# occurrences TEXT NEEDLE: how many times NEEDLE occurs in TEXT.
occurrences() { grep -oF -- "$2" <<<"$1" | wc -l; }

check_a9() {
  criterion A9 "two questions at once are answered in order, and the resumed agent holds both once and carries on from them" || return 0
  local token=${agent[$store:architect.token]} t1 t2 s1 s2 e1 e2 first reply m request a1 a2 counts
  t1="$(question_text in-order-1) (question in-order-1 of run $run_tag, A9)"
  t2="$(question_text in-order-2) (question in-order-2 of run $run_tag, A9)"
  s1=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t1")
  s2=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t2")
  e1=${s1#* }
  e2=${s2#* }
  both() { inbox_reply "$e1" && inbox_reply "$e2"; }
  soft_wait 300 "both answers" both || {
    record_result A9 FAIL "not both questions were answered (listener $s1; $s2)"
    return 0
  }
  first=$(jq -s -r --arg a "In Reply To: $e1" --arg b "In Reply To: $e2" '[.[] | select((.text | contains($a)) or (.text | contains($b)))] | .[0].text' "$evidence/inbox.jsonl")
  a1=$(inbox_message "$e1" | head -1 | head -c 60)
  a2=$(inbox_message "$e2" | head -1 | head -c 60)
  note "answers: 1: $(inbox_message "$e1" | head -c 200 | tr '\n' ' ') | 2: $(inbox_message "$e2" | head -c 200 | tr '\n' ' ')"
  m=$(proxy_count agents)
  resume_agent architect || true
  claims_cmd deliver --claim "$token" --task "$(question_text after-resume)" >/dev/null
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  soft_wait 600 "the resumed architect's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  request=$(first_request_after agents "$m")
  counts="question 1 $(occurrences "$request" "in-order-1 of run $run_tag, A9"), question 2 $(occurrences "$request" "in-order-2 of run $run_tag, A9"), answer 1 $(occurrences "$request" "$(jq -Rr @json <<<"$a1" | sed 's/^"//; s/"$//')"), answer 2 $(occurrences "$request" "$(jq -Rr @json <<<"$a2" | sed 's/^"//; s/"$//')")"
  reply=$(conversation_text architect | jq -R -r 'fromjson? | select(.type == "message" and .message.role == "assistant") | [.message.content[]? | .text? // empty] | join(" ")' | tail -1)
  note "the first reply on the asker's topic answers question 1: $(grep -qF "In Reply To: $e1" <<<"$first" && echo yes || echo no)"
  note "the resumed architect's first request (agents, after ordinal $m) holds: $counts"
  note "the resumed architect: $(head -c 400 <<<"$reply" | tr '\n' ' ')"
  suspend_agent architect || true
  if grep -qF "In Reply To: $e1" <<<"$first" && [ "$counts" = "question 1 1, question 2 1, answer 1 1, answer 2 1" ] &&
    answer_holds after-resume "$reply" >/dev/null; then
    record_result A9 PASS "answered one after the other; the resumed architect's first request held each question and answer once, and it quoted both"
  else
    record_result A9 FAIL "out of order, or the first request did not hold each exchange once ($counts), or the resumed agent did not carry on from them"
  fi
}

# daemon_request_for TEXT prints the request body of the loop's first model call (this store's
# daemon label) whose body holds TEXT.
daemon_request_for() {
  jq -s -c --arg l "daemon-$store" --arg t "$1" '[.[] | select(.label == $l and (.request_body | tostring | contains($t)))] | sort_by(.ordinal) | (.[0].request_body // null)' "$evidence/model-calls.jsonl"
}

# check_a10: A10's Claude Code leg (the pre-change reviewer, answered twice within the cache
# lifetime: the first request carries the effort the transcript last ran at and every thinking
# signature it holds, byte for byte; the second reads the conversation from cache; resumed after,
# its first request holds both exchanges) and its compaction leg (the architect compacts its
# conversation on a person's /compact, is answered from the compacted conversation, and resumes
# after holding the exchange). The GPT-6.1 leg waits on the OpenAI API.
check_a10() {
  criterion A10 "a GPT-6.1 conversation's reasoning replayed byte-equal and its cache read" openai || true
  criterion A10 "a Claude Code worker and a compacted Oh My Pi conversation are answered from their conversations and resume after" || return 0
  local session=${agent[$store:pre_change.session]} token=${agent[$store:pre_change.token]} t1 t2 s1 s2 e1 e2 line request transcript effort want_effort sigs missing m counts ok_claude=0 ok_compact=0 t3 s3 e3
  transcript=$(conversation_text pre_change)
  want_effort=$(jq -R -r 'fromjson? | select(.type == "assistant") | .effort // empty' <<<"$transcript" | tail -1)
  sigs=$(jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "thinking") | .signature // empty' <<<"$transcript")
  t1="$(question_text pre-change-scan) (question pre-change-scan of run $run_tag, A10-1)"
  t2="$(question_text pre-change-scan) (question pre-change-scan of run $run_tag, A10-2)"
  s1=$(envoy_ask_text session "$session" "$t1")
  e1=${s1#* }
  soft_wait 240 "the first answer" inbox_reply "$e1" || true
  s2=$(envoy_ask_text session "$session" "$t2")
  e2=${s2#* }
  soft_wait 240 "the second answer" inbox_reply "$e2" || true
  line=$(boot_log | jq -c --arg s "$session" 'select(.msg == "answers: answered" and .session == $s)' | tail -1)
  request=$(daemon_request_for "A10-1)")
  effort=$(jq -r '[.. | objects | .effort? // empty] | first // ""' <<<"$request")
  missing=0
  while IFS= read -r sig; do
    [ -z "$sig" ] || grep -qF -- "$sig" <<<"$request" || missing=$((missing + 1))
  done <<<"$sigs"
  note "the transcript's last effort \"$want_effort\", the first answer's request effort \"$effort\"; the transcript's thinking signatures $(grep -c . <<<"$sigs" || true), missing from the request $missing"
  note "answers: $(inbox_reply "$e1" | head -c 200 | tr '\n' ' ') | $(inbox_reply "$e2" | head -c 200 | tr '\n' ' ')"
  note "the second answer's cost line: $line"
  m=$(proxy_count "claude-$store")
  resume_agent pre_change || true
  claims_cmd deliver --claim "$token" --task "While you were stopped you were asked twice what blocked the release. How many times were you asked, and what did you answer?" >/dev/null
  soft_wait 300 "the resumed Claude Code reviewer's first model call" more_than "$m" proxy_count "claude-$store" || true
  soft_wait 600 "the resumed reviewer's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
  request=$(first_request_after "claude-$store" "$m")
  counts="$(occurrences "$request" "A10-1)") $(occurrences "$request" "A10-2)")"
  note "the resumed Claude Code reviewer's first request (claude-$store, after ordinal $m) holds the two questions: $counts"
  suspend_agent pre_change || true
  if answer_holds pre-change-scan "$(inbox_reply "$e1")" >/dev/null && answer_holds pre-change-scan "$(inbox_reply "$e2")" >/dev/null &&
    [ -n "$want_effort" ] && [ "$effort" = "$want_effort" ] && [ -n "$sigs" ] && [ "$missing" = 0 ] &&
    jq -e '.cacheReadTokens > 0' <<<"$line" >/dev/null && [ "$counts" = "1 1" ]; then
    ok_claude=1
  fi
  # The compaction leg: a person's /compact from the architect's conversation page, typed input the
  # running pane runs as Oh My Pi's command.
  resume_agent architect || true
  dispatch_human POST "agents/${agent[$store:architect.session]}/messages" '{"body": "/compact", "delivery": "steer"}' >/dev/null || true
  compacted() { conversation_text architect | grep -qF '"type":"compaction"'; }
  soft_wait 300 "the architect's conversation to hold a compaction" compacted || true
  note "the architect's conversation holds a compaction: $(compacted && echo yes || echo no)"
  suspend_agent architect || true
  t3="$(question_text in-order-2) (question in-order-2 of run $run_tag, A10 after compaction)"
  s3=$(envoy_ask_text session "${agent[$store:architect.session]}" "$t3")
  e3=${s3#* }
  soft_wait 240 "the answer from the compacted conversation" inbox_reply "$e3" || true
  note "from the compacted conversation: $(inbox_reply "$e3" 2>/dev/null | head -c 300 | tr '\n' ' ')"
  m=$(proxy_count agents)
  resume_agent architect || true
  claims_cmd deliver --claim "${agent[$store:architect.token]}" --task "Reply with one word: ready." >/dev/null
  soft_wait 300 "the resumed architect's first model call" more_than "$m" proxy_count agents || true
  request=$(first_request_after agents "$m")
  note "the resumed architect's first request holds the question asked after the compaction $(occurrences "$request" "A10 after compaction)") time(s)"
  compacted && inbox_reply "$e3" >/dev/null && answer_holds in-order-2 "$(inbox_reply "$e3")" >/dev/null &&
    [ "$(occurrences "$request" "A10 after compaction)")" = 1 ] && ok_compact=1
  suspend_agent architect || true
  if [ "$ok_claude$ok_compact" = 11 ]; then
    record_result A10 PASS "Claude Code: effort $effort and every thinking signature replayed, the second answer read $(jq -r .cacheReadTokens <<<"$line") tokens from cache, resumed holding both; the compacted conversation answered and resumed holding the exchange"
  else
    record_result A10 FAIL "Claude Code leg $ok_claude, compaction leg $ok_compact (1 = shown)"
  fi
}

check_a12() {
  criterion A12 "a reply past a bound or out of form is not posted; the daemon replies naming the fault and passes the question on" || return 0
  local entry bounds fault sent event reply passed=0
  for entry in "{ model_calls: 1 }|model_calls" "{ answer_seconds: 1 }|answer_seconds" "{ output_tokens: 16 }|length" "{}|malformed" "{ daily_usd: 0.01 }|over_limit"; do
    bounds=${entry%|*}
    fault=${entry#*|}
    stop_daemon
    write_legion_config "$work/a12-$store.yaml" yes "$bounds"
    [ "$fault" != malformed ] || jq -n '{type: "message", role: "assistant", content: [{type: "text", text: "not the reply form"}], stop_reason: "end_turn", usage: {input_tokens: 10, output_tokens: 5}}' >"$work/proxy-stand-in/daemon-$store.json"
    start_daemon "$work/branch/legion" "$work/a12-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$root/packages/claude-envoy"
    sent=$(envoy_ask session "${agent[$store:planner.session]}" repository-file "a12-$fault-$run_tag")
    event=${sent#* }
    passed_on() { boot_log | jq -e -s --arg f "$fault" 'any(.[]; .msg == "answers: question passed to the controller" and ((.reason // "") | contains($f)))'; }
    if soft_wait 300 "the $fault fault to be passed on" passed_on && soft_wait 60 "the daemon's reply" inbox_reply "$event"; then
      reply=$(inbox_reply "$event")
      note "$fault: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
      grep -qF 'not the reply form' <<<"$reply" || passed=$((passed + 1))
    fi
    rm -f "$work/proxy-stand-in/daemon-$store.json"
  done
  stop_daemon
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$root/packages/claude-envoy"
  if [ "$passed" = 5 ]; then
    record_result A12 PASS "each of model_calls, answer_seconds, length, malformed and over_limit ended with the daemon's own reply and the question passed on"
  else
    record_result A12 FAIL "$passed of 5 faults ended as they must"
  fi
}

check_a14() {
  criterion A14 "a cost line per answer, cache writes included, recomputed from the captured usage and the prices" || return 0
  local lines bad
  lines=$(boot_log | jq -c 'select(.msg == "answers: answered" and .fault == "")')
  bad=$(jq -s -c '[.[] | select(.model | test("claude-sonnet-5-5")) | select((((.inputTokens * 3) + (.outputTokens * 15) + (.cacheWriteTokens * 3.75) + (.cacheReadTokens * 0.3)) / 1000000 - .costUSD | fabs) > 0.000001)]' <<<"$lines")
  note "cost lines: $(jq -s -c 'map({question, model, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens, costUSD})' <<<"$lines")"
  if [ -n "$lines" ] && [ "$bad" = "[]" ]; then
    record_result A14 PASS "every answer's costUSD equals its tokens at the configured prices, cache writes included"
  else
    record_result A14 FAIL "no cost line, or a cost that does not recompute: $bad"
  fi
}

check_a15() {
  criterion A15 "an answer read from the repository with gh, the log naming App and owner, the call in the conversation; the same for the Claude Code worker" || return 0
  local gh_line ok_omp=0 ok_claude=0
  if ask_and_expect "planner, repository file" session "${agent[$store:planner.session]}" repository-file; then
    gh_line=$(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "planner")' | tail -1)
    note "gh line: $gh_line"
    jq -e '.owner == "sjawhar" and .app == "review"' <<<"$gh_line" >/dev/null && conversation_text planner | grep -qF 'stage4b-fixture' && ok_omp=1
  fi
  if ask_and_expect "Claude Code reviewer, repository file" session "${agent[$store:pre_change.session]}" repository-file; then
    gh_line=$(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "reviewer")' | tail -1)
    note "gh line: $gh_line"
    jq -e '.owner == "sjawhar" and .app == "review"' <<<"$gh_line" >/dev/null && conversation_text pre_change | grep -qF 'stage4b-fixture' && ok_claude=1
  fi
  if [ "$ok_omp$ok_claude" = 11 ]; then
    record_result A15 PASS "both answered from go.mod with gh as the review App on sjawhar; each conversation holds the call"
  else
    record_result A15 FAIL "Oh My Pi planner $ok_omp, Claude Code reviewer $ok_claude (1 = answered with the call in the log and the conversation)"
  fi
}

check_a16() {
  local pattern hits unrelated paths
  store=-
  printf '== A16: the search for the loop described as tool-less finds no line about the loop\n'
  pattern=$(jq -r '.a16.phrases | join("|")' "$questions_file")
  paths=(packages/daemon/internal/answer packages/daemon/internal/answerloop packages/daemon/internal/ghtool
    packages/daemon/internal/conversation packages/daemon/internal/agenthistory packages/daemon/internal/prompts skills docs)
  hits=$(cd "$root" && {
    grep -rniE --exclude-dir=node_modules --exclude-dir=dist --exclude-dir=.astro "$pattern" "${paths[@]}" | grep -vE '^docs/(plans|solutions)/' || true
    find . -name AGENTS.md -not -path '*/node_modules/*' -print0 | xargs -0 grep -niE "$pattern" /dev/null || true
  })
  printf '   $ grep -rniE "%s" %s (docs/plans and docs/solutions left out) and every AGENTS.md\n' "$pattern" "${paths[*]}"
  while IFS= read -r line; do [ -z "$line" ] || note "$line"; done <<<"$hits"
  unrelated=$(while IFS= read -r line; do
    [ -n "$line" ] || continue
    file=${line%%:*}
    text=${line#*:}
    text=${text#*:}
    jq -e --arg f "$file" --arg t "$text" 'any(.a16.unrelated_hits[]; .file == $f and (.text == $t or (.starts_with as $s | $s != null and ($t | startswith($s)))))' \
      "$questions_file" >/dev/null || printf '%s\n' "$line"
  done <<<"$hits")
  local test_out
  test_out=$(cd "$root/packages/daemon" && go test -count=1 ./internal/answer -run TestTheLoopsToolListHoldsGh -v 2>&1) || true
  note "$(grep -E '^(--- |ok|FAIL)' <<<"$test_out" | tr '\n' ' ')"
  if [ -z "$unrelated" ] && grep -q '^--- PASS: TestTheLoopsToolListHoldsGh' <<<"$test_out"; then
    record_result A16 PASS "every hit is one the questions file records as not about the loop; TestTheLoopsToolListHoldsGh passes"
  else
    record_result A16 FAIL "hits about the loop or unrecorded: ${unrelated:-none}; the unit test: $(grep -E '^(--- |ok|FAIL)' <<<"$test_out" | tr '\n' ' ')"
  fi
}

# implementer_with_pull_request makes, once per store, what A17 and A18 ask: a pull request on the
# smoke repository as the implement App (lib/smoke-pull-request.ts; recorded for cleanup) and a
# stopped implementer whose conversation holds it.
implementer_with_pull_request() {
  [ -z "${agent[$store:implementer.session]:-}" ] || return 0
  local issue branch pr token
  issue=$(new_rig_issue "Answer loop proof child: the implementation" "${agent[$store:root]}")
  branch=legion-e2eal/${issue,,}
  pr=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" open "$work/github-apps.yaml" "$repo" "$branch" \
    "Answer loop proof $issue (closed by the run)") || return 1
  agent[$store:implementer.pr]=$(jq -r .number <<<"$pr")
  github_writes+=("pr:${agent[$store:implementer.pr]}:$branch")
  note "opened $repo pull request #${agent[$store:implementer.pr]} from $branch as the implement App"
  spawn_agent implementer implementer "${agent[$store:root]}" "$issue" \
    "Record this and keep it: your pull request is #${agent[$store:implementer.pr]} on $repo, from the branch $branch. Reply with one sentence restating it. Run no tool."
  await_turn implementer
  token=${agent[$store:implementer.token]}
  claims_cmd suspend --claim "$token" >/dev/null
  soft_wait 120 "the implementer to be suspended" claim_is "$token" '.state == "suspended"' &&
    soft_wait "$session_ttl_wait" "the implementer's session to leave the registry" session_gone "${agent[$store:implementer.session]}"
}

# comment_request MARKER: the question asking the stopped implementer for a comment on its pull request.
comment_request() {
  printf 'Please post a comment on your pull request #%s on %s whose body is exactly: %s. Then tell me what you did.' \
    "${agent[$store:implementer.pr]}" "$repo" "$1"
}

check_a17() {
  criterion A17 "the stopped implementer comments on its pull request as the implement App; the reviewer's write to another owner is refused" second-owner || return 0
  local marker=answer-loop-a17-$run_tag-$store sent event comments gh_line reply refusal ok_impl=0 ok_review=0 id
  implementer_with_pull_request || {
    record_result A17 FAIL "no implementer with a pull request could be made"
    return 0
  }
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker")" "a17-$run_tag-$store")
  event=${sent#* }
  if soft_wait 300 "the implementer's reply" inbox_reply "$event"; then
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    gh_line=$(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "implementer")' | tail -1)
    reply=$(inbox_reply "$event")
    note "comments: $(jq -c 'map({id, user, footer: (.body | capture("legion: (?<f>\\{[^}]*\\})").f // null)})' <<<"$comments")"
    note "gh line: $gh_line"
    note "reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    jq -e --arg login "$implement_app_login" --arg s "${agent[$store:implementer.session]}" \
      'length == 1 and .[0].user == $login and (.[0].body | contains("\"session\":\"" + $s + "\""))' <<<"$comments" >/dev/null &&
      jq -e '.app == "implement" and .owner == "sjawhar" and .exit == 0' <<<"$gh_line" >/dev/null && ok_impl=1
  fi
  sent=$(envoy_ask_text session "${agent[$store:pre_change.session]}" \
    "Using your gh tool, create the file answer-loop-$marker.md on the default branch of $second_repo (gh api -X PUT repos/$second_repo/contents/answer-loop-$marker.md -f message=proof -f content=cHJvb2YK). Then tell me exactly what GitHub answered." \
    "a17-review-$run_tag-$store")
  event=${sent#* }
  if soft_wait 300 "the reviewer's reply" inbox_reply "$event"; then
    reply=$(inbox_reply "$event")
    gh_line=$(boot_log | jq -c --arg owner "${second_repo%%/*}" 'select(.msg == "answers: gh" and .role == "reviewer" and .owner == $owner)' | tail -1)
    note "reviewer's gh line: $gh_line"
    note "reviewer's reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    refusal=$(grep -ciE 'refus|denied|403|404|not accessible|permission' <<<"$reply" || true)
    jq -e '.app == "review" and .exit != 0' <<<"$gh_line" >/dev/null && [ "$refusal" -gt 0 ] && ok_review=1
  fi
  if [ "$ok_impl$ok_review" = 11 ]; then
    record_result A17 PASS "the implementer's one comment is the implement App's with its session's footer, logged with App and owner; the reviewer's write to the second owner was refused and it said so"
  else
    record_result A17 FAIL "implementer comment $ok_impl, reviewer refusal $ok_review (1 = shown)"
  fi
}

# hold_after_tool LABEL N: with LABEL held at N (hold_at), the loop's first model call (ordinal N+1)
# is let through and the one after its tool call (N+2) held, so the loop stands between the gh call
# and its reply.
hold_after_tool() {
  soft_wait 300 "the loop's first model call" held "$1" "$(($2 + 1))" || return 1
  hold_at "$1" "$(($2 + 1))"
  soft_wait 300 "the loop's model call after its gh call" held "$1" "$(($2 + 2))"
}

check_a18() {
  criterion A18 "a restart after the comment's gh call posts the answer once and the comment once; a resume at that point hands the agent the call" || return 0
  local label=daemon-$store marker=answer-loop-a18-$run_tag-$store sent event n comments token ok_restart=0 ok_resume=0 id
  implementer_with_pull_request || {
    record_result A18 FAIL "no implementer with a pull request could be made"
    return 0
  }
  token=${agent[$store:implementer.token]}
  n=$(proxy_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker-restart")" "a18-restart-$run_tag-$store")
  event=${sent#* }
  if hold_after_tool "$label" "$n"; then
    note "the comment's gh line before the kill: $(boot_log | jq -c 'select(.msg == "answers: gh" and .role == "implementer")' | tail -1)"
    kill -KILL "$daemon_pid"
    daemon_pid=
    start_daemon "$daemon_bin" "$daemon_config" "$daemon_log" "$daemon_home" "$daemon_profile" "$daemon_plugin_dir"
    release "$label"
    soft_wait 300 "the answer after the restart" inbox_reply "$event" || true
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker-restart")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    note "after the restart: comments $(jq length <<<"$comments"), replies $(inbox_replies "$event"), gh lookups $(boot_log | jq -s -c '[.[] | select(.msg == "answers: gh lookup")] | length')"
    [ "$(jq length <<<"$comments")" = 1 ] && [ "$(inbox_replies "$event")" = 1 ] && ok_restart=1
  fi
  release "$label"
  n=$(proxy_count "$label")
  hold_at "$label" "$n"
  sent=$(envoy_ask_text session "${agent[$store:implementer.session]}" "$(comment_request "$marker-resume")" "a18-resume-$run_tag-$store")
  event=${sent#* }
  if hold_after_tool "$label" "$n"; then
    claims_cmd resume --claim "$token" >/dev/null
    soft_wait 300 "the implementer to start" claim_is "$token" '.state | IN("launching", "shim_connected", "registered", "ready", "working", "idle")' || true
    release "$label"
    forwarded_count() { boot_log | jq -e -s 'any(.[]; .msg == "answers: question" and .state == "forwarded" and .role == "implementer")'; }
    soft_wait 300 "the question to be forwarded with its gh call" forwarded_count || true
    soft_wait 600 "the running implementer's turn" claim_is "$token" '.state == "idle" and .pending == null' || true
    comments=$(pr_comments "${agent[$store:implementer.pr]}" "$marker-resume")
    for id in $(jq -r '.[].id' <<<"$comments"); do github_writes+=("comment:$id"); done
    note "after the resume: comments $(jq length <<<"$comments"); the running implementer's conversation names the gh call: $(conversation_text implementer | grep -c "$marker-resume" || true)"
    [ "$(jq length <<<"$comments")" = 1 ] && forwarded_count && [ "$(inbox_replies "$event")" = 0 ] && ok_resume=1
    claims_cmd suspend --claim "$token" >/dev/null || true
  fi
  release "$label"
  if [ "$ok_restart$ok_resume" = 11 ]; then
    record_result A18 PASS "killed after the comment's gh call: one answer, one comment; resumed at that point: the agent got the call, one comment"
  else
    record_result A18 FAIL "restart $ok_restart, resume $ok_resume (1 = one comment and the answer or the hand-over as required)"
  fi
}

# review_record ISSUE prints the daemon's record of ISSUE's review round: its phase, the reviewer's
# recorded decision, and the newest deciding review its pull request has seen.
review_record() {
  pg_sql "$daemon_db" "select i.phase, p.decision, r.review_seen from issues i join phases p on p.issue = i.key and p.role = 'reviewer' join pull_requests r on r.issue = i.key where i.key = '$1'" | jq -c '.[0]'
}

# check_a19: a review round the daemon's record holds open on a pull request of the smoke
# repository, with its reviewer's completion recorded and the reviewer stopped. The record is
# written with the daemon stopped, as the workflow writes it when a reviewer completes undecided
# (issue in reviewing; the reviewer's phase row naming its claim, its handoff commit and completion
# time; the pull request at its head): driving a tree there through planning, implementing and
# testing with real agents is the work of the stage proofs, not of this one. The reviewer is a real
# Claude Code reviewer the daemon ran on that issue, its conversation holding its review. The
# control comes first: an approval of the head by the review App whose footer names another
# session decides nothing over two of the daemon's review reads. Then the stopped reviewer is asked
# to approve: its gh tool posts the approval with its recorded session's footer, and the daemon's
# next read of the pull request's reviews ends the round as the reviewer's running approval would.
check_a19() {
  criterion A19 "the stopped reviewer's approval with its recorded session ends the round; another session's footer ends nothing" || return 0
  local issue branch pr number head token session seq control control_after sent event reply after review ok_control=0 ok_reviewer=0
  issue=$(new_rig_issue "Answer loop proof: the review round ($store)")
  branch=legion-e2eal/${issue,,}
  if ! pr=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" open "$work/github-apps.yaml" "$repo" "$branch" "Answer loop proof $issue (closed by the run)"); then
    record_result A19 FAIL "no pull request could be opened on $repo"
    return 0
  fi
  number=$(jq -r .number <<<"$pr")
  head=$(jq -r .head <<<"$pr")
  github_writes+=("pr:$number:$branch")
  note "opened $repo pull request #$number from $branch at $head as the implement App"
  token=$(claim_token_of "$issue" reviewer)
  write_claude_route "$token" "claude-$store"
  provision_workspace "$issue"
  claims_cmd spawn --json --tree "$issue" --issue "$issue" --role architect --prompt-file "$(write_prompt architect "$issue")" >/dev/null
  agent[$store:a19_architect.token]=$(claim_token_of "$issue" architect)
  spawn_agent reviewer reviewer "$issue" "$issue" \
    "Record this and keep it: you are the reviewer of pull request #$number on $repo, head $head. You reviewed it, found nothing to change, and completed your round. Reply with one sentence restating it. Run no tool."
  await_turn reviewer
  session=${agent[$store:reviewer.session]}
  suspend_agent reviewer || true
  claims_cmd suspend --claim "${agent[$store:a19_architect.token]}" >/dev/null || true
  stop_daemon
  seq=$(dispatch_events "$issue" | jq -s '[.[].seq] | max // 0')
  pg_sql "$daemon_db" "insert into issues (key, tree, project, title, parent, phase, generation, status, rank, handed_over, last_dispatch_seq, dispatch_status, repo, default_branch, github_repo)
    values ('$issue', '$issue', '$project', 'Answer loop proof: the review round ($store)', null, 'reviewing', 1, 'in_progress', 'a', true, $seq, 'in_progress', '$repo', 'main', '$repo')" >/dev/null
  pg_sql "$daemon_db" "insert into phases (issue, role, claim, handoff_commit, rounds, verdict, completed_at) values ('$issue', 'reviewer', '$token', '$head', 1, '', now() - interval '2 minutes')" >/dev/null
  pg_sql "$daemon_db" "insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, pushes, head_counted, state)
    values ('$issue', '$repo', $number, '$branch', '$head', now(), '[]', '[]', 0, 0, '[]', 0, '', '[]', '', 'open')" >/dev/null
  note "recorded $issue's review round open on #$number at $head, the reviewer $token (session $session) completed undecided: $(review_record "$issue")"
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$daemon_log" "$branch_home" "$branch_profile" "$root/packages/claude-envoy"
  control=$(timeout 120 bun "$root/scripts/e2e/lib/smoke-pull-request.ts" approve "$work/github-apps.yaml" "$repo" "$number" "$head" \
    "Approved (control: another session's footer). <!-- legion: {\"session\":\"legion-e2eal-another-session-$run_tag\",\"phase\":\"reviewing\"} -->") || control=
  note "control: the review App's approval with another session's footer: $control"
  sleep 270
  control_after=$(review_record "$issue")
  note "after two of the daemon's review reads: $control_after; the reviewer's claim: $(claim_json "$token" | jq -c '{state, session}')"
  [ -n "$control" ] && jq -e '.phase == "reviewing" and .decision == null and .review_seen == 0' <<<"$control_after" >/dev/null && ok_control=1
  sent=$(envoy_ask_text session "$session" "Please approve your pull request #$number on $repo at its head $head now, with your gh tool: gh pr review $number -R $repo --approve --body 'Approved: nothing to change.' Then tell me what you did.")
  event=${sent#* }
  note "to the stopped reviewer: listener $sent"
  if soft_wait 300 "the reviewer's reply" inbox_reply "$event"; then
    reply=$(inbox_reply "$event")
    note "the reviewer's reply: $(head -c 300 <<<"$reply" | tr '\n' ' ')"
    note "its gh line: $(boot_log | jq -c --arg i "$issue" 'select(.msg == "answers: gh" and .issue == $i)' | tail -1)"
  fi
  ended() { review_record "$issue" | jq -e '.phase != "reviewing"' >/dev/null; }
  soft_wait 300 "the daemon to end the round" ended || true
  after=$(review_record "$issue")
  review=$(timeout 60 gh api "repos/$repo/pulls/$number/reviews" | jq -c --arg s "$session" '[.[] | select(.state == "APPROVED" and (.body | contains("\"session\":\"" + $s + "\""))) | {id, user: .user.login, commit_id, footer: (.body | capture("legion: (?<f>\\{[^}]*\\})").f // null)}]')
  note "the reviewer's approvals on GitHub: $review"
  note "the round after its approval: $after; the daemon's lines for $issue: $(boot_log | jq -s -c --arg i "$issue" '[.[] | select((.issue // "") == $i and ((.msg | startswith("answers:")) | not)) | .msg] | .[-6:]')"
  jq -e --arg login "$review_app_login" --arg h "$head" 'length == 1 and .[0].user == $login and .[0].commit_id == $h' <<<"$review" >/dev/null &&
    jq -e --argjson id "$(jq '.[0].id // 0' <<<"$review")" '.phase != "reviewing" and .review_seen == $id' <<<"$after" >/dev/null && ok_reviewer=1
  if [ "$ok_control$ok_reviewer" = 11 ]; then
    record_result A19 PASS "the review App's approval with another session's footer decided nothing; the stopped reviewer's approval, footed with its recorded session $session, ended the round ($(jq -r .phase <<<"$after"))"
  else
    record_result A19 FAIL "control decided nothing $ok_control, the reviewer's approval ended the round $ok_reviewer (1 = shown)"
  fi
}

# ---- A13: refused exactly as today without answers --------------------------------------------------
# a13_probe LABEL STORE: against whatever listener, Dispatch and daemon run now, the four answers a
# stopped agent's message gets today, saved byte for byte under $evidence/a13/<LABEL>/<STORE>/: an
# envoy_send to its session, a Dispatch message to its session and to its role (the delivery records,
# their ids and times left out), and a publish to its role topic (last, since a lapsed holder's
# first publish releases the claim; both stacks see the same snapshot of the bus).
a13_probe() {
  local label=$1 probe_store=$2 dir s r issue
  dir=$evidence/a13/$label/$probe_store
  mkdir -p "$dir"
  s=${agent[$probe_store:architect.session]}
  r=${agent[$probe_store:architect.token]}
  envoy -o "$dir/send.body" -w '%{http_code}\n' --data "$(jq -cn --arg s "$s" '{target_session: $s, source: "agent", source_session: "legion-e2eal-a13", message: "A13 probe: what did you decide?"}')" \
    "$(listener_url /v1/messages/send)" >"$dir/send.status"
  issue=$(dispatch_human POST issues "$(jq -cn --arg p "$a13_project" '{project: $p, title: "A13 probe", force: true}')" | jq -er .key)
  dispatch_human POST "issues/$issue/messages" "$(jq -cn --arg t "session:$s" '{body: "A13 probe: what did you decide?", target: $t, delivery: "steer"}')" |
    jq -S '.deliveries | map(del(.message_id, .created_at, .requested_by.id, .requested_by.name, .requested_by.login))' >"$dir/dispatch-session.json"
  dispatch_human POST "issues/$issue/messages" "$(jq -cn --arg t "role:$r" '{body: "A13 probe: what did you decide?", target: $t, delivery: "steer"}')" |
    jq -S '.deliveries | map(del(.message_id, .created_at, .requested_by.id, .requested_by.name, .requested_by.login))' >"$dir/dispatch-role.json"
  envoy -o "$dir/publish.body" -w '%{http_code}\n' --data "$(jq -cn --arg t "notifications.role.$r" '{topic: $t, source: "agent", source_session: "legion-e2eal-a13", message: "A13 probe: what did you decide?"}')" \
    "$(listener_url /v1/messages/publish)" >"$dir/publish.status"
  note "$label $probe_store: send $(cat "$dir/send.status") $(cat "$dir/send.body"); publish $(cat "$dir/publish.status") $(cat "$dir/publish.body")"
  note "$label $probe_store: Dispatch to session $(jq -c 'map({state, error, session_id})' "$dir/dispatch-session.json"); to role $(jq -c 'map({state, error, session_id})' "$dir/dispatch-role.json")"
}

restart_nats_from() {
  stop_service nats
  rm -rf "$work/nats-store"
  cp -a "$1" "$work/nats-store"
  start_nats
}

# ---- the matrix ------------------------------------------------------------------------------------
wanted A16 && check_a16

for store in $stores; do
  begin "store-$store"
  project="AL${store:0:1}$run_tag"
  project=${project^^}
  daemon_db=legion_${store}_$run_tag
  state=$work/state-$store
  mkdir -p "$state"
  dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Answer loop proof $project" '{key: $key, name: $name}')" >/dev/null
  dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project: $project, default: true}')" >/dev/null
  # The second owner's repository joins the project's mapping, never as its default: every rig issue
  # works the smoke repository, and A17 names the second one in its question.
  [ -z "$second_repo" ] || dispatch_human PUT "settings/repo-projects/$second_repo" "$(jq -cn --arg project "$project" '{project: $project, default: false}')" >/dev/null
  agent[$store:root]=$(new_rig_issue "Answer loop proof tree ($store)")
  agent[$store:planner.issue]=$(new_rig_issue "Answer loop proof child: the plan" "${agent[$store:root]}")
  agent[$store:tester.issue]=$(new_rig_issue "Answer loop proof child: the tests" "${agent[$store:root]}")
  agent[$store:pre_change.tree]=$(new_rig_issue "Answer loop proof tree before the change ($store)")
  agent[$store:pre_change.issue]=$(new_rig_issue "Answer loop proof child: before the change" "${agent[$store:pre_change.tree]}")
  note "project $project, issues: root ${agent[$store:root]}, planner ${agent[$store:planner.issue]}, tester ${agent[$store:tester.issue]}, pre-change ${agent[$store:pre_change.issue]} (tree ${agent[$store:pre_change.tree]})"
  pick_port port_daemon
  produce_pre_change
  write_legion_config "$work/answers-$store.yaml" yes
  write_legion_config "$work/plain-$store.yaml" no
  start_daemon "$work/branch/legion" "$work/answers-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
    "$root/packages/claude-envoy"
  produce_agents
  save_conversations
  # The registry's 404 is retried each minute; a boot that has run past a minute shows it logged once.
  sleep 65
  wanted boot && check_boot
  [ "$store" != postgres ] || ! wanted store || check_postgres_store
  wanted A1 && check_a1
  wanted A2 && check_a2
  wanted A3 && check_a3
  wanted A4 && check_passed_on unanswerable A4
  wanted A5 && check_passed_on working-copy A5
  wanted A6 && check_a6
  wanted A7 && check_a7
  wanted A8 && check_a8
  wanted A9 && check_a9
  wanted A10 && check_a10
  wanted A12 && check_a12
  wanted A14 && check_a14
  wanted A15 && check_a15
  wanted A17 && check_a17
  wanted A18 && check_a18
  wanted A19 && check_a19
  stop_daemon
  pass
done

# A13: the same stopped agents, the same bus state, the branch with `answers` removed against the
# base commit's binaries.
run_a13() {
  local differs probe_store
  begin a13
  store=-
  a13_project=A13$run_tag
  stop_service dispatch
  stop_service listener
  stop_service nats
  cp -a "$work/nats-store" "$work/nats-snapshot"
  start_nats
  start_listener "$work/branch"
  start_dispatch "$work/branch" dispatch
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  declare -gA a13_quiet=()
  for store in $stores; do
    project="AL${store:0:1}$run_tag"
    project=${project^^}
    daemon_db=legion_${store}_$run_tag
    state=$work/state-$store
    write_legion_config "$work/plain-$store.yaml" no
    start_daemon "$work/branch/legion" "$work/plain-$store.yaml" "$evidence/logs/daemon-$store.log" "$branch_home" "$branch_profile" \
      "$root/packages/claude-envoy"
    sleep 65
    a13_quiet[$store]=0
    check_boot_without_answers || a13_quiet[$store]=1
    a13_probe branch "$store"
    stop_daemon
  done
  stop_service dispatch
  stop_service listener
  restart_nats_from "$work/nats-snapshot"
  start_listener "$work/base"
  start_dispatch "$work/base" dispatch_base
  dispatch_human POST projects "$(jq -cn --arg key "$a13_project" '{key: $key, name: "A13 probes"}')" >/dev/null
  store=files
  project=A13B$run_tag
  daemon_db=legion_base_$run_tag
  state=$work/state-base
  mkdir -p "$state"
  write_legion_config "$work/base-a13.yaml" base
  start_daemon "$work/base/legion" "$work/base-a13.yaml" "$evidence/logs/base-daemon-a13.log" "$base_home" "$base_profile" \
    "$base_src/packages/claude-envoy"
  for probe_store in $stores; do a13_probe base "$probe_store"; done
  stop_daemon
  for store in $stores; do
    differs=$(cd "$evidence/a13" && for f in send.status send.body publish.status publish.body dispatch-session.json dispatch-role.json; do
      cmp -s "branch/$store/$f" "base/$store/$f" || printf '%s ' "$f"
    done)
    printf '== A13 (%s): with answers removed, a stopped agent is refused exactly as the base commit refuses it\n' "$store"
    note "branch vs base ($base_commit): $(cd "$evidence/a13" && for f in send.body publish.body; do printf '%s %s ' "$f" "$(sha256sum "branch/$store/$f" | cut -c1-12)/$(sha256sum "base/$store/$f" | cut -c1-12)"; done)"
    if [ -z "$differs" ] && [ "${a13_quiet[$store]}" = 0 ] && grep -q '^404$' "$evidence/a13/branch/$store/send.status" &&
      grep -q '^404$' "$evidence/a13/branch/$store/publish.status" &&
      jq -e 'length == 1 and .[0].state == "failed"' "$evidence/a13/branch/$store/dispatch-session.json" >/dev/null; then
      record_result A13 PASS "envoy_send 404, role publish 404 and Dispatch's failed deliveries are byte-identical to the base commit's; the daemon logged no registry or answers line"
    else
      record_result A13 FAIL "differs: ${differs:-nothing}; registry or answers lines with answers removed: ${a13_quiet[$store]}"
    fi
  done
  pass
}
wanted A13 && run_a13

# ---- summary ---------------------------------------------------------------------------------------
begin summary
printf '%-10s %-8s %-8s %s\n' STORE CRITERION VERDICT DETAIL
for row in "${results[@]}"; do
  IFS='|' read -r r_store r_criterion r_verdict r_detail <<<"$row"
  printf '%-10s %-8s %-8s %s\n' "$r_store" "$r_criterion" "$r_verdict" "$r_detail"
done
printf '%s\n' "${results[@]}" >"$evidence/summary.txt"
failed=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "FAIL"' | wc -l)
waiting=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "WAITING" { print $2 }' | sort -u | tr '\n' ' ')
passed=$(printf '%s\n' "${results[@]}" | awk -F'|' '$3 == "PASS" { print $1 ":" $2 }' | tr '\n' ' ')
note "PASS: ${passed:-none}"
note "WAITING: ${waiting:-none}"
note "FAIL: $failed"
[ "$failed" = 0 ] || fail "$failed criterion checks failed"
pass
ok=1
echo "answer loop e2e: every runnable check passed; the rest are WAITING as listed"
