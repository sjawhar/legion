#!/usr/bin/env bash
# INFRA-195's local-stack proof: Dispatch's live view shows an Oh My Pi session's current model and
# thinking level and switches them, on a real session. Oh My Pi runs in `--mode rpc`, as a Legion
# pane runs it, with this checkout's Envoy plugin in an isolated agent directory
# (PI_CODING_AGENT_DIR) under the run's own HOME; it registers with a real Envoy listener and a real
# nats-server, and a real Dispatch built from the checkout, with NATS on and its trusted identity
# header, serves the SPA this checkout builds. One held-open Chromium page, signed in by that header,
# reads and drives the header. Every value the page shows is compared with oracles that do not read
# the page: the session's own RPC answers (get_state, get_available_models, the command output of
# an `/effort` prompt), its session file, and Dispatch's stored messages.
#
# This script is setup and cleanup only. The checks are lib/dispatch-session-settings.ts's: each
# prints `== <name>`, what it observed and `ok <name>`, and the first that fails ends the run
# non-zero naming it. Run it from a checkout on a Linux host with go, bun, jq, curl and python3, no
# docker, tmux or root needed, as
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     ANTHROPIC_API_KEY=… OPENAI_API_KEY=… bash scripts/e2e/dispatch-session-settings.sh
#
# Inputs, each refused when missing, naming it:
#   LEGION_TEST_PG_DSN          a loopback PostgreSQL with pgvector, as merged-root-waits.sh takes it;
#                               the run creates a database of its own in it and drops it on any exit
#   LEGION_E2E_NATS_BIN         a nats-server; default the pinned v2.10.22, installed once with
#                               `go install` into /legion/scratch/bin on an issue pod's volume, else
#                               into the run's own directory
#   LEGION_TEST_OMP             the Oh My Pi binary; default the `omp` on PATH, else the worker
#                               image's /opt/omp/bin/omp
#   SESSION_SETTINGS_MODELS     two `provider/id` values the caller's keys serve (default
#                               "anthropic/claude-haiku-5-5 openai/gpt-5-nano"); the first is the
#                               profile's default model and the fallback target
#   SESSION_SETTINGS_CHROMIUM   the Chromium Playwright launches (default /usr/bin/chromium)
#   ANTHROPIC_API_KEY, OPENAI_API_KEY   the caller's own provider keys, required for a model of that
#                               provider; taken out of the environment before the run starts
#                               anything and given back to the driver alone, which passes them to
#                               Oh My Pi and to nothing else; never printed
#   SESSION_SETTINGS_EVIDENCE_DIR   default a fresh directory under TMPDIR; kept and printed: the
#                               transcript, every service's log, each Oh My Pi's stderr and log, the
#                               session files, and one screenshot per check
#
# Everything the run starts is its own and goes on any exit, by the exact pid it was started as: the
# nats-server, the listener, Dispatch, the process holding the broken provider's port closed, and
# every Oh My Pi the driver started (it records each pid in $work/omp.pids). Its database is dropped
# and its work directory, under TMPDIR, removed. No DISPATCH_*, ENVOY_*, NATS_*, OMP* or PI_*
# variable of the caller's shell reaches any process of the run: the rig talks only to its own
# loopback stack.
set -euo pipefail
for variable in $(compgen -e); do
  case $variable in DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_*) unset "$variable" ;; esac
done

# The provider keys leave the environment before anything starts, so no process of the run inherits
# them; the driver alone gets them back, and hands them to each Oh My Pi it starts and to nothing
# else.
anthropic_key=${ANTHROPIC_API_KEY:-}
openai_key=${OPENAI_API_KEY:-}
unset ANTHROPIC_API_KEY OPENAI_API_KEY

# The nats-server the run installs when the caller names none: the 2.10 line the daemon's own NATS
# tests pin (packages/daemon/internal/testnats/nats.go), as merged-root-waits.sh installs it.
nats_version=v2.10.22
# The pi-envoy the older-plugin session runs: the last release before the settings frame.
older_pi_envoy=10.24.0

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
evidence=${SESSION_SETTINGS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-settings-evidence.XXXXXXXX")}
mkdir -p "$evidence/logs" "$evidence/checks"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-settings.$$.XXXXXXXX")
driver=$root/scripts/e2e/lib/dispatch-session-settings.ts
check=setup
timeout_hook=
nats_pid=
listener_pid=
dispatch_pid=
holder_pid=
database=
ok=

note() { echo "   $*"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# stop_omp PID ends one Oh My Pi the driver started, and only while its command line still names
# this run's work directory, so a pid the kernel gave to another process since is never signalled.
# It is continued first: a check stops one with SIGSTOP, and a stopped process keeps a TERM pending.
stop_omp() {
  grep -qsF "$work" "/proc/$1/cmdline" || return 0
  signalable "$1" || return 0
  kill -CONT "$1" 2>/dev/null || true
  stop_pid "$1"
}

cleanup() {
  local status=$? pid
  set +e
  if [ -f "$work/omp.pids" ]; then
    while read -r pid; do stop_omp "$pid"; done <"$work/omp.pids"
  fi
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_pid "$holder_pid"
  stop_pid "$nats_pid"
  if [ -n "$database" ]; then
    bun "$driver" db-drop "$database" || echo "could not drop database $database; drop it by hand" >&2
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ] && [ -n "$ok" ]; then
    echo "dispatch session settings e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "dispatch session settings e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- inputs ------------------------------------------------------------------------------------------
echo "== setup"
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  fail "LEGION_TEST_PG_DSN is required: a loopback PostgreSQL with pgvector (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable) the run creates its own database in. This script starts no database; docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md starts one without root."
for tool in go bun jq curl tar python3; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done
omp=${LEGION_TEST_OMP:-$(command -v omp || true)}
if [ -z "$omp" ] && [ -x /opt/omp/bin/omp ]; then omp=/opt/omp/bin/omp; fi
[ -n "$omp" ] || fail "LEGION_TEST_OMP is required: no omp is on PATH and the worker image's /opt/omp/bin/omp is absent"
[ -x "$omp" ] || fail "LEGION_TEST_OMP names $omp, which is not an executable file"
read -r -a models <<<"${SESSION_SETTINGS_MODELS:-anthropic/claude-haiku-5-5 openai/gpt-5-nano}"
[ "${#models[@]}" -eq 2 ] && [ "${models[0]}" != "${models[1]}" ] ||
  fail "SESSION_SETTINGS_MODELS must name two different provider/id models, not '${SESSION_SETTINGS_MODELS:-}'"
for model in "${models[@]}"; do
  [[ $model =~ ^[A-Za-z0-9._-]+/[^[:space:]]+$ ]] || fail "SESSION_SETTINGS_MODELS holds '$model', which is not provider/id"
  case ${model%%/*} in
    anthropic) [ -n "$anthropic_key" ] || fail "ANTHROPIC_API_KEY is required for $model (SESSION_SETTINGS_MODELS)" ;;
    openai) [ -n "$openai_key" ] || fail "OPENAI_API_KEY is required for $model (SESSION_SETTINGS_MODELS)" ;;
  esac
done
chromium=${SESSION_SETTINGS_CHROMIUM:-/usr/bin/chromium}
[ -x "$chromium" ] || fail "SESSION_SETTINGS_CHROMIUM names $chromium, which is not an executable file; name the Chromium Playwright launches"
nats_bin=${LEGION_E2E_NATS_BIN:-}
if [ -z "$nats_bin" ]; then
  nats_gobin=$work/bin
  if [ -d /legion/scratch ]; then nats_gobin=/legion/scratch/bin; fi
  nats_bin=$nats_gobin/nats-server
  if ! { [ -x "$nats_bin" ] && "$nats_bin" --version | grep -qF "$nats_version"; }; then
    note "LEGION_E2E_NATS_BIN names none; installing nats-server $nats_version into $nats_gobin"
    GOBIN=$nats_gobin go install "github.com/nats-io/nats-server/v2@$nats_version" ||
      fail "could not install nats-server $nats_version; name a binary in LEGION_E2E_NATS_BIN instead"
  fi
fi
[ -x "$nats_bin" ] || fail "LEGION_E2E_NATS_BIN names $nats_bin, which is not an executable file"
note "Oh My Pi $omp ($("$omp" --version 2>/dev/null | head -1)); nats-server $("$nats_bin" --version); Chromium $chromium"
note "models ${models[0]} (the profile's default) and ${models[1]}; LEGION_TEST_PG_DSN set (not printed)"
note "keys: ANTHROPIC_API_KEY ${#anthropic_key} chars, OPENAI_API_KEY ${#openai_key} chars (passed to Oh My Pi only)"

# ---- builds ------------------------------------------------------------------------------------------
(cd "$root" && bun install --frozen-lockfile >"$evidence/logs/bun-install.log" 2>&1) || fail "bun install failed; see $evidence/logs/bun-install.log"
(cd "$root/packages/contracts" && bun run build >"$evidence/logs/build-contracts.log" 2>&1) || fail "@legion/contracts did not build; see $evidence/logs/build-contracts.log"
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/dispatch" ./cmd/dispatch) >"$evidence/logs/build-go.log" 2>&1 ||
  fail "the listener or Dispatch did not build; see $evidence/logs/build-go.log"
(cd "$root/packages/dispatch" && bun run build:web >"$evidence/logs/build-web.log" 2>&1) || fail "the SPA did not build; see $evidence/logs/build-web.log"
# This checkout's pi-envoy, packed as the release packs it (lib/pack-plugin.sh builds its dist) and
# unpacked as the worker image unpacks it, so `--extension` loads what a Legion pane loads.
tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$evidence/logs/pack-pi-envoy.log") ||
  fail "pi-envoy did not pack; see $evidence/logs/pack-pi-envoy.log"
mkdir -p "$work/pi-envoy"
tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
# A pi-envoy released before the settings frame, for the session that shows the older-plugin notice.
# Pinned, not the newest release: once this pi-envoy is released, the newest reports its settings.
mkdir -p "$work/released"
printf '{"name":"legion-e2e-session-settings-released","private":true}\n' >"$work/released/package.json"
(cd "$work/released" && bun add "@sjawhar/pi-envoy@$older_pi_envoy" >"$evidence/logs/bun-add-released.log" 2>&1) ||
  fail "bun add @sjawhar/pi-envoy@$older_pi_envoy failed; see $evidence/logs/bun-add-released.log"
note "pi-envoy $(jq -r .version "$work/pi-envoy/package.json") from this checkout; released pi-envoy $(jq -r .version "$work/released/node_modules/@sjawhar/pi-envoy/package.json")"

# ---- services ----------------------------------------------------------------------------------------
mkdir -p "$work/nats-store"
"$nats_bin" -js -a 127.0.0.1 -p -1 -sd "$work/nats-store" >"$evidence/logs/nats.log" 2>&1 &
nats_pid=$!
until_true 60 "the nats-server to be ready" grep -q 'Server is ready' "$evidence/logs/nats.log"
nats_url="nats://127.0.0.1:$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "$evidence/logs/nats.log")"

database=legion_e2e_session_settings_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
database_url=$(bun "$driver" db-create "$database") || fail "could not create database $database in LEGION_TEST_PG_DSN"

(umask 077 &&
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/envoy-token" &&
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
listener() { curl -fsS -H "@$work/envoy-auth-header" "$@"; }
envoy_port=$(bun "$driver" free-port)
dispatch_port=$(bun "$driver" free-port)
# Neither service inherits this shell (dispatch-user-turns.sh's reasoning): a HOME or XDG directory
# holding legion/envoy.json would hand Dispatch a production configuration.
env -i PATH="$PATH" HOME=/nonexistent ENVOY_API_TOKEN="$(cat "$work/envoy-token")" PORT="$envoy_port" \
  ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="legion-e2e-session-settings-$$" NATS_URLS="$nats_url" \
  "$work/envoy-listener" >"$evidence/logs/listener.log" 2>&1 &
listener_pid=$!
until_true 60 "the Envoy listener" listener "http://127.0.0.1:$envoy_port/v1/sessions"
env -i PATH="$PATH" HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent XDG_DATA_HOME=/nonexistent \
  DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$(cat "$work/dispatch-token")" \
  DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$dispatch_port" \
  DISPATCH_SERVER_URL="http://127.0.0.1:$dispatch_port" DISPATCH_SIGNING_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" \
  DISPATCH_WEB_DIST="$root/packages/dispatch/web/dist" ENVOY_URL="http://127.0.0.1:$envoy_port" \
  ENVOY_TOKEN="$(cat "$work/envoy-token")" NATS_URLS="$nats_url" \
  "$work/dispatch" >"$evidence/logs/dispatch.log" 2>&1 &
dispatch_pid=$!
until_true 120 "Dispatch to serve, with NATS" sh -c "curl -fsS http://127.0.0.1:$dispatch_port/healthz | jq -e '.ok and .nats'"

# The broken provider's port: bound and never listened on, by a process of the run's own, so every
# connection to it is refused for the whole run and nothing else can take it.
python3 -c 'import socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1], flush=True)
sys.stdout.close()
time.sleep(1e9)' >"$work/broken-port" &
holder_pid=$!
until_true 30 "the broken provider's port to be held" test -s "$work/broken-port"
broken_port=$(cat "$work/broken-port")

# ---- the Oh My Pi profile ----------------------------------------------------------------------------
# HOME is the run's own, with .omp/natives linked to the caller's native-module cache as
# lib/omp-home.sh's make_omp_home links it, so nothing is downloaded and nothing of the caller's
# ~/.omp is written. The agent directory holds the profile every session of the run shares.
natives=$HOME/.omp/natives
if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
mkdir -p "$natives" "$work/home/.omp" "$work/agent" "$work/session" "$work/tmp"
ln -sfT "$natives" "$work/home/.omp/natives"
# rig-broken answers nothing: its one model is the fallback check's, and retry.fallbackChains (the
# key the fork's session/retry-fallback-chains.ts reads) sends a failed request on it to the first
# model. Every role is the first model, so no side request (a title, a summary) reaches another.
cat >"$work/agent/models.yml" <<EOF
providers:
  rig-broken:
    baseUrl: http://127.0.0.1:$broken_port/v1
    api: openai-completions
    apiKey: rig-broken-literal-key
    models:
      - id: broken-model
        name: Rig broken model
EOF
{
  echo "defaultThinkingLevel: low"
  echo "modelRoles:"
  for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
    echo "  $role: ${models[0]}"
  done
  echo "retry:"
  echo "  fallbackChains:"
  echo "    rig-broken/broken-model:"
  echo "      - ${models[0]}"
} >"$work/agent/config.yml"
cp "$work/agent/models.yml" "$work/agent/config.yml" "$evidence/logs/"

jq -n --arg work "$work" --arg evidence "$evidence" --arg omp "$omp" --arg chromium "$chromium" \
  --arg dispatch "http://127.0.0.1:$dispatch_port" --arg envoy "http://127.0.0.1:$envoy_port" --arg nats "$nats_url" \
  --arg a "${models[0]}" --arg b "${models[1]}" --arg plugin "$work/pi-envoy" \
  --arg released "$work/released/node_modules/@sjawhar/pi-envoy" '{
    work: $work, evidence: $evidence, omp: $omp, chromium: $chromium, dispatchUrl: $dispatch,
    envoyUrl: $envoy, natsUrl: $nats, models: [$a, $b], plugin: $plugin, releasedPlugin: $released,
    home: ($work + "/home"), agentDir: ($work + "/agent"), sessionCwd: ($work + "/session"),
    tmp: ($work + "/tmp"), envoyTokenFile: ($work + "/envoy-token"),
    dispatchTokenFile: ($work + "/dispatch-token"), pidFile: ($work + "/omp.pids"),
    defaultLevel: "low", unkeyedModel: "google/gemini-2.5-flash"
  }' >"$work/rig.json"
note "NATS $nats_url, listener port $envoy_port, Dispatch port $dispatch_port, database $database, broken provider port $broken_port"
note "setup took $((SECONDS - started))s"

# ---- the checks --------------------------------------------------------------------------------------
check=checks
ANTHROPIC_API_KEY=$anthropic_key OPENAI_API_KEY=$openai_key bun "$driver" run "$work/rig.json" ||
  fail "the driver failed (the failing check is named above)"
ok=1
