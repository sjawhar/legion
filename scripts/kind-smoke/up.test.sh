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
for t in docker kind kubectl go bun tmux ss mise curl; do fake "$t" <<<'exit 0'; done   # setsid, jq, openssl, shred stay real
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
for t in docker go bun tmux ss mise curl jq openssl shred setsid; do
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
method=GET; prev=""; want_code=""; data=""
for a in "$@"; do
  [ "$prev" = -X ] && method="$a"
  [ "$prev" = --data ] && data="$a"
  [ "$prev" = -w ] && want_code=1
  prev="$a"
done
case "$url" in
  *"/healthz") echo '{"status":"ok"}' ;;
  *"/v1/sessions") echo "[]" ;;
  *"/legion/v1/controller/secret") printf '%s' "${FAKE_SECRET_ROUTE_CODE:-404}" ;;
  *"/legion/v1/state") cat "$FAKE_HTTP/state.json" ;;
  *"/api/v1/projects")
    if [ "$method" = POST ]; then printf '%s' "$data" | jq -c '{key:.key}' >"$FAKE_HTTP/projects.json"; jq -c '[.]' "$FAKE_HTTP/projects.json"
    elif [ -f "$FAKE_HTTP/projects.json" ]; then jq -c '[.]' "$FAKE_HTTP/projects.json"; else echo "[]"; fi ;;
  *"/api/v1/settings/repo-projects/"*) echo '{"repo":"sjawhar/legion-smoke","project":"ST1"}' ;;
  *"/api/v1/issues/"*) [ "$method" = PATCH ] || { echo "unexpected $method on $url" >&2; exit 1; }; echo '{"key":"'"${url##*/}"'","status":"todo"}' ;;
  *"/api/v1/issues")
    n="$(cat "$FAKE_HTTP/issue-counter" 2>/dev/null || echo 0)"; n=$((n + 1)); echo "$n" >"$FAKE_HTTP/issue-counter"
    printf '%s' "$data" | jq -e '.project == "ST1" and .force == true and (.spec | contains("kind smoke t1"))' >/dev/null || { echo "bad issue body: $data" >&2; exit 1; }
    echo '{"key":"ST1-'"$n"'"}' ;;
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

# ---- Dispatch seed, overlay, apply, daemon wait, port-forward (SMOKE_STOP_AFTER=daemon) ----------
fake kubectl <<'EOF'
all="$*"
case "$all" in
  *"kustomize "*) dir="${@: -1}"; cat "$dir/legion.yaml" "$dir/kustomization.yaml" ;;
  *"apply -k "*) echo "deployment.apps/legion-daemon-demo created" ;;
  *"rollout status "*) echo 'deployment "legion-daemon-demo" successfully rolled out' ;;
  *"logs deploy/legion-daemon-demo"*) cat "$FAKE_HTTP/daemon.log" ;;
  *"get pod -l app.kubernetes.io/name=legion-daemon -o json"*) cat "$FAKE_HTTP/daemon-pod.json" ;;
  *"describe pod"*) echo "Events: none" ;;
  *"port-forward"*) exec sleep 300 ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF
cat >"$FAKE_HTTP/state.json" <<'EOF'
{"project":"demo","version":33,"issues":{},"trees":{},"admission":{"cap":3,"active":[],"queue":[]},"gates":{},"roles":{},"controllerPendingNotices":0,"pendingStatusWrites":[],"workerAdmission":{"queue":[]}}
EOF
printf '[legion] worker image sha256:%s: probe pod legion-probe-demo-aaaaaaaaaaaa passed: probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5\n' "$(printf 'a%.0s' $(seq 64))" >"$FAKE_HTTP/daemon.log"
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":0}]}}]}' >"$FAKE_HTTP/daemon-pod.json"

run_up SMOKE_STOP_AFTER=daemon >"$tmp/last.txt" || { echo "daemon run failed:" >&2; cat "$tmp/last.txt" >&2; exit 1; }
o="$tmp/state/overlay"
digest="sha256:$(printf 'a%.0s' $(seq 64))"
grep -Fxq 'project: demo' "$o/legion.yaml"
grep -Fxq "    image: $good_image" "$o/legion.yaml"
grep -Fq "digest: $digest" "$o/kustomization.yaml"
! grep -q 'sha256:0000' "$o/kustomization.yaml" "$o/legion.yaml"
grep -Fxq '  - ../base' "$o/kustomization.yaml" && [ -f "$tmp/state/base/deployment.yaml" ]
grep -Fxq 'envoy_url: http://172.30.0.1:41001' "$o/legion.yaml"
grep -Fxq '  - nats://172.30.0.1:41000' "$o/legion.yaml"
grep -Fxq 'dispatch_url: http://172.30.0.1:41002' "$o/legion.yaml"
grep -Fxq 'dispatch_project: ST1' "$o/legion.yaml" && grep -Fxq '  - sjawhar/legion-smoke' "$o/legion.yaml"
grep -Fxq 'worker_cap: 6' "$o/legion.yaml" && grep -Fxq '  design: off' "$o/legion.yaml"
grep -Fxq '    app_id: "3202636"' "$o/legion.yaml" && grep -Fxq '    app_id: "3202653"' "$o/legion.yaml"
grep -Fq 'port: 41000' "$o/networkpolicy-egress.yaml" && grep -Fq 'port: 41001' "$o/networkpolicy-egress.yaml" && grep -Fq 'port: 41002' "$o/networkpolicy-egress.yaml"
! grep -Fq 'port: 41003' "$o/networkpolicy-egress.yaml"      # the daemon never reaches Postgres
grep -Fxq '      - instructions.md' "$o/kustomization.yaml"
grep -Fxq 'ANTHROPIC_API_KEY=anthropic-canary-value' "$o/secrets/providers.env"
! grep -q '^GEMINI_API_KEY=' "$o/secrets/providers.env"        # empty keys are not written
grep -q '^DISPATCH_TOKEN=.\{48\}$' "$o/secrets/providers.env" && grep -q '^ENVOY_TOKEN=.\{48\}$' "$o/secrets/providers.env"
[ "$(stat -c %a "$o/secrets/providers.env")" = 600 ] && [ "$(stat -c %a "$o/secrets")" = 700 ]
head -c 10 "$o/secrets/github-app-implement.pem" | grep -q -- '-----BEGIN'
if [ -f "$here/../../deploy/kubernetes/daemon/overlays/kind/secrets/operator.env.example" ]; then
  grep -q '^OPERATOR_TOKEN=.\{48\}$' "$o/secrets/operator.env"
  grep -Fxq 'operator_token_file: /var/run/legion/operator/OPERATOR_TOKEN' "$o/legion.yaml"
else
  ! grep -q operator_token_file "$o/legion.yaml"
fi
grep -Fq 'kubectl --kubeconfig '"$tmp"'/state/kubeconfig -n legion apply -k '"$o" "$FAKE_LOG"
grep -Fq 'rollout status deploy/legion-daemon-demo --timeout=120s' "$FAKE_LOG"
grep -Fq 'curl -fsS --max-time 20 -X PUT -H X-Dispatch-User: smoke -H content-type: application/json --data {"project":"ST1"} http://172.30.0.1:41002/api/v1/settings/repo-projects/sjawhar/legion-smoke' "$FAKE_LOG"
grep -Fq 'CREATED Dispatch project ST1' "$tmp/last.txt"
[ "$(cat "$tmp/state/records/dispatch-project")" = ST1 ] && [ "$(cat "$tmp/state/records/probe-contract")" = 5 ]
jq -e '.role_profiles.tester == "large" and .resources.large.limits.memory == "12Gi"' "$tmp/state/records/profiles.json" >/dev/null
[ -f "$tmp/state/pids/port-forward.pid" ]
grep -Fq 'port-forward --address 127.0.0.1 svc/legion-daemon-demo 41004:13370' "$FAKE_LOG"
grep -Fq 'stopped after daemon' "$tmp/last.txt"
! grep -Fq 'anthropic-canary-value' "$FAKE_LOG" "$tmp/last.txt"   # the provider key never reaches argv or stdout
for s in dispatch-token envoy-token postgres-password; do
  ! grep -Fq "$(cat "$tmp/state/secrets/$s")" "$FAKE_LOG" "$tmp/last.txt" || { echo "secret $s leaked into argv or output" >&2; exit 1; }
done
# rerun: the project is reused, nothing re-created
run_up SMOKE_STOP_AFTER=daemon >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED Dispatch project ST1' "$tmp/last2.txt" && grep -Fq 'REUSED port-forward' "$tmp/last2.txt"
# a crash-looping daemon is reported with its log and the cluster is left for inspection
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":2}]}}]}' >"$FAKE_HTTP/daemon-pod.json"
printf 'Unknown config key "operator_token_file" in /etc/legion/legion.yaml\n' >"$FAKE_HTTP/daemon.log"
if run_up SMOKE_STOP_AFTER=daemon >"$tmp/last3.txt"; then echo "crash-loop should fail" >&2; exit 1; fi
grep -Fq 'the daemon pod is crash-looping (2 restarts); its last log lines:' "$tmp/last3.txt"
grep -Fq 'Unknown config key "operator_token_file"' "$tmp/last3.txt"
grep -Fq 'left for inspection' "$tmp/last3.txt"
! grep -Fq 'kind delete' "$FAKE_LOG"
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":0}]}}]}' >"$FAKE_HTTP/daemon-pod.json"
printf '[legion] worker image sha256:%s: probe pod legion-probe-demo-aaaaaaaaaaaa passed: probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5\n' "$(printf 'a%.0s' $(seq 64))" >"$FAKE_HTTP/daemon.log"
echo "up.test.sh: overlay and daemon OK"
