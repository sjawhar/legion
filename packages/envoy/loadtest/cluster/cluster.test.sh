#!/usr/bin/env bash
# packages/envoy/loadtest/cluster/cluster.test.sh
#
# Proves cluster.sh's behaviour without a cluster and without a network: fakes `kubectl`, `gh`,
# `curl`, `go`, `git`, `uv`, `dispatch`, `hostname` and `sleep` on PATH. The fake kubectl records
# every call and answers from files the test writes (the run's pods, the lock ConfigMap, cpu.max,
# the errors an apply fails with), and runs the stored-order check's runner script on the fake
# runner's /work, where the fake go's envoy-dispatch prints a summary line; the fake curl answers
# registry manifest reads with a digest (or 404 for the refs the test lists) and serves the replay
# fixture's parts as Dispatch artifact downloads, the fake gh serves the production repository's
# two files and a commit's first parent, the fake git holds the commits whose trees the test lists,
# and the fake sleep returns at once from apply_manifest's 3 s wait.
#
# Asserts that: (a) every subcommand refuses a missing --namespace; (b) up refuses a commit that is
# not 40 hex, a commit with no image (naming its newest first-parent ancestor with one), a
# --pg-major other than the production repository's (naming both), and a --stored-order-commit
# that the git store does not hold or whose tree has no cmd/dispatch/storedorder.go (naming it),
# each before any kubectl call creates anything, and refuses with exit 2 while the lock exists,
# naming the holder and the heartbeat's age, and past 15 min calling it stale and naming the down
# command; down refuses another run's lock; replay-prepare refuses a manifest whose seed is null and
# a seed check that failed (check.passed false, version_size_mismatches or
# anchors_outside_logged_block not 0), each before any kubectl call; (c) up, seed, step,
# replay-prepare, replay, db-size, collect, status, down and all run through to the end, and every
# kubectl call any of them makes, the background heartbeat and sampler included, carries
# `-n <namespace>`; each step and replay writes pg-stats-before.json and pg-stats-after.json; a
# step's reset looks up the nats-box pod once; loadgen, envoy-dispatch and seed.json go into the
# runner only when it does not hold them, and a step's directory is removed from the runner once
# copied back; up --stored-order-commit builds envoy-dispatch from the commit's archive with
# GOWORK=off and CGO_ENABLED=0 and records its commit and sha256; each step and replay then runs
# stored-order --since its window start with DATABASE_URL set, the password on no command line and
# in no recorded file, and writes stored-order.txt and stored-order.json (a clean run's counts, a
# mismatching run's, which is no step failure, and nulls for a missing summary line); all without
# the flag prints one skip line per step;
# replay-prepare downloads and verifies the parts, streams the dump into the Postgres pod,
# restores it and renames it to dispatch_replay, writing replay-prepared.json and keeping the
# manifest as replay-manifest.json; a replay's loadgen run holds -source replay -replay-rate 1
# -replay-from 0 -replay-to 39m (5m under all --profile quick), runs codepaths beside the outage
# with the run's replay-residual.md, and is left out of curves; collect runs loadgen tables
# and curves over the steps in the order they ran (meta.json start, not the directory names'
# order), loadgen codepaths in each step directory, writes STORED-ORDER.md with one row per step in
# that order and the commit, and packs and uploads curves/, TABLES.md and STORED-ORDER.md as
# phase results; all with a null-seed manifest prints one line per skipped replay phase and goes
# on, and with a seeded one runs replay r16 after soak and replay r4 after db-size and 1x-db4; all
# --profile quick runs no refinement and says so, and all --profile full, given a refine.txt with
# one factor and then an empty one, runs exactly one refinement step <factor>x; (d) up retries an
# apply the namespace's ResourceQuota race refuses ("the object has been modified") 3 s later and
# goes on, stops at once on any other apply error, naming the manifest and showing the error, and
# stops after five races in a row; (e) every manifest renders with no placeholder left and every
# image by digest, and a value of the wrong shape is refused; (f) a container restart between the
# before and after reads writes step-failed.json with kind restart; (g) the sampler writes an
# error line with no number when its exec fails, and a numeric line when it reads; (h) when go and
# the network reach helm, render-nats.sh re-renders nats.yaml equal to the committed file (else it
# says it skipped that); (i) the run's residual notes are replay-residual.md's lines, then the seed
# check's anchors by placement rule (with the share on the nearest surviving block) and its
# sign-flipped versions, a count the check lacks written as "not recorded"; (j) stored-order.json
# holds a clean run's and a mismatching run's counts, and nulls (never 0) for a missing summary
# line, a timeout (exit 124) and a check not run.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly cluster=$here/cluster.sh
readonly real_path=$PATH
# Not readonly: cluster.sh's functions, which (e) to (g) source, declare a local tmp.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cluster-test.XXXXXX")
readonly fake=$tmp/fake bin=$tmp/bin
readonly ns=loadtest-test

cleanup() {
  local f pid
  for f in "$tmp"/root*/*/heartbeat.pid "$tmp"/root*/*/sampler.pid; do
    [ -f "$f" ] || continue
    pid=$(<"$f")
    kill -TERM -- "-$pid" 2>/dev/null || true
  done
  rm -rf -- "$tmp"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  if [ -f "$tmp/out" ]; then
    printf -- '--- last output:\n' >&2
    cat "$tmp/out" >&2
  fi
  exit 1
}
pass() { printf 'PASS: %s\n' "$*"; }

mkdir -p "$fake" "$bin" "$fake/stepdir"

cat >"$bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# The fake kubectl: records each call, flags one without -n <FAKE_NS>, answers from FAKE_DIR.
set -u
printf '%s\n' "$*" >>"$FAKE_DIR/kubectl.log"
verb="" sel="" ctr="" prev="" has_ns=false seen=false cmd=()
for a in "$@"; do
  if $seen; then
    cmd+=("$a")
  elif [ "$a" = -- ]; then
    seen=true
  else
    [ "$prev" = -n ] && [ "$a" = "$FAKE_NS" ] && has_ns=true
    [ "$prev" = -l ] && sel=$a
    [ "$prev" = -c ] && ctr=$a
    if [ -z "$verb" ] && [ "$prev" != -n ] && [[ $a != -* ]]; then verb=$a; fi
  fi
  prev=$a
done
$has_ns || printf '%s\n' "$*" >>"$FAKE_DIR/kubectl-no-ns.log"
c=${cmd[*]:-}
case $verb in
  get)
    case "$*" in
      *"configmap dispatch-loadtest-lock"*) [ ! -f "$FAKE_DIR/lock.json" ] || cat "$FAKE_DIR/lock.json" ;;
      *"-o json"*)
        role=$(sed -n 's/.*dispatch-loadtest\/role=\([a-z-]*\).*/\1/p' <<<"$sel")
        jq --arg role "$role" 'if $role == "" then . else .items |= map(select(.metadata.labels["dispatch-loadtest/role"] == $role)) end' "$FAKE_DIR/pods.json" ;;
      *"-o name"*) ;;
      *jsonpath*) printf 'runner\trunner: requests {"cpu":"8"}, limits {"cpu":"8"}\n' ;;
      *"-o wide"*) printf 'NAME     READY   STATUS\nrunner   1/1     Running\n' ;;
      *) echo "No resources found in $FAKE_NS namespace." >&2 ;;
    esac
    ;;
  create)
    input=$(cat)
    if grep -q dispatch-loadtest-lock <<<"$input"; then
      if [ -f "$FAKE_DIR/lock.json" ]; then
        echo 'Error from server (AlreadyExists): configmaps "dispatch-loadtest-lock" already exists' >&2
        exit 1
      fi
      printf '%s\n' "$input" >"$FAKE_DIR/lock.json"
    fi
    ;;
  apply)
    # FAKE_APPLY_FAIL=NAME: an apply of manifests/NAME (its first line is `# NAME: ...`) fails with
    # the first line left in FAKE_DIR/apply-errors, which it uses up.
    input=$(cat)
    if [ -n "${FAKE_APPLY_FAIL:-}" ] && [[ $input == "# $FAKE_APPLY_FAIL:"* ]] && [ -s "$FAKE_DIR/apply-errors" ]; then
      head -n1 "$FAKE_DIR/apply-errors" >&2
      sed -i 1d "$FAKE_DIR/apply-errors"
      exit 1
    fi
    printf '%s\n' "$input" >>"$FAKE_DIR/applied.yaml"
    printf -- '---\n' >>"$FAKE_DIR/applied.yaml"
    ;;
  delete)
    run=${sel#dispatch-loadtest/run=}
    if [ -f "$FAKE_DIR/lock.json" ] && [ "$(jq -r .data.run "$FAKE_DIR/lock.json")" = "$run" ]; then
      rm -f "$FAKE_DIR/lock.json"
    fi
    ;;
  exec)
    case $c in
      "sh -c while :; do"*)
        if [ -n "${FAKE_EXEC_FAIL:-}" ]; then
          echo 'error: unable to upgrade connection: container not found ("envoy-dispatch")' >&2
          exit 1
        elif [ -n "${FAKE_SAMPLE:-}" ]; then
          printf 'usage_usec 123456\nthrottled_usec 0\ncpu_max 1600000 100000\nmemory_current 1048576\ninactive_file 4096\nend\n'
        fi
        ;;
      "sh -c name="*) printf '%s\n' "${cmd[*]:4}" >>"$FAKE_DIR/runner-starts" ;;
      "sh -c kill"*) ;;
      # replay-prepare: the dump streamed into the Postgres pod, its sha256 there, the restore.
      'sh -c cat >"$1" sh '*) cat >"$FAKE_DIR/pod-dump" ;;
      "sha256sum "*) sha256sum "$FAKE_DIR/pod-dump" ;;
      pg_restore*) printf '%s\n' "$c" >>"$FAKE_DIR/restores" ;;
      "rm -f /var/lib/postgresql/copy/replay-seed.dump") ;;
      # runner_has: the runner's file (FAKE_DIR/runner, which cp fills) when it passes the test.
      'sh -c test "$1" "$2" && sha256sum "$2" sh '*)
        f=$FAKE_DIR/runner${cmd[5]}
        test "${cmd[4]}" "$f" && sha256sum "$f" || exit 1
        ;;
      # A step's directory and copy-meta file, removed from the runner once copied back.
      "rm -rf /work/steps/"*) ;;
      # A replay step: the runner's directories, the bundle copied in and verified there.
      "sh -c rm -rf /work/bundle"*) ;;
      "tar -C /work -xf -") cat >/dev/null ;;
      "/work/loadgen replay-checksum "*) echo '{"issues": 1047, "md5": "fakemd5"}' ;;
      "/work/loadgen replay-verify-bundle "*) echo '{"files": 28}' ;;
      *"/healthz")
        echo '{"ok":true,"nats":true,"schema_version":121}'
        # FAKE_QUIET: two good reads of an idle Dispatch, 40 s apart, after any quiet check's start.
        if [ -n "${FAKE_QUIET:-}" ] && [ -f "$FAKE_DIR/lock.json" ]; then
          for at in 2099-01-01T00:00:00.000000Z 2099-01-01T00:00:40.000000Z; do
            printf '{"at":"%s","role":"dispatch","pod":"dispatch-5d9c","container":"envoy-dispatch","usage_usec":5000000,"throttled_usec":0,"cpu_max":"1600000 100000","memory_current":1048576,"inactive_file":0,"error":""}\n' \
              "$at" >>"$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$FAKE_DIR/lock.json")/cgroup.jsonl"
          done
        fi
        ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 stream info "*" --json") echo '{"config":{"num_replicas":3}}' ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 stream ls --names") echo ENVOY_NOTIFICATIONS ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 kv ls --names") echo interests ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 "*) ;;
      *"datname = 'dispatch_seed'"*) if [ -f "$FAKE_DIR/seeded" ]; then echo 1; else echo 0; fi ;;
      *"rename to dispatch_seed"*) : >"$FAKE_DIR/seeded" ;;
      *"datname = 'dispatch_replay'"*) if [ -f "$FAKE_DIR/replay-template" ]; then echo 1; else echo 0; fi ;;
      *"rename to dispatch_replay"*) : >"$FAKE_DIR/replay-template" ;;
      *"drop database if exists dispatch_replay"*) rm -f "$FAKE_DIR/replay-template" ;;
      *"to_regclass('pg_catalog.pg_stat_checkpointer')"*) echo f ;;
      *pg_stat_bgwriter*) echo '{"at": "2026-10-28T12:00:00+00:00", "bgwriter": {"buffers_clean": 0, "maxwritten_clean": 0}}' ;;
      *"select version()"*) echo 'PostgreSQL 16.4 (fake)' ;;
      *"show all"*) echo 'max_connections|800|Sets the maximum number of concurrent connections.' ;;
      *"max(version)"*) echo 121 ;;
      *json_build_object*) echo '{"projects": 4, "issues": 1000}' ;;
      psql*) ;;
      *cpuinfo*) printf 'model name\t: Fake CPU @ 2.00GHz\n' ;;
      # Dispatch's container keeps its 16-core limit; Postgres's reads FAKE_DIR/cpu.max.
      "cat /sys/fs/cgroup/cpu.max") if [ "$ctr" = envoy-dispatch ]; then echo '1600000 100000'; else cat "$FAKE_DIR/cpu.max"; fi ;;
      "cat /work/logs/"*.rc) echo 0 ;;
      "cat /work/logs/"*) echo 'fake runner log' ;;
      "cat /work/seed.json") echo '{"projects": ["LGA"]}' ;;
      "tar -C /work/steps/"*)
        # Each step's meta.json starts now, so the steps' start order is the order they ran.
        jq --arg start "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" '.start = $start' "$FAKE_DIR/stepdir/meta.json" >"$FAKE_DIR/meta.tmp"
        mv "$FAKE_DIR/meta.tmp" "$FAKE_DIR/stepdir/meta.json"
        tar -C "$FAKE_DIR/stepdir" -cf - .
        ;;
      chmod* | mkdir*) ;;
      # The stored-order check: the runner's own script, on the fake runner's /work (where cp put
      # the fake envoy-dispatch), with this call's standard input and its exit status.
      "sh -c IFS= read -r DATABASE_URL;"*)
        sh -c "${cmd[2]//\/work\//$FAKE_DIR/runner/work/}" "${cmd[@]:3}"
        exit $?
        ;;
      *)
        echo "fake kubectl exec: unexpected command: $c" >&2
        exit 1
        ;;
    esac
    ;;
  logs) echo 'fake log line' ;;
  cp)
    # A copy into the runner lands under FAKE_DIR/runner, where runner_has reads it.
    dst=$FAKE_DIR/runner${5#runner:}
    mkdir -p "${dst%/*}" && cp "$4" "$dst"
    ;;
  scale | rollout | wait | patch) ;;
  *)
    echo "fake kubectl: unexpected call: $*" >&2
    exit 1
    ;;
esac
exit 0
EOF

cat >"$bin/gh" <<'EOF'
#!/usr/bin/env bash
# The fake gh: the production repository's two files and each commit's first parent.
printf '%s\n' "$*" >>"$FAKE_DIR/gh.log"
case $* in
  *contents/dispatch/main.go*) printf 'const (\n\tdatabaseVersion = "POSTGRES_%s"\n)\n' "${FAKE_PROD_MAJOR:-16}" ;;
  *contents/dispatch/Pulumi.prod.yaml*) printf 'config:\n  example:listenerImage: ghcr.io/sjawhar/legion/envoy@sha256:%064d\n' 1 ;;
  "api repos/sjawhar/legion/commits/"*)
    c=${2##*/}
    awk -v c="$c" '$1 == c { print $2 }' "$FAKE_DIR/parents"
    ;;
  *)
    echo "fake gh: unexpected call: $*" >&2
    exit 1
    ;;
esac
EOF

cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
# The fake curl: a registry manifest read answers 200 with a digest, or 404 for a ref listed in
# FAKE_DIR/missing-refs; a token read answers a token; a Dispatch artifact version download (-o
# OUT) writes FAKE_DIR/parts/<slug>, after it reads the bearer header from its standard input.
printf '%s\n' "$*" >>"$FAKE_DIR/curl.log"
url=${*: -1}
case $url in
  */token\?*) echo '{"token": "anonymous"}' ;;
  */api/v1/issues/*/artifacts/*/versions/*)
    cat >/dev/null
    out="" prev=""
    for a in "$@"; do
      [ "$prev" = -o ] && out=$a
      prev=$a
    done
    slug=${url#*/artifacts/}
    cp "$FAKE_DIR/parts/${slug%%/versions/*}" "$out" || exit 22
    ;;
  */manifests/*)
    ref=${url##*/manifests/}
    if grep -qxF -- "$ref" "$FAKE_DIR/missing-refs" 2>/dev/null; then
      printf 'HTTP/2 404 \r\n\r\n'
    elif [[ $ref == sha256:* ]]; then
      printf 'HTTP/2 200 \r\ndocker-content-digest: %s\r\n\r\n' "$ref"
    else
      printf 'HTTP/2 200 \r\ndocker-content-digest: sha256:%s\r\n\r\n' "$(printf '%s' "$url" | sha256sum | cut -c1-64)"
    fi
    ;;
  *)
    echo "fake curl: unexpected call: $*" >&2
    exit 22
    ;;
esac
EOF

cat >"$bin/go" <<'EOF'
#!/usr/bin/env bash
# The fake go: `go build -o PATH ./loadtest` writes a fake loadgen to PATH, and `go build ... -o PATH
# ./cmd/dispatch`, in a directory that holds cmd/dispatch/storedorder.go, a fake envoy-dispatch,
# with GOWORK and CGO_ENABLED recorded in FAKE_DIR/go-dispatch.log.
printf '%s\n' "$*" >>"$FAKE_DIR/go.log"
out=""
prev=""
for a in "$@"; do
  [ "$prev" = -o ] && out=$a
  prev=$a
done
[ "${1:-}" = build ] && [ -n "$out" ] || {
  echo "fake go: unexpected call: $*" >&2
  exit 1
}
if [ "${*: -1}" = ./cmd/dispatch ]; then
  [ -f cmd/dispatch/storedorder.go ] || {
    echo "fake go: $PWD holds no cmd/dispatch/storedorder.go" >&2
    exit 1
  }
  printf 'GOWORK=%s CGO_ENABLED=%s go %s\n' "${GOWORK:-}" "${CGO_ENABLED:-}" "$*" >>"$FAKE_DIR/go-dispatch.log"
  cat >"$out" <<'DISPATCH'
#!/usr/bin/env bash
# The fake envoy-dispatch: stored-order records its arguments and DATABASE_URL, then answers as
# FAKE_STORED_ORDER says: clean (the default), mismatch or nosummary.
printf 'args %s\nDATABASE_URL %s\n' "$*" "${DATABASE_URL:-}" >>"$FAKE_DIR/stored-order.log"
case ${FAKE_STORED_ORDER:-clean} in
  clean)
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 base=4 updates_checked=9 updates_skipped=3 versions_checked=2 versions_skipped=1 mismatches=0'
    echo 'stored-order: 22222222-2222-4222-8222-222222222222 base=0 updates_checked=5 updates_skipped=0 versions_checked=1 versions_skipped=0 mismatches=0'
    echo 'stored-order: documents=2 clean=2 mismatched=0 refused=0 failed=0'
    ;;
  mismatch)
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 base=4 updates_checked=9 updates_skipped=3 versions_checked=2 versions_skipped=1 mismatches=1'
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 version 7: markdown md5 0cc175b9 length 12, folded md5 92eb5ffe length 13'
    echo 'stored-order: 22222222-2222-4222-8222-222222222222 base=0 updates_checked=0 updates_skipped=0 versions_checked=0 versions_skipped=0 mismatches=0 refused="no stored class"'
    echo 'stored-order: 33333333-3333-4333-8333-333333333333 base=1 updates_checked=2 updates_skipped=0 versions_checked=1 versions_skipped=0 mismatches=0'
    echo 'stored-order: documents=3 clean=1 mismatched=1 refused=1 failed=0'
    exit 1
    ;;
  nosummary)
    echo 'stored-order: open database: failed to connect: connection refused' >&2
    exit 1
    ;;
esac
DISPATCH
  chmod +x "$out"
  exit 0
fi
cat >"$out" <<'LOADGEN'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_DIR/loadgen.log"
if [ "${FAKE_LOADGEN_FAIL:-}" = "$1" ]; then
  echo "fake loadgen: $1 failed" >&2
  exit 1
fi
out="" prev=""
for a in "$@"; do
  [ "$prev" = -out ] && out=$a
  prev=$a
done
case $1 in
  series-sessions) echo 400 ;;
  replay-unpack)
    mkdir -p "$out"
    echo '{"project": "FAKE"}' >"$out/bundle.meta.json"
    ;;
  replay-verify-bundle) echo '{"bundle_meta_sha256": "feed", "files": 1}' ;;
  tables) [ -z "$out" ] || echo '# Load test tables' >"$out" ;;
  curves)
    # refine.txt is the first fixture FAKE_DIR/refine.<n> left, which this call uses up; else empty.
    mkdir -p "$out"
    echo '# Scaling curves' >"$out/CURVES.md"
    echo '# Knees' >"$out/KNEES.md"
    : >"$out/refine.txt"
    for f in "$FAKE_DIR"/refine.[0-9]*; do
      [ -f "$f" ] || break
      mv "$f" "$out/refine.txt"
      break
    done
    ;;
  codepaths) echo '# Code paths' >"$out/CODEPATHS.md" ;;
esac
exit 0
LOADGEN
chmod +x "$out"
EOF

cat >"$bin/git" <<'EOF'
#!/usr/bin/env bash
# The fake git: the git store holds a commit SHA when FAKE_DIR/git-trees/SHA lists its tree's files;
# `archive SHA PATH` writes a tar of those files (empty) under PATH.
printf '%s\n' "$*" >>"$FAKE_DIR/git.log"
[ "${1:-}" != -C ] || shift 2
case ${1:-} in
  cat-file)
    obj=$3
    case $obj in
      *'^{commit}') [ -f "$FAKE_DIR/git-trees/${obj%'^{commit}'}" ] ;;
      *:*) [ -f "$FAKE_DIR/git-trees/${obj%%:*}" ] && grep -qxF -- "${obj#*:}" "$FAKE_DIR/git-trees/${obj%%:*}" ;;
      *) exit 1 ;;
    esac
    ;;
  archive)
    [ -f "$FAKE_DIR/git-trees/$2" ] || {
      echo "fatal: not a valid object name $2" >&2
      exit 128
    }
    d=$(mktemp -d)
    while read -r p; do mkdir -p "$d/${p%/*}" && : >"$d/$p"; done <"$FAKE_DIR/git-trees/$2"
    tar -C "$d" -cf - "$3"
    rc=$?
    rm -rf "$d"
    exit "$rc"
    ;;
  *)
    echo "fake git: unexpected call: $*" >&2
    exit 1
    ;;
esac
EOF

cat >"$bin/uv" <<'EOF'
#!/usr/bin/env bash
# The fake uv: zstd_compress's `uv run ... python3 -c ...` passes its input through.
cat
EOF

cat >"$bin/dispatch" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_DIR/dispatch.log"
EOF

cat >"$bin/hostname" <<'EOF'
#!/usr/bin/env bash
echo test-worker-0
EOF

# The fake sleep: apply_manifest's 3 s wait between tries is recorded and returns at once; every
# other sleep (the daemons' loops, their start waits) is the real one.
cat >"$bin/sleep" <<EOF
#!/usr/bin/env bash
if [ "\$*" = 3 ]; then
  printf '%s\n' "\$*" >>"\$FAKE_DIR/sleep.log"
  exit 0
fi
exec $(PATH=$real_path command -v sleep) "\$@"
EOF
chmod +x "$bin"/*

export FAKE_DIR=$fake FAKE_NS=$ns
export LOADTEST_CLUSTER_ROOT=$tmp/root
export POD_UID=00000000-1111-2222-3333-444444444444 LEGION_ISSUE=TEST-1
export PATH="$bin:$PATH"
unset FAKE_EXEC_FAIL FAKE_SAMPLE FAKE_PROD_MAJOR FAKE_LOADGEN_FAIL FAKE_APPLY_FAIL FAKE_QUIET FAKE_STORED_ORDER
export DISPATCH_URL=http://dispatch.invalid DISPATCH_TOKEN_FILE=$fake/token
echo fake-token >"$fake/token"

readonly commit_ok=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly commit_none=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly commit_parent=cccccccccccccccccccccccccccccccccccccccc
readonly commit_ancestor=dddddddddddddddddddddddddddddddddddddddd
printf '%s %s\n%s %s\n' "$commit_none" "$commit_parent" "$commit_parent" "$commit_ancestor" >"$fake/parents"
printf '%s\n%s\n' "$commit_none" "$commit_parent" >"$fake/missing-refs"
# The stored-order commits: one whose tree has cmd/dispatch/storedorder.go, one whose tree has
# none, and one the git store does not hold.
readonly commit_so=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
readonly commit_old=ffffffffffffffffffffffffffffffffffffffff
readonly commit_unknown=0123456789abcdef0123456789abcdef01234567
mkdir -p "$fake/git-trees"
printf '%s\n' packages/envoy/go.mod packages/envoy/cmd/dispatch/main.go packages/envoy/cmd/dispatch/storedorder.go >"$fake/git-trees/$commit_so"
printf '%s\n' packages/envoy/go.mod packages/envoy/cmd/dispatch/main.go >"$fake/git-trees/$commit_old"
jq -n '{window_start: "2026-10-28T10:00:00Z", window_end: "2026-10-28T10:01:00Z"}' >"$fake/stepdir/meta.json"
echo '{"config": {"step": "fake"}}' >"$fake/stepdir/summary.json"

# The replay's fixture: two bundle parts and one seed dump part, served by the fake curl by slug.
mkdir -p "$fake/parts"
for slug in fake-replay-tar-zst-part00 fake-replay-tar-zst-part01 fake-seed-dump-part00; do
  head -c 3000 /dev/urandom >"$fake/parts/$slug"
done

# part_entry FILE SLUG: a manifest part entry for FAKE_DIR/parts/SLUG.
part_entry() {
  jq -n --arg f "$1" --arg s "$2" --arg sha "$(sha256sum <"$fake/parts/$2" | cut -d' ' -f1)" --argjson b "$(stat -c %s "$fake/parts/$2")" \
    '{file: $f, slug: $s, ref: "dispatch://INFRA-639/artifact/\($s)", version: 1, bytes: $b, sha256: $sha}'
}

# write_manifest OUT [CHECK]: a schema 1 replay manifest of the fixture's parts whose seed (at
# migration 121, the fake Postgres's) has the seed check CHECK (default: passed, both counts 0).
write_manifest() {
  local check=${2:-'{"passed": true, "version_size_mismatches": 0, "anchors_outside_logged_block": 0}'}
  jq -n --argjson b0 "$(part_entry fake-replay.tar.zst.part00 fake-replay-tar-zst-part00)" \
    --argjson b1 "$(part_entry fake-replay.tar.zst.part01 fake-replay-tar-zst-part01)" \
    --argjson s0 "$(part_entry fake-seed.dump.part00 fake-seed-dump-part00)" \
    --arg tar "$(cat "$fake/parts/fake-replay-tar-zst-part00" "$fake/parts/fake-replay-tar-zst-part01" | sha256sum | cut -d' ' -f1)" \
    --arg dump "$(sha256sum <"$fake/parts/fake-seed-dump-part00" | cut -d' ' -f1)" --argjson check "$check" \
    '{schema: 1, project: "FAKE", window: {from: "2026-10-26T22:55:00Z", to: "2026-10-26T23:34:00Z"}, issue: "INFRA-639",
      bundle: {tar_zst_sha256: $tar, parts: [$b0, $b1]},
      seed: {dump_sha256: $dump, migration: 121, check: $check, parts: [$s0]}}' >"$1"
}
readonly seeded=$fake/replay-seeded.json
write_manifest "$seeded"

# write_pods [DISPATCH_RESTARTS [DISPATCH_REASON]]: the run's pods as the fake kubectl lists them.
write_pods() {
  jq -n --argjson r "${1:-0}" --arg reason "${2:-}" '
    def c($name): {name: $name, restartCount: 0, lastState: {}};
    def pod($name; $role; $containers): {metadata: {name: $name, labels: {"dispatch-loadtest/role": $role}},
      spec: {nodeName: "node-1", containers: [$containers[] | {name, image: "example.io/x@sha256:00", resources: {}}]},
      status: {phase: "Running", containerStatuses: $containers}};
    {items: [
      pod("dispatch-5d9c"; "dispatch"; [{name: "envoy-dispatch", restartCount: $r,
        lastState: (if $reason == "" then {} else {terminated: {reason: $reason}} end)}, c("pprof")]),
      pod("postgres-0"; "postgres"; [c("postgres")]),
      pod("nats-0"; "nats"; [c("nats"), c("reloader")]),
      pod("nats-1"; "nats"; [c("nats"), c("reloader")]),
      pod("nats-2"; "nats"; [c("nats"), c("reloader")]),
      pod("nats-box-7f6d"; "nats-box"; [c("nats-box")]),
      pod("listener-6b8f"; "listener"; [c("envoy-listener")]),
      pod("runner"; "runner"; [c("runner")])]}' >"$fake/pods.json"
}

reset_fake() {
  rm -f -- "$fake/lock.json" "$fake/applied.yaml" "$fake/kubectl.log" "$fake/seeded" "$fake/runner-starts" "$fake/dispatch.log" "$fake/loadgen.log" "$fake"/refine.[0-9]* \
    "$fake/sleep.log" "$fake/apply-errors" "$fake/replay-template" "$fake/restores" "$fake/pod-dump" \
    "$fake/stored-order.log" "$fake/go-dispatch.log" "$fake/git.log"
  rm -rf -- "$fake/runner"
  write_pods
  echo '1600000 100000' >"$fake/cpu.max"
}

# run_cluster ARGS: cluster.sh ARGS, its output in $tmp/out and its status in RC.
run_cluster() {
  set +e
  bash "$cluster" "$@" >"$tmp/out" 2>&1
  RC=$?
  set -e
}

# no_create: no kubectl call so far created or applied anything.
no_create() {
  ! grep -qE "^-n $ns (create|apply) " "$fake/kubectl.log" 2>/dev/null
}

# logged_steps PREFIX: the step names, in argument order, of the last loadgen call that started
# with PREFIX.
logged_steps() {
  local words=() w names=()
  read -ra words < <(grep -- "^$1 " "$fake/loadgen.log" | tail -n1)
  for w in "${words[@]}"; do
    if [[ $w == */steps/* ]]; then names+=("${w##*/}"); fi
  done
  printf '%s\n' "${names[*]}"
}

up_args=(--namespace "$ns" --storage-class premium-rwo --pg-major 16 --production-repo example/deploy)

# ---- (a) every subcommand refuses a missing --namespace --------------------------------------------

reset_fake
for sub in up seed step series soak db-size redeploy replay-prepare replay collect status down all; do
  run_cluster "$sub"
  ((RC != 0)) || fail "cluster.sh $sub without --namespace exited 0"
  grep -q -- '--namespace is required' "$tmp/out" || fail "cluster.sh $sub without --namespace did not name --namespace"
done
[ ! -s "$fake/kubectl.log" ] || fail "a subcommand without --namespace called kubectl"
pass "every subcommand refuses a missing --namespace before any kubectl call"

# ---- (b) up's refusals -----------------------------------------------------------------------------

reset_fake
run_cluster up "${up_args[@]}" --dispatch-commit 9b92ac1
((RC != 0)) || fail "up accepted a short commit"
grep -q 'not a full 40-hex commit' "$tmp/out" || fail "up's refusal of a short commit does not say why"
no_create || fail "up created something before refusing a short commit"
pass "up refuses a commit that is not 40 hex"

reset_fake
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_none"
((RC != 0)) || fail "up accepted a commit with no image"
grep -q "no image ghcr.io/sjawhar/legion/envoy:$commit_none" "$tmp/out" || fail "up's refusal does not name the commit"
grep -q "newest first-parent ancestor on sjawhar/legion main that has one is $commit_ancestor" "$tmp/out" ||
  fail "up's refusal does not name the newest first-parent ancestor with an image"
no_create || fail "up created something before refusing a commit with no image"
pass "up refuses a commit with no image, naming its newest first-parent ancestor with one, before creating anything"

reset_fake
run_cluster up --namespace "$ns" --storage-class premium-rwo --pg-major 18 --production-repo example/deploy --dispatch-commit "$commit_ok"
((RC != 0)) || fail "up accepted --pg-major 18 while production runs 16"
grep -q -- '--pg-major 18 differs from production.s Postgres 16' "$tmp/out" || fail "up's refusal does not name both majors"
grep -q 'POSTGRES_16' "$tmp/out" || fail "up's refusal does not name production's databaseVersion"
no_create || fail "up created something before refusing the Postgres major"
pass "up refuses a --pg-major that differs from the production repository's, naming both"

reset_fake
for c in "9b92ac1|--stored-order-commit '9b92ac1' is not a full 40-hex commit sha" \
  "$commit_unknown|--stored-order-commit $commit_unknown is not a commit in this checkout's git store" \
  "$commit_old|--stored-order-commit $commit_old has no packages/envoy/cmd/dispatch/storedorder.go"; do
  run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --stored-order-commit "${c%%|*}"
  ((RC != 0)) || fail "up accepted --stored-order-commit ${c%%|*}"
  grep -qF -- "${c#*|}" "$tmp/out" || fail "up's refusal of --stored-order-commit ${c%%|*} does not say '${c#*|}'"
  no_create || fail "up created something before refusing --stored-order-commit ${c%%|*}"
  [ ! -f "$fake/lock.json" ] || fail "up took the lock before refusing --stored-order-commit ${c%%|*}"
  [ ! -s "$fake/go-dispatch.log" ] || fail "up built envoy-dispatch before refusing --stored-order-commit ${c%%|*}"
done
pass "up refuses a --stored-order-commit that is not 40 hex, that the git store lacks, or whose tree has no cmd/dispatch/storedorder.go, naming it, before creating anything"

# ---- (b) replay-prepare's refusals ---------------------------------------------------------------------

# The committed manifest's seed is null until INFRA-639 uploads it; a seed check that failed any of
# the three conditions is refused too. Each before any kubectl call, so before any kubectl write.
reset_fake
jq '.seed = null' "$seeded" >"$fake/replay-null.json"
write_manifest "$fake/replay-unpassed.json" '{"passed": false, "version_size_mismatches": 0, "anchors_outside_logged_block": 0}'
write_manifest "$fake/replay-sizes.json" '{"passed": true, "version_size_mismatches": 2, "anchors_outside_logged_block": 0}'
write_manifest "$fake/replay-anchors.json" '{"passed": true, "version_size_mismatches": 0, "anchors_outside_logged_block": 1}'
for c in "null|seed is null): INFRA-639's seed is not uploaded yet" "unpassed|the seed check failed: check.passed is false" \
  "sizes|the seed check failed: version_size_mismatches is 2" "anchors|the seed check failed: anchors_outside_logged_block is 1"; do
  run_cluster replay-prepare --namespace "$ns" --manifest "$fake/replay-${c%%|*}.json"
  ((RC != 0)) || fail "replay-prepare accepted the manifest replay-${c%%|*}.json"
  grep -qF -- "${c#*|}; refusing before it touches the copy" "$tmp/out" || fail "replay-prepare's refusal of replay-${c%%|*}.json does not say '${c#*|}'"
  [ ! -s "$fake/kubectl.log" ] || fail "replay-prepare called kubectl before refusing replay-${c%%|*}.json: $(cat "$fake/kubectl.log")"
  [ ! -s "$fake/curl.log" ] || ! grep -q /api/v1/ "$fake/curl.log" || fail "replay-prepare downloaded a part before refusing replay-${c%%|*}.json"
done
pass "replay-prepare refuses a null seed and a seed check that failed any of its three conditions, before any kubectl call"

# write_lock RUN HEARTBEAT [UID]: another run's lock ConfigMap.
write_lock() {
  jq -n --arg run "$1" --arg hb "$2" --arg uid "${3:-99999999-8888-7777-6666-555555555555}" \
    '{metadata: {name: "dispatch-loadtest-lock"}, data: {"holder-pod": "other-worker-3", "holder-uid": $uid,
      issue: "OTHER-9", run: $run, started: $hb, heartbeat: $hb}}' >"$fake/lock.json"
}
readonly other_run=lt-20261028t080000-beef

reset_fake
write_lock "$other_run" "$(date -u -d '-120 seconds' +%Y-%m-%dT%H:%M:%SZ)"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 2)) || fail "up while the lock exists exited $RC, not 2"
grep -q 'holder pod other-worker-3' "$tmp/out" || fail "up's lock refusal does not name the holder"
grep -qE 'last heartbeat [^ ]+, 1[2-5][0-9] s ago' "$tmp/out" || fail "up's lock refusal does not give the heartbeat's age"
! grep -q stale "$tmp/out" || fail "up called a 2-minute-old heartbeat stale"
no_create || fail "up created something while the lock existed"
pass "up refuses with exit 2 while the lock exists, naming the holder and the heartbeat's age"

write_lock "$other_run" "$(date -u -d '-20 minutes' +%Y-%m-%dT%H:%M:%SZ)"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 2)) || fail "up while a stale lock exists exited $RC, not 2"
grep -q 'older than 15 min, so the holder is stale' "$tmp/out" || fail "up does not call a 20-minute-old heartbeat stale"
grep -q "cluster.sh down --namespace $ns --run $other_run" "$tmp/out" || fail "up's stale refusal does not name the down command"
pass "up names a lock whose heartbeat is older than 15 min stale, with the command that clears it"

run_cluster down --namespace "$ns"
((RC != 0)) || fail "down removed another run's lock without --run"
grep -q -- "pass --run $other_run" "$tmp/out" || fail "down's refusal of another run's lock does not name --run"
[ -f "$fake/lock.json" ] || fail "down deleted another run's lock without --run"
pass "down refuses another run's lock unless --run names it"

# ---- (c) the whole path, and -n on every kubectl call ------------------------------------------------

reset_fake
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --stored-order-commit "$commit_so"
((RC == 0)) || fail "up exited $RC"
run=$(jq -r .data.run "$fake/lock.json")
run_dir=$LOADTEST_CLUSTER_ROOT/$run
[ "$(jq -r '.data."holder-pod"' "$fake/lock.json")" = test-worker-0 ] || fail "the lock does not name this pod"
for key in holder-pod holder-uid issue run started heartbeat; do
  [ -n "$(jq -r --arg k "$key" '.data[$k] // empty' "$fake/lock.json")" ] || fail "the lock has no $key"
done
[ "$(stat -c %a "$run_dir/state.json")" = 600 ] || fail "state.json is not mode 600"
grep -q 'requests .*limits' "$tmp/out" || fail "up did not print the containers' requests and limits"
jq -e --arg c "$commit_so" --arg s "$(sha256sum <"$run_dir/envoy-dispatch" | cut -d' ' -f1)" '.stored_order == {commit: $c, sha256: $s}' \
  "$run_dir/state.json" >/dev/null || fail "state.json does not record the stored-order commit and envoy-dispatch's sha256: $(jq -c .stored_order "$run_dir/state.json")"
grep -qx -- "-C $(cd -- "$here/../../../.." && pwd) archive $commit_so packages/envoy" "$fake/git.log" ||
  fail "up did not git archive packages/envoy of $commit_so from the checkout's git store: $(cat "$fake/git.log")"
grep -qx -- "GOWORK=off CGO_ENABLED=0 go build -buildvcs=false -o $run_dir/envoy-dispatch ./cmd/dispatch" "$fake/go-dispatch.log" ||
  fail "up did not build a static envoy-dispatch outside the workspace: $(cat "$fake/go-dispatch.log" 2>/dev/null)"
[ -z "$(find "$run_dir" -maxdepth 1 -name '.stored-order-src.*')" ] || fail "up left the build's scratch source in the run directory"
[ "$(grep -c -- "^-n $ns cp $run_dir/envoy-dispatch runner:/work/envoy-dispatch " "$fake/kubectl.log")" = 1 ] ||
  fail "up did not copy envoy-dispatch into the runner"
pg_password=$(jq -r .secrets.pg_password "$run_dir/state.json")

run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC"
[ -f "$run_dir/seed.json" ] || fail "seed kept no seed.json in the run directory"

run_cluster step --namespace "$ns" smoke 1 --duration 1m
((RC == 0)) || fail "step exited $RC"
for f in reset.json copy-meta.json meta.json summary.json postgres-settings.txt containers-before.tsv containers-after.tsv pg-stats-before.json pg-stats-after.json; do
  [ -f "$run_dir/steps/smoke/$f" ] || fail "the step directory has no $f"
done
[ ! -f "$run_dir/steps/smoke/step-failed.json" ] || fail "a step with no restart was marked failed"
start=$(grep '^step-smoke ' "$fake/runner-starts") || fail "the step started no loadgen run in the runner"
for want in "run -allow-foreign" "-events 4 " "-sessions 400 " "-requests 60 " "-duration 1m " "-warmup 30s " "-grace 20s " \
  "-rng-seed 1 " "-copy-meta /work/meta/smoke.json" "-nats-server-name nats-0" "-pg postgres://dispatch:"; do
  [[ $start == *"$want"* ]] || fail "the step's loadgen run lacks '$want': $start"
done
jq -e '.namespace == "'"$ns"'" and .postgres.version != "" and .postgres.settings_sha256 != "" and .node_cpu_model.dispatch == "Fake CPU @ 2.00GHz" and (.pods | length) == 8' \
  "$run_dir/steps/smoke/copy-meta.json" >/dev/null || fail "copy-meta.json lacks a field: $(cat "$run_dir/steps/smoke/copy-meta.json")"
jq -e '.bgwriter.buffers_clean == 0' "$run_dir/steps/smoke/pg-stats-after.json" >/dev/null ||
  fail "pg-stats-after.json does not hold pg_stat_bgwriter: $(cat "$run_dir/steps/smoke/pg-stats-after.json")"
# The stored-order check after the step: --since the window start, DATABASE_URL set from standard
# input, the password on no command line and in no recorded file, and a clean run's counts.
grep -qx -- "args stored-order --since 2026-10-28T10:00:00Z" "$fake/stored-order.log" ||
  fail "envoy-dispatch was not run as stored-order --since the window start: $(cat "$fake/stored-order.log" 2>/dev/null)"
grep -qxF -- "DATABASE_URL postgres://dispatch:$pg_password@postgres.$ns.svc:5432/dispatch?sslmode=disable" "$fake/stored-order.log" ||
  fail "stored-order ran without the copy's DATABASE_URL"
so_call=$(grep -F '/work/envoy-dispatch stored-order' "$fake/kubectl.log") || fail "no kubectl call ran stored-order in the runner"
[[ $so_call == "-n $ns exec -i runner -c runner -- sh -c "*" sh 1800 2026-10-28T10:00:00Z" ]] ||
  fail "stored-order's kubectl call is not an exec -i in the runner bounded at 1800 s since the window start: $so_call"
! grep -qF -- "$pg_password" <<<"$so_call" || fail "stored-order's kubectl call carries the Postgres password"
grep -qx 'stored-order: documents=2 clean=2 mismatched=0 refused=0 failed=0' "$run_dir/steps/smoke/stored-order.txt" ||
  fail "stored-order.txt does not hold the command's output: $(cat "$run_dir/steps/smoke/stored-order.txt" 2>/dev/null)"
jq -e --arg c "$commit_so" '. == {commit: $c, exit: 0, since: "2026-10-28T10:00:00Z", documents: 2, clean: 2, mismatched: 0, refused: 0, failed: 0}' \
  "$run_dir/steps/smoke/stored-order.json" >/dev/null || fail "a clean run's stored-order.json is wrong: $(cat "$run_dir/steps/smoke/stored-order.json" 2>/dev/null)"
grep -qF "step smoke: stored-order (envoy-dispatch of eeeeeeeeeeee) since 2026-10-28T10:00:00Z: exit 0, documents=2 clean=2 mismatched=0 refused=0 failed=0" "$tmp/out" ||
  fail "step smoke did not print the stored-order counts"

boxes=$(grep -c -- "-l dispatch-loadtest/run=$run,dispatch-loadtest/role=nats-box -o json" "$fake/kubectl.log" || true)
FAKE_STORED_ORDER=mismatch run_cluster step --namespace "$ns" reads 10 --duration 1m --reads-only
((RC == 0)) || fail "step --reads-only with a stored-order mismatch exited $RC"
jq -e --arg c "$commit_so" '. == {commit: $c, exit: 1, since: "2026-10-28T10:00:00Z", documents: 3, clean: 1, mismatched: 1, refused: 1, failed: 0}' \
  "$run_dir/steps/reads/stored-order.json" >/dev/null || fail "a mismatching run's stored-order.json is wrong: $(cat "$run_dir/steps/reads/stored-order.json" 2>/dev/null)"
grep -qF 'step reads: stored-order (envoy-dispatch of eeeeeeeeeeee) since 2026-10-28T10:00:00Z: exit 1, documents=3 clean=1 mismatched=1 refused=1 failed=0; a correctness finding, not a step failure' "$tmp/out" ||
  fail "step reads did not print the mismatching stored-order counts as a finding"
[ ! -f "$run_dir/steps/reads/step-failed.json" ] || fail "a stored-order mismatch was recorded as a step failure: $(cat "$run_dir/steps/reads/step-failed.json")"
grep -q "^cgroup-merge $run_dir/steps/reads " "$fake/loadgen.log" || fail "step reads did not go on to cgroup-merge after a stored-order mismatch"
start=$(grep '^step-reads ' "$fake/runner-starts")
[[ $start == *"-events 0 "* && $start == *"-requests 600 "* ]] || fail "a --reads-only step at 10x is not 0 events and 600 requests: $start"
boxes=$(($(grep -c -- "-l dispatch-loadtest/run=$run,dispatch-loadtest/role=nats-box -o json" "$fake/kubectl.log") - boxes))
((boxes == 1)) || fail "a step's reset looked up the nats-box pod $boxes times, not once"
[ "$(grep -c -- "^-n $ns cp $run_dir/seed.json runner:/work/seed.json " "$fake/kubectl.log")" = 1 ] ||
  fail "seed.json was copied into the runner again although the runner held it"
grep -qx -- "-n $ns exec runner -c runner -- rm -rf /work/steps/smoke /work/meta/smoke.json" "$fake/kubectl.log" ||
  fail "step smoke's directory and copy-meta file were not removed from the runner once copied back"
pass "a step's reset looks up the nats-box pod once, seed.json goes into the runner only when it lacks it, and a step's directory leaves the runner once copied back"

FAKE_QUIET=1 run_cluster replay-prepare --namespace "$ns" --manifest "$seeded"
((RC == 0)) || fail "replay-prepare exited $RC"
for slug in fake-replay-tar-zst-part00 fake-replay-tar-zst-part01 fake-seed-dump-part00; do
  grep -q "/api/v1/issues/INFRA-639/artifacts/$slug/versions/1\$" "$fake/curl.log" || fail "replay-prepare did not download $slug"
done
cmp -s "$fake/pod-dump" "$fake/parts/fake-seed-dump-part00" || fail "the dump streamed into the Postgres pod is not the seed part"
grep -q -- '^pg_restore --no-owner --no-privileges .*-d dispatch /var/lib/postgresql/copy/replay-seed.dump$' "$fake/restores" ||
  fail "replay-prepare did not pg_restore --no-owner --no-privileges into database dispatch: $(cat "$fake/restores" 2>/dev/null)"
[ -f "$fake/replay-template" ] || fail "replay-prepare did not rename the database to dispatch_replay"
jq -e --arg dump "$(jq -r .seed.dump_sha256 "$seeded")" --arg tar "$(jq -r .bundle.tar_zst_sha256 "$seeded")" \
  '.seed_dump_sha256 == $dump and .bundle_tar_zst_sha256 == $tar and .migration_before == 121 and .migration_after == 121
    and .schema_version == 121 and .checksum.md5 == "fakemd5" and (.seconds | has("restore") and has("quiet") and has("total"))' \
  "$run_dir/replay-prepared.json" >/dev/null || fail "replay-prepared.json lacks a field: $(cat "$run_dir/replay-prepared.json")"
FAKE_STORED_ORDER=nosummary run_cluster replay --namespace "$ns" r16
((RC == 0)) || fail "replay r16 exited $RC"
jq -e --arg c "$commit_so" '. == {commit: $c, exit: 1, since: "2026-10-28T10:00:00Z", documents: null, clean: null, mismatched: null, refused: null, failed: null}' \
  "$run_dir/steps/r16/stored-order.json" >/dev/null || fail "a stored-order run with no summary line does not record null counts: $(cat "$run_dir/steps/r16/stored-order.json" 2>/dev/null)"
grep -qF 'stored-order: open database: failed to connect' "$run_dir/steps/r16/stored-order.txt" || fail "stored-order.txt does not hold the command's standard error"
[ "$(grep -c -- "^-n $ns cp $run_dir/envoy-dispatch runner:/work/envoy-dispatch " "$fake/kubectl.log")" = 1 ] ||
  fail "envoy-dispatch was copied into the runner again although the runner held it since up"
! grep -rlF -- "$pg_password" "$run_dir"/steps/*/stored-order.txt "$run_dir"/steps/*/stored-order.json || fail "a stored-order file holds the Postgres password"
start=$(grep '^replay-r16 ' "$fake/runner-starts") || fail "replay r16 started no loadgen run in the runner"
for want in "env LOADTEST_ROOT=/work/root LOADTEST_GOMAXPROCS=16 /work/loadgen run -allow-foreign -source replay " \
  "-events 0 -requests 0 -replay-inputs /work/bundle -replay-rate 1 -replay-from 0 -replay-to 39m -rng-seed 1 -warmup 30s " \
  "-copy-meta /work/meta/r16.json" "-pprof http://dispatch-pprof.$ns.svc:6061" "-nats-server-name nats-0"; do
  [[ $start == *"$want"* ]] || fail "replay r16's loadgen run lacks '$want': $start"
done
for f in reset.json replay-prepared.json last-reset.json envoy-dispatch.json pg-stats-before.json pg-stats-after.json summary.json CODEPATHS.md; do
  [ -f "$run_dir/steps/r16/$f" ] || fail "the replay step directory has no $f"
done
jq -e '.template == "dispatch_replay" and .checksum.md5 == "fakemd5"' "$run_dir/steps/r16/reset.json" >/dev/null ||
  fail "replay r16's reset is not from dispatch_replay with its checksum: $(cat "$run_dir/steps/r16/reset.json")"
jq -e --arg dump "$(jq -r .seed.dump_sha256 "$seeded")" '.seed_dump_sha256 == $dump' "$run_dir/steps/r16/last-reset.json" >/dev/null ||
  fail "last-reset.json does not name the seed dump"
jq -e --arg c "$commit_ok" '.dispatch_commit == $c' "$run_dir/steps/r16/envoy-dispatch.json" >/dev/null || fail "envoy-dispatch.json does not name the commit"
{ grep -q -- "cp $run_dir/steps/r16/last-reset.json runner:/work/root/replay/last-reset.json" "$fake/kubectl.log" &&
  grep -q -- "cp $run_dir/steps/r16/envoy-dispatch.json runner:/work/root/bin/envoy-dispatch.json" "$fake/kubectl.log"; } ||
  fail "replay r16 did not put last-reset.json and envoy-dispatch.json under the runner's LOADTEST_ROOT"
cmp -s "$seeded" "$run_dir/replay-manifest.json" || fail "replay-prepare did not keep the manifest as replay-manifest.json"
grep -qx -- "codepaths -step $run_dir/steps/r16 -out $run_dir/steps/r16 -heap-start $run_dir/steps/r16/heap-start.pprof -heap-end $run_dir/steps/r16/heap-end.pprof -outage-cores 11.1,13.8 -outage-memory 13.8 -residual-notes $run_dir/replay-residual.md -window-offset 0" \
  "$fake/loadgen.log" || fail "replay r16 did not run loadgen codepaths beside the outage: $(grep '^codepaths ' "$fake/loadgen.log")"
# The fixture's seed check holds neither anchor_rules nor sign_flipped.
{
  cat "$here/replay-residual.md"
  printf '%s\n' '- Anchors by placement rule (seed check): not recorded on the logged quote, not recorded on a sentence in the logged block, not recorded on the nearest surviving block, of not recorded (share placed approximately not recorded).' \
    "- Seed versions with the size change's sign flipped (seed check): not recorded."
} | cmp -s - "$run_dir/replay-residual.md" || fail "the run's replay-residual.md is not the residual notes and the seed check's lines: $(cat "$run_dir/replay-residual.md")"
grep -qx r16 "$run_dir/phases/replay" || fail "replay r16 is not in the phase replay"
[ "$(grep -c -- "^-n $ns cp $run_dir/loadgen runner:/work/loadgen " "$fake/kubectl.log")" = 1 ] ||
  fail "loadgen was copied into the runner again although the runner held it since up"
pass "replay-prepare downloads, streams, restores and prepares dispatch_replay; replay r16 resets from it and runs loadgen run -source replay at rate 1 over 0-39m with codepaths beside the outage"

echo '400000 100000' >"$fake/cpu.max"
run_cluster db-size --namespace "$ns" 4 15Gi
((RC == 0)) || fail "db-size exited $RC"
[ "$(jq -r .postgres.tier "$run_dir/state.json")" = db-custom-4-15360 ] || fail "db-size did not record db-custom-4-15360"
grep -q 'max_connections=500' "$fake/applied.yaml" || fail "db-size did not apply db-custom-4-15360's max_connections"
echo '1600000 100000' >"$fake/cpu.max"
run_cluster db-size --namespace "$ns" 4 15Gi
((RC != 0)) || fail "db-size accepted a cpu.max that is not the new limit"
grep -q "cpu.max reads '1600000 100000', not '400000 100000'" "$tmp/out" || fail "db-size's cpu.max refusal does not name both values"

run_cluster collect --namespace "$ns" --upload-issue TEST-1
((RC == 0)) || fail "collect exited $RC"
part=$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-steps.tar.zst.part00' | head -n1)
[ -n "$part" ] || fail "collect wrote no part of phase steps"
grep -q -- "artifact --issue TEST-1 --name ${part##*/} --path $part" "$fake/dispatch.log" || fail "collect did not upload ${part##*/}"
[ "$(logged_steps 'tables -tops=false')" = "smoke reads r16" ] ||
  fail "collect's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran (smoke reads r16)"
[ "$(logged_steps 'curves -out')" = "smoke reads" ] ||
  fail "collect's loadgen curves got the steps as '$(logged_steps 'curves -out')', not in the order they ran (smoke reads)"
grep -qx -- "curves -out $run_dir/curves $run_dir/steps/smoke $run_dir/steps/reads" "$fake/loadgen.log" ||
  fail "collect did not run loadgen curves -out <run>/curves over every step directory but the replay's"
[ -f "$run_dir/curves/CURVES.md" ] || fail "collect wrote no curves/CURVES.md"
for s in smoke reads; do
  grep -qx -- "codepaths -step $run_dir/steps/$s -out $run_dir/steps/$s" "$fake/loadgen.log" ||
    fail "collect ran no loadgen codepaths -step <dir> -out <dir> in step $s"
  [ -f "$run_dir/steps/$s/CODEPATHS.md" ] || fail "collect wrote no CODEPATHS.md in step $s"
done
[ "$(grep -c -- "^codepaths -step $run_dir/steps/r16 .* -residual-notes " "$fake/loadgen.log")" = 2 ] ||
  fail "collect did not run the replay step's loadgen codepaths beside the outage"
printf '%s\n' '| smoke | 2026-10-28T10:00:00Z | 0 | 2 | 2 | 0 | 0 | 0 |' '| reads | 2026-10-28T10:00:00Z | 1 | 3 | 1 | 1 | 1 | 0 |' \
  '| r16 | 2026-10-28T10:00:00Z | 1 | null | null | null | null | null |' >"$tmp/want-stored-order"
sed -n '/^| --- /,$p' "$run_dir/STORED-ORDER.md" | tail -n +2 | diff -u "$tmp/want-stored-order" - >&2 ||
  fail "STORED-ORDER.md does not hold one row per step, in the order they ran, with their counts and exit"
grep -qF "built from commit $commit_so (binary sha256 $(jq -r .stored_order.sha256 "$run_dir/state.json"))" "$run_dir/STORED-ORDER.md" ||
  fail "STORED-ORDER.md does not name the commit envoy-dispatch was built from: $(cat "$run_dir/STORED-ORDER.md")"
! grep -qF -- "$pg_password" "$run_dir/STORED-ORDER.md" || fail "STORED-ORDER.md holds the Postgres password"
results=$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-results.tar.zst.part00' | head -n1)
[ -n "$results" ] || fail "collect wrote no part of phase results"
for f in curves/CURVES.md curves/refine.txt TABLES.md STORED-ORDER.md steps/smoke/CODEPATHS.md steps/reads/CODEPATHS.md steps/r16/CODEPATHS.md; do
  tar -tf "$results" | grep -qx -- "$f" || fail "the results tar lacks $f: $(tar -tf "$results" | tr '\n' ' ')"
done
grep -q -- "artifact --issue TEST-1 --name ${results##*/} --path $results" "$fake/dispatch.log" || fail "collect did not upload ${results##*/}"
! grep -q 'curves command' "$tmp/out" || fail "collect still says loadgen has no curves command"

: >"$fake/dispatch.log"
FAKE_LOADGEN_FAIL=curves run_cluster collect --namespace "$ns" --upload-issue TEST-1
((RC == 1)) || fail "collect with a failing loadgen curves exited $RC, not 1"
grep -q 'loadgen curves over the step directories failed' "$tmp/out" || fail "collect did not report the failed loadgen curves"
[ "$(grep -c '^codepaths ' "$fake/loadgen.log")" = 7 ] || fail "collect did not go on to loadgen codepaths after curves failed"
grep -q -- "--name ${results##*/}" "$fake/dispatch.log" || fail "collect did not go on to pack and upload the results after curves failed"
pass "collect runs tables, curves and codepaths over the steps in the order they ran, writes STORED-ORDER.md in that order, and uploads the results; a loadgen failure is reported, the rest runs, and it exits 1"

run_cluster status --namespace "$ns"
((RC == 0)) || fail "status exited $RC"
grep -q "lock: run $run" "$tmp/out" || fail "status does not show the lock"

run_cluster down --namespace "$ns"
((RC == 0)) || fail "down exited $RC"
[ ! -f "$fake/lock.json" ] || fail "down left the lock"
grep -q "kubectl -n $ns get pods,services,deployments,statefulsets,replicasets,jobs,persistentvolumeclaims,configmaps,secrets -l dispatch-loadtest/run=$run" "$tmp/out" ||
  fail "down did not print the listing of what is left"
for d in heartbeat sampler; do
  [ ! -f "$run_dir/$d.pid" ] || fail "down left the $d daemon's pidfile"
done
pass "up, seed, step, step --reads-only, replay-prepare, replay, db-size, collect, status and down run through, each step and replay with its stored-order check"

reset_fake
echo '400000 100000' >"$fake/cpu.max"
export LOADTEST_CLUSTER_ROOT=$tmp/root-all
run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --profile quick --manifest "$fake/replay-null.json"
((RC == 0)) || fail "all exited $RC"
for line in 'skipped phase refinement: profile quick runs no refinement steps' 'skipped phase replay-prepare' 'skipped phase replay r16' 'skipped phase replay r4'; do
  grep -q "$line" "$tmp/out" || fail "all did not print '$line'"
done
run_dir=$(find "$LOADTEST_CLUSTER_ROOT" -mindepth 1 -maxdepth 1 -type d)
for s in 1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4; do
  [ -f "$run_dir/steps/$s/summary.json" ] || fail "all ran no step $s"
done
[ "$(grep -c '^cluster.sh: step [^ ]*: the stored-order check is skipped: up was given no --stored-order-commit$' "$tmp/out")" = 9 ] ||
  fail "all without --stored-order-commit did not print one skip line for each of its 9 steps: $(grep 'stored-order' "$tmp/out")"
[ -z "$(find "$run_dir/steps" -name 'stored-order.*')" ] || fail "all without --stored-order-commit wrote stored-order files"
[ ! -s "$fake/stored-order.log" ] || fail "all without --stored-order-commit ran envoy-dispatch"
grep -qF 'No step ran' "$run_dir/STORED-ORDER.md" || fail "STORED-ORDER.md without the check does not say no step ran it"
[ "$(grep -c '| not run |' "$run_dir/STORED-ORDER.md")" = 9 ] || fail "STORED-ORDER.md without the check does not hold 9 rows not run"
grep -q '^step-10x-hour .*-duration 5m ' "$fake/runner-starts" || fail "quick's hour is not 5 min"
grep -q '^step-1x-db4 .*-duration 2m ' "$fake/runner-starts" || fail "quick's steps are not 2 min"
[ ! -e "$run_dir/phases/refinement" ] || fail "all --profile quick ran a refinement step"
[ "$(grep -c '^curves -out ' "$fake/loadgen.log")" = 1 ] || fail "all --profile quick ran loadgen curves outside collect"
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4" ] ||
  fail "all's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
[ -f "$run_dir/steps/1x/CODEPATHS.md" ] || fail "all's collect wrote no CODEPATHS.md"
[ ! -f "$fake/lock.json" ] || fail "all left the lock"
! grep -q '^replay-' "$fake/runner-starts" || fail "all ran a replay on a manifest whose seed is null"
pass "all runs up, seed, the series, soak, db-size, 1x-db4, collect and down, one line per skipped phase"

# line_of PATTERN [first|last]: the line number in kubectl.log of PATTERN's first (or last) match.
line_of() {
  local n
  n=$(grep -nF -- "$1" "$fake/kubectl.log" | cut -d: -f1 | if [ "${2:-first}" = last ]; then tail -n1; else head -n1; fi)
  [ -n "$n" ] || fail "kubectl.log has no '$1'"
  printf '%s\n' "$n"
}

reset_fake
echo '400000 100000' >"$fake/cpu.max"
export LOADTEST_CLUSTER_ROOT=$tmp/root-replay
FAKE_QUIET=1 run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --stored-order-commit "$commit_so" --profile quick --manifest "$seeded" --upload-issue TEST-1
((RC == 0)) || fail "all with a seeded manifest exited $RC"
! grep -q 'skipped phase replay' "$tmp/out" || fail "all skipped a replay phase with a seeded manifest: $(grep 'skipped phase replay' "$tmp/out")"
run_dir=$(find "$LOADTEST_CLUSTER_ROOT" -mindepth 1 -maxdepth 1 -type d)
for r in r16 r4; do
  start=$(grep "^replay-$r " "$fake/runner-starts") || fail "all ran no replay $r"
  for want in "-source replay " "-replay-rate 1 " "-replay-from 0 " "-replay-to 5m "; do
    [[ $start == *"$want"* ]] || fail "all --profile quick's replay $r lacks '$want': $start"
  done
  for f in pg-stats-before.json pg-stats-after.json last-reset.json; do
    [ -f "$run_dir/steps/$r/$f" ] || fail "replay $r's step directory has no $f"
  done
done
hour=$(line_of ' sh step-10x-hour ') r16=$(line_of ' sh replay-r16 ') resize=$(line_of 'rollout status statefulset/postgres' last)
db4=$(line_of ' sh step-1x-db4 ') r4=$(line_of ' sh replay-r4 ')
((hour < r16 && r16 < resize && resize < db4 && db4 < r4)) ||
  fail "all's order is not 10x-hour, replay r16, db-size, 1x-db4, replay r4 (kubectl.log lines $hour, $r16, $resize, $db4, $r4)"
[ "$(tr '\n' ' ' <"$run_dir/phases/replay")" = "r16 r4 " ] || fail "the phase replay holds '$(cat "$run_dir/phases/replay")', not r16 and r4"
part=$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-replay.tar.zst.part00' | head -n1)
{ [ -n "$part" ] && grep -q -- "--name ${part##*/} --path $part" "$fake/dispatch.log"; } || fail "all did not pack and upload the phase replay"
tar -tf "$part" | grep -q '^\./r4/\|^r4/' || fail "the phase replay's tar lacks r4: $(tar -tf "$part" | head -n5)"
[ -f "$run_dir/steps/1x/pg-stats-after.json" ] || fail "all's step 1x has no pg-stats-after.json"
! grep -q -- "^curves -out .*/steps/r16" "$fake/loadgen.log" || fail "all's loadgen curves took a replay step"
# all --stored-order-commit: every step and replay runs the check while Dispatch runs, before the
# next scale-down of Dispatch (a reset, replay-prepare or db-size).
for s in 1x 2x 3x 5x 7x 10x 10x-reads 10x-hour r16 1x-db4 r4; do
  jq -e '.exit == 0 and .documents == 2' "$run_dir/steps/$s/stored-order.json" >/dev/null || fail "all's step $s has no clean stored-order.json"
done
[ "$(awk '/ scale deployment dispatch --replicas / { up = $NF } /envoy-dispatch stored-order/ { n++; if (up != 1) bad++ } END { print n + 0, bad + 0 }' "$fake/kubectl.log")" = "11 0" ] ||
  fail "all did not run the stored-order check once per step and replay, each while Dispatch was scaled up"
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 10x-hour r16 1x-db4 r4" ] ||
  fail "all's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
pass "all with a seeded manifest runs replay-prepare and replay r16 after soak, replay r4 after db-size and 1x-db4, each over 0-5m under quick, and packs the phase replay"

reset_fake
echo '400000 100000' >"$fake/cpu.max"
printf '8.5\n' >"$fake/refine.1"
export LOADTEST_CLUSTER_ROOT=$tmp/root-full
FAKE_QUIET=1 run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --profile full --manifest "$seeded"
((RC == 0)) || fail "all --profile full exited $RC"
run_dir=$(find "$LOADTEST_CLUSTER_ROOT" -mindepth 1 -maxdepth 1 -type d)
[ "$(cat "$run_dir/phases/refinement" 2>/dev/null)" = 8.5x ] ||
  fail "the refinement ran '$(cat "$run_dir/phases/refinement" 2>/dev/null)', not exactly the one step 8.5x"
[ "$(grep -c '^step-' "$fake/runner-starts")" = 10 ] ||
  fail "all --profile full ran $(grep -c '^step-' "$fake/runner-starts") steps, not the nine of the plan and one refinement step"
start=$(grep '^step-8.5x ' "$fake/runner-starts") || fail "the refinement started no loadgen run for 8.5x"
[[ $start == *"-events 34 "* && $start == *"-requests 510 "* && $start == *"-duration 10m "* ]] ||
  fail "step 8.5x is not 34 events/s and 510 requests/s for 10 min: $start"
[ "$(grep -c '^curves -out ' "$fake/loadgen.log")" = 3 ] ||
  fail "loadgen curves ran $(grep -c '^curves -out ' "$fake/loadgen.log") times, not twice in the refinement and once in collect"
grep -q 'refinement done: refine.txt is empty; refinement steps run: 1;' "$tmp/out" || fail "all did not say the refinement is done"
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 8.5x 10x-hour r16 1x-db4 r4" ] ||
  fail "all --profile full's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
grep -q '^step-10x-hour .*-duration 60m ' "$fake/runner-starts" || fail "full's hour is not 60 min"
for r in r16 r4; do
  grep -q "^replay-$r .*-replay-from 0 -replay-to 39m " "$fake/runner-starts" || fail "all --profile full's replay $r is not over 0-39m"
done
[ -n "$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-refinement.tar.zst.part00')" ] || fail "all packed no phase refinement"
[ ! -f "$fake/lock.json" ] || fail "all --profile full left the lock"
pass "all --profile full runs one refinement step per refine.txt factor, then curves again, until refine.txt is empty, and each replay over 0-39m"

[ -s "$fake/kubectl.log" ] || fail "no kubectl call was recorded"
if [ -s "$fake/kubectl-no-ns.log" ]; then
  fail "kubectl calls without -n $ns: $(head -n5 "$fake/kubectl-no-ns.log")"
fi
pass "every kubectl call ($(wc -l <"$fake/kubectl.log") of them, the heartbeat's and sampler's included) carries -n $ns"

# ---- (d) apply_manifest's retry of the quota race ---------------------------------------------------

readonly race='Error from server (Conflict): Operation cannot be fulfilled on resourcequotas "loadtest": the object has been modified; please apply your changes to the latest version and try again'
readonly forbidden='Error from server (Forbidden): error when creating "STDIN": statefulsets.apps "postgres" is forbidden: exceeded quota: loadtest, requested: requests.cpu=16, used: requests.cpu=60, limited: requests.cpu=64'

# applies: the number of kubectl apply calls so far.
applies() { grep -c "^-n $ns apply " "$fake/kubectl.log" || true; }

reset_fake
printf '%s\n' "$race" >"$fake/apply-errors"
FAKE_APPLY_FAIL=nats.yaml run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up with one quota race on nats.yaml exited $RC"
grep -qF 'kubectl apply of manifests/nats.yaml: the quota was updated under it (try 1 of 5); retrying in 3 s' "$tmp/out" ||
  fail "up's log does not name the retry of nats.yaml"
[ "$(cat "$fake/sleep.log" 2>/dev/null)" = 3 ] || fail "up did not wait 3 s exactly once before the retry: $(cat "$fake/sleep.log" 2>/dev/null)"
[ "$(applies)" = 6 ] || fail "up made $(applies) applies, not the five manifests' and one retry"
[ "$(grep -c '^# nats.yaml:' "$fake/applied.yaml")" = 1 ] || fail "nats.yaml was not applied once after the race"
grep -q '^# dispatch.yaml:' "$fake/applied.yaml" || fail "up did not go on to the manifests after nats.yaml"
grep -q 'the copy is up' "$tmp/out" || fail "up did not finish after the retry"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down after the retried up exited $RC"
pass "up retries an apply refused by the quota race 3 s later, says so, and goes on"

reset_fake
printf '%s\n' "$forbidden" "$forbidden" >"$fake/apply-errors"
FAKE_APPLY_FAIL=postgres.yaml run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 1)) || fail "up with a quota refusal of postgres.yaml exited $RC, not 1"
grep -qF 'kubectl apply of manifests/postgres.yaml failed' "$tmp/out" || fail "up's failure does not name manifests/postgres.yaml"
grep -qF "$forbidden" "$tmp/out" || fail "up's failure does not show kubectl's error"
! grep -q 'retrying' "$tmp/out" || fail "up retried an apply error that is not the quota race"
[ ! -s "$fake/sleep.log" ] || fail "up waited before failing on an apply error that is not the quota race"
[ "$(applies)" = 2 ] || fail "up made $(applies) applies, not secrets.yaml.tmpl's and postgres.yaml's one"
! grep -q '^# nats.yaml:' "$fake/applied.yaml" || fail "up went on to nats.yaml after postgres.yaml failed"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down after the failed up exited $RC"
pass "up stops at once on an apply error other than the quota race, naming the manifest and showing the error"

reset_fake
printf '%s\n' "$race" "$race" "$race" "$race" "$race" "$race" >"$fake/apply-errors"
FAKE_APPLY_FAIL=nats.yaml run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 1)) || fail "up with six quota races on nats.yaml exited $RC, not 1"
grep -qF '(try 4 of 5); retrying in 3 s' "$tmp/out" || fail "up did not retry the fourth race"
! grep -qF '(try 5 of 5)' "$tmp/out" || fail "up retried after the fifth try"
grep -qF 'kubectl apply of manifests/nats.yaml failed' "$tmp/out" || fail "up's failure after five races does not name manifests/nats.yaml"
grep -qF "$race" "$tmp/out" || fail "up's failure after five races does not show kubectl's error"
[ "$(wc -l <"$fake/sleep.log")" = 4 ] || fail "up waited $(wc -l <"$fake/sleep.log") times, not 4, between five tries"
[ "$(applies)" = 7 ] || fail "up made $(applies) applies, not two manifests' and four retries"
[ "$(wc -l <"$fake/apply-errors")" = 1 ] || fail "up did not stop at the fifth try"
! grep -q '^# listener.yaml:' "$fake/applied.yaml" || fail "up went on to listener.yaml after five races on nats.yaml"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down after the failed up exited $RC"
pass "up stops after five quota races in a row on one manifest"

# ---- (e) templates ---------------------------------------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  digest=sha256:$(printf '%064d' 7)
  values=(NAMESPACE=loadtest RUN_ID=lt-20261028t120000-abcd STORAGE_CLASS=premium-rwo
    "ENVOY_IMAGE=ghcr.io/sjawhar/legion/envoy@$digest" "LISTENER_IMAGE=ghcr.io/sjawhar/legion/envoy@$digest"
    "PG_IMAGE=mirror.gcr.io/pgvector/pgvector@$digest" "SOCAT_IMAGE=mirror.gcr.io/alpine/socat@$digest"
    "RUNNER_IMAGE=mirror.gcr.io/library/debian@$digest")
  mapfile -t -O "${#values[@]}" values < <(pg_tier 16 60 | jq -r '"PG_TIER=\(.tier)", "PG_CPU=\(.cpu)", "PG_MEMORY=\(.memory)",
    (.settings | to_entries[] | "PG_\(.key | ascii_upcase)=\(.value)")')
  values+=("PG_PASSWORD=$(random_hex 24)" "DISPATCH_TOKEN=$(random_hex 32)" "LISTENER_TOKEN=$(random_hex 32)" "SIGNING_KEY=$(random_hex 32)")
  for m in secrets.yaml.tmpl postgres.yaml nats.yaml listener.yaml dispatch.yaml runner.yaml; do
    out=$(render_manifest "$MANIFESTS/$m" "${values[@]}") || fail "$m did not render"
    ! grep -qE '__[A-Z][A-Z0-9_]*__' <<<"$out" || fail "$m keeps a placeholder once rendered"
    if grep -E '^[[:space:]]*image:' <<<"$out" | grep -vqE '@sha256:[0-9a-f]{64}$'; then
      fail "$m names an image not by digest"
    fi
  done
  grep -q 'shared_buffers=20480MB' <(render_manifest "$MANIFESTS/postgres.yaml" "${values[@]}") ||
    fail "postgres.yaml does not carry db-custom-16-61440's shared_buffers"
  if (render_manifest "$MANIFESTS/runner.yaml" "${values[@]}" RUNNER_IMAGE=mirror.gcr.io/library/debian:bookworm-slim) 2>/dev/null; then
    fail "render accepted an image given by tag"
  fi
  if (render_manifest "$MANIFESTS/runner.yaml" NAMESPACE=loadtest) 2>/dev/null; then
    fail "render accepted a manifest with placeholders left"
  fi
)
pass "every manifest renders with no placeholder left and every image by digest; a tag or a missing value is refused"

# ---- (f) a restart between the two reads ------------------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run lt-20261028t120000-abcd
  dir=$tmp/restart-step
  mkdir -p "$dir"
  write_pods 0
  container_snapshot "$dir/before.tsv"
  write_pods 1 OOMKilled
  container_snapshot "$dir/after.tsv"
  crash_check "$dir/before.tsv" "$dir/after.tsv" "$dir" 2>/dev/null
  jq -e '(.reasons | length) == 1 and .reasons[0].kind == "restart" and .reasons[0].role == "dispatch"
    and (.reasons[0].detail | test("restartCount 0 -> 1") and test("OOMKilled"))' "$dir/step-failed.json" >/dev/null ||
    fail "step-failed.json does not hold the restart: $(cat "$dir/step-failed.json" 2>/dev/null)"
  record_failure "$dir" loadgen_exit "" "loadgen run ended with exit status 1" 2>/dev/null
  jq -e '(.reasons | map(.kind)) == ["restart", "loadgen_exit"]' "$dir/step-failed.json" >/dev/null ||
    fail "a second reason did not keep the first"
)
pass "a restart between the before and after container reads writes step-failed.json kind restart"

# ---- (g) the sampler --------------------------------------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  # shellcheck disable=SC2030 # this root stays in (g)'s subshell; (i) sources cluster.sh in its own
  LOADTEST_CLUSTER_ROOT=$tmp/root-sampler
  CLUSTER_ROOT=$LOADTEST_CLUSTER_ROOT
  set_run lt-20261028t120000-abcd
  mkdir -p "$RUN_DIR/tmp"
  write_pods 0
  out=$RUN_DIR/cgroup.jsonl
  FAKE_EXEC_FAIL=1 sample_role_once dispatch "$out"
  [ "$(wc -l <"$out")" = 1 ] || fail "a failed exec wrote $(wc -l <"$out") lines, not 1"
  jq -e '.role == "dispatch" and .pod == "dispatch-5d9c" and .container == "envoy-dispatch" and (.error | test("container not found"))
    and .usage_usec == null and .throttled_usec == null and .memory_current == null and .inactive_file == null' "$out" >/dev/null ||
    fail "the failed exec's line is not an error line with no number: $(cat "$out")"
  : >"$out"
  FAKE_SAMPLE=1 sample_role_once nats-1 "$out"
  head -n1 "$out" | jq -e '.role == "nats-1" and .pod == "nats-1" and .container == "nats" and .usage_usec == 123456
    and .throttled_usec == 0 and .cpu_max == "1600000 100000" and .memory_current == 1048576 and .inactive_file == 4096
    and .error == "" and (.at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}\\.[0-9]{6}Z$"))' >/dev/null ||
    fail "the sampler's numeric line is wrong: $(head -n1 "$out")"
  tail -n1 "$out" | jq -e '.error != "" and .usage_usec == null' >/dev/null || fail "the end of an exec stream wrote no error line"
)
pass "the sampler writes an error line with no number when its exec fails, and a numeric line when it reads"

# ---- (h) nats.yaml against a fresh render ------------------------------------------------------------

if PATH=$real_path command -v go >/dev/null &&
  PATH=$real_path curl -fsS --max-time 20 -o /dev/null https://nats-io.github.io/k8s/helm/charts/index.yaml 2>/dev/null; then
  # shellcheck disable=SC2031 # (e) to (g) source cluster.sh in subshells; its here and tmp stay there
  PATH=$real_path bash "$here/render-nats.sh" --pin-from "$here/manifests/nats.yaml" --out "$tmp/nats.yaml" 2>"$tmp/out" ||
    fail "render-nats.sh failed"
  # shellcheck disable=SC2031 # as above
  diff -u "$here/manifests/nats.yaml" "$tmp/nats.yaml" >"$tmp/out" ||
    fail "manifests/nats.yaml differs from a fresh render; run render-nats.sh and commit its output"
  pass "manifests/nats.yaml equals a fresh render of chart nats/nats 2.15.1 with nats-values.yaml"
else
  printf 'SKIP: go or the network does not reach helm and the chart, so manifests/nats.yaml was not compared with a fresh render\n'
fi

# ---- (i) the replay residual's seed-check lines -------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  dir=$tmp/residual
  mkdir -p "$dir"
  printf -- '- First note.\n- Second note.\n' >"$dir/notes.md"
  write_manifest "$dir/full.json" '{"passed": true, "version_size_mismatches": 0, "anchors_outside_logged_block": 0, "sign_flipped": 21,
    "anchor_rules": {"logged_quote": 4110, "in_logged_block": 960, "nearest_surviving_block": 7002}}'
  printf '%s\n' '- First note.' '- Second note.' \
    '- Anchors by placement rule (seed check): 4110 on the logged quote, 960 on a sentence in the logged block, 7002 on the nearest surviving block, of 12072 (58.0% placed approximately).' \
    "- Seed versions with the size change's sign flipped (seed check): 21." >"$dir/want"
  replay_residual_notes "$dir/notes.md" "$dir/full.json" >"$dir/got" || fail "replay_residual_notes failed on a full seed check"
  diff -u "$dir/want" "$dir/got" >&2 || fail "replay_residual_notes of a full seed check is not the notes and the two exact lines"

  write_manifest "$dir/no-sign.json" '{"passed": true, "version_size_mismatches": 0, "anchors_outside_logged_block": 0,
    "anchor_rules": {"logged_quote": 2, "in_logged_block": 0, "nearest_surviving_block": 1}}'
  printf '%s\n' '- First note.' '- Second note.' \
    '- Anchors by placement rule (seed check): 2 on the logged quote, 0 on a sentence in the logged block, 1 on the nearest surviving block, of 3 (33.3% placed approximately).' \
    "- Seed versions with the size change's sign flipped (seed check): not recorded." >"$dir/want"
  replay_residual_notes "$dir/notes.md" "$dir/no-sign.json" >"$dir/got" || fail "replay_residual_notes failed on a seed check without sign_flipped"
  diff -u "$dir/want" "$dir/got" >&2 || fail "replay_residual_notes does not write a missing sign_flipped as not recorded"

  printf '%s\n' '- First note.' '- Second note.' \
    '- Anchors by placement rule (seed check): not recorded on the logged quote, not recorded on a sentence in the logged block, not recorded on the nearest surviving block, of not recorded (share placed approximately not recorded).' \
    "- Seed versions with the size change's sign flipped (seed check): not recorded." >"$dir/want"
  replay_residual_notes "$dir/notes.md" "$dir/absent.json" >"$dir/got" || fail "replay_residual_notes failed with no manifest"
  diff -u "$dir/want" "$dir/got" >&2 || fail "replay_residual_notes with no manifest does not write every count as not recorded"
)
pass "the replay residual is the notes, then the seed check's anchors by placement rule and sign-flipped versions, a missing count as not recorded"

# ---- (j) stored-order.json from the command's output ---------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  dir=$tmp/stored-order
  mkdir -p "$dir"
  since=2026-10-28T10:00:00.123456789Z
  doc='stored-order: 11111111-1111-4111-8111-111111111111 base=4 updates_checked=9 updates_skipped=3 versions_checked=2 versions_skipped=1 mismatches=0'
  printf '%s\n' "$doc" 'stored-order: documents=1 clean=1 mismatched=0 refused=0 failed=0' >"$dir/clean.txt"
  printf '%s\n' "$doc" 'stored-order: 11111111-1111-4111-8111-111111111111 version 3: markdown md5 0cc175b9 length 1, folded md5 92eb5ffe length 2' \
    'stored-order: 22222222-2222-4222-8222-222222222222 error (read updates: context canceled)' \
    'stored-order: documents=4 clean=1 mismatched=2 refused=0 failed=1' >"$dir/mismatch.txt"
  printf '%s\n' 'stored-order: open database: failed to connect' 'command terminated with exit code 1' >"$dir/missing.txt"
  printf '%s\n' "$doc" 'stored-order: documents=2 clean=2' >"$dir/truncated.txt"
  printf '%s\n' "$doc" 'stored-order: documents=1 clean=1 mismatched=0 refused=0 failed=0' 'stored-order: timed out after 1800 s (exit 124)' >"$dir/timeout.txt"
  printf '%s\n' 'stored-order: not run: x/meta.json holds no RFC 3339 window_start' >"$dir/notrun.txt"
  nulls='documents: null, clean: null, mismatched: null, refused: null, failed: null'
  for c in "clean|0|$since|{commit: \$c, exit: 0, since: \$s, documents: 1, clean: 1, mismatched: 0, refused: 0, failed: 0}" \
    "mismatch|1|$since|{commit: \$c, exit: 1, since: \$s, documents: 4, clean: 1, mismatched: 2, refused: 0, failed: 1}" \
    "missing|1|$since|{commit: \$c, exit: 1, since: \$s, $nulls}" \
    "truncated|1|$since|{commit: \$c, exit: 1, since: \$s, $nulls}" \
    "timeout|124|$since|{commit: \$c, exit: 124, since: \$s, $nulls}" \
    "notrun|null||{commit: \$c, exit: null, since: null, $nulls}"; do
    IFS='|' read -r name rc s want <<<"$c"
    stored_order_summary "$dir/$name.txt" "$commit_so" "$rc" "$s" >"$dir/$name.json" || fail "stored_order_summary failed on the $name output"
    jq -e --arg c "$commit_so" --arg s "$since" ". == $want" "$dir/$name.json" >/dev/null ||
      fail "stored-order.json of the $name output is wrong: $(cat "$dir/$name.json")"
  done
)
pass "stored-order.json holds a clean and a mismatching run's counts, and nulls, never 0, for a missing or truncated summary line, a timeout and a check not run"
