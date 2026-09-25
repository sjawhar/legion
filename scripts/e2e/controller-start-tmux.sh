#!/usr/bin/env bash
# The operator-launched controller's live proof on tmux (LEGION-208 Stage 4b, task 4b.5): the Go
# daemon on a real Postgres with this checkout's plugin in an isolated OMP profile, a real Envoy
# listener and NATS, and `legion controller start` run in real tmux panes as an operator would. It
# checks `legion start --check-config`, the boot gate's contract refusal, `legion state --config`
# running no key command, the operator token file's mode, the controller's registration, role and
# environment, Ctrl-C reaching Oh My Pi, `legion status` from an operator shell, a second start
# revoking the first, the controller liveness probe against the listener, and the command's exit
# code. Each check prints `== <name>`, what it observed, and `ok <name>`; the first that fails ends
# the run non-zero, naming it.
#
# Run it as `bash scripts/e2e/controller-start-tmux.sh` from a checkout. Everything the run
# creates is its own and goes on any exit: its scratch directory, the isolated OMP profile, the
# tmux servers, and its Postgres and NATS containers. CONTROLLER_START_EVIDENCE_DIR (default a fresh
# /tmp directory, kept and printed) keeps the daemon and listener logs.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d /tmp/legion-e2e-controller.XXXXXXXX)
evidence=${CONTROLLER_START_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e-controller-evidence.XXXXXXXX)}
mkdir -p "$evidence/logs" "$evidence/checks"
ok=
daemon_pid=
listener_pid=
daemon_port=
envoy_port=
profile=legion-e2e-controller-$$-$(date +%s)
project="CS$$$(date +%s)"
project=${project:0:10}
ptoken=${project,,}
nats_container=legion-e2e-controller-nats-$$
pg_container=legion-e2e-controller-pg-$$
state=$work/state
ctl_state=$work/controller-state
daemon_log=$evidence/logs/daemon.log
check=setup
timeout_hook=

begin() { check=$1; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# Unconditional: every run removes what it made, whatever it ended on.
cleanup() {
  local p
  set +e
  for p in ctl1 ctl2; do TMUX_TMPDIR=$work/tmux tmux -L accept capture-pane -p -t "$p" >"$evidence/checks/$p.pane" 2>/dev/null; done
  TMUX_TMPDIR=$work/tmux tmux -L accept kill-server >/dev/null 2>&1
  stop_pid "$daemon_pid"
  TMUX_TMPDIR=$work/tmux tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1
  stop_pid "$listener_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null; done
  docker rm -f "$nats_container" "$pg_container" >/dev/null 2>&1
  rm -rf "$HOME/.omp/profiles/$profile" "$work"
  [ -n "$ok" ] || echo "controller start e2e: FAIL (check $check)"
  echo "evidence: $evidence (logs/daemon.log, logs/listener.log, and checks/: each check's own output and the controller panes)"
  return 0
}
trap cleanup EXIT
trap 'echo "FAIL $check: line $LINENO exited $?: $BASH_COMMAND" >&2' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

legion() { "$work/legion" "$@"; }
tm() { tmux -L accept "$@"; }
state_json() { legion state --json --port "$daemon_port"; }
envoy_role() { curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/roles/$1"; }
log_count() { jq -R --arg m "$1" 'fromjson? | select(.msg == $m)' "$daemon_log" | jq -s length; }
# The operator's shell, less the running session's own OMP and Envoy variables.
operator_env() { env -u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH OMP_PROFILE="$profile" "$@"; }
# controller_omp STATE_DIR: the pid of the Oh My Pi `legion controller start` launched for STATE_DIR.
controller_omp() {
  local p
  for p in /proc/[0-9]*; do
    [ "$(basename -- "$( (tr '\0' '\n' <"$p/cmdline") 2>/dev/null | head -1)" 2>/dev/null)" = omp ] || continue
    (tr '\0' '\n' <"$p/environ") 2>/dev/null | grep -qxF "LEGION_STATE_DIR=$1" || continue
    echo "${p#/proc/}"
    return 0
  done
  return 1
}
env_of() { tr '\0' '\n' <"/proc/$1/environ" | sed -n "s/^$2=//p"; }

# ---- setup -----------------------------------------------------------------------------------------
for tool in go docker jq curl tmux bun mise; do command -v "$tool" >/dev/null || fail "$tool is required"; done
mkdir -p "$state" "$work/xdg" "$work/tmux"
export XDG_STATE_HOME=$work/xdg TMUX_TMPDIR=$work/tmux
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
pick_port daemon_port
pick_port envoy_port
(cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener)
note "legion $("$work/legion" version); OMP pin $pin; daemon port $daemon_port; listener port $envoy_port"

(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
# Docker assigns the containers' host ports when it binds them, so neither can lose a race.
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=legion \
  -p "127.0.0.1::5432" postgres:16 >/dev/null
pg_port=$(docker port "$pg_container" 5432/tcp | head -1 | sed 's/.*://')
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion
dsn="postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:${pg_port}/legion?sslmode=disable"

docker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null
until_true 90 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
nats_url="nats://127.0.0.1:$(docker port "$nats_container" 4222/tcp | head -1 | sed 's/.*://')"
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
ENVOY_API_TOKEN="$(cat "$work/envoy-token")" PORT=$envoy_port ENVOY_LISTEN_HOST=127.0.0.1 \
  ENVOY_MACHINE_ID="legion-e2e-controller-$$" NATS_URLS=$nats_url "$work/envoy-listener" >"$evidence/logs/listener.log" 2>&1 &
listener_pid=$!
until_true 60 "the Envoy listener" curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/sessions"

(cd "$root" && bun install --frozen-lockfile >/dev/null)
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --dest "$work/plugin")
want_contract=$(jq -r .legion.goDaemonApiVersion "$root/packages/pi-envoy/package.json")
note "plugin $(jq -r '.name + "@" + .version' "$manifest") in OMP profile $profile, goDaemonApiVersion $want_contract"
# The boot gate resolves the model of every task agent the prompts dispatch, so the profile names
# their roles (@review, @oracle) and the default one model, served by a static-key provider that
# listens nowhere: this proof takes no model turn, so no model is called and no credential the
# machine carries decides the gate.
mkdir -p "$HOME/.omp/profiles/$profile/agent"
cat >"$HOME/.omp/profiles/$profile/agent/models.yml" <<'EOF'
providers:
  offline:
    baseUrl: http://127.0.0.1:9
    auth: apiKey
    api: anthropic-messages
    apiKey: static-key
    models:
      - id: m1
        name: M1
EOF
printf 'modelRoles:\n  default: offline/m1\n  review: offline/m1\n  oracle: offline/m1\n' >"$HOME/.omp/profiles/$profile/agent/config.yml"

(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token")
cat >"$work/legion.yaml" <<EOF
project: $project
port: $daemon_port
postgres_dsn: $dsn
state_dir: $state
operator_token_file: $work/operator-token
omp_invocation: mise x $pin -- omp
envoy_url: http://127.0.0.1:$envoy_port
nats_urls:
  - $nats_url
envoy_token_file: $work/envoy-token
probe_interval_seconds: 5
EOF
printf 'Always answer in one sentence.\n' >"$work/instructions.md"
cat >"$work/controller.yaml" <<EOF
project: $project
daemon_url: http://127.0.0.1:$daemon_port
operator_token_file: ./operator-token
envoy_url: http://127.0.0.1:$envoy_port
envoy_token_file: ./envoy-token
nats_urls: [$nats_url]
instructions: ./instructions.md
omp_invocation: mise x $pin -- omp
state_dir: ./controller-state
EOF

# ---- --check-config --------------------------------------------------------------------------------
begin check-config-passes
out=$(legion start --check-config --config "$work/legion.yaml")
[ "$out" = "Config OK: project=$project" ] || fail "got $out"
note "$out"
pass

begin check-config-names-each-broken-key
marker=$work/private-key-command-ran
workflow_tail="dispatch_url: https://dispatch.test
projects:
  $project: { repo: acme/widgets }
github_apps:
  implement: { app_id: \"1\", private_key_command: \"touch $marker; exit 1\" }
  review: { app_id: \"2\", private_key_command: \"touch $marker; exit 1\" }"
variant() { # variant NAME SAYS: the file is $work/variant-NAME.yaml
  local st=0 out
  out=$(legion start --check-config --config "$work/variant-$1.yaml" 2>&1) || st=$?
  [ "$st" = 1 ] || fail "variant $1 exited $st: $out"
  grep -qF "$2" <<<"$out" || fail "variant $1 does not say \"$2\": $out"
  note "variant $1 → $out"
}
{ cat "$work/legion.yaml"; echo "worker_cap: 3"; } >"$work/variant-tossed.yaml"
variant tossed "unknown key worker_cap"
{ cat "$work/legion.yaml"; echo "admission_cap: 0"; } >"$work/variant-cap.yaml"
variant cap "admission_cap must be a positive integer"
sed "s|^envoy_url: .*|envoy_url: not a url|" "$work/legion.yaml" >"$work/variant-envoy.yaml"
variant envoy "envoy_url must be a valid URL"
{ cat "$work/legion.yaml"; echo "$workflow_tail"; } >"$work/variant-dispatch.yaml"
variant dispatch "dispatch_token_file is required when dispatch_url is configured"
[ ! -e "$marker" ] || fail "a private_key_command ran"
note "no private_key_command ran ($marker absent)"
pass

# ---- the boot gate on the plugin's contract --------------------------------------------------------
begin gate-refuses-the-previous-contract
cp -p "$work/plugin/package.json" "$work/manifest.orig"
previous_contract=$((want_contract - 1))
jq --argjson c "$previous_contract" '.legion.goDaemonApiVersion = $c' "$work/manifest.orig" >"$work/plugin/package.json"
st=0
operator_env timeout 300 "$work/legion" start --config "$work/legion.yaml" >"$evidence/checks/refusal-contract.log" 2>&1 || st=$?
cp -p "$work/manifest.orig" "$work/plugin/package.json"
[ "$st" != 0 ] && [ "$st" != 124 ] || fail "legion start exited $st"
grep -qF "speaks Go daemon API contract $previous_contract; this daemon requires $want_contract" "$evidence/checks/refusal-contract.log" ||
  fail "the refusal does not name both contracts: $(head -3 "$evidence/checks/refusal-contract.log")"
note "legion start exit $st: $(grep -oF "speaks Go daemon API contract $previous_contract; this daemon requires $want_contract" "$evidence/checks/refusal-contract.log" | head -1)"
pass

# ---- the daemon ------------------------------------------------------------------------------------
begin daemon-serves
operator_env "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 &
daemon_pid=$!
until_true 180 "the daemon to answer /healthz" curl -fs "http://127.0.0.1:$daemon_port/healthz"
state_json | jq -e '.controllerLocator == null' >/dev/null || fail "a controller is recorded before any start"
note "healthy; no controllerLocator yet"
pass

begin state-config-runs-no-key-command
{ cat "$work/legion.yaml"; echo "dispatch_token_file: $work/absent-dispatch-token"; echo "$workflow_tail"; } >"$work/key-command.yaml"
got=$(legion state --json --config "$work/key-command.yaml" | jq -r .daemon.project)
[ "$got" = "$project" ] || fail "legion state --config read $got"
[ ! -e "$marker" ] || fail "legion state --config ran a private_key_command"
note "legion state --config read project $got; $marker absent"
pass

# ---- legion controller start ----------------------------------------------------------------------
begin operator-token-file-others-can-read-is-refused
cp "$work/operator-token" "$work/operator-token-0640" && chmod 0640 "$work/operator-token-0640"
sed 's|^operator_token_file: .*|operator_token_file: ./operator-token-0640|' "$work/controller.yaml" >"$work/controller-0640.yaml"
st=0
out=$(operator_env "$work/legion" controller start --config "$work/controller-0640.yaml" 2>&1 </dev/null) || st=$?
[ "$st" = 1 ] || fail "exit $st: $out"
grep -qF "operator_token_file $work/operator-token-0640 is readable by its group or others (mode 0640); chmod 0600 it" <<<"$out" || fail "$out"
[ "$(log_count "api: minted a controller capability; the previous controller's registration and grants are revoked")" = 0 ] ||
  fail "the daemon minted a capability for a refused start"
[ ! -e "$ctl_state" ] || fail "the refused start wrote $ctl_state"
note "exit 1: $out"
note "the daemon minted nothing; $ctl_state not created"
pass

begin controller-claims-the-role
tm new-session -d -s ctl1 -x 200 -y 50 "cd '$work' && env -u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH OMP_PROFILE='$profile' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/checks/ctl1.stderr'; echo \$? >'$evidence/checks/ctl1.exit'; sleep 600"
until_true 180 "controllerLocator in the state" sh -c "'$work/legion' state --json --port $daemon_port | jq -e '.controllerLocator.sessionId != null'"
locator1=$(state_json | jq -c .controllerLocator)
session1=$(jq -r .sessionId <<<"$locator1")
jq -e --arg s "$session1" '.runtime == "tmux" and .external == true and .sessionId == $s and (.registeredAt | length > 0)' <<<"$locator1" >/dev/null ||
  fail "controllerLocator = $locator1"
note "GET /legion/v1/state .controllerLocator = $locator1"
role=legion-$ptoken-controller
until_true 60 "the Envoy role $role to name $session1" sh -c "curl -fsS -H '@$work/envoy-auth-header' 'http://127.0.0.1:$envoy_port/v1/roles/$role' | jq -e --arg s '$session1' '.holder == \$s'"
note "GET /v1/roles/$role → holder $(envoy_role "$role" | jq -r .holder)"
registered=$(jq -R -c --arg s "$session1" 'fromjson? | select(.msg == "api: controller registered" and .session == $s) | {claim, generation, session, pluginContract}' "$daemon_log" | head -1)
[ -n "$registered" ] || fail "the daemon logged no controller registration for $session1"
note "daemon: api: controller registered $registered"
omp1=$(controller_omp "$ctl_state") || fail "no omp process carries LEGION_STATE_DIR=$ctl_state"
for pair in LEGION_CONTROLLER=1 LEGION_ROLE=controller LEGION_DAEMON_API=go "LEGION_PROJECT=$ptoken" \
  "LEGION_CONTROLLER_SECRET_FILE=$ctl_state/secrets/$role" "LEGION_GRANT_FILE=$ctl_state/secrets/$role-grant" \
  "ENVOY_TOKEN_FILE=$work/envoy-token" "ENVOY_NATS_URL=$nats_url"; do
  [ "$(env_of "$omp1" "${pair%%=*}")" = "${pair#*=}" ] || fail "omp $omp1 has ${pair%%=*}=$(env_of "$omp1" "${pair%%=*}"), want ${pair#*=}"
done
env_of "$omp1" PI_SHELL_PREFIX | grep -qF "'$ctl_state/worker-bin:$ctl_state/bin:'" || fail "PI_SHELL_PREFIX = $(env_of "$omp1" PI_SHELL_PREFIX)"
[ -z "$(env_of "$omp1" LEGION_CONTROLLER_SECRET)" ] || fail "the controller secret's value is in omp's environment"
tr '\0' '\n' <"/proc/$omp1/cmdline" | grep -qxF -- "--append-system-prompt" || fail "omp has no --append-system-prompt"
! tr '\0' '\n' <"/proc/$omp1/cmdline" | grep -qx -- "--mode\|rpc\|--resume.*" || fail "omp runs --mode rpc or --resume"
[ "$(stat -c %a "$ctl_state/secrets/$role")" = 600 ] || fail "the secret file is not 0600"
note "omp $omp1: LEGION_CONTROLLER=1 LEGION_DAEMON_API=go PI_SHELL_PREFIX over $ctl_state; secret only as a 0600 file; interactive (no --mode rpc)"
pass

begin ctrl-c-reaches-omp-not-the-cli
cli1=$(pgrep -f "^$work/legion controller start --config $work/controller.yaml" | head -1)
[ -n "$cli1" ] || fail "no legion controller start process"
tm send-keys -t ctl1 C-c
sleep 2
kill -0 "$cli1" 2>/dev/null || fail "one Ctrl-C ended legion controller start"
kill -0 "$omp1" 2>/dev/null || fail "one Ctrl-C ended Oh My Pi"
[ ! -e "$evidence/checks/ctl1.exit" ] || fail "the command exited on one Ctrl-C"
note "after one Ctrl-C both legion ($cli1) and omp ($omp1) run; Oh My Pi owns the terminal"
pass

begin status-from-an-operator-shell
st=0
out=$(legion status "$project-1" backlog --operator-token-file "$work/operator-token" --port "$daemon_port" 2>&1) || st=$?
# This rig has no Dispatch, so the route that took the minted grant answers 500 naming it: the
# bearer minted a grant and the status request reached the Dispatch client. The status write
# itself is the full-tree proof's (stage4b-sandbox-tree.sh, controller), against a real Dispatch.
grep -qF 'the daemon answered 500 Internal Server Error: Dispatch is unavailable' <<<"$out" || fail "exit $st: $out"
note "operator bearer → controller grant → status route reached Dispatch (this rig has none): exit $st: $out"
printf 'not-the-operator-token\n' >"$work/wrong-token" && chmod 600 "$work/wrong-token"
st=0
out=$(legion status "$project-1" backlog --operator-token-file "$work/wrong-token" --port "$daemon_port" 2>&1) || st=$?
if [ "$st" != 1 ] || ! grep -qF 'the daemon answered 403 Forbidden: Invalid operator token' <<<"$out"; then fail "wrong token: exit $st: $out"; fi
note "wrong bearer: exit $st: $out"
pass

begin second-start-revokes-the-first
cap1=$(cat "$ctl_state/secrets/$role")
grant_before=$(curl -fsS -X POST -H "Authorization: Bearer $(cat "$work/operator-token")" -H 'Content-Type: application/json' \
  --data '{}' "http://127.0.0.1:$daemon_port/legion/v1/grants" | jq -r .grantId)
# The grant's positive control: before the second start it redeems, reaching the status route's
# missing Dispatch, so the route takes issue X-1 past the grant check.
redeem_status() {
  curl -sS -o "$evidence/checks/$1" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg g "$grant_before" '{grantId: $g, issue: "X-1", status: "backlog"}')" "http://127.0.0.1:$daemon_port/legion/v1/issues/status"
}
code=$(redeem_status grant-before-second-start.json) || fail "the status route could not be reached: $code"
if [ "$code" != 500 ] || ! grep -qF DISPATCH_UNAVAILABLE "$evidence/checks/grant-before-second-start.json"; then
  fail "the grant answered $code $(cat "$evidence/checks/grant-before-second-start.json") before the second start, want 500 DISPATCH_UNAVAILABLE"
fi
note "the grant, before the second start → $code $(cat "$evidence/checks/grant-before-second-start.json")"
tm new-session -d -s ctl2 -x 200 -y 50 "cd '$work' && env -u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH OMP_PROFILE='$profile' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/checks/ctl2.stderr'; echo \$? >'$evidence/checks/ctl2.exit'; sleep 600"
until_true 180 "controllerLocator to name a second session" sh -c "'$work/legion' state --json --port $daemon_port | jq -e --arg s '$session1' '.controllerLocator.sessionId != null and .controllerLocator.sessionId != \$s'"
locator2=$(state_json | jq -c .controllerLocator)
session2=$(jq -r .sessionId <<<"$locator2")
note "GET /legion/v1/state .controllerLocator = $locator2"
until_true 60 "the Envoy role to move to $session2" sh -c "curl -fsS -H '@$work/envoy-auth-header' 'http://127.0.0.1:$envoy_port/v1/roles/$role' | jq -e --arg s '$session2' '.holder == \$s'"
note "GET /v1/roles/$role → holder $session2"
mints=$(jq -R -c 'fromjson? | select(.msg | startswith("api: minted a controller capability")) | .generation' "$daemon_log" | tr '\n' ' ')
[ "$mints" = "1 2 " ] || fail "capability mints logged: $mints"
note "daemon minted generations: $mints"
body=$(jq -cn --arg t "$cap1" --arg s "$session1" --argjson c "$want_contract" '{bootToken: $t, sessionId: $s, ompSessionFile: "/x.jsonl", agentId: "a", pluginContract: $c}')
code=$(curl -s -o "$evidence/checks/reregister.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data "$body" "http://127.0.0.1:$daemon_port/legion/v1/claims/register")
[ "$code" = 403 ] || fail "registering with the first capability answered $code $(cat "$evidence/checks/reregister.json")"
note "POST /claims/register with the first capability → $code $(cat "$evidence/checks/reregister.json")"
code=$(redeem_status stale-grant.json) || fail "the status route could not be reached: $code"
if [ "$code" != 403 ] || ! grep -qF GRANT_UNAVAILABLE "$evidence/checks/stale-grant.json"; then
  fail "the same grant answered $code $(cat "$evidence/checks/stale-grant.json") after the second start, want 403 GRANT_UNAVAILABLE"
fi
note "the same grant, after the second start → $code $(cat "$evidence/checks/stale-grant.json")"
pass

begin liveness-probe-against-the-listener
cat >"$work/liveprobe.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/controller"
)

func main() {
	token, _ := os.ReadFile(os.Args[2])
	p := controller.NewProber(controller.ProberOptions{EnvoyURL: os.Args[1], EnvoyToken: strings.TrimSpace(string(token)), Project: os.Args[3], BootTimeout: 120 * time.Second})
	for _, session := range os.Args[4:] {
		fmt.Printf("%s=%s\n", session, p.Probe(context.Background(), session))
	}
}
GO
printf '{"Replace":{"%s":"%s"}}' "$root/packages/daemon-go/cmd/liveprobe-accept/main.go" "$work/liveprobe.go" >"$work/overlay.json"
verdicts=$(cd "$root/packages/daemon-go" && go run -overlay "$work/overlay.json" ./cmd/liveprobe-accept \
  "http://127.0.0.1:$envoy_port" "$work/envoy-token" "$ptoken" "$session2" "$session1" 2>"$evidence/checks/liveprobe.log" | tr '\n' ' ')
[ "$verdicts" = "$session2=alive $session1=gone " ] || fail "verdicts: $verdicts; log $(cat "$evidence/checks/liveprobe.log")"
note "controller.Prober on the live listener: $verdicts"
note "$(grep -o 'msg=.*' "$evidence/checks/liveprobe.log" | head -1)"
pass

begin exit-code-is-oh-my-pis
# Oh My Pi: "press Ctrl+C again to exit, or Ctrl+D" — Ctrl+D on an empty editor quits.
tm send-keys -t ctl2 C-d
until_true 30 "legion controller start to exit after Oh My Pi" test -s "$evidence/checks/ctl2.exit"
[ "$(cat "$evidence/checks/ctl2.exit")" = 0 ] ||
  fail "legion controller start exited $(cat "$evidence/checks/ctl2.exit") after Oh My Pi quit cleanly, want 0; stderr: $(cat "$evidence/checks/ctl2.stderr")"
note "the second controller's legion controller start exited 0 after Oh My Pi quit cleanly; stderr: $(cat "$evidence/checks/ctl2.stderr")"
pass

ok=1
echo "controller start e2e: PASS"
