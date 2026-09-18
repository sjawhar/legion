#!/usr/bin/env bash
# Harness for scripts/kind-smoke/up.sh: every external binary is a PATH fake that logs its argv, so the
# script's decisions, records, and refusals are pinned without docker, kind, kubectl, go, tmux, or a
# network. Needs only bash, coreutils, and jq.
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/kind-smoke/test-lib.sh
source "$here/test-lib.sh"
tmp="$(mktemp -d)"
cleanup() {
  local f
  for f in "$tmp"/*state/pids/*.pid; do
    [ -f "$f" ] || continue
    kill -- "-$(<"$f")" 2>/dev/null || kill "$(<"$f")" 2>/dev/null || true
  done
  rm -rf -- "$tmp"
}
trap cleanup EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
export FAKE_HOST_MANIFEST="$tmp/host-secret.json"
: >"$FAKE_LOG"

fake() { # fake NAME <<'EOF' body EOF — every fake logs "NAME argv" to $FAKE_LOG, then runs the body
  {
    # shellcheck disable=SC2016  # The generated fake expands these variables when it runs.
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}
REAL_BUN=""
REAL_BUN="$(command -v bun)"
export REAL_BUN
for t in docker kind kubectl go bun tmux ss mise curl omp; do fake "$t" <<<'exit 0'; done   # setsid, jq, openssl, shred stay real
export PATH="$fake_bin:$PATH"
REAL_JQ=""
REAL_JQ="$(command -v jq)"
export REAL_JQ
fake jq <<'EOF'
printf 'jq %s\n' "$*" >>"$FAKE_LOG"
exec "$REAL_JQ" "$@"
EOF

good_image="ghcr.io/sjawhar/legion-worker@sha256:$(printf 'a%.0s' $(seq 64))"
pem_b64() { printf -- '-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n' "$1" | base64 -w0; }

# secret_values → the values the run holds, raw and base64-encoded (the form a rendered Secret carries):
# the instance's generated tokens, the canary provider key, the two fake PEMs
secret_values() {
  local f
  for f in "$tmp"/state/secrets/dispatch-token "$tmp"/state/secrets/envoy-token "$tmp"/state/secrets/postgres-password "$tmp"/state/secrets/operator-token; do
    [ -s "$f" ] || continue
    cat "$f"
    tr -d '\n' <"$f" | base64 -w0
    echo
  done
  printf '%s\n%s\n' anthropic-canary-value "$(printf '%s' anthropic-canary-value | base64 -w0)"
  for f in "$tmp"/state/overlay/secrets/github-app-implement.pem "$tmp"/state/overlay/secrets/github-app-review.pem; do
    [ -s "$f" ] || continue
    base64 -w0 <"$f" | cut -c1-40   # the rendered form …
    echo
    sed -n '2p' "$f"                # … and the key body itself (the fake PEMs' one body line)
  done
}
# refute_secret_leak — no secret value in the fakes' argv log, in the run's output, or in any file
# under the state directory outside a secrets/ directory (a rendered manifest, a log, a record)
refute_secret_leak() {
  local values="$tmp/values"
  secret_values | sed '/^$/d' >"$values"   # an empty pattern would match every line
  [ "$(wc -l <"$values")" -ge 3 ]           # the canary, its base64, and at least one generated token
  refute grep -Fq -f "$values" "$FAKE_LOG" "$tmp/last.txt"
  local outside
  outside="$(find "$tmp/state" -type f -not -path '*/secrets/*' 2>/dev/null)"
  [ -n "$outside" ]
  # shellcheck disable=SC2086  # one path per line, none with spaces (the harness creates them)
  refute grep -lF -f "$values" $outside
  rm -f "$values"
}
run_up() { # run_up ENV… — runs up.sh with the harness environment; captures stdout+stderr; returns its exit code
  local out="$tmp/out.txt" smoke_state="$tmp/state" arg
  for arg in "$@"; do
    case "$arg" in SMOKE_TEST_STATE=*) smoke_state="${arg#SMOKE_TEST_STATE=}" ;; esac
  done
  : >"$out"
  local status=0
  env HOME="$tmp/home" SMOKE_DIR="$smoke_state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WORKER_IMAGE="$good_image" \
    GH_AGENT_APP_PRIVATE_KEY_B64="$(pem_b64 implement-pem-body-canary-9f3c)" GH_REVIEW_APP_PRIVATE_KEY_B64="$(pem_b64 review-pem-body-canary-2b7e)" \
    ANTHROPIC_API_KEY=anthropic-canary-value GEMINI_API_KEY= OPENAI_API_KEY= \
    "$@" bash "$here/up.sh" >"$out" 2>&1 || status=$?
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
  # a refusal creates nothing; `bun run … controller start --help` (checkout_has_controller, before
  # check_ports) is a read-only probe of the checkout, not a process the rig started
  refute grep -Eq '^(docker (run|start|rm|exec)|kind (create|delete)|kubectl (apply|delete|port-forward)|go build|tmux new-session|bun run) ' <(sed '/ --help$/d' "$FAKE_LOG")
  : >"$FAKE_LOG"
}

# 1. a tag is refused before anything is created
expect_refusal 'SMOKE_WORKER_IMAGE must be pinned by digest (…@sha256:<64 hex>); got ghcr.io/sjawhar/legion-worker:latest' \
  SMOKE_WORKER_IMAGE=ghcr.io/sjawhar/legion-worker:latest
# 2. instance name rules
expect_refusal 'SMOKE_INSTANCE must be 1-9 lowercase letters or digits' SMOKE_INSTANCE=this-is-way-too-long
expect_refusal 'SMOKE_DIR must be absolute; got relative-state' SMOKE_DIR=relative-state
# 3. every missing tool is named in one line: a PATH with fakes for everything but kind and kubectl
nokind="$tmp/bin-nokind"
mkdir -p "$nokind"
for t in docker go bun tmux ss mise curl jq openssl shred setsid; do
  if [ -e "$fake_bin/$t" ]; then
    cp "$fake_bin/$t" "$nokind/$t"
  else
    ln -s "$(command -v "$t")" "$nokind/$t"
  fi
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
export FAKE_ENV="$tmp/env"
mkdir -p "$FAKE_ENV"
# the fakes that stand in for a host process record the names (never the values) of their environment under $FAKE_ENV/<name>
fake go <<'EOF'
case "$*" in
  build*) all="$*"; out="${all#*-o }"; out="${out%% *}"; printf '#!/usr/bin/env bash\nenv | cut -d= -f1 | sort >"$FAKE_ENV/$(basename "$0")"\nexec sleep 300\n' >"$out"; chmod +x "$out" ;;
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
refute grep -Fq -- '.kube/config' "$FAKE_LOG"                          # the kubeconfig is under SMOKE_DIR, never ~/.kube/config
[ "$(cat "$tmp/state/records/gateway")" = 172.30.0.1 ]            # the IPv4 entry, not the IPv6 one
[ "$(cat "$tmp/state/records/cluster")" = legion-smoke-t1 ]
grep -Fq 'docker run -d --name legion-smoke-t1-nats --label legion-smoke.instance=t1 -p 172.30.0.1:41000:4222 nats:2.10 -js' "$FAKE_LOG"
grep -Eq 'docker run -d --name legion-smoke-t1-postgres --label legion-smoke.instance=t1 -p 172.30.0.1:41003:5432 .* postgres:16' "$FAKE_LOG"
refute grep -Eq 'POSTGRES_PASSWORD=[^ ]' "$FAKE_LOG"   # the postgres password travels by --env-file, never argv
grep -Fq 'go build -o' "$FAKE_LOG"
[ -x "$tmp/state/bin/envoy-listener" ]
[ -x "$tmp/state/bin/envoy-dispatch" ]
[ "$(stat -c %a "$tmp/state/secrets/dispatch-token")" = 600 ]
[ "$(stat -c %a "$tmp/state/secrets")" = 700 ]
for name in listener dispatch; do
  [ -f "$tmp/state/pids/$name.pid" ]
  [ -f "$tmp/state/pids/$name.start" ]
  # the record names the process itself, never a shell wrapper around it: a backgrounded function is a
  # bash subshell still running up.sh (cmdline `bash …/up.sh`), a wrapper that forwards no signal
  refute grep -Fq -- 'up.sh' <(tr '\0' ' ' <"/proc/$(cat "$tmp/state/pids/$name.pid")/cmdline")
done
grep -Fq 'STARTED listener' "$tmp/last.txt"
grep -Fq 'STARTED dispatch' "$tmp/last.txt"
grep -Fq 'stopped after host-services' "$tmp/last.txt"
# no secret value reached an argv or the output
refute_secret_leak
refute grep -Fq 'anthropic-canary-value' "$FAKE_LOG" "$tmp/last.txt"
# a second run reuses everything
calls_before="$(wc -l <"$FAKE_LOG")"
run_up SMOKE_STOP_AFTER=host-services >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED listener' "$tmp/last2.txt"
grep -Fq 'REUSED dispatch' "$tmp/last2.txt"
grep -Fq 'REUSED cluster legion-smoke-t1' "$tmp/last2.txt"
grep -Fq 'REUSED container legion-smoke-t1-nats' "$tmp/last2.txt"
refute grep -Eq '^(kind create|docker run)' <(tail -n +"$((calls_before + 1))" "$FAKE_LOG")
echo "up.test.sh: host services OK"

# ---- Dispatch seed, overlay, apply, daemon wait, port-forward (SMOKE_STOP_AFTER=daemon) ----------
fake kubectl <<'EOF'
all="$*"
case "$all" in
  *"kustomize "*)
    # what kubectl kustomize renders: the configMap with legion.yaml, and the two secretGenerators
    # as kind: Secret with base64 data — every secret of the run in one document
    dir="${@: -1}"
    cat "$dir/legion.yaml" "$dir/kustomization.yaml"
    # FAKE_KUSTOMIZE_PLACEHOLDER=1: the render still carries the zero digest deep inside (as a real
    # render would if the sed had missed a place) — up.sh must refuse
    if [ -n "${FAKE_KUSTOMIZE_PLACEHOLDER:-}" ]; then for _ in $(seq 400); do echo "        image: ghcr.io/sjawhar/legion-worker@sha256:$(printf 'b%.0s' $(seq 64))"; done; echo "  digest: sha256:0000000000000000000000000000000000000000000000000000000000000000"; fi
    printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: legion-demo-providers\ndata:\n'
    printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: legion-demo-daemon\ndata:\n'
    printf '  github-app-implement.pem: %s\n  github-app-review.pem: %s\n' "$(base64 -w0 <"$dir/secrets/github-app-implement.pem")" "$(base64 -w0 <"$dir/secrets/github-app-review.pem")" ;;
  *"config view --raw "*)
    cat <<'JSON'
{"apiVersion":"v1","kind":"Config","clusters":[{"name":"kind-legion-smoke-t1","cluster":{"server":"https://127.0.0.1:6443","certificate-authority-data":"Y2E="}}],"contexts":[{"name":"kind-legion-smoke-t1","context":{"cluster":"kind-legion-smoke-t1","user":"kind-legion-smoke-t1"}}],"current-context":"kind-legion-smoke-t1","users":[{"name":"kind-legion-smoke-t1","user":{"client-certificate-data":"Y2VydA=="}}]}
JSON
    ;;
  *"get nodes -l !node-role.kubernetes.io/control-plane -o jsonpath="*) printf 'legion-smoke-t1-worker\n' ;;
  *"label node legion-smoke-t1-worker legion.dev/pool=legion --overwrite"*) ;;
  *"taint node legion-smoke-t1-worker legion.dev/pool=legion:NoSchedule --overwrite"*) ;;
  *"create namespace legion"*) ;;
  *"apply -f "*"/serviceaccount.yaml"*"/role.yaml"*"/rolebinding.yaml"*) ;;
  *"apply -f -"*) doc="$(cat)"; printf '%s' "$doc" >"$FAKE_HOST_MANIFEST"; if [ "${FAKE_HOST_SECRET_APPLY_FAIL:-}" = 1 ] && [[ "$doc" == *Secret* ]]; then exit 1; fi; echo 'secret/legion-demo-providers configured' ;;
  *"create token legion-daemon"*) printf 'service-account-token\n' ;;
  *"get node legion-smoke-t1-worker -o jsonpath="*) printf 'legion\n' ;;
  *"describe node legion-smoke-t1-worker"*) printf 'Taints: legion.dev/pool=legion:NoSchedule\n' ;;
  *"get priorityclass legion -o jsonpath="*) printf '1000\n' ;;
  *"get deploy -o name"*) ;;
  *"apply -k "*) echo "deployment.apps/legion-daemon-demo created" ;;
  *"rollout status "*) echo 'deployment "legion-daemon-demo" successfully rolled out' ;;
  *"logs deploy/legion-daemon-"*) cat "$FAKE_HTTP/daemon.log" ;;
  *"get pod -l app.kubernetes.io/name=legion-daemon -o json"*) cat "$FAKE_HTTP/daemon-pod.json" ;;
  *"get pods -l legion.dev/project,!legion.dev/probe"*) env | cut -d= -f1 | sort >"$FAKE_ENV/legion-177-keeper"; echo "" ;;
  *"describe pod"*) echo "Events: none" ;;
  *"port-forward"*) env | cut -d= -f1 | sort >"$FAKE_ENV/port-forward"; exec sleep 300 ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF
cat >"$FAKE_HTTP/state.json" <<'EOF'
{"project":"demo","version":33,"issues":{},"trees":{},"admission":{"cap":3,"active":[],"queue":[]},"gates":{},"roles":{},"controllerPendingNotices":0,"pendingStatusWrites":[],"workerAdmission":{"queue":[]}}
EOF
printf '[legion] worker image sha256:%s: probe pod legion-probe-demo-aaaaaaaaaaaa passed: probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5\n' "$(printf 'a%.0s' $(seq 64))" >"$FAKE_HTTP/daemon.log"
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":0}]}}]}' >"$FAKE_HTTP/daemon-pod.json"

run_up SMOKE_INSTANCE=t-1 SMOKE_STOP_AFTER=daemon >"$tmp/last.txt" || { echo "daemon run failed:" >&2; cat "$tmp/last.txt" >&2; exit 1; }
o="$tmp/state/overlay"
digest="sha256:$(printf 'a%.0s' $(seq 64))"
cluster_project="$("$REAL_BUN" -e "import { legionProjectToken } from \"$here/../../packages/daemon/src/daemon/config.ts\"; console.log(legionProjectToken('smoke-t1', 'LEGION_ID'))")"
assert_eq "$(<"$tmp/state/records/project")" "$cluster_project"
grep -Fxq "project: $cluster_project" "$o/legion.yaml"
grep -Fxq "    image: $good_image" "$o/legion.yaml"
grep -Fq "digest: $digest" "$o/kustomization.yaml"
refute grep -q 'sha256:0000' "$o/kustomization.yaml" "$o/legion.yaml"
grep -Fxq '  - ../base' "$o/kustomization.yaml"
[ -f "$tmp/state/base/deployment.yaml" ]
grep -Fxq "          command: [legion, start, $cluster_project, --config, /etc/legion/legion.yaml]" "$tmp/state/base/deployment.yaml"
grep -Fq "legion-daemon-$cluster_project" "$o/kustomization.yaml" "$tmp/state/base/deployment.yaml"
grep -Fxq 'envoy_url: http://172.30.0.1:41001' "$o/legion.yaml"
grep -Fxq '  - nats://172.30.0.1:41000' "$o/legion.yaml"
grep -Fxq 'dispatch_url: http://172.30.0.1:41002' "$o/legion.yaml"
grep -Fxq 'projects:' "$o/legion.yaml"
grep -Fxq '  ST1: { repo: sjawhar/legion-smoke }' "$o/legion.yaml"
refute grep -q '^dispatch_project:\|^repos:' "$o/legion.yaml"
grep -Fxq 'worker_cap: 6' "$o/legion.yaml"
grep -Fxq '  design: off' "$o/legion.yaml"
grep -Fxq 'resync_interval_seconds: 60' "$o/legion.yaml"
[ "$(cat "$tmp/state/records/resync-interval")" = 60 ]
grep -Fxq 'worker_idle_retire_seconds: 600' "$o/legion.yaml"
[ "$(cat "$tmp/state/records/worker-idle-retire")" = 600 ]
grep -Fxq '    app_id: "3202636"' "$o/legion.yaml"
grep -Fxq '    app_id: "3202653"' "$o/legion.yaml"
grep -Fq 'port: 41000' "$o/networkpolicy-egress.yaml"
grep -Fq 'port: 41001' "$o/networkpolicy-egress.yaml"
grep -Fq 'port: 41002' "$o/networkpolicy-egress.yaml"
refute grep -Fq 'port: 41003' "$o/networkpolicy-egress.yaml"      # the daemon never reaches Postgres
grep -Fxq '      - instructions.md' "$o/kustomization.yaml"
grep -Fxq 'ANTHROPIC_API_KEY=anthropic-canary-value' "$o/secrets/providers.env"
refute grep -q '^GEMINI_API_KEY=' "$o/secrets/providers.env"        # empty keys are not written
grep -q '^DISPATCH_TOKEN=.\{48\}$' "$o/secrets/providers.env"
grep -q '^ENVOY_TOKEN=.\{48\}$' "$o/secrets/providers.env"
[ "$(stat -c %a "$o/secrets/providers.env")" = 600 ]
[ "$(stat -c %a "$o/secrets")" = 700 ]
head -c 10 "$o/secrets/github-app-implement.pem" | grep -q -- '-----BEGIN'
if [ -f "$here/../../deploy/kubernetes/daemon/overlays/kind/secrets/operator.env.example" ]; then
  grep -q '^OPERATOR_TOKEN=.\{48\}$' "$o/secrets/operator.env"
  grep -Fxq 'operator_token_file: /var/run/legion/operator/OPERATOR_TOKEN' "$o/legion.yaml"
else
  refute grep -q operator_token_file "$o/legion.yaml"
fi
grep -Fq 'kubectl --kubeconfig '"$tmp"'/state/kubeconfig -n legion apply -k '"$o" "$FAKE_LOG"
grep -Fq "rollout status deploy/legion-daemon-$cluster_project --timeout=120s" "$FAKE_LOG"
# the host processes' environments (recorded by the fakes as names only): each has what it needs and none of the
# App private keys or provider keys, which reach them only as files (r4012899787)
grep -Fxq ENVOY_API_TOKEN "$FAKE_ENV/envoy-listener"
grep -Fxq DISPATCH_AGENT_TOKEN "$FAKE_ENV/envoy-dispatch"
grep -Fxq DATABASE_URL "$FAKE_ENV/envoy-dispatch"
for f in envoy-listener envoy-dispatch port-forward legion-177-keeper; do
  [ -s "$FAKE_ENV/$f" ]
  refute grep -Eq '^(GH_AGENT_APP_PRIVATE_KEY_B64|GH_REVIEW_APP_PRIVATE_KEY_B64|ANTHROPIC_API_KEY|GEMINI_API_KEY|OPENAI_API_KEY)$' "$FAKE_ENV/$f"
done
grep -Fq 'kustomize '"$o" "$FAKE_LOG"                      # the render is validated and checked for the placeholder …
[ ! -e "$tmp/state/rendered.yaml" ]                        # … but never written to disk
grep -Fq 'curl -fsS --max-time 20 -X PUT -H X-Dispatch-User: smoke -H content-type: application/json --data {"project":"ST1"} http://172.30.0.1:41002/api/v1/settings/repo-projects/sjawhar/legion-smoke' "$FAKE_LOG"
grep -Fq 'CREATED Dispatch project ST1' "$tmp/last.txt"
grep -Fq "port-forward --address 127.0.0.1 svc/legion-daemon-$cluster_project 41004:13370" "$FAKE_LOG"
[ "$(cat "$tmp/state/records/probe-contract")" = 5 ]
jq -e '.role_profiles.tester == "large" and .resources.large.limits.memory == "12Gi"' "$tmp/state/records/profiles.json" >/dev/null
[ -f "$tmp/state/pids/port-forward.pid" ]
[ -f "$tmp/state/pids/legion-177-keeper.pid" ]
[ "$(cat "$tmp/state/records/legion-177-workaround")" = keeper ]
grep -Fq 'STARTED legion-177-keeper' "$tmp/last.txt"
grep -Fq 'stopped after daemon' "$tmp/last.txt"
refute_secret_leak   # the provider key, the tokens, and the PEMs never reach argv, stdout, or a file outside secrets/
# rerun: the project is reused, nothing re-created
run_up SMOKE_STOP_AFTER=daemon >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED Dispatch project ST1' "$tmp/last2.txt"
grep -Fq 'REUSED port-forward' "$tmp/last2.txt"
# a render that still carries the placeholder digest (deep in a large document) is refused, and nothing is applied
: >"$FAKE_LOG"
status=0
run_up SMOKE_STOP_AFTER=daemon FAKE_KUSTOMIZE_PLACEHOLDER=1 >"$tmp/last3.txt" || status=$?
[ "$status" = 1 ]
grep -Fq 'error: the rendered overlay still carries the placeholder digest' "$tmp/last3.txt"
refute grep -Fq 'apply -k' "$FAKE_LOG"
# a crash-looping daemon is reported with its log and the cluster is left for inspection
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":2}]}}]}' >"$FAKE_HTTP/daemon-pod.json"
printf 'Unknown config key "operator_token_file" in /etc/legion/legion.yaml\n' >"$FAKE_HTTP/daemon.log"
if run_up SMOKE_STOP_AFTER=daemon >"$tmp/last3.txt"; then echo "crash-loop should fail" >&2; exit 1; fi
grep -Fq 'the daemon pod is crash-looping (2 restarts); its last log lines:' "$tmp/last3.txt"
grep -Fq 'Unknown config key "operator_token_file"' "$tmp/last3.txt"
grep -Fq 'left for inspection' "$tmp/last3.txt"
refute grep -Fq 'kind delete' "$FAKE_LOG"
echo '{"items":[{"status":{"containerStatuses":[{"restartCount":0}]}}]}' >"$FAKE_HTTP/daemon-pod.json"
printf '[legion] worker image sha256:%s: probe pod legion-probe-demo-aaaaaaaaaaaa passed: probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=5\n' "$(printf 'a%.0s' $(seq 64))" >"$FAKE_HTTP/daemon.log"
echo "up.test.sh: overlay and daemon OK"

# ---- controller decision, GitHub bridge, root issues, summary (a full run) ----------------------
export FAKE_TMUX="$tmp/tmux"
mkdir -p "$FAKE_TMUX"
fake bun <<'EOF'
all="$*"
case "$all" in
  *"controller start --help") exit "${FAKE_CONTROLLER_HELP_EXIT:-1}" ;;
  *omp-pin*) echo "github:sjawhar/oh-my-pi@18.1.21-sami.20260914-080519" ;;
  *"cli/index.ts start "*)
    printf '%s\n' "${OMP_PROFILE:-}" >"$FAKE_ENV/daemon-omp-profile"
    config="${all##* --config }"
    state_dir="$(dirname "$config")"
    "$state_dir/exec-token.sh" >/dev/null
    if [ -n "${FAKE_DAEMON_BOOT_DELAY:-}" ]; then
      (sleep "$FAKE_DAEMON_BOOT_DELAY"; echo 'legion daemon listening on 0.0.0.0:41004'; touch "$FAKE_ENV/daemon-booted") &
    fi
    echo 'kubeconfig exec plugin minted a token'
    exec sleep 300
    ;;
  *envoy-bridge.ts)
    env | cut -d= -f1 | sort >"$FAKE_ENV/envoy-bridge"
    if [ -n "${FAKE_BRIDGE_UNHEALTHY:-}" ]; then echo "BRIDGE UNHEALTHY upstream unreachable"; exit 1; fi
    echo "BRIDGE READY subjects=notifications.github.sjawhar.legion-smoke.> upstream=$SMOKE_UPSTREAM_NATS downstream=$SMOKE_RIG_NATS"; exec sleep 300 ;;
esac
EOF
fake mise <<'EOF'
case "$*" in where*) exit 0 ;; esac
EOF
fake tmux <<'EOF'
all="$*"
case "$all" in
  *"has-session"*) [ -f "$FAKE_TMUX/$2" ] ;;
  *"new-session"*) env | cut -d= -f1 | sort >"$FAKE_ENV/tmux-server"; touch "$FAKE_TMUX/$2" ;;
  *"kill-server"*) rm -f "$FAKE_TMUX/$2" ;;
esac
EOF

# default: this checkout has no legion controller start → controller: none, one root issue released
rm -f "$FAKE_HTTP/issue-counter"
run_up >"$tmp/last.txt" || { echo "full run failed:" >&2; cat "$tmp/last.txt" >&2; exit 1; }
grep -q '^none: the checkout has no legion controller start (pull request #1110)$' "$tmp/state/records/controller"
grep -Fq 'controller:      none (the checkout has no legion controller start (pull request #1110))' "$tmp/last.txt"
refute grep -Eq '^tmux ' "$FAKE_LOG"
[ "$(cat "$tmp/state/records/root-issues")" = ST1-1 ]
post_line="$(grep -n -- '-X POST .*/api/v1/issues$' "$FAKE_LOG" | head -n1 | cut -d: -f1)"
patch_line="$(grep -n -- '-X PATCH .*--data {"status":"todo"} .*/api/v1/issues/ST1-1$' "$FAKE_LOG" | head -n1 | cut -d: -f1)"
[ -n "$post_line" ]
[ -n "$patch_line" ]
[ "$patch_line" -gt "$post_line" ]
grep -Fq 'CREATED root issue ST1-1 (todo)' "$tmp/last.txt"
grep -Fq 'KIND SMOKE READY' "$tmp/last.txt"
for line in 'instance:' 'state dir:' 'cluster:' 'gateway:' 'nats:' 'listener:' 'dispatch:' 'postgres:' 'daemon:' 'image:' 'session store:' 'worker cap:' 'controller:' 'github ingress:' 'root issues:' 'records:'; do
  grep -Fq "$line" "$tmp/last.txt" || { echo "summary lacks $line" >&2; cat "$tmp/last.txt" >&2; exit 1; }
done
grep -Fq 'github ingress:  none (checkpoint done will report SKIPPED-BLOCKED)' "$tmp/last.txt"
grep -Eq 'legion-177:      keeper \(pgid [0-9]+, every 3s; LEGION-177 workaround\)' "$tmp/last.txt"
grep -Fq 'image:           '"$good_image"' (daemon API contract 5)' "$tmp/last.txt"
refute grep -Fq 'anthropic-canary-value' "$FAKE_LOG" "$tmp/last.txt"
refute_secret_leak
# a rerun with another SMOKE_PORT_BASE while the instance's processes are live is refused, nothing re-recorded
: >"$FAKE_LOG"
expect_refusal 'SMOKE_PORT_BASE is 42000 but this instance was started with 41000 and its container legion-smoke-t1-nats container legion-smoke-t1-postgres process listener process dispatch process port-forward process legion-177-keeper are still live; run scripts/kind-smoke/down.sh first (or rerun with SMOKE_PORT_BASE=41000)' SMOKE_PORT_BASE=42000
[ "$(cat "$tmp/state/records/port-base")" = 41000 ]
# a live controller tmux session alone (a partial manual teardown left only the tmux server) is a refusal too:
touch "$FAKE_TMUX/legion-smoket1"
expect_refusal 'SMOKE_PORT_BASE is 42000 but this instance was started with 41000 and its container legion-smoke-t1-nats container legion-smoke-t1-postgres process listener process dispatch process port-forward process legion-177-keeper controller tmux legion-smoket1 are still live' SMOKE_PORT_BASE=42000
rm -f "$FAKE_TMUX/legion-smoket1"
# the LEGION-177 keeper is gated: off records `off`, starts no loop, and says so in the summary
kill -- "-$(cat "$tmp/state/pids/legion-177-keeper.pid")" 2>/dev/null || true
rm -f "$tmp/state/pids/legion-177-keeper.pid" "$tmp/state/pids/legion-177-keeper.start"
run_up SMOKE_LEGION_177_WORKAROUND=0 >"$tmp/last3.txt" || { cat "$tmp/last3.txt" >&2; exit 1; }
[ "$(cat "$tmp/state/records/legion-177-workaround")" = off ]
[ ! -f "$tmp/state/pids/legion-177-keeper.pid" ]
grep -Fq 'SKIPPED LEGION-177 keeper (SMOKE_LEGION_177_WORKAROUND=0)' "$tmp/last3.txt"
grep -Fq 'legion-177:      off (SMOKE_LEGION_177_WORKAROUND=0)' "$tmp/last3.txt"
# rerun reuses the root issue; a second root issue is appended
run_up SMOKE_ROOT_ISSUES=2 >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED root issue ST1-1' "$tmp/last2.txt"
grep -Fq 'CREATED root issue ST1-2 (todo)' "$tmp/last2.txt"
[ "$(paste -sd' ' "$tmp/state/records/root-issues")" = 'ST1-1 ST1-2' ]

# route probe: a checkout with the controller (faked) against a daemon that predates the route → none
touch "$tmp/controller.yaml.example"
run_up FAKE_CONTROLLER_HELP_EXIT=0 SMOKE_CONTROLLER_EXAMPLE="$tmp/controller.yaml.example" FAKE_SECRET_ROUTE_CODE=404 >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
grep -q '^none: the daemon answers 404 on POST /legion/v1/controller/secret (the image predates legion controller start)$' "$tmp/state/records/controller"
grep -Fq 'Authorization: Bearer smoke-route-probe' "$FAKE_LOG"        # the probe bearer is deliberately wrong: nothing is minted
refute grep -Eq '^tmux ' "$FAKE_LOG"
# route present, plugin contract mismatch → none
echo '{"legion":{"daemonApiVersion":6}}' >"$tmp/pkg6.json"
run_up FAKE_CONTROLLER_HELP_EXIT=0 SMOKE_CONTROLLER_EXAMPLE="$tmp/controller.yaml.example" FAKE_SECRET_ROUTE_CODE=403 SMOKE_PLUGIN_MANIFEST="$tmp/pkg6.json" >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
grep -q '^none: installed pi-legion-envoy speaks contract 6; the image daemon requires 5$' "$tmp/state/records/controller"
refute grep -Eq '^tmux ' "$FAKE_LOG"
# route present, contract matches → the pane opens, is recorded, and the daemon sees the controller
echo '{"legion":{"daemonApiVersion":5}}' >"$tmp/pkg5.json"
(umask 077; openssl rand -hex 24 >"$tmp/state/secrets/operator-token")   # what the checkout's operator.env.example would have produced
jq '.controllerLocator = {"runtime":"kubernetes","external":true,"sessionId":"ses_c","registeredAt":"2026-09-15T00:00:00Z"}' "$FAKE_HTTP/state.json" >"$FAKE_HTTP/state-ctl.json"
cp "$FAKE_HTTP/state.json" "$FAKE_HTTP/state-plain.json"; cp "$FAKE_HTTP/state-ctl.json" "$FAKE_HTTP/state.json"
run_up FAKE_CONTROLLER_HELP_EXIT=0 SMOKE_CONTROLLER_EXAMPLE="$tmp/controller.yaml.example" FAKE_SECRET_ROUTE_CODE=403 SMOKE_PLUGIN_MANIFEST="$tmp/pkg5.json" SMOKE_OMP_LAUNCH_PREFIX= >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
grep -Fq 'tmux -L legion-smoket1 new-session -d -s controller -n controller -c ' "$FAKE_LOG"
grep -Fq 'controller start --config '"$tmp"'/state/controller/controller.yaml --daemon-url http://127.0.0.1:41004' "$FAKE_LOG"
[ "$(cat "$tmp/state/records/controller")" = 'tmux legion-smoket1 controller' ]
refute grep -Eq '^GH_(AGENT|REVIEW)_APP_PRIVATE_KEY_B64$' "$FAKE_ENV/tmux-server"   # the App keys never reach the tmux server
grep -Fxq ANTHROPIC_API_KEY "$FAKE_ENV/tmux-server"                                  # SMOKE_OMP_LAUNCH_PREFIX= : up.sh's environment is the controller's key source
grep -Fq 'controller:      tmux -L legion-smoket1 attach (window controller)' "$tmp/last.txt"
grep -Fxq "project: $cluster_project" "$tmp/state/controller/controller.yaml"
grep -Fxq 'operator_token_file: ./operator-token' "$tmp/state/controller/controller.yaml"
grep -Fxq 'daemon_url: http://127.0.0.1:41004' "$tmp/state/controller/controller.yaml"
refute grep -q omp_launch_prefix "$tmp/state/controller/controller.yaml"         # SMOKE_OMP_LAUNCH_PREFIX= omits the key
[ "$(stat -c %a "$tmp/state/controller/operator-token")" = 600 ]
refute grep -Fq "$(cat "$tmp/state/secrets/operator-token")" "$FAKE_LOG" "$tmp/last.txt"
# rerun with the pane alive → REUSED, no second new-session
calls_before="$(wc -l <"$FAKE_LOG")"
run_up FAKE_CONTROLLER_HELP_EXIT=0 SMOKE_CONTROLLER_EXAMPLE="$tmp/controller.yaml.example" FAKE_SECRET_ROUTE_CODE=403 SMOKE_PLUGIN_MANIFEST="$tmp/pkg5.json" SMOKE_OMP_LAUNCH_PREFIX= >"$tmp/last2.txt" || { cat "$tmp/last2.txt" >&2; exit 1; }
grep -Fq 'REUSED controller' "$tmp/last2.txt"
refute grep -Fq 'new-session' <(tail -n +"$((calls_before + 1))" "$FAKE_LOG")
# the default launch prefix lands as a list
rm -f "$FAKE_TMUX/legion-smoket1"
run_up FAKE_CONTROLLER_HELP_EXIT=0 SMOKE_CONTROLLER_EXAMPLE="$tmp/controller.yaml.example" FAKE_SECRET_ROUTE_CODE=403 SMOKE_PLUGIN_MANIFEST="$tmp/pkg5.json" >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
refute grep -Fxq ANTHROPIC_API_KEY "$FAKE_ENV/tmux-server"                          # a launch prefix supplies the keys: the tmux server carries none
refute grep -Eq '^GH_(AGENT|REVIEW)_APP_PRIVATE_KEY_B64$' "$FAKE_ENV/tmux-server"
grep -Fxq 'omp_launch_prefix:' "$tmp/state/controller/controller.yaml"
grep -Fxq '  - secrets' "$tmp/state/controller/controller.yaml"
grep -Fxq '  - --' "$tmp/state/controller/controller.yaml"
cp "$FAKE_HTTP/state-plain.json" "$FAKE_HTTP/state.json"
rm -f "$FAKE_TMUX/legion-smoket1"

# GitHub ingress through the bridge: started before the daemon; an unhealthy bridge stops the run early
run_up SMOKE_GITHUB_INGRESS=envoy >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
[ -f "$tmp/state/pids/envoy-bridge.pid" ]
grep -Fq 'STARTED envoy-bridge' "$tmp/last.txt"
grep -Fxq SMOKE_UPSTREAM_NATS "$FAKE_ENV/envoy-bridge"
refute grep -Eq '^(GH_AGENT_APP_PRIVATE_KEY_B64|GH_REVIEW_APP_PRIVATE_KEY_B64|ANTHROPIC_API_KEY)$' "$FAKE_ENV/envoy-bridge"
bridge_line="$(grep -n 'envoy-bridge.ts' "$FAKE_LOG" | head -n1 | cut -d: -f1)"
apply_line="$(grep -n 'apply -k' "$FAKE_LOG" | tail -n1 | cut -d: -f1)"
[ "$bridge_line" -lt "$apply_line" ]
grep -Fq 'github ingress:  envoy (bridge pid' "$tmp/last.txt"
grep -Fq 'upstream nats://envoy-nats.tailb86685.ts.net:4222' "$tmp/last.txt"
kill "$(cat "$tmp/state/pids/envoy-bridge.pid")" 2>/dev/null || true
rm -f "$tmp/state/pids/envoy-bridge.pid" "$tmp/state/pids/envoy-bridge.start" "$tmp/state/logs/envoy-bridge.log"
calls_before="$(wc -l <"$FAKE_LOG")"
if run_up SMOKE_GITHUB_INGRESS=envoy FAKE_BRIDGE_UNHEALTHY=1 SMOKE_UPSTREAM_NATS=nats://nowhere.example:4222 >"$tmp/last.txt"; then echo "unhealthy bridge should fail" >&2; exit 1; fi
grep -Fq 'the GitHub bridge could not subscribe upstream (nats://nowhere.example:4222); see '"$tmp"'/state/logs/envoy-bridge.log' "$tmp/last.txt"
refute grep -Fq 'apply -k' <(tail -n +"$((calls_before + 1))" "$FAKE_LOG")
echo "up.test.sh: controller, bridge, root issues, summary OK"

# ---- up then down: nothing secret-shaped survives down.sh anywhere under the state directory -------
# down.sh shreds a fixed list; this cross-check judges the result by value instead, so a secret file
# up.sh gains and down.sh's list forgets fails here (the same fakes serve both scripts).
run_up >"$tmp/last.txt" || { cat "$tmp/last.txt" >&2; exit 1; }
# the controller's token copies from the earlier controller cases are still under $tmp/state/controller
[ -s "$tmp/state/controller/operator-token" ]
secret_values | sed '/^$/d' >"$tmp/values-before-down"
[ "$(wc -l <"$tmp/values-before-down")" -ge 5 ]
grep -rlF -f "$tmp/values-before-down" "$tmp/state" >/dev/null      # the secrets are there before down.sh …
status=0
env HOME="$tmp/home" SMOKE_DIR="$tmp/state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 bash "$here/down.sh" >"$tmp/down.txt" 2>&1 || status=$?
[ "$status" = 0 ]
tail -n1 "$tmp/down.txt" | grep -Fxq 'KIND SMOKE DOWN'
refute grep -rlF -f "$tmp/values-before-down" "$tmp/state"          # … and nowhere under the state directory after it
[ ! -e "$tmp/state/kubeconfig" ]
[ -f "$tmp/state/records/instance" ]                                 # records and logs stay
rm -f "$tmp/values-before-down"
echo "up.test.sh: up then down leaves no secret OK"

# ---- host daemon mode ----------------------------------------------------------------------------
state="$tmp/host-state"
: >"$FAKE_LOG"
run_up SMOKE_TEST_STATE="$state" SMOKE_DAEMON_MODE=host SMOKE_STOP_AFTER=daemon >"$tmp/host.txt"
host_values="$tmp/host-values"
{
  cat "$state/secrets/dispatch-token" "$state/secrets/envoy-token"
  printf 'anthropic-canary-value\nservice-account-token\n'
} >"$host_values"
refute grep -Fq -f "$host_values" "$FAKE_LOG"
rm -f "$host_values"
assert_record daemon-mode host
assert_record project smoket1
assert_record controller-tmux-server legion-smoket1
grep -Fq 'omp config set setupVersion 2' "$FAKE_LOG"
daemon_project="$("$REAL_BUN" -e "import { legionProjectToken } from \"$here/../../packages/daemon/src/daemon/config.ts\"; console.log(legionProjectToken('smoke-t1', 'LEGION_ID'))")"
assert_eq "$(<"$state/records/project")" "$daemon_project"
assert_record omp-profile legion-smoke-t1
assert_record controller 'host: daemon-managed'
assert_file "$state/host-daemon/legion.yaml"
assert_grep 'runtime:' "$state/host-daemon/legion.yaml"
assert_grep 'kubeconfig: .*/host-daemon/kubeconfig' "$state/host-daemon/legion.yaml"
assert_grep 'node_selector: *{ *legion.dev/pool: legion *}' "$state/host-daemon/legion.yaml"
assert_grep 'priority_class: legion' "$state/host-daemon/legion.yaml"
assert_grep 'omp_launch_prefix:' "$state/host-daemon/legion.yaml"
assert_grep '  - secrets' "$state/host-daemon/legion.yaml"
assert_grep 'project: smoket1' "$state/host-daemon/legion.yaml"
assert_grep 'legion-smoket1-providers' "$FAKE_HOST_MANIFEST"
assert_grep 'daemon_url: http://[0-9.]*:41004' "$state/host-daemon/legion.yaml"
assert_grep 'operator_token_file: .*/secrets/operator-token' "$state/host-daemon/legion.yaml"
assert_grep 'exec:' "$state/host-daemon/kubeconfig"
assert_grep 'command: .*/exec-token.sh' "$state/host-daemon/kubeconfig"
assert_eq "$(kubectl --kubeconfig "$state/kubeconfig" get node "$(cat "$state/records/legion-node")" -o jsonpath='{.metadata.labels.legion\.dev/pool}')" legion
assert_grep 'legion.dev/pool=legion:NoSchedule' <(kubectl --kubeconfig "$state/kubeconfig" describe node "$(cat "$state/records/legion-node")")
assert_eq "$(kubectl --kubeconfig "$state/kubeconfig" get priorityclass legion -o jsonpath='{.value}')" 1000
assert_eq "$(kubectl --kubeconfig "$state/kubeconfig" -n legion get deploy -o name | wc -l)" 0
jq -e '.role_profiles.implementer == "medium" and .resources.medium.limits.memory == "6Gi"' "$state/records/profiles.json" >/dev/null
assert_pid_live daemon
assert_eq "$(<"$FAKE_ENV/daemon-omp-profile")" legion-smoke-t1
assert_grep 'cli/index.ts start smoket1 --config' "$FAKE_LOG"
# Two host-mode records derive distinct Legion IDs and tmux servers from their smoke instances.
state2="$tmp/host2-state"
run_up SMOKE_TEST_STATE="$state2" SMOKE_INSTANCE=t2 SMOKE_DAEMON_MODE=host SMOKE_STOP_AFTER=daemon >"$tmp/host2.txt"
assert_eq "$(<"$state2/records/project")" smoket2
assert_eq "$(<"$state2/records/controller-tmux-server")" legion-smoket2
curl -fsS "http://127.0.0.1:41004/legion/v1/state" | jq -e '.project' >/dev/null
# Only the host daemon remains live. A changed base must still be refused: it would otherwise
# reuse a daemon bound to the old API and worker-stream ports.
for name in listener dispatch legion-177-keeper; do
  kill -- "-$(<"$state/pids/$name.pid")" 2>/dev/null || kill "$(<"$state/pids/$name.pid")"
  rm -f "$state/pids/$name.pid" "$state/pids/$name.start"
done
rm -f "$FAKE_CONTAINERS/legion-smoke-t1-nats" "$FAKE_CONTAINERS/legion-smoke-t1-postgres"
status=0
run_up SMOKE_TEST_STATE="$state" SMOKE_DAEMON_MODE=host SMOKE_PORT_BASE=42000 || status=$?
assert_eq "$status" 1
assert_grep 'process daemon' "$tmp/out.txt"
# A live host controller alone keeps the old daemon URL and must block a port-base change.
kill "$(<"$state/pids/daemon.pid")"
rm -f "$state/pids/daemon.pid" "$state/pids/daemon.start"
echo legion-smoke-t1-controller >"$state/records/controller-tmux-server"
touch "$FAKE_TMUX/legion-smoke-t1-controller"
status=0
run_up SMOKE_TEST_STATE="$state" SMOKE_DAEMON_MODE=host SMOKE_PORT_BASE=42000 || status=$?
assert_eq "$status" 1
assert_grep 'controller tmux legion-smoke-t1-controller' "$tmp/out.txt"
rm -f "$FAKE_TMUX/legion-smoke-t1-controller"
env SMOKE_DIR="$state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 SMOKE_DAEMON_BOOT_WAIT=3 SMOKE_POLL_INTERVAL=1 FAKE_DAEMON_BOOT_DELAY=1 bash "$here/daemon-ctl.sh" start >"$tmp/daemon-restart.txt"
status=0
SMOKE_EXEC_TOKEN_TTL=90s "$state/host-daemon/exec-token.sh" >"$tmp/exec-token-invalid.txt" 2>&1 || status=$?
assert_eq "$status" 64
assert_grep 'SMOKE_EXEC_TOKEN_TTL must be <minutes>m' "$tmp/exec-token-invalid.txt"
: >"$FAKE_LOG"
SMOKE_EXEC_TOKEN_TTL=2m "$state/host-daemon/exec-token.sh" >"$tmp/exec-token-short.txt"
assert_grep 'create token legion-daemon --duration=10m' "$FAKE_LOG"
daemon_pid="$(<"$state/pids/daemon.pid")"
# A failed host providers-Secret apply leaves enough ownership records for down.sh to remove
# the credentials and config that were prepared before the apply.
failed_host_state="$tmp/host-secret-failure"
status=0
run_up SMOKE_TEST_STATE="$failed_host_state" SMOKE_DAEMON_MODE=host FAKE_HOST_SECRET_APPLY_FAIL=1 || status=$?
assert_eq "$status" 1
assert_file "$failed_host_state/records/host-daemon-state-dir"
assert_file "$failed_host_state/host-daemon/legion.yaml"
env HOME="$tmp/home" SMOKE_DIR="$failed_host_state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 bash "$here/down.sh" >"$tmp/failed-host-down.txt" 2>&1
assert_no_file "$failed_host_state/host-daemon/legion.yaml"
assert_no_file "$failed_host_state/host-daemon/kubeconfig"
assert_no_file "$failed_host_state/host-daemon/secrets/github-app-implement.pem"
env SMOKE_DIR="$state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 bash "$here/daemon-ctl.sh" stop >"$tmp/daemon-stop.txt"
refute pid_live "$daemon_pid"
env SMOKE_DIR="$state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 bash "$here/daemon-ctl.sh" stop >"$tmp/daemon-second-stop.txt"
refute pid_live "$daemon_pid"
rm -f "$FAKE_ENV/daemon-booted"
env SMOKE_DIR="$state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 SMOKE_DAEMON_BOOT_WAIT=3 SMOKE_POLL_INTERVAL=1 FAKE_DAEMON_BOOT_DELAY=1 bash "$here/daemon-ctl.sh" start >"$tmp/daemon-ready-start.txt"
assert_file "$FAKE_ENV/daemon-booted"
assert_pid_live daemon
# daemon-ctl derives the API port from the started instance record, not its ambient default.
nondefault_state="$tmp/nondefault-state"
run_up SMOKE_TEST_STATE="$nondefault_state" SMOKE_INSTANCE=t4 SMOKE_DAEMON_MODE=host SMOKE_PORT_BASE=42000 SMOKE_STOP_AFTER=daemon >"$tmp/nondefault.txt"
env SMOKE_DIR="$nondefault_state" SMOKE_INSTANCE=t4 bash "$here/daemon-ctl.sh" stop >"$tmp/nondefault-stop.txt"
: >"$FAKE_LOG"
env SMOKE_DIR="$nondefault_state" SMOKE_INSTANCE=t4 SMOKE_DAEMON_BOOT_WAIT=3 SMOKE_POLL_INTERVAL=1 FAKE_DAEMON_BOOT_DELAY=1 bash "$here/daemon-ctl.sh" start >"$tmp/nondefault-start.txt"
assert_grep 'curl -fsS --max-time 10 http://127.0.0.1:42004/legion/v1/state' "$FAKE_LOG"
cluster_state="$tmp/cluster-state"
run_up SMOKE_TEST_STATE="$cluster_state" SMOKE_STOP_AFTER=daemon >"$tmp/cluster.txt"
state="$cluster_state"
assert_record daemon-mode cluster
assert_no_file "$state/host-daemon/legion.yaml"

fake ss <<'EOF'
if [ "${FAKE_WORKER_STREAM_BUSY:-}" = 1 ] && [[ "$*" == *"sport = :41005"* ]]; then
  echo "LISTEN 0 4096 172.30.0.1:41005 0.0.0.0:*"
fi
EOF
busy_state="$tmp/busy-state"
status=0
run_up SMOKE_TEST_STATE="$busy_state" SMOKE_DAEMON_MODE=host FAKE_WORKER_STREAM_BUSY=1 >"$tmp/busy.txt" || status=$?
assert_eq "$status" 1
assert_grep 'port 41005 \(worker-stream\) is already in use' "$tmp/busy.txt"
echo "up.test.sh: host daemon mode OK"
echo "up.test.sh: OK"
