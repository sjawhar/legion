#!/usr/bin/env bash
# cluster.sh: the Dispatch load test on a full-size copy of Dispatch, its Postgres, the Envoy
# listener and a three-pod NATS JetStream hub in a Kubernetes namespace, driven from a Legion
# worker pod (INFRA-598). It installs the copy from manifests/, seeds it, runs loadgen steps from a
# runner pod inside it, samples every copy container's cgroup, collects, and removes the copy.
#
#   cluster.sh up --namespace NS --storage-class SC --dispatch-commit SHA --pg-major 16|18
#                 --production-repo OWNER/REPO [--listener-image REF]
#       Refuses, before it creates anything, a commit with no ghcr.io envoy:<commit> image (naming
#       the newest first-parent ancestor that has one), a --pg-major other than the production
#       repository's dispatch/main.go databaseVersion, and an image tag that does not resolve to a
#       digest. Refuses with exit 2 while the lock ConfigMap dispatch-loadtest-lock exists. Then
#       takes the lock, installs the copy (Postgres 16 CPU / 60Gi, Dispatch 16 / 32Gi, listener
#       500m / 512Mi, NATS 3 × 2 / 4Gi, runner 8 / 16Gi), starts the heartbeat and the sampler,
#       and prints each copy container's requests and limits.
#   cluster.sh seed --namespace NS            loadgen seed and seed-finish, then the template
#                                             database dispatch_seed.
#   cluster.sh step --namespace NS NAME FACTOR [--duration 10m] [--reads-only]
#       Reset, then one loadgen run at FACTOR × (4 events/s, 60 requests/s) with the
#       series-sessions count (--reads-only: 0 events), then the crash check, cgroup-merge, tables.
#   cluster.sh series --namespace NS [--duration 10m]   1x 2x 3x 5x 7x 10x, then 10x-reads.
#   cluster.sh soak --namespace NS [--duration 60m]     the step 10x-hour.
#   cluster.sh db-size --namespace NS CPU MEMORY        Postgres resized to the Cloud SQL tier of
#                                                       CPU and MEMORY (Gi), on the same PVC.
#   cluster.sh redeploy --namespace NS --dispatch-commit SHA   Dispatch's image only, then a reset.
#   cluster.sh collect --namespace NS [--upload-issue KEY]
#       The cross-step TABLES.md, and each phase's step directories as zstd tar parts under
#       24 MiB in <run>/upload/; with --upload-issue, each part goes to that Dispatch issue.
#   cluster.sh status --namespace NS          the lock, the run, its daemons and its pods.
#   cluster.sh down --namespace NS [--run ID] deletes everything labelled with the run id and the
#       lock, and waits until nothing of the run is left. A lock of another run is deleted only
#       when --run names that run.
#   cluster.sh all --namespace NS <up's flags> --profile quick|full [--upload-issue KEY] [--keep]
#       up, seed, the series, soak, db-size 4 15Gi, step 1x-db4, collect, down; any exit runs
#       down unless --keep. quick: 2 min steps and a 5 min hour; full: 10 and 60 min.
#
# Every subcommand needs --namespace (no default; a Legion pod's own namespace is builds), and
# every kubectl call names it. Subcommands after up find the run from the lock's `run` and its
# directory ${LOADTEST_CLUSTER_ROOT:-$TMPDIR/dispatch-loadtest-cluster}/<run id>/ (state.json, mode
# 600, holds the digests and the run's random credentials; cgroup.jsonl; steps/; logs/); --run ID
# names the run instead. Needs kubectl, jq, curl, gh, go, setsid, timeout, uv (collect) and
# dispatch (collect --upload-issue).
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SELF=$here/cluster.sh
MANIFESTS=$here/manifests
ENVOY_DIR=$(cd -- "$here/../.." && pwd)
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/../common.sh"
export LC_ALL=C TZ=UTC

CLUSTER_ROOT=${LOADTEST_CLUSTER_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest-cluster}
# The lock's name and data keys are a contract with prove-loadtest (INFRA-690), which refuses to
# run while it exists: holder-pod, holder-uid, issue, run, started, heartbeat.
LOCK=dispatch-loadtest-lock
STALE_SECONDS=900
RUN_LABEL=dispatch-loadtest/run
ROLE_LABEL=dispatch-loadtest/role
LEGION_REPO=sjawhar/legion
ENVOY_IMAGE_REPO=ghcr.io/sjawhar/legion/envoy
PG_IMAGE_PREFIX=mirror.gcr.io/pgvector/pgvector:0.8.1-pg
SOCAT_IMAGE=mirror.gcr.io/alpine/socat:1.8.0.3
RUNNER_IMAGE=mirror.gcr.io/library/debian:bookworm-slim
MANIFEST_TYPES='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json'
# The copy containers the sampler reads and the crash check compares, by role (cgroup.go).
SAMPLE_ROLES=(dispatch pprof postgres nats-0 nats-1 nats-2 listener runner)
# What down deletes and then lists: the kinds of `kubectl get all` that the load-test Role may
# read (it may not list replicationcontrollers, daemonsets, horizontalpodautoscalers or
# cronjobs, so `get all` itself is refused), and the claims, ConfigMaps and Secrets.
COPY_KINDS=pods,services,deployments,statefulsets,replicasets,jobs,persistentvolumeclaims,configmaps,secrets
DELETE_KINDS=deployments,statefulsets,jobs,pods,services,persistentvolumeclaims,configmaps,secrets
SSA=(apply --server-side --force-conflicts --field-manager=dispatch-loadtest -f -)

note() { printf 'cluster.sh: %s\n' "$*" >&2; }

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# kube ARGS: kubectl in the copy's namespace, bounded by timeout (never --request-timeout, which
# drops a pod's in-cluster config: kubernetes/kubernetes#93474). kube_for SECONDS ARGS: the same
# with its own bound.
kube() { timeout 300 kubectl -n "$NS" "$@"; }
kube_for() {
  local seconds=$1
  shift
  timeout "$seconds" kubectl -n "$NS" "$@"
}

# valid KIND VALUE: VALUE has KIND's shape. Every value cluster.sh puts into a manifest, a URL or a
# command passes one of these first.
valid() {
  local re
  case $1 in
    namespace) re='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$' ;;
    storage_class) re='^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$' ;;
    run) re='^lt-[0-9]{8}t[0-9]{6}-[0-9a-f]{4}$' ;;
    commit) re='^[0-9a-f]{40}$' ;;
    repo) re='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' ;;
    image) re='^[a-z0-9.-]+\.[a-z]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$' ;;
    hex) re='^[0-9a-f]{32,128}$' ;;
    cpu) re='^[1-9][0-9]?$' ;;
    memory) re='^[1-9][0-9]{0,2}Gi$' ;;
    tier) re='^db-custom-[1-9][0-9]*-[1-9][0-9]*$' ;;
    pg_setting) re='^[1-9][0-9]*(kB|MB|GB)?$' ;;
    duration) re='^[1-9][0-9]*(s|m|h)$' ;;
    step) re='^[A-Za-z0-9._-]{1,63}$' ;;
    factor) re='^[0-9]+(\.[0-9]+)?$' ;;
    issue) re='^[A-Z][A-Z0-9]*-[0-9]+$' ;;
    pod) re='^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$' ;;
    uid) re='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' ;;
    *) die "valid: no kind $1" ;;
  esac
  [[ $2 =~ $re ]]
}

# duration_seconds D: a Go duration of one unit (90s, 10m, 1h) in seconds.
duration_seconds() {
  local n=${1%?}
  case $1 in
    *s) echo "$n" ;;
    *m) echo $((n * 60)) ;;
    *h) echo $((n * 3600)) ;;
  esac
}

# ---- arguments ----------------------------------------------------------------------------------

declare -A OPT=()
POS=()

# parse "VALUE FLAGS" "BOOLEAN FLAGS" ARGS...: OPT[flag] and POS, then the required --namespace
# into NS. Every subcommand calls it first, so each refuses a missing namespace before anything
# else.
parse() {
  local vflags=" $1 " bflags=" $2 " flag
  shift 2
  OPT=() POS=()
  while [ $# -gt 0 ]; do
    case $1 in
      --*)
        flag=${1#--}
        if [[ $vflags == *" $flag "* ]]; then
          [ $# -ge 2 ] || die "$CMD: --$flag needs a value"
          [ -z "${OPT[$flag]+set}" ] || die "$CMD: --$flag is given twice"
          OPT[$flag]=$2
          shift 2
        elif [[ $bflags == *" $flag "* ]]; then
          OPT[$flag]=1
          shift
        else
          die "$CMD takes no $1 (cluster.sh with no arguments prints the usage)"
        fi
        ;;
      *)
        POS+=("$1")
        shift
        ;;
    esac
  done
  [ -n "${OPT[namespace]:-}" ] || die "$CMD: --namespace is required: name the copy's namespace (there is no default; a Legion pod's own namespace is builds)"
  valid namespace "${OPT[namespace]}" || die "$CMD: --namespace '${OPT[namespace]}' is not a namespace name"
  NS=${OPT[namespace]}
}

# positional N WHAT: refuse anything but N positional arguments.
positional() {
  [ ${#POS[@]} -eq "$1" ] || die "$CMD takes $2 (got ${#POS[@]} arguments: ${POS[*]:-none})"
}

# ---- registry -------------------------------------------------------------------------------------

# registry_digest REGISTRY REPO REF: the digest that a manifest read of REF (a tag or a digest)
# answers, with an anonymous pull token when the registry asks for one (ghcr.io does). Fails when
# the registry does not answer 200 with a digest.
registry_digest() {
  local registry=$1 repo=$2 ref=$3 url headers first realm="" service="" token
  url="https://$registry/v2/$repo/manifests/$ref"
  headers=$(curl -sSI --max-time 30 -H "Accept: $MANIFEST_TYPES" "$url" | tr -d '\r') || return 1
  first=${headers%%$'\n'*}
  if [[ $first =~ ^HTTP/[0-9.]+\ 401 ]]; then
    local auth
    auth=$(grep -i '^www-authenticate:' <<<"$headers" || true)
    [[ $auth =~ realm=\"([^\"]+)\" ]] && realm=${BASH_REMATCH[1]}
    [[ $auth =~ service=\"([^\"]+)\" ]] && service=${BASH_REMATCH[1]}
    [ -n "$realm" ] || return 1
    token=$(curl -fsS --max-time 30 "$realm?service=$service&scope=repository:$repo:pull" | jq -r '.token // .access_token // empty') || return 1
    [ -n "$token" ] || return 1
    headers=$(curl -sSI --max-time 30 -H "Authorization: Bearer $token" -H "Accept: $MANIFEST_TYPES" "$url" | tr -d '\r') || return 1
    first=${headers%%$'\n'*}
  fi
  [[ $first =~ ^HTTP/[0-9.]+\ 200 ]] || return 1
  sed -n 's/^docker-content-digest:[[:space:]]*\(sha256:[0-9a-f]\{64\}\).*$/\1/Ip' <<<"$headers" | grep -m1 .
}

# resolve_image REF: REF (registry/repository:tag or @sha256:digest) as registry/repository@digest,
# read from the registry. Fails, naming REF, when it does not resolve.
resolve_image() {
  local ref=$1
  if ! [[ $ref =~ ^([a-z0-9.-]+\.[a-z]+(:[0-9]+)?)/([a-z0-9._/-]+)(:([A-Za-z0-9._-]{1,128}))?(@(sha256:[0-9a-f]{64}))?$ ]]; then
    note "image '$ref' is not registry/repository:tag or registry/repository@sha256:<digest>"
    return 1
  fi
  local registry=${BASH_REMATCH[1]} repo=${BASH_REMATCH[3]} tag=${BASH_REMATCH[5]} digest=${BASH_REMATCH[7]} got
  if [ -z "$tag$digest" ]; then
    note "image '$ref' names neither a tag nor a digest"
    return 1
  fi
  if ! got=$(registry_digest "$registry" "$repo" "${digest:-$tag}"); then
    note "image '$ref' does not resolve: $registry has no manifest for it"
    return 1
  fi
  if [ -n "$digest" ] && [ "$got" != "$digest" ]; then
    note "image '$ref' resolves to $got, not to the digest it names"
    return 1
  fi
  printf '%s/%s@%s\n' "$registry" "$repo" "$got"
}

# image_for_commit SHA: ghcr.io's envoy image of SHA by digest. With none, it walks SHA's
# first-parent ancestors on sjawhar/legion (gh api) to the newest that has one and dies naming
# both; nothing has been created by then.
image_for_commit() {
  local commit=$1 image c i
  if image=$(resolve_image "$ENVOY_IMAGE_REPO:$commit" 2>/dev/null); then
    printf '%s\n' "$image"
    return 0
  fi
  c=$commit
  for ((i = 0; i < 100; i++)); do
    c=$(gh api "repos/$LEGION_REPO/commits/$c" --jq '.parents[0].sha // empty') ||
      die "no image $ENVOY_IMAGE_REPO:$commit, and gh api could not read commit $c of $LEGION_REPO"
    [[ $c =~ ^[0-9a-f]{40}$ ]] || die "no image $ENVOY_IMAGE_REPO:$commit, and no first-parent ancestor of it on $LEGION_REPO within $i commits has one"
    if resolve_image "$ENVOY_IMAGE_REPO:$c" >/dev/null 2>&1; then
      die "no image $ENVOY_IMAGE_REPO:$commit (images are built for main commits that touch packages/envoy, or by release-envoy-listener.yaml on a branch); the newest first-parent ancestor on $LEGION_REPO main that has one is $c: pass --dispatch-commit $c, or publish an image of $commit"
    fi
  done
  die "no image $ENVOY_IMAGE_REPO:$commit, and none of its 100 newest first-parent ancestors on $LEGION_REPO has one"
}

# production_file PATH: PATH on the production repository's main (--production-repo).
production_file() {
  gh api "repos/$PRODUCTION_REPO/contents/$1?ref=main" -H 'Accept: application/vnd.github.raw' ||
    die "gh api could not read $1 on $PRODUCTION_REPO main"
}

# ---- manifests ------------------------------------------------------------------------------------

# Every placeholder the manifests hold, and the kind (valid) its value must be.
declare -A PLACEHOLDER_KIND=(
  [NAMESPACE]=namespace [RUN_ID]=run [STORAGE_CLASS]=storage_class
  [ENVOY_IMAGE]=image [LISTENER_IMAGE]=image [PG_IMAGE]=image [SOCAT_IMAGE]=image [RUNNER_IMAGE]=image
  [PG_TIER]=tier [PG_CPU]=cpu [PG_MEMORY]=memory
  [PG_MAX_CONNECTIONS]=pg_setting [PG_SHARED_BUFFERS]=pg_setting [PG_EFFECTIVE_CACHE_SIZE]=pg_setting
  [PG_WORK_MEM]=pg_setting [PG_MAINTENANCE_WORK_MEM]=pg_setting [PG_MAX_WAL_SIZE]=pg_setting
  [PG_PASSWORD]=hex [DISPATCH_TOKEN]=hex [LISTENER_TOKEN]=hex [SIGNING_KEY]=hex
)

# render_manifest FILE KEY=VALUE...: FILE with each __KEY__ replaced by its VALUE, which must have
# the key's shape; dies when a placeholder is left. The sed script goes through stdin, so no
# credential is on a command line.
render_manifest() {
  local file=$1 kv key value kind script="" out left
  shift
  for kv in "$@"; do
    key=${kv%%=*} value=${kv#*=}
    kind=${PLACEHOLDER_KIND[$key]:-}
    [ -n "$kind" ] || die "render: no placeholder $key"
    if ! valid "$kind" "$value"; then
      [ "$kind" = hex ] && value='<redacted>'
      die "render: $key value '$value' is not a valid $kind"
    fi
    script+="s|__${key}__|${value}|g"$'\n'
  done
  out=$(sed -f - "$file" <<<"$script")
  left=$(grep -o '__[A-Z][A-Z0-9_]*__' <<<"$out" | sort -u | tr '\n' ' ' || true)
  [ -z "$left" ] || die "render: $file still holds ${left% } once filled"
  printf '%s\n' "$out"
}

# manifest_values: every placeholder's value from state.json, as KEY=VALUE lines.
manifest_values() {
  jq -r '"NAMESPACE=\(.namespace)", "RUN_ID=\(.run)", "STORAGE_CLASS=\(.storage_class)",
    "ENVOY_IMAGE=\(.images.dispatch)", "LISTENER_IMAGE=\(.images.listener)",
    "PG_IMAGE=\(.images.postgres)", "SOCAT_IMAGE=\(.images.socat)", "RUNNER_IMAGE=\(.images.runner)",
    "PG_TIER=\(.postgres.tier)", "PG_CPU=\(.postgres.cpu)", "PG_MEMORY=\(.postgres.memory)",
    "PG_MAX_CONNECTIONS=\(.postgres.settings.max_connections)",
    "PG_SHARED_BUFFERS=\(.postgres.settings.shared_buffers)",
    "PG_EFFECTIVE_CACHE_SIZE=\(.postgres.settings.effective_cache_size)",
    "PG_WORK_MEM=\(.postgres.settings.work_mem)",
    "PG_MAINTENANCE_WORK_MEM=\(.postgres.settings.maintenance_work_mem)",
    "PG_MAX_WAL_SIZE=\(.postgres.settings.max_wal_size)",
    "PG_PASSWORD=\(.secrets.pg_password)", "DISPATCH_TOKEN=\(.secrets.dispatch_token)",
    "LISTENER_TOKEN=\(.secrets.listener_token)", "SIGNING_KEY=\(.secrets.signing_key)"' "$STATE"
}

# apply_manifest NAME [KUBECTL ARGS...]: manifests/NAME rendered from state.json, applied
# server-side, or given to the kubectl arguments named instead (the runner pod is created: the
# load-test Role may not patch a pod, and a server-side apply patches).
apply_manifest() {
  local name=$1 values=()
  shift
  [ $# -gt 0 ] || set -- "${SSA[@]}"
  mapfile -t values < <(manifest_values)
  render_manifest "$MANIFESTS/$name" "${values[@]}" | kube "$@" >/dev/null ||
    die "kubectl $1 of manifests/$name failed"
}

# pg_tier CPU MEMORY_GI: the Cloud SQL tier db-custom-<CPU>-<MB> and its documented default
# settings (https://cloud.google.com/sql/docs/postgres/flags; manifests/postgres.yaml cites
# each), as the JSON object state.json keeps under postgres.
pg_tier() {
  local cpu=$1 gi=$2 mb=$(($2 * 1024)) conns
  ((gi >= 4)) || die "db-size: Cloud SQL's max_connections table starts its custom tiers at 3.75 GB; give at least 4Gi"
  if ((gi >= 120)); then conns=1000
  elif ((gi >= 60)); then conns=800
  elif ((gi >= 30)); then conns=600
  elif ((gi >= 15)); then conns=500
  elif ((mb * 10 >= 75 * 1024)); then conns=400
  elif ((gi >= 6)); then conns=200
  else conns=100
  fi
  jq -n --argjson cpu "$cpu" --argjson mb "$mb" --argjson gi "$gi" --argjson conns "$conns" '{
    tier: "db-custom-\($cpu)-\($mb)", cpu: "\($cpu)", memory: "\($gi)Gi",
    settings: {max_connections: "\($conns)", shared_buffers: "\($mb / 3 | floor)MB",
      effective_cache_size: "\($mb * 4 / 10 | floor)MB", work_mem: "4MB",
      maintenance_work_mem: "64MB", max_wal_size: "1504MB"}}'
}

# ---- run state --------------------------------------------------------------------------------------

# use_run: RUN from --run, else from the lock; then its directory and state.json, which must name
# NS.
use_run() {
  local run=${OPT[run]:-} lock
  if [ -z "$run" ]; then
    lock=$(lock_get)
    [ -n "$lock" ] || die "namespace $NS has no lock ConfigMap $LOCK, so no run is up there (cluster.sh up first, or name a run with --run)"
    run=$(jq -r '.data.run // ""' <<<"$lock")
  fi
  valid run "$run" || die "run id '$run' is not one cluster.sh makes"
  set_run "$run"
  [ -f "$STATE" ] || die "no $STATE: run $run was not started from this pod (LOADTEST_CLUSTER_ROOT $CLUSTER_ROOT)"
  [ "$(jq -r .namespace "$STATE")" = "$NS" ] || die "run $run is in namespace $(jq -r .namespace "$STATE"), not $NS"
}

set_run() {
  RUN=$1
  RUN_DIR=$CLUSTER_ROOT/$RUN
  STATE=$RUN_DIR/state.json
  LOADGEN=$RUN_DIR/loadgen
}

# state FILTER: one value of state.json.
state() { jq -r "$1" "$STATE"; }

# state_set FILTER [jq args...]: rewrite state.json through a filter, keeping it mode 600.
state_set() {
  local filter=$1 tmp
  shift
  tmp=$(umask 077 && mktemp "$RUN_DIR/.state.XXXXXX")
  jq "$@" "$filter" "$STATE" >"$tmp"
  mv -- "$tmp" "$STATE"
}

# loadgen_local ARGS: the run's loadgen on the worker pod, with its root in the run directory.
loadgen_local() { LOADTEST_ROOT=$RUN_DIR "$LOADGEN" "$@"; }

# endpoints: every endpoint flag of loadgen seed and run (guard.go), into ENDPOINTS.
endpoints() {
  ENDPOINTS=(
    -url "http://dispatch.$NS.svc:8766" -token "$(state .secrets.dispatch_token)"
    -listener "http://listener.$NS.svc:9020" -listener-token "$(state .secrets.listener_token)"
    -nats "nats://nats.$NS.svc:4222" -nats-mon "http://nats-0.nats-headless.$NS.svc:8222"
    -pprof "http://dispatch-pprof.$NS.svc:6061" -pg "$(pg_url)" -nats-server-name nats-0
  )
}

pg_url() { printf 'postgres://dispatch:%s@postgres.%s.svc:5432/dispatch?sslmode=disable\n' "$(state .secrets.pg_password)" "$NS"; }

# ---- the lock -------------------------------------------------------------------------------------

# lock_get: the lock ConfigMap as JSON, empty when there is none.
lock_get() {
  kube get configmap "$LOCK" --ignore-not-found -o json || die "kubectl could not read ConfigMap $LOCK in $NS"
}

# heartbeat_age RFC3339: seconds since it, or "unknown".
heartbeat_age() {
  local t
  t=$(date -u -d "$1" +%s 2>/dev/null) || {
    echo unknown
    return
  }
  echo $(($(date -u +%s) - t))
}

# refuse_lock JSON: exit 2, naming the lock's holder and its heartbeat's age; past 15 min it is
# stale, and the command that clears it is named.
refuse_lock() {
  local pod uid issue run started hb age
  IFS=$'\t' read -r pod uid issue run started hb < <(jq -r '.data | [."holder-pod", ."holder-uid", .issue, .run, .started, .heartbeat] | map(. // "?") | @tsv' <<<"$1")
  age=$(heartbeat_age "$hb")
  printf 'cluster.sh: namespace %s is locked: ConfigMap %s names run %s, holder pod %s (uid %s, issue %s), started %s, last heartbeat %s, %s s ago.\n' \
    "$NS" "$LOCK" "$run" "$pod" "$uid" "$issue" "$started" "$hb" "$age" >&2
  if [[ $age =~ ^[0-9]+$ ]] && ((age > STALE_SECONDS)); then
    printf 'cluster.sh: that heartbeat is older than 15 min, so the holder is stale. Clear its copy and the lock with: cluster.sh down --namespace %s --run %s\n' "$NS" "$run" >&2
  else
    printf 'cluster.sh: one copy at a time: wait for that run to end (its holder updates the heartbeat every 60 s)\n' >&2
  fi
  exit 2
}

lock_take() {
  local now existing
  now=$(utc_now)
  existing=$(lock_get)
  [ -z "$existing" ] || refuse_lock "$existing"
  if ! jq -n --arg name "$LOCK" --arg run "$RUN" --arg pod "$HOLDER_POD" --arg uid "$POD_UID" \
    --arg issue "$LEGION_ISSUE" --arg now "$now" '{apiVersion: "v1", kind: "ConfigMap",
      metadata: {name: $name, labels: {"app.kubernetes.io/part-of": "dispatch-loadtest", "dispatch-loadtest/run": $run}},
      data: {"holder-pod": $pod, "holder-uid": $uid, issue: $issue, run: $run, started: $now, heartbeat: $now}}' |
    kube create -f - >/dev/null; then
    existing=$(lock_get)
    [ -z "$existing" ] || refuse_lock "$existing"
    die "kubectl could not create the lock ConfigMap $LOCK in $NS"
  fi
}

# ---- background daemons -------------------------------------------------------------------------------

# start_daemon NAME: `cluster.sh _NAME` in a session of its own, so it outlives this command and
# down stops it with its children by its process group. It writes its pidfile itself.
start_daemon() {
  local name=$1 i
  setsid bash "$SELF" "_$name" --namespace "$NS" --run "$RUN" >>"$RUN_DIR/logs/$name.log" 2>&1 </dev/null &
  for ((i = 0; i < 50; i++)); do
    daemon_alive "$name" && return 0
    sleep 0.1
  done
  die "the $name daemon did not start; see $RUN_DIR/logs/$name.log"
}

# daemon_alive NAME: NAME's pidfile names a live `cluster.sh _NAME` of this run.
daemon_alive() {
  local pid
  pid=$(cat -- "$RUN_DIR/$1.pid" 2>/dev/null) || return 1
  [[ $pid =~ ^[0-9]+$ ]] || return 1
  tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null | grep -qF -- "_$1 --namespace $NS --run $RUN"
}

ensure_daemons() {
  local name
  for name in heartbeat sampler; do
    daemon_alive "$name" || {
      note "the $name daemon is not running; starting it"
      start_daemon "$name"
    }
  done
}

stop_daemon() {
  local name=$1 pid i
  if daemon_alive "$name"; then
    pid=$(<"$RUN_DIR/$name.pid")
    kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
    for ((i = 0; i < 100; i++)); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
  fi
  rm -f -- "$RUN_DIR/$name.pid"
}

# The heartbeat: every 60 s, the lock's heartbeat, by a JSON patch that first tests that the lock
# still names this run; it ends when the lock is gone or another run's.
cmd__heartbeat() {
  parse "namespace run" "" "$@"
  use_run
  printf '%s\n' "$$" >"$RUN_DIR/heartbeat.pid"
  local patch out lock
  while :; do
    sleep 60
    patch=$(jq -nc --arg run "$RUN" --arg now "$(utc_now)" \
      '[{op: "test", path: "/data/run", value: $run}, {op: "replace", path: "/data/heartbeat", value: $now}]')
    if ! out=$(kube patch configmap "$LOCK" --type json -p "$patch" 2>&1); then
      lock=$(kube get configmap "$LOCK" --ignore-not-found -o json 2>/dev/null) || {
        note "heartbeat: $out"
        continue
      }
      if [ -z "$lock" ] || [ "$(jq -r .data.run <<<"$lock")" != "$RUN" ]; then
        note "heartbeat: the lock no longer names run $RUN; stopping"
        exit 0
      fi
      note "heartbeat: $out"
    fi
  done
}

# ---- the cgroup sampler (plan D6) -------------------------------------------------------------------

# The script each copy container runs under one long-lived kubectl exec: every 5 s, its cgroup v2
# counters as "key value" lines, then "end". One exec per container for the whole run, not one per
# sample, keeps the worker's CPU low; the worker stamps each sample with its own clock.
# shellcheck disable=SC2016 # expanded by the container's sh
SAMPLE_SCRIPT='while :; do
  for f in cpu.stat cpu.max memory.current memory.stat; do [ -r "/sys/fs/cgroup/$f" ] || echo "error /sys/fs/cgroup/$f is not readable"; done
  grep -E "^(usage_usec|throttled_usec) " /sys/fs/cgroup/cpu.stat
  echo "cpu_max $(cat /sys/fs/cgroup/cpu.max)"
  echo "memory_current $(cat /sys/fs/cgroup/memory.current)"
  grep "^inactive_file " /sys/fs/cgroup/memory.stat
  echo end
  sleep 5
done'

# role_target ROLE: the pod role label, the container and (NATS) the pod name of a sampled role.
role_target() {
  T_POD=""
  case $1 in
    dispatch) T_ROLE=dispatch T_CONTAINER=envoy-dispatch ;;
    pprof) T_ROLE=dispatch T_CONTAINER=pprof ;;
    postgres) T_ROLE=postgres T_CONTAINER=postgres ;;
    nats-[0-2]) T_ROLE=nats T_CONTAINER=nats T_POD=$1 ;;
    listener) T_ROLE=listener T_CONTAINER=envoy-listener ;;
    runner) T_ROLE=runner T_CONTAINER=runner ;;
    *) die "no sampled role $1" ;;
  esac
}

# find_pod ROLE [NAME]: a running pod of the run with that role label (and that name), not being
# deleted; empty when none.
find_pod() {
  local pods
  pods=$(kube get pods -l "$RUN_LABEL=$RUN,$ROLE_LABEL=$1" -o json) || return 1
  jq -r --arg name "${2:-}" '[.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running"
    and ($name == "" or .metadata.name == $name)) | .metadata.name][0] // empty' <<<"$pods"
}

# sample_at: the worker's clock, RFC 3339 with microseconds, into AT.
sample_at() {
  local now=$EPOCHREALTIME
  printf -v AT '%(%Y-%m-%dT%H:%M:%S)T' "${now%.*}"
  AT+=".${now#*.}Z"
}

# emit_error ROLE POD CONTAINER MESSAGE OUT: a failed read: the error, and no number.
emit_error() {
  local msg=$4
  msg=${msg//\\/\\\\}
  msg=${msg//\"/\\\"}
  msg=${msg//[$'\001'-$'\037']/ }
  sample_at
  printf '{"at":"%s","role":"%s","pod":"%s","container":"%s","usage_usec":null,"throttled_usec":null,"cpu_max":"","memory_current":null,"inactive_file":null,"error":"%s"}\n' \
    "$AT" "$1" "$2" "$3" "$msg" >>"$5"
}

# parse_samples ROLE POD CONTAINER OUT: SAMPLE_SCRIPT's blocks from stdin, one cgroupSample line
# per block; a block missing a number is an error line. Returns when the stream ends or stays
# silent for 30 s.
parse_samples() {
  local role=$1 pod=$2 ctr=$3 out=$4 line usage="" thr="" cmax="" cur="" inact="" err=""
  while IFS= read -r -t 30 line; do
    case $line in
      "usage_usec "*) usage=${line#* } ;;
      "throttled_usec "*) thr=${line#* } ;;
      "cpu_max "*) cmax=${line#* } ;;
      "memory_current "*) cur=${line#* } ;;
      "inactive_file "*) inact=${line#* } ;;
      "error "*) err+="${err:+; }${line#* }" ;;
      end)
        if [ -z "$err" ] && [[ $usage =~ ^[0-9]+$ && $thr =~ ^[0-9]+$ && $cur =~ ^[0-9]+$ && $inact =~ ^[0-9]+$ ]] &&
          [[ $cmax =~ ^(max|[0-9]+)\ [0-9]+$ ]]; then
          sample_at
          printf '{"at":"%s","role":"%s","pod":"%s","container":"%s","usage_usec":%s,"throttled_usec":%s,"cpu_max":"%s","memory_current":%s,"inactive_file":%s,"error":""}\n' \
            "$AT" "$role" "$pod" "$ctr" "$usage" "$thr" "$cmax" "$cur" "$inact" >>"$out"
        else
          emit_error "$role" "$pod" "$ctr" "incomplete read: ${err:-usage_usec=$usage throttled_usec=$thr cpu.max=$cmax memory.current=$cur inactive_file=$inact}" "$out"
        fi
        usage="" thr="" cmax="" cur="" inact="" err=""
        ;;
    esac
  done
}

# sample_role_once ROLE OUT: find the role's pod, stream its samples into OUT until the stream
# ends, then write the error line that says why. Pods change on every reset, so each stream
# starts with a fresh lookup by label.
sample_role_once() {
  local role=$1 out=$2 pod errf kpid
  role_target "$role"
  errf=$(mktemp "$RUN_DIR/tmp/sampler.XXXXXX")
  if ! pod=$(find_pod "$T_ROLE" "$T_POD" 2>"$errf"); then
    emit_error "$role" "" "$T_CONTAINER" "pod lookup failed: $(tr '\n' ' ' <"$errf")" "$out"
  elif [ -z "$pod" ]; then
    emit_error "$role" "" "$T_CONTAINER" "no running pod with labels $RUN_LABEL=$RUN,$ROLE_LABEL=$T_ROLE${T_POD:+ named $T_POD}" "$out"
  else
    parse_samples "$role" "$pod" "$T_CONTAINER" "$out" < <(exec kubectl -n "$NS" exec "$pod" -c "$T_CONTAINER" -- sh -c "$SAMPLE_SCRIPT" 2>"$errf")
    kpid=$!
    kill "$kpid" 2>/dev/null || true
    wait "$kpid" 2>/dev/null || true
    emit_error "$role" "$pod" "$T_CONTAINER" "the kubectl exec stream ended or went silent for 30 s: $(tr '\n' ' ' <"$errf" | tail -c 400)" "$out"
  fi
  rm -f -- "$errf"
}

cmd__sampler() {
  parse "namespace run" "" "$@"
  use_run
  printf '%s\n' "$$" >"$RUN_DIR/sampler.pid"
  local role
  for role in "${SAMPLE_ROLES[@]}"; do
    while :; do
      # In an || context errexit is off inside, so no failed read ends the role's loop.
      sample_role_once "$role" "$RUN_DIR/cgroup.jsonl" || true
      sleep 5
    done &
  done
  wait
}

# ---- the copy -------------------------------------------------------------------------------------

# psql_at DB SQL: one value per line, through psql in the Postgres pod.
psql_at() {
  kube exec postgres-0 -c postgres -- psql -X -U dispatch -d "$1" -v ON_ERROR_STOP=1 -At -c "$2"
}

db_exists() { [ "$(psql_at postgres "select count(*) from pg_database where datname = '$1'")" = 1 ]; }

# nats_box ARGS: the nats CLI in the run's nats-box pod.
nats_box() {
  local pod
  pod=$(find_pod nats-box) || die "kubectl could not list the nats-box pod"
  [ -n "$pod" ] || die "no running nats-box pod in $NS"
  # The server named explicitly, so no nats-box context decides which NATS this purges.
  kube exec "$pod" -c nats-box -- nats --server "nats://nats.$NS.svc:4222" "$@"
}

# scale_to DEPLOYMENT N: scale it; with 0, wait until none of its pods is left, else until its
# rollout is done.
scale_to() {
  local d=$1 n=$2 deadline pods
  kube scale deployment "$d" --replicas "$n" >/dev/null || die "kubectl could not scale deployment $d to $n"
  if ((n == 0)); then
    deadline=$((EPOCHSECONDS + 180))
    while :; do
      pods=$(kube get pods -l "$RUN_LABEL=$RUN,$ROLE_LABEL=$d" -o name) || pods="(kubectl get failed)"
      [ -n "$pods" ] || break
      ((EPOCHSECONDS < deadline)) || die "deployment $d still has pods 180 s after it was scaled to 0: $pods"
      sleep 2
    done
  else
    kube_for 960 rollout status "deployment/$d" --timeout=900s >/dev/null ||
      die "deployment $d was not ready within 15 min (kubectl -n $NS describe deployment $d says why)"
  fi
}

wait_dispatch_healthy() {
  local pod body="" deadline=$((EPOCHSECONDS + 180))
  while :; do
    pod=$(find_pod dispatch || true)
    if [ -n "$pod" ] && body=$(kube exec "$pod" -c envoy-dispatch -- curl -fsS --max-time 5 http://127.0.0.1:8766/healthz 2>/dev/null) &&
      jq -e '.ok and .nats' <<<"$body" >/dev/null 2>&1; then
      return 0
    fi
    ((EPOCHSECONDS < deadline)) || die "Dispatch's /healthz did not report ok and nats within 180 s (last answer: ${body:-none}); kubectl -n $NS logs deployment/dispatch -c envoy-dispatch says why"
    sleep 3
  done
}

# check_stream_replicas: ENVOY_NOTIFICATIONS must run on 3 replicas (plan D4), else no step starts.
check_stream_replicas() {
  local info n
  info=$(nats_box stream info ENVOY_NOTIFICATIONS --json) || die "nats stream info ENVOY_NOTIFICATIONS failed through nats-box"
  n=$(jq -r '.config.num_replicas // "none"' <<<"$info")
  [ "$n" = 3 ] || die "stream ENVOY_NOTIFICATIONS has config.num_replicas $n, not 3, so the step refuses to start (the listener starts first, with ENVOY_NATS_REPLICAS=3, to create it on three)"
}

# kv_replicas: each KV bucket's replicas, as a JSON object, for the reset's record.
kv_replicas() {
  local names b n obj='{}'
  names=$(nats_box kv ls --names) || die "nats kv ls failed through nats-box"
  while read -r b; do
    [ -n "$b" ] || continue
    [[ $b =~ ^[A-Za-z0-9_-]+$ ]] || die "nats kv ls named '$b', which is not a bucket name"
    n=$(nats_box stream info "KV_$b" --json | jq -r '.config.num_replicas // "none"') || n=error
    obj=$(jq -c --arg b "$b" --arg n "$n" '. + {($b): ($n | tonumber? // .)}' <<<"$obj")
  done <<<"$names"
  printf '%s\n' "$obj"
}

nats_purge() {
  local streams names b
  streams=$(nats_box stream ls --names) || die "nats stream ls failed through nats-box"
  if grep -qx ENVOY_NOTIFICATIONS <<<"$streams"; then
    nats_box stream rm ENVOY_NOTIFICATIONS -f >/dev/null || die "nats stream rm ENVOY_NOTIFICATIONS failed"
  fi
  names=$(nats_box kv ls --names) || die "nats kv ls failed through nats-box"
  while read -r b; do
    [ -n "$b" ] || continue
    [[ $b =~ ^[A-Za-z0-9_-]+$ ]] || die "nats kv ls named '$b', which is not a bucket name"
    nats_box kv del "$b" -f >/dev/null || die "nats kv del $b failed"
  done <<<"$names"
}

# start_copy: the listener first, so it creates ENVOY_NOTIFICATIONS on three replicas, then
# Dispatch, until /healthz reports ok and nats; then the replica check.
start_copy() {
  scale_to listener 1
  scale_to dispatch 1
  wait_dispatch_healthy
  check_stream_replicas
}

elapsed_since() { awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }'; }

# reset_copy RECORD: the reset before every step (plan D5), its times and replicas into RECORD.
reset_copy() {
  local record=$1 started t0 kv
  started=$(utc_now) t0=$EPOCHREALTIME
  db_exists dispatch_seed || die "the copy's Postgres has no template database dispatch_seed; cluster.sh seed first"
  scale_to dispatch 0
  scale_to listener 0
  psql_at postgres 'drop database if exists dispatch with (force)' >/dev/null || die "drop database dispatch failed"
  psql_at postgres 'create database dispatch template dispatch_seed' >/dev/null || die "create database dispatch from dispatch_seed failed"
  nats_purge
  start_copy
  kv=$(kv_replicas)
  jq -n --arg started "$started" --arg ended "$(utc_now)" --argjson seconds "$(elapsed_since "$t0")" --argjson kv "$kv" \
    '{started: $started, ended: $ended, seconds: $seconds, template: "dispatch_seed",
      stream: {name: "ENVOY_NOTIFICATIONS", num_replicas: 3}, kv_buckets: $kv}' >"$record"
  note "reset done in $(jq -r .seconds "$record") s: database dispatch from dispatch_seed, NATS purged, listener and Dispatch ready, ENVOY_NOTIFICATIONS on 3 replicas"
}

# ---- the runner -----------------------------------------------------------------------------------

runner_put() { kube cp "$1" "runner:$2" -c runner >/dev/null || die "kubectl cp of $1 into the runner failed"; }

put_loadgen() {
  runner_put "$LOADGEN" /work/loadgen
  kube exec runner -c runner -- chmod 755 /work/loadgen || die "chmod of /work/loadgen in the runner failed"
}

# runner_start NAME CMD...: CMD detached in the runner pod, its output in /work/logs/NAME.log, its
# pid in NAME.pid and its exit status in NAME.rc.
runner_start() {
  local name=$1
  shift
  # shellcheck disable=SC2016 # expanded by the runner's sh
  kube exec runner -c runner -- sh -c 'name=$1; shift; mkdir -p /work/logs; rm -f "/work/logs/$name.rc"
    ( cd /work || exit 1; "$@" >"/work/logs/$name.log" 2>&1 & child=$!; echo "$child" >"/work/logs/$name.pid"; wait "$child"; echo "$?" >"/work/logs/$name.rc" ) </dev/null >/dev/null 2>&1 &' \
    sh "$name" "$@" || die "kubectl exec could not start $name in the runner"
}

# runner_wait NAME SECONDS: NAME's exit status once it has one; past SECONDS it kills NAME and
# prints "timeout". Its log is copied to LOG_TO (default the run's logs/).
runner_wait() {
  local name=$1 deadline=$((EPOCHSECONDS + $2)) rc
  while :; do
    if rc=$(kube exec runner -c runner -- cat "/work/logs/$name.rc" 2>/dev/null) && [[ $rc =~ ^[0-9]+$ ]]; then
      break
    fi
    if ((EPOCHSECONDS >= deadline)); then
      # shellcheck disable=SC2016 # expanded by the runner's sh
      kube exec runner -c runner -- sh -c 'kill "$(cat "/work/logs/$1.pid")"' sh "$name" >/dev/null 2>&1 || true
      rc=timeout
      break
    fi
    sleep 15
  done
  kube exec runner -c runner -- cat "/work/logs/$name.log" >"${LOG_TO:-$RUN_DIR/logs}/$name.log" 2>/dev/null || true
  printf '%s\n' "$rc"
}

# ---- steps ----------------------------------------------------------------------------------------

# The jq program that turns the run's pods into one line per sampled container: role, pod,
# container, restartCount, lastState.terminated.reason.
# shellcheck disable=SC2016 # $m, $r and $role are jq's variables
SNAPSHOT_JQ='.items[] | select(.metadata.deletionTimestamp == null) | .metadata as $m
  | ($m.labels["dispatch-loadtest/role"] // "") as $r | .status.containerStatuses[]?
  | (if $r == "dispatch" and .name == "envoy-dispatch" then "dispatch"
     elif $r == "dispatch" and .name == "pprof" then "pprof"
     elif $r == "postgres" and .name == "postgres" then "postgres"
     elif $r == "nats" and .name == "nats" then $m.name
     elif $r == "listener" and .name == "envoy-listener" then "listener"
     elif $r == "runner" and .name == "runner" then "runner"
     else empty end) as $role
  | [$role, $m.name, .name, (.restartCount | tostring), (.lastState.terminated.reason // "-")] | @tsv'

# container_snapshot FILE: every sampled container's restart record, one line each.
container_snapshot() {
  local pods
  pods=$(kube get pods -l "$RUN_LABEL=$RUN" -o json) || die "kubectl could not list the copy's pods"
  jq -r "$SNAPSHOT_JQ" <<<"$pods" | sort >"$1"
}

# record_failure DIR KIND ROLE DETAIL: one reason in DIR/step-failed.json, in cgroup.go's shape
# ({"reasons": [{kind, role, detail}]}), keeping every reason and key the file holds.
record_failure() {
  local dir=$1 f=$1/step-failed.json tmp
  tmp=$(mktemp "$dir/.step-failed.XXXXXX")
  if [ -f "$f" ]; then
    jq --arg k "$2" --arg r "$3" --arg d "$4" \
      '.reasons = ((.reasons // []) + [{kind: $k} + (if $r == "" then {} else {role: $r} end) + {detail: $d}])' "$f" >"$tmp"
  else
    jq -n --arg k "$2" --arg r "$3" --arg d "$4" '{reasons: [{kind: $k} + (if $r == "" then {} else {role: $r} end) + {detail: $d}]}' >"$tmp"
  fi
  mv -- "$tmp" "$f"
  note "step ${dir##*/} failed: $2${3:+ of $3}: $4"
}

# crash_check BEFORE AFTER DIR: a sampled container that restarted between the two snapshots (its
# restartCount rose, its pod was replaced, or its role has no pod after) is a restart reason.
crash_check() {
  local before=$1 after=$2 dir=$3 role pod ctr n reason now
  local -A apod=() arest=() areason=()
  while IFS=$'\t' read -r role pod ctr n reason; do
    apod[$role]=$pod arest[$role]=$n areason[$role]=$reason
  done <"$after"
  while IFS=$'\t' read -r role pod ctr n reason; do
    if [ -z "${apod[$role]:-}" ]; then
      record_failure "$dir" restart "$role" "container $ctr of pod $pod: the role has no running pod after the step"
    elif [ "${apod[$role]}" != "$pod" ]; then
      record_failure "$dir" restart "$role" "pod $pod was replaced by ${apod[$role]} during the step (container $ctr, lastState.terminated.reason ${areason[$role]})"
    elif now=${arest[$role]} && ((now > n)); then
      record_failure "$dir" restart "$role" "container $ctr of pod $pod restarted during the step: restartCount $n -> ${arest[$role]}, lastState.terminated.reason ${areason[$role]}"
    fi
  done <"$before"
}

cpu_model() {
  kube exec "$1" -c "$2" -- grep -m1 '^model name' /proc/cpuinfo | sed 's/^model name[[:space:]]*:[[:space:]]*//' || echo unknown
}

# save_pg_settings FILE: SHOW ALL of the copy's Postgres.
save_pg_settings() { psql_at postgres 'show all' >"$1" || die "SHOW ALL failed in the copy's Postgres"; }

# write_copy_meta FILE DIR: loadgen run's -copy-meta object (recorded under meta.json copy):
# the images, Postgres's version and the sha256 of its SHOW ALL (DIR/postgres-settings.txt), the
# node CPU model in the Dispatch and Postgres containers, the namespace, the run id and each copy
# pod's resources as the live pods have them.
write_copy_meta() {
  local file=$1 dir=$2 version sha dpod cpu_d cpu_p pods
  version=$(psql_at postgres 'select version()') || die "select version() failed in the copy's Postgres"
  save_pg_settings "$dir/postgres-settings.txt"
  sha=$(sha256sum <"$dir/postgres-settings.txt" | cut -d' ' -f1)
  dpod=$(find_pod dispatch) || die "kubectl could not list the Dispatch pod"
  [ -n "$dpod" ] || die "no running Dispatch pod for the copy meta"
  cpu_d=$(cpu_model "$dpod" envoy-dispatch)
  cpu_p=$(cpu_model postgres-0 postgres)
  pods=$(kube get pods -l "$RUN_LABEL=$RUN" -o json | jq -c '[.items[] | select(.metadata.deletionTimestamp == null)
    | {pod: .metadata.name, node: .spec.nodeName, containers: [.spec.containers[] | {name, image, resources}]}]') ||
    die "kubectl could not list the copy's pods"
  jq --arg version "$version" --arg sha "$sha" --arg cpu_d "$cpu_d" --arg cpu_p "$cpu_p" --argjson pods "$pods" \
    '{namespace, run_id: .run, dispatch_commit, production_repo, pg_major, images,
      postgres: {version: $version, settings_sha256: $sha, settings_file: "postgres-settings.txt", tier: .postgres.tier, cpu: .postgres.cpu, memory: .postgres.memory, settings: .postgres.settings},
      node_cpu_model: {dispatch: $cpu_d, postgres: $cpu_p}, pods: $pods}' "$STATE" >"$file"
}

# save_pod_logs DIR SINCE: the copy's logs since the step's start, into DIR.
save_pod_logs() {
  local dir=$1 since=$2 pod i
  pod=$(find_pod dispatch || true)
  [ -z "$pod" ] || kube logs "$pod" -c envoy-dispatch --since-time="$since" >"$dir/dispatch.log" 2>&1 || true
  pod=$(find_pod listener || true)
  [ -z "$pod" ] || kube logs "$pod" -c envoy-listener --since-time="$since" >"$dir/listener.log" 2>&1 || true
  kube logs postgres-0 -c postgres --since-time="$since" >"$dir/postgres.log" 2>&1 || true
  for i in 0 1 2; do
    kube logs "nats-$i" -c nats --since-time="$since" >"$dir/nats-$i.log" 2>&1 || true
  done
}

# run_step NAME FACTOR DURATION READS_ONLY PHASE: one step (see the header), into steps/NAME.
run_step() {
  local name=$1 factor=$2 duration=$3 reads=$4 phase=$5
  valid step "$name" || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"
  if ! valid factor "$factor" || ! awk -v f="$factor" 'BEGIN { exit !(f > 0) }'; then
    die "factor '$factor' is not a number above 0"
  fi
  valid duration "$duration" || die "--duration '$duration' is not a whole number of s, m or h"
  local dir=$RUN_DIR/steps/$name
  [ ! -e "$dir" ] || die "$dir exists: name another step, or remove it"
  [ -f "$RUN_DIR/seed.json" ] || die "no $RUN_DIR/seed.json: cluster.sh seed first"
  ensure_daemons
  local events requests sessions
  events=$(awk -v f="$factor" 'BEGIN { printf "%g", 4 * f }')
  requests=$(awk -v f="$factor" 'BEGIN { printf "%g", 60 * f }')
  sessions=$(loadgen_local series-sessions "$factor") || die "loadgen series-sessions $factor failed"
  [ "$reads" = true ] && events=0
  mkdir -p -- "$dir" "$RUN_DIR/phases"
  printf '%s\n' "$name" >>"$RUN_DIR/phases/$phase"
  note "step $name: factor $factor, $events events/s, $sessions sessions, $requests requests/s, window $duration"

  reset_copy "$dir/reset.json"
  container_snapshot "$dir/containers-before.tsv"
  write_copy_meta "$dir/copy-meta.json" "$dir"
  put_loadgen
  runner_put "$RUN_DIR/seed.json" /work/seed.json
  kube exec runner -c runner -- mkdir -p /work/meta /work/steps || die "mkdir in the runner failed"
  runner_put "$dir/copy-meta.json" "/work/meta/$name.json"

  local started rc
  started=$(utc_now)
  endpoints
  runner_start "step-$name" /work/loadgen run -allow-foreign -name "$name" -out "/work/steps/$name" \
    -seed /work/seed.json -copy-meta "/work/meta/$name.json" -build-commit "$(state .dispatch_commit)" \
    -events "$events" -sessions "$sessions" -requests "$requests" -duration "$duration" \
    -warmup 30s -grace 20s -rng-seed 1 "${ENDPOINTS[@]}"
  rc=$(LOG_TO=$dir runner_wait "step-$name" $((30 + $(duration_seconds "$duration") + 20 + 900)))
  [ ! -f "$dir/step-$name.log" ] || mv -- "$dir/step-$name.log" "$dir/loadgen.out"
  kube exec runner -c runner -- tar -C "/work/steps/$name" -cf - . | tar -C "$dir" -xf - ||
    note "step $name: copying /work/steps/$name back from the runner failed"

  container_snapshot "$dir/containers-after.tsv"
  crash_check "$dir/containers-before.tsv" "$dir/containers-after.tsv" "$dir"
  save_pod_logs "$dir" "$started"
  [ "$rc" = 0 ] || record_failure "$dir" loadgen_exit "" "loadgen run ended with exit status $rc (see run.log and loadgen.out)"
  loadgen_local cgroup-merge "$dir" "$RUN_DIR/cgroup.jsonl" || note "step $name: loadgen cgroup-merge failed"
  loadgen_local tables "$dir" || note "step $name: loadgen tables failed"
  note "step $name done: $dir"
}

# isolated CMD...: CMD in a subshell with errexit in force, its exit status in ISOLATED_RC. A
# subshell called in an `||` or `if` context runs with errexit off, so this one is not.
isolated() {
  set +e
  (
    set -e
    "$@"
  )
  ISOLATED_RC=$?
  set -e
}

# run_step_logged ARGS: run_step in a subshell, so that a step that dies leaves the run going.
run_step_logged() {
  isolated run_step "$@"
  ((ISOLATED_RC == 0)) || note "step $1 did not complete (exit $ISOLATED_RC); the run goes on"
}

run_series() {
  local duration=$1 s name factor
  for s in "1x 1" "2x 2" "3x 3" "5x 5" "7x 7" "10x 10"; do
    read -r name factor <<<"$s"
    run_step_logged "$name" "$factor" "$duration" false series
  done
  run_step_logged 10x-reads 10 "$duration" true series
}

# resize_postgres CPU GI: the Postgres StatefulSet at the tier of CPU and GI (resources and
# settings), restarted on the same PVC; cpu.max must then read CPU × 100000 per 100000.
resize_postgres() {
  local cpu=$1 gi=$2 tier got want
  tier=$(pg_tier "$cpu" "$gi")
  # shellcheck disable=SC2016 # $tier is jq's variable
  state_set '.postgres = $tier' --argjson tier "$tier"
  # Dispatch and the listener hold Postgres connections; the next step's reset starts both again.
  scale_to dispatch 0
  scale_to listener 0
  apply_manifest postgres.yaml
  kube_for 960 rollout status statefulset/postgres --timeout=900s >/dev/null ||
    die "the Postgres StatefulSet did not roll out within 15 min"
  kube_for 960 wait --for=condition=Ready pod/postgres-0 --timeout=900s >/dev/null ||
    die "postgres-0 was not ready within 15 min"
  want="$((cpu * 100000)) 100000"
  got=$(kube exec postgres-0 -c postgres -- cat /sys/fs/cgroup/cpu.max) || die "reading postgres-0's cpu.max failed"
  [ "$got" = "$want" ] || die "postgres-0's cpu.max reads '$got', not '$want'"
  save_pg_settings "$RUN_DIR/postgres-settings.txt"
  note "Postgres at $(state .postgres.tier): cpu.max $got; SHOW ALL in $RUN_DIR/postgres-settings.txt"
}

# ---- hooks for the phases other tasks add -----------------------------------------------------------

loadgen_has_curves() { loadgen_local curves -h >/dev/null 2>&1; }
loadgen_has_replay() { loadgen_local run -h 2>&1 | grep -q -- '-source'; }

# HOOK (refinement, plan D7): after the grid, `loadgen curves` names each knee, and a knee whose
# bracket spans more than one grid interval gets up to two 10-minute steps at the midpoint factor.
# Another task adds `loadgen curves`; the call to it belongs here.
phase_refinement() {
  if loadgen_has_curves; then
    note "skipped phase refinement: loadgen has curves, but this cluster.sh does not call it yet (hook phase_refinement)"
  else
    note "skipped phase refinement: loadgen has no curves command yet"
  fi
}

# HOOK (replay, plan D8): replay-prepare (INFRA-639's seed restored into dispatch_replay), replay
# r16 before db-size and replay r4 after it, each `loadgen run -source replay`. They come from
# another branch; the calls belong here.
phase_replay() {
  if loadgen_has_replay; then
    note "skipped phase $1: loadgen run has -source, but this cluster.sh does not call the replay yet (hook phase_replay)"
  else
    note "skipped phase $1: loadgen run has no -source replay yet"
  fi
}

# HOOK (curves, plan D9): collect's CURVES.md, KNEES.md and curves.csv from `loadgen curves`.
collect_curves() {
  if loadgen_has_curves; then
    note "skipped curves: loadgen has curves, but this cluster.sh does not call it yet (hook collect_curves)"
  else
    note "skipped curves: loadgen has no curves command yet"
  fi
}

# ---- collect --------------------------------------------------------------------------------------

# zstd_compress: stdin to stdout as zstd (level 19), through the zstandard wheel at a pinned
# version; the worker image carries no zstd command.
zstd_compress() {
  uv run -q --with zstandard==0.23.0 --no-project python3 -c \
    'import sys, zstandard; zstandard.ZstdCompressor(level=19, threads=-1).copy_stream(sys.stdin.buffer, sys.stdout.buffer)'
}

# pack_phase PHASE [ISSUE]: the phase's step directories as one zstd tar, split into parts under
# 24 MiB (upload/cluster-load-test-<date>-raw-<phase>.tar.zst.partNN); with ISSUE, each part
# goes to that Dispatch issue. A phase packed and uploaded since its last step is left alone.
pack_phase() {
  local phase=$1 issue=${2:-} list=$RUN_DIR/phases/$1 up=$RUN_DIR/upload names=() name base part
  [ -f "$list" ] || return 0
  while read -r name; do
    [ -d "$RUN_DIR/steps/$name" ] && names+=("$name")
  done <"$list"
  ((${#names[@]})) || return 0
  mkdir -p -- "$up"
  base=$up/cluster-load-test-$(state '.created[0:10]')-raw-$phase.tar.zst.part
  if [ ! -f "$up/$phase.packed" ] || [ "$list" -nt "$up/$phase.packed" ]; then
    rm -f -- "$base"[0-9][0-9] "$up/$phase.uploaded"
    tar -C "$RUN_DIR/steps" -cf - "${names[@]}" | zstd_compress | split -b 24000000 -d -a 2 - "$base" ||
      die "packing phase $phase failed"
    : >"$up/$phase.packed"
    note "phase $phase packed: $(printf '%s ' "$base"[0-9][0-9])"
  fi
  if [ -n "$issue" ] && [ ! -f "$up/$phase.uploaded" ]; then
    for part in "$base"[0-9][0-9]; do
      dispatch artifact --issue "$issue" --name "${part##*/}" --path "$part" >/dev/null ||
        die "dispatch artifact of ${part##*/} to $issue failed"
    done
    : >"$up/$phase.uploaded"
    note "phase $phase uploaded to $issue"
  fi
}

collect_run() {
  local issue=${1:-} dirs=() d p
  for d in "$RUN_DIR"/steps/*/; do
    [ -f "$d/summary.json" ] && dirs+=("${d%/}")
  done
  if ((${#dirs[@]})); then
    loadgen_local tables -tops=false -out "$RUN_DIR/TABLES.md" "${dirs[@]}" || die "loadgen tables across the steps failed"
    note "wrote $RUN_DIR/TABLES.md over ${#dirs[@]} steps"
  else
    note "no step directory holds summary.json; no TABLES.md"
  fi
  collect_curves
  for p in "$RUN_DIR"/phases/*; do
    [ -f "$p" ] && pack_phase "${p##*/}" "$issue"
  done
  return 0
}

# ---- down -----------------------------------------------------------------------------------------

# down_run: stop the run's daemons, delete everything labelled with it (the lock too, when it
# names this run), and wait until nothing of it is left; then print that listing.
down_run() {
  local name deadline listing
  if [ -d "$RUN_DIR" ]; then
    for name in sampler heartbeat; do stop_daemon "$name"; done
  fi
  kube delete "$DELETE_KINDS" -l "$RUN_LABEL=$RUN" --ignore-not-found --wait=false >/dev/null ||
    die "kubectl delete of run $RUN's objects failed"
  deadline=$((EPOCHSECONDS + 900))
  while :; do
    listing=$(kube get "$COPY_KINDS" -l "$RUN_LABEL=$RUN" -o name) || listing="(kubectl get failed)"
    [ -n "$listing" ] || break
    ((EPOCHSECONDS < deadline)) || die "15 min after the delete, run $RUN still has: $(tr '\n' ' ' <<<"$listing")"
    sleep 5
  done
  printf '$ kubectl -n %s get %s -l %s=%s\n' "$NS" "$COPY_KINDS" "$RUN_LABEL" "$RUN"
  kube get "$COPY_KINDS" -l "$RUN_LABEL=$RUN" 2>&1
}

# ---- subcommands ----------------------------------------------------------------------------------

cmd_up() {
  parse "namespace storage-class dispatch-commit pg-major production-repo listener-image" "" "$@"
  positional 0 "no positional arguments"
  trap up_exit EXIT
  up_run
  trap - EXIT
}

# up_exit: up's trap: a failure after the lock was taken names the command that removes what up
# made.
up_exit() {
  local rc=$?
  if ((rc != 0)) && [ -n "${RUN_TAKEN:-}" ]; then
    note "up failed after it took the lock for run $RUN; cluster.sh down --namespace $NS --run $RUN removes what it made"
  fi
}

# up_run: up from OPT (also all's first phase).
up_run() {
  local sc=${OPT[storage-class]:-} commit=${OPT[dispatch-commit]:-} major=${OPT[pg-major]:-}
  PRODUCTION_REPO=${OPT[production-repo]:-}
  local tool
  for tool in kubectl jq curl gh go setsid timeout sha256sum awk; do
    command -v "$tool" >/dev/null || die "up needs $tool on PATH"
  done
  [ -n "$sc" ] || die "up: --storage-class is required (for GKE: premium-rwo)"
  valid storage_class "$sc" || die "up: --storage-class '$sc' is not a storage class name"
  [ -n "$commit" ] || die "up: --dispatch-commit is required: the 40-hex commit whose envoy image Dispatch runs"
  valid commit "$commit" || die "up: --dispatch-commit '$commit' is not a full 40-hex commit sha"
  case $major in
    16 | 18) ;;
    "") die "up: --pg-major is required: 16 or 18, production's Postgres major" ;;
    *) die "up: --pg-major '$major' is not 16 or 18" ;;
  esac
  [ -n "$PRODUCTION_REPO" ] || die "up: --production-repo is required: the owner/repo whose main pins production's Dispatch"
  valid repo "$PRODUCTION_REPO" || die "up: --production-repo '$PRODUCTION_REPO' is not owner/repo"
  HOLDER_POD=$(hostname)
  valid pod "$HOLDER_POD" || die "up: this pod's name '$HOLDER_POD' is not a pod name"
  valid uid "${POD_UID:-}" || die "up: POD_UID ('${POD_UID:-}') is not a pod uid; the lock names its holder's uid (a Legion pod sets POD_UID)"
  valid issue "${LEGION_ISSUE:-}" || die "up: LEGION_ISSUE ('${LEGION_ISSUE:-}') is not an issue key; the lock names it (a Legion pod sets LEGION_ISSUE)"

  # Production's facts, from the production repository's main.
  local prod_major listener_ref
  prod_major=$(production_file dispatch/main.go | sed -n 's/^[[:space:]]*databaseVersion[[:space:]]*=[[:space:]]*"POSTGRES_\([0-9][0-9]*\)".*$/\1/p' | head -n1)
  [ -n "$prod_major" ] || die "up: $PRODUCTION_REPO main's dispatch/main.go holds no databaseVersion = \"POSTGRES_<n>\""
  [ "$prod_major" = "$major" ] || die "up: --pg-major $major differs from production's Postgres $prod_major ($PRODUCTION_REPO main, dispatch/main.go databaseVersion POSTGRES_$prod_major); the copy runs production's major"
  listener_ref=${OPT[listener-image]:-}
  if [ -z "$listener_ref" ]; then
    listener_ref=$(production_file dispatch/Pulumi.prod.yaml | sed -n 's/^[[:space:]]*[A-Za-z0-9_.-]*:listenerImage:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}.*$/\1/p' | head -n1)
    [ -n "$listener_ref" ] || die "up: $PRODUCTION_REPO main's dispatch/Pulumi.prod.yaml holds no <project>:listenerImage; give --listener-image"
  fi

  # Every image by digest, before anything is created.
  local dispatch_image listener_image pg_image socat_image runner_image nats_images
  dispatch_image=$(image_for_commit "$commit")
  listener_image=$(resolve_image "$listener_ref") || die "up: the listener image does not resolve"
  pg_image=$(resolve_image "$PG_IMAGE_PREFIX$major") || die "up: the Postgres image does not resolve"
  socat_image=$(resolve_image "$SOCAT_IMAGE") || die "up: the pprof sidecar image does not resolve"
  runner_image=$(resolve_image "$RUNNER_IMAGE") || die "up: the runner image does not resolve"
  nats_images=$(nats_manifest_images) || die "up: manifests/nats.yaml names an image that does not resolve"

  set_run "lt-$(date -u +%Y%m%dt%H%M%S)-$(random_hex 2)"
  lock_take
  RUN_TAKEN=1
  note "took the lock $LOCK in $NS for run $RUN"
  (umask 077 && mkdir -p -- "$RUN_DIR/logs" "$RUN_DIR/tmp" "$RUN_DIR/steps")
  local tier
  tier=$(pg_tier 16 60)
  (umask 077 && jq -n --arg ns "$NS" --arg run "$RUN" --arg sc "$sc" --arg commit "$commit" --arg major "$major" \
    --arg repo "$PRODUCTION_REPO" --arg created "$(utc_now)" --arg holder "$HOLDER_POD" \
    --arg dispatch "$dispatch_image" --arg listener "$listener_image" --arg listener_ref "$listener_ref" \
    --arg pg "$pg_image" --arg socat "$socat_image" --arg runner "$runner_image" --argjson nats "$nats_images" \
    --argjson tier "$tier" --arg pgpw "$(random_hex 24)" --arg dt "$(random_hex 32)" --arg lt "$(random_hex 32)" \
    --arg sk "$(random_hex 32)" '{namespace: $ns, run: $run, storage_class: $sc, dispatch_commit: $commit,
      pg_major: $major, production_repo: $repo, created: $created, holder_pod: $holder,
      images: ({dispatch: $dispatch, listener: $listener, postgres: $pg, socat: $socat, runner: $runner} + $nats),
      image_sources: {listener: $listener_ref}, postgres: $tier,
      secrets: {pg_password: $pgpw, dispatch_token: $dt, listener_token: $lt, signing_key: $sk}}' >"$STATE")
  chmod 600 "$STATE"
  start_daemon heartbeat

  note "building loadgen (static) into $LOADGEN"
  (cd -- "$ENVOY_DIR" && CGO_ENABLED=0 go build -o "$LOADGEN" ./loadtest) || die "go build of loadgen failed"

  note "installing the copy in $NS"
  apply_manifest secrets.yaml.tmpl
  apply_manifest postgres.yaml
  apply_manifest nats.yaml
  apply_manifest listener.yaml
  apply_manifest dispatch.yaml
  apply_manifest runner.yaml create -f -
  kube_for 960 rollout status statefulset/postgres --timeout=900s >/dev/null || die "Postgres was not ready within 15 min"
  kube_for 960 rollout status statefulset/nats --timeout=900s >/dev/null || die "NATS was not ready within 15 min"
  kube_for 960 rollout status deployment/nats-box --timeout=900s >/dev/null || die "nats-box was not ready within 15 min"
  kube_for 960 wait --for=condition=Ready pod/runner --timeout=900s >/dev/null || die "the runner was not ready within 15 min"
  start_copy
  put_loadgen
  save_pg_settings "$RUN_DIR/postgres-settings.txt"
  psql_at postgres 'select version()' >"$RUN_DIR/postgres-version.txt" || die "select version() failed"
  start_daemon sampler
  note "the copy is up: run $RUN, $(<"$RUN_DIR/postgres-version.txt")"
  print_resources
}

# nats_manifest_images: the three images manifests/nats.yaml pins, each confirmed at its registry,
# as a JSON object.
nats_manifest_images() {
  local nats reloader box ref
  nats=$(grep -oE 'image: [^ ]+/library/nats@sha256:[0-9a-f]{64}' "$MANIFESTS/nats.yaml" | head -n1)
  reloader=$(grep -oE 'image: [^ ]+/nats-server-config-reloader@sha256:[0-9a-f]{64}' "$MANIFESTS/nats.yaml" | head -n1)
  box=$(grep -oE 'image: [^ ]+/nats-box@sha256:[0-9a-f]{64}' "$MANIFESTS/nats.yaml" | head -n1)
  for ref in "$nats" "$reloader" "$box"; do
    [ -n "$ref" ] || {
      note "manifests/nats.yaml lacks one of its three images by digest (render-nats.sh writes them)"
      return 1
    }
    resolve_image "${ref#image: }" >/dev/null || return 1
  done
  jq -n --arg n "${nats#image: }" --arg r "${reloader#image: }" --arg b "${box#image: }" '{nats: $n, nats_reloader: $r, nats_box: $b}'
}

print_resources() {
  local jp='{range .items[*]}{.metadata.name}{range .spec.containers[*]}{"\t"}{.name}{": requests "}{.resources.requests}{", limits "}{.resources.limits}{end}{"\n"}{end}'
  printf '$ kubectl -n %s get pod -l %s=%s -o jsonpath='"'"'%s'"'"'\n' "$NS" "$RUN_LABEL" "$RUN" "$jp"
  kube get pod -l "$RUN_LABEL=$RUN" -o jsonpath="$jp" || die "kubectl could not read the copy's pods"
}

cmd_seed() {
  parse "namespace run" "" "$@"
  positional 0 "no positional arguments"
  use_run
  seed_run
}

seed_run() {
  [ ! -f "$RUN_DIR/seed.json" ] || die "run $RUN is seeded already ($RUN_DIR/seed.json)"
  ! db_exists dispatch_seed || die "the copy's Postgres already holds the template database dispatch_seed"
  ensure_daemons
  local started t0 rc
  started=$(utc_now) t0=$EPOCHREALTIME
  wait_dispatch_healthy
  put_loadgen
  endpoints
  note "seeding through Dispatch's Service from the runner (the harness's 4 projects, 1,000 issues, 8 comments each)"
  runner_start seed /work/loadgen seed -allow-foreign "${ENDPOINTS[@]}" -out /work/seed.json
  rc=$(runner_wait seed 5400)
  [ "$rc" = 0 ] || die "loadgen seed ended with $rc; see $RUN_DIR/logs/seed.log"
  runner_start seed-finish /work/loadgen seed-finish -pg "$(pg_url)" -timeout 15m
  rc=$(runner_wait seed-finish 2400)
  [ "$rc" = 0 ] || die "loadgen seed-finish ended with $rc; see $RUN_DIR/logs/seed-finish.log"
  kube exec runner -c runner -- cat /work/seed.json >"$RUN_DIR/seed.json.tmp" || die "copying seed.json back failed"
  mv -- "$RUN_DIR/seed.json.tmp" "$RUN_DIR/seed.json"
  local migration counts
  migration=$(psql_at dispatch 'select max(version) from schema_migrations') || die "reading the seed's migration failed"
  counts=$(psql_at dispatch "select json_build_object('projects', (select count(*) from projects),
    'issues', (select count(*) from issues), 'comments', (select count(*) from comments),
    'artifacts', (select count(*) from artifacts), 'events', (select count(*) from events))") || die "counting the seed failed"
  scale_to dispatch 0
  scale_to listener 0
  psql_at postgres 'alter database dispatch rename to dispatch_seed' >/dev/null || die "renaming dispatch to dispatch_seed failed"
  jq -n --argjson migration "$migration" --arg commit "$(state .dispatch_commit)" --arg at "$started" \
    --argjson seconds "$(elapsed_since "$t0")" --argjson counts "$counts" \
    '{migration: $migration, build_commit: $commit, created_at: $at, seed_seconds: $seconds, counts: $counts}' >"$RUN_DIR/seed.meta.json"
  note "seed done: template database dispatch_seed; $(jq -c . "$RUN_DIR/seed.meta.json")"
}

cmd_step() {
  parse "namespace run duration" "reads-only" "$@"
  positional 2 "NAME FACTOR"
  use_run
  local reads=false
  [ -z "${OPT[reads-only]:-}" ] || reads=true
  run_step "${POS[0]}" "${POS[1]}" "${OPT[duration]:-10m}" "$reads" steps
}

cmd_series() {
  parse "namespace run duration" "" "$@"
  positional 0 "no positional arguments"
  use_run
  local d=${OPT[duration]:-10m}
  valid duration "$d" || die "--duration '$d' is not a whole number of s, m or h"
  run_series "$d"
}

cmd_soak() {
  parse "namespace run duration" "" "$@"
  positional 0 "no positional arguments"
  use_run
  run_step 10x-hour 10 "${OPT[duration]:-60m}" false soak
}

cmd_db_size() {
  parse "namespace run" "" "$@"
  positional 2 "CPU MEMORY (for example 4 15Gi)"
  valid cpu "${POS[0]}" || die "db-size: CPU '${POS[0]}' is not a whole number of cores"
  valid memory "${POS[1]}" || die "db-size: MEMORY '${POS[1]}' is not a whole number of Gi (for example 15Gi)"
  use_run
  ensure_daemons
  resize_postgres "${POS[0]}" "${POS[1]%Gi}"
}

cmd_redeploy() {
  parse "namespace run dispatch-commit" "" "$@"
  positional 0 "no positional arguments"
  local commit=${OPT[dispatch-commit]:-} image
  [ -n "$commit" ] || die "redeploy: --dispatch-commit is required"
  valid commit "$commit" || die "redeploy: --dispatch-commit '$commit' is not a full 40-hex commit sha"
  use_run
  image=$(image_for_commit "$commit")
  ensure_daemons
  # shellcheck disable=SC2016 # $c and $i are jq's variables
  state_set '.dispatch_commit = $c | .images.dispatch = $i' --arg c "$commit" --arg i "$image"
  scale_to dispatch 0
  apply_manifest dispatch.yaml
  note "Dispatch now runs $image (commit $commit)"
  if db_exists dispatch_seed; then
    reset_copy "$RUN_DIR/logs/redeploy-$commit-reset.json"
  else
    start_copy
  fi
}

cmd_collect() {
  parse "namespace run upload-issue" "" "$@"
  positional 0 "no positional arguments"
  local issue=${OPT[upload-issue]:-}
  [ -z "$issue" ] || valid issue "$issue" || die "--upload-issue '$issue' is not an issue key"
  use_run
  collect_run "$issue"
}

cmd_status() {
  parse "namespace run" "" "$@"
  positional 0 "no positional arguments"
  local lock run=${OPT[run]:-} name
  lock=$(lock_get)
  if [ -n "$lock" ]; then
    jq -r '.data | "lock: run \(.run), holder pod \(."holder-pod") (uid \(."holder-uid"), issue \(.issue)), started \(.started), heartbeat \(.heartbeat)"' <<<"$lock"
    printf 'heartbeat age: %s s\n' "$(heartbeat_age "$(jq -r .data.heartbeat <<<"$lock")")"
    [ -n "$run" ] || run=$(jq -r .data.run <<<"$lock")
  else
    printf 'lock: none in %s\n' "$NS"
  fi
  [ -n "$run" ] || return 0
  valid run "$run" || die "run id '$run' is not one cluster.sh makes"
  set_run "$run"
  if [ -f "$STATE" ]; then
    jq -r '"run \(.run): commit \(.dispatch_commit), Postgres \(.postgres.tier), created \(.created)"' "$STATE"
    printf 'run directory: %s\n' "$RUN_DIR"
    for name in heartbeat sampler; do
      if daemon_alive "$name"; then printf '%s: running (pid %s)\n' "$name" "$(<"$RUN_DIR/$name.pid")"; else printf '%s: not running\n' "$name"; fi
    done
  else
    printf 'run %s has no directory in %s on this pod\n' "$run" "$CLUSTER_ROOT"
  fi
  kube get pods -l "$RUN_LABEL=$run" -o wide || die "kubectl could not list the run's pods"
}

cmd_down() {
  parse "namespace run" "" "$@"
  positional 0 "no positional arguments"
  local lock lock_run="" run=${OPT[run]:-}
  lock=$(lock_get)
  [ -z "$lock" ] || lock_run=$(jq -r '.data.run // ""' <<<"$lock")
  if [ -z "$run" ]; then
    [ -n "$lock_run" ] || die "down: namespace $NS has no lock ConfigMap $LOCK; name the run to remove with --run"
    run=$lock_run
    valid run "$run" || die "down: the lock names run '$run', which is not one cluster.sh makes"
    if [ "$(jq -r '.data."holder-uid" // ""' <<<"$lock")" != "${POD_UID:-}" ] || [ ! -f "$CLUSTER_ROOT/$run/state.json" ]; then
      printf 'cluster.sh: down: the lock is held by another run:\n' >&2
      jq -r '.data | "cluster.sh:   run \(.run), holder pod \(."holder-pod") (uid \(."holder-uid"), issue \(.issue)), heartbeat \(.heartbeat)"' <<<"$lock" >&2
      die "down: refusing to remove another run's copy and lock; pass --run $run to remove them"
    fi
  fi
  valid run "$run" || die "down: run id '$run' is not one cluster.sh makes"
  set_run "$run"
  if [ -n "$lock_run" ] && [ "$lock_run" != "$run" ]; then
    note "down: the lock names run $lock_run, not $run; it stays, and only run $run's objects go"
  fi
  down_run
}

cmd_all() {
  parse "namespace storage-class dispatch-commit pg-major production-repo listener-image profile upload-issue" "keep" "$@"
  positional 0 "no positional arguments"
  local step_d hour_d issue=${OPT[upload-issue]:-}
  case ${OPT[profile]:-} in
    quick) step_d=2m hour_d=5m ;;
    full) step_d=10m hour_d=60m ;;
    *) die "all: --profile quick or --profile full is required" ;;
  esac
  [ -z "$issue" ] || valid issue "$issue" || die "--upload-issue '$issue' is not an issue key"
  ALL_KEEP=${OPT[keep]:-}
  RUN_TAKEN=""
  trap all_exit EXIT
  up_run
  seed_run
  run_series "$step_d"
  pack_phase series "$issue"
  phase_refinement
  run_step_logged 10x-hour 10 "$hour_d" false soak
  pack_phase soak "$issue"
  phase_replay replay-prepare
  phase_replay "replay r16"
  isolated resize_postgres 4 15
  if ((ISOLATED_RC == 0)); then
    run_step_logged 1x-db4 1 "$step_d" false db4
  else
    note "db-size 4 15Gi failed (exit $ISOLATED_RC); step 1x-db4 is skipped"
  fi
  pack_phase db4 "$issue"
  phase_replay "replay r4"
  collect_run "$issue"
  if [ -z "$ALL_KEEP" ]; then
    down_run
    RUN_TAKEN=""
  fi
}

# all_exit: all's trap: any exit with a run taken runs down, unless --keep.
all_exit() {
  local rc=$?
  trap - EXIT
  if [ -n "${RUN_TAKEN:-}" ] && [ -z "${ALL_KEEP:-}" ]; then
    note "all: exiting with status $rc; tearing run $RUN down (--keep skips this)"
    isolated down_run
    ((ISOLATED_RC == 0)) || note "all: down failed; run: cluster.sh down --namespace $NS --run $RUN"
  elif [ -n "${RUN_TAKEN:-}" ]; then
    note "all: --keep: run $RUN stays up; cluster.sh down --namespace $NS --run $RUN removes it"
  fi
  exit "$rc"
}

main() {
  CMD=${1:-}
  [ $# -eq 0 ] || shift
  case $CMD in
    up) cmd_up "$@" ;;
    seed) cmd_seed "$@" ;;
    step) cmd_step "$@" ;;
    series) cmd_series "$@" ;;
    soak) cmd_soak "$@" ;;
    db-size) cmd_db_size "$@" ;;
    redeploy) cmd_redeploy "$@" ;;
    collect) cmd_collect "$@" ;;
    status) cmd_status "$@" ;;
    down) cmd_down "$@" ;;
    all) cmd_all "$@" ;;
    _heartbeat) cmd__heartbeat "$@" ;;
    _sampler) cmd__sampler "$@" ;;
    *) usage ;;
  esac
}

# render-nats.sh and cluster.test.sh source this file for its functions.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
