#!/usr/bin/env bash
# packages/envoy/loadtest/cluster/cluster.test.sh
#
# Proves cluster.sh's behaviour without a cluster and without a network: fakes `kubectl`, `gh`,
# `curl`, `go`, `uv`, `dispatch`, `hostname` and `sleep` on PATH. The fake kubectl records every
# call and answers from files the test writes (the run's pods, the lock ConfigMap, cpu.max, the
# errors an apply fails with), the fake curl answers registry manifest reads with a digest (or 404
# for the refs the test lists), the fake gh serves the production repository's two files and a
# commit's first parent, and the fake sleep returns at once from apply_manifest's 3 s wait.
#
# Asserts that: (a) every subcommand refuses a missing --namespace; (b) up refuses a commit that is
# not 40 hex, a commit with no image (naming its newest first-parent ancestor with one) and a
# --pg-major other than the production repository's (naming both), each before any kubectl call
# creates anything, and refuses with exit 2 while the lock exists, naming the holder and the
# heartbeat's age, and past 15 min calling it stale and naming the down command; down refuses
# another run's lock; (c) up, seed, step, db-size, collect, status, down and all run through to
# the end, and every kubectl call any of them makes, the background heartbeat and sampler
# included, carries `-n <namespace>`; collect runs loadgen tables and curves over the steps in the
# order they ran (meta.json start, not the directory names' order), loadgen codepaths in each step
# directory, and packs and uploads curves/ and TABLES.md as phase results; all --profile quick runs
# no refinement and says so, and all --profile full, given a refine.txt with one factor and then
# an empty one, runs exactly one refinement step <factor>x; (d) up retries an apply the
# namespace's ResourceQuota race refuses ("the object has been modified") 3 s later and goes on,
# stops at once on any other apply error, naming the manifest and showing the error, and stops
# after five races in a row; (e) every manifest renders with no placeholder left and every image
# by digest, and a value of the wrong shape is refused; (f) a container restart between the
# before and after reads writes step-failed.json with kind restart; (g) the sampler writes an
# error line with no number when its exec fails, and a numeric line when it reads; (h) when go
# and the network reach helm, render-nats.sh re-renders nats.yaml equal to the committed file
# (else it says it skipped that).
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
verb="" sel="" prev="" has_ns=false seen=false cmd=()
for a in "$@"; do
  if $seen; then
    cmd+=("$a")
  elif [ "$a" = -- ]; then
    seen=true
  else
    [ "$prev" = -n ] && [ "$a" = "$FAKE_NS" ] && has_ns=true
    [ "$prev" = -l ] && sel=$a
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
      *"/healthz") echo '{"ok":true,"nats":true}' ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 stream info "*" --json") echo '{"config":{"num_replicas":3}}' ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 stream ls --names") echo ENVOY_NOTIFICATIONS ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 kv ls --names") echo interests ;;
      "nats --server nats://nats.$FAKE_NS.svc:4222 "*) ;;
      *"datname = 'dispatch_seed'"*) if [ -f "$FAKE_DIR/seeded" ]; then echo 1; else echo 0; fi ;;
      *"rename to dispatch_seed"*) : >"$FAKE_DIR/seeded" ;;
      *"select version()"*) echo 'PostgreSQL 16.4 (fake)' ;;
      *"show all"*) echo 'max_connections|800|Sets the maximum number of concurrent connections.' ;;
      *"max(version)"*) echo 121 ;;
      *json_build_object*) echo '{"projects": 4, "issues": 1000}' ;;
      psql*) ;;
      *cpuinfo*) printf 'model name\t: Fake CPU @ 2.00GHz\n' ;;
      "cat /sys/fs/cgroup/cpu.max") cat "$FAKE_DIR/cpu.max" ;;
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
      *)
        echo "fake kubectl exec: unexpected command: $c" >&2
        exit 1
        ;;
    esac
    ;;
  logs) echo 'fake log line' ;;
  cp | scale | rollout | wait | patch) ;;
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
# FAKE_DIR/missing-refs; a token read answers a token.
printf '%s\n' "$*" >>"$FAKE_DIR/curl.log"
url=${*: -1}
case $url in
  */token\?*) echo '{"token": "anonymous"}' ;;
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
# The fake go: `go build -o PATH ./loadtest` writes a fake loadgen to PATH.
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
  run) [ "${2:-}" != -h ] || echo '  -events float' >&2 ;;
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
unset FAKE_EXEC_FAIL FAKE_SAMPLE FAKE_PROD_MAJOR FAKE_LOADGEN_FAIL FAKE_APPLY_FAIL

readonly commit_ok=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly commit_none=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly commit_parent=cccccccccccccccccccccccccccccccccccccccc
readonly commit_ancestor=dddddddddddddddddddddddddddddddddddddddd
printf '%s %s\n%s %s\n' "$commit_none" "$commit_parent" "$commit_parent" "$commit_ancestor" >"$fake/parents"
printf '%s\n%s\n' "$commit_none" "$commit_parent" >"$fake/missing-refs"
jq -n '{window_start: "2026-10-28T10:00:00Z", window_end: "2026-10-28T10:01:00Z"}' >"$fake/stepdir/meta.json"
echo '{"config": {"step": "fake"}}' >"$fake/stepdir/summary.json"

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
    "$fake/sleep.log" "$fake/apply-errors"
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
for sub in up seed step series soak db-size redeploy collect status down all; do
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
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
run=$(jq -r .data.run "$fake/lock.json")
run_dir=$LOADTEST_CLUSTER_ROOT/$run
[ "$(jq -r '.data."holder-pod"' "$fake/lock.json")" = test-worker-0 ] || fail "the lock does not name this pod"
for key in holder-pod holder-uid issue run started heartbeat; do
  [ -n "$(jq -r --arg k "$key" '.data[$k] // empty' "$fake/lock.json")" ] || fail "the lock has no $key"
done
[ "$(stat -c %a "$run_dir/state.json")" = 600 ] || fail "state.json is not mode 600"
grep -q 'requests .*limits' "$tmp/out" || fail "up did not print the containers' requests and limits"

run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC"
[ -f "$run_dir/seed.json" ] || fail "seed kept no seed.json in the run directory"

run_cluster step --namespace "$ns" smoke 1 --duration 1m
((RC == 0)) || fail "step exited $RC"
for f in reset.json copy-meta.json meta.json summary.json postgres-settings.txt containers-before.tsv containers-after.tsv; do
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

run_cluster step --namespace "$ns" reads 10 --duration 1m --reads-only
((RC == 0)) || fail "step --reads-only exited $RC"
start=$(grep '^step-reads ' "$fake/runner-starts")
[[ $start == *"-events 0 "* && $start == *"-requests 600 "* ]] || fail "a --reads-only step at 10x is not 0 events and 600 requests: $start"

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
[ "$(logged_steps 'tables -tops=false')" = "smoke reads" ] ||
  fail "collect's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran (smoke reads)"
[ "$(logged_steps 'curves -out')" = "smoke reads" ] ||
  fail "collect's loadgen curves got the steps as '$(logged_steps 'curves -out')', not in the order they ran (smoke reads)"
grep -qx -- "curves -out $run_dir/curves $run_dir/steps/smoke $run_dir/steps/reads" "$fake/loadgen.log" ||
  fail "collect did not run loadgen curves -out <run>/curves over every step directory"
[ -f "$run_dir/curves/CURVES.md" ] || fail "collect wrote no curves/CURVES.md"
for s in smoke reads; do
  grep -qx -- "codepaths -step $run_dir/steps/$s -out $run_dir/steps/$s" "$fake/loadgen.log" ||
    fail "collect ran no loadgen codepaths -step <dir> -out <dir> in step $s"
  [ -f "$run_dir/steps/$s/CODEPATHS.md" ] || fail "collect wrote no CODEPATHS.md in step $s"
done
results=$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-results.tar.zst.part00' | head -n1)
[ -n "$results" ] || fail "collect wrote no part of phase results"
for f in curves/CURVES.md curves/refine.txt TABLES.md steps/smoke/CODEPATHS.md steps/reads/CODEPATHS.md; do
  tar -tf "$results" | grep -qx -- "$f" || fail "the results tar lacks $f: $(tar -tf "$results" | tr '\n' ' ')"
done
grep -q -- "artifact --issue TEST-1 --name ${results##*/} --path $results" "$fake/dispatch.log" || fail "collect did not upload ${results##*/}"
! grep -q 'curves command' "$tmp/out" || fail "collect still says loadgen has no curves command"

: >"$fake/dispatch.log"
FAKE_LOADGEN_FAIL=curves run_cluster collect --namespace "$ns" --upload-issue TEST-1
((RC == 1)) || fail "collect with a failing loadgen curves exited $RC, not 1"
grep -q 'loadgen curves over the step directories failed' "$tmp/out" || fail "collect did not report the failed loadgen curves"
[ "$(grep -c '^codepaths ' "$fake/loadgen.log")" = 4 ] || fail "collect did not go on to loadgen codepaths after curves failed"
grep -q -- "--name ${results##*/}" "$fake/dispatch.log" || fail "collect did not go on to pack and upload the results after curves failed"
pass "collect runs tables, curves and codepaths over the steps in the order they ran and uploads the results; a loadgen failure is reported, the rest runs, and it exits 1"

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
pass "up, seed, step, step --reads-only, db-size, collect, status and down run through"

reset_fake
echo '400000 100000' >"$fake/cpu.max"
export LOADTEST_CLUSTER_ROOT=$tmp/root-all
run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --profile quick
((RC == 0)) || fail "all exited $RC"
for line in 'skipped phase refinement: profile quick runs no refinement steps' 'skipped phase replay-prepare' 'skipped phase replay r16' 'skipped phase replay r4'; do
  grep -q "$line" "$tmp/out" || fail "all did not print '$line'"
done
run_dir=$(find "$LOADTEST_CLUSTER_ROOT" -mindepth 1 -maxdepth 1 -type d)
for s in 1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4; do
  [ -f "$run_dir/steps/$s/summary.json" ] || fail "all ran no step $s"
done
grep -q '^step-10x-hour .*-duration 5m ' "$fake/runner-starts" || fail "quick's hour is not 5 min"
grep -q '^step-1x-db4 .*-duration 2m ' "$fake/runner-starts" || fail "quick's steps are not 2 min"
[ ! -e "$run_dir/phases/refinement" ] || fail "all --profile quick ran a refinement step"
[ "$(grep -c '^curves -out ' "$fake/loadgen.log")" = 1 ] || fail "all --profile quick ran loadgen curves outside collect"
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4" ] ||
  fail "all's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
[ -f "$run_dir/steps/1x/CODEPATHS.md" ] || fail "all's collect wrote no CODEPATHS.md"
[ ! -f "$fake/lock.json" ] || fail "all left the lock"
pass "all runs up, seed, the series, soak, db-size, 1x-db4, collect and down, one line per skipped phase"

reset_fake
echo '400000 100000' >"$fake/cpu.max"
printf '8.5\n' >"$fake/refine.1"
export LOADTEST_CLUSTER_ROOT=$tmp/root-full
run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --profile full
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
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 8.5x 10x-hour 1x-db4" ] ||
  fail "all --profile full's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
grep -q '^step-10x-hour .*-duration 60m ' "$fake/runner-starts" || fail "full's hour is not 60 min"
[ -n "$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-refinement.tar.zst.part00')" ] || fail "all packed no phase refinement"
[ ! -f "$fake/lock.json" ] || fail "all --profile full left the lock"
pass "all --profile full runs one refinement step per refine.txt factor, then curves again, until refine.txt is empty"

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
