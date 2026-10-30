#!/usr/bin/env bash
# cluster.sh: the Dispatch load test on a full-size copy of Dispatch, its Postgres, the Envoy
# listener and a three-pod NATS JetStream hub in a Kubernetes namespace, driven from a Legion
# worker pod (INFRA-598). It installs the copy from manifests/, seeds it, runs loadgen steps from a
# runner pod inside it, samples every copy container's cgroup, collects, and removes the copy.
#
#   cluster.sh up --namespace NS --storage-class SC --dispatch-commit SHA --pg-major 16|18
#                 --production-repo OWNER/REPO [--pg-volume 100Gi] [--listener-image REF]
#                 [--stored-order-source SHA | --stored-order-checker-commit SHA]
#                 [--browsertype-commit SHA]
#       Refuses, before it creates anything, a commit with no ghcr.io envoy:<commit> image (naming
#       the newest first-parent ancestor that has one), a --pg-major other than the production
#       repository's dispatch/main.go databaseVersion, an image tag that does not resolve to a
#       digest, a stored-order checker it cannot build (below: a --dispatch-commit that this
#       checkout's git store does not hold; a dispatch tree without the stored-order command and
#       no --stored-order-source, or with it and a --stored-order-source; a source without
#       INFRA-707's files or registration; a dispatch subcommand table without the registration's
#       anchor; a build failure, naming the commits and the compiler's first error), and a
#       --browsertype-commit that the git store does not hold or whose tree has no
#       packages/envoy/loadtest/browsertype, and a --pg-volume that is not a whole number of Gi
#       or is above the 240Gi the namespace's quota leaves beside NATS's three 20Gi claims.
#       Refuses with exit 2 while the lock ConfigMap
#       dispatch-loadtest-lock exists. Then takes the lock, installs the copy (Postgres 16 CPU /
#       60Gi on a --pg-volume data volume, default 100Gi, Dispatch 16 / 32Gi, listener 500m /
#       512Mi, NATS 3 × 2 / 4Gi, runner 8 / 16Gi), builds
#       loadgen and (with --browsertype-commit) a static browsertype of that commit for browser
#       typing (below), starts the heartbeat and the sampler, and prints each copy container's
#       requests and limits and the size of the Postgres data volume.
#   cluster.sh seed --namespace NS            loadgen seed, seed-refs and seed-finish, then the template
#                                             database dispatch_seed.
#   cluster.sh step --namespace NS NAME FACTOR [--duration 10m] [--reads-only] [--typing
#                   [--typing-peers 2] [--typing-rate 5] [--typing-docs 20]]
#       Reset, then one loadgen run at FACTOR × (4 events/s, 60 requests/s) with the
#       series-sessions count (--reads-only: 0 events), then the crash check, the stored-order
#       check, cgroup-merge, tables. --typing adds browser typing (below).
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
#   cluster.sh replay --namespace NS NAME [--from 0] [--to 39m] [--typing [--typing-peers 2]
#                     [--typing-rate 5] [--typing-docs 20]]
#       The reset from dispatch_replay (refused without it), the bundle into the runner, then
#       loadgen run -allow-foreign -source replay at rate 1 over the sub-window, then the step's
#       after-work and loadgen codepaths beside the outage (11.1-13.8 cores, 13.8 GiB, the
#       residual <run>/replay-residual.md: replay-residual.md's lines, then the seed check's anchors
#       by placement rule and sign-flipped versions from <run>/replay-manifest.json). --typing
#       adds browser typing (below).
#   cluster.sh collect --namespace NS [--upload-issue KEY]
#       Every step in the order the steps ran (meta.json start): the cross-step STORED-ORDER.md
#       (with each step's browser typing) and TABLES.md, loadgen curves into <run>/curves over
#       every step but the phase replay's and those that ran browser typing, loadgen codepaths
#       in each step directory (a replay step's beside the outage, as replay runs it); then each
#       phase's step directories, and the results (curves/, TABLES.md,
#       STORED-ORDER.md, each step's CODEPATHS.md and codepaths.json), as zstd tar parts under
#       24 MiB in <run>/upload/; with --upload-issue, each part goes to that Dispatch issue. A
#       failure is reported, the rest still runs, and collect exits 1.
#   cluster.sh status --namespace NS          the lock, the run, its hold, its daemons and its pods.
#   cluster.sh down --namespace NS [--run ID] deletes everything labelled with the run id and the
#       lock, removes the run's hold, and waits until nothing of the run is left. A lock of
#       another run is deleted only when --run names that run. A run another PID namespace
#       started keeps its daemons: down says so, and they stop on their own (below).
#   cluster.sh hold --namespace NS [--for DURATION]   holds the run that is up for DURATION
#       (default 2h), starting its daemons again when they stopped: while the hold lasts its
#       heartbeat renews the lock with no command of the run running, so no other worker's up
#       calls it stale. Refused when no run is up.
#   cluster.sh all --namespace NS <up's flags> --profile quick|full [--manifest FILE]
#                  [--upload-issue KEY] [--keep [--keep-for DURATION]]
#       up, seed, the series, the refinement (full only), soak, replay-prepare, replay r16,
#       db-size 4 15Gi, step 1x-db4, replay r4, collect, down; any exit runs down unless --keep.
#       quick: 2 min steps, a 5 min hour, replays over 0-5m; full: 10 and 60 min, replays over
#       0-39m. A manifest whose seed is null or fails its check skips the replay phases, one
#       line each. all runs no browser typing; with --browsertype-commit and --keep, the run
#       stays up for typing steps and replays named by the caller, held (--keep-for, default 2h,
#       valid only with --keep) so it does not go stale while they are typed; cluster.sh hold
#       extends that hold.
#
# The stored-order checker: up builds a static envoy-dispatch from --dispatch-commit's own tree
# (git archive of this checkout's git store, built in a scratch directory), so the checker's
# renderer is the one Dispatch runs. A dispatch tree without the stored-order command
# (packages/envoy/cmd/dispatch/storedorder.go) needs --stored-order-source SHA, refused otherwise:
# that commit's INFRA-707 files (internal/dispatch/docs/storedorder.go,
# cmd/dispatch/storedorder.go) are copied into the scratch copy, and its stored-order entry of
# cmd/dispatch/subcommands.go's table is inserted after the dispatch tree's table opener `var
# subcommands = []subcommand{` (refused when that line is not there exactly once). The escape
# --stored-order-checker-commit SHA builds the checker wholly from SHA instead; up warns that its
# renderer may differ from Dispatch's and records renderer_matches false. state.json stored_order
# records dispatch_commit, built_from (the commit whose tree was built), overlay_source (or null),
# overlay_files, registration (the file the overlay edited, or null), renderer_matches and the
# binary's sha256; up builds the checker before it takes the lock.
#
# The stored-order check (INFRA-707's oracle): after each step's and each replay's loadgen run,
# while Dispatch still runs, the runner runs the checker's `stored-order --since <meta.json
# window_start>`, with the copy's DATABASE_URL on its standard input (never on a command line),
# within STORED_ORDER_SECONDS. A step that typed runs it twice inside that bound: first over the
# typed documents alone (one --artifact of each), into <step>/stored-order-typed.txt and
# stored-order-typed.json, so their result stands even when the run that follows times out; then
# over every document, into <step>/stored-order.txt and <step>/stored-order.json (the checker's
# fields and its summary line, renderer_matches false when Dispatch was redeployed
# to another commit), with the bound's rest. Each call names its counts in one line. A non-zero
# exit is a correctness finding,
# not a step failure. Before the next reset drops the database, the rows of each mismatched or
# refused document (artifacts, doc_checkpoints, artifact_versions, doc_updates, doc_snapshots),
# its issue's events since window_start (issue-events-in-window), and its doc_checkpoints,
# artifact_versions, doc_updates and doc_snapshots in the template database the step was reset
# from (start-<table>) go into <step>/stored-order-evidence/<artifact uuid>/ as gzipped CSVs,
# with version-<n>.md for each mismatched version's stored markdown, through psql \copy to stdout
# in the Postgres pod, within 600 s; stored-order.json evidence names them. A failed export is
# reported, never a step failure, and the evidence is never uploaded. collect writes
# <run>/STORED-ORDER.md.
#
# Browser typing (step and replay --typing, INFRA-707's race of agent writes and browser typing):
# INFRA-707's browsertype of --browsertype-commit, refused without it, types into the K
# (--typing-docs) documents the step writes most, --typing-peers peers each at --typing-rate
# keystroke updates per second, through Dispatch's document websocket as session
# ses-browsertype with the copy's agent token, which goes into the runner's /work/typing/token on
# kubectl exec's standard input (never on a command line, in no recorded file, never printed) and
# is removed when typing stops. A replay's documents are the artifacts with the most agent
# writes (artifact.version events the replay sends) in the bundle's window.jsonl over the
# sub-window, mapped to the copy's ids through loadtest.replay_ids; a load step's are the K
# documents `loadgen plan-docs` names from seed.json and the step's own loadgen run flags, those
# its in-window edits and uploads plan the most versions for, and browsertype.json records each
# one's planned count (planned_versions); a Kth document under 10 planned versions, which
# INFRA-707's acceptance 2 needs, is one warning line. It starts detached in the runner just
# before loadgen run, so its peers connect during loadgen's warm-up, with a -duration that
# outlasts loadgen's window, grace and the stored-order check, and stops with SIGTERM right after
# the last stored-order command. Its output goes into
# <step>/browsertype.txt, and its exit and per-peer sums into <step>/browsertype.json; a peer
# error is reported, never a step failure.
#
# The Postgres data volume (up --pg-volume, default 100Gi; db-size keeps the same claim): a
# replay with browser typing on the reference seed filled a 20Gi volume, and Postgres crash-looped
# with "No space left on device" with the step's load still running. So each step and each replay
# reads the volume (df -B1 of /var/lib/postgresql/copy in the Postgres pod, a read) after its
# reset and before its load, every PG_DISK_POLL_SECONDS (30 s) while the load runs, and once more
# when the load ends. Free space below PG_DISK_MIN_FREE_PCT (15 percent of the volume;
# LOADTEST_PG_DISK_MIN_FREE_PCT overrides it) refuses the step before any load, or, from the
# watcher, stops the step's loadgen run and its browser typing. Either way the step is a failed
# one (step-failed.json kind pg_disk, with the reading), never a crash, so the series goes on and
# shows it, and the step's after-work still runs. Each step and replay writes <step>/pg-disk.json:
# size_bytes, used_before, used_after, used_peak, free_min, samples (each reading with its time)
# and threshold_pct; a read that failed is a sample with error and no number.
#
# A read-only kubectl exec (psql reads, nats-box reads, the runner's file reads) that fails on a
# transient API server error (error dialing backend, a refused or reset connection to the API
# server) is retried once, 5 s later; a write is never retried.
#
# The run's two daemons, started under setsid: the heartbeat renews the lock every 60 s while a
# command of the run is alive or a hold holds it, and the sampler reads every copy container's
# cgroup. The sampler ends with the heartbeat (no live heartbeat of the run for 60 s, or no run
# directory), so a killed or torn-down run leaves neither kubectl execs nor growing cgroup.jsonl
# error lines; the run's next command starts both again. up records its PID namespace in
# state.json (pid_ns); a command of the run from another PID namespace is refused before it
# registers anything or starts a daemon, because the pids of one namespace mean nothing in
# another (a Legion pod gives each role its own over one CLUSTER_ROOT). status and down are not
# refused: neither registers a command, and down deletes the run's objects and the lock.
#
# Every subcommand needs --namespace (no default; a Legion pod's own namespace is builds), and
# every kubectl call names it. Subcommands after up find the run from the lock's `run` and its
# directory ${LOADTEST_CLUSTER_ROOT:-$TMPDIR/dispatch-loadtest-cluster}/<run id>/ (state.json, mode
# 600, holds the digests and the run's random credentials; cgroup.jsonl; steps/; logs/); --run ID
# names the run instead. Needs kubectl, jq, curl, gh, go, setsid, timeout, tar, gzip, base64,
# sha256sum, uv (collect), git (up), dispatch (collect --upload-issue), and DISPATCH_URL and
# DISPATCH_TOKEN_FILE (replay-prepare's downloads, without --from-dir).
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
# The Postgres data volume: where postgres.yaml mounts the claim (PGDATA is under it), the
# default and the largest --pg-volume, and what the namespace's ResourceQuota leaves for it.
# The quota allows requests.storage 300Gi over 10 claims, and NATS takes three claims of 20Gi, so
# the Postgres claim may ask for at most 300 - 60 = 240Gi. db-size keeps the claim it finds.
PG_DATA_MOUNT=/var/lib/postgresql/copy
PG_VOLUME_DEFAULT=100Gi
PG_QUOTA_STORAGE_GI=300
NATS_VOLUME_GI=60
PG_VOLUME_MAX_GI=$((PG_QUOTA_STORAGE_GI - NATS_VOLUME_GI))
# The disk guard (pg_disk): the free space, as a percent of the volume, that a step needs before
# it sends load and keeps while it does (LOADTEST_PG_DISK_MIN_FREE_PCT overrides it), and how
# often the watcher reads the volume while the load runs. A step with less is a failed step
# (kind pg_disk) that sends no load, or whose load the watcher stops: a Postgres that fills its
# volume crash-loops, which loses the step and every later one until the copy is reset.
PG_DISK_MIN_FREE_PCT=${LOADTEST_PG_DISK_MIN_FREE_PCT:-15}
[[ $PG_DISK_MIN_FREE_PCT =~ ^[1-9][0-9]?$ ]] ||
  { printf 'cluster.sh: LOADTEST_PG_DISK_MIN_FREE_PCT must be a whole percent from 1 to 99, not %s\n' "$PG_DISK_MIN_FREE_PCT" >&2; exit 2; }
PG_DISK_POLL_SECONDS=30
# The last reading of the volume (pg_disk_sample), in bytes, each empty when that read failed,
# and the pid of the step's watcher while one runs (pg_disk_watch_stop ends it).
PG_DISK_SIZE="" PG_DISK_USED="" PG_DISK_FREE=""
PG_DISK_WATCHER=""
# The LINUX replay: INFRA-639's manifest, the template database replay-prepare makes,
# where the dump goes in the Postgres pod (on its claim), and the outage's Dispatch figures that
# loadgen codepaths sets a replay beside (production profile §5: 11.1-13.8 cores, 13.8 GiB).
REPLAY_MANIFEST=$here/../replay-2026-10-26.json
REPLAY_TEMPLATE=dispatch_replay
REPLAY_POD_DUMP=/var/lib/postgresql/copy/replay-seed.dump
OUTAGE_CORES=11.1,13.8
OUTAGE_MEMORY_GIB=13.8
# The stored-order checker (INFRA-707's command): the file whose presence in a tree means the tree
# has the command, the files an overlay copies from --stored-order-source into a dispatch tree
# without it, the subcommand table the overlay registers it in, and the table's opener, after
# which the overlay inserts the source's stored-order entry. CHECKER_TMP is up's scratch
# directory for the checker until the run directory exists (removed on every exit), and
# STORED_ORDER_STATE the checker's state.json stored_order (stored_order_build).
STORED_ORDER_COMMAND=packages/envoy/cmd/dispatch/storedorder.go
STORED_ORDER_FILES=(packages/envoy/internal/dispatch/docs/storedorder.go "$STORED_ORDER_COMMAND")
STORED_ORDER_REGISTRY=packages/envoy/cmd/dispatch/subcommands.go
STORED_ORDER_ANCHOR='var subcommands = []subcommand{'
CHECKER_TMP="" STORED_ORDER_STATE=""
# The stored-order check's bound, in seconds (its timeout's exit status is 124). A step with browser
# typing gives each typed document thousands of stored updates: at 10x with 20 typed documents the
# check took 1800 s for 976 of 1000 documents (run lt-20261029t115927-655c), so the bound is 7200 s.
# LOADTEST_STORED_ORDER_SECONDS overrides it.
STORED_ORDER_SECONDS=${LOADTEST_STORED_ORDER_SECONDS:-7200}
[[ $STORED_ORDER_SECONDS =~ ^[1-9][0-9]*$ ]] ||
  { printf 'cluster.sh: LOADTEST_STORED_ORDER_SECONDS must be a whole number of seconds, not %s\n' "$STORED_ORDER_SECONDS" >&2; exit 2; }
# The stored-order evidence export's bound for all of one step's documents, in seconds, and the
# tables it exports for each document at the step's end, as table:key column[:order]: every table
# that `envoy-dispatch stored-order` reads of a document (INFRA-707's
# internal/dispatch/docs/storedorder.go), and doc_snapshots. STORED_ORDER_START_TABLES are the ones
# it exports again from the template database the step was reset from, as start-<table>.csv.gz.
STORED_ORDER_EVIDENCE_SECONDS=600
STORED_ORDER_TABLES=(artifacts:id doc_checkpoints:artifact_id artifact_versions:artifact_id:number doc_updates:artifact_id:version doc_snapshots:artifact_id:name)
STORED_ORDER_START_TABLES=(doc_checkpoints:artifact_id artifact_versions:artifact_id:number doc_updates:artifact_id:version doc_snapshots:artifact_id:name)
# Browser typing: the session INFRA-707's browsertype peers act as, and the runner's files: the
# agent token (written on kubectl exec's standard input, removed when typing stops) and each
# step's document list.
TYPING_SESSION=ses-browsertype
TYPING_DIR=/work/typing
# The text browsertype must never type into: loadgen's edits replace the spec's "Revision token
# rN" and its uploads read it, so one keystroke there fails every later edit and upload to the
# document (404 TARGET_NOT_FOUND). browsertype -protect skips each paragraph that holds it; up
# refuses a browsertype without that flag.
TYPING_PROTECT="Revision token"
# A step's --typing settings (typing_parse); TYPING is empty for a step without --typing. The
# step's documents (TYPING_LIST), a load step's planned versions of each of them (TYPING_PLANNED,
# typing_plan_step; null for a replay, whose documents come from its bundle) and, once
# browsertype runs, the step's name and directory for typing_stop_at_exit (TYPING_STEP).
TYPING=""
TYPING_PEERS="" TYPING_RATE="" TYPING_DOCS=""
TYPING_LIST=()
TYPING_PLANNED=null
TYPING_STEP=()
# What down deletes and then lists: the kinds of `kubectl get all` that the load-test Role may
# read (it may not list replicationcontrollers, daemonsets, horizontalpodautoscalers or
# cronjobs, so `get all` itself is refused), and the claims, ConfigMaps and Secrets.
COPY_KINDS=pods,services,deployments,statefulsets,replicasets,jobs,persistentvolumeclaims,configmaps,secrets
DELETE_KINDS=deployments,statefulsets,jobs,pods,services,persistentvolumeclaims,configmaps,secrets
SSA=(apply --server-side --force-conflicts --field-manager=dispatch-loadtest -f -)
# The nats-box pod that reset_copy looked up for nats_box; empty outside a reset.
NATS_BOX_POD=""
# The transient API server errors on which kube_read retries a read-only kubectl call once: the
# API server could not dial the kubelet ("error dialing backend: dial timeout, backstop", or a
# refused dial), or the connection to the API server was refused or reset.
TRANSIENT_RE='^(Error from server: error dialing backend: |The connection to the server .* was refused|Unable to connect to the server: |error: .*(connect: connection refused|connection reset by peer))'
TRANSIENT_RETRY_SECONDS=5

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# kube ARGS: kubectl in the copy's namespace, bounded by timeout (never --request-timeout, which
# drops a pod's in-cluster config: kubernetes/kubernetes#93474). kube_for SECONDS ARGS: the same
# with its own bound.
kube() { timeout 300 kubectl -n "$NS" "$@"; }
kube_for() {
  local seconds=$1
  shift
  timeout "$seconds" kubectl -n "$NS" "$@"
}

# kube_read ARGS: kube ARGS for a call that only reads (an exec that reads a file, a psql query,
# a nats-box listing), retried once TRANSIENT_RETRY_SECONDS later when it fails with a
# TRANSIENT_RE line on its standard error, saying so. Its standard output and error are those of
# the last try (held in files until then, so a failed try leaves nothing on them); its exit
# status is the last try's. Never for a write: a write's failure is never retried.
kube_read() {
  local out err rc try
  out=$(mktemp "${TMPDIR:-/tmp}/cluster-read.XXXXXX") || return 1
  err=$(mktemp "${TMPDIR:-/tmp}/cluster-read.XXXXXX") || {
    rm -f -- "$out"
    return 1
  }
  for try in 1 2; do
    rc=0
    kube "$@" >"$out" 2>"$err" || rc=$?
    if ((rc == 0 || try == 2)) || ! grep -qE -- "$TRANSIENT_RE" "$err"; then break; fi
    note "kubectl $1 failed on a transient API server error ($(grep -m1 -E -- "$TRANSIENT_RE" "$err")); retrying once in $TRANSIENT_RETRY_SECONDS s"
    sleep "$TRANSIENT_RETRY_SECONDS"
  done
  cat -- "$out"
  cat -- "$err" >&2
  rm -f -- "$out" "$err"
  return "$rc"
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
    volume) re='^[1-9][0-9]{0,3}Gi$' ;;
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
  [PG_TIER]=tier [PG_CPU]=cpu [PG_MEMORY]=memory [PG_VOLUME]=volume
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
    "PG_VOLUME=\(.postgres.volume)",
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
# NS and, when it records one, this PID namespace (pid_ns_check); then this command is registered
# as one that holds the run alive.
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
  pid_ns_check
  register_command
}

# pid_ns: this process's PID namespace (readlink /proc/self/ns/pid), empty where /proc gives none.
pid_ns() { readlink /proc/self/ns/pid 2>/dev/null || true; }

# pid_ns_check: refuse, before anything is registered or started, a command of a run that another
# PID namespace started. A Legion pod runs each role in a PID namespace of its own over one
# CLUSTER_ROOT, so the pids that role recorded (the run's command files, the daemons' pidfiles)
# name nothing here, and a command from here would register itself as alive for a heartbeat it
# cannot see. Only the role that ran up drives its run; another role runs a copy of its own. A
# state.json from before pid_ns (an older run) and a pod whose /proc gives no namespace are not
# checked, and status (read-only) and down (which deletes the run's objects and the lock without
# reading a pid) never come through here.
pid_ns_check() {
  local recorded here
  recorded=$(jq -r '.pid_ns // ""' "$STATE")
  [ -n "$recorded" ] || return 0
  here=$(pid_ns)
  [ -n "$here" ] || return 0
  [ "$here" != "$recorded" ] || return 0
  note "run $RUN was started in PID namespace $recorded by pod $(state '.holder_pod // "?"'), and this command runs in $here."
  die "$CMD: only the role that ran up can drive run $RUN: its pids mean nothing in this PID namespace, so a command from here would hold a heartbeat it cannot see. Run your own copy in another namespace, or ask that role's holder (issue $(lock_issue)) to run it. cluster.sh status and cluster.sh down work from here."
}

# lock_issue: the issue the lock names, "?" when there is no lock or no issue in it.
lock_issue() {
  local lock
  lock=$(kube_lock_get 2>/dev/null) || lock=""
  [ -n "$lock" ] || {
    echo '?'
    return 0
  }
  jq -r '.data.issue // "?"' <<<"$lock"
}

# proc_start PID: the process's start time (field 22 of /proc/PID/stat, read after the command name,
# which may hold spaces), so a pid the kernel reused for another process is told apart.
proc_start() { sed -E 's/^.*\) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }

# register_command: this command of RUN (never one of its daemons) is alive: its pid in
# <run>/commands/, holding its start time. The heartbeat renews the lock only while one is.
register_command() {
  [[ $CMD == _* ]] && return 0
  mkdir -p -- "$RUN_DIR/commands"
  proc_start "$$" >"$RUN_DIR/commands/$$"
}

# run_command_alive: the run is being driven: some command of RUN that registered itself is still
# the same live process, or a hold (--keep-for, cluster.sh hold) has not expired yet. The file of
# each command that is not that live process any more is removed.
run_command_alive() {
  local f pid until alive=1
  for f in "$RUN_DIR"/commands/*; do
    [ -f "$f" ] || continue
    pid=${f##*/}
    if [[ $pid =~ ^[0-9]+$ ]] && [ -n "$(<"$f")" ] && [ "$(proc_start "$pid")" = "$(<"$f")" ]; then
      alive=0
    else
      rm -f -- "$f"
    fi
  done
  if ((alive != 0)); then
    until=$(hold_until)
    [ -z "$until" ] || ((EPOCHSECONDS >= until)) || alive=0
  fi
  return "$alive"
}

# The hold: <run>/hold-until, the epoch second a hold of the run ends. run_command_alive calls a
# held run alive, so its heartbeat keeps renewing the lock with no command of it running and the
# copy stays whole for the steps and replays its caller types by hand; after the hold expires an
# abandoned run ages and goes stale as any other. all --keep writes one for --keep-for (default
# KEEP_FOR_DEFAULT), cluster.sh hold sets or extends it, and down removes it.
KEEP_FOR_DEFAULT=2h

# hold_until: the run's hold's end as epoch seconds; empty when it holds none.
hold_until() {
  local until
  until=$(cat -- "$RUN_DIR/hold-until" 2>/dev/null) || return 0
  [[ $until =~ ^[0-9]+$ ]] || return 0
  printf '%s\n' "$until"
}

# hold_set SECONDS: hold the run for SECONDS from now. It fails when the run has no directory here.
hold_set() {
  [ -d "$RUN_DIR" ] || return 1
  printf '%s\n' "$((EPOCHSECONDS + $1))" >"$RUN_DIR/hold-until"
}

# hold_at EPOCH: an epoch second as RFC 3339 UTC.
hold_at() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }

set_run() {
  RUN=$1
  RUN_DIR=$CLUSTER_ROOT/$RUN
  STATE=$RUN_DIR/state.json
  LOADGEN=$RUN_DIR/loadgen
  STORED_ORDER_BIN=$RUN_DIR/envoy-dispatch
  BROWSERTYPE_BIN=$RUN_DIR/browsertype
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
# stale, and then the holder to confirm with before the down command that clears it is named. A
# stale heartbeat is not proof that the run is abandoned: a held run (--keep-for) whose hold
# expired, or a holder whose pod is wedged, still owns the copy its issue is measuring, and the
# down deletes a seeded copy that took an hour to make.
refuse_lock() {
  local pod uid issue run started hb age
  IFS=$'\t' read -r pod uid issue run started hb < <(jq -r '.data | [."holder-pod", ."holder-uid", .issue, .run, .started, .heartbeat] | map(. // "?") | @tsv' <<<"$1")
  age=$(heartbeat_age "$hb")
  note "namespace $NS is locked: ConfigMap $LOCK names run $run, holder pod $pod (uid $uid, issue $issue), started $started, last heartbeat $hb, $age s ago."
  if [[ $age =~ ^[0-9]+$ ]] && ((age > STALE_SECONDS)); then
    note "that heartbeat is older than $((STALE_SECONDS / 60)) min, so the holder is stale: pod $pod, issue $issue."
    note "confirm with issue $issue's holder that run $run is abandoned before you clear it (ask on $issue: a kept run holds its copy for typing steps and replays, and the down below deletes it)."
    note "once they confirm, clear its copy and the lock with: cluster.sh down --namespace $NS --run $run"
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
# still names this run; it ends when the lock is gone or another run's, and when nothing drives the
# run any more (run_command_alive: no registered command of it is alive, and no hold holds it). A
# command that dies without its EXIT trap (killed, an OOM, a worker turn interrupted) then leaves a
# lock that ages, so after 15 min the next up names it stale with the down command; the run's next
# command restarts the heartbeat (ensure_daemons), and the sampler ends with the heartbeat.
cmd__heartbeat() {
  parse "namespace run" "" "$@"
  use_run
  printf '%s\n' "$$" >"$RUN_DIR/heartbeat.pid"
  local patch out lock
  while :; do
    sleep 60
    if ! run_command_alive; then
      note "heartbeat: no command of run $RUN is alive and no hold holds it; stopping, so the lock ages and goes stale"
      rm -f -- "$RUN_DIR/heartbeat.pid"
      exit 0
    fi
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

# SAMPLER_GRACE_CHECKS: the sampler ends by the heartbeat's rule (sampler_wait), so no role loop of
# a killed or torn-down run samples on: every 60 s it looks for a live heartbeat of the run, and
# stops after this many checks in a row with none, a grace of at least 60 s for a heartbeat that
# the run's next command is restarting.
SAMPLER_GRACE_CHECKS=2

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

# The sampler: one role loop per sampled container, each holding its own kubectl exec, until
# sampler_wait says the run is over.
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
  sampler_wait
}

# sampler_wait: hold the role loops while a heartbeat of the run lives, then stop every one of them
# and exit. Without it, a run whose commands and heartbeat are gone (killed, or a down from another
# pod) keeps eight kubectl execs open for ever and appends an error line of each to cgroup.jsonl
# every 5 s. The sampler is a session of its own (start_daemon), so one TERM of its process group
# ends every role loop with its kubectl exec; the pidfile goes first, so the run's next command
# starts a sampler again (ensure_daemons), and TERM is ignored here (the loops were started before
# that, so they still take it) so this process reaches its own exit.
sampler_wait() {
  local misses=0 why=""
  while [ -z "$why" ]; do
    sleep 60
    if [ ! -d "$RUN_DIR" ]; then
      why="the run directory $RUN_DIR is gone"
    elif daemon_alive heartbeat; then
      misses=0
    elif ((++misses >= SAMPLER_GRACE_CHECKS)); then
      why="no heartbeat of run $RUN has been alive for $((SAMPLER_GRACE_CHECKS * 60)) s"
    fi
  done
  note "sampler: $why; stopping every role loop with its kubectl exec, and exiting"
  rm -f -- "$RUN_DIR/sampler.pid"
  trap '' TERM
  kill -TERM -- "-$$" 2>/dev/null || true
  exit 0
}

# ---- the copy -------------------------------------------------------------------------------------

# psql_at DB SQL: one value per line, through psql in the Postgres pod, for SQL that only reads
# (kube_read retries it once on a transient API server error). psql_write DB SQL: the same for
# SQL that writes, never retried.
psql_at() {
  kube_read exec postgres-0 -c postgres -- psql -X -U dispatch -d "$1" -v ON_ERROR_STOP=1 -At -c "$2"
}
psql_write() {
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

# ---- the Postgres data volume -----------------------------------------------------------------

# pg_disk: the Postgres data volume's size, used and free bytes, as one line "SIZE USED FREE",
# from df in the Postgres pod (a read, so kube_read retries a transient API server error). When
# the read or its parse fails it prints why, in one line, and returns 1, so the caller records a
# reading with that error and no number.
pg_disk() {
  local out rc=0
  out=$(kube_read exec postgres-0 -c postgres -- df -B1 --output=size,used,avail "$PG_DATA_MOUNT" 2>&1) || rc=$?
  if ((rc != 0)); then
    printf 'df of %s in postgres-0 failed (exit %s): %s\n' "$PG_DATA_MOUNT" "$rc" "$(tr '\n' ' ' <<<"$out")"
    return 1
  fi
  if awk '$1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ { print $1, $2, $3; found = 1; exit }
    END { exit !found }' <<<"$out"; then
    return 0
  fi
  printf 'df of %s in postgres-0 gave no size, used and free bytes: %s\n' "$PG_DATA_MOUNT" "$(tr '\n' ' ' <<<"$out")"
  return 1
}

# pg_disk_sample FILE: one reading of the volume, into the step's readings FILE (one JSON object
# per line) and into PG_DISK_SIZE, PG_DISK_USED and PG_DISK_FREE. A read that failed is a sample
# with error and no number (never 0), leaves the three empty, is said, and returns 1.
pg_disk_sample() {
  local file=$1 out
  PG_DISK_SIZE="" PG_DISK_USED="" PG_DISK_FREE=""
  if out=$(pg_disk); then
    read -r PG_DISK_SIZE PG_DISK_USED PG_DISK_FREE <<<"$out"
    jq -nc --arg at "$(utc_now)" --argjson size "$PG_DISK_SIZE" --argjson used "$PG_DISK_USED" --argjson free "$PG_DISK_FREE" \
      '{at: $at, size_bytes: $size, used_bytes: $used, free_bytes: $free}' >>"$file"
    return 0
  fi
  jq -nc --arg at "$(utc_now)" --arg why "$out" '{at: $at, error: $why}' >>"$file"
  note "reading the Postgres data volume failed: $out"
  return 1
}

# pg_disk_low: the last reading leaves less than PG_DISK_MIN_FREE_PCT of the volume free. A
# reading that failed holds no number, and is never low: a read that did not happen measures
# nothing.
pg_disk_low() {
  [ -n "$PG_DISK_SIZE" ] && ((PG_DISK_SIZE > 0)) || return 1
  ((PG_DISK_FREE * 100 < PG_DISK_SIZE * PG_DISK_MIN_FREE_PCT))
}

# pg_disk_gib BYTES: BYTES in GiB with one decimal, for a message.
pg_disk_gib() { awk -v b="$1" 'BEGIN { printf "%.1f GiB", b / 1073741824 }'; }

# pg_disk_detail: the last reading as one phrase, for the step failure it is the detail of: the
# volume and its size, what is used and what is free, the threshold, and the flag that gives a
# bigger volume.
pg_disk_detail() {
  printf '%s is %s, %s used, %s free (%s%% of it), under the %s%% (%s) a step needs; up --pg-volume SIZE gives a bigger volume (default %s, at most %sGi)' \
    "$PG_DATA_MOUNT" "$(pg_disk_gib "$PG_DISK_SIZE")" "$(pg_disk_gib "$PG_DISK_USED")" "$(pg_disk_gib "$PG_DISK_FREE")" \
    "$((PG_DISK_FREE * 100 / PG_DISK_SIZE))" "$PG_DISK_MIN_FREE_PCT" "$(pg_disk_gib $((PG_DISK_SIZE * PG_DISK_MIN_FREE_PCT / 100)))" \
    "$PG_VOLUME_DEFAULT" "$PG_VOLUME_MAX_GI"
}

# pg_disk_write DIR FILE: DIR/pg-disk.json from the step's readings FILE: the volume's size, the
# used bytes before and after the load and the highest of them, the least free, every reading with
# its time, and the threshold the step held to. A reading that failed is a sample with error and
# no number, and counts for none of the figures, which are null (never 0) when no reading gave a
# number. FILE is removed once the file is written.
pg_disk_write() {
  local dir=$1 file=$2
  [ -f "$file" ] || return 0
  # shellcheck disable=SC2016 # $read and $pct are jq's variables
  if jq -s --argjson pct "$PG_DISK_MIN_FREE_PCT" '[.[] | select(has("error") | not)] as $read
    | {size_bytes: ($read | last | .size_bytes), used_before: ($read | first | .used_bytes),
      used_after: ($read | last | .used_bytes), used_peak: ($read | map(.used_bytes) | max),
      free_min: ($read | map(.free_bytes) | min), samples: ., threshold_pct: $pct}' "$file" >"$dir/pg-disk.json.tmp"; then
    mv -- "$dir/pg-disk.json.tmp" "$dir/pg-disk.json"
    rm -f -- "$file"
  else
    rm -f -- "$dir/pg-disk.json.tmp"
    note "step ${dir##*/}: summing the Postgres data volume's readings failed; no pg-disk.json"
  fi
}

# pg_disk_watch NAME DIR JOB FILE: a reading of the volume into FILE every PG_DISK_POLL_SECONDS
# while the step's loadgen run JOB runs in the runner. The first reading under the threshold stops
# JOB and the step's browser typing, records the step failed (kind pg_disk) with the reading, and
# ends the watch: Postgres never reaches a full volume with load still arriving, and the step's
# after-work runs as it does for any step. run_in_runner starts it in the background, and
# pg_disk_watch_stop ends it on every path out of the step.
pg_disk_watch() {
  local name=$1 dir=$2 job=$3 file=$4 nap=""
  # Its own TERM ends the nap it is in, so the step leaves no sleep of its own behind.
  trap '[ -z "$nap" ] || kill -- "$nap" 2>/dev/null; exit 0' TERM
  while :; do
    sleep "$PG_DISK_POLL_SECONDS" &
    nap=$!
    wait "$nap" || true
    nap=""
    pg_disk_sample "$file" || continue
    pg_disk_low || continue
    # From here the watcher records and stops; a TERM of a step that ends at this moment
    # (pg_disk_watch_stop) waits for that instead of cutting it in half.
    trap '' TERM
    record_failure "$dir" pg_disk postgres "$(pg_disk_detail); the step's loadgen run was stopped before Postgres filled the volume"
    runner_stop "$job"
    typing_stop "$name" "$dir" || note "step $name: stopping browser typing after the disk watcher fired failed"
    return 0
  done
}

# pg_disk_watch_stop: the step's disk watcher ended and reaped, with the nap it is in; nothing
# when none runs, so every path out of a step may call it.
pg_disk_watch_stop() {
  local pid=$PG_DISK_WATCHER
  [ -n "$pid" ] || return 0
  PG_DISK_WATCHER=""
  kill -- "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# nats_box ARGS: the nats CLI in the run's nats-box pod: NATS_BOX_POD when reset_copy has looked it
# up, else the one running now.
nats_box() {
  local pod=${NATS_BOX_POD:-}
  if [ -z "$pod" ]; then
    pod=$(find_pod nats-box) || die "kubectl could not list the nats-box pod"
    [ -n "$pod" ] || die "no running nats-box pod in $NS"
  fi
  # The server named explicitly, so no nats-box context decides which NATS this purges. A listing
  # or an info only reads, so kube_read retries it; a removal is never retried.
  case "${1:-} ${2:-}" in
    "stream info" | "stream ls" | "kv ls") kube_read exec "$pod" -c nats-box -- nats --server "nats://nats.$NS.svc:4222" "$@" ;;
    *) kube exec "$pod" -c nats-box -- nats --server "nats://nats.$NS.svc:4222" "$@" ;;
  esac
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
  psql_write postgres 'drop database if exists dispatch with (force)' >/dev/null || die "drop database dispatch failed"
  psql_write postgres "create database dispatch template $template" >/dev/null || die "create database dispatch from $template failed"
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
  got=$(kube_read exec runner -c runner -- sh -c 'test "$1" "$2" && sha256sum "$2"' sh "$3" "$2" 2>/dev/null) || return 1
  [ "${got%% *}" = "$(sha256_of "$1")" ]
}

# put_executable FILE DEST: FILE into the runner as DEST, executable, unless the runner holds it
# (runner_has: only when its sha256 differs there).
put_executable() {
  if runner_has "$1" "$2" -x; then return 0; fi
  runner_put "$1" "$2"
  kube exec runner -c runner -- chmod 755 "$2" || die "chmod of $2 in the runner failed"
}

# put_loadgen: the run's loadgen into the runner as /work/loadgen (put_executable).
put_loadgen() { put_executable "$LOADGEN" /work/loadgen; }

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

# runner_stop NAME: the runner job NAME stopped, by the pid it recorded. It never fails: the job
# may have ended already. runner_wait's bound and the disk watcher both end a job this way.
runner_stop() {
  # shellcheck disable=SC2016 # expanded by the runner's sh
  kube exec runner -c runner -- sh -c 'kill "$(cat "/work/logs/$1.pid")"' sh "$1" >/dev/null 2>&1 || true
}

# runner_wait NAME SECONDS [LOG_DIR]: NAME's exit status once it has one; past SECONDS it stops
# NAME and prints "timeout". Its log is copied to LOG_DIR (default the run's logs/).
runner_wait() {
  local name=$1 deadline=$((EPOCHSECONDS + $2)) log_dir=${3:-$RUN_DIR/logs} rc
  while :; do
    if rc=$(kube_read exec runner -c runner -- cat "/work/logs/$name.rc" 2>/dev/null) && [[ $rc =~ ^[0-9]+$ ]]; then
      break
    fi
    if ((EPOCHSECONDS >= deadline)); then
      runner_stop "$name"
      rc=timeout
      break
    fi
    sleep 15
  done
  kube_read exec runner -c runner -- cat "/work/logs/$name.log" >"$log_dir/$name.log" 2>/dev/null || true
  printf '%s\n' "$rc"
}

# commit_source FLAG COMMIT PATH WHY: refuse a --FLAG commit that this checkout's git store (which
# `jj git fetch` fills) does not hold, or whose tree has no PATH (a file or a directory), naming
# it; WHY says what a tree without PATH lacks.
commit_source() {
  command -v git >/dev/null || die "up: --$1 needs git on PATH"
  git -C "$REPO_DIR" cat-file -e "$2^{commit}" 2>/dev/null ||
    die "up: --$1 $2 is not a commit in this checkout's git store ($REPO_DIR); fetch it (jj git fetch) or name another"
  git -C "$REPO_DIR" cat-file -e "$2:$3" 2>/dev/null ||
    die "up: --$1 $2 has no $3, $4; name a commit that has it"
}

# tree_has COMMIT PATH: COMMIT's tree in this checkout's git store holds PATH.
tree_has() { git -C "$REPO_DIR" cat-file -e "$1:$2" 2>/dev/null; }

# build_at_commit COMMIT PACKAGE OUT [SOURCE]: a static binary of COMMIT's Go package PACKAGE (a
# path under packages/envoy, as ./cmd/dispatch) at OUT (an absolute path): `git archive` of this
# checkout's git store unpacked in a scratch directory beside OUT, with SOURCE's stored-order
# command added to that scratch copy when SOURCE is given (stored_order_overlay), built there
# outside any workspace, then the scratch directory removed. It builds the stored-order checker
# and browser typing's browsertype. The overlay's and go's errors go to standard error.
build_at_commit() {
  local commit=$1 pkg=$2 out=$3 source=${4:-} src rc=0
  src=$(mktemp -d "${out%/*}/.build-src.XXXXXX") || return 1
  { git -C "$REPO_DIR" archive "$commit" packages/envoy | tar -C "$src" -xf - &&
    { [ -z "$source" ] || stored_order_overlay "$src" "$commit" "$source"; } &&
    (cd -- "$src/packages/envoy" && CGO_ENABLED=0 GOWORK=off go build -buildvcs=false -o "$out" "$pkg"); } || rc=$?
  rm -rf -- "$src"
  return "$rc"
}

# stored_order_entry SOURCE: the stored-order entry of SOURCE's subcommand table
# (STORED_ORDER_REGISTRY), from its line that starts `<tab>{"stored-order", func(` through the
# first `<tab>}},` line after it. Fails when the table holds no such entry, or more than one.
stored_order_entry() {
  local table
  table=$(git -C "$REPO_DIR" cat-file -p "$1:$STORED_ORDER_REGISTRY" 2>/dev/null) || return 1
  awk '/^\t\{"stored-order", func\(/ { n++; on = 1 } on { print } on && $0 == "\t}}," { on = 0; closed++ }
    END { exit !(n == 1 && closed == 1) }' <<<"$table"
}

# stored_order_register FILE ENTRY COMMIT: ENTRY (stored_order_entry's) inserted into FILE, the
# scratch copy of dispatch commit COMMIT's STORED_ORDER_REGISTRY, on the line after the table's
# opener STORED_ORDER_ANCHOR, in place. Refuses, saying why on standard error, a FILE that is
# missing, that holds the opener on other than exactly one line, or that registers stored-order
# already, and checks that the result registers it on exactly one line.
stored_order_register() {
  local file=$1 entry=$2 commit=$3 n opener=$'^\t{"stored-order", func('
  if [ ! -f "$file" ]; then
    printf 'the overlay: dispatch commit %s has no %s, the subcommand table that registers stored-order\n' "$commit" "$STORED_ORDER_REGISTRY" >&2
    return 1
  fi
  n=$(grep -cxF -- "$STORED_ORDER_ANCHOR" "$file") || true
  if ((n != 1)); then
    printf "the overlay: dispatch commit %s's %s holds the registration's anchor \`%s\` on %s lines, not 1, so the overlay cannot register stored-order there\n" \
      "$commit" "$STORED_ORDER_REGISTRY" "$STORED_ORDER_ANCHOR" "$n" >&2
    return 1
  fi
  if grep -q -- "$opener" "$file"; then
    printf "the overlay: dispatch commit %s's %s registers stored-order already\n" "$commit" "$STORED_ORDER_REGISTRY" >&2
    return 1
  fi
  ENTRY=$entry awk -v anchor="$STORED_ORDER_ANCHOR" '{ print } $0 == anchor { print ENVIRON["ENTRY"] }' "$file" >"$file.tmp" &&
    mv -- "$file.tmp" "$file" || return 1
  n=$(grep -c -- "$opener" "$file") || true
  if ((n != 1)); then
    printf "the overlay: dispatch commit %s's %s registers stored-order on %s lines after the edit, not 1\n" "$commit" "$STORED_ORDER_REGISTRY" "$n" >&2
    return 1
  fi
}

# stored_order_overlay DIR COMMIT SOURCE: SOURCE's stored-order command added to DIR, the scratch
# copy of dispatch commit COMMIT's archive: STORED_ORDER_FILES copied from SOURCE's tree, and
# SOURCE's stored-order entry registered in DIR's STORED_ORDER_REGISTRY (stored_order_register).
# Refuses, saying why on standard error, a SOURCE without the entry and a registry without the
# anchor.
stored_order_overlay() {
  local dir=$1 commit=$2 source=$3 entry
  if ! entry=$(stored_order_entry "$source"); then
    printf "the overlay: source commit %s's %s holds no single stored-order entry to register\n" "$source" "$STORED_ORDER_REGISTRY" >&2
    return 1
  fi
  if ! git -C "$REPO_DIR" archive "$source" "${STORED_ORDER_FILES[@]}" | tar -C "$dir" -xf -; then
    printf 'the overlay: git archive of %s from source commit %s failed\n' "${STORED_ORDER_FILES[*]}" "$source" >&2
    return 1
  fi
  stored_order_register "$dir/$STORED_ORDER_REGISTRY" "$entry" "$commit"
}

# stored_order_args COMMIT SOURCE CHECKER: refuse, before up creates anything, stored-order
# flags from which up cannot build a checker for dispatch commit COMMIT: COMMIT not in this
# checkout's git store; with CHECKER (--stored-order-checker-commit), a SOURCE too, CHECKER not
# 40 hex, COMMIT itself, or not a commit whose tree has the command; a COMMIT tree with the
# command and a SOURCE; a COMMIT tree without it and no SOURCE, or a SOURCE that is not 40 hex or
# whose tree lacks STORED_ORDER_FILES or a stored-order entry in STORED_ORDER_REGISTRY.
stored_order_args() {
  local commit=$1 source=$2 checker=$3 f
  git -C "$REPO_DIR" cat-file -e "$commit^{commit}" 2>/dev/null ||
    die "up: --dispatch-commit $commit is not a commit in this checkout's git store ($REPO_DIR), and the stored-order checker is built from its tree; fetch it (jj git fetch)"
  if [ -n "$checker" ]; then
    [ -z "$source" ] || die "up: --stored-order-source and --stored-order-checker-commit exclude each other: the source adds INFRA-707's files to the dispatch tree, the checker commit replaces that tree; give one"
    valid commit "$checker" || die "up: --stored-order-checker-commit '$checker' is not a full 40-hex commit sha"
    [ "$checker" != "$commit" ] || die "up: --stored-order-checker-commit $checker is --dispatch-commit itself; drop it: the checker is built from the dispatch commit by default"
    commit_source stored-order-checker-commit "$checker" "$STORED_ORDER_COMMAND" "so its envoy-dispatch has no stored-order command (INFRA-707)"
  elif tree_has "$commit" "$STORED_ORDER_COMMAND"; then
    [ -z "$source" ] || die "up: --stored-order-source $source is refused: --dispatch-commit $commit's tree has the stored-order command ($STORED_ORDER_COMMAND), so the checker is built from that tree alone"
  else
    [ -n "$source" ] || die "up: --dispatch-commit $commit's tree has no stored-order command ($STORED_ORDER_COMMAND); give --stored-order-source <commit>, whose INFRA-707 files (${STORED_ORDER_FILES[*]}) and stored-order registration are added to it, or, naming the renderer mismatch, --stored-order-checker-commit <commit> to build the checker wholly from another commit"
    valid commit "$source" || die "up: --stored-order-source '$source' is not a full 40-hex commit sha"
    for f in "${STORED_ORDER_FILES[@]}" "$STORED_ORDER_REGISTRY"; do
      commit_source stored-order-source "$source" "$f" "so it lacks INFRA-707's stored-order command"
    done
    stored_order_entry "$source" >/dev/null ||
      die "up: --stored-order-source $source's $STORED_ORDER_REGISTRY holds no single stored-order entry (a line starting {\"stored-order\", func( and the }}, that closes it); name a commit that registers the command"
  fi
}

# stored_order_build COMMIT SOURCE CHECKER: the stored-order checker, a static envoy-dispatch, at
# CHECKER_TMP/envoy-dispatch (a scratch directory in CLUSTER_ROOT, since the run directory does
# not exist yet), and its state.json stored_order object in STORED_ORDER_STATE: built from
# dispatch commit COMMIT's tree, with SOURCE's overlay when SOURCE is given, or wholly from
# CHECKER when it is given, after a warning that its renderer may differ from Dispatch's
# (renderer_matches false). stored_order_args checked the flags. Dies, before up creates
# anything, when it does not build, after the overlay's and go's whole output: naming the dispatch
# commit, the source commit (or none), the checker commit when given, and the first error.
stored_order_build() {
  local commit=$1 source=$2 checker=$3 from what err first files='[]' registry="" matches=true
  (umask 077 && mkdir -p -- "$CLUSTER_ROOT") || die "up: cannot create $CLUSTER_ROOT"
  CHECKER_TMP=$(mktemp -d "$CLUSTER_ROOT/.checker.XXXXXX") || die "up: cannot create a scratch directory in $CLUSTER_ROOT"
  err=$CHECKER_TMP/build.err
  if [ -n "$checker" ]; then
    from=$checker matches=false what="checker commit $checker, not dispatch commit $commit"
    note "WARNING: --stored-order-checker-commit $checker: the stored-order checker is built wholly from $checker, not from dispatch commit $commit, so its renderer may differ from Dispatch's and a mismatch it reports may be renderer skew; state.json records renderer_matches false"
  elif [ -n "$source" ]; then
    from=$commit what="dispatch commit $commit with INFRA-707's files of source commit $source (${STORED_ORDER_FILES[*]}, registered in $STORED_ORDER_REGISTRY)"
    files=$(printf '%s\n' "${STORED_ORDER_FILES[@]}" | jq -Rnc '[inputs]')
    registry=$STORED_ORDER_REGISTRY
  else
    from=$commit what="dispatch commit $commit"
  fi
  note "building the stored-order checker, envoy-dispatch (static) of $what"
  if ! build_at_commit "$from" ./cmd/dispatch "$CHECKER_TMP/envoy-dispatch" "$source" 2>"$err"; then
    cat -- "$err" >&2
    first=$(grep -v -m1 -e '^#' -e '^[[:space:]]*$' "$err") || first="go build failed with no error line"
    die "up: the stored-order checker does not build (dispatch commit $commit, source commit ${source:-none}${checker:+, checker commit $checker}): $first"
  fi
  # shellcheck disable=SC2016 # $d, $b, $s, $f, $r, $m and $sha are jq's variables
  STORED_ORDER_STATE=$(jq -nc --arg d "$commit" --arg b "$from" --arg s "$source" --argjson f "$files" --arg r "$registry" \
    --argjson m "$matches" --arg sha "$(sha256_of "$CHECKER_TMP/envoy-dispatch")" \
    '{dispatch_commit: $d, built_from: $b, overlay_source: (if $s == "" then null else $s end), overlay_files: $f,
      registration: (if $r == "" then null else $r end), renderer_matches: $m, sha256: $sha}')
  note "the stored-order checker's sha256 is $(jq -r .sha256 <<<"$STORED_ORDER_STATE"), renderer_matches $matches"
}

# checker_tmp_remove: up's scratch directory for the checker removed (an exit trap's part).
checker_tmp_remove() {
  [ -z "$CHECKER_TMP" ] || rm -rf -- "$CHECKER_TMP"
  CHECKER_TMP=""
}

# ---- the stored-order check -----------------------------------------------------------------------

# The runner's script: DATABASE_URL from its standard input, so the password is on no command line
# and in no file, then stored-order since $2 with the flags after it (a --artifact of each typed
# document, or none for the run over every document), bounded at $1 seconds (timeout's exit
# status 124).
# shellcheck disable=SC2016 # expanded by the runner's sh
STORED_ORDER_SCRIPT='IFS= read -r DATABASE_URL; export DATABASE_URL; seconds=$1; since=$2; shift 2; exec timeout "$seconds" /work/envoy-dispatch stored-order --since "$since" "$@"'

# stored_order_summary TXT CHECKER EXIT SINCE: stored-order.json from the command's output TXT:
# the checker's fields (CHECKER, the JSON object stored_order_checker gives: dispatch_commit,
# built_from, overlay_source, overlay_files, registration, renderer_matches, sha256), its exit
# status (null when it did not run), --since (null when none), and the counts of the last summary
# line (`stored-order: documents=N clean=N mismatched=N refused=N failed=N`, which INFRA-707's
# checker from eb0fe0be on ends with ` unchecked=N`, a document it compared no update and no version
# of), each null when there is no such line or the command timed out (exit 124), never 0; unchecked
# is null too for a line without it.
stored_order_summary() {
  jq -Rn --argjson checker "$2" --argjson exit "${3:-null}" --arg since "$4" '
    ([inputs | capture("^stored-order: documents=(?<documents>[0-9]+) clean=(?<clean>[0-9]+) mismatched=(?<mismatched>[0-9]+) refused=(?<refused>[0-9]+) failed=(?<failed>[0-9]+)( unchecked=(?<unchecked>[0-9]+))?$")] | last) as $s
    | $checker + {exit: $exit, since: (if $since == "" then null else $since end)}
      + (if $s == null or $exit == 124 then {documents: null, clean: null, mismatched: null, refused: null, failed: null, unchecked: null}
        else $s | map_values(if . == null or . == "" then null else tonumber end) end)' <"$1"
}

# stored_order_checker: state.json's stored_order (the run's checker, stored_order_build) as one
# line of JSON, its renderer_matches false when Dispatch now runs another commit than the one the
# checker was built for (a redeploy); empty when state.json records no checker.
stored_order_checker() {
  jq -c '.dispatch_commit as $running | .stored_order // empty
    | .renderer_matches = (.renderer_matches and .dispatch_commit == $running)' "$STATE"
}

# The jq function that names a checker (stored_order_checker's object) in one phrase.
# shellcheck disable=SC2016 # jq's program
STORED_ORDER_LABEL='def checker_label: if .overlay_source != null
    then "dispatch commit \(.built_from[0:12]) + INFRA-707 files of \(.overlay_source[0:12])"
  elif .built_from == .dispatch_commit then "dispatch commit \(.built_from[0:12])"
  else "checker commit \(.built_from[0:12]), not dispatch commit \(.dispatch_commit[0:12])" end
  + ", renderer_matches \(.renderer_matches)";'

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

# stored_order_psql SECONDS OUT DB SQL...: psql in the Postgres pod on database DB, each SQL a -c
# (a psql \copy runs there and writes to its standard output) after one that sets the session's
# statement_timeout to SECONDS, that output streamed back through kubectl exec into OUT, bounded at
# SECONDS. The statement_timeout ends a psql that the bound's kill of kubectl leaves running in the
# pod, so that none holds the template database the next reset copies. Nothing is written in the
# pod. psql's standard error goes to OUT.err, removed when psql succeeds. Returns kubectl's exit
# status (124 when the bound ran out).
stored_order_psql() {
  local seconds=$1 out=$2 db=$3 sql rc=0 args=()
  shift 3
  args=(-c "set statement_timeout = '${seconds}s'")
  for sql in "$@"; do args+=(-c "$sql"); done
  kube_for "$seconds" exec postgres-0 -c postgres -- psql -X -q -U dispatch -d "$db" -v ON_ERROR_STOP=1 "${args[@]}" \
    </dev/null >"$out" 2>"$out.err" || rc=$?
  ((rc != 0)) || rm -f -- "$out.err"
  return "$rc"
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

# stored_order_csv FILE DB SQL: one CSV of stored_order_export, whose locals out, deadline, files
# and errors it reads and sets: the \copy of SQL to stdout with a header line, on database DB,
# bounded by what is left of deadline, into OUT/FILE gzipped as OUT/FILE.gz, and its row count into
# files under FILE.gz; or, when it fails or no time is left, why into errors.
stored_order_csv() {
  local name=$1.gz file=$out/$1 db=$2 sql=$3 rows left rc=0
  left=$((deadline - EPOCHSECONDS))
  if ((left <= 0)); then
    errors+=("$name: not exported: the export's $STORED_ORDER_EVIDENCE_SECONDS s ran out")
    return 0
  fi
  stored_order_psql "$left" "$file" "$db" "\\copy ($sql) to stdout with csv header" || rc=$?
  if ((rc != 0)); then
    errors+=("$name: $(stored_order_failure "$file" "$rc")")
  elif rows=$(csv_rows "$file") && gzip -f -- "$file"; then
    files=$(jq -c --arg f "$name" --argjson n "$rows" '. + {($f): $n}' <<<"$files")
  else
    errors+=("$name: counting or compressing its rows failed")
    rm -f -- "$file" "$file.gz"
  fi
}

# stored_order_export ID OUT DEADLINE SINCE TEMPLATE VERSION...: one document's rows, each psql call
# bounded by what is left of DEADLINE (epoch seconds), into OUT as gzipped CSVs (a header line,
# then every row): from the copy's database dispatch, <table>.csv.gz for each table of
# STORED_ORDER_TABLES and issue-events-in-window.csv.gz, the events of the document or of its
# issue created since SINCE (the step's window_start), by seq; version-<n>.md, each mismatched
# VERSION's stored markdown, byte for byte, when artifact_versions holds one; and from database
# TEMPLATE, which the step was reset from and so holds the document as it was before the step,
# start-<table>.csv.gz for each table of STORED_ORDER_START_TABLES (none without TEMPLATE). Prints
# one JSON object: artifact, files (each exported CSV's row count, by file name), versions (the
# version-<n>.md written) and, when a part failed, error (each failed part, with why).
stored_order_export() {
  local id=$1 out=$2 deadline=$3 since=$4 template=$5 spec table key order file left rc n files='{}' errors=() written=()
  shift 5
  mkdir -p -- "$out" || return 1
  for spec in "${STORED_ORDER_TABLES[@]}"; do
    IFS=: read -r table key order <<<"$spec"
    stored_order_csv "$table.csv" dispatch "select * from $table where $key = '$id'${order:+ order by $order}"
  done
  stored_order_csv issue-events-in-window.csv dispatch \
    "select e.* from events e where (e.artifact_id = '$id' or e.issue_key = (select issue_key from artifacts where id = '$id')) and e.created_at >= '$since' order by e.seq"
  for n in "$@"; do
    file=$out/.version-$n.base64
    left=$((deadline - EPOCHSECONDS))
    if ((left <= 0)); then
      errors+=("version $n: not exported: the export's $STORED_ORDER_EVIDENCE_SECONDS s ran out")
      continue
    fi
    rc=0
    # No row when the version holds no markdown; base64, so the markdown's bytes come back as stored.
    stored_order_psql "$left" "$file" dispatch "\\pset tuples_only on" "\\pset format unaligned" \
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
  for spec in "${STORED_ORDER_START_TABLES[@]}"; do
    IFS=: read -r table key order <<<"$spec"
    if [ -z "$template" ]; then
      errors+=("start-$table.csv.gz: not exported: the step's reset.json names no template database")
      continue
    fi
    stored_order_csv "start-$table.csv" "$template" "select * from $table where $key = '$id'${order:+ order by $order}"
  done
  jq -nc --arg a "$id" --argjson f "$files" \
    --argjson e "$(printf '%s\n' "${errors[@]}" | jq -Rsc 'split("\n") | map(select(. != ""))')" \
    '{artifact: $a, files: $f, versions: ($ARGS.positional | map(tonumber))} + (if $e == [] then {} else {error: $e} end)' \
    --args "${written[@]}"
}

# stored_order_evidence NAME TXT DIR SINCE: before the next reset drops the database, the rows of
# each document that stored-order's output TXT names as mismatched or refused (stored_order_flagged),
# with its issue's events since SINCE and its rows in the template database that DIR/reset.json
# names (dispatch_seed or the replay's), exported into DIR/stored-order-evidence/<artifact>/
# (stored_order_export), all within STORED_ORDER_EVIDENCE_SECONDS. Prints the JSON array of what
# was exported (stored-order.json's evidence) and one line naming the documents and each failure. A
# failure never fails the step. Nothing is uploaded: pack_tar leaves stored-order-evidence out of
# the tar parts.
stored_order_evidence() {
  local name=$1 txt=$2 out=$3/stored-order-evidence since=$4 template deadline entry why entries=() flagged=() words=() line
  rm -rf -- "$out"
  mapfile -t flagged < <(stored_order_flagged "$txt")
  if ((${#flagged[@]} == 0)); then
    echo '[]'
    return 0
  fi
  template=$(jq -r '.template // empty' "$3/reset.json" 2>/dev/null || true)
  case $template in
    dispatch_seed | "$REPLAY_TEMPLATE") ;;
    *) template="" ;;
  esac
  deadline=$((EPOCHSECONDS + STORED_ORDER_EVIDENCE_SECONDS))
  for line in "${flagged[@]}"; do
    read -ra words <<<"$line"
    entry=$(stored_order_export "${words[0]}" "$out/${words[0]}" "$deadline" "$since" "$template" "${words[@]:1}") ||
      entry=$(jq -nc --arg a "${words[0]}" '{artifact: $a, files: {}, versions: [], error: ["the export did not complete"]}')
    entries+=("$entry")
    why=$(jq -r '(.error // []) | join("; ")' <<<"$entry")
    [ -z "$why" ] || note "step $name: the stored-order evidence of ${words[0]} is incomplete: $why"
  done
  note "step $name: the stored-order evidence of ${#flagged[@]} mismatched or refused documents is in $out (kept on this pod, not uploaded)"
  printf '%s\n' "${entries[@]}" | jq -sc .
}

# stored_order_run SECONDS SINCE TXT ARG...: one stored-order command in the runner, bounded at
# SECONDS, with the copy's DATABASE_URL on its standard input and the ARGs (one --artifact of each
# typed document, or none for the run over every document) after --since SINCE. Its standard
# output and error go into TXT, a timed-out run's line is appended, and its exit status is
# printed.
stored_order_run() {
  local seconds=$1 since=$2 txt=$3 rc=0
  shift 3
  printf '%s\n' "$(pg_url)" | kube_for $((seconds + 60)) exec -i runner -c runner -- \
    sh -c "$STORED_ORDER_SCRIPT" sh "$seconds" "$since" "$@" >"$txt" 2>&1 || rc=$?
  ((rc != 124)) || printf 'stored-order: timed out after %s s (exit 124)\n' "$seconds" >>"$txt"
  printf '%s\n' "$rc"
}

# stored_order_note NAME WHAT JSON TXT WHY: one line of what a stored-order command found: the
# step, WHAT (the command, named), the checker (STORED_ORDER_LABEL), --since and the counts of the
# summary JSON, or WHY it did not run. A non-zero exit points at TXT as a correctness finding,
# never a step failure.
stored_order_note() {
  note "$(jq -r --arg name "$1" --arg what "$2" --arg txt "$4" --arg why "$5" "$STORED_ORDER_LABEL"' def n: if . == null then "null" else tostring end;
    "step \($name): \($what) (envoy-dispatch of \(checker_label)) since \(.since | n): "
    + if .exit == null then "not run: \($why)"
      else "exit \(.exit)\(if .exit == 124 then " (timed out)" else "" end), documents=\(.documents | n) clean=\(.clean | n) mismatched=\(.mismatched | n) refused=\(.refused | n) failed=\(.failed | n) unchecked=\(.unchecked | n)"
        + (if .exit == 0 then "" else "; a correctness finding, not a step failure: \($txt)" end) end' "$3")"
}

# stored_order_check NAME DIR: the stored-order check of step NAME (see the header) after its
# loadgen run, while Dispatch still runs. A step that typed (TYPING_LIST) is checked twice, both
# calls within STORED_ORDER_SECONDS: first one call with a --artifact of each typed document, into
# DIR/stored-order-typed.txt and DIR/stored-order-typed.json, so the typed documents' result
# exists even when the run over every document then times out; then that run, into
# DIR/stored-order.txt and DIR/stored-order.json (stored_order_summary with the run's checker, and
# evidence: stored_order_evidence's export of each mismatched or refused document, made here,
# before the next reset drops the database), with the bound's rest. Each call prints one line
# naming the checker and the counts. A non-zero exit is a finding, never the step's failure.
# Browser typing, when the step runs it, stops right after the last command (typing_stop). A run
# whose state.json records no checker (up builds one for every run) says so in one line and runs
# nothing.
stored_order_check() {
  local name=$1 dir=$2 checker since deadline left id evidence rc=0 why="" typed=() txt=$2/stored-order.txt
  checker=$(stored_order_checker)
  if [ -z "$checker" ]; then
    note "step $name: the stored-order check is not run: $STATE records no stored-order checker"
    return 0
  fi
  since=$(jq -r '.window_start // empty' "$dir/meta.json" 2>/dev/null || true)
  if valid rfc3339 "$since"; then
    put_executable "$STORED_ORDER_BIN" /work/envoy-dispatch
    left=$STORED_ORDER_SECONDS
    if [ -n "$TYPING" ] && ((${#TYPING_LIST[@]})); then
      deadline=$((EPOCHSECONDS + STORED_ORDER_SECONDS))
      for id in "${TYPING_LIST[@]}"; do typed+=(--artifact "$id"); done
      rc=$(stored_order_run "$STORED_ORDER_SECONDS" "$since" "$dir/stored-order-typed.txt" "${typed[@]}")
      stored_order_summary "$dir/stored-order-typed.txt" "$checker" "$rc" "$since" >"$dir/stored-order-typed.json"
      stored_order_note "$name" "stored-order over the ${#TYPING_LIST[@]} typed documents" \
        "$dir/stored-order-typed.json" "$name/stored-order-typed.txt" ""
      rc=0
      left=$((deadline - EPOCHSECONDS))
    fi
    if ((left > 0)); then
      rc=$(stored_order_run "$left" "$since" "$txt")
    else
      why="the typed documents' check used the whole bound of $STORED_ORDER_SECONDS s"
      printf 'stored-order: not run: %s\n' "$why" >"$txt"
      rc=null
    fi
  else
    why="meta.json holds no window_start"
    printf 'stored-order: not run: %s/meta.json holds no RFC 3339 window_start\n' "$name" >"$txt"
    since="" rc=null
  fi
  typing_stop "$name" "$dir"
  stored_order_summary "$txt" "$checker" "$rc" "$since" >"$dir/stored-order.json"
  stored_order_note "$name" stored-order "$dir/stored-order.json" "$name/stored-order.txt" "$why"
  evidence=$(stored_order_evidence "$name" "$txt" "$dir" "$since") || {
    note "step $name: the stored-order evidence export did not complete"
    evidence='[]'
  }
  jq --argjson evidence "$evidence" '. + {evidence: $evidence}' "$dir/stored-order.json" >"$dir/stored-order.json.tmp"
  mv -- "$dir/stored-order.json.tmp" "$dir/stored-order.json"
}

# stored_order_table DIR...: <run>/STORED-ORDER.md: the checker the run's up built (state.json
# stored_order: the dispatch commit it was built for, the commit whose tree was built, the
# overlay's source commit and files, renderer_matches and the binary's sha256), then one row per
# step directory in the order given (the run's) from its stored-order.json, "not run" for a step
# without one. A count that no summary line gave is null, never 0. Renderer matches is the step's
# renderer_matches (false after a redeploy to another commit). The five Typed columns are the
# counts of the step's own typed call over its typed documents alone (stored-order-typed.json),
# none for a step that typed into no document. The Typing column is the keystroke updates browser
# typing sent during the step (browsertype.json sent, null when its output held no peer line), or
# none for a step without --typing.
stored_order_table() {
  local bt d typing typed out=$RUN_DIR/STORED-ORDER.md
  bt=$(state '.browsertype.commit // empty')
  {
    printf '# Stored order\n\n'
    jq -r '.stored_order | if . == null then "No step ran INFRA-707'\''s oracle `envoy-dispatch stored-order`: state.json records no stored-order checker."
      else "INFRA-707'\''s oracle `envoy-dispatch stored-order` ran on each step after its window, while Dispatch still ran, with `--since` the window start. Exit 1 (a mismatch, a refused document, a failed read, or a document the checker compared no update and no version of) is a correctness finding, not a failed step; `<step>/stored-order.txt` holds its lines. Unchecked counts those documents; it is `null` for a checker older than INFRA-707'\''s eb0fe0be, whose summary line has no such count.\n\n"
        + "The checker was built for dispatch commit \(.dispatch_commit) "
        + (if .overlay_source != null then "from that commit'\''s tree with INFRA-707'\''s files of source commit \(.overlay_source) added (\(.overlay_files | map("`\(.)`") | join(", ")); its stored-order entry registered in `\(.registration)`)"
          elif .built_from == .dispatch_commit then "from that commit'\''s own tree (no overlay)"
          else "wholly from checker commit \(.built_from) (`--stored-order-checker-commit`), whose renderer may differ from Dispatch'\''s: a mismatch may be renderer skew" end)
        + ". renderer_matches: \(.renderer_matches). Binary sha256 \(.sha256)." end' "$STATE" || return 1
    printf '\n'
    if [ -n "$bt" ]; then
      printf "Typing: the keystroke updates that INFRA-707's \`browsertype\`, built from commit %s, sent into the step's documents from its warm-up until the check ended (\`<step>/browsertype.json\`); \`none\` for a step run without \`--typing\`.\n\n" "$bt"
    else
      printf "Typing: \`none\` for every step: up was given no \`--browsertype-commit\`.\n\n"
    fi
    printf "Typed: the counts of the call that runs first over the step's typed documents alone (\`stored-order --since <window start> --artifact <id>\`, \`<step>/stored-order-typed.json\`), so their result stands even when the run over every document then times out; \`none\` for a step that typed into no document.\n\n"
    printf '| Step | Since | Exit | Documents | Clean | Mismatched | Refused | Failed | Unchecked | Typed documents | Typed clean | Typed mismatched | Typed refused | Typed failed | Typed unchecked | Renderer matches | Typing |\n'
    printf '| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n'
    for d in "$@"; do
      typing=none typed='none | none | none | none | none | none'
      if [ -f "$d/browsertype.json" ]; then
        typing=$(jq -r '.sent | if . == null then "null" else tostring end' "$d/browsertype.json") || return 1
      fi
      if [ -f "$d/stored-order-typed.json" ]; then
        typed=$(jq -r 'def n: if . == null then "null" else tostring end;
          "\(.documents | n) | \(.clean | n) | \(.mismatched | n) | \(.refused | n) | \(.failed | n) | \(.unchecked | n)"' "$d/stored-order-typed.json") || return 1
      fi
      if [ -f "$d/stored-order.json" ]; then
        jq -r --arg step "${d##*/}" --arg typed "$typed" --arg typing "$typing" 'def n: if . == null then "null" else tostring end;
          "| \($step) | \(.since | n) | \(if .exit == null then "not run" else .exit | tostring end) | \(.documents | n) | \(.clean | n) | \(.mismatched | n) | \(.refused | n) | \(.failed | n) | \(.unchecked | n) | \($typed) | \(.renderer_matches | n) | \($typing) |"' \
          "$d/stored-order.json" || return 1
      else
        printf '| %s |  | not run |  |  |  |  |  |  | %s |  | %s |\n' "${d##*/}" "$typed" "$typing"
      fi
    done
  } >"$out.tmp" || return 1
  mv -- "$out.tmp" "$out"
}

# ---- browser typing -------------------------------------------------------------------------------

# typing_parse: a step's or a replay's --typing flags from OPT, into TYPING (1 with --typing, else
# empty), TYPING_PEERS, TYPING_RATE and TYPING_DOCS; refused, before anything is touched, when a
# value has the wrong shape, when a --typing-* flag comes without --typing, or when the run has
# no browsertype (up was given no --browsertype-commit). Called after use_run.
typing_parse() {
  local f
  TYPING="" TYPING_PLANNED=null
  if [ -z "${OPT[typing]:-}" ]; then
    for f in typing-peers typing-rate typing-docs; do
      [ -z "${OPT[$f]+set}" ] || die "$CMD: --$f needs --typing"
    done
    return 0
  fi
  TYPING_PEERS=${OPT[typing-peers]:-2} TYPING_RATE=${OPT[typing-rate]:-5} TYPING_DOCS=${OPT[typing-docs]:-20}
  [[ $TYPING_PEERS =~ ^[1-9][0-9]{0,2}$ ]] || die "$CMD: --typing-peers '$TYPING_PEERS' is not a whole number from 1 to 999"
  if ! [[ $TYPING_RATE =~ ^(0|[1-9][0-9]{0,3})(\.[0-9]{1,3})?$ ]] || ! awk -v r="$TYPING_RATE" 'BEGIN { exit !(r > 0) }'; then
    die "$CMD: --typing-rate '$TYPING_RATE' is not a number of keystroke updates per second above 0 (for example 5 or 2.5)"
  fi
  [[ $TYPING_DOCS =~ ^[1-9][0-9]{0,3}$ ]] || die "$CMD: --typing-docs '$TYPING_DOCS' is not a whole number from 1 to 9999"
  [ -n "$(state '.browsertype.commit // empty')" ] ||
    die "$CMD: --typing needs INFRA-707's browsertype, and up was given no --browsertype-commit; bring the copy up with --browsertype-commit <sha>"
  TYPING=1
}

# TYPING_MIN_VERSIONS is the versions INFRA-707's acceptance 2 needs of every typed document (at
# least 100 updates and at least 10 versions checked); typing_plan_step warns under it.
TYPING_MIN_VERSIONS=10

# typing_plan_step NAME SEED_JSON SESSIONS K EVENTS WARMUP DURATION RNG_SEED: a load step's typing
# documents, into TYPING_LIST, and their planned versions, into TYPING_PLANNED (an object of
# artifact id to count, which browsertype.json records as planned_versions): the K documents that
# `loadgen plan-docs` names, those the step's own in-window edits and uploads plan the most
# versions for, most first, from the step's own flags (the ones run_step gives loadgen run), so
# browser typing types where the agent writes land. When the K-th document has fewer than
# TYPING_MIN_VERSIONS planned versions, one warning line names the count and the step goes on.
typing_plan_step() {
  local name=$1 k=$4 plan count
  plan=$(loadgen_local plan-docs -json -seed "$2" -sessions "$3" -top "$k" -events "$5" -warmup "$6" -duration "$7" -rng-seed "$8") ||
    die "step $name: loadgen plan-docs failed; it names the documents browser typing types into"
  mapfile -t TYPING_LIST < <(jq -r '.[].artifact | strings' <<<"$plan")
  TYPING_PLANNED=$(jq -c 'map({key: .artifact, value: .versions}) | from_entries' <<<"$plan") ||
    die "step $name: reading loadgen plan-docs' planned versions failed"
  count=$(jq -r --argjson k "$k" 'if length < $k then 0 else .[$k - 1].versions end' <<<"$plan")
  ((count >= TYPING_MIN_VERSIONS)) ||
    note "WARNING: step $name: typed document number $k has $count planned versions, under the $TYPING_MIN_VERSIONS versions INFRA-707's acceptance 2 needs of every typed document (loadgen plan-docs named ${#TYPING_LIST[@]} documents of --typing-docs $k); the step runs on"
}

# The jq program over a replay bundle's window.jsonl (typing_docs_replay): each document's agent
# writes in the sub-window, the artifact.version events that the replay sends as an edit or an
# upload (not a person's, which a browser made, and not a named version with no diff, a
# settlement), at $from <= created_at - state.json at < $to seconds; the documents of state.json
# (kind doc) that loadtest.replay_ids maps ($ids: "<logged id>|<local id>" lines), most writes
# first (ties by logged id), the first $k as their local ids.
# shellcheck disable=SC2016 # jq's variables
TYPING_REPLAY_JQ='def secs: capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$") as $c
    | ($c.d + "Z" | fromdateiso8601) + ("0" + ($c.f // "") | tonumber)
      - (if $c.z == "Z" then 0 else ($c.z[0:1] | if . == "-" then -1 else 1 end) * (($c.z[1:3] | tonumber) * 3600 + ($c.z[4:6] | tonumber) * 60) end);
  ($state[0].at | secs) as $at
  | ([$state[0].issues[]? | .artifacts[]? | select(.kind == "doc") | {key: .id, value: true}] | from_entries) as $docs
  | ([$ids | split("\n")[] | select(. != "") | split("|") | select(length == 2) | {key: .[0], value: .[1]}] | from_entries) as $local
  | [inputs | select(.type == "artifact.version" and .actor.kind != "user")
      | select((.created_at | secs) - $at | . >= $from and . < $to)
      | .payload | select((.version.named == true and .diff == null) | not) | .artifact_id
      | select(type == "string" and $docs[.] == true and $local[.] != null)]
  | group_by(.) | map({id: .[0], n: length}) | sort_by(-.n, .id) | .[:$k][] | $local[.id]'

# typing_docs_replay BUNDLE FROM_S TO_S K IDS: a replay's typing documents over the sub-window
# FROM_S to TO_S seconds after the bundle's state (TYPING_REPLAY_JQ over BUNDLE/window.jsonl and
# BUNDLE/state.json), IDS being loadtest.replay_ids's artifact rows as "<logged id>|<local id>"
# lines; one copy id per line.
typing_docs_replay() {
  jq -nr --slurpfile state "$1/state.json" --rawfile ids "$5" --argjson from "$2" --argjson to "$3" --argjson k "$4" \
    "$TYPING_REPLAY_JQ" "$1/window.jsonl"
}

# typing_replay_ids OUT: loadtest.replay_ids's artifact rows of the copy's database dispatch (the
# replay's reset copies them from dispatch_replay) into OUT, as psql -At prints them:
# "<logged id>|<local id>".
typing_replay_ids() {
  psql_at dispatch "select logged_id, local_id from loadtest.replay_ids where kind = 'artifact' order by logged_id" >"$1"
}

# typing_chosen NAME: refuse a typing step whose document choice (TYPING_LIST) is empty, and say
# which documents it types into.
typing_chosen() {
  ((${#TYPING_LIST[@]})) || die "step $1: --typing found no document to type into"
  note "step $1: browser typing into ${#TYPING_LIST[@]} documents (of --typing-docs $TYPING_DOCS): ${TYPING_LIST[*]}"
}

# typing_start NAME DIR SECONDS: when the step runs browser typing, browsertype detached in the
# runner as job typing-NAME, just before loadgen run (whose bound is SECONDS) starts, so its peers
# connect during the warm-up: the binary (put_executable), the copy's agent token into
# TYPING_DIR/token through kubectl exec's standard input (never on a command line, in no recorded
# file, never printed), the document list, then browsertype for SECONDS, the stored-order check's
# bound and 600 s more, which outlasts loadgen's window, grace, the after-work and the check;
# typing_stop ends it before that. A step that dies before typing_stop stops it on its way out
# (step_at_exit, the EXIT trap run_in_runner sets), so no browsertype types into a later step.
typing_start() {
  local name=$1 dir=$2 seconds=$3 docs
  [ -n "$TYPING" ] || return 0
  put_executable "$BROWSERTYPE_BIN" /work/browsertype
  # shellcheck disable=SC2016 # expanded by the runner's sh
  state .secrets.dispatch_token | kube exec -i runner -c runner -- sh -c 'umask 077 && mkdir -p "$1" && cat >"$1/token"' sh "$TYPING_DIR" ||
    die "step $name: writing the agent token for browsertype into the runner failed"
  docs=$RUN_DIR/tmp/typing-$name.docs
  printf '%s\n' "${TYPING_LIST[@]}" >"$docs"
  runner_put "$docs" "$TYPING_DIR/$name.docs"
  rm -f -- "$docs"
  runner_start "typing-$name" /work/browsertype -url "http://dispatch.$NS.svc:8766" -token-file "$TYPING_DIR/token" \
    -session "$TYPING_SESSION" -artifacts-file "$TYPING_DIR/$name.docs" -peers "$TYPING_PEERS" -rate "$TYPING_RATE" \
    -duration "$((seconds + STORED_ORDER_SECONDS + 600))s" -seed 1 -protect "$TYPING_PROTECT"
  note "step $name: browsertype (commit $(state '.browsertype.commit[0:12]')) started: $TYPING_PEERS peers on each of ${#TYPING_LIST[@]} documents at $TYPING_RATE keystroke updates/s, until the stored-order check ends"
  TYPING_STEP=("$name" "$dir")
}

# typing_stop_at_exit: step_at_exit's part: typing_stop of the step typing_start started, which
# does nothing once that step has stopped its typing, and nothing at all before any step typed.
typing_stop_at_exit() {
  ((${#TYPING_STEP[@]} == 2)) || return 0
  typing_stop "${TYPING_STEP[@]}" || true
}

# typing_summary TXT EXIT: browsertype.json from browsertype's output TXT: the commit it was built
# from, the documents (TYPING_LIST), the versions a load step plans for each of them
# (TYPING_PLANNED, null for a replay), peers, rate, its exit status (null when it gave none in
# time) and the sums of its peer lines' sent, skipped and errors, each null (never 0) when TXT
# holds no peer line.
typing_summary() {
  jq -Rn --arg commit "$(state '.browsertype.commit')" --argjson exit "$2" --argjson peers "$TYPING_PEERS" \
    --argjson rate "$TYPING_RATE" --argjson planned "$TYPING_PLANNED" '
    [inputs | capture("^artifact=[^ ]+ peer=[0-9]+ sent=(?<sent>[0-9]+) skipped=(?<skipped>[0-9]+) errors=(?<errors>[0-9]+) ") | map_values(tonumber)] as $l
    | {commit: $commit, docs: $ARGS.positional, planned_versions: $planned, peers: $peers, rate: $rate, exit: $exit}
      + (if $l == [] then {sent: null, skipped: null, errors: null}
        else {sent: ($l | map(.sent) | add), skipped: ($l | map(.skipped) | add), errors: ($l | map(.errors) | add)} end)' \
    --args "${TYPING_LIST[@]}" <"$1"
}

# typing_stop NAME DIR: when the step runs browser typing and has not stopped it yet (no
# DIR/browsertype.json): SIGTERM to browsertype, its exit status (at most 120 s later), its output
# into DIR/browsertype.txt and typing_summary into DIR/browsertype.json, the token removed from the
# runner, and one line naming the sums. A peer error or a non-zero exit is said, never a step
# failure. Called right after the stored-order command, and again after the check (which a run
# whose state.json records no checker does not run), so it never fails.
typing_stop() {
  local name=$1 dir=$2 job=typing-$1 rc
  { [ -n "$TYPING" ] && [ ! -f "$dir/browsertype.json" ]; } || return 0
  # shellcheck disable=SC2016 # expanded by the runner's sh
  kube exec runner -c runner -- sh -c 'kill -TERM "$(cat "/work/logs/$1.pid")"' sh "$job" >/dev/null 2>&1 ||
    note "step $name: SIGTERM to browsertype failed (it may have ended already)"
  rc=$(runner_wait "$job" 120 "$dir") || rc=timeout
  if [ -f "$dir/$job.log" ]; then mv -f -- "$dir/$job.log" "$dir/browsertype.txt"; else : >"$dir/browsertype.txt"; fi
  kube exec runner -c runner -- rm -f "$TYPING_DIR/token" "$TYPING_DIR/$name.docs" >/dev/null 2>&1 ||
    note "step $name: removing the agent token from the runner failed"
  [[ $rc =~ ^[0-9]+$ ]] || rc=null
  if ! typing_summary "$dir/browsertype.txt" "$rc" >"$dir/browsertype.json.tmp"; then
    rm -f -- "$dir/browsertype.json.tmp"
    note "step $name: summing browsertype's output failed; no browsertype.json"
    return 0
  fi
  mv -- "$dir/browsertype.json.tmp" "$dir/browsertype.json"
  note "$(jq -r --arg name "$name" 'def n: if . == null then "null" else tostring end;
    "step \($name): browsertype \(if .exit == null then "gave no exit status within 120 s of SIGTERM" else "exit \(.exit)" end): sent=\(.sent | n) skipped=\(.skipped | n) errors=\(.errors | n)"
    + (if .exit == 0 and .errors == 0 then "" else "; a peer error is reported, not a step failure: \($name)/browsertype.txt" end)' "$dir/browsertype.json")"
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
  kube_read exec "$1" -c "$2" -- grep -m1 '^model name' /proc/cpuinfo | sed 's/^model name[[:space:]]*:[[:space:]]*//' || echo unknown
}

# save_pg_settings FILE: SHOW ALL of the copy's Postgres.
save_pg_settings() { psql_at postgres 'show all' >"$1" || die "SHOW ALL failed in the copy's Postgres"; }

# write_copy_meta FILE DIR PHASE READS_ONLY: loadgen run's -copy-meta object (recorded under
# meta.json copy): the step's phase and whether it sends reads alone, which say what the run did,
# so that loadgen curves places the step by the run and not by its name (curves.go, classify);
# the images, Postgres's version and the sha256 of its SHOW ALL (DIR/postgres-settings.txt), the
# node CPU model in the Dispatch and Postgres containers, the namespace, the run id and each copy
# pod's resources as the live pods have them (COPY_PODS, container_snapshot's listing).
write_copy_meta() {
  local file=$1 dir=$2 phase=$3 reads=$4 version sha dpod cpu_d cpu_p pods
  [ "$reads" = true ] || [ "$reads" = false ] || die "write_copy_meta: reads_only '$reads' is not true or false"
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
    --arg phase "$phase" --argjson reads "$reads" \
    '{namespace, run_id: .run, phase: $phase, reads_only: $reads, dispatch_commit, production_repo, pg_major, images,
      postgres: {version: $version, settings_sha256: $sha, settings_file: "postgres-settings.txt", tier: .postgres.tier, cpu: .postgres.cpu, memory: .postgres.memory, volume: .postgres.volume, settings: .postgres.settings},
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

# run_step NAME FACTOR DURATION READS_ONLY PHASE: one step (see the header), into steps/NAME; with
# --typing (TYPING), browser typing into typing_plan_step's documents, the ones its own loadgen
# run plans the most versions for.
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
  local events requests sessions rng_seed=1 warmup=30s
  events=$(awk -v f="$factor" 'BEGIN { printf "%g", 4 * f }')
  requests=$(awk -v f="$factor" 'BEGIN { printf "%g", 60 * f }')
  sessions=$(loadgen_local series-sessions "$factor") || die "loadgen series-sessions $factor failed"
  [ "$reads" = true ] && events=0
  if [ -n "$TYPING" ]; then
    [ "$events" != 0 ] ||
      die "step $name: --typing with --reads-only has no agent write to choose the typed documents by; run it without --reads-only"
    typing_plan_step "$name" "$RUN_DIR/seed.json" "$sessions" "$TYPING_DOCS" "$events" "$warmup" "$duration" "$rng_seed"
    typing_chosen "$name"
  fi
  mkdir -p -- "$dir" "$RUN_DIR/phases"
  printf '%s\n' "$name" >>"$RUN_DIR/phases/$phase"
  note "step $name: factor $factor, $events events/s, $sessions sessions, $requests requests/s, window $duration"

  reset_copy "$dir/reset.json"
  container_snapshot "$dir/containers-before.tsv"
  write_copy_meta "$dir/copy-meta.json" "$dir" "$phase" "$reads"
  put_loadgen
  runner_has "$RUN_DIR/seed.json" /work/seed.json -f || runner_put "$RUN_DIR/seed.json" /work/seed.json
  kube exec runner -c runner -- mkdir -p /work/meta /work/steps || die "mkdir in the runner failed"
  runner_put "$dir/copy-meta.json" "/work/meta/$name.json"

  endpoints
  run_in_runner "$name" "$dir" "step-$name" $((30 + $(duration_seconds "$duration") + 20 + 900)) \
    /work/loadgen run -allow-foreign -name "$name" -out "/work/steps/$name" \
    -seed /work/seed.json -copy-meta "/work/meta/$name.json" -build-commit "$(state .dispatch_commit)" \
    -events "$events" -sessions "$sessions" -requests "$requests" -duration "$duration" \
    -warmup "$warmup" -grace 20s -rng-seed "$rng_seed" "${ENDPOINTS[@]}"
  note "step $name done: $dir"
}

# runner_copy_back NAME DIR: the runner's /work/steps/NAME into DIR, in two gzip-compressed
# streams: every file but heap.txt first, then heap.txt alone. With typing at 10x, heap.txt (the
# step-end heap profile in text form) passed 1 GB, and one uncompressed tar stream of it through
# kubectl exec ended early ("Unexpected EOF in archive", run lt-20261029t115927-655c, r16-typing),
# which lost meta.json and summary.json with it. Each stream is tried twice; a failure of either
# leaves the step in the runner, so nothing is removed there, and run_in_runner records the step
# as failed (kind copy_back). The first stream is extracted into a scratch directory beside DIR
# and its files are moved into DIR only once the stream ended cleanly, so a stream cut mid-file
# never leaves a truncated summary.json in DIR for loadgen curves and tables to read; heap.txt
# lands the same way, through heap.txt.part.
runner_copy_back() {
  local name=$1 dir=$2 src="/work/steps/$1" try scratch copied
  for try in 1 2; do
    scratch=$(mktemp -d "$dir/.copy-back.XXXXXX") || return 1
    copied=no
    if kube exec runner -c runner -- tar -C "$src" --exclude=./heap.txt -czf - . | tar -C "$scratch" -xzf - &&
      find "$scratch" -mindepth 1 -maxdepth 1 -exec mv -f -t "$dir" -- {} +; then
      copied=yes
    fi
    rm -rf -- "$scratch"
    [ "$copied" = no ] || break
    ((try < 2)) || return 1
    note "step $name: copying $src back failed (try $try of 2); retrying"
  done
  local has
  # shellcheck disable=SC2016 # expanded by the runner's sh
  has=$(kube_read exec runner -c runner -- sh -c 'if test -f "$1"; then echo yes; else echo no; fi' sh "$src/heap.txt") ||
    return 1
  [ "$has" = yes ] || return 0
  for try in 1 2; do
    if kube exec runner -c runner -- gzip -1 -c "$src/heap.txt" | gzip -d >"$dir/heap.txt.part" &&
      mv -- "$dir/heap.txt.part" "$dir/heap.txt"; then
      return 0
    fi
    rm -f -- "$dir/heap.txt.part"
    note "step $name: copying $src/heap.txt back failed (try $try of 2)"
  done
  return 1
}

# step_at_exit: the EXIT trap run_in_runner sets before the step's load: the step's disk watcher
# ended and its browser typing stopped, so neither outlives the step that started it.
step_at_exit() {
  pg_disk_watch_stop
  typing_stop_at_exit
}

# run_in_runner NAME DIR JOB SECONDS CMD...: a step's loadgen run CMD as the runner job JOB (at
# most SECONDS), with the Postgres data volume read before the load and refusing it with too
# little free space (pg_disk_low: the step fails with kind pg_disk and sends nothing), a watcher
# reading it while the load runs, Postgres's write-out counters before and after it
# (pg-stats-before.json, pg-stats-after.json) and, with --typing, browser typing from just before
# it (typing_start) until the stored-order command ends (typing_stop, and at the latest when the
# step ends); then the
# after-work of every step: DIR/pg-disk.json, /work/steps/NAME copied back into DIR (and, once
# copied, removed from the runner with /work/meta/NAME.json; a copy-back that failed is a step
# failure, kind copy_back,
# and leaves the files in the runner), the crash check, the container logs, a non-zero exit
# recorded, the stored-order check while Dispatch still runs (one that dies leaves the step
# going), cgroup-merge and tables.
run_in_runner() {
  local name=$1 dir=$2 job=$3 seconds=$4 started rc readings=$2/.pg-disk.jsonl
  shift 4
  trap step_at_exit EXIT
  pg_disk_sample "$readings" || true
  if pg_disk_low; then
    record_failure "$dir" pg_disk postgres "$(pg_disk_detail); the step sent no load, so Postgres keeps the room it has left"
    pg_disk_write "$dir" "$readings"
    return 1
  fi
  pg_stats "$dir/pg-stats-before.json"
  started=$(utc_now)
  typing_start "$name" "$dir" "$seconds"
  runner_start "$job" "$@"
  pg_disk_watch "$name" "$dir" "$job" "$readings" &
  PG_DISK_WATCHER=$!
  rc=$(runner_wait "$job" "$seconds" "$dir")
  pg_disk_watch_stop
  pg_disk_sample "$readings" || true
  pg_disk_write "$dir" "$readings"
  pg_stats "$dir/pg-stats-after.json"
  [ ! -f "$dir/$job.log" ] || mv -- "$dir/$job.log" "$dir/loadgen.out"
  if runner_copy_back "$name" "$dir"; then
    kube exec runner -c runner -- rm -rf "/work/steps/$name" "/work/meta/$name.json" ||
      note "step $name: removing /work/steps/$name from the runner failed"
  else
    record_failure "$dir" copy_back "" "copying /work/steps/$name back from the runner failed, so the step's files are missing here; they stay in the runner at /work/steps/$name"
  fi

  container_snapshot "$dir/containers-after.tsv"
  crash_check "$dir/containers-before.tsv" "$dir/containers-after.tsv" "$dir"
  save_pod_logs "$dir" "$started"
  [ "$rc" = 0 ] || record_failure "$dir" loadgen_exit "" "loadgen run ended with exit status $rc (see run.log and loadgen.out)"
  go_on "step $name: the stored-order check" stored_order_check "$name" "$dir"
  go_on "step $name: stopping browser typing" typing_stop "$name" "$dir"
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
# settings), restarted on the same PVC, whose size (up --pg-volume) the new tier keeps; cpu.max
# must then read CPU × 100000 per 100000.
resize_postgres() {
  local cpu=$1 gi=$2 tier got want
  tier=$(pg_tier "$cpu" "$gi")
  # shellcheck disable=SC2016 # $tier is jq's variable
  state_set '.postgres = $tier + {volume: .postgres.volume}' --argjson tier "$tier"
  # Dispatch and the listener hold Postgres connections; the next step's reset starts both again.
  stop_copy
  apply_manifest postgres.yaml
  kube_for 960 rollout status statefulset/postgres --timeout=900s >/dev/null ||
    die "the Postgres StatefulSet did not roll out within 15 min"
  kube_for 960 wait --for=condition=Ready pod/postgres-0 --timeout=900s >/dev/null ||
    die "postgres-0 was not ready within 15 min"
  want="$((cpu * 100000)) 100000"
  got=$(kube_read exec postgres-0 -c postgres -- cat /sys/fs/cgroup/cpu.max) || die "reading postgres-0's cpu.max failed"
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
  psql_write postgres "drop database if exists $REPLAY_TEMPLATE with (force)" >/dev/null || die "drop database $REPLAY_TEMPLATE failed"
  psql_write postgres 'drop database if exists dispatch with (force)' >/dev/null || die "drop database dispatch failed"
  psql_write postgres 'create database dispatch' >/dev/null || die "create database dispatch failed"
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
  psql_write dispatch 'vacuum analyze' >/dev/null || die "vacuum analyze of database dispatch failed"
  s_vacuum=$(elapsed_since "$t0")
  local after checksum
  after=$(psql_at dispatch 'select max(version) from schema_migrations') || die "reading the migrated seed's migration failed"
  t0=$EPOCHREALTIME
  put_loadgen
  checksum=$(replay_checksum dispatch) || die "loadgen replay-checksum of the restored template failed"
  s_checksum=$(elapsed_since "$t0")
  psql_write postgres "alter database dispatch rename to $REPLAY_TEMPLATE" >/dev/null || die "renaming dispatch to $REPLAY_TEMPLATE failed"
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

# run_replay NAME FROM TO PHASE: one replay step (see the header), into steps/NAME; with --typing
# (TYPING), browser typing into typing_docs_replay's documents, mapped through the reset
# database's loadtest.replay_ids.
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
  write_copy_meta "$dir/copy-meta.json" "$dir" "$phase" false
  cp -- "$rec" "$dir/replay-prepared.json"
  if [ -n "$TYPING" ]; then
    local ids=$RUN_DIR/tmp/replay-ids-$name.txt
    typing_replay_ids "$ids" || die "replay $name: reading loadtest.replay_ids for browser typing failed"
    mapfile -t TYPING_LIST < <(typing_docs_replay "$RUN_DIR/replay/bundle" "$(duration_seconds "$from")" "$(duration_seconds "$to")" "$TYPING_DOCS" "$ids")
    rm -f -- "$ids"
    typing_chosen "$name"
  fi
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
  cmax=$(kube_read exec "$dpod" -c envoy-dispatch -- cat /sys/fs/cgroup/cpu.max) || die "reading Dispatch's cpu.max failed"
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
# under 24 MiB (upload/cluster-load-test-<run id>-raw-<NAME>.tar.zst.partNN: the run id, which
# begins with the run's date and time, keeps two runs of one day apart on the issue); with ISSUE,
# each part goes to that Dispatch issue. upload/NAME.packed keeps a copy of the file STAMP (the phase's list
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
  base=$up/cluster-load-test-$RUN-raw-$name.tar.zst.part
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
# loadgen curves over every step directory but the phase replay's and those that ran browser
# typing (browsertype.json: the curves run without it) into <run>/curves, and loadgen
# codepaths in each step directory that holds summary.json (a step that wrote none has no profile;
# a replay step's as replay_codepaths runs it); then each phase's tar and the results tar. A
# failure is reported, the rest still runs, and collect_run returns 1.
collect_run() {
  local issue=${1:-} ran=() names=() curve_dirs=() replays=" " d p rc=0
  [ ! -f "$RUN_DIR/phases/replay" ] || replays+="$(tr '\n' ' ' <"$RUN_DIR/phases/replay")"
  steps_in_run_order
  for d in "${DIRS[@]}"; do
    if [ -f "$d/summary.json" ]; then ran+=("$d") names+=("${d##*/}"); fi
    if [[ $replays != *" ${d##*/} "* ]] && [ ! -f "$d/browsertype.json" ]; then curve_dirs+=("$d"); fi
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

# down_run: remove the run's hold, stop its daemons, delete everything labelled with it (the lock
# too, when it names this run), and wait until nothing of it is left; then print that listing. A
# run another PID namespace started has daemons this command cannot see, let alone stop: it says
# so, and they stop on their own, the heartbeat because the lock is gone and the sampler because
# the heartbeat is.
down_run() {
  local name deadline listing recorded here
  if [ -d "$RUN_DIR" ]; then
    rm -f -- "$RUN_DIR/hold-until"
    recorded=""
    [ ! -f "$STATE" ] || recorded=$(jq -r '.pid_ns // ""' "$STATE")
    here=$(pid_ns)
    if [ -n "$recorded" ] && [ -n "$here" ] && [ "$recorded" != "$here" ]; then
      note "down: run $RUN was started in PID namespace $recorded, not this one ($here), so this command can stop no daemon of it: it drops their pidfiles here, and they stop on their own, the heartbeat once the lock below is gone and the sampler once that heartbeat is."
      rm -f -- "$RUN_DIR/heartbeat.pid" "$RUN_DIR/sampler.pid"
    else
      for name in sampler heartbeat; do stop_daemon "$name"; done
    fi
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
  parse "namespace storage-class dispatch-commit pg-major production-repo pg-volume listener-image stored-order-source stored-order-checker-commit browsertype-commit" "" "$@"
  positional 0 "no positional arguments"
  trap up_exit EXIT
  up_run
  trap - EXIT
}

# up_exit: up's trap: the checker's scratch directory removed; a failure after the lock was taken
# names the command that removes what up made.
up_exit() {
  local rc=$?
  checker_tmp_remove
  if ((rc != 0)) && [ -n "${RUN_TAKEN:-}" ]; then
    note "up failed after it took the lock for run $RUN; cluster.sh down --namespace $NS --run $RUN removes what it made"
  fi
}

# up_run: up from OPT (also all's first phase).
up_run() {
  local sc=${OPT[storage-class]:-} commit=${OPT[dispatch-commit]:-} major=${OPT[pg-major]:-}
  local so_source=${OPT[stored-order-source]:-} so_checker=${OPT[stored-order-checker-commit]:-} bt_commit=${OPT[browsertype-commit]:-}
  local pg_volume=${OPT[pg-volume]:-$PG_VOLUME_DEFAULT}
  PRODUCTION_REPO=${OPT[production-repo]:-}
  local tool
  for tool in kubectl jq curl gh go git setsid timeout sha256sum awk tar gzip base64; do
    command -v "$tool" >/dev/null || die "up needs $tool on PATH"
  done
  [ -n "$sc" ] || die "up: --storage-class is required (for GKE: premium-rwo)"
  valid storage_class "$sc" || die "up: --storage-class '$sc' is not a storage class name"
  valid volume "$pg_volume" ||
    die "up: --pg-volume '$pg_volume' is not a whole number of Gi (for example $PG_VOLUME_DEFAULT)"
  ((${pg_volume%Gi} <= PG_VOLUME_MAX_GI)) ||
    die "up: --pg-volume $pg_volume is more than the ${PG_VOLUME_MAX_GI}Gi the namespace's quota leaves for the Postgres claim (requests.storage ${PG_QUOTA_STORAGE_GI}Gi, of which NATS's three claims take ${NATS_VOLUME_GI}Gi)"
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
  stored_order_args "$commit" "$so_source" "$so_checker"
  if [ -n "$bt_commit" ]; then
    valid commit "$bt_commit" || die "up: --browsertype-commit '$bt_commit' is not a full 40-hex commit sha"
    commit_source browsertype-commit "$bt_commit" packages/envoy/loadtest/browsertype \
      "so it has no browsertype for browser typing (INFRA-707)"
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

  # The stored-order checker, before the lock, so a checker that does not build is refused.
  stored_order_build "$commit" "$so_source" "$so_checker"
  # browsertype too, before the lock, so a driver that does not build or has no -protect is refused
  # with nothing taken.
  if [ -n "$bt_commit" ]; then
    note "building browsertype (static) of commit $bt_commit, for browser typing"
    build_at_commit "$bt_commit" ./loadtest/browsertype "$CHECKER_TMP/browsertype" || die "up: go build of browsertype at commit $bt_commit failed"
    local bt_help
    bt_help=$("$CHECKER_TMP/browsertype" -help 2>&1 || true)
    [[ $bt_help == *-protect* ]] ||
      die "up: browsertype of commit $bt_commit has no -protect flag: it would type into loadgen's '$TYPING_PROTECT' paragraph and fail every later edit and upload of the typed documents; give a commit of INFRA-707's driver that has -protect"
  fi

  set_run "lt-$(date -u +%Y%m%dt%H%M%S)-$(random_hex 2)"
  kube_lock_take
  RUN_TAKEN=1
  note "took the lock $LOCK in $NS for run $RUN"
  (umask 077 && mkdir -p -- "$RUN_DIR/logs" "$RUN_DIR/tmp" "$RUN_DIR/steps")
  register_command
  mv -- "$CHECKER_TMP/envoy-dispatch" "$STORED_ORDER_BIN" || die "moving the stored-order checker into $STORED_ORDER_BIN failed"
  [ -z "$bt_commit" ] || mv -- "$CHECKER_TMP/browsertype" "$BROWSERTYPE_BIN" || die "moving browsertype into $BROWSERTYPE_BIN failed"
  checker_tmp_remove
  local tier
  tier=$(pg_tier 16 60)
  tier=$(jq -c --arg v "$pg_volume" '. + {volume: $v}' <<<"$tier")
  (umask 077 && jq -n --arg ns "$NS" --arg run "$RUN" --arg sc "$sc" --arg commit "$commit" --arg major "$major" \
    --argjson stored_order "$STORED_ORDER_STATE" \
    --arg repo "$PRODUCTION_REPO" --arg created "$(utc_now)" --arg holder "$HOLDER_POD" --arg pidns "$(pid_ns)" \
    --arg dispatch "$dispatch_image" --arg listener "$listener_image" --arg listener_ref "$listener_ref" \
    --arg pg "$pg_image" --arg socat "$socat_image" --arg runner "$runner_image" --argjson nats "$nats_images" \
    --argjson tier "$tier" --arg pgpw "$(random_hex 24)" --arg dt "$(random_hex 32)" --arg lt "$(random_hex 32)" \
    --arg sk "$(random_hex 32)" '{namespace: $ns, run: $run, storage_class: $sc, dispatch_commit: $commit,
      pg_major: $major, production_repo: $repo, created: $created, holder_pod: $holder, pid_ns: $pidns,
      images: ({dispatch: $dispatch, listener: $listener, postgres: $pg, socat: $socat, runner: $runner} + $nats),
      image_sources: {listener: $listener_ref}, postgres: $tier, stored_order: $stored_order, browsertype: null,
      secrets: {pg_password: $pgpw, dispatch_token: $dt, listener_token: $lt, signing_key: $sk}}' >"$STATE")
  chmod 600 "$STATE"
  start_daemon heartbeat

  note "building loadgen (static) into $LOADGEN"
  (cd -- "$ENVOY_DIR" && CGO_ENABLED=0 go build -o "$LOADGEN" ./loadtest) || die "go build of loadgen failed"
  if [ -n "$bt_commit" ]; then
    # shellcheck disable=SC2016 # $c and $s are jq's variables
    state_set '.browsertype = {commit: $c, sha256: $s}' --arg c "$bt_commit" --arg s "$(sha256_of "$BROWSERTYPE_BIN")"
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
  put_executable "$STORED_ORDER_BIN" /work/envoy-dispatch
  [ -z "$bt_commit" ] || put_executable "$BROWSERTYPE_BIN" /work/browsertype
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

# print_resources: each copy container's requests and limits, and the size of the Postgres data
# volume, which is the claim's and no container's.
print_resources() {
  local jp='{range .items[*]}{.metadata.name}{range .spec.containers[*]}{"\t"}{.name}{": requests "}{.resources.requests}{", limits "}{.resources.limits}{end}{"\n"}{end}'
  printf 'postgres-0\tdata volume (claim data-postgres-0 on %s): %s\n' "$(state .storage_class)" "$(state .postgres.volume)"
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
  # The reference seed, as stack.sh seed writes it: 1,000 cited documents and their citations, so
  # the references route does the work it does in production.
  note "writing the production-shaped references through Dispatch's Service from the runner"
  runner_start seed-refs /work/loadgen seed-refs -allow-foreign "${ENDPOINTS[@]}" -seed /work/seed.json
  rc=$(runner_wait seed-refs 5400)
  [ "$rc" = 0 ] || die "loadgen seed-refs ended with $rc; see $RUN_DIR/logs/seed-refs.log"
  runner_start seed-finish /work/loadgen seed-finish -pg "$(pg_url)" -timeout 15m
  rc=$(runner_wait seed-finish 2400)
  [ "$rc" = 0 ] || die "loadgen seed-finish ended with $rc; see $RUN_DIR/logs/seed-finish.log"
  kube_read exec runner -c runner -- cat /work/seed.json >"$RUN_DIR/seed.json.tmp" || die "copying seed.json back failed"
  mv -- "$RUN_DIR/seed.json.tmp" "$RUN_DIR/seed.json"
  local migration counts
  migration=$(psql_at dispatch 'select max(version) from schema_migrations') || die "reading the seed's migration failed"
  counts=$(psql_at dispatch "$SEED_COUNTS_SQL") || die "counting the seed failed"
  stop_copy
  psql_write postgres 'alter database dispatch rename to dispatch_seed' >/dev/null || die "renaming dispatch to dispatch_seed failed"
  jq -n --argjson migration "$migration" --arg commit "$(state .dispatch_commit)" --arg at "$started" \
    --argjson seconds "$(elapsed_since "$t0")" --argjson counts "$counts" \
    '{migration: $migration, build_commit: $commit, created_at: $at, seed_seconds: $seconds, counts: $counts}' >"$RUN_DIR/seed.meta.json"
  note "seed done: template database dispatch_seed; $(jq -c . "$RUN_DIR/seed.meta.json")"
}

cmd_step() {
  parse "namespace run duration typing-peers typing-rate typing-docs" "reads-only typing" "$@"
  positional 2 "NAME FACTOR"
  use_run
  typing_parse
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
  if [ "$(state '.stored_order.dispatch_commit // empty')" != "$commit" ]; then
    note "WARNING: the stored-order checker was built for dispatch commit $(state '.stored_order.dispatch_commit // "none"'), not $commit: its renderer may differ from this Dispatch's, so each step's stored-order.json records renderer_matches false"
  fi
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
  parse "namespace run from to typing-peers typing-rate typing-docs" "typing" "$@"
  positional 1 "NAME"
  use_run
  typing_parse
  run_replay "${POS[0]}" "${OPT[from]:-0}" "${OPT[to]:-39m}" replay
}

# status: the lock, the run (its PID namespace and its hold), its daemons and its pods.
cmd_status() {
  parse "namespace run" "" "$@"
  positional 0 "no positional arguments"
  local lock run=${OPT[run]:-} name hold recorded
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
    recorded=$(jq -r '.pid_ns // ""' "$STATE")
    if [ -n "$recorded" ] && [ -n "$(pid_ns)" ] && [ "$recorded" != "$(pid_ns)" ]; then
      printf 'PID namespace: %s, not this one (%s): only the role that ran up can drive this run; status and down work from here\n' "$recorded" "$(pid_ns)"
    fi
    hold=$(hold_until)
    if [ -z "$hold" ]; then
      printf 'hold: none\n'
    elif ((EPOCHSECONDS < hold)); then
      printf 'hold: until %s (%s s left; cluster.sh hold --namespace %s --for DURATION extends it)\n' "$(hold_at "$hold")" "$((hold - EPOCHSECONDS))" "$NS"
    else
      printf 'hold: expired at %s (%s s ago)\n' "$(hold_at "$hold")" "$((EPOCHSECONDS - hold))"
    fi
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

# hold: set or extend the hold on the run that is up, so its heartbeat renews the lock while no
# command of it runs. It starts the run's daemons again when they stopped before it.
cmd_hold() {
  parse "namespace run for" "" "$@"
  positional 0 "no positional arguments"
  local d=${OPT[for]:-$KEEP_FOR_DEFAULT}
  valid duration "$d" || die "hold: --for '$d' is not a whole number of s, m or h (for example 2h)"
  use_run
  hold_set "$(duration_seconds "$d")" || die "hold: run $RUN has no directory in $CLUSTER_ROOT on this pod"
  ensure_daemons
  note "run $RUN is held until $(hold_at "$(hold_until)") ($d from now): its heartbeat renews the lock with no command of it running. cluster.sh hold --namespace $NS --for DURATION extends it; cluster.sh down --namespace $NS --run $RUN removes the copy."
}

cmd_all() {
  parse "namespace storage-class dispatch-commit pg-major production-repo pg-volume listener-image stored-order-source stored-order-checker-commit browsertype-commit profile manifest upload-issue keep-for" "keep" "$@"
  positional 0 "no positional arguments"
  local step_d hour_d replay_to refine collect_rc issue=${OPT[upload-issue]:-} manifest=${OPT[manifest]:-$REPLAY_MANIFEST} why
  case ${OPT[profile]:-} in
    quick) step_d=2m hour_d=5m replay_to=5m refine=false ;;
    full) step_d=10m hour_d=60m replay_to=39m refine=true ;;
    *) die "all: --profile quick or --profile full is required" ;;
  esac
  ALL_KEEP=${OPT[keep]:-}
  ALL_KEEP_FOR=${OPT[keep-for]:-$KEEP_FOR_DEFAULT}
  [ -n "$ALL_KEEP" ] || [ -z "${OPT[keep-for]:-}" ] ||
    die "all: --keep-for is valid only with --keep: without --keep the run is torn down at the end, so there is nothing to hold"
  valid duration "$ALL_KEEP_FOR" || die "all: --keep-for '$ALL_KEEP_FOR' is not a whole number of s, m or h (for example 2h)"
  [ -z "$issue" ] || valid issue "$issue" || die "--upload-issue '$issue' is not an issue key"
  replay_manifest_valid "$manifest"
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

# all_exit: all's trap: up's scratch directory for the checker removed; any exit with a run taken
# runs down, unless --keep, which instead holds the run for --keep-for, so its heartbeat renews the
# lock while the caller types steps and replays and no up of another worker calls it stale.
all_exit() {
  local rc=$?
  trap - EXIT
  checker_tmp_remove
  if [ -n "${RUN_TAKEN:-}" ] && [ -z "${ALL_KEEP:-}" ]; then
    note "all: exiting with status $rc; tearing run $RUN down (--keep skips this)"
    isolated down_run
    ((ISOLATED_RC == 0)) || note "all: down failed; run: cluster.sh down --namespace $NS --run $RUN"
  elif [ -n "${RUN_TAKEN:-}" ]; then
    if hold_set "$(duration_seconds "${ALL_KEEP_FOR:-$KEEP_FOR_DEFAULT}")"; then
      note "all: --keep: run $RUN stays up, held until $(hold_at "$(hold_until)") (--keep-for ${ALL_KEEP_FOR:-$KEEP_FOR_DEFAULT}): until then its heartbeat renews the lock with no command running, so no other worker's up calls it stale. cluster.sh hold --namespace $NS --for DURATION extends the hold."
    else
      note "all: --keep: run $RUN stays up, with no hold: it has no directory in $CLUSTER_ROOT on this pod"
    fi
    note "all: cluster.sh down --namespace $NS --run $RUN removes run $RUN"
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
    hold) cmd_hold "$@" ;;
    _heartbeat) cmd__heartbeat "$@" ;;
    _sampler) cmd__sampler "$@" ;;
    *) usage ;;
  esac
}

# render-nats.sh and cluster.test.sh source this file for its functions.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
