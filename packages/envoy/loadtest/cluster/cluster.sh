#!/usr/bin/env bash
# cluster.sh: the Dispatch load test on a full-size copy of Dispatch, its Postgres, the Envoy
# listener and a three-pod NATS JetStream hub in a Kubernetes namespace, driven from a Legion
# worker pod (INFRA-598). It installs the copy from manifests/, seeds it, runs loadgen steps from a
# runner pod inside it, samples every copy container's cgroup, collects, and removes the copy.
#
#   cluster.sh up --namespace NS --storage-class SC --dispatch-commit SHA --pg-major 16|18
#                 --production-repo OWNER/REPO [--listener-image REF] [--stored-order-commit SHA]
#       Refuses, before it creates anything, a commit with no ghcr.io envoy:<commit> image (naming
#       the newest first-parent ancestor that has one), a --pg-major other than the production
#       repository's dispatch/main.go databaseVersion, an image tag that does not resolve to a
#       digest, and a --stored-order-commit that this checkout's git store does not hold or whose
#       tree has no packages/envoy/cmd/dispatch/storedorder.go. Refuses with exit 2 while the lock
#       ConfigMap dispatch-loadtest-lock exists. Then takes the lock, installs the copy (Postgres 16
#       CPU / 60Gi, Dispatch 16 / 32Gi, listener 500m / 512Mi, NATS 3 × 2 / 4Gi, runner 8 / 16Gi),
#       builds loadgen and (with --stored-order-commit) a static envoy-dispatch of that commit for
#       the stored-order check (below), starts the heartbeat and the sampler, and prints each copy
#       container's requests and limits.
#   cluster.sh seed --namespace NS            loadgen seed and seed-finish, then the template
#                                             database dispatch_seed.
#   cluster.sh step --namespace NS NAME FACTOR [--duration 10m] [--reads-only]
#       Reset, then one loadgen run at FACTOR × (4 events/s, 60 requests/s) with the
#       series-sessions count (--reads-only: 0 events), then the crash check, the stored-order
#       check, cgroup-merge, tables.
#       FACTOR may be fractional (8.5). Postgres's write-out counters (pg_stat_bgwriter, and
#       pg_stat_checkpointer from Postgres 17) go into pg-stats-before.json and pg-stats-after.json.
#   cluster.sh series --namespace NS [--duration 10m]   1x 2x 3x 5x 7x 10x, then 10x-reads, then
#       the refinement: loadgen curves over the series writes refine.txt (one factor per knee whose
#       bracket it can narrow, at most two per knee); each factor F runs as step <F>x for
#       --duration, then curves again, until refine.txt is empty.
#   cluster.sh soak --namespace NS [--duration 60m]     the step 10x-hour.
#   cluster.sh db-size --namespace NS CPU MEMORY        Postgres resized to the Cloud SQL tier of
#                                                       CPU and MEMORY (Gi), on the same PVC.
#   cluster.sh redeploy --namespace NS --dispatch-commit SHA   Dispatch's image only, then a reset.
#   cluster.sh replay-prepare --namespace NS [--manifest FILE] [--from-dir DIR]
#       INFRA-639's LINUX replay inputs (default manifest ../replay-2026-10-26.json) into the
#       template database dispatch_replay. Refuses, before it touches the copy, a manifest whose
#       seed is null (INFRA-639's seed is not uploaded yet) or whose seed check failed (check.passed
#       not true, version_size_mismatches or anchors_outside_logged_block not 0). Then downloads
#       (or copies from --from-dir) and verifies the parts with stack.sh replay-get's code
#       (common.sh replay_get_bundle, replay_fetch_parts) under <run>/replay/; streams the dump into
#       the Postgres pod and restores it (pg_restore --no-owner --no-privileges) into a fresh
#       database dispatch; starts the listener, then Dispatch, which migrates it; waits for /healthz
#       (its schema_version), the block-index backfill and the quiet check (Dispatch below 0.05
#       cores over 30 s in the sampler's cgroup reads); stops both, runs vacuum analyze and
#       loadgen replay-checksum, and renames the database to dispatch_replay. Writes
#       <run>/replay-prepared.json, and keeps the manifest as <run>/replay-manifest.json. A
#       template of the same seed dump is reused.
#   cluster.sh replay --namespace NS NAME [--from 0] [--to 39m]
#       The reset from dispatch_replay (refused without it), the bundle into the runner, then
#       loadgen run -allow-foreign -source replay at rate 1 over the sub-window, then the step's
#       after-work and loadgen codepaths beside the outage (11.1-13.8 cores, 13.8 GiB, the
#       residual <run>/replay-residual.md: replay-residual.md's lines, then the seed check's anchors
#       by placement rule and sign-flipped versions from <run>/replay-manifest.json).
#   cluster.sh collect --namespace NS [--upload-issue KEY]
#       Every step in the order the steps ran (meta.json start): the cross-step STORED-ORDER.md
#       and TABLES.md, loadgen curves into <run>/curves over every step but the phase replay's,
#       loadgen codepaths in each step directory (a replay step's beside the outage, as replay
#       runs it); then each phase's step directories, and the results (curves/, TABLES.md,
#       STORED-ORDER.md, each step's CODEPATHS.md and codepaths.json), as zstd tar parts under
#       24 MiB in <run>/upload/; with --upload-issue, each part goes to that Dispatch issue. A
#       failure is reported, the rest still runs, and collect exits 1.
#   cluster.sh status --namespace NS          the lock, the run, its daemons and its pods.
#   cluster.sh down --namespace NS [--run ID] deletes everything labelled with the run id and the
#       lock, and waits until nothing of the run is left. A lock of another run is deleted only
#       when --run names that run.
#   cluster.sh all --namespace NS <up's flags> --profile quick|full [--manifest FILE]
#                  [--upload-issue KEY] [--keep]
#       up, seed, the series, the refinement (full only), soak, replay-prepare, replay r16,
#       db-size 4 15Gi, step 1x-db4, replay r4, collect, down; any exit runs down unless --keep.
#       quick: 2 min steps, a 5 min hour, replays over 0-5m; full: 10 and 60 min, replays over
#       0-39m. A manifest whose seed is null or fails its check skips the replay phases, one
#       line each.
#
# The stored-order check (INFRA-707's oracle): after each step's and each replay's loadgen run,
# while Dispatch still runs, the runner runs `envoy-dispatch stored-order --since <meta.json
# window_start>` of --stored-order-commit, with the copy's DATABASE_URL on its standard input (never
# on a command line), for at most 30 min. Its output goes into <step>/stored-order.txt, its summary
# line into <step>/stored-order.json, and one line names the counts. A non-zero exit is a
# correctness finding, not a step failure. Before the next reset drops the database, the rows of
# each mismatched or refused document (artifacts, doc_checkpoints, artifact_versions, doc_updates)
# go into <step>/stored-order-evidence/<artifact uuid>/ as gzipped CSVs, with version-<n>.md for
# each mismatched version's stored markdown, through psql \copy to stdout in the Postgres pod,
# within 600 s; stored-order.json evidence names them. A failed export is reported, never a step
# failure, and the evidence is never uploaded. collect writes <run>/STORED-ORDER.md. Without
# --stored-order-commit, each step prints one line saying that the check is skipped.
#
# Every subcommand needs --namespace (no default; a Legion pod's own namespace is builds), and
# every kubectl call names it. Subcommands after up find the run from the lock's `run` and its
# directory ${LOADTEST_CLUSTER_ROOT:-$TMPDIR/dispatch-loadtest-cluster}/<run id>/ (state.json, mode
# 600, holds the digests and the run's random credentials; cgroup.jsonl; steps/; logs/); --run ID
# names the run instead. Needs kubectl, jq, curl, gh, go, setsid, timeout, tar, gzip, base64,
# sha256sum, uv (collect), git (up --stored-order-commit), dispatch (collect --upload-issue), and
# DISPATCH_URL and DISPATCH_TOKEN_FILE (replay-prepare's downloads, without --from-dir).
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SELF=$here/cluster.sh
MANIFESTS=$here/manifests
ENVOY_DIR=$(cd -- "$here/../.." && pwd)
REPO_DIR=$(cd -- "$ENVOY_DIR/../.." && pwd)
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
# The LINUX replay: INFRA-639's manifest, the template database replay-prepare makes,
# where the dump goes in the Postgres pod (on its claim), and the outage's Dispatch figures that
# loadgen codepaths sets a replay beside (production profile §5: 11.1-13.8 cores, 13.8 GiB).
REPLAY_MANIFEST=$here/../replay-2026-10-26.json
REPLAY_TEMPLATE=dispatch_replay
REPLAY_POD_DUMP=/var/lib/postgresql/copy/replay-seed.dump
OUTAGE_CORES=11.1,13.8
OUTAGE_MEMORY_GIB=13.8
# The stored-order check's bound, in seconds (its timeout's exit status is 124).
STORED_ORDER_SECONDS=1800
# The stored-order evidence export's bound for all of one step's documents, in seconds, and the
# tables it exports for each document, as table:key column[:order]: every table that `envoy-dispatch
# stored-order` reads of a document (INFRA-707's internal/dispatch/docs/storedorder.go).
STORED_ORDER_EVIDENCE_SECONDS=600
STORED_ORDER_TABLES=(artifacts:id doc_checkpoints:artifact_id artifact_versions:artifact_id:number doc_updates:artifact_id:version)
# What down deletes and then lists: the kinds of `kubectl get all` that the load-test Role may
# read (it may not list replicationcontrollers, daemonsets, horizontalpodautoscalers or
# cronjobs, so `get all` itself is refused), and the claims, ConfigMaps and Secrets.
COPY_KINDS=pods,services,deployments,statefulsets,replicasets,jobs,persistentvolumeclaims,configmaps,secrets
DELETE_KINDS=deployments,statefulsets,jobs,pods,services,persistentvolumeclaims,configmaps,secrets
SSA=(apply --server-side --force-conflicts --field-manager=dispatch-loadtest -f -)
# The nats-box pod that reset_copy looked up for nats_box; empty outside a reset.
NATS_BOX_POD=""

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
    offset) re='^(0|[1-9][0-9]*(s|m|h))$' ;;
    step) re='^[A-Za-z0-9._-]{1,63}$' ;;
    factor) re='^[0-9]+(\.[0-9]+)?$' ;;
    issue) re='^[A-Z][A-Z0-9]*-[0-9]+$' ;;
    pod) re='^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$' ;;
    uid) re='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' ;;
    rfc3339) re='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$' ;;
    *) die "valid: no kind $1" ;;
  esac
  [[ $2 =~ $re ]]
}

# duration_seconds D: a Go duration of one unit (90s, 10m, 1h), or 0, in seconds.
duration_seconds() {
  local n=${1%?}
  case $1 in
    0) echo 0 ;;
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
# load-test Role may not patch a pod, and a server-side apply patches). Admission charges every
# new object to the namespace's ResourceQuota, and two objects admitted at once race on the
# quota's status: the loser is refused with "the object has been modified" and nothing of it is
# created. That refusal alone is retried, up to 5 tries 3 s apart; any other failure stops.
apply_manifest() {
  local name=$1 values=() try err
  shift
  [ $# -gt 0 ] || set -- "${SSA[@]}"
  mapfile -t values < <(manifest_values)
  err=$RUN_DIR/tmp/apply-$name.err
  mkdir -p -- "$RUN_DIR/tmp"
  for try in 1 2 3 4 5; do
    if render_manifest "$MANIFESTS/$name" "${values[@]}" | kube "$@" >/dev/null 2>"$err"; then
      return 0
    fi
    if ! grep -q 'the object has been modified' "$err" || ((try == 5)); then break; fi
    note "kubectl $1 of manifests/$name: the quota was updated under it (try $try of 5); retrying in 3 s"
    sleep 3
  done
  cat -- "$err" >&2
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
    lock=$(kube_lock_get)
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
  STORED_ORDER_BIN=$RUN_DIR/envoy-dispatch
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

# kube_lock_get: the lock ConfigMap as JSON, empty when there is none.
kube_lock_get() {
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
  note "namespace $NS is locked: ConfigMap $LOCK names run $run, holder pod $pod (uid $uid, issue $issue), started $started, last heartbeat $hb, $age s ago."
  if [[ $age =~ ^[0-9]+$ ]] && ((age > STALE_SECONDS)); then
    note "that heartbeat is older than $((STALE_SECONDS / 60)) min, so the holder is stale. Clear its copy and the lock with: cluster.sh down --namespace $NS --run $run"
  else
    note "one copy at a time: wait for that run to end (its holder updates the heartbeat every 60 s)"
  fi
  exit 2
}

# kube_lock_take: create the lock ConfigMap for RUN; refuse_lock when another run holds it.
kube_lock_take() {
  local now existing
  now=$(utc_now)
  existing=$(kube_lock_get)
  [ -z "$existing" ] || refuse_lock "$existing"
  if ! jq -n --arg name "$LOCK" --arg run "$RUN" --arg pod "$HOLDER_POD" --arg uid "$POD_UID" \
    --arg issue "$LEGION_ISSUE" --arg now "$now" --arg run_label "$RUN_LABEL" '{apiVersion: "v1", kind: "ConfigMap",
      metadata: {name: $name, labels: {"app.kubernetes.io/part-of": "dispatch-loadtest", ($run_label): $run}},
      data: {"holder-pod": $pod, "holder-uid": $uid, issue: $issue, run: $run, started: $now, heartbeat: $now}}' |
    kube create -f - >/dev/null; then
    existing=$(kube_lock_get)
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
      lock=$(kube_lock_get 2>/dev/null) || {
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

# ---- the cgroup sampler ---------------------------------------------------------------------------

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
  running_pod "$pods" "$1" "${2:-}"
}

# running_pod PODS ROLE [NAME]: in PODS (kubectl get pods -o json), a running pod with that role
# label (and that name), not being deleted; empty when none.
running_pod() {
  jq -r --arg label "$ROLE_LABEL" --arg role "$2" --arg name "${3:-}" '[.items[] | select(.metadata.deletionTimestamp == null
    and .status.phase == "Running" and .metadata.labels[$label] == $role and ($name == "" or .metadata.name == $name))
    | .metadata.name][0] // empty' <<<"$1"
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

# pg_has_table DB TABLE: DB holds public.TABLE.
pg_has_table() { [ "$(psql_at "$1" "select to_regclass('public.$2') is not null")" = t ]; }

# pg_stats FILE: Postgres's write-out counters as one JSON object (the time read, pg_stat_bgwriter,
# and pg_stat_checkpointer where it exists: Postgres 17 and later), so a disk-bound step shows. A
# failed read is said and leaves no file; the step goes on.
pg_stats() {
  local file=$1 has sql
  if ! has=$(psql_at postgres "select to_regclass('pg_catalog.pg_stat_checkpointer') is not null"); then
    note "reading Postgres's write-out counters failed; no ${file##*/}"
    return 0
  fi
  sql="select json_build_object('at', now(), 'bgwriter', (select row_to_json(b) from pg_stat_bgwriter b)"
  [ "$has" != t ] || sql+=", 'checkpointer', (select row_to_json(c) from pg_stat_checkpointer c)"
  if psql_at postgres "$sql)" >"$file.tmp" && jq -e .bgwriter "$file.tmp" >/dev/null 2>&1; then
    mv -- "$file.tmp" "$file"
  else
    rm -f -- "$file.tmp"
    note "reading Postgres's write-out counters failed; no ${file##*/}"
  fi
}

# nats_box ARGS: the nats CLI in the run's nats-box pod: NATS_BOX_POD when reset_copy has looked it
# up, else the one running now.
nats_box() {
  local pod=${NATS_BOX_POD:-}
  if [ -z "$pod" ]; then
    pod=$(find_pod nats-box) || die "kubectl could not list the nats-box pod"
    [ -n "$pod" ] || die "no running nats-box pod in $NS"
  fi
  # The server named explicitly, so no nats-box context decides which NATS this purges.
  kube exec "$pod" -c nats-box -- nats --server "nats://nats.$NS.svc:4222" "$@"
}

# scale_to DEPLOYMENT N [SECONDS]: scale it; with 0, wait until none of its pods is left, else
# until its rollout is done (at most SECONDS, default 900).
scale_to() {
  local d=$1 n=$2 seconds=${3:-900} deadline pods
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
    kube_for $((seconds + 60)) rollout status "deployment/$d" --timeout="${seconds}s" >/dev/null ||
      die "deployment $d was not ready within $seconds s (kubectl -n $NS describe deployment $d says why)"
  fi
}

# wait_dispatch_healthy [SECONDS]: until Dispatch's /healthz reports ok and nats (at most SECONDS,
# default 180); its answer goes into HEALTHZ.
wait_dispatch_healthy() {
  local seconds=${1:-180} pod body=""
  local deadline=$((EPOCHSECONDS + seconds))
  while :; do
    pod=$(find_pod dispatch || true)
    if [ -n "$pod" ] && body=$(kube exec "$pod" -c envoy-dispatch -- curl -fsS --max-time 5 http://127.0.0.1:8766/healthz 2>/dev/null) &&
      jq -e '.ok and .nats' <<<"$body" >/dev/null 2>&1; then
      HEALTHZ=$body
      return 0
    fi
    ((EPOCHSECONDS < deadline)) || die "Dispatch's /healthz did not report ok and nats within $seconds s (last answer: ${body:-none}); kubectl -n $NS logs deployment/dispatch -c envoy-dispatch says why"
    sleep 3
  done
}

# check_stream_replicas: ENVOY_NOTIFICATIONS must run on 3 replicas, else no step starts.
check_stream_replicas() {
  local info n
  info=$(nats_box stream info ENVOY_NOTIFICATIONS --json) || die "nats stream info ENVOY_NOTIFICATIONS failed through nats-box"
  n=$(jq -r '.config.num_replicas // "none"' <<<"$info")
  [ "$n" = 3 ] || die "stream ENVOY_NOTIFICATIONS has config.num_replicas $n, not 3, so the step refuses to start (the listener starts first, with ENVOY_NATS_REPLICAS=3, to create it on three)"
}

# each_kv_bucket CMD...: CMD BUCKET for each KV bucket, in nats kv ls's order; a name that is not a
# bucket name stops it there.
each_kv_bucket() {
  local names b
  names=$(nats_box kv ls --names) || die "nats kv ls failed through nats-box"
  while read -r b; do
    [ -n "$b" ] || continue
    [[ $b =~ ^[A-Za-z0-9_-]+$ ]] || die "nats kv ls named '$b', which is not a bucket name"
    "$@" "$b"
  done <<<"$names"
}

# kv_replicas: each KV bucket's replicas, as a JSON object, for the reset's record.
kv_replicas() {
  local obj='{}'
  each_kv_bucket kv_replicas_add
  printf '%s\n' "$obj"
}

# kv_replicas_add BUCKET: BUCKET's replicas into kv_replicas' obj.
kv_replicas_add() {
  local n
  n=$(nats_box stream info "KV_$1" --json | jq -r '.config.num_replicas // "none"') || n=error
  obj=$(jq -c --arg b "$1" --arg n "$n" '. + {($b): ($n | tonumber? // .)}' <<<"$obj")
}

nats_purge() {
  local streams
  streams=$(nats_box stream ls --names) || die "nats stream ls failed through nats-box"
  if grep -qx ENVOY_NOTIFICATIONS <<<"$streams"; then
    nats_box stream rm ENVOY_NOTIFICATIONS -f >/dev/null || die "nats stream rm ENVOY_NOTIFICATIONS failed"
  fi
  each_kv_bucket kv_delete
}

kv_delete() { nats_box kv del "$1" -f >/dev/null || die "nats kv del $1 failed"; }

# start_copy: the listener first, so it creates ENVOY_NOTIFICATIONS on three replicas, then
# Dispatch, until /healthz reports ok and nats; then the replica check.
start_copy() {
  scale_to listener 1
  scale_to dispatch 1
  wait_dispatch_healthy
  check_stream_replicas
}

# stop_copy: Dispatch and the listener scaled to 0, each until none of its pods is left.
stop_copy() {
  scale_to dispatch 0
  scale_to listener 0
}

elapsed_since() { awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }'; }

# reset_copy RECORD [TEMPLATE]: the reset before every step from TEMPLATE (default
# dispatch_seed), its times and replicas into RECORD. From the replay's template, the new
# database's loadgen replay-checksum is read before Dispatch starts, into RECORD's checksum.
reset_copy() {
  local record=$1 template=${2:-dispatch_seed} started t0 kv checksum=null
  started=$(utc_now) t0=$EPOCHREALTIME
  if ! db_exists "$template"; then
    [ "$template" = "$REPLAY_TEMPLATE" ] && die "the copy's Postgres has no template database $template; cluster.sh replay-prepare first"
    die "the copy's Postgres has no template database $template; cluster.sh seed first"
  fi
  stop_copy
  psql_at postgres 'drop database if exists dispatch with (force)' >/dev/null || die "drop database dispatch failed"
  psql_at postgres "create database dispatch template $template" >/dev/null || die "create database dispatch from $template failed"
  if [ "$template" = "$REPLAY_TEMPLATE" ]; then
    checksum=$(replay_checksum dispatch) || die "loadgen replay-checksum of database dispatch failed"
  fi
  # The nats-box pod, looked up once for the reset's NATS calls; a failed lookup leaves each call to
  # look it up and say why.
  NATS_BOX_POD=$(find_pod nats-box 2>/dev/null || true)
  nats_purge
  start_copy
  kv=$(kv_replicas)
  NATS_BOX_POD=""
  jq -n --arg started "$started" --arg ended "$(utc_now)" --argjson seconds "$(elapsed_since "$t0")" --argjson kv "$kv" \
    --arg template "$template" --argjson checksum "$checksum" \
    '{started: $started, ended: $ended, seconds: $seconds, template: $template,
      stream: {name: "ENVOY_NOTIFICATIONS", num_replicas: 3}, kv_buckets: $kv}
      + (if $checksum == null then {} else {checksum: $checksum} end)' >"$record"
  note "reset done in $(jq -r .seconds "$record") s: database dispatch from $template, NATS purged, listener and Dispatch ready, ENVOY_NOTIFICATIONS on 3 replicas"
}

# ---- the runner -----------------------------------------------------------------------------------

runner_put() { kube cp "$1" "runner:$2" -c runner >/dev/null || die "kubectl cp of $1 into the runner failed"; }

# runner_has FILE DEST TEST: the runner's DEST passes `test TEST DEST` and has FILE's sha256. /work
# lasts as long as the runner pod, so a file that an earlier command put there need not be copied
# again.
runner_has() {
  local got
  # shellcheck disable=SC2016 # expanded by the runner's sh
  got=$(kube exec runner -c runner -- sh -c 'test "$1" "$2" && sha256sum "$2"' sh "$3" "$2" 2>/dev/null) || return 1
  [ "${got%% *}" = "$(sha256_of "$1")" ]
}

# put_loadgen: the run's loadgen into the runner as /work/loadgen, executable, unless it is there.
put_loadgen() {
  if runner_has "$LOADGEN" /work/loadgen -x; then return 0; fi
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

# runner_wait NAME SECONDS [LOG_DIR]: NAME's exit status once it has one; past SECONDS it kills
# NAME and prints "timeout". Its log is copied to LOG_DIR (default the run's logs/).
runner_wait() {
  local name=$1 deadline=$((EPOCHSECONDS + $2)) log_dir=${3:-$RUN_DIR/logs} rc
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
  kube exec runner -c runner -- cat "/work/logs/$name.log" >"$log_dir/$name.log" 2>/dev/null || true
  printf '%s\n' "$rc"
}

# ---- the stored-order check -----------------------------------------------------------------------

# stored_order_source COMMIT: refuse a --stored-order-commit that this checkout's git store (which
# `jj git fetch` fills) does not hold, or whose tree has no cmd/dispatch/storedorder.go, naming it.
stored_order_source() {
  command -v git >/dev/null || die "up: --stored-order-commit needs git on PATH"
  git -C "$REPO_DIR" cat-file -e "$1^{commit}" 2>/dev/null ||
    die "up: --stored-order-commit $1 is not a commit in this checkout's git store ($REPO_DIR); fetch it (jj git fetch) or name another"
  git -C "$REPO_DIR" cat-file -e "$1:packages/envoy/cmd/dispatch/storedorder.go" 2>/dev/null ||
    die "up: --stored-order-commit $1 has no packages/envoy/cmd/dispatch/storedorder.go, so its envoy-dispatch has no stored-order command (INFRA-707); name a commit that has it"
}

# build_stored_order COMMIT OUT: a static envoy-dispatch of COMMIT's packages/envoy at OUT (an
# absolute path): `git archive` of this checkout's git store unpacked in a scratch directory beside
# OUT, built there outside any workspace, then the scratch directory removed.
build_stored_order() {
  local commit=$1 out=$2 src rc=0
  src=$(mktemp -d "${out%/*}/.stored-order-src.XXXXXX") || return 1
  { git -C "$REPO_DIR" archive "$commit" packages/envoy | tar -C "$src" -xf - &&
    (cd -- "$src/packages/envoy" && CGO_ENABLED=0 GOWORK=off go build -buildvcs=false -o "$out" ./cmd/dispatch); } || rc=$?
  rm -rf -- "$src"
  return "$rc"
}

# put_stored_order: the run's envoy-dispatch into the runner as /work/envoy-dispatch, executable,
# unless it is there (put_loadgen's rule).
put_stored_order() {
  if runner_has "$STORED_ORDER_BIN" /work/envoy-dispatch -x; then return 0; fi
  runner_put "$STORED_ORDER_BIN" /work/envoy-dispatch
  kube exec runner -c runner -- chmod 755 /work/envoy-dispatch || die "chmod of /work/envoy-dispatch in the runner failed"
}

# The runner's script: DATABASE_URL from its standard input, so the password is on no command line
# and in no file, then stored-order since $2, bounded at $1 seconds (timeout's exit status 124).
# shellcheck disable=SC2016 # expanded by the runner's sh
STORED_ORDER_SCRIPT='IFS= read -r DATABASE_URL; export DATABASE_URL; exec timeout "$1" /work/envoy-dispatch stored-order --since "$2"'

# stored_order_summary TXT COMMIT EXIT SINCE: stored-order.json from the command's output TXT: the
# commit it was built from, its exit status (null when it did not run), --since (null when none),
# and the counts of the last summary line (`stored-order: documents=N clean=N mismatched=N
# refused=N failed=N`), each null when there is no such line or the command timed out (exit 124),
# never 0.
stored_order_summary() {
  jq -Rn --arg commit "$2" --argjson exit "${3:-null}" --arg since "$4" '
    ([inputs | capture("^stored-order: documents=(?<documents>[0-9]+) clean=(?<clean>[0-9]+) mismatched=(?<mismatched>[0-9]+) refused=(?<refused>[0-9]+) failed=(?<failed>[0-9]+)$")] | last) as $s
    | {commit: $commit, exit: $exit, since: (if $since == "" then null else $since end)}
      + (if $s == null or $exit == 124 then {documents: null, clean: null, mismatched: null, refused: null, failed: null}
        else $s | map_values(tonumber) end)' <"$1"
}

# stored_order_flagged TXT: each document that stored-order's output TXT names as mismatched or
# refused, once, in the order TXT first names it: a document line with mismatches above 0 or a
# refused=, or a mismatch line (`version number=N ...`, `class update=N ...`, `pending update=N
# ...`). One line each: the artifact id, then the numbers of its mismatched versions. A document
# that failed to read (`<id> error (...)`) is not named.
stored_order_flagged() {
  local line id rest
  local re_line='^stored-order: ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) (.*)$'
  local re_doc='^base=[0-9]+ .* mismatches=([0-9]+)( refused=.*)?$'
  local re_version='^version number=([0-9]+) ' re_update='^(class|pending) update=[0-9]+ '
  local order=()
  local -A versions=()
  while IFS= read -r line; do
    [[ $line =~ $re_line ]] || continue
    id=${BASH_REMATCH[1]} rest=${BASH_REMATCH[2]}
    if [[ $rest =~ $re_doc ]]; then
      [[ ${BASH_REMATCH[1]} != 0 || -n ${BASH_REMATCH[2]} ]] || continue
    elif [[ $rest =~ $re_version ]]; then
      versions[$id]+=" ${BASH_REMATCH[1]}"
    elif ! [[ $rest =~ $re_update ]]; then
      continue
    fi
    [ -n "${versions[$id]+x}" ] || order+=("$id")
    versions[$id]+=""
  done <"$1"
  for id in "${order[@]}"; do printf '%s%s\n' "$id" "${versions[$id]}"; done
}

# csv_rows FILE: the records of the CSV file FILE less its header line. A quoted field keeps its
# line breaks inside the record, and a quote inside one is two quotes, so a record ends at the
# line where the count of quotes so far is even.
csv_rows() { awk '{ q += gsub(/"/, "") } q % 2 == 0 { n++ } END { print (n > 0 ? n - 1 : 0) }' "$1"; }

# stored_order_psql SECONDS OUT SQL...: psql in the Postgres pod on database dispatch, each SQL a
# -c (a psql \copy runs there and writes to its standard output), that output streamed back through
# kubectl exec into OUT, bounded at SECONDS. Nothing is written in the pod. psql's standard error
# goes to OUT.err, removed when psql succeeds.
stored_order_psql() {
  local seconds=$1 out=$2 sql args=()
  shift 2
  for sql in "$@"; do args+=(-c "$sql"); done
  if kube_for "$seconds" exec postgres-0 -c postgres -- psql -X -q -U dispatch -d dispatch -v ON_ERROR_STOP=1 "${args[@]}" \
    </dev/null >"$out" 2>"$out.err"; then
    rm -f -- "$out.err"
  else
    return 1
  fi
}

# stored_order_failure OUT EXIT: why stored_order_psql of OUT failed, on one line: the timeout, or
# psql's first line of standard error. OUT and OUT.err are removed.
stored_order_failure() {
  local why
  if (($2 == 124)); then
    why="timed out (exit 124)"
  else
    why="exit $2: $(head -n1 -- "$1.err" 2>/dev/null)"
  fi
  rm -f -- "$1" "$1.err"
  printf '%s\n' "$why"
}

# stored_order_export ID OUT DEADLINE VERSION...: one document's rows from the copy's database, each
# psql call bounded by what is left of DEADLINE (epoch seconds), into OUT: <table>.csv.gz (a header
# line, then every row) for each table of STORED_ORDER_TABLES, and version-<n>.md, each mismatched
# VERSION's stored markdown, byte for byte, when artifact_versions holds one. Prints one JSON
# object: artifact, tables (each exported table's row count), versions (the version-<n>.md
# written) and, when a part failed, error (each failed part, with why).
stored_order_export() {
  local id=$1 out=$2 deadline=$3 spec table key order file rows left rc n tables='{}' errors=() written=()
  shift 3
  mkdir -p -- "$out" || return 1
  for spec in "${STORED_ORDER_TABLES[@]}"; do
    IFS=: read -r table key order <<<"$spec"
    file=$out/$table.csv
    left=$((deadline - EPOCHSECONDS))
    if ((left <= 0)); then
      errors+=("$table: not exported: the export's $STORED_ORDER_EVIDENCE_SECONDS s ran out")
      continue
    fi
    rc=0
    stored_order_psql "$left" "$file" \
      "\\copy (select * from $table where $key = '$id'${order:+ order by $order}) to stdout with csv header" || rc=$?
    if ((rc != 0)); then
      errors+=("$table: $(stored_order_failure "$file" "$rc")")
    elif rows=$(csv_rows "$file") && gzip -f -- "$file"; then
      tables=$(jq -c --arg t "$table" --argjson n "$rows" '. + {($t): $n}' <<<"$tables")
    else
      errors+=("$table: counting or compressing its rows failed")
      rm -f -- "$file" "$file.gz"
    fi
  done
  for n in "$@"; do
    file=$out/.version-$n.base64
    left=$((deadline - EPOCHSECONDS))
    if ((left <= 0)); then
      errors+=("version $n: not exported: the export's $STORED_ORDER_EVIDENCE_SECONDS s ran out")
      continue
    fi
    rc=0
    # No row when the version holds no markdown; base64, so the markdown's bytes come back as stored.
    stored_order_psql "$left" "$file" "\\pset tuples_only on" "\\pset format unaligned" \
      "select encode(convert_to(markdown, 'UTF8'), 'base64') from artifact_versions where artifact_id = '$id' and number = $n and markdown is not null" || rc=$?
    if ((rc != 0)); then
      errors+=("version $n: $(stored_order_failure "$file" "$rc")")
    elif [ -s "$file" ]; then
      if base64 -d <"$file" >"$out/version-$n.md"; then
        written+=("$n")
      else
        errors+=("version $n: its markdown did not decode from base64")
        rm -f -- "$out/version-$n.md"
      fi
    fi
    rm -f -- "$file"
  done
  jq -nc --arg a "$id" --argjson t "$tables" \
    --argjson e "$(printf '%s\n' "${errors[@]}" | jq -Rsc 'split("\n") | map(select(. != ""))')" \
    '{artifact: $a, tables: $t, versions: ($ARGS.positional | map(tonumber))} + (if $e == [] then {} else {error: $e} end)' \
    --args "${written[@]}"
}

# stored_order_evidence NAME TXT DIR: before the next reset drops the database, the rows of each
# document that stored-order's output TXT names as mismatched or refused (stored_order_flagged),
# exported into DIR/stored-order-evidence/<artifact>/ (stored_order_export), all within
# STORED_ORDER_EVIDENCE_SECONDS. Prints the JSON array of what was exported (stored-order.json's
# evidence) and one line naming the documents and each failure. A failure never fails the step.
# Nothing is uploaded: pack_tar leaves stored-order-evidence out of the tar parts.
stored_order_evidence() {
  local name=$1 txt=$2 out=$3/stored-order-evidence deadline entry why entries=() flagged=() words=() line
  rm -rf -- "$out"
  mapfile -t flagged < <(stored_order_flagged "$txt")
  if ((${#flagged[@]} == 0)); then
    echo '[]'
    return 0
  fi
  deadline=$((EPOCHSECONDS + STORED_ORDER_EVIDENCE_SECONDS))
  for line in "${flagged[@]}"; do
    read -ra words <<<"$line"
    entry=$(stored_order_export "${words[0]}" "$out/${words[0]}" "$deadline" "${words[@]:1}") ||
      entry=$(jq -nc --arg a "${words[0]}" '{artifact: $a, tables: {}, versions: [], error: ["the export did not complete"]}')
    entries+=("$entry")
    why=$(jq -r '(.error // []) | join("; ")' <<<"$entry")
    [ -z "$why" ] || note "step $name: the stored-order evidence of ${words[0]} is incomplete: $why"
  done
  note "step $name: the stored-order evidence of ${#flagged[@]} mismatched or refused documents is in $out (kept on this pod, not uploaded)"
  printf '%s\n' "${entries[@]}" | jq -sc .
}

# stored_order_check NAME DIR: the stored-order check of step NAME (see the header) after its
# loadgen run, while Dispatch still runs: DIR/stored-order.txt (the command's standard output and
# error), DIR/stored-order.json (stored_order_summary, and evidence: stored_order_evidence's
# export of each mismatched or refused document, made here, before the next reset drops the
# database) and one line naming the counts. A non-zero exit is a finding, never the step's failure.
# Without the run's envoy-dispatch (up was given no --stored-order-commit), one line says that the
# check is skipped.
stored_order_check() {
  local name=$1 dir=$2 commit since evidence rc=0 txt=$2/stored-order.txt
  commit=$(state '.stored_order.commit // empty')
  if [ -z "$commit" ]; then
    note "step $name: the stored-order check is skipped: up was given no --stored-order-commit"
    return 0
  fi
  since=$(jq -r '.window_start // empty' "$dir/meta.json" 2>/dev/null || true)
  if valid rfc3339 "$since"; then
    put_stored_order
    printf '%s\n' "$(pg_url)" | kube_for $((STORED_ORDER_SECONDS + 60)) exec -i runner -c runner -- \
      sh -c "$STORED_ORDER_SCRIPT" sh "$STORED_ORDER_SECONDS" "$since" >"$txt" 2>&1 || rc=$?
    ((rc != 124)) || printf 'stored-order: timed out after %s s (exit 124)\n' "$STORED_ORDER_SECONDS" >>"$txt"
  else
    printf 'stored-order: not run: %s/meta.json holds no RFC 3339 window_start\n' "$name" >"$txt"
    since="" rc=null
  fi
  stored_order_summary "$txt" "$commit" "$rc" "$since" >"$dir/stored-order.json"
  note "$(jq -r --arg name "$name" 'def n: if . == null then "null" else tostring end;
    "step \($name): stored-order (envoy-dispatch of \(.commit[0:12])) since \(.since | n): "
    + if .exit == null then "not run: meta.json holds no window_start"
      else "exit \(.exit)\(if .exit == 124 then " (timed out)" else "" end), documents=\(.documents | n) clean=\(.clean | n) mismatched=\(.mismatched | n) refused=\(.refused | n) failed=\(.failed | n)"
        + (if .exit == 0 then "" else "; a correctness finding, not a step failure: \($name)/stored-order.txt" end) end' "$dir/stored-order.json")"
  evidence=$(stored_order_evidence "$name" "$txt" "$dir") || {
    note "step $name: the stored-order evidence export did not complete"
    evidence='[]'
  }
  jq --argjson evidence "$evidence" '. + {evidence: $evidence}' "$dir/stored-order.json" >"$dir/stored-order.json.tmp"
  mv -- "$dir/stored-order.json.tmp" "$dir/stored-order.json"
}

# stored_order_table DIR...: <run>/STORED-ORDER.md: the commit the check's envoy-dispatch was built
# from, then one row per step directory in the order given (the run's) from its stored-order.json,
# "not run" for a step without one. A count that no summary line gave is null, never 0.
stored_order_table() {
  local commit sha d out=$RUN_DIR/STORED-ORDER.md
  commit=$(state '.stored_order.commit // empty')
  sha=$(state '.stored_order.sha256 // empty')
  {
    printf '# Stored order\n\n'
    if [ -n "$commit" ]; then
      printf "INFRA-707's oracle \`envoy-dispatch stored-order\`, built from commit %s (binary sha256 %s), ran on each step after its window, while Dispatch still ran, with \`--since\` the window start. Exit 1 (a mismatch, a refused document or a failed read) is a correctness finding, not a failed step; \`<step>/stored-order.txt\` holds its lines.\n\n" "$commit" "$sha"
    else
      printf "No step ran INFRA-707's oracle \`envoy-dispatch stored-order\`: up was given no \`--stored-order-commit\`.\n\n"
    fi
    printf '| Step | Since | Exit | Documents | Clean | Mismatched | Refused | Failed |\n'
    printf '| --- | --- | --- | --- | --- | --- | --- | --- |\n'
    for d in "$@"; do
      if [ -f "$d/stored-order.json" ]; then
        jq -r --arg step "${d##*/}" 'def n: if . == null then "null" else tostring end;
          "| \($step) | \(.since | n) | \(if .exit == null then "not run" else .exit | tostring end) | \(.documents | n) | \(.clean | n) | \(.mismatched | n) | \(.refused | n) | \(.failed | n) |"' \
          "$d/stored-order.json" || return 1
      else
        printf '| %s | | not run | | | | | |\n' "${d##*/}"
      fi
    done
  } >"$out.tmp" || return 1
  mv -- "$out.tmp" "$out"
}

# ---- steps ----------------------------------------------------------------------------------------

# The jq program that turns the run's pods into one line per sampled container: role, pod,
# container, restartCount, lastState.terminated.reason.
# shellcheck disable=SC2016 # $m, $r, $role and $role_label are jq's variables
SNAPSHOT_JQ='.items[] | select(.metadata.deletionTimestamp == null) | .metadata as $m
  | ($m.labels[$role_label] // "") as $r | .status.containerStatuses[]?
  | (if $r == "dispatch" and .name == "envoy-dispatch" then "dispatch"
     elif $r == "dispatch" and .name == "pprof" then "pprof"
     elif $r == "postgres" and .name == "postgres" then "postgres"
     elif $r == "nats" and .name == "nats" then $m.name
     elif $r == "listener" and .name == "envoy-listener" then "listener"
     elif $r == "runner" and .name == "runner" then "runner"
     else empty end) as $role
  | [$role, $m.name, .name, (.restartCount | tostring), (.lastState.terminated.reason // "-")] | @tsv'

# container_snapshot FILE: every sampled container's restart record, one line each. The pods it
# lists stay in COPY_PODS for write_copy_meta and save_pod_logs.
container_snapshot() {
  COPY_PODS=$(kube get pods -l "$RUN_LABEL=$RUN" -o json) || die "kubectl could not list the copy's pods"
  jq -r --arg role_label "$ROLE_LABEL" "$SNAPSHOT_JQ" <<<"$COPY_PODS" | sort >"$1"
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
# pod's resources as the live pods have them (COPY_PODS, container_snapshot's listing).
write_copy_meta() {
  local file=$1 dir=$2 version sha dpod cpu_d cpu_p pods
  version=$(psql_at postgres 'select version()') || die "select version() failed in the copy's Postgres"
  save_pg_settings "$dir/postgres-settings.txt"
  sha=$(sha256_of "$dir/postgres-settings.txt")
  dpod=$(running_pod "$COPY_PODS" dispatch)
  [ -n "$dpod" ] || die "no running Dispatch pod for the copy meta"
  cpu_d=$(cpu_model "$dpod" envoy-dispatch)
  cpu_p=$(cpu_model postgres-0 postgres)
  pods=$(jq -c '[.items[] | select(.metadata.deletionTimestamp == null)
    | {pod: .metadata.name, node: .spec.nodeName, containers: [.spec.containers[] | {name, image, resources}]}]' <<<"$COPY_PODS")
  jq --arg version "$version" --arg sha "$sha" --arg cpu_d "$cpu_d" --arg cpu_p "$cpu_p" --argjson pods "$pods" \
    '{namespace, run_id: .run, dispatch_commit, production_repo, pg_major, images,
      postgres: {version: $version, settings_sha256: $sha, settings_file: "postgres-settings.txt", tier: .postgres.tier, cpu: .postgres.cpu, memory: .postgres.memory, settings: .postgres.settings},
      node_cpu_model: {dispatch: $cpu_d, postgres: $cpu_p}, pods: $pods}' "$STATE" >"$file"
}

# save_pod_logs DIR SINCE: the copy's logs since the step's start, into DIR, fetched at once; the
# Dispatch and listener pods are COPY_PODS's.
save_pod_logs() {
  local dir=$1 since=$2 pod i pids=()
  pod=$(running_pod "$COPY_PODS" dispatch)
  if [ -n "$pod" ]; then
    kube logs "$pod" -c envoy-dispatch --since-time="$since" >"$dir/dispatch.log" 2>&1 &
    pids+=($!)
  fi
  pod=$(running_pod "$COPY_PODS" listener)
  if [ -n "$pod" ]; then
    kube logs "$pod" -c envoy-listener --since-time="$since" >"$dir/listener.log" 2>&1 &
    pids+=($!)
  fi
  kube logs postgres-0 -c postgres --since-time="$since" >"$dir/postgres.log" 2>&1 &
  pids+=($!)
  for i in 0 1 2; do
    kube logs "nats-$i" -c nats --since-time="$since" >"$dir/nats-$i.log" 2>&1 &
    pids+=($!)
  done
  # These fetches alone: this shell may also be the parent of the heartbeat and sampler daemons.
  wait "${pids[@]}" || true
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
  runner_has "$RUN_DIR/seed.json" /work/seed.json -f || runner_put "$RUN_DIR/seed.json" /work/seed.json
  kube exec runner -c runner -- mkdir -p /work/meta /work/steps || die "mkdir in the runner failed"
  runner_put "$dir/copy-meta.json" "/work/meta/$name.json"

  endpoints
  run_in_runner "$name" "$dir" "step-$name" $((30 + $(duration_seconds "$duration") + 20 + 900)) \
    /work/loadgen run -allow-foreign -name "$name" -out "/work/steps/$name" \
    -seed /work/seed.json -copy-meta "/work/meta/$name.json" -build-commit "$(state .dispatch_commit)" \
    -events "$events" -sessions "$sessions" -requests "$requests" -duration "$duration" \
    -warmup 30s -grace 20s -rng-seed 1 "${ENDPOINTS[@]}"
  note "step $name done: $dir"
}

# run_in_runner NAME DIR JOB SECONDS CMD...: a step's loadgen run CMD as the runner job JOB (at
# most SECONDS), with Postgres's write-out counters before and after it (pg-stats-before.json,
# pg-stats-after.json), then the after-work of every step: /work/steps/NAME copied back into DIR
# (and, once copied, removed from the runner with /work/meta/NAME.json), the crash check, the
# container logs, a non-zero exit recorded, the stored-order check while Dispatch still runs (one
# that dies leaves the step going), cgroup-merge and tables.
run_in_runner() {
  local name=$1 dir=$2 job=$3 seconds=$4 started rc
  shift 4
  pg_stats "$dir/pg-stats-before.json"
  started=$(utc_now)
  runner_start "$job" "$@"
  rc=$(runner_wait "$job" "$seconds" "$dir")
  pg_stats "$dir/pg-stats-after.json"
  [ ! -f "$dir/$job.log" ] || mv -- "$dir/$job.log" "$dir/loadgen.out"
  if kube exec runner -c runner -- tar -C "/work/steps/$name" -cf - . | tar -C "$dir" -xf -; then
    kube exec runner -c runner -- rm -rf "/work/steps/$name" "/work/meta/$name.json" ||
      note "step $name: removing /work/steps/$name from the runner failed"
  else
    note "step $name: copying /work/steps/$name back from the runner failed"
  fi

  container_snapshot "$dir/containers-after.tsv"
  crash_check "$dir/containers-before.tsv" "$dir/containers-after.tsv" "$dir"
  save_pod_logs "$dir" "$started"
  [ "$rc" = 0 ] || record_failure "$dir" loadgen_exit "" "loadgen run ended with exit status $rc (see run.log and loadgen.out)"
  go_on "step $name: the stored-order check" stored_order_check "$name" "$dir"
  loadgen_local cgroup-merge "$dir" "$RUN_DIR/cgroup.jsonl" || note "step $name: loadgen cgroup-merge failed"
  loadgen_local tables "$dir" || note "step $name: loadgen tables failed"
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

# go_on WHAT CMD...: CMD isolated, so that a CMD that dies leaves the run going, and a note naming
# WHAT when it does.
go_on() {
  local what=$1
  shift
  isolated "$@"
  ((ISOLATED_RC == 0)) || note "$what did not complete (exit $ISOLATED_RC); the run goes on"
}

# run_step_logged ARGS: run_step, so that a step that dies leaves the run going.
run_step_logged() { go_on "step $1" run_step "$@"; }

run_series() {
  local duration=$1 f
  for f in 1 2 3 5 7 10; do
    run_step_logged "${f}x" "$f" "$duration" false series
  done
  run_step_logged 10x-reads 10 "$duration" true series
}

# phase_dirs PHASE...: the step directories that the phases' lists (phases/<phase>) name and that
# exist, into DIRS.
phase_dirs() {
  local p name
  DIRS=()
  for p in "$@"; do
    [ -f "$RUN_DIR/phases/$p" ] || continue
    while read -r name; do
      if [ -d "$RUN_DIR/steps/$name" ]; then DIRS+=("$RUN_DIR/steps/$name"); fi
    done <"$RUN_DIR/phases/$p"
  done
}

# steps_in_run_order: every step directory of the run, in the order the steps ran, into DIRS: by
# meta.json's start, else by reset.json's started (a step whose loadgen run wrote no meta.json);
# a directory with neither goes last.
steps_in_run_order() {
  local d key
  DIRS=()
  while IFS=$'\t' read -r key d; do
    DIRS+=("$d")
  done < <(
    for d in "$RUN_DIR"/steps/*/; do
      [ -d "$d" ] || continue
      d=${d%/}
      key=$(jq -r '.start // empty' "$d/meta.json" 2>/dev/null || true)
      [ -n "$key" ] || key=$(jq -r '.started // empty' "$d/reset.json" 2>/dev/null || true)
      printf '%s\t%s\n' "${key:-~}" "$d"
    done | sort
  )
}

# refine_run DURATION: the refinement, after the grid and the control. loadgen curves over
# the steps of the phases series and refinement writes KNEES.md and refine.txt into <run>/curves:
# one factor per knee whose bracket it can still narrow, at most two refinement steps per knee.
# Each factor F runs as step <F>x for DURATION, then curves again, until refine.txt is empty.
refine_run() {
  local duration=$1 f factors=()
  while :; do
    phase_dirs series refinement
    ((${#DIRS[@]})) || die "refinement: no step of the series has a directory, so there is nothing to refine"
    loadgen_local curves -out "$RUN_DIR/curves" "${DIRS[@]}" || die "refinement: loadgen curves failed"
    [ -f "$RUN_DIR/curves/refine.txt" ] || die "refinement: loadgen curves wrote no $RUN_DIR/curves/refine.txt"
    mapfile -t factors < <(grep -v '^[[:space:]]*$' "$RUN_DIR/curves/refine.txt")
    ((${#factors[@]})) || break
    note "refinement: refine.txt names the factors ${factors[*]}"
    for f in "${factors[@]}"; do
      valid factor "$f" || die "refinement: refine.txt line '$f' is not a factor"
      # A factor that comes back, or a step that leaves no directory, would loop for ever.
      [ ! -e "$RUN_DIR/steps/${f}x" ] || die "refinement: refine.txt names factor $f again, but step ${f}x has run; stopping"
      run_step_logged "${f}x" "$f" "$duration" false refinement
      [ -d "$RUN_DIR/steps/${f}x" ] || die "refinement: step ${f}x did not start; stopping"
    done
  done
  phase_dirs refinement
  note "refinement done: refine.txt is empty; refinement steps run: ${#DIRS[@]}; $RUN_DIR/curves/KNEES.md names the knees"
}

# resize_postgres CPU GI: the Postgres StatefulSet at the tier of CPU and GI (resources and
# settings), restarted on the same PVC; cpu.max must then read CPU × 100000 per 100000.
resize_postgres() {
  local cpu=$1 gi=$2 tier got want
  tier=$(pg_tier "$cpu" "$gi")
  # shellcheck disable=SC2016 # $tier is jq's variable
  state_set '.postgres = $tier' --argjson tier "$tier"
  # Dispatch and the listener hold Postgres connections; the next step's reset starts both again.
  stop_copy
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

# ---- the replay -----------------------------------------------------------------------------------

# replay_manifest_valid MANIFEST: refuse a file that is not a schema 1 replay manifest.
replay_manifest_valid() {
  [ -f "$1" ] || die "no replay manifest at $1"
  [ "$(jq -r .schema "$1" 2>/dev/null)" = 1 ] || die "$1 is not a schema 1 replay manifest"
}

# replay_seed_refusal MANIFEST: why the manifest's seed may not be prepared, nothing when it may:
# no seed (INFRA-639's seed is not uploaded yet), or a seed check that failed. INFRA-514's architect
# requires check.passed true, version_size_mismatches 0 (each synthetic version's byte delta is the
# real version's size change) and anchors_outside_logged_block 0 (each anchor sits in its logged
# block, or the bundle counts it under the stated rule for a block that no longer exists).
replay_seed_refusal() {
  if [ "$(jq '.seed == null' "$1")" = true ]; then
    printf "the manifest %s names no seed (seed is null): INFRA-639's seed is not uploaded yet\n" "${1##*/}"
    return 0
  fi
  jq -r '.seed.check as $c | if ($c | type) != "object" then "the manifest'"'"'s seed has no check"
    else [(if $c.passed != true then "check.passed is \($c.passed)" else empty end),
      (if $c.version_size_mismatches != 0 then "version_size_mismatches is \($c.version_size_mismatches)" else empty end),
      (if $c.anchors_outside_logged_block != 0 then "anchors_outside_logged_block is \($c.anchors_outside_logged_block)" else empty end)]
      | if length == 0 then empty else "the seed check failed: \(join(", "))" end end' "$1"
}

# replay_checksum DB: loadgen replay-checksum of the copy's database DB (its line of JSON), from
# the runner, which needs its loadgen (put_loadgen).
replay_checksum() {
  local out
  endpoints
  out=$(kube_for 1800 exec runner -c runner -- /work/loadgen replay-checksum -allow-foreign "${ENDPOINTS[@]}" -db "$1") || return 1
  jq -ce . <<<"$out"
}

# The jq program over cgroup.jsonl lines: Dispatch's cores over the newest span of at least 30 s
# between two good reads of pod $pod taken at or after $since (epoch seconds), with no restart
# (usage going down) in it; nothing while there is no such span.
# shellcheck disable=SC2016 # jq's variables
QUIET_JQ='def secs: (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) + ((capture("\\.(?<f>[0-9]+)Z$").f // "0") | "0." + . | tonumber);
  [inputs | fromjson? | select(.role == "dispatch" and .pod == $pod and .error == "" and .usage_usec != null)
    | {t: (.at | secs), u: .usage_usec} | select(.t >= $since)] | sort_by(.t)
  | (last // empty) as $l | (map(select(.t <= $l.t - 30)) | last // empty) as $f
  | map(select(.t >= $f.t)) as $s
  | if any(range(1; $s | length); $s[.].u < $s[. - 1].u) then empty else ($l.u - $f.u) / 1e6 / ($l.t - $f.t) end'

# wait_dispatch_quiet: until the sampler's reads of Dispatch's container (cgroup.jsonl) give below
# 0.05 cores over 30 s, at most 900 s; the seconds it took go into QUIET_SECONDS.
wait_dispatch_quiet() {
  local since=$EPOCHSECONDS pod cores=""
  local deadline=$((since + 900)) logged=$since
  pod=$(find_pod dispatch) || die "kubectl could not list the Dispatch pod"
  [ -n "$pod" ] || die "no running Dispatch pod for the quiet check"
  note "waiting for Dispatch ($pod) to idle below 0.05 cores over 30 s in the sampler's cgroup reads"
  while :; do
    cores=$(tail -n 2000 -- "$RUN_DIR/cgroup.jsonl" 2>/dev/null | jq -nRr --arg pod "$pod" --argjson since "$since" "$QUIET_JQ") || cores=""
    if [ -n "$cores" ] && awk -v c="$cores" 'BEGIN { exit !(c < 0.05) }'; then break; fi
    ((EPOCHSECONDS < deadline)) || die "Dispatch did not idle below 0.05 cores over 30 s within 900 s (last: ${cores:-no 30 s span of good reads})"
    if ((EPOCHSECONDS - logged >= 30)); then
      note "Dispatch: ${cores:-no 30 s span of good reads yet} cores over the last 30 s; waiting"
      logged=$EPOCHSECONDS
    fi
    sleep 5
  done
  QUIET_SECONDS=$((EPOCHSECONDS - since))
  note "Dispatch is quiet: $cores cores over 30 s, after $QUIET_SECONDS s"
}

# wait_block_index DB: until the block-index backfill tables that DB has are empty
# (document_block_versions with a null version, block_grammar_owed), at most 3600 s.
wait_block_index() {
  local db=$1 parts=() sql pending started=$EPOCHSECONDS logged=$EPOCHSECONDS
  if pg_has_table "$db" document_block_versions; then parts+=("(select count(*) from document_block_versions where version is null)"); fi
  if pg_has_table "$db" block_grammar_owed; then parts+=("(select count(*) from block_grammar_owed)"); fi
  if ((${#parts[@]} == 0)); then
    note "database $db has no block-index backfill tables"
    return 0
  fi
  sql="select $(
    IFS=+
    printf '%s' "${parts[*]}"
  )"
  note "waiting for Dispatch's block-index backfill in $db (at most 3600 s)"
  while :; do
    pending=$(psql_at "$db" "$sql") || die "counting the block-index entries owed in $db failed"
    [ "$pending" != 0 ] || break
    ((EPOCHSECONDS - started < 3600)) || die "Dispatch still owes $pending block-index entries after 3600 s"
    if ((EPOCHSECONDS - logged >= 30)); then
      note "block-index backfill: $pending entries owed after $((EPOCHSECONDS - started)) s"
      logged=$EPOCHSECONDS
    fi
    sleep 2
  done
  note "block-index backfill done after $((EPOCHSECONDS - started)) s"
}

# replay_prepare_run MANIFEST FROM_DIR: replay-prepare (see the header), on a manifest that its
# caller has checked (replay_manifest_valid, replay_seed_refusal).
replay_prepare_run() {
  local manifest=$1 from_dir=$2 rdir=$RUN_DIR/replay rec=$RUN_DIR/replay-prepared.json
  local dump_sha t0 total0=$EPOCHREALTIME
  dump_sha=$(jq -er .seed.dump_sha256 "$manifest")
  cp -- "$manifest" "$RUN_DIR/replay-manifest.json"
  if [ -f "$rec" ] && [ "$(jq -r .seed_dump_sha256 "$rec")" = "$dump_sha" ] && db_exists "$REPLAY_TEMPLATE"; then
    note "replay-prepare: the template $REPLAY_TEMPLATE of seed dump $dump_sha exists ($rec); reused"
    return 0
  fi
  ensure_daemons

  # INFRA-639's parts, downloaded and verified on the worker as stack.sh replay-get does.
  local s_inputs s_stream s_restore s_migrate s_backfill s_vacuum s_checksum bundle_meta_sha seed_parts
  t0=$EPOCHREALTIME
  replay_get_bundle "$manifest" "$rdir" "$from_dir" loadgen_local
  bundle_meta_sha=$(jq -r '.bundle_meta_sha256 // ""' <<<"$REPLAY_BUNDLE_VERIFIED")
  replay_fetch_parts "$manifest" seed "$rdir/parts" "$from_dir"
  seed_parts=("${REPLAY_PARTS[@]}")
  s_inputs=$(elapsed_since "$t0")

  # The dump, restored into a fresh database dispatch.
  rm -f -- "$rec"
  stop_copy
  psql_at postgres "drop database if exists $REPLAY_TEMPLATE with (force)" >/dev/null || die "drop database $REPLAY_TEMPLATE failed"
  psql_at postgres 'drop database if exists dispatch with (force)' >/dev/null || die "drop database dispatch failed"
  psql_at postgres 'create database dispatch' >/dev/null || die "create database dispatch failed"
  t0=$EPOCHREALTIME
  note "streaming the seed dump (sha256 $dump_sha) into postgres-0:$REPLAY_POD_DUMP"
  # shellcheck disable=SC2016 # expanded by the pod's sh
  cat -- "${seed_parts[@]}" | kube_for 3600 exec -i postgres-0 -c postgres -- sh -c 'cat >"$1"' sh "$REPLAY_POD_DUMP" ||
    die "streaming the seed dump into the Postgres pod failed"
  local got
  got=$(kube_for 900 exec postgres-0 -c postgres -- sha256sum "$REPLAY_POD_DUMP" | cut -d' ' -f1)
  [ "$got" = "$dump_sha" ] || die "the dump in the Postgres pod has sha256 '$got', not the manifest's $dump_sha"
  s_stream=$(elapsed_since "$t0")
  t0=$EPOCHREALTIME
  local log=$RUN_DIR/logs/replay-restore.log
  note "pg_restore --no-owner --no-privileges -j 8 of the seed dump into database dispatch; log $log"
  if ! kube_for 7200 exec postgres-0 -c postgres -- pg_restore --no-owner --no-privileges -j 8 -U dispatch -d dispatch "$REPLAY_POD_DUMP" >"$log" 2>&1; then
    kube exec postgres-0 -c postgres -- rm -f "$REPLAY_POD_DUMP" >/dev/null 2>&1 || true
    tail -n 20 -- "$log" >&2 || true
    die "pg_restore of the replay seed failed; see $log"
  fi
  kube exec postgres-0 -c postgres -- rm -f "$REPLAY_POD_DUMP" || note "removing $REPLAY_POD_DUMP from the Postgres pod failed"
  local before want_before
  before=$(psql_at dispatch 'select max(version) from schema_migrations') || die "reading the restored seed's migration failed"
  want_before=$(jq -r .seed.migration "$manifest")
  [ "$before" = "$want_before" ] || die "the restored seed is at migration $before, the manifest says $want_before"
  s_restore=$(elapsed_since "$t0")

  # The listener first (it creates ENVOY_NOTIFICATIONS on three replicas), then Dispatch at the
  # run's commit, which migrates the seed forward before it serves /healthz.
  t0=$EPOCHREALTIME
  nats_purge
  scale_to listener 1
  note "starting Dispatch at $(state .dispatch_commit) on the seed (migration $before); it migrates it forward"
  scale_to dispatch 1 1800
  wait_dispatch_healthy 1800
  check_stream_replicas
  local schema
  schema=$(jq -r '.schema_version // empty' <<<"$HEALTHZ")
  [[ $schema =~ ^[0-9]+$ ]] || die "Dispatch's /healthz reports no schema_version: $HEALTHZ"
  s_migrate=$(elapsed_since "$t0")
  t0=$EPOCHREALTIME
  wait_block_index dispatch
  s_backfill=$(elapsed_since "$t0")
  wait_dispatch_quiet
  stop_copy

  t0=$EPOCHREALTIME
  psql_at dispatch 'vacuum analyze' >/dev/null || die "vacuum analyze of database dispatch failed"
  s_vacuum=$(elapsed_since "$t0")
  local after checksum
  after=$(psql_at dispatch 'select max(version) from schema_migrations') || die "reading the migrated seed's migration failed"
  t0=$EPOCHREALTIME
  put_loadgen
  checksum=$(replay_checksum dispatch) || die "loadgen replay-checksum of the restored template failed"
  s_checksum=$(elapsed_since "$t0")
  psql_at postgres "alter database dispatch rename to $REPLAY_TEMPLATE" >/dev/null || die "renaming dispatch to $REPLAY_TEMPLATE failed"
  jq -n --arg template "$REPLAY_TEMPLATE" --arg manifest "${manifest##*/}" --arg dump "$dump_sha" \
    --arg tar "$(jq -r .bundle.tar_zst_sha256 "$manifest")" --arg meta "$bundle_meta_sha" \
    --arg commit "$(state .dispatch_commit)" --arg image "$(state .images.dispatch)" \
    --argjson before "$before" --argjson after "$after" --argjson schema "$schema" --argjson checksum "$checksum" \
    --arg pg "$(psql_at postgres 'select version()')" --arg at "$(utc_now)" \
    --argjson inputs "$s_inputs" --argjson stream "$s_stream" --argjson restore "$s_restore" \
    --argjson migrate "$s_migrate" --argjson backfill "$s_backfill" --argjson quiet "$QUIET_SECONDS" \
    --argjson vacuum "$s_vacuum" --argjson csum "$s_checksum" --argjson total "$(elapsed_since "$total0")" \
    '{template: $template, manifest: $manifest, seed_dump_sha256: $dump, bundle_tar_zst_sha256: $tar,
      bundle_meta_sha256: $meta, dispatch_commit: $commit, dispatch_image: $image,
      migration_before: $before, migration_after: $after, schema_version: $schema, checksum: $checksum,
      postgres_version: $pg, prepared_at: $at,
      seconds: {inputs: $inputs, stream: $stream, restore: $restore, migrate: $migrate, backfill: $backfill,
        quiet: $quiet, vacuum: $vacuum, checksum: $csum, total: $total}}' >"$rec"
  note "replay-prepare done: template $REPLAY_TEMPLATE; $(jq -c '{migration_before, migration_after, schema_version, checksum, seconds}' "$rec")"
}

# replay_residual_notes NOTES MANIFEST: the residual notes file NOTES, then two list lines from the
# seed check (seed.check) of the replay manifest MANIFEST: the anchors by placement rule, with the
# share on the nearest surviving block (placed approximately), and the versions whose size change
# has its sign flipped. A count that the check does not hold, or a MANIFEST that is not there,
# reads "not recorded", never 0.
replay_residual_notes() {
  cat -- "$1" || return 1
  { if [ -f "$2" ]; then cat -- "$2"; else echo null; fi; } | jq -r '
    def count: if type == "number" then tostring else "not recorded" end;
    (try .seed.check catch null) as $c
    | (if ($c | type) == "object" then $c else {} end) as $c
    | (if ($c.anchor_rules | type) == "object" then $c.anchor_rules else {} end) as $r
    | [$r.logged_quote, $r.in_logged_block, $r.nearest_surviving_block] as $n
    | (if all($n[]; type == "number") then
        ($n | add) as $sum
        | if $sum == 0 then "0 (none placed approximately)"
          else (($n[2] * 1000 / $sum) | round) as $t | "\($sum) (\($t / 10 | floor).\($t % 10)% placed approximately)" end
      else "not recorded (share placed approximately not recorded)" end) as $of
    | "- Anchors by placement rule (seed check): \($n[0] | count) on the logged quote, \($n[1] | count) on a sentence in the logged block, \($n[2] | count) on the nearest surviving block, of \($of).",
      "- Seed versions with the size change'"'"'s sign flipped (seed check): \($c.sign_flipped | count)."'
}

# replay_codepaths DIR OFFSET: loadgen codepaths of the replay step DIR beside the outage: its
# heap profiles, the outage's cores and memory, the run's residual notes (replay_residual_notes of
# replay-residual.md and the prepared manifest, into <run>/replay-residual.md) and the window's
# offset from 22:55.
replay_codepaths() {
  local notes=$RUN_DIR/replay-residual.md
  replay_residual_notes "$here/replay-residual.md" "$RUN_DIR/replay-manifest.json" >"$notes" || return 1
  loadgen_local codepaths -step "$1" -out "$1" -heap-start "$1/heap-start.pprof" -heap-end "$1/heap-end.pprof" \
    -outage-cores "$OUTAGE_CORES" -outage-memory "$OUTAGE_MEMORY_GIB" -residual-notes "$notes" -window-offset "$2"
}

# run_replay NAME FROM TO PHASE: one replay step (see the header), into steps/NAME.
run_replay() {
  local name=$1 from=$2 to=$3 phase=$4 window
  valid step "$name" || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"
  valid offset "$from" || die "--from '$from' is not 0 or a whole number of s, m or h"
  valid duration "$to" || die "--to '$to' is not a whole number of s, m or h"
  window=$(($(duration_seconds "$to") - $(duration_seconds "$from")))
  ((window > 0)) || die "--to $to is not after --from $from"
  local dir=$RUN_DIR/steps/$name rec=$RUN_DIR/replay-prepared.json
  [ ! -e "$dir" ] || die "$dir exists: name another step, or remove it"
  { [ -f "$rec" ] && [ -f "$RUN_DIR/replay/bundle/bundle.meta.json" ]; } || die "no prepared replay ($rec and the bundle): cluster.sh replay-prepare first"
  ensure_daemons
  mkdir -p -- "$dir" "$RUN_DIR/phases"
  printf '%s\n' "$name" >>"$RUN_DIR/phases/$phase"
  note "replay $name: LINUX's write log from $from to $to after 22:55 at rate 1, on Postgres $(state .postgres.tier)"

  put_loadgen
  reset_copy "$dir/reset.json" "$REPLAY_TEMPLATE"
  container_snapshot "$dir/containers-before.tsv"
  write_copy_meta "$dir/copy-meta.json" "$dir"
  cp -- "$rec" "$dir/replay-prepared.json"
  # What loadgen run reads under $LOADTEST_ROOT for the replay's inputs: the reset
  # (seed_dump_sha256) and the build (dispatch_commit).
  jq -n --slurpfile p "$rec" --slurpfile r "$dir/reset.json" \
    '{template: $r[0].template, seed_dump_sha256: $p[0].seed_dump_sha256, checksum: $r[0].checksum,
      migration: $p[0].migration_after, prepared: $p[0], reset_at: $r[0].ended}' >"$dir/last-reset.json"
  jq -n --slurpfile p "$rec" '{dispatch_commit: $p[0].dispatch_commit, dispatch_image: $p[0].dispatch_image,
    newest_migration: $p[0].schema_version}' >"$dir/envoy-dispatch.json"
  kube exec runner -c runner -- sh -c 'rm -rf /work/bundle /work/root && mkdir -p /work/meta /work/steps /work/root/replay /work/root/bin' ||
    die "preparing the runner's directories failed"
  tar -C "$RUN_DIR/replay" -cf - bundle | kube_for 900 exec -i runner -c runner -- tar -C /work -xf - ||
    die "copying the bundle into the runner failed"
  kube exec runner -c runner -- /work/loadgen replay-verify-bundle -dir /work/bundle >/dev/null ||
    die "the bundle copied into the runner does not verify"
  runner_put "$dir/last-reset.json" /work/root/replay/last-reset.json
  runner_put "$dir/envoy-dispatch.json" /work/root/bin/envoy-dispatch.json
  runner_put "$dir/copy-meta.json" "/work/meta/$name.json"
  # Dispatch's GOMAXPROCS is its container's CPU limit, rounded up (Go's default in a cgroup).
  local dpod cmax gomaxprocs
  dpod=$(find_pod dispatch) || die "kubectl could not list the Dispatch pod"
  cmax=$(kube exec "$dpod" -c envoy-dispatch -- cat /sys/fs/cgroup/cpu.max) || die "reading Dispatch's cpu.max failed"
  [[ $cmax =~ ^([0-9]+)\ ([0-9]+)$ ]] || die "Dispatch's cpu.max reads '$cmax': no CPU limit, so its GOMAXPROCS is not known"
  gomaxprocs=$(((BASH_REMATCH[1] + BASH_REMATCH[2] - 1) / BASH_REMATCH[2]))

  endpoints
  run_in_runner "$name" "$dir" "replay-$name" $((30 + window + 300 + 900)) \
    env LOADTEST_ROOT=/work/root "LOADTEST_GOMAXPROCS=$gomaxprocs" \
    /work/loadgen run -allow-foreign -source replay -name "$name" -out "/work/steps/$name" \
    -events 0 -requests 0 -replay-inputs /work/bundle -replay-rate 1 -replay-from "$from" -replay-to "$to" \
    -rng-seed 1 -warmup 30s -copy-meta "/work/meta/$name.json" -build-commit "$(state .dispatch_commit)" "${ENDPOINTS[@]}"
  replay_codepaths "$dir" "$from" || note "replay $name: loadgen codepaths failed"
  note "replay $name done: $dir"
}

# run_replay_logged ARGS: run_replay, so that a replay that dies leaves the run going.
run_replay_logged() { go_on "replay $1" run_replay "$@"; }

# ---- collect --------------------------------------------------------------------------------------

# zstd_compress: stdin to stdout as zstd (level 19), through the zstandard wheel at a pinned
# version; the worker image carries no zstd command.
zstd_compress() {
  uv run -q --with zstandard==0.23.0 --no-project python3 -c \
    'import sys, zstandard; zstandard.ZstdCompressor(level=19, threads=-1).copy_stream(sys.stdin.buffer, sys.stdout.buffer)'
}

# pack_tar NAME ISSUE STAMP DIR PATH...: the PATHs under DIR as one zstd tar, split into parts
# under 24 MiB (upload/cluster-load-test-<date>-raw-<NAME>.tar.zst.partNN); with ISSUE, each part
# goes to that Dispatch issue. upload/NAME.packed keeps a copy of the file STAMP (the phase's list
# of steps) as it was at the last pack, so a tar packed and uploaded from the same list is left
# alone, and a list that gained a step, however soon after the last pack, packs again. The copy is
# compared, not the two files' times, since two steps of a phase can end within one tick of the
# file system's clock. With no STAMP, the tar is packed and uploaded again. A step's
# stored-order-evidence/ (document text, which a replay recreates from a production bundle) is left
# out: it stays on the pod, never uploaded.
pack_tar() {
  local name=$1 issue=$2 stamp=$3 dir=$4 up=$RUN_DIR/upload base part
  shift 4
  mkdir -p -- "$up"
  base=$up/cluster-load-test-$(state '.created[0:10]')-raw-$name.tar.zst.part
  if [ -z "$stamp" ] || ! cmp -s -- "$stamp" "$up/$name.packed"; then
    rm -f -- "$base"[0-9][0-9] "$up/$name.uploaded" "$up/$name.packed"
    tar -C "$dir" --exclude=stored-order-evidence -cf - "$@" | zstd_compress | split -b 24000000 -d -a 2 - "$base" ||
      die "packing phase $name failed"
    if [ -n "$stamp" ]; then cp -- "$stamp" "$up/$name.packed"; else : >"$up/$name.packed"; fi
    note "phase $name packed: $(printf '%s ' "$base"[0-9][0-9])"
  fi
  if [ -n "$issue" ] && [ ! -f "$up/$name.uploaded" ]; then
    for part in "$base"[0-9][0-9]; do
      dispatch artifact --issue "$issue" --name "${part##*/}" --path "$part" >/dev/null ||
        die "dispatch artifact of ${part##*/} to $issue failed"
    done
    : >"$up/$name.uploaded"
    note "phase $name uploaded to $issue"
  fi
}

# pack_phase PHASE [ISSUE]: the phase's step directories (pack_tar), packed again only when the
# phase has run a step since.
pack_phase() {
  phase_dirs "$1"
  ((${#DIRS[@]})) || return 0
  pack_tar "$1" "${2:-}" "$RUN_DIR/phases/$1" "$RUN_DIR/steps" "${DIRS[@]##*/}"
}

# pack_results [ISSUE]: the phase results (pack_tar), packed and uploaded again on every collect:
# curves/, TABLES.md, STORED-ORDER.md, and each step's CODEPATHS.md and codepaths.json, which
# collect writes after the step's own phase was packed.
pack_results() {
  local paths=() f
  [ ! -d "$RUN_DIR/curves" ] || paths+=(curves)
  [ ! -f "$RUN_DIR/TABLES.md" ] || paths+=(TABLES.md)
  [ ! -f "$RUN_DIR/STORED-ORDER.md" ] || paths+=(STORED-ORDER.md)
  for f in "$RUN_DIR"/steps/*/CODEPATHS.md "$RUN_DIR"/steps/*/codepaths.json; do
    if [ -f "$f" ]; then paths+=("${f#"$RUN_DIR"/}"); fi
  done
  ((${#paths[@]})) || return 0
  pack_tar results "${1:-}" "" "$RUN_DIR" "${paths[@]}"
}

# collect_run [ISSUE]: the cross-step outputs, with every step in the order the steps ran:
# STORED-ORDER.md over every step directory, TABLES.md over the steps that wrote summary.json,
# loadgen curves over every step directory but the phase replay's into <run>/curves, and loadgen
# codepaths in each step directory that holds summary.json (a step that wrote none has no profile;
# a replay step's as replay_codepaths runs it); then each phase's tar and the results tar. A
# failure is reported, the rest still runs, and collect_run returns 1.
collect_run() {
  local issue=${1:-} ran=() names=() curve_dirs=() replays=" " d p rc=0
  [ ! -f "$RUN_DIR/phases/replay" ] || replays+="$(tr '\n' ' ' <"$RUN_DIR/phases/replay")"
  steps_in_run_order
  for d in "${DIRS[@]}"; do
    if [ -f "$d/summary.json" ]; then ran+=("$d") names+=("${d##*/}"); fi
    if [[ $replays != *" ${d##*/} "* ]]; then curve_dirs+=("$d"); fi
  done
  if stored_order_table "${DIRS[@]}"; then
    note "wrote $RUN_DIR/STORED-ORDER.md over ${#DIRS[@]} steps, in the order they ran"
  else
    note "writing $RUN_DIR/STORED-ORDER.md failed"
    rc=1
  fi
  if ((${#ran[@]})); then
    if loadgen_local tables -tops=false -out "$RUN_DIR/TABLES.md" "${ran[@]}"; then
      note "wrote $RUN_DIR/TABLES.md over ${#ran[@]} steps, in the order they ran: ${names[*]}"
    else
      note "loadgen tables across the steps failed"
      rc=1
    fi
  else
    note "no step directory holds summary.json; no TABLES.md"
  fi
  if ((${#curve_dirs[@]})); then
    if loadgen_local curves -out "$RUN_DIR/curves" "${curve_dirs[@]}"; then
      note "wrote $RUN_DIR/curves over ${#curve_dirs[@]} step directories"
    else
      note "loadgen curves over the step directories failed"
      rc=1
    fi
  else
    note "no step directory outside the replay; no curves"
  fi
  for d in "${ran[@]}"; do
    if [[ $replays == *" ${d##*/} "* ]]; then
      replay_codepaths "$d" "$(jq -r '.replay.inputs.from_s // 0' "$d/summary.json")s"
    else
      loadgen_local codepaths -step "$d" -out "$d"
    fi || {
      note "step ${d##*/}: loadgen codepaths failed"
      rc=1
    }
  done
  for p in "$RUN_DIR"/phases/*; do
    if [ -f "$p" ]; then pack_phase "${p##*/}" "$issue"; fi
  done
  pack_results "$issue"
  ((rc == 0)) || note "collect: a command failed (above); every other output was written"
  return "$rc"
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
  parse "namespace storage-class dispatch-commit pg-major production-repo listener-image stored-order-commit" "" "$@"
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
  local so_commit=${OPT[stored-order-commit]:-}
  PRODUCTION_REPO=${OPT[production-repo]:-}
  local tool
  for tool in kubectl jq curl gh go setsid timeout sha256sum awk tar gzip base64; do
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
  if [ -n "$so_commit" ]; then
    valid commit "$so_commit" || die "up: --stored-order-commit '$so_commit' is not a full 40-hex commit sha"
    stored_order_source "$so_commit"
  fi

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
  kube_lock_take
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
      image_sources: {listener: $listener_ref}, postgres: $tier, stored_order: null,
      secrets: {pg_password: $pgpw, dispatch_token: $dt, listener_token: $lt, signing_key: $sk}}' >"$STATE")
  chmod 600 "$STATE"
  start_daemon heartbeat

  note "building loadgen (static) into $LOADGEN"
  (cd -- "$ENVOY_DIR" && CGO_ENABLED=0 go build -o "$LOADGEN" ./loadtest) || die "go build of loadgen failed"
  if [ -n "$so_commit" ]; then
    note "building envoy-dispatch (static) of commit $so_commit into $STORED_ORDER_BIN, for the stored-order check"
    build_stored_order "$so_commit" "$STORED_ORDER_BIN" || die "go build of envoy-dispatch at commit $so_commit failed"
    # shellcheck disable=SC2016 # $c and $s are jq's variables
    state_set '.stored_order = {commit: $c, sha256: $s}' --arg c "$so_commit" --arg s "$(sha256_of "$STORED_ORDER_BIN")"
  fi

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
  [ -z "$so_commit" ] || put_stored_order
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
  stop_copy
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
  refine_run "$d"
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

cmd_replay_prepare() {
  parse "namespace run manifest from-dir" "" "$@"
  positional 0 "no positional arguments"
  local manifest=${OPT[manifest]:-$REPLAY_MANIFEST} from_dir=${OPT[from-dir]:-} why
  replay_manifest_valid "$manifest"
  why=$(replay_seed_refusal "$manifest")
  [ -z "$why" ] || die "replay-prepare: $why; refusing before it touches the copy"
  [ -z "$from_dir" ] || [ -d "$from_dir" ] || die "replay-prepare: --from-dir $from_dir is not a directory"
  use_run
  replay_prepare_run "$manifest" "$from_dir"
}

cmd_replay() {
  parse "namespace run from to" "" "$@"
  positional 1 "NAME"
  use_run
  run_replay "${POS[0]}" "${OPT[from]:-0}" "${OPT[to]:-39m}" replay
}

cmd_status() {
  parse "namespace run" "" "$@"
  positional 0 "no positional arguments"
  local lock run=${OPT[run]:-} name
  lock=$(kube_lock_get)
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
  local lock lock_run="" run=${OPT[run]:-} holder
  lock=$(kube_lock_get)
  [ -z "$lock" ] || lock_run=$(jq -r '.data.run // ""' <<<"$lock")
  if [ -z "$run" ]; then
    [ -n "$lock_run" ] || die "down: namespace $NS has no lock ConfigMap $LOCK; name the run to remove with --run"
    run=$lock_run
    valid run "$run" || die "down: the lock names run '$run', which is not one cluster.sh makes"
    if [ "$(jq -r '.data."holder-uid" // ""' <<<"$lock")" != "${POD_UID:-}" ] || [ ! -f "$CLUSTER_ROOT/$run/state.json" ]; then
      note "down: the lock is held by another run:"
      holder=$(jq -r '.data | "run \(.run), holder pod \(."holder-pod") (uid \(."holder-uid"), issue \(.issue)), heartbeat \(.heartbeat)"' <<<"$lock")
      note "  $holder"
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
  parse "namespace storage-class dispatch-commit pg-major production-repo listener-image stored-order-commit profile manifest upload-issue" "keep" "$@"
  positional 0 "no positional arguments"
  local step_d hour_d replay_to refine collect_rc issue=${OPT[upload-issue]:-} manifest=${OPT[manifest]:-$REPLAY_MANIFEST} why
  case ${OPT[profile]:-} in
    quick) step_d=2m hour_d=5m replay_to=5m refine=false ;;
    full) step_d=10m hour_d=60m replay_to=39m refine=true ;;
    *) die "all: --profile quick or --profile full is required" ;;
  esac
  [ -z "$issue" ] || valid issue "$issue" || die "--upload-issue '$issue' is not an issue key"
  replay_manifest_valid "$manifest"
  ALL_KEEP=${OPT[keep]:-}
  RUN_TAKEN=""
  trap all_exit EXIT
  up_run
  seed_run
  run_series "$step_d"
  pack_phase series "$issue"
  if [ "$refine" = true ]; then
    go_on "the refinement" refine_run "$step_d"
    pack_phase refinement "$issue"
  else
    note "skipped phase refinement: profile quick runs no refinement steps"
  fi
  run_step_logged 10x-hour 10 "$hour_d" false soak
  pack_phase soak "$issue"
  # The replay on the 16 vCPU database, then on the 4 vCPU one after its 1x step.
  local replay_ready=false
  why=$(replay_seed_refusal "$manifest")
  if [ -n "$why" ]; then
    note "skipped phase replay-prepare: $why"
    note "skipped phase replay r16: replay-prepare was skipped, so there is no template $REPLAY_TEMPLATE"
  else
    isolated replay_prepare_run "$manifest" ""
    if ((ISOLATED_RC == 0)); then
      replay_ready=true
      run_replay_logged r16 0 "$replay_to" replay
      pack_phase replay "$issue"
    else
      why="replay-prepare did not complete (exit $ISOLATED_RC)"
      note "skipped phase replay r16: $why"
    fi
  fi
  isolated resize_postgres 4 15
  if ((ISOLATED_RC == 0)); then
    run_step_logged 1x-db4 1 "$step_d" false db4
  else
    note "db-size 4 15Gi failed (exit $ISOLATED_RC); step 1x-db4 is skipped"
  fi
  pack_phase db4 "$issue"
  if [ "$replay_ready" = true ]; then
    run_replay_logged r4 0 "$replay_to" replay
    pack_phase replay "$issue"
  else
    note "skipped phase replay r4: $why"
  fi
  isolated collect_run "$issue"
  collect_rc=$ISOLATED_RC
  if [ -z "$ALL_KEEP" ]; then
    down_run
    RUN_TAKEN=""
  fi
  ((collect_rc == 0)) || die "all: collect ended with exit $collect_rc; its messages above name what failed"
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
    replay-prepare) cmd_replay_prepare "$@" ;;
    replay) cmd_replay "$@" ;;
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
