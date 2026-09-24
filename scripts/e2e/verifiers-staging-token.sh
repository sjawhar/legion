#!/usr/bin/env bash
# LEGION-208's live proof of the OIDC verifiers: the branch's own `dispatch` and `listener`
# binaries authenticate a real projected service-account token minted by the staging EKS
# cluster, refuse a token minted for the other binary's audience, refuse a request with no
# bearer, and still accept their shared tokens. It also proves the two refusals that keep the
# feature honest: a half-configured OIDC pair and an issuer that does not answer both stop the
# boot, naming what is wrong.
#
# Every step is fatal and names itself — no `|| true` outside the cleanup a trap must never
# fail on, and every wait is bounded. The run leaves no container, no process, and nothing in
# the real Dispatch data dir (the binaries run under a scratch HOME).
#
# No raw token is ever printed. A token lives in a shell variable, reaches curl through a
# config document on stdin — never argv, whose /proc entry is world-readable, and never disk —
# and everything this script prints goes through `redact`, which replaces any JWT-shaped run
# with <redacted-jwt>. The two minted tokens are reported by their decoded iss/aud/sub/exp.
set -euo pipefail

pg_name=verifiers-e2e-pg
nats_name=verifiers-e2e-nats
work=/tmp/verifiers-e2e
dispatch_pid=
listener_pid=

# cleanup never returns non-zero: a trap that fails under set -e would mask the real status.
cleanup() {
  if [ -n "${dispatch_pid:-}" ]; then kill -TERM "$dispatch_pid" 2>/dev/null || true; fi
  if [ -n "${listener_pid:-}" ]; then kill -TERM "$listener_pid" 2>/dev/null || true; fi
  docker rm -f "$pg_name" "$nats_name" >/dev/null 2>&1 || true
  return 0
}

# redact is the structural half of "no raw token is printed": every byte this script emits
# that it did not compose itself passes through here first.
redact() { sed -E 's/eyJ[A-Za-z0-9_.-]+/<redacted-jwt>/g'; }

fail() {
  echo "verifiers e2e: FAIL: $1"
  if [ -n "${2:-}" ] && [ -s "$2" ]; then
    echo "--- $2"
    redact <"$2" | tail -20
  fi
  exit 1
}

# claims_json decodes a token's payload — base64url, unverified, for reporting only.
claims_json() {
  local payload
  payload=$(printf '%s' "$1" | cut -d. -f2)
  case $((${#payload} % 4)) in
    2) payload="$payload==" ;;
    3) payload="$payload=" ;;
  esac
  printf '%s' "$payload" | tr '_-' '/+' | base64 -d
}

# claims prints the four claims this proof is about, and nothing else from the token.
claims() {
  claims_json "$1" | jq -r '"iss=\(.iss) aud=\(.aud | join(",")) sub=\(.sub) exp=\(.exp | todate)"'
}

# request <url> [token] → the HTTP status on stdout, the body in $body. curl reads the
# Authorization header from a config document on stdin, so the token is never an argument.
request() {
  local url=$1 token=${2-}
  if [ -z "$token" ]; then
    curl -sS -o "$body" -w '%{http_code}' --max-time 15 "$url"
  else
    printf 'header = "Authorization: Bearer %s"\n' "$token" |
      curl -sS -o "$body" -w '%{http_code}' --max-time 15 -K - "$url"
  fi
}

# expect_status <what> <expected> <actual>
expect_status() {
  [ "$3" = "$2" ] || fail "$1: expected HTTP $2, got $3" "$body"
  echo "  $1 → $3"
}

# expect_body <what> <jq filter>
expect_body() {
  jq -e "$2" "$body" >/dev/null || fail "$1: body does not satisfy $2" "$body"
}

# expect_refusal <what> <log> <substring…>: the command already run must have exited non-zero,
# its log must name each substring, and it must never name a token.
expect_refusal() {
  local what=$1 log=$2
  shift 2
  local needle
  for needle in "$@"; do
    grep -qF -- "$needle" "$log" || fail "$what: the refusal does not name \"$needle\"" "$log"
  done
  # Written as `grep && fail`, never `! grep`: set -e ignores a negated pipeline
  # (shellcheck SC2251), so the negated form could never fail the run.
  grep -q 'eyJ' "$log" && fail "$what: the refusal printed a token"
  echo "  $what → refused, naming $*"
}

for tool in go docker kubectl jq curl ss base64; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done

root=$(cd "$(dirname "$0")/../.." && pwd)
rm -rf "$work"
mkdir -p "$work/home"
body="$work/body.json"

echo "== build the branch's binaries"
cd "$root/packages/envoy"
go build -o "$work/dispatch" ./cmd/dispatch
go build -o "$work/listener" ./cmd/listener

echo "== read the staging cluster's OIDC issuer"
issuer=$(kubectl --context "${VERIFIERS_E2E_KUBE_CONTEXT:-staging}" get --raw /.well-known/openid-configuration | jq -r .issuer)
case "$issuer" in
  https://*) ;;
  *) fail "the cluster's discovery document has no https issuer (got \"$issuer\")" ;;
esac
echo "  issuer=$issuer"
# An issuer of the same shape that answers 404: what a misconfigured deployment looks like.
absent_issuer="${issuer%/*}/00000000000000000000000000000000"

echo "== postgres:16 and nats:2.10 on ephemeral loopback ports"
docker rm -f "$pg_name" "$nats_name" >/dev/null 2>&1 || docker ps >/dev/null # a missing container is fine; a broken docker is not
trap cleanup EXIT
docker run -d --name "$pg_name" -e POSTGRES_USER=dispatch -e POSTGRES_PASSWORD=dispatch \
  -e POSTGRES_DB=dispatch -p 127.0.0.1::5432 postgres:16 >/dev/null
docker run -d --name "$nats_name" -p 127.0.0.1::4222 -p 127.0.0.1::8222 nats:2.10 -js -m 8222 >/dev/null
# Over TCP, not the container's unix socket: the entrypoint's bootstrap phase answers on the
# socket while nothing listens on 5432 yet, and a client that connects then is reset.
for i in $(seq 1 60); do
  docker exec "$pg_name" pg_isready -h 127.0.0.1 -p 5432 -U dispatch -d dispatch >/dev/null 2>&1 && break
  [ "$i" = 60 ] && fail "postgres never became ready"
  sleep 0.5
done
pg_port=$(docker port "$pg_name" 5432/tcp | head -1 | sed 's/.*://')
nats_port=$(docker port "$nats_name" 4222/tcp | head -1 | sed 's/.*://')
nats_monitor=$(docker port "$nats_name" 8222/tcp | head -1 | sed 's/.*://')
for i in $(seq 1 60); do
  # -fs, not -fsS: a refused connection is this loop's expected state, not an error to print.
  curl -fs -o /dev/null --max-time 5 "http://127.0.0.1:$nats_monitor/healthz" && break
  [ "$i" = 60 ] && fail "nats never became ready"
  sleep 0.5
done
echo "  postgres 127.0.0.1:$pg_port, nats 127.0.0.1:$nats_port"

database_url="postgres://dispatch:dispatch@127.0.0.1:$pg_port/dispatch"
# The binaries take a port number, not a socket; lib/free-port.sh picks one below the ephemeral
# range, so a port that is taken by the time they bind is a loud boot failure, not a silent pass.
dispatch_port=$(bash "$root/scripts/e2e/lib/free-port.sh") || fail "no free port for dispatch"
listener_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$dispatch_port") || fail "no free port for the listener"
shared_dispatch_token="dispatch-shared-$RANDOM$RANDOM"
shared_listener_token="listener-shared-$RANDOM$RANDOM"

# The environment each binary boots with. HOME is the scratch dir so the signing key and app
# credentials land there and never in the box's ~/.local/share/dispatch, and the working
# directory is the scratch dir so no repo .opencode/envoy.json is picked up.
dispatch_env=(
  env -i PATH="$PATH" HOME="$work/home"
  DATABASE_URL="$database_url"
  DISPATCH_AGENT_TOKEN="$shared_dispatch_token"
  DISPATCH_ALLOWED_LOGINS=sjawhar
  DISPATCH_LISTEN_HOST=127.0.0.1
  DISPATCH_PORT="$dispatch_port"
  NATS_URLS="nats://127.0.0.1:$nats_port"
)
listener_env=(
  env -i PATH="$PATH" HOME="$work/home"
  ENVOY_MACHINE_ID=verifiers-e2e
  NATS_URLS="nats://127.0.0.1:$nats_port"
  ENVOY_LISTEN_HOST=127.0.0.1
  PORT="$listener_port"
  ENVOY_API_TOKEN="$shared_listener_token"
)

echo "== the boot refusals"
cd "$work"
# Half a pair is a refusal naming the variable that is missing, before any network read.
"${dispatch_env[@]}" DISPATCH_OIDC_ISSUER="$issuer" "$work/dispatch" >"$work/refusal-dispatch-pair.log" 2>&1 &&
  fail "dispatch booted with DISPATCH_OIDC_ISSUER and no DISPATCH_OIDC_AUDIENCE"
expect_refusal "dispatch, issuer without audience" "$work/refusal-dispatch-pair.log" \
  "DISPATCH_OIDC_AUDIENCE is required when DISPATCH_OIDC_ISSUER is set"
"${listener_env[@]}" ENVOY_OIDC_AUDIENCE=envoy "$work/listener" >"$work/refusal-listener-pair.log" 2>&1 &&
  fail "the listener booted with ENVOY_OIDC_AUDIENCE and no ENVOY_OIDC_ISSUER"
expect_refusal "listener, audience without issuer" "$work/refusal-listener-pair.log" \
  "ENVOY_OIDC_ISSUER is required when ENVOY_OIDC_AUDIENCE is set"
# An issuer that does not answer stops the boot naming the issuer it could not discover.
"${dispatch_env[@]}" DISPATCH_OIDC_ISSUER="$absent_issuer" DISPATCH_OIDC_AUDIENCE=dispatch \
  "$work/dispatch" >"$work/refusal-dispatch-issuer.log" 2>&1 &&
  fail "dispatch booted against an issuer that does not answer"
expect_refusal "dispatch, issuer that does not answer" "$work/refusal-dispatch-issuer.log" "$absent_issuer"
"${listener_env[@]}" ENVOY_OIDC_ISSUER="$absent_issuer" ENVOY_OIDC_AUDIENCE=envoy \
  "$work/listener" >"$work/refusal-listener-issuer.log" 2>&1 &&
  fail "the listener booted against an issuer that does not answer"
expect_refusal "listener, issuer that does not answer" "$work/refusal-listener-issuer.log" "$absent_issuer"

echo "== mint two real projected service-account tokens"
ns="${VERIFIERS_E2E_NAMESPACE:-legion}"
kube=(kubectl --context "${VERIFIERS_E2E_KUBE_CONTEXT:-staging}" -n "$ns")
dispatch_token=$("${kube[@]}" create token default --audience dispatch --duration 10m)
envoy_token=$("${kube[@]}" create token default --audience envoy --duration 10m)
echo "  dispatch-audience token: $(claims "$dispatch_token")"
echo "  envoy-audience token:    $(claims "$envoy_token")"
subject=$(claims_json "$dispatch_token" | jq -r .sub)
[ "$(claims_json "$dispatch_token" | jq -r .iss)" = "$issuer" ] ||
  fail "the minted token's iss is not the cluster's published issuer"
claims_json "$dispatch_token" | jq -e '.aud == ["dispatch"]' >/dev/null ||
  fail "the dispatch-audience token was not minted for the dispatch audience"
claims_json "$envoy_token" | jq -e '.aud == ["envoy"]' >/dev/null ||
  fail "the envoy-audience token was not minted for the envoy audience"
case "$subject" in
  system:serviceaccount:"$ns":*) ;;
  *) fail "the minted token's subject is not a $ns service account (got \"$subject\")" ;;
esac

echo "== boot both binaries against the staging issuer"
"${dispatch_env[@]}" DISPATCH_OIDC_ISSUER="$issuer" DISPATCH_OIDC_AUDIENCE=dispatch \
  "$work/dispatch" >"$work/dispatch.log" 2>&1 &
dispatch_pid=$!
"${listener_env[@]}" ENVOY_OIDC_ISSUER="$issuer" ENVOY_OIDC_AUDIENCE=envoy \
  "$work/listener" >"$work/listener.log" 2>&1 &
listener_pid=$!
dispatch_url="http://127.0.0.1:$dispatch_port"
listener_url="http://127.0.0.1:$listener_port"
# A binary that refuses to boot is a dead pid, not a slow one: say so at once, with its log,
# instead of spending the whole window on a process that will never answer.
for i in $(seq 1 100); do
  curl -fs -o /dev/null --max-time 5 "$dispatch_url/healthz" && break
  kill -0 "$dispatch_pid" 2>/dev/null || fail "dispatch exited during boot" "$work/dispatch.log"
  [ "$i" = 100 ] && fail "dispatch never answered /healthz" "$work/dispatch.log"
  sleep 0.3
done
# The listener answers /healthz 200 with {"status":"starting"} before NATS is up, and /v1 is
# 503 until then: wait for the healthy status, not for the port.
for i in $(seq 1 100); do
  [ "$(curl -fs --max-time 5 "$listener_url/healthz" | jq -r .status)" = healthy ] && break
  kill -0 "$listener_pid" 2>/dev/null || fail "the listener exited during boot" "$work/listener.log"
  [ "$i" = 100 ] && fail "the listener never reported healthy" "$work/listener.log"
  sleep 0.3
done
grep -qF "$issuer" "$work/dispatch.log" ||
  fail "dispatch's boot log does not name the issuer it verifies against" "$work/dispatch.log"
grep -qF "listener API auth: shared token and oidc $issuer" "$work/listener.log" ||
  fail "the listener's boot log does not name both credentials it accepts" "$work/listener.log"
echo "  dispatch on $dispatch_port, listener on $listener_port, both naming the issuer at boot"

echo "== dispatch: GET /api/v1/whoami"
status=$(request "$dispatch_url/api/v1/whoami" "$dispatch_token")
expect_status "the dispatch-audience token" 200 "$status"
expect_body "the dispatch-audience token" \
  ".kind == \"agent\" and .owner == null and .service == \"$subject\""
echo "    body: $(redact <"$body")"
status=$(request "$dispatch_url/api/v1/whoami" "$envoy_token")
expect_status "the envoy-audience token" 401 "$status"
expect_body "the envoy-audience token" '.code == "OIDC_TOKEN_INVALID"'
echo "    body: $(redact <"$body")"
status=$(request "$dispatch_url/api/v1/whoami")
expect_status "no bearer" 401 "$status"
status=$(request "$dispatch_url/api/v1/whoami" "$shared_dispatch_token")
expect_status "the shared token" 200 "$status"
expect_body "the shared token" '.kind == "agent" and .owner == null and .service == null'
echo "    body: $(redact <"$body")"

echo "== listener: GET /v1/sessions"
status=$(request "$listener_url/v1/sessions" "$envoy_token")
expect_status "the envoy-audience token" 200 "$status"
status=$(request "$listener_url/v1/sessions" "$dispatch_token")
expect_status "the dispatch-audience token" 401 "$status"
status=$(request "$listener_url/v1/sessions")
expect_status "no bearer" 401 "$status"
status=$(request "$listener_url/v1/sessions" "$shared_listener_token")
expect_status "the shared token" 200 "$status"

echo "== each refusal leaves the operator the class, and only in the log"
# Dispatch names the class in its 401 body as well; the listener does not. That
# asymmetry is a deliberate trade, not a claim that the caller is authenticated
# — optionalActor runs before any authorization, so the caller of a Dispatch 401
# is as anonymous as the caller of a listener 401. Dispatch buys operator
# diagnosability with it, and the price is affordable because an issuer and an
# audience are not secrets; the listener declines the same trade because its /v1
# is the flatter surface.
grep -qF 'reason=audience' "$work/dispatch.log" ||
  fail "dispatch did not log the class of the token it refused" "$work/dispatch.log"
grep -qF '"msg":"listener: service-account token rejected"' "$work/listener.log" ||
  fail "the listener logged nothing for the token it refused" "$work/listener.log"
grep -qF '"reason":"audience"' "$work/listener.log" ||
  fail "the listener's rejection line does not name the class" "$work/listener.log"
echo "  dispatch reason=audience, listener reason=audience"

echo "== neither log repeats a token"
for log in "$work/dispatch.log" "$work/listener.log"; do
  grep -q 'eyJ' "$log" && fail "$log contains a token"
done
echo "  dispatch.log and listener.log are clean"

echo "verifiers e2e: PASS"
