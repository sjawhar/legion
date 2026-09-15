#!/usr/bin/env bash
# Harness for scripts/kind-smoke/up.sh: every external binary is a PATH fake that logs its argv, so the
# script's decisions, records, and refusals are pinned without docker, kind, kubectl, go, tmux, or a
# network. Needs only bash, coreutils, and jq.
set -euo pipefail
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
cleanup() {
  local f
  for f in "$tmp"/state*/pids/*.pid; do
    [ -f "$f" ] || continue
    kill -- "-$(<"$f")" 2>/dev/null || kill "$(<"$f")" 2>/dev/null || true
  done
  rm -rf -- "$tmp"
}
trap cleanup EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"

fake() { # fake NAME <<'EOF' body EOF — every fake logs "NAME argv" to $FAKE_LOG, then runs the body
  {
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}
for t in docker kind kubectl go bun tmux ss mise setsid curl; do fake "$t" <<<'exit 0'; done
export PATH="$fake_bin:$PATH"

good_image="ghcr.io/sjawhar/legion-worker@sha256:$(printf 'a%.0s' $(seq 64))"
pem_b64() { printf -- '-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n' "$1" | base64 -w0; }

run_up() { # run_up ENV… — runs up.sh with the harness environment; captures stdout+stderr; returns its exit code
  local out="$tmp/out.txt"
  : >"$out"
  set +e
  env SMOKE_DIR="$tmp/state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WORKER_IMAGE="$good_image" \
    GH_AGENT_APP_PRIVATE_KEY_B64="$(pem_b64 x)" GH_REVIEW_APP_PRIVATE_KEY_B64="$(pem_b64 y)" \
    ANTHROPIC_API_KEY=anthropic-canary-value GEMINI_API_KEY= OPENAI_API_KEY= \
    "$@" bash "$here/up.sh" >"$out" 2>&1
  local status=$?
  set -e
  cat "$out"
  return $status
}
expect_refusal() { # expect_refusal 'substring' ENV… — up.sh must exit 1 with the text and create nothing
  local want="$1"
  shift
  if run_up "$@" >"$tmp/last.txt"; then
    echo "expected refusal: $want" >&2
    cat "$tmp/last.txt" >&2
    exit 1
  fi
  grep -Fq -- "$want" "$tmp/last.txt" || { echo "missing refusal text: $want" >&2; cat "$tmp/last.txt" >&2; exit 1; }
  ! grep -Eq '^(docker (run|start|rm|exec)|kind (create|delete)|kubectl (apply|delete|port-forward)|go build|tmux new-session|bun run) ' "$FAKE_LOG" || { echo "refusal created something:" >&2; cat "$FAKE_LOG" >&2; exit 1; }
  : >"$FAKE_LOG"
}

# 1. a tag is refused before anything is created
expect_refusal 'SMOKE_WORKER_IMAGE must be pinned by digest (…@sha256:<64 hex>); got ghcr.io/sjawhar/legion-worker:latest' \
  SMOKE_WORKER_IMAGE=ghcr.io/sjawhar/legion-worker:latest
# 2. instance name rules
expect_refusal 'SMOKE_INSTANCE must be 1-9 lowercase letters or digits' SMOKE_INSTANCE=this-is-way-too-long
# 3. every missing tool is named in one line: a PATH with fakes for everything but kind and kubectl
nokind="$tmp/bin-nokind"
mkdir -p "$nokind"
for t in docker go bun tmux ss mise setsid curl jq openssl shred; do
  [ -e "$fake_bin/$t" ] && cp "$fake_bin/$t" "$nokind/$t" || ln -s "$(command -v "$t")" "$nokind/$t"
done
ln -s "$(command -v bash)" "$nokind/bash"
expect_refusal 'missing required tools: kind kubectl' PATH="$nokind"
# 4. an occupied port names the port and SMOKE_PORT_BASE
fake ss <<'EOF'
if [[ "$*" == *"sport = :41002"* ]]; then echo "LISTEN 0 4096 172.30.0.1:41002 0.0.0.0:*"; fi
EOF
expect_refusal "port 41002 (dispatch) is already in use and is not this instance's; choose another SMOKE_PORT_BASE (current 41000)"
fake ss <<<'exit 0'
# 5. no provider key in the environment
expect_refusal 'no provider key in the environment (ANTHROPIC_API_KEY, GEMINI_API_KEY, or OPENAI_API_KEY)' ANTHROPIC_API_KEY=
# 6. no App private key in either form
expect_refusal 'GH_AGENT_APP_PRIVATE_KEY_B64 is unset and SMOKE_IMPLEMENT_APP_KEY_FILE is unset' GH_AGENT_APP_PRIVATE_KEY_B64=
# 6b. a PEM path that is not readable
expect_refusal "SMOKE_REVIEW_APP_KEY_FILE names $tmp/absent.pem, which is not readable" GH_REVIEW_APP_PRIVATE_KEY_B64= SMOKE_REVIEW_APP_KEY_FILE="$tmp/absent.pem"
# 7. postgres session store needs the checkout to carry it (this checkout is judged by grep on config.ts)
if ! grep -q session_dsn_secret "$here/../../packages/daemon/src/daemon/config.ts"; then
  expect_refusal 'SMOKE_SESSION_STORE=postgres needs runtime.kubernetes.session_store in packages/daemon/src/daemon/config.ts (pull request #1108)' SMOKE_SESSION_STORE=postgres
fi
# 8. bad mode values
expect_refusal 'SMOKE_GITHUB_INGRESS must be none or envoy; got webhook' SMOKE_GITHUB_INGRESS=webhook
expect_refusal 'SMOKE_ROOT_ISSUES must be a positive integer; got 0' SMOKE_ROOT_ISSUES=0
echo "up.test.sh: refusals OK"

# ---- happy path through the host services (SMOKE_STOP_AFTER=host-services) ------------------------
export FAKE_CLUSTERS="$tmp/clusters"
: >"$FAKE_CLUSTERS"
export FAKE_CONTAINERS="$tmp/containers"
mkdir -p "$FAKE_CONTAINERS"
fake kind <<'EOF'
case "$*" in
  "get clusters") cat "$FAKE_CLUSTERS" 2>/dev/null ;;
  create\ cluster*)
    all="$*"; name="${all#*--name }"; name="${name%% *}"; echo "$name" >>"$FAKE_CLUSTERS"
    all="$*"; kc="${all#*--kubeconfig }"; kc="${kc%% *}"; printf 'apiVersion: v1\nkind: Config\n' >"$kc" ;;
  export\ kubeconfig*) all="$*"; kc="${all#*--kubeconfig }"; kc="${kc%% *}"; printf 'apiVersion: v1\nkind: Config\n' >"$kc" ;;
  delete\ cluster*) all="$*"; name="${all#*--name }"; name="${name%% *}"; grep -Fxv -- "$name" "$FAKE_CLUSTERS" >"$FAKE_CLUSTERS.new" || true; mv "$FAKE_CLUSTERS.new" "$FAKE_CLUSTERS" ;;
esac
EOF
fake docker <<'EOF'
case "$*" in
  "network inspect kind"*) printf 'fc00:f853:ccd:e793::1\n172.30.0.1\n' ;;
  "inspect -f {{.State.Running}} "*) [ -f "$FAKE_CONTAINERS/${@: -1}" ] && echo true || { echo "Error: No such object" >&2; exit 1; } ;;
  "inspect -f {{index .Config.Labels \"legion-smoke.instance\"}} "*) [ -f "$FAKE_CONTAINERS/${@: -1}" ] && cat "$FAKE_CONTAINERS/${@: -1}" || { echo "Error: No such object" >&2; exit 1; } ;;
  "inspect "*) [ -f "$FAKE_CONTAINERS/${@: -1}" ] || { echo "Error: No such object" >&2; exit 1; } ;;
  run*) all="$*"; name="${all#*--name }"; name="${name%% *}"; echo t1 >"$FAKE_CONTAINERS/$name"; echo deadbeef ;;
  start*) exit 0 ;;
  "exec "*pg_isready*) exit 0 ;;
  "exec "*psql*) echo 1 ;;
  "exec "*) exit 0 ;;
  "rm -f "*) rm -f "$FAKE_CONTAINERS/${@: -1}" ;;
esac
EOF
fake go <<'EOF'
case "$*" in
  build*) all="$*"; out="${all#*-o }"; out="${out%% *}"; printf '#!/usr/bin/env bash\nexec sleep 300\n' >"$out"; chmod +x "$out" ;;
esac
EOF
export FAKE_HTTP="$tmp/http"
mkdir -p "$FAKE_HTTP"
fake curl <<'EOF'
# routes answered by URL substring; bodies the harness plants under $FAKE_HTTP
url=""; for a in "$@"; do case "$a" in http*) url="$a" ;; esac; done
method=GET; prev=""; for a in "$@"; do [ "$prev" = -X ] && method="$a"; prev="$a"; done
case "$url" in
  *"/healthz") echo '{"status":"ok"}' ;;
  *"/v1/sessions") echo "[]" ;;
  *"/api/v1") echo '{"routes":[]}' ;;
  *) echo "unexpected curl request: $*" >&2; exit 1 ;;
esac
EOF

run_up SMOKE_STOP_AFTER=host-services >"$tmp/last.txt" || { echo "host-services run failed:" >&2; cat "$tmp/last.txt" >&2; exit 1; }
grep -Fxq 'legion-smoke-t1' "$FAKE_CLUSTERS"
grep -Fq 'kind create cluster --name legion-smoke-t1 --kubeconfig '"$tmp"'/state/kubeconfig' "$FAKE_LOG"
! grep -Fq -- '.kube/config' "$FAKE_LOG"                          # the kubeconfig is under SMOKE_DIR, never ~/.kube/config
[ "$(cat "$tmp/state/records/gateway")" = 172.30.0.1 ]            # the IPv4 entry, not the IPv6 one
[ "$(cat "$tmp/state/records/cluster")" = legion-smoke-t1 ]
grep -Fq 'docker run -d --name legion-smoke-t1-nats --label legion-smoke.instance=t1 -p 172.30.0.1:41000:4222 nats:2.10 -js' "$FAKE_LOG"
grep -Eq 'docker run -d --name legion-smoke-t1-postgres --label legion-smoke.instance=t1 -p 172.30.0.1:41003:5432 .* postgres:16' "$FAKE_LOG"
! grep -Eq 'POSTGRES_PASSWORD=[^ ]' "$FAKE_LOG" || { echo "postgres password in argv" >&2; exit 1; }
grep -Fq 'go build -o' "$FAKE_LOG"
[ -x "$tmp/state/bin/envoy-listener" ] && [ -x "$tmp/state/bin/envoy-dispatch" ]
[ "$(stat -c %a "$tmp/state/secrets/dispatch-token")" = 600 ] && [ "$(stat -c %a "$tmp/state/secrets")" = 700 ]
for name in listener dispatch; do [ -f "$tmp/state/pids/$name.pid" ] && [ -f "$tmp/state/pids/$name.start" ]; done
grep -Fq 'STARTED listener' "$tmp/last.txt" && grep -Fq 'STARTED dispatch' "$tmp/last.txt"
grep -Fq 'stopped after host-services' "$tmp/last.txt"
# no secret value reached an argv or the output
for s in dispatch-token envoy-token postgres-password; do
  ! grep -Fq "$(cat "$tmp/state/secrets/$s")" "$FAKE_LOG" "$tmp/last.txt" || { echo "secret $s leaked into argv or output" >&2; exit 1; }
done
! grep -Fq 'anthropic-canary-value' "$FAKE_LOG" "$tmp/last.txt"
# a second run reuses everything
calls_before="$(wc -l <"$FAKE_LOG")"
run_up SMOKE_STOP_AFTER=host-services >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED listener' "$tmp/last2.txt" && grep -Fq 'REUSED dispatch' "$tmp/last2.txt"
grep -Fq 'REUSED cluster legion-smoke-t1' "$tmp/last2.txt" && grep -Fq 'REUSED container legion-smoke-t1-nats' "$tmp/last2.txt"
! tail -n +"$((calls_before + 1))" "$FAKE_LOG" | grep -Eq '^(kind create|docker run)'
echo "up.test.sh: host services OK"
