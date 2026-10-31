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
# --pg-major other than the production repository's (naming both), stored-order flags it cannot
# build a checker from (a dispatch commit the git store lacks; a dispatch tree without the
# stored-order command and no --stored-order-source; a --stored-order-source beside a dispatch
# tree that has the command; a source that is not 40 hex, that the git store lacks or whose tree
# lacks INFRA-707's files; --stored-order-source with --stored-order-checker-commit), an overlay
# whose dispatch subcommand table lacks the registration's anchor, and a checker that does not
# compile (naming the dispatch commit, the source commit and the compiler's first error), and a
# --browsertype-commit that the git store does not hold or whose tree has no loadtest/browsertype
# (naming it), each before any kubectl call creates anything and leaving no scratch directory,
# and refuses with exit 2 while the lock exists,
# naming the holder and the heartbeat's age, and past 15 min calling it stale and naming the down
# command; down refuses another run's lock; replay-prepare refuses a manifest whose seed is null and
# a seed check that failed (check.passed false, version_size_mismatches or
# anchors_outside_logged_block not 0), each before any kubectl call; (c) up, seed, step,
# replay-prepare, replay, db-size, collect, status, down and all run through to the end, and every
# kubectl call any of them makes, the background heartbeat and sampler included, carries
# `-n <namespace>`; each step and replay writes pg-stats-before.json and pg-stats-after.json; a
# step's reset looks up the nats-box pod once; loadgen, envoy-dispatch and seed.json go into the
# runner only when it does not hold them, and a step's directory is removed from the runner once
# copied back; up builds the stored-order checker from the dispatch commit's own archive with
# GOWORK=off and CGO_ENABLED=0 and records its fields (dispatch_commit, built_from, overlay_source
# null, overlay_files [], registration null, renderer_matches true) and sha256; with
# --stored-order-source it copies the source's INFRA-707 files into the dispatch tree's scratch
# copy and registers the source's stored-order entry after the table opener, recording the
# overlay; with --stored-order-checker-commit it builds wholly from that commit, warns, and records
# renderer_matches false; each step and replay then runs
# stored-order --since its window start, bounded at 7200 s, with DATABASE_URL set, the password on
# no command line and
# in no recorded file, and writes stored-order.txt and stored-order.json (a clean run's counts, a
# mismatching run's, which is no step failure, and nulls for a missing summary line); a mismatching
# run exports each mismatched or refused document's rows (one psql \copy to stdout per table in the
# Postgres pod, from database dispatch, which the fake kubectl records and answers), its issue's
# events since the window start, mismatched versions' markdown, and its start-* tables from the
# template dispatch_seed into stored-order-evidence/<artifact>/, each psql call's statement_timeout
# within the export's 600 s, a failed table reported and no step failure, stored-order.json
# evidence naming each file's rows, and collect's tar parts leave it out; a clean run exports
# nothing; all runs the check on each of its steps;
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
# holds the checker's fields and a clean run's and a mismatching run's counts, and nulls (never 0)
# for a missing summary line, a timeout (exit 124) and a check not run, and renderer_matches false
# after a redeploy to another commit; (k) the evidence export takes each mismatched or
# refused document once with its mismatched versions, never a clean or failed one, and counts CSV
# records past quoted line breaks; (l) a replay step's start-* tables come from dispatch_replay, and
# a step whose reset.json names no template exports none, naming each; (c) step and replay refuse
# --typing on a run without --browsertype-commit, naming it, before anything; up
# --browsertype-commit builds browsertype with the stored-order build code and records it; step
# and replay --typing write the agent token into the runner from standard input (on no command
# line, in no file under the step directory, never printed), start browsertype with the step's
# flags before loadgen's run and stop it with SIGTERM after the stored-order commands (their order
# in the fake kubectl's log), type into the K documents loadgen plan-docs names from the step's own
# flags, run stored-order over those documents (one --artifact each) before the run over every
# document, both within the bound, and write stored-order-typed.txt, stored-order-typed.json,
# browsertype.txt and browsertype.json (with each typed document's planned versions), a peer error
# being no step failure; collect's
# STORED-ORDER.md has the typed columns and a typing column (sent updates, or none) and its curves
# leave typing steps
# out; (m) a replay's typing documents are the most-written documents of its sub-window in the
# bundle fixture, mapped through replay_ids, and a load step's are loadgen plan-docs' documents,
# with their planned versions and a warning when the Kth has fewer than 10; (n) a read-only
# kubectl exec is retried once 5 s after a dial timeout or a refused
# or reset API server connection, never after another error, and a write is never retried; (o) the
# heartbeat renews the lock only while a command of its run is alive, so an abandoned run's lock
# goes stale; (p) stored_order_summary reads a summary line that ends with `unchecked=N` and one
# without it; (q) a step whose copy-back from the runner failed is marked failed with kind
# copy_back, keeps no truncated file of the cut stream, and leaves its files in the runner;
# (r) the sampler stops with its run's heartbeat, with the kubectl exec of every role loop, and
# removes its pidfile; (s) all refuses --keep-for without --keep, cluster.sh hold holds a run
# whose commands have all ended (its heartbeat keeps patching the lock, and status shows the
# hold's expiry), and an expired hold stops it; (t) up's refusal of a stale lock names the
# holder's pod and issue and says to confirm with that issue's holder that the run is abandoned
# before the down command it then gives; (u) a run whose state.json records another PID namespace
# refuses every command that would drive it, registering nothing and starting no daemon, while
# status still reads it and down still deletes it, saying that the daemons it cannot see stop on
# their own; (v) up applies a Postgres claim of its --pg-volume and refuses one above what the
# namespace's quota leaves beside NATS's claims, a step that starts under the free-space
# threshold sends no load, one whose free space falls under it mid-run has its runner job stopped
# by the disk watcher, both are marked failed with kind pg_disk, and each step writes pg-disk.json
# with the volume's size, the used bytes around the load, the peak, the least free and the
# out; (w) the disk watcher and the runner-job wait of a step killed outright stop at their
# next poll, recording no failure in that step's directory and stopping no runner job, however
# little free space the volume then shows, a wait past its deadline stops no job once the lock
# names another run and the step's EXIT trap then stops no browser typing and removes no token
# there, while a wait with no pid and start time still stops its job while the lock is its own
# run's; (x) a step whose heap.txt alone did not come back from the runner is no
# failed step: its summary.json is here, it says in a note and in heap-missing.txt where heap.txt
# stayed, written from what the removal then did -- the runner keeps that heap.txt with the
# step's other files removed, or every file of the step when that removal failed too -- while an
# existence check that failed removes nothing there; (y) with the lock naming another run,
# nothing of this run writes into, kills in or removes from the copy those fixed names reach:
# runner_stop and runner_step_remove (runner_exec) and typing_stop stop and remove nothing,
# runner_start, runner_put, typing_start's token write and psql_write (postgres_exec) and
# stop_copy (scale_to) refuse outright, each saying which pod or deployment it left alone, a
# command whose --run names this run is refused, naming both runs, and collect, which reads this
# pod's run directory and no pod of the copy, packs and uploads that run with such a --run,
# reading not even the lock; (z) the daemons write their pidfile without reading the lock, so
# both start although every read of the lock fails, while that same failure stops up itself at
# its first write into the copy; (aa) a guarded write, kill or removal whose every read of the
# lock fails stops after 3 tries 2 s apart, touching nothing of the copy and saying so in the
# words of that last failed read, and one whose read answers after a failed try acts; the
# replay's three long writes (the seed dump streamed into postgres-0, its pg_restore, the bundle
# into the runner) go through those guarded helpers, each with a bound of its own; (bb) a poll
# whose every read of the lock failed stops nothing and records nothing, and the disk watcher
# and the runner-job wait keep polling and read the lock again at the next one, where an answer
# has them act; (cc) copy_exec, the one option parse of runner_exec and postgres_exec, takes -t
# and -i in either order and ends at the first argument that is neither.
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
  # A case that failed before its assertions could see the step end leaves the step shell (w)
  # killed, its disk watcher and its wait on the runner job behind; a cluster.sh that passes (w)
  # ends all three itself.
  pkill -f "$cluster step --namespace $ns " 2>/dev/null || true
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
# kubectl-calls.log: one line per call, tagged with the caller's session, so a check can pair two
# calls of one cluster.sh command whatever the run's daemons (each a session of its own, setsid)
# call in between, and whatever lines one call's arguments span. Not the process group: kube and
# kube_for run kubectl under timeout, which gives each call a process group of its own. Truncated
# to stay one short write, which the kernel appends whole.
call_sid=$(sed -E 's/^.*\) //' "/proc/$$/stat" | cut -d' ' -f4)
call_args=${*//$'\n'/\\n}
printf '%s\t%s\n' "$call_sid" "${call_args:0:1000}" >>"$FAKE_DIR/kubectl-calls.log"
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
      *"configmap dispatch-loadtest-lock"*)
        # FAKE_LOCK_GET_FAIL: a read of the lock fails with a refused API server connection once
        # the lock exists, so the command that takes it goes on and every read after that fails.
        # FAKE_LOCK_GET_FAIL_TIMES=N: only the first N of those reads fail (the count lives in
        # FAKE_DIR/lock-get-fails, since each call is a process of its own), and the reads after
        # them answer the lock, so a caller that reads again gets through.
        if [ -n "${FAKE_LOCK_GET_FAIL:-}" ] && [ -f "$FAKE_DIR/lock.json" ]; then
          fails=0
          [ ! -f "$FAKE_DIR/lock-get-fails" ] || fails=$(cat "$FAKE_DIR/lock-get-fails")
          if [ -z "${FAKE_LOCK_GET_FAIL_TIMES:-}" ] || ((fails < FAKE_LOCK_GET_FAIL_TIMES)); then
            printf '%s\n' "$((fails + 1))" >"$FAKE_DIR/lock-get-fails"
            echo 'The connection to the server 10.0.0.1:443 was refused - did you specify the right host or port?' >&2
            exit 1
          fi
        fi
        [ ! -f "$FAKE_DIR/lock.json" ] || cat "$FAKE_DIR/lock.json"
        ;;
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
    # FAKE_EXEC_ERRORS=FILE: an exec fails with FILE's first line, which it uses up.
    if [ -n "${FAKE_EXEC_ERRORS:-}" ] && [ -s "$FAKE_EXEC_ERRORS" ]; then
      head -n1 "$FAKE_EXEC_ERRORS" >&2
      sed -i 1d "$FAKE_EXEC_ERRORS"
      exit 1
    fi
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
      # The job a stop names (runner_stop, runner_wait's bound, typing_stop).
      "sh -c pid="*) printf '%s\n' "${cmd[4]}" >>"$FAKE_DIR/runner-stops" ;;
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
      # A step's directory and copy-meta file, removed from the runner once copied back, and the
      # same with heap.txt left behind when that file alone did not come back.
      # FAKE_HEAP_KEEP_REMOVE_FAIL: that second removal fails, so every file of the step stays.
      "rm -rf /work/steps/"*) ;;
      'sh -c rm -f "/work/meta/'*)
        if [ -n "${FAKE_HEAP_KEEP_REMOVE_FAIL:-}" ]; then
          echo 'error: unable to upgrade connection: container not found ("runner")' >&2
          exit 1
        fi
        printf '%s\n' "${cmd[4]}" >>"$FAKE_DIR/runner-heap-kept"
        ;;
      # Browser typing: the agent token written into the runner from standard input, the token and
      # document list removed when typing stops.
      'sh -c umask 077 && mkdir -p "$1" && cat >"$1/token" sh '*)
        mkdir -p "$FAKE_DIR/runner${cmd[4]}" && cat >"$FAKE_DIR/runner${cmd[4]}/token"
        ;;
      "rm -f /work/typing/"*) ;;
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
      *"loadtest.replay_ids"*) cat "$FAKE_DIR/replay-ids" ;;
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
      # The seed's counts: every key of common.sh's SEED_COUNTS_SQL (FAKE_DIR/seed-counts.sql) is
      # answered only for exactly that query, so a cluster.sh that counts less than stack.sh does
      # writes a seed.meta.json that lacks those counts; any other json_build_object read gets the
      # short object.
      *json_build_object*)
        if [ "${cmd[-1]}" = "$(<"$FAKE_DIR/seed-counts.sql")" ]; then
          echo '{"projects": 4, "issues": 1000, "comments": 2000, "artifacts": 300, "artifact_versions": 900, "ref_targets": 20, "ref_target_versions": 60, "ref_sources": 5, "artifact_refs": 40, "events": 8000}'
        else
          echo '{"projects": 4, "issues": 1000}'
        fi
        ;;
      # The stored-order evidence export: each table's CSV (a quoted field holding a line break and
      # quotes, so a record is not a line), doc_checkpoints with no row and failing for
      # 22222222-..., the issue's events, version 7's markdown as base64, and from a template
      # database (dispatch_seed or dispatch_replay) one doc_updates row and no row of the others.
      *"-d dispatch_"*"\\copy (select * from doc_updates "*) printf 'artifact_id,version,update\nfake,1,\\x01\n' ;;
      *"-d dispatch_"*"\\copy (select * from "*) printf 'artifact_id\n' ;;
      *"\\copy (select * from doc_checkpoints where artifact_id = '22222222-"*)
        echo 'ERROR:  canceling statement due to statement timeout' >&2
        exit 1
        ;;
      *"\\copy (select * from doc_checkpoints "*) printf 'artifact_id,ceiling,state\n' ;;
      *"\\copy (select * from artifacts "*) printf 'id,name\nfake,"a ""quoted""\nname"\n' ;;
      *"\\copy (select * from artifact_versions "*) printf 'number,markdown\n1,"# One\n\nbody"\n7,"# Seven ""x"""\n' ;;
      *"\\copy (select * from doc_updates "*) printf 'version,update\n1,\\x01\n2,\\x02\n3,\\x03\n' ;;
      *"\\copy (select * from doc_snapshots "*) printf 'artifact_id,name,version,state\nfake,main,3,\\x03\n' ;;
      *"\\copy (select e.* from events e "*) printf 'id,issue_key,seq,type,payload\n1,LGA-1,1,doc.updated,"{""a"":\n1}"\n2,LGA-1,2,doc.updated,{}\n' ;;
      *"encode(convert_to(markdown, 'UTF8'), 'base64')"*"number = 7 "*) printf '# Seven "x"\n\nbody\n' | base64 ;;
      psql*) ;;
      *cpuinfo*) printf 'model name\t: Fake CPU @ 2.00GHz\n' ;;
      # Dispatch's container keeps its 16-core limit; Postgres's reads FAKE_DIR/cpu.max.
      "cat /sys/fs/cgroup/cpu.max") if [ "$ctr" = envoy-dispatch ]; then echo '1600000 100000'; else cat "$FAKE_DIR/cpu.max"; fi ;;
      # The Postgres data volume (pg_disk). FAKE_PG_DF_SEQ names a file of `size used avail`
      # lines, one per read, the last repeated; with none, a 100Gi volume a tenth used.
      "df -B1 --output=size,used,avail /var/lib/postgresql/copy")
        df_line='107374182400 10737418240 96636764160'
        if [ -n "${FAKE_PG_DF_SEQ:-}" ] && [ -s "$FAKE_PG_DF_SEQ" ]; then
          df_line=$(head -n1 "$FAKE_PG_DF_SEQ")
          [ "$(wc -l <"$FAKE_PG_DF_SEQ")" -le 1 ] || sed -i 1d "$FAKE_PG_DF_SEQ"
        fi
        printf '%s %s %s\n%s\n' 1B-blocks Used Avail "$df_line"
        ;;
      # browsertype's exit status (FAKE_TYPING_RC, default 0) and its peer lines: with exit 1, a
      # third peer that failed to connect.
      "cat /work/logs/typing-"*.rc) echo "${FAKE_TYPING_RC:-0}" ;;
      "cat /work/logs/typing-"*.log)
        printf 'artifact=a peer=0 sent=40 skipped=1 errors=0 error=""\nartifact=a peer=1 sent=38 skipped=0 errors=0 error=""\n'
        [ "${FAKE_TYPING_RC:-0}" = 0 ] || printf 'artifact=b peer=0 sent=0 skipped=0 errors=1 error="websocket: bad handshake"\n'
        ;;
      "cat /work/logs/"*.rc)
        # FAKE_RC_UNTIL_STOP: the job has no exit status until something stops it (the disk
        # watcher), as a job that still runs has none. At most 200 reads, so a watcher that never
        # fires fails the test instead of hanging it.
        if [ -n "${FAKE_RC_UNTIL_STOP:-}" ] && [ ! -s "$FAKE_DIR/runner-stops" ]; then
          polls=0
          [ ! -f "$FAKE_DIR/rc-polls" ] || polls=$(cat "$FAKE_DIR/rc-polls")
          if [ "$polls" -lt 200 ]; then
            echo $((polls + 1)) >"$FAKE_DIR/rc-polls"
            echo 'cat: /work/logs: No such file or directory' >&2
            exit 1
          fi
        fi
        echo 0
        ;;
      "cat /work/logs/"*) echo 'fake runner log' ;;
      "cat /work/seed.json") echo '{"projects": ["LGA"]}' ;;
      "tar -C /work/steps/"*)
        # Each step's meta.json starts now, so the steps' start order is the order they ran.
        jq --arg start "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" '.start = $start' "$FAKE_DIR/stepdir/meta.json" >"$FAKE_DIR/meta.tmp"
        mv "$FAKE_DIR/meta.tmp" "$FAKE_DIR/stepdir/meta.json"
        # FAKE_COPY_BACK_FAIL: the stream is cut mid-file and the exec fails, as the API server's
        # cut of a 1 GB heap.txt once did. meta.json is whole and summary.json is padded with
        # incompressible bytes, so the cut falls inside it.
        if [ -n "${FAKE_COPY_BACK_FAIL:-}" ]; then
          cutdir=$FAKE_DIR/cut-stepdir
          rm -rf "$cutdir" && mkdir -p "$cutdir"
          cp "$FAKE_DIR/stepdir/meta.json" "$cutdir/meta.json"
          { printf '{"config": {"step": "fake"}, "pad": "'; head -c 100000 /dev/urandom | base64 -w0; printf '"}\n'; } >"$cutdir/summary.json"
          tar -C "$cutdir" -czf - ./meta.json ./summary.json | head -c 20000
          echo 'error: unable to upgrade connection: error dialing backend: EOF' >&2
          exit 1
        fi
        # The copy-back streams everything but heap.txt, compressed; heap.txt comes separately.
        tar -C "$FAKE_DIR/stepdir" --exclude=./heap.txt -czf - .
        ;;
      # FAKE_HEAP_EXISTS_FAIL: the read of whether the runner holds heap.txt fails, on an error
      # kube_read does not retry.
      "sh -c if test -f "*)
        if [ -n "${FAKE_HEAP_EXISTS_FAIL:-}" ]; then
          echo 'error: unable to upgrade connection: container not found ("runner")' >&2
          exit 1
        fi
        if [ -f "$FAKE_DIR/stepdir/heap.txt" ]; then echo yes; else echo no; fi
        ;;
      # FAKE_HEAP_COPY_FAIL: the heap.txt stream alone fails (every try), while every other file
      # of the step came back in the stream above.
      "gzip -1 -c /work/steps/"*)
        if [ -n "${FAKE_HEAP_COPY_FAIL:-}" ]; then
          echo 'error: unable to upgrade connection: error dialing backend: EOF' >&2
          exit 1
        fi
        gzip -1 -c "$FAKE_DIR/stepdir/heap.txt"
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
# The fake go: `go build -o PATH ./loadtest` writes a fake loadgen to PATH, `go build ... -o PATH
# ./cmd/dispatch`, in a directory that holds cmd/dispatch/storedorder.go, a fake envoy-dispatch,
# with GOWORK and CGO_ENABLED recorded in FAKE_DIR/go-dispatch.log, the tree's storedorder.go files
# in FAKE_DIR/go-dispatch-tree.log and its cmd/dispatch/subcommands.go copied to
# FAKE_DIR/go-subcommands.go (or, with FAKE_DISPATCH_BUILD_ERROR set, go's errors and exit 1), and
# `go build ... -o PATH ./loadtest/browsertype`, in a directory that holds loadtest/browsertype, a
# fake browsertype, recorded so in FAKE_DIR/go-browsertype.log.
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
if [ "${*: -1}" = ./loadtest/browsertype ]; then
  [ -d loadtest/browsertype ] || {
    echo "fake go: $PWD holds no loadtest/browsertype" >&2
    exit 1
  }
  printf 'GOWORK=%s CGO_ENABLED=%s go %s\n' "${GOWORK:-}" "${CGO_ENABLED:-}" "$*" >>"$FAKE_DIR/go-browsertype.log"
  # Its -help lists -protect, as INFRA-707's fixed driver does; FAKE_BT_NO_PROTECT=1 builds one
  # without it, as the driver before that fix.
  if [ -n "${FAKE_BT_NO_PROTECT:-}" ]; then
    printf '#!/usr/bin/env bash\n[ "$1" = -help ] && { echo "  -peers int"; exit 0; }\necho "fake browsertype: $*"\n' >"$out"
  else
    printf '#!/usr/bin/env bash\n[ "$1" = -help ] && { echo "  -protect value"; exit 0; }\necho "fake browsertype: $*"\n' >"$out"
  fi
  chmod +x "$out"
  exit 0
fi
if [ "${*: -1}" = ./cmd/dispatch ]; then
  [ -f cmd/dispatch/storedorder.go ] || {
    echo "fake go: $PWD holds no cmd/dispatch/storedorder.go" >&2
    exit 1
  }
  if [ -n "${FAKE_DISPATCH_BUILD_ERROR:-}" ]; then
    printf '%s\n' '# github.com/sjawhar/envoy/cmd/dispatch' 'cmd/dispatch/storedorder.go:42:9: undefined: docs.CheckStoredOrder' \
      'cmd/dispatch/storedorder.go:57:2: undefined: docs.StoredOrderReport' >&2
    exit 1
  fi
  printf 'GOWORK=%s CGO_ENABLED=%s go %s\n' "${GOWORK:-}" "${CGO_ENABLED:-}" "$*" >>"$FAKE_DIR/go-dispatch.log"
  find . -name storedorder.go | sort >"$FAKE_DIR/go-dispatch-tree.log"
  rm -f -- "$FAKE_DIR/go-subcommands.go"
  [ ! -f cmd/dispatch/subcommands.go ] || cp cmd/dispatch/subcommands.go "$FAKE_DIR/go-subcommands.go"
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
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 base=4 updates_checked=9 updates_skipped=3 versions_checked=2 versions_skipped=1 mismatches=2'
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 class update=9 stored=false expected=true'
    echo 'stored-order: 11111111-1111-4111-8111-111111111111 version number=7 doc_update_version=9 stored_md5=0cc175b9c0f1b6a831c399e269772661 stored_length=12 fold_md5=92eb5ffee6ae2fec3ad71c777531578f fold_length=13'
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
  # The fake plan-docs: the seed's spec documents, each with the planned versions
  # FAKE_PLAN_VERSIONS names (whitespace-separated, in the seed's order; 100 for the rest), most
  # versions first, the first -top of them; with -json the objects, else one artifact id per line.
  plan-docs)
    seed="" top=20 json=false prev=""
    for a in "$@"; do
      case $prev in
        -seed) seed=$a ;;
        -top) top=$a ;;
      esac
      [ "$a" != -json ] || json=true
      prev=$a
    done
    jq -r --argjson top "$top" --argjson json "$json" --arg counts "${FAKE_PLAN_VERSIONS:-}" '
      ($counts | split(" ") | map(select(. != "") | tonumber)) as $c
      | [.issues[] | .artifact_id] | to_entries
      | map(($c[.key] // 100) as $v | {artifact: .value, edits: ($v - ($v / 3 | floor)), uploads: ($v / 3 | floor), versions: $v})
      | sort_by(-.versions, .artifact) | .[:$top]
      | if $json then tojson else map(.artifact) | .[] end' "$seed"
    ;;
  series-sessions) echo 400 ;;
  replay-unpack)
    # The bundle: its meta, and FAKE_DIR/bundle-fixture's files (state.json, window.jsonl) when the
    # test wrote them.
    mkdir -p "$out"
    echo '{"project": "FAKE"}' >"$out/bundle.meta.json"
    [ ! -d "$FAKE_DIR/bundle-fixture" ] || cp "$FAKE_DIR/bundle-fixture"/* "$out/"
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
# The fake git: the git store holds a commit SHA when FAKE_DIR/git-trees/SHA lists its tree's files,
# and its tree holds each listed file and each directory above one, each file empty unless
# FAKE_DIR/git-files/SHA/<path> holds its content; `cat-file -p SHA:PATH` prints a file, and
# `archive SHA PATH...` writes a tar of those files under the PATHs.
printf '%s\n' "$*" >>"$FAKE_DIR/git.log"
[ "${1:-}" != -C ] || shift 2
case ${1:-} in
  cat-file)
    obj=$3
    case $2:$obj in
      -e:*'^{commit}') [ -f "$FAKE_DIR/git-trees/${obj%'^{commit}'}" ] ;;
      -e:*:*)
        tree=$FAKE_DIR/git-trees/${obj%%:*} path=${obj#*:}
        [ -f "$tree" ] && { grep -qxF -- "$path" "$tree" || awk -v p="$path/" 'index($0, p) == 1 { f = 1 } END { exit !f }' "$tree"; }
        ;;
      -p:*:*)
        tree=$FAKE_DIR/git-trees/${obj%%:*} path=${obj#*:}
        [ -f "$tree" ] && grep -qxF -- "$path" "$tree" || {
          echo "fatal: path '$path' does not exist in '${obj%%:*}'" >&2
          exit 128
        }
        content=$FAKE_DIR/git-files/${obj%%:*}/$path
        [ ! -f "$content" ] || cat -- "$content"
        ;;
      *) exit 1 ;;
    esac
    ;;
  archive)
    [ -f "$FAKE_DIR/git-trees/$2" ] || {
      echo "fatal: not a valid object name $2" >&2
      exit 128
    }
    d=$(mktemp -d)
    while read -r p; do
      mkdir -p "$d/${p%/*}" && : >"$d/$p"
      [ ! -f "$FAKE_DIR/git-files/$2/$p" ] || cp -- "$FAKE_DIR/git-files/$2/$p" "$d/$p"
    done <"$FAKE_DIR/git-trees/$2"
    tar -C "$d" -cf - "${@:3}"
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

# The fake sleep: apply_manifest's 3 s wait between tries, and a wait of any of FAKE_FAST_SLEEP's
# seconds (space-separated: kube_read's 5 s, the daemons' 60 s, runner_wait's 15 s and the disk
# watcher's 30 s, where a test sets them), are recorded and return at once; every other sleep (the
# daemons' loops, their start waits) is the real one.
cat >"$bin/sleep" <<EOF
#!/usr/bin/env bash
if [ "\$*" = 3 ] || [[ " \${FAKE_FAST_SLEEP:-none} " == *" \$* "* ]]; then
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
unset FAKE_EXEC_FAIL FAKE_SAMPLE FAKE_PROD_MAJOR FAKE_LOADGEN_FAIL FAKE_APPLY_FAIL FAKE_QUIET FAKE_STORED_ORDER \
  FAKE_EXEC_ERRORS FAKE_FAST_SLEEP FAKE_TYPING_RC FAKE_DISPATCH_BUILD_ERROR FAKE_COPY_BACK_FAIL \
  FAKE_PG_DF_SEQ FAKE_RC_UNTIL_STOP FAKE_HEAP_COPY_FAIL FAKE_HEAP_EXISTS_FAIL FAKE_HEAP_KEEP_REMOVE_FAIL \
  FAKE_LOCK_GET_FAIL FAKE_LOCK_GET_FAIL_TIMES
export DISPATCH_URL=http://dispatch.invalid DISPATCH_TOKEN_FILE=$fake/token
# The manifest names no issue: the operator does, and replay-prepare refuses without it.
export LOADTEST_REPLAY_ISSUE=INFRA-639
echo fake-token >"$fake/token"
# The seed's counts query, as common.sh holds it: the fake kubectl answers every count only for
# exactly this query, so a cluster.sh seed that asks for less is seen.
(
  # shellcheck source=packages/envoy/loadtest/common.sh
  source "$here/../common.sh"
  printf '%s' "$SEED_COUNTS_SQL"
) >"$fake/seed-counts.sql"

readonly commit_ok=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly commit_none=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly commit_parent=cccccccccccccccccccccccccccccccccccccccc
readonly commit_ancestor=dddddddddddddddddddddddddddddddddddddddd
printf '%s %s\n%s %s\n' "$commit_none" "$commit_parent" "$commit_parent" "$commit_ancestor" >"$fake/parents"
printf '%s\n%s\n' "$commit_none" "$commit_parent" >"$fake/missing-refs"
# The stored-order commits: commit_so, an INFRA-707 head (the command's files, and its
# stored-order entry in cmd/dispatch/subcommands.go's table); commit_old, a dispatch tree without
# the command whose table has the opener the overlay anchors on (as 9b92ac14); commit_noanchor,
# one without the command whose table has no such opener; commit_noentry, one with the command's
# files and no stored-order entry; and one the git store does not hold. commit_ok and commit_none
# are dispatch trees with the command (commit_none has no image).
readonly commit_so=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
readonly commit_old=ffffffffffffffffffffffffffffffffffffffff
readonly commit_noanchor=1111111111111111111111111111111111111111
readonly commit_noentry=2222222222222222222222222222222222222222
readonly commit_unknown=0123456789abcdef0123456789abcdef01234567
mkdir -p "$fake/git-trees"
so_files=(packages/envoy/internal/dispatch/docs/storedorder.go packages/envoy/cmd/dispatch/storedorder.go)
so_registry=packages/envoy/cmd/dispatch/subcommands.go
# The table's opener, an entry before the overlay's place, and INFRA-707's stored-order entry.
so_opener='var subcommands = []subcommand{'
so_census=$'\t{"census", func(ctx context.Context, _ []string, env settingValues, stdout, stderr io.Writer) int {\n\t\treturn census(ctx, env.get("DATABASE_URL"), stdout, stderr)\n\t}},'
so_entry=$'\t{"stored-order", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {\n\t\treturn storedOrder(ctx, args, env.get("DATABASE_URL"), stdout)\n\t}},'
# fake_tree SHA SUBCOMMANDS PATH...: the fake git store's commit SHA, whose tree holds go.mod,
# cmd/dispatch/main.go, the subcommand table (content SUBCOMMANDS) and each PATH.
fake_tree() {
  local sha=$1 table=$2
  shift 2
  printf '%s\n' packages/envoy/go.mod packages/envoy/cmd/dispatch/main.go "$so_registry" "$@" >"$fake/git-trees/$sha"
  mkdir -p "$fake/git-files/$sha/${so_registry%/*}"
  printf 'package main\n\n%s\n' "$table" >"$fake/git-files/$sha/$so_registry"
}
fake_tree "$commit_so" "$(printf '%s\n' "$so_opener" "$so_census" "$so_entry" '}')" "${so_files[@]}"
fake_tree "$commit_ok" "$(printf '%s\n' "$so_opener" "$so_census" "$so_entry" '}')" "${so_files[@]}"
fake_tree "$commit_none" "$(printf '%s\n' "$so_opener" "$so_census" "$so_entry" '}')" "${so_files[@]}"
fake_tree "$commit_old" "$(printf '%s\n' "$so_opener" "$so_census" '}')"
fake_tree "$commit_noanchor" "$(printf '%s\n' 'var subcommands = map[string]subcommand{' "$so_census" '}')"
fake_tree "$commit_noentry" "$(printf '%s\n' "$so_opener" "$so_census" '}')" "${so_files[@]}"
# The browsertype commit, whose tree has loadtest/browsertype (commit_so's has none).
readonly commit_bt=abababababababababababababababababababab
printf '%s\n' packages/envoy/go.mod packages/envoy/cmd/dispatch/main.go packages/envoy/loadtest/browsertype/main.go >"$fake/git-trees/$commit_bt"
jq -n '{window_start: "2026-10-28T10:00:00Z", window_end: "2026-10-28T10:01:00Z"}' >"$fake/stepdir/meta.json"
echo '{"config": {"step": "fake"}}' >"$fake/stepdir/summary.json"
printf 'heap profile: fake\n# runtime.MemStats\n# HeapInuse = 1\n' >"$fake/stepdir/heap.txt"

# The replay's fixture: two bundle parts and one seed dump part, served by the fake curl by slug.
mkdir -p "$fake/parts"
for slug in fake-replay-tar-zst-part00 fake-replay-tar-zst-part01 fake-seed-dump-part00; do
  head -c 3000 /dev/urandom >"$fake/parts/$slug"
done

# part_entry FILE SLUG: a manifest part entry for FAKE_DIR/parts/SLUG.
part_entry() {
  jq -n --arg f "$1" --arg s "$2" --arg sha "$(sha256sum <"$fake/parts/$2" | cut -d' ' -f1)" --argjson b "$(stat -c %s "$fake/parts/$2")" \
    '{file: $f, slug: $s, version: 1, bytes: $b, sha256: $sha}'
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
    '{schema: 1, project: "FAKE", window: {from: "2026-10-26T22:55:00Z", to: "2026-10-26T23:34:00Z"},
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
  rm -f -- "$fake/lock.json" "$fake/applied.yaml" "$fake/kubectl.log" "$fake/kubectl-calls.log" "$fake/seeded" "$fake/runner-starts" "$fake/dispatch.log" "$fake/loadgen.log" "$fake"/refine.[0-9]* \
    "$fake/runner-stops" "$fake/rc-polls" "$fake/df-seq" "$fake/runner-heap-kept" "$fake/lock-get-fails" \
    "$fake/sleep.log" "$fake/apply-errors" "$fake/replay-template" "$fake/restores" "$fake/pod-dump" \
    "$fake/stored-order.log" "$fake/go-dispatch.log" "$fake/go-browsertype.log" "$fake/git.log" \
    "$fake/go-dispatch-tree.log" "$fake/go-subcommands.go"
  rm -rf -- "$fake/runner" "$fake/bundle-fixture"
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

# guarded_by_lock_read PATTERN: the last kubectl call that holds PATTERN came right after a read
# of the lock by the same command, which is what a write through a guarded helper (runner_exec,
# postgres_exec and the rest) looks like: copy_is_ours reads the lock, and the write is that
# command's next kubectl call. The calls are paired in kubectl-calls.log (one line per call,
# tagged with the caller's session), so the run's daemons, each a session of its own, can call
# kubectl in between without changing the answer.
guarded_by_lock_read() {
  local entry sid
  entry=$(grep -F -- "$1" "$fake/kubectl-calls.log" | tail -n1)
  [ -n "$entry" ] || return 1
  sid=${entry%%$'\t'*}
  awk -F '\t' -v g="$sid" -v pat="$1" '
    $1 != g { next }
    index($0, pat) { before = prev }
    { prev = $0 }
    END { exit (index(before, "get configmap dispatch-loadtest-lock") ? 0 : 1) }' "$fake/kubectl-calls.log"
}

# own_calls_mark: a mark for own_calls_since, the number of calls in kubectl-calls.log now.
own_calls_mark() { wc -l <"$fake/kubectl-calls.log" 2>/dev/null || echo 0; }

# own_calls_since MARK: the kubectl calls made since MARK (own_calls_mark) by this test's own
# session, one per line: the commands it runs (run_cluster, or a sourced helper) share its session,
# and the run's daemons, each a session of its own (setsid), are left out, so a heartbeat patch or
# a sampler exec that lands meanwhile is never read as the command's own call.
own_calls_since() {
  local sid
  sid=$(sed -E 's/^.*\) //' "/proc/$$/stat" | cut -d' ' -f4)
  awk -F '\t' -v from="$1" -v g="$sid" 'NR > from && $1 == g { sub(/^[^\t]*\t/, ""); print }' \
    "$fake/kubectl-calls.log" 2>/dev/null || true
}

# all_gone PID...: none of the pids is a live process any more.
all_gone() {
  local pid
  for pid in "$@"; do
    if kill -0 "$pid" 2>/dev/null; then return 1; fi
  done
}

# await_gone PID...: wait until none of the pids is a live process. What ends them is their own
# next poll, whose naps the fake sleep returns from at once, so this waits on the processes, not
# on a stretch of wall clock; the 300 s is a hang's backstop, never a timing assertion.
await_gone() {
  local deadline=$((SECONDS + 300))
  while ! all_gone "$@"; do
    ((SECONDS < deadline)) || return 1
    sleep 0.05
  done
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

# checker_refused WHAT: up's refusal of WHAT left nothing: no kubectl create or apply, no lock, no
# envoy-dispatch built, and no checker scratch directory.
checker_refused() {
  no_create || fail "up created something before refusing $1"
  [ ! -f "$fake/lock.json" ] || fail "up took the lock before refusing $1"
  [ ! -s "$fake/go-dispatch.log" ] || fail "up built envoy-dispatch before refusing $1"
  [ -z "$(find "$LOADTEST_CLUSTER_ROOT" -maxdepth 1 -name '.checker.*' 2>/dev/null)" ] || fail "up left the checker's scratch directory after refusing $1"
}

reset_fake
for c in "$commit_unknown||--dispatch-commit $commit_unknown is not a commit in this checkout's git store" \
  "$commit_old||--dispatch-commit $commit_old's tree has no stored-order command (packages/envoy/cmd/dispatch/storedorder.go); give --stored-order-source <commit>" \
  "$commit_ok|--stored-order-source $commit_so|--stored-order-source $commit_so is refused: --dispatch-commit $commit_ok's tree has the stored-order command" \
  "$commit_old|--stored-order-source 9b92ac1|--stored-order-source '9b92ac1' is not a full 40-hex commit sha" \
  "$commit_old|--stored-order-source $commit_unknown|--stored-order-source $commit_unknown is not a commit in this checkout's git store" \
  "$commit_old|--stored-order-source $commit_old|--stored-order-source $commit_old has no packages/envoy/internal/dispatch/docs/storedorder.go, so it lacks INFRA-707's stored-order command" \
  "$commit_old|--stored-order-source $commit_noentry|--stored-order-source $commit_noentry's packages/envoy/cmd/dispatch/subcommands.go holds no single stored-order entry" \
  "$commit_old|--stored-order-source $commit_so --stored-order-checker-commit $commit_so|--stored-order-source and --stored-order-checker-commit exclude each other" \
  "$commit_old|--stored-order-checker-commit 9b92ac1|--stored-order-checker-commit '9b92ac1' is not a full 40-hex commit sha" \
  "$commit_old|--stored-order-checker-commit $commit_old|--stored-order-checker-commit $commit_old is --dispatch-commit itself" \
  "$commit_ok|--stored-order-checker-commit $commit_noanchor|--stored-order-checker-commit $commit_noanchor has no packages/envoy/cmd/dispatch/storedorder.go"; do
  IFS='|' read -r d flags want <<<"$c"
  read -ra flags <<<"$flags"
  run_cluster up "${up_args[@]}" --dispatch-commit "$d" "${flags[@]}"
  ((RC != 0)) || fail "up accepted --dispatch-commit $d ${flags[*]}"
  grep -qF -- "$want" "$tmp/out" || fail "up's refusal of --dispatch-commit $d ${flags[*]} does not say '$want': $(cat "$tmp/out")"
  checker_refused "--dispatch-commit $d ${flags[*]}"
done
pass "up refuses, naming them, before creating anything, stored-order flags it cannot build a checker from: a dispatch commit git lacks, a dispatch tree without the command and no --stored-order-source, a source beside a tree with the command, a source that is not 40 hex, git lacks or lacks INFRA-707's files or entry, and a bad or conflicting --stored-order-checker-commit"

reset_fake
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_noanchor" --stored-order-source "$commit_so"
((RC != 0)) || fail "up accepted an overlay whose dispatch subcommand table has no registration anchor"
grep -qF -- "the overlay: dispatch commit $commit_noanchor's packages/envoy/cmd/dispatch/subcommands.go holds the registration's anchor \`var subcommands = []subcommand{\` on 0 lines, not 1" "$tmp/out" ||
  fail "up's refusal of an overlay without the registration anchor does not name the anchor: $(cat "$tmp/out")"
grep -qF -- "up: the stored-order checker does not build (dispatch commit $commit_noanchor, source commit $commit_so): the overlay:" "$tmp/out" ||
  fail "up's refusal of an overlay without the registration anchor does not name both commits: $(cat "$tmp/out")"
checker_refused "an overlay without the registration anchor"
pass "up refuses an overlay whose dispatch subcommand table lacks the registration anchor, naming the anchor and both commits, before creating anything"

reset_fake
FAKE_DISPATCH_BUILD_ERROR=1 run_cluster up "${up_args[@]}" --dispatch-commit "$commit_old" --stored-order-source "$commit_so"
((RC != 0)) || fail "up accepted a checker that does not compile"
grep -qxF -- "cluster.sh: up: the stored-order checker does not build (dispatch commit $commit_old, source commit $commit_so): cmd/dispatch/storedorder.go:42:9: undefined: docs.CheckStoredOrder" "$tmp/out" ||
  fail "up's refusal of a checker that does not compile does not name both commits and the compiler's first error: $(cat "$tmp/out")"
grep -qF 'cmd/dispatch/storedorder.go:57:2: undefined: docs.StoredOrderReport' "$tmp/out" || fail "up did not show the compiler's whole output"
no_create || fail "up created something before refusing a checker that does not compile"
[ ! -f "$fake/lock.json" ] || fail "up took the lock before refusing a checker that does not compile"
[ -z "$(find "$LOADTEST_CLUSTER_ROOT" -maxdepth 1 -name '.checker.*' 2>/dev/null)" ] || fail "up left the checker's scratch directory after a failed build"
pass "up refuses a checker that does not compile, naming the dispatch commit, the source commit and the compiler's first error, before creating anything"

reset_fake
for c in "abab|--browsertype-commit 'abab' is not a full 40-hex commit sha" \
  "$commit_unknown|--browsertype-commit $commit_unknown is not a commit in this checkout's git store" \
  "$commit_so|--browsertype-commit $commit_so has no packages/envoy/loadtest/browsertype, so it has no browsertype"; do
  run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --browsertype-commit "${c%%|*}"
  ((RC != 0)) || fail "up accepted --browsertype-commit ${c%%|*}"
  grep -qF -- "${c#*|}" "$tmp/out" || fail "up's refusal of --browsertype-commit ${c%%|*} does not say '${c#*|}'"
  no_create || fail "up created something before refusing --browsertype-commit ${c%%|*}"
  [ ! -f "$fake/lock.json" ] || fail "up took the lock before refusing --browsertype-commit ${c%%|*}"
  { [ ! -s "$fake/go-browsertype.log" ] && [ ! -s "$fake/go-dispatch.log" ]; } ||
    fail "up built a binary before refusing --browsertype-commit ${c%%|*}"
done
pass "up refuses a --browsertype-commit that is not 40 hex, that the git store lacks, or whose tree has no loadtest/browsertype, naming it, before creating anything"

# A browsertype whose -help lists no -protect would type into loadgen's revision token: refused
# after its build, before the lock.
reset_fake
FAKE_BT_NO_PROTECT=1 run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --browsertype-commit "$commit_bt"
((RC != 0)) || fail "up accepted a browsertype with no -protect"
grep -qF -- "has no -protect flag" "$tmp/out" || fail "up's refusal of a browsertype without -protect does not say so: $(tail -3 "$tmp/out")"
no_create || fail "up created something before refusing a browsertype without -protect"
[ ! -f "$fake/lock.json" ] || fail "up took the lock before refusing a browsertype without -protect"
pass "up refuses a browsertype with no -protect before the lock, naming the flag"

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

# The manifest names no issue, so the operator's LOADTEST_REPLAY_ISSUE must: without it
# replay-prepare refuses before any kubectl call, as stack.sh replay-get does.
reset_fake
(
  unset LOADTEST_REPLAY_ISSUE
  run_cluster replay-prepare --namespace "$ns" --manifest "$seeded"
  ((RC != 0)) || fail "replay-prepare accepted an unset LOADTEST_REPLAY_ISSUE"
  grep -qF 'LOADTEST_REPLAY_ISSUE is not set' "$tmp/out" ||
    fail "replay-prepare's refusal does not name LOADTEST_REPLAY_ISSUE: $(cat "$tmp/out")"
  [ ! -s "$fake/kubectl.log" ] || fail "replay-prepare called kubectl before refusing an unset LOADTEST_REPLAY_ISSUE: $(cat "$fake/kubectl.log")"
) || exit 1
pass "replay-prepare refuses an unset LOADTEST_REPLAY_ISSUE, which the manifest does not name, before any kubectl call"

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
[ -z "$(find "$LOADTEST_CLUSTER_ROOT" -maxdepth 1 -name '.checker.*' 2>/dev/null)" ] || fail "up left the checker's scratch directory after the lock refused it"
pass "up refuses with exit 2 while the lock exists, naming the holder and the heartbeat's age, and removes the checker it built"

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
# The checker: built from the dispatch commit's own tree (no overlay), in a scratch directory of
# the cluster root before the lock, then moved into the run directory.
checker=$(jq -c .stored_order "$run_dir/state.json")
jq -e --arg c "$commit_ok" --arg s "$(sha256sum <"$run_dir/envoy-dispatch" | cut -d' ' -f1)" \
  '. == {dispatch_commit: $c, built_from: $c, overlay_source: null, overlay_files: [], registration: null, renderer_matches: true, sha256: $s}' \
  <<<"$checker" >/dev/null || fail "state.json does not record the checker built from the dispatch commit and its sha256: $checker"
grep -qx -- "-C $(cd -- "$here/../../../.." && pwd) archive $commit_ok packages/envoy" "$fake/git.log" ||
  fail "up did not git archive packages/envoy of the dispatch commit $commit_ok from the checkout's git store: $(cat "$fake/git.log")"
[[ $(cat "$fake/go-dispatch.log") == "GOWORK=off CGO_ENABLED=0 go build -buildvcs=false -o $LOADTEST_CLUSTER_ROOT/.checker."??????"/envoy-dispatch ./cmd/dispatch" ]] ||
  fail "up did not build one static envoy-dispatch in a scratch directory outside the workspace: $(cat "$fake/go-dispatch.log" 2>/dev/null)"
[ "$(cat "$fake/go-dispatch-tree.log")" = ./cmd/dispatch/storedorder.go$'\n'./internal/dispatch/docs/storedorder.go ] ||
  fail "the checker's tree is not the dispatch commit's: $(cat "$fake/go-dispatch-tree.log")"
cmp -s "$fake/go-subcommands.go" "$fake/git-files/$commit_ok/$so_registry" || fail "up changed the subcommand table of a dispatch tree that has the command"
[ -z "$(find "$LOADTEST_CLUSTER_ROOT" -maxdepth 2 \( -name '.checker.*' -o -name '.build-src.*' \))" ] || fail "up left the build's scratch directory"
grep -qF "building the stored-order checker, envoy-dispatch (static) of dispatch commit $commit_ok" "$tmp/out" || fail "up did not name the commit it builds the checker from"
jq -e '.browsertype == null' "$run_dir/state.json" >/dev/null || fail "state.json without --browsertype-commit does not record browsertype null"
[ "$(grep -c -- "^-n $ns cp $run_dir/envoy-dispatch runner:/work/envoy-dispatch " "$fake/kubectl.log")" = 1 ] ||
  fail "up did not copy envoy-dispatch into the runner"
pg_password=$(jq -r .secrets.pg_password "$run_dir/state.json")

run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC"
[ -f "$run_dir/seed.json" ] || fail "seed kept no seed.json in the run directory"
# seed.meta.json's counts are stack.sh's (common.sh SEED_COUNTS_SQL), the reference documents'
# among them, so a seed with loadgen seed-refs is told from one without. The fake kubectl answers
# every count only for exactly that query, so counts that are missing here are counts the seed
# did not ask Postgres for.
for k in artifact_versions ref_targets ref_target_versions ref_sources artifact_refs; do
  jq -e --arg k "$k" '.counts | has($k)' "$run_dir/seed.meta.json" >/dev/null ||
    fail "seed.meta.json's counts lack $k, which common.sh's SEED_COUNTS_SQL counts: $(jq -c .counts "$run_dir/seed.meta.json" 2>/dev/null)"
done

run_cluster step --namespace "$ns" smoke 1 --duration 1m
((RC == 0)) || fail "step exited $RC"
for f in reset.json copy-meta.json meta.json summary.json postgres-settings.txt containers-before.tsv containers-after.tsv pg-stats-before.json pg-stats-after.json; do
  [ -f "$run_dir/steps/smoke/$f" ] || fail "the step directory has no $f"
done
[ ! -f "$run_dir/steps/smoke/step-failed.json" ] || fail "a step with no restart was marked failed"
# heap.txt comes back by its own compressed stream (one tar of a 1 GB heap.txt once ended early),
# byte for byte, and the step's other files by the first.
cmp -s "$fake/stepdir/heap.txt" "$run_dir/steps/smoke/heap.txt" || fail "heap.txt did not come back from the runner"
grep -q -- 'gzip -1 -c /work/steps/smoke/heap.txt' "$fake/kubectl.log" || fail "heap.txt did not come back by its own stream"
start=$(grep '^step-smoke ' "$fake/runner-starts") || fail "the step started no loadgen run in the runner"
for want in "run -allow-foreign" "-events 4 " "-sessions 400 " "-requests 60 " "-duration 1m " "-warmup 30s " "-grace 20s " \
  "-rng-seed 1 " "-copy-meta /work/meta/smoke.json" "-nats-server-name nats-0" "-pg postgres://dispatch:"; do
  [[ $start == *"$want"* ]] || fail "the step's loadgen run lacks '$want': $start"
done
jq -e '.namespace == "'"$ns"'" and .postgres.version != "" and .postgres.settings_sha256 != "" and .node_cpu_model.dispatch == "Fake CPU @ 2.00GHz" and (.pods | length) == 8' \
  "$run_dir/steps/smoke/copy-meta.json" >/dev/null || fail "copy-meta.json lacks a field: $(cat "$run_dir/steps/smoke/copy-meta.json")"
# The phase and the reads-only flag, which loadgen curves reads to place the step in the curves.
jq -e '.phase == "steps" and .reads_only == false' "$run_dir/steps/smoke/copy-meta.json" >/dev/null ||
  fail "copy-meta.json does not record the step's phase and reads_only: $(cat "$run_dir/steps/smoke/copy-meta.json")"
jq -e '.bgwriter.buffers_clean == 0' "$run_dir/steps/smoke/pg-stats-after.json" >/dev/null ||
  fail "pg-stats-after.json does not hold pg_stat_bgwriter: $(cat "$run_dir/steps/smoke/pg-stats-after.json")"
# The stored-order check after the step: --since the window start, DATABASE_URL set from standard
# input, the password on no command line and in no recorded file, and a clean run's counts.
grep -qx -- "args stored-order --since 2026-10-28T10:00:00Z" "$fake/stored-order.log" ||
  fail "envoy-dispatch was not run as stored-order --since the window start: $(cat "$fake/stored-order.log" 2>/dev/null)"
grep -qxF -- "DATABASE_URL postgres://dispatch:$pg_password@postgres.$ns.svc:5432/dispatch?sslmode=disable" "$fake/stored-order.log" ||
  fail "stored-order ran without the copy's DATABASE_URL"
so_call=$(grep -F '/work/envoy-dispatch stored-order' "$fake/kubectl.log") || fail "no kubectl call ran stored-order in the runner"
[[ $so_call == "-n $ns exec -i runner -c runner -- sh -c "*" sh 7200 2026-10-28T10:00:00Z" ]] ||
  fail "stored-order's kubectl call is not an exec -i in the runner bounded at 7200 s since the window start, over every document: $so_call"
[ ! -e "$run_dir/steps/smoke/stored-order-typed.json" ] || fail "a step that did not type ran the typed stored-order call"
! grep -qF -- "$pg_password" <<<"$so_call" || fail "stored-order's kubectl call carries the Postgres password"
grep -qx 'stored-order: documents=2 clean=2 mismatched=0 refused=0 failed=0' "$run_dir/steps/smoke/stored-order.txt" ||
  fail "stored-order.txt does not hold the command's output: $(cat "$run_dir/steps/smoke/stored-order.txt" 2>/dev/null)"
jq -e --argjson k "$checker" '. == $k + {exit: 0, since: "2026-10-28T10:00:00Z", documents: 2, clean: 2, mismatched: 0, refused: 0, failed: 0, unchecked: null, evidence: []}' \
  "$run_dir/steps/smoke/stored-order.json" >/dev/null || fail "a clean run's stored-order.json is wrong: $(cat "$run_dir/steps/smoke/stored-order.json" 2>/dev/null)"
grep -qF "step smoke: stored-order (envoy-dispatch of dispatch commit aaaaaaaaaaaa, renderer_matches true) since 2026-10-28T10:00:00Z: exit 0, documents=2 clean=2 mismatched=0 refused=0 failed=0" "$tmp/out" ||
  fail "step smoke did not print the stored-order counts"
# A clean run exports no evidence.
! grep -qF -- '\copy' "$fake/kubectl.log" || fail "a clean stored-order run exported evidence: $(grep -F -- '\copy' "$fake/kubectl.log")"
[ ! -e "$run_dir/steps/smoke/stored-order-evidence" ] || fail "a clean stored-order run wrote stored-order-evidence/"

# --typing on a run that up gave no --browsertype-commit: refused, naming the flag, before the step
# or the replay touches anything (no scale, no psql; the daemons' calls go on beside it); and a
# --typing-* flag without --typing.
for sub in "step --namespace $ns typed 1 --duration 1m --typing" "replay --namespace $ns typed --typing"; do
  calls=$(own_calls_mark)
  read -ra words <<<"$sub"
  run_cluster "${words[@]}"
  ((RC != 0)) || fail "${words[0]} --typing ran although up was given no --browsertype-commit"
  grep -qF -- "${words[0]}: --typing needs INFRA-707's browsertype, and up was given no --browsertype-commit" "$tmp/out" ||
    fail "${words[0]} --typing without browsertype does not name --browsertype-commit"
  [ ! -e "$run_dir/steps/typed" ] || fail "${words[0]} --typing without browsertype made its step directory"
  new_calls=$(own_calls_since "$calls")
  ! grep -qE ' (scale|cp) | psql ' <<<"$new_calls" ||
    fail "${words[0]} --typing without browsertype touched the copy: $new_calls"
done
run_cluster step --namespace "$ns" typed 1 --typing-peers 3
{ ((RC != 0)) && grep -qF -- '--typing-peers needs --typing' "$tmp/out"; } || fail "step accepted --typing-peers without --typing"
pass "step and replay refuse --typing when up was given no --browsertype-commit, naming it, before anything"

boxes=$(grep -c -- "-l dispatch-loadtest/run=$run,dispatch-loadtest/role=nats-box -o json" "$fake/kubectl.log" || true)
FAKE_STORED_ORDER=mismatch run_cluster step --namespace "$ns" reads 10 --duration 1m --reads-only
((RC == 0)) || fail "step --reads-only with a stored-order mismatch exited $RC"
jq -e --argjson k "$checker" 'del(.evidence) == $k + {exit: 1, since: "2026-10-28T10:00:00Z", documents: 3, clean: 1, mismatched: 1, refused: 1, failed: 0, unchecked: null}' \
  "$run_dir/steps/reads/stored-order.json" >/dev/null || fail "a mismatching run's stored-order.json is wrong: $(cat "$run_dir/steps/reads/stored-order.json" 2>/dev/null)"
grep -qF 'step reads: stored-order (envoy-dispatch of dispatch commit aaaaaaaaaaaa, renderer_matches true) since 2026-10-28T10:00:00Z: exit 1, documents=3 clean=1 mismatched=1 refused=1 failed=0 unchecked=null; a correctness finding, not a step failure' "$tmp/out" ||
  fail "step reads did not print the mismatching stored-order counts as a finding"
[ ! -f "$run_dir/steps/reads/step-failed.json" ] || fail "a stored-order mismatch was recorded as a step failure: $(cat "$run_dir/steps/reads/step-failed.json")"
grep -q "^cgroup-merge $run_dir/steps/reads " "$fake/loadgen.log" || fail "step reads did not go on to cgroup-merge after a stored-order mismatch"
# The evidence of the mismatched document (11111111-...) and the refused one (22222222-...): each
# table psql \copy'd to stdout from database dispatch in the Postgres pod, gzipped here, the
# issue's events since the window start, version 7's markdown, and the start-* tables from the
# template dispatch_seed the step was reset from; none of the clean 33333333-.... The fake's
# doc_checkpoints of 22222222-... fails: reported, no step failure.
so_mismatch=11111111-1111-4111-8111-111111111111 so_refused=22222222-2222-4222-8222-222222222222
ev=$run_dir/steps/reads/stored-order-evidence
# evidence_calls OUT: the evidence export's psql calls in the fake kubectl's log (those that set a
# statement_timeout) into OUT, each statement_timeout written as N; fails when one is not 1-600 s.
evidence_calls() {
  grep -F -- "-c set statement_timeout = '" "$fake/kubectl.log" | sed -E "s/statement_timeout = '[0-9]+s'/statement_timeout = 'Ns'/" >"$1"
  grep -oE "statement_timeout = '[0-9]+s'" "$fake/kubectl.log" | tr -dc '0-9\n' | awk '$1 < 1 || $1 > 600 { bad = 1 } END { exit bad }'
}
# psql_call DB: an evidence export's kubectl call up to its first SQL, on database DB.
psql_call() { printf '%s' "-n $ns exec postgres-0 -c postgres -- psql -X -q -U dispatch -d $1 -v ON_ERROR_STOP=1 -c set statement_timeout = 'Ns' -c"; }
evidence_calls "$tmp/evidence-calls" || fail "an evidence export's statement_timeout is not within its 600 s: $(grep -oE "statement_timeout = '[0-9]+s'" "$fake/kubectl.log")"
for id in "$so_mismatch" "$so_refused"; do
  for t in "artifacts where id = '$id'" "doc_checkpoints where artifact_id = '$id'" "artifact_versions where artifact_id = '$id' order by number" \
    "doc_updates where artifact_id = '$id' order by version" "doc_snapshots where artifact_id = '$id' order by name"; do
    grep -qxF -- "$(psql_call dispatch) \\copy (select * from $t) to stdout with csv header" "$tmp/evidence-calls" ||
      fail "the evidence export did not \\copy $t to stdout from database dispatch in the Postgres pod: $(grep -F -- '\copy' "$tmp/evidence-calls")"
    [ "${t%% *}" = artifacts ] && continue
    grep -qxF -- "$(psql_call dispatch_seed) \\copy (select * from $t) to stdout with csv header" "$tmp/evidence-calls" ||
      fail "the evidence export did not \\copy $t from the step's template dispatch_seed: $(grep -F -- '\copy' "$tmp/evidence-calls")"
  done
  grep -qxF -- "$(psql_call dispatch) \\copy (select e.* from events e where (e.artifact_id = '$id' or e.issue_key = (select issue_key from artifacts where id = '$id')) and e.created_at >= '2026-10-28T10:00:00Z' order by e.seq) to stdout with csv header" "$tmp/evidence-calls" ||
    fail "the evidence export did not \\copy $id's issue events since the window start from database dispatch: $(grep -F events "$tmp/evidence-calls")"
done
[ "$(grep -cF -- '\copy' "$fake/kubectl.log")" = 20 ] || fail "the evidence export ran other \\copy calls: $(grep -F -- '\copy' "$fake/kubectl.log")"
! grep -qF 33333333 "$fake/kubectl.log" || fail "the evidence export took the clean document 33333333-..."
grep -qxF -- "$(psql_call dispatch) \\pset tuples_only on -c \\pset format unaligned -c select encode(convert_to(markdown, 'UTF8'), 'base64') from artifact_versions where artifact_id = '$so_mismatch' and number = 7 and markdown is not null" \
  "$tmp/evidence-calls" || fail "the evidence export did not read version 7's markdown: $(grep -F markdown "$fake/kubectl.log")"
[ "$(grep -cF "encode(convert_to(markdown" "$fake/kubectl.log")" = 1 ] || fail "the evidence export read the markdown of a version that did not mismatch"
# names DIR: the names in DIR, sorted, on one line.
names() { find "$1" -mindepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' '; }
start_files="start-artifact_versions.csv.gz start-doc_checkpoints.csv.gz start-doc_snapshots.csv.gz start-doc_updates.csv.gz"
[ "$(names "$ev/$so_mismatch")" = "artifact_versions.csv.gz artifacts.csv.gz doc_checkpoints.csv.gz doc_snapshots.csv.gz doc_updates.csv.gz issue-events-in-window.csv.gz $start_files version-7.md " ] ||
  fail "the mismatched document's evidence is not one gzipped CSV per table, the events, the start-* tables and version-7.md: $(names "$ev/$so_mismatch")"
[ "$(names "$ev/$so_refused")" = "artifact_versions.csv.gz artifacts.csv.gz doc_snapshots.csv.gz doc_updates.csv.gz issue-events-in-window.csv.gz $start_files " ] ||
  fail "the refused document's evidence is not the tables that exported: $(names "$ev/$so_refused")"
printf 'number,markdown\n1,"# One\n\nbody"\n7,"# Seven ""x"""\n' | cmp -s - <(gzip -dc "$ev/$so_mismatch/artifact_versions.csv.gz") ||
  fail "artifact_versions.csv.gz is not psql's CSV: $(gzip -dc "$ev/$so_mismatch/artifact_versions.csv.gz")"
printf 'id,issue_key,seq,type,payload\n1,LGA-1,1,doc.updated,"{""a"":\n1}"\n2,LGA-1,2,doc.updated,{}\n' | cmp -s - <(gzip -dc "$ev/$so_mismatch/issue-events-in-window.csv.gz") ||
  fail "issue-events-in-window.csv.gz is not psql's CSV: $(gzip -dc "$ev/$so_mismatch/issue-events-in-window.csv.gz")"
printf 'artifact_id,version,update\nfake,1,\\x01\n' | cmp -s - <(gzip -dc "$ev/$so_mismatch/start-doc_updates.csv.gz") ||
  fail "start-doc_updates.csv.gz is not the template's CSV: $(gzip -dc "$ev/$so_mismatch/start-doc_updates.csv.gz")"
printf '# Seven "x"\n\nbody\n' | cmp -s - "$ev/$so_mismatch/version-7.md" || fail "version-7.md is not the stored markdown: $(cat "$ev/$so_mismatch/version-7.md")"
jq -e --arg m "$so_mismatch" --arg r "$so_refused" '
  {"start-doc_checkpoints.csv.gz": 0, "start-artifact_versions.csv.gz": 0, "start-doc_updates.csv.gz": 1, "start-doc_snapshots.csv.gz": 0} as $start
  | .evidence == [
    {artifact: $m, files: ({"artifacts.csv.gz": 1, "doc_checkpoints.csv.gz": 0, "artifact_versions.csv.gz": 2, "doc_updates.csv.gz": 3,
      "doc_snapshots.csv.gz": 1, "issue-events-in-window.csv.gz": 2} + $start), versions: [7]},
    {artifact: $r, files: ({"artifacts.csv.gz": 1, "artifact_versions.csv.gz": 2, "doc_updates.csv.gz": 3, "doc_snapshots.csv.gz": 1,
      "issue-events-in-window.csv.gz": 2} + $start), versions: [],
     error: ["doc_checkpoints.csv.gz: exit 1: ERROR:  canceling statement due to statement timeout"]}]' \
  "$run_dir/steps/reads/stored-order.json" >/dev/null || fail "stored-order.json's evidence is wrong: $(jq -c .evidence "$run_dir/steps/reads/stored-order.json")"
grep -qF "step reads: the stored-order evidence of $so_refused is incomplete: doc_checkpoints.csv.gz: exit 1: ERROR:  canceling statement due to statement timeout" "$tmp/out" ||
  fail "step reads did not report the failed part of the evidence export"
grep -qF "step reads: the stored-order evidence of 2 mismatched or refused documents is in $ev (kept on this pod, not uploaded)" "$tmp/out" ||
  fail "step reads did not name where the stored-order evidence is"
start=$(grep '^step-reads ' "$fake/runner-starts")
[[ $start == *"-events 0 "* && $start == *"-requests 600 "* ]] || fail "a --reads-only step at 10x is not 0 events and 600 requests: $start"
jq -e '.phase == "steps" and .reads_only == true' "$run_dir/steps/reads/copy-meta.json" >/dev/null ||
  fail "a --reads-only step's copy-meta.json does not record reads_only true: $(cat "$run_dir/steps/reads/copy-meta.json")"
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
# The two long writes of replay-prepare, which kube's 300 s would cut, carry a bound of their own
# (postgres_exec -t) and go through the guard with every other write into postgres-0: each is the
# kubectl call right after a read of the lock.
guarded_by_lock_read 'exec -i postgres-0 -c postgres -- sh -c cat >"$1"' ||
  fail "the seed dump streamed into postgres-0 did not go through postgres_exec: no lock read came right before it ($(grep -nF -- 'sh -c cat >"$1"' "$fake/kubectl.log" | tail -n1))"
guarded_by_lock_read 'exec postgres-0 -c postgres -- pg_restore' ||
  fail "the pg_restore of the seed dump did not go through postgres_exec: no lock read came right before it ($(grep -nF -- 'pg_restore' "$fake/kubectl.log" | tail -n1))"
[ -f "$fake/replay-template" ] || fail "replay-prepare did not rename the database to dispatch_replay"
jq -e --arg dump "$(jq -r .seed.dump_sha256 "$seeded")" --arg tar "$(jq -r .bundle.tar_zst_sha256 "$seeded")" \
  '.seed_dump_sha256 == $dump and .bundle_tar_zst_sha256 == $tar and .migration_before == 121 and .migration_after == 121
    and .schema_version == 121 and .checksum.md5 == "fakemd5" and (.seconds | has("restore") and has("quiet") and has("total"))' \
  "$run_dir/replay-prepared.json" >/dev/null || fail "replay-prepared.json lacks a field: $(cat "$run_dir/replay-prepared.json")"
FAKE_STORED_ORDER=nosummary run_cluster replay --namespace "$ns" r16
((RC == 0)) || fail "replay r16 exited $RC"
jq -e --argjson k "$checker" '. == $k + {exit: 1, since: "2026-10-28T10:00:00Z", documents: null, clean: null, mismatched: null, refused: null, failed: null, unchecked: null, evidence: []}' \
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
jq -e '.phase == "replay" and .reads_only == false' "$run_dir/steps/r16/copy-meta.json" >/dev/null ||
  fail "the replay's copy-meta.json does not record the phase replay: $(cat "$run_dir/steps/r16/copy-meta.json")"
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
# The replay's own long write, the bundle into the runner, carries its bound (runner_exec -t) and
# goes through the guard too.
guarded_by_lock_read 'exec -i runner -c runner -- tar -C /work -xf -' ||
  fail "the bundle streamed into the runner did not go through runner_exec: no lock read came right before it ($(grep -nF -- 'tar -C /work -xf -' "$fake/kubectl.log" | tail -n1))"
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
# The evidence stays on the pod: the steps tar leaves it out, and collect leaves it in place. (Each
# listing is read whole before grep -q, so its early exit cannot fail tar with SIGPIPE.)
grep -qx 'reads/stored-order.json' <<<"$(tar -tf "$part")" || fail "the steps tar lacks reads/stored-order.json: $(tar -tf "$part" | tr '\n' ' ')"
! tar -tf "$part" | grep -q stored-order-evidence || fail "the steps tar, which collect uploads, holds the stored-order evidence"
[ -f "$ev/$so_mismatch/doc_updates.csv.gz" ] || fail "collect did not leave the stored-order evidence in the step directory"
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
none6='none | none | none | none | none | none'
printf '%s\n' "| smoke | 2026-10-28T10:00:00Z | 0 | 2 | 2 | 0 | 0 | 0 | null | $none6 | true | none |" \
  "| reads | 2026-10-28T10:00:00Z | 1 | 3 | 1 | 1 | 1 | 0 | null | $none6 | true | none |" \
  "| r16 | 2026-10-28T10:00:00Z | 1 | null | null | null | null | null | null | $none6 | true | none |" >"$tmp/want-stored-order"
sed -n '/^| --- /,$p' "$run_dir/STORED-ORDER.md" | tail -n +2 | diff -u "$tmp/want-stored-order" - >&2 ||
  fail "STORED-ORDER.md does not hold one row per step, in the order they ran, with their counts, exit, the typed call's counts and renderer_matches"
grep -qF "The checker was built for dispatch commit $commit_ok from that commit's own tree (no overlay). renderer_matches: true. Binary sha256 $(jq -r .stored_order.sha256 "$run_dir/state.json")." "$run_dir/STORED-ORDER.md" ||
  fail "STORED-ORDER.md does not name the commit the checker was built from, its overlay, renderer_matches and sha256: $(cat "$run_dir/STORED-ORDER.md")"
! grep -qF -- "$pg_password" "$run_dir/STORED-ORDER.md" || fail "STORED-ORDER.md holds the Postgres password"
results=$(find "$run_dir/upload" -name 'cluster-load-test-*-raw-results.tar.zst.part00' | head -n1)
[ -n "$results" ] || fail "collect wrote no part of phase results"
for f in curves/CURVES.md curves/refine.txt TABLES.md STORED-ORDER.md steps/smoke/CODEPATHS.md steps/reads/CODEPATHS.md steps/r16/CODEPATHS.md; do
  grep -qx -- "$f" <<<"$(tar -tf "$results")" || fail "the results tar lacks $f: $(tar -tf "$results" | tr '\n' ' ')"
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
for s in 1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4; do
  jq -e --arg c "$commit_ok" '.exit == 0 and .built_from == $c and .dispatch_commit == $c and .overlay_source == null and .renderer_matches == true' \
    "$run_dir/steps/$s/stored-order.json" >/dev/null || fail "all's step $s has no clean stored-order.json from the dispatch commit's checker"
done
[ "$(grep -c '| true | none |$' "$run_dir/STORED-ORDER.md")" = 9 ] || fail "STORED-ORDER.md of all does not hold 9 rows whose renderer matches"
grep -q '^step-10x-hour .*-duration 5m ' "$fake/runner-starts" || fail "quick's hour is not 5 min"
grep -q '^step-1x-db4 .*-duration 2m ' "$fake/runner-starts" || fail "quick's steps are not 2 min"
[ ! -e "$run_dir/phases/refinement" ] || fail "all --profile quick ran a refinement step"
[ "$(grep -c '^curves -out ' "$fake/loadgen.log")" = 1 ] || fail "all --profile quick ran loadgen curves outside collect"
[ "$(logged_steps 'tables -tops=false')" = "1x 2x 3x 5x 7x 10x 10x-reads 10x-hour 1x-db4" ] ||
  fail "all's TABLES.md lists the steps as '$(logged_steps 'tables -tops=false')', not in the order they ran"
[ -f "$run_dir/steps/1x/CODEPATHS.md" ] || fail "all's collect wrote no CODEPATHS.md"
[ ! -f "$fake/lock.json" ] || fail "all left the lock"
! grep -q '^replay-' "$fake/runner-starts" || fail "all ran a replay on a manifest whose seed is null"
pass "all runs up, seed, the series, soak, db-size, 1x-db4, collect and down, the stored-order check on each step, one line per skipped phase"

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
FAKE_QUIET=1 run_cluster all "${up_args[@]}" --dispatch-commit "$commit_old" --stored-order-source "$commit_so" --profile quick --manifest "$seeded" --upload-issue TEST-1
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
grep -q '^\./r4/\|^r4/' <<<"$(tar -tf "$part")" || fail "the phase replay's tar lacks r4: $(tar -tf "$part" | head -n5)"
[ -f "$run_dir/steps/1x/pg-stats-after.json" ] || fail "all's step 1x has no pg-stats-after.json"
! grep -q -- "^curves -out .*/steps/r16" "$fake/loadgen.log" || fail "all's loadgen curves took a replay step"
# all --stored-order-source: every step and replay runs the overlaid checker while Dispatch runs,
# before the next scale-down of Dispatch (a reset, replay-prepare or db-size).
for s in 1x 2x 3x 5x 7x 10x 10x-reads 10x-hour r16 1x-db4 r4; do
  jq -e --arg d "$commit_old" --arg s "$commit_so" '.exit == 0 and .documents == 2 and .evidence == [] and .built_from == $d and .overlay_source == $s and .renderer_matches' \
    "$run_dir/steps/$s/stored-order.json" >/dev/null || fail "all's step $s has no clean stored-order.json of the overlaid checker"
done
! grep -qF -- '\copy' "$fake/kubectl.log" || fail "all's clean stored-order runs exported evidence"
[ -z "$(find "$run_dir/steps" -name stored-order-evidence)" ] || fail "all's clean stored-order runs wrote stored-order-evidence/"
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

# ---- (c) browser typing ----------------------------------------------------------------------------

# The replay fixture for browser typing: a bundle state of one issue with four documents and a
# file, its window's events, and loadtest.replay_ids's artifact rows. In 0-5m after the state's
# 22:55: doc-1 has 3 agent writes (one at a fractional second, one with a +00:00 offset), doc-2 2
# (and 2 by a person and a settlement, which do not count), doc-3 2 (and 3 more after 5m and one
# before 22:55); doc-4 4 but no replay_ids row; file-1 5 but a file; doc-5 5 but not in the state
# (a document the window creates); and comment events on doc-3, which are no document writes.
readonly typing_l1=10000000-0000-4000-8000-000000000001 typing_l2=10000000-0000-4000-8000-000000000002
readonly typing_l3=10000000-0000-4000-8000-000000000003
# typing_event TYPE ACTOR_KIND CREATED_AT ARTIFACT [NAMED DIFF]: one window.jsonl line.
typing_event() {
  jq -nc --arg t "$1" --arg k "$2" --arg at "$3" --arg a "$4" --argjson named "${5:-false}" --argjson diff "${6:-\"@@ -1 +1 @@\"}" \
    '{id: 1, issue_key: "FAKE-1", artifact_id: $a, project: "FAKE", seq: 1, type: $t, actor: {kind: $k, id: "s-1"}, notify: false,
      created_at: $at, payload: {artifact_id: $a, name: "doc", diff: $diff, version: {named: $named, size: 10}}}'
}
# write_typing_bundle DIR IDS: the fixture's state.json and window.jsonl into DIR, its
# replay_ids rows ("<logged>|<local>", as psql -At prints them) into IDS.
write_typing_bundle() {
  local v=artifact.version t
  mkdir -p "$1"
  jq -n '{project: "FAKE", at: "2026-10-26T22:55:00Z", until: "2026-10-26T23:34:00Z", issues: [{key: "FAKE-1", number: 1, exists: true,
    created_at: "2026-10-01T00:00:00Z", artifacts: ([("doc-1", "doc-2", "doc-3", "doc-4") | {id: ., name: ., slug: ., kind: "doc"}]
      + [{id: "file-1", name: "f", slug: "f", kind: "file"}])}]}' >"$1/state.json"
  {
    typing_event "$v" session 2026-10-26T22:55:10Z doc-1
    typing_event "$v" session 2026-10-26T22:55:20.5Z doc-1
    typing_event "$v" session 2026-10-26T22:56:00+00:00 doc-1
    typing_event "$v" session 2026-10-26T22:57:00Z doc-2
    typing_event "$v" user 2026-10-26T22:57:30Z doc-2
    typing_event "$v" user 2026-10-26T22:57:31Z doc-2
    typing_event "$v" session 2026-10-26T22:58:00Z doc-2
    typing_event "$v" session 2026-10-26T22:59:00Z doc-2 true null
    typing_event "$v" session 2026-10-26T22:54:00Z doc-3
    typing_event "$v" session 2026-10-26T22:56:30Z doc-3
    typing_event "$v" session 2026-10-26T22:59:59.999Z doc-3
    for t in 23:01:00 23:02:00 23:03:00; do typing_event "$v" session "2026-10-26T${t}Z" doc-3; done
    for t in 22:55:01 22:55:02 22:55:03; do typing_event comment.created session "2026-10-26T${t}Z" doc-3; done
    for t in 22:56:01 22:56:02 22:56:03 22:56:04; do typing_event "$v" session "2026-10-26T${t}Z" doc-4; done
    for t in 22:57:01 22:57:02 22:57:03 22:57:04 22:57:05; do typing_event "$v" session "2026-10-26T${t}Z" file-1; done
    for t in 22:58:01 22:58:02 22:58:03 22:58:04 22:58:05; do typing_event "$v" session "2026-10-26T${t}Z" doc-5; done
  } >"$1/window.jsonl"
  printf '%s\n' "doc-1|$typing_l1" "doc-2|$typing_l2" "doc-3|$typing_l3" "doc-5|10000000-0000-4000-8000-000000000005" \
    "file-1|10000000-0000-4000-8000-00000000000f" >"$2"
}

reset_fake
echo '400000 100000' >"$fake/cpu.max"
export LOADTEST_CLUSTER_ROOT=$tmp/root-typing
write_typing_bundle "$fake/bundle-fixture" "$fake/replay-ids"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --browsertype-commit "$commit_bt"
((RC == 0)) || fail "up --browsertype-commit exited $RC"
run=$(jq -r .data.run "$fake/lock.json")
run_dir=$LOADTEST_CLUSTER_ROOT/$run
jq -e --arg c "$commit_bt" --arg s "$(sha256sum <"$run_dir/browsertype" | cut -d' ' -f1)" '.browsertype == {commit: $c, sha256: $s}' \
  "$run_dir/state.json" >/dev/null || fail "state.json does not record the browsertype commit and binary's sha256: $(jq -c .browsertype "$run_dir/state.json")"
grep -qx -- "-C $(cd -- "$here/../../../.." && pwd) archive $commit_bt packages/envoy" "$fake/git.log" ||
  fail "up did not git archive packages/envoy of $commit_bt: $(cat "$fake/git.log")"
# Built in up's scratch directory before the lock, then moved to $run_dir/browsertype.
grep -qxE -- "GOWORK=off CGO_ENABLED=0 go build -buildvcs=false -o $LOADTEST_CLUSTER_ROOT/\.checker\.[^/]+/browsertype \./loadtest/browsertype" "$fake/go-browsertype.log" ||
  fail "up did not build a static browsertype with build_at_commit before the lock: $(cat "$fake/go-browsertype.log" 2>/dev/null)"
[ -x "$run_dir/browsertype" ] || fail "up did not move browsertype into $run_dir"
{ [ -s "$fake/go-dispatch.log" ] && [ -x "$run_dir/envoy-dispatch" ]; } || fail "up --browsertype-commit did not build the stored-order checker too"
[ -z "$(find "$run_dir" -maxdepth 1 -name '.build-src.*')" ] || fail "up left a build's scratch source in the run directory"
[ "$(grep -c -- "^-n $ns cp $run_dir/browsertype runner:/work/browsertype " "$fake/kubectl.log")" = 1 ] || fail "up did not copy browsertype into the runner"
dispatch_token=$(jq -r .secrets.dispatch_token "$run_dir/state.json")
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC"
# Five issues, out of key order; the step's sessions (400) cover them all.
jq -n '{projects: ["LGA"], issues: [3, 1, 5, 2, 4 | {key: "LGA-\(.)", project: "LGA", artifact_id: "20000000-0000-4000-8000-00000000000\(.)"}]}' >"$run_dir/seed.json"
for c in "--typing-rate 0|--typing-rate '0' is not a number" "--typing-peers x|--typing-peers 'x' is not a whole number" \
  "--typing-docs 0|--typing-docs '0' is not a whole number"; do
  read -ra words <<<"${c%%|*}"
  run_cluster step --namespace "$ns" bad 1 --typing "${words[@]}"
  { ((RC != 0)) && grep -qF -- "${c#*|}" "$tmp/out"; } || fail "step accepted --typing ${c%%|*}"
  [ ! -e "$run_dir/steps/bad" ] || fail "step --typing ${c%%|*} made its step directory"
done

# The step's documents: the 3 that loadgen plan-docs, over the step's own flags, says its
# in-window edits and uploads plan the most versions for (the fake plan-docs reads
# FAKE_PLAN_VERSIONS in the seed's order: LGA-3 40, LGA-1 12, LGA-5 7, LGA-2 30, LGA-4 60).
FAKE_PLAN_VERSIONS='40 12 7 30 60' run_cluster step --namespace "$ns" 10x-typing 10 --duration 1m --typing --typing-docs 3 --typing-peers 3 --typing-rate 2.5
((RC == 0)) || fail "step --typing exited $RC"
tdir=$run_dir/steps/10x-typing
want_docs=$(printf '20000000-0000-4000-8000-00000000000%s\n' 4 3 2)
grep -qxF -- "plan-docs -json -seed $run_dir/seed.json -sessions 400 -top 3 -events 40 -warmup 30s -duration 1m -rng-seed 1" "$fake/loadgen.log" ||
  fail "the step did not choose its typing documents with loadgen plan-docs over its own flags: $(grep '^plan-docs' "$fake/loadgen.log")"
[ "$(cat "$fake/runner/work/typing/10x-typing.docs")" = "$want_docs" ] ||
  fail "the step's typing documents are not the 3 plan-docs plans the most versions for: $(cat "$fake/runner/work/typing/10x-typing.docs" 2>/dev/null)"
! grep -q 'WARNING: step 10x-typing:' "$tmp/out" ||
  fail "a step whose 3rd typed document has 30 planned versions warned: $(grep 'WARNING: step 10x-typing:' "$tmp/out")"
grep -qxF -- "typing-10x-typing /work/browsertype -url http://dispatch.$ns.svc:8766 -token-file /work/typing/token -session ses-browsertype -artifacts-file /work/typing/10x-typing.docs -peers 3 -rate 2.5 -duration 8810s -seed 1 -protect Revision token" \
  "$fake/runner-starts" || fail "browsertype did not start with the step's flags: $(grep '^typing-' "$fake/runner-starts")"
# The token: written into the runner from standard input, on no command line, in no file under the
# step directory, never printed.
[ "$(cat "$fake/runner/work/typing/token")" = "$dispatch_token" ] || fail "the runner's token file does not hold the copy's agent token"
# (loadgen run's own command line carries -token; browsertype's calls are those naming it, its
# job or its runner directory.)
grep -E 'browsertype|/work/typing|sh typing-' "$fake/kubectl.log" >"$tmp/typing-calls" || fail "kubectl.log holds no browsertype call"
! grep -qF -- "$dispatch_token" "$tmp/typing-calls" || fail "a browsertype kubectl call carries the agent token on its command line"
! grep -rqF -- "$dispatch_token" "$tdir" || fail "a file under the step directory holds the agent token: $(grep -rlF -- "$dispatch_token" "$tdir")"
! grep -qF -- "$dispatch_token" "$tmp/out" || fail "step --typing printed the agent token"
# The order in kubectl.log: the token, browsertype's start, loadgen's start (its warm-up, then its
# window), the two stored-order commands, then browsertype's SIGTERM.
# shellcheck disable=SC2016 # the token write's runner script, as kubectl.log holds it
token_at=$(line_of 'cat >"$1/token"') bt_at=$(line_of 'sh typing-10x-typing /work/browsertype ') lg_at=$(line_of ' sh step-10x-typing ')
so_at=$(line_of '/work/envoy-dispatch stored-order' last) kill_at=$(line_of 'kill -TERM' last)
grep -n -- 'kill -TERM' "$fake/kubectl.log" | tail -n1 | grep -qF 'sh typing-10x-typing' || fail "the SIGTERM did not go to typing-10x-typing"
((token_at < bt_at && bt_at < lg_at && lg_at < so_at && so_at < kill_at)) ||
  fail "the order is not token, browsertype, loadgen, stored-order, SIGTERM (kubectl.log lines $token_at, $bt_at, $lg_at, $so_at, $kill_at)"
# The typed documents first, one --artifact each, then the run over every document, both since the
# window start and both within the 7200 s bound.
mapfile -t so_calls < <(grep -F '/work/envoy-dispatch stored-order' "$fake/kubectl.log")
[ "${#so_calls[@]}" = 2 ] ||
  fail "a typing step ran ${#so_calls[@]} stored-order commands, not the typed documents' and then every document's: $(printf '%s\n' "${so_calls[@]}")"
mapfile -t want_docs_a <<<"$want_docs"
typed_args=$(printf -- ' --artifact %s' "${want_docs_a[@]}")
[[ ${so_calls[0]} == *" sh 7200 2026-10-28T10:00:00Z$typed_args" ]] ||
  fail "the first stored-order call is not the typed documents' within the bound: ${so_calls[0]}"
{ [[ ${so_calls[1]} =~ \ sh\ ([0-9]+)\ 2026-10-28T10:00:00Z$ ]] && ((BASH_REMATCH[1] > 0 && BASH_REMATCH[1] <= 7200)); } ||
  fail "the second stored-order call is not the run over every document within what is left of the bound: ${so_calls[1]}"
grep -qx 'stored-order: documents=2 clean=2 mismatched=0 refused=0 failed=0' "$tdir/stored-order-typed.txt" ||
  fail "stored-order-typed.txt does not hold the typed call's output: $(cat "$tdir/stored-order-typed.txt" 2>/dev/null)"
jq -e --argjson k "$checker" '. == $k + {exit: 0, since: "2026-10-28T10:00:00Z", documents: 2, clean: 2, mismatched: 0, refused: 0, failed: 0, unchecked: null}' \
  "$tdir/stored-order-typed.json" >/dev/null || fail "the typed call's stored-order-typed.json is wrong: $(cat "$tdir/stored-order-typed.json" 2>/dev/null)"
grep -qF 'step 10x-typing: stored-order over the 3 typed documents (envoy-dispatch of dispatch commit aaaaaaaaaaaa, renderer_matches true) since 2026-10-28T10:00:00Z: exit 0, documents=2 clean=2' "$tmp/out" ||
  fail "the typed stored-order call printed no line of its own"
grep -qx 'artifact=a peer=1 sent=38 skipped=0 errors=0 error=""' "$tdir/browsertype.txt" || fail "browsertype.txt does not hold browsertype's output"
jq -e --arg c "$commit_bt" --argjson d "$(jq -Rn '[inputs]' <<<"$want_docs")" \
  --argjson p "$(jq -nc --args '{($ARGS.positional[0]): 60, ($ARGS.positional[1]): 40, ($ARGS.positional[2]): 30}' "${want_docs_a[@]}")" \
  '. == {commit: $c, docs: $d, planned_versions: $p, peers: 3, rate: 2.5, exit: 0, sent: 78, skipped: 1, errors: 0}' "$tdir/browsertype.json" >/dev/null ||
  fail "browsertype.json is wrong: $(cat "$tdir/browsertype.json")"
grep -qF 'step 10x-typing: browsertype exit 0: sent=78 skipped=1 errors=0' "$tmp/out" || fail "step --typing did not print browsertype's sums"
[ ! -f "$tdir/step-failed.json" ] || fail "a typing step was marked failed: $(cat "$tdir/step-failed.json")"

# --typing-docs 20 over a seed of 5 documents: one warning line, since the 20th has no planned
# version and INFRA-707's acceptance 2 needs 10 of every typed document.
FAKE_TYPING_RC=1 FAKE_PLAN_VERSIONS='40 12 7 30 60' run_cluster step --namespace "$ns" 10x-typing-err 10 --duration 1m --typing
((RC == 0)) || fail "step --typing with a peer error exited $RC"
grep -qF 'WARNING: step 10x-typing-err: typed document number 20 has 0 planned versions, under the 10 versions INFRA-707'"'"'s acceptance 2 needs of every typed document (loadgen plan-docs named 5 documents of --typing-docs 20); the step runs on' "$tmp/out" ||
  fail "a step whose 20th typed document has no planned version did not warn: $(grep WARNING "$tmp/out")"
grep -q '^typing-10x-typing-err .* -peers 2 -rate 5 ' "$fake/runner-starts" || fail "step --typing's defaults are not 2 peers at 5 keystrokes/s"
jq -e '.exit == 1 and .errors == 1 and .sent == 78 and (.docs | length) == 5 and (.planned_versions | length) == 5
  and .planned_versions["20000000-0000-4000-8000-000000000005"] == 7' "$run_dir/steps/10x-typing-err/browsertype.json" >/dev/null ||
  fail "browsertype.json with a peer error is wrong: $(cat "$run_dir/steps/10x-typing-err/browsertype.json")"

FAKE_QUIET=1 run_cluster replay-prepare --namespace "$ns" --manifest "$seeded"
((RC == 0)) || fail "replay-prepare for the typing replay exited $RC"
run_cluster replay --namespace "$ns" r16-typing --to 5m --typing --typing-docs 2
((RC == 0)) || fail "replay --typing exited $RC"
grep -qF -- "-d dispatch -v ON_ERROR_STOP=1 -At -c select logged_id, local_id from loadtest.replay_ids where kind = 'artifact'" "$fake/kubectl.log" ||
  fail "replay --typing did not read loadtest.replay_ids from database dispatch"
jq -e --arg a "$typing_l1" --arg b "$typing_l2" '.docs == [$a, $b] and .exit == 0 and .planned_versions == null' "$run_dir/steps/r16-typing/browsertype.json" >/dev/null ||
  fail "replay --typing did not type into the 2 documents the window writes most, with no planned versions: $(cat "$run_dir/steps/r16-typing/browsertype.json")"
mapfile -t so_calls < <(grep -F '/work/envoy-dispatch stored-order' "$fake/kubectl.log" | tail -n2)
[[ ${so_calls[0]} == *" --artifact $typing_l1 --artifact $typing_l2" && ${so_calls[1]} != *--artifact* ]] ||
  fail "replay --typing did not run the typed documents' stored-order call first: $(printf '%s\n' "${so_calls[@]}")"
bt_at=$(line_of 'sh typing-r16-typing /work/browsertype ') lg_at=$(line_of ' sh replay-r16-typing ') kill_at=$(line_of 'kill -TERM' last)
so_at=$(line_of '/work/envoy-dispatch stored-order' last)
((bt_at < lg_at && lg_at < so_at && so_at < kill_at)) || fail "replay --typing's order is not browsertype, loadgen, stored-order, SIGTERM"

run_cluster step --namespace "$ns" plain 1 --duration 1m
((RC == 0)) || fail "step plain exited $RC"
{ [ ! -e "$run_dir/steps/plain/browsertype.json" ] && ! grep -q '^typing-plain ' "$fake/runner-starts"; } || fail "a step without --typing ran browsertype"
run_cluster collect --namespace "$ns"
((RC == 0)) || fail "collect of the typing run exited $RC"
grep -qF "built from commit $commit_bt" "$run_dir/STORED-ORDER.md" || fail "STORED-ORDER.md does not name the browsertype commit"
sed -n '/^| --- /,$p' "$run_dir/STORED-ORDER.md" | tail -n +2 | awk -F' [|] ' '{ print $1 " " $(NF - 6) " " $NF }' >"$tmp/typing-column"
printf '%s\n' '| 10x-typing 2 78 |' '| 10x-typing-err 2 78 |' '| r16-typing 2 78 |' '| plain none none |' | diff -u - "$tmp/typing-column" >&2 ||
  fail "STORED-ORDER.md's typed documents and typing columns are not each step's typed count and sent updates, or none"
[ "$(logged_steps 'curves -out')" = plain ] || fail "collect's curves took a typing step: '$(logged_steps 'curves -out')'"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down of the typing run exited $RC"
pass "up --browsertype-commit builds browsertype; step and replay --typing start it before loadgen with the token from standard input, type into the documents their own writes plan the most versions for, stop it after the stored-order commands, and record browsertype.txt and browsertype.json; a peer error is no step failure; collect's STORED-ORDER.md has the typed and typing columns and its curves leave typing steps out"

# up --stored-order-source: the checker is the dispatch tree (commit_old, without the command) with
# the source's INFRA-707 files copied into its scratch copy and the source's stored-order entry
# registered on the line after the table's opener; state.json records the overlay.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-overlay
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_old" --stored-order-source "$commit_so"
((RC == 0)) || fail "up --stored-order-source exited $RC: $(cat "$tmp/out")"
run_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")
jq -e --arg d "$commit_old" --arg s "$commit_so" --argjson f "$(printf '%s\n' "${so_files[@]}" | jq -Rnc '[inputs]')" --arg r "$so_registry" \
  --arg sha "$(sha256sum <"$run_dir/envoy-dispatch" | cut -d' ' -f1)" \
  '.stored_order == {dispatch_commit: $d, built_from: $d, overlay_source: $s, overlay_files: $f, registration: $r, renderer_matches: true, sha256: $sha}' \
  "$run_dir/state.json" >/dev/null || fail "state.json does not record the overlaid checker: $(jq -c .stored_order "$run_dir/state.json")"
repo=$(cd -- "$here/../../../.." && pwd)
grep -qx -- "-C $repo archive $commit_old packages/envoy" "$fake/git.log" || fail "up did not archive the dispatch commit's tree: $(cat "$fake/git.log")"
grep -qx -- "-C $repo archive $commit_so ${so_files[*]}" "$fake/git.log" || fail "up did not archive the source's INFRA-707 files: $(cat "$fake/git.log")"
[ "$(cat "$fake/go-dispatch-tree.log")" = ./cmd/dispatch/storedorder.go$'\n'./internal/dispatch/docs/storedorder.go ] ||
  fail "the checker's tree does not hold the source's INFRA-707 files: $(cat "$fake/go-dispatch-tree.log")"
printf 'package main\n\n%s\n' "$(printf '%s\n' "$so_opener" "$so_entry" "$so_census" '}')" | diff -u - "$fake/go-subcommands.go" >&2 ||
  fail "the overlay did not register the source's stored-order entry right after the table's opener"
grep -qF "building the stored-order checker, envoy-dispatch (static) of dispatch commit $commit_old with INFRA-707's files of source commit $commit_so" "$tmp/out" ||
  fail "up did not name the dispatch and source commits of the checker"
! grep -q WARNING "$tmp/out" || fail "up warned about an overlaid checker: $(grep WARNING "$tmp/out")"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down of the overlay run exited $RC"
pass "up --stored-order-source adds the source's INFRA-707 files and stored-order registration to a scratch copy of the dispatch tree, builds it and records the overlay with renderer_matches true"

# up --stored-order-checker-commit: the checker wholly from another commit, a warning, and
# renderer_matches false.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-escape
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_old" --stored-order-checker-commit "$commit_so"
((RC == 0)) || fail "up --stored-order-checker-commit exited $RC: $(cat "$tmp/out")"
run_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")
jq -e --arg d "$commit_old" --arg c "$commit_so" --arg sha "$(sha256sum <"$run_dir/envoy-dispatch" | cut -d' ' -f1)" \
  '.stored_order == {dispatch_commit: $d, built_from: $c, overlay_source: null, overlay_files: [], registration: null, renderer_matches: false, sha256: $sha}' \
  "$run_dir/state.json" >/dev/null || fail "state.json does not record the escape's checker with renderer_matches false: $(jq -c .stored_order "$run_dir/state.json")"
grep -qF "WARNING: --stored-order-checker-commit $commit_so: the stored-order checker is built wholly from $commit_so, not from dispatch commit $commit_old, so its renderer may differ from Dispatch's" "$tmp/out" ||
  fail "up --stored-order-checker-commit did not warn that the renderer may differ: $(cat "$tmp/out")"
grep -qx -- "-C $repo archive $commit_so packages/envoy" "$fake/git.log" || fail "up did not archive the checker commit's tree: $(cat "$fake/git.log")"
! grep -q -- "archive $commit_old" "$fake/git.log" || fail "up --stored-order-checker-commit archived the dispatch tree"
cmp -s "$fake/go-subcommands.go" "$fake/git-files/$commit_so/$so_registry" || fail "up --stored-order-checker-commit changed the checker commit's subcommand table"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down of the escape run exited $RC"
pass "up --stored-order-checker-commit builds the checker wholly from that commit, warns that its renderer may differ from Dispatch's, and records renderer_matches false"

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
  values+=(PG_VOLUME=100Gi "PG_PASSWORD=$(random_hex 24)" "DISPATCH_TOKEN=$(random_hex 32)" "LISTENER_TOKEN=$(random_hex 32)" "SIGNING_KEY=$(random_hex 32)")
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
  printf '%s\n' "$doc" 'stored-order: 11111111-1111-4111-8111-111111111111 version number=3 doc_update_version=7 stored_md5=0cc175b9c0f1b6a831c399e269772661 stored_length=1 fold_md5=92eb5ffee6ae2fec3ad71c777531578f fold_length=2' \
    'stored-order: 22222222-2222-4222-8222-222222222222 error (read updates: context canceled)' \
    'stored-order: documents=4 clean=1 mismatched=2 refused=0 failed=1' >"$dir/mismatch.txt"
  printf '%s\n' 'stored-order: open database: failed to connect' 'command terminated with exit code 1' >"$dir/missing.txt"
  printf '%s\n' "$doc" 'stored-order: documents=2 clean=2' >"$dir/truncated.txt"
  printf '%s\n' "$doc" 'stored-order: documents=1 clean=1 mismatched=0 refused=0 failed=0' 'stored-order: timed out after 1800 s (exit 124)' >"$dir/timeout.txt"
  printf '%s\n' 'stored-order: not run: x/meta.json holds no RFC 3339 window_start' >"$dir/notrun.txt"
  nulls='documents: null, clean: null, mismatched: null, refused: null, failed: null, unchecked: null'
  checker=$(jq -nc --arg d "$commit_old" --arg s "$commit_so" '{dispatch_commit: $d, built_from: $d, overlay_source: $s,
    overlay_files: ["a.go", "b.go"], registration: "c.go", renderer_matches: true, sha256: "feed"}')
  for c in "clean|0|$since|\$k + {exit: 0, since: \$s, documents: 1, clean: 1, mismatched: 0, refused: 0, failed: 0, unchecked: null}" \
    "mismatch|1|$since|\$k + {exit: 1, since: \$s, documents: 4, clean: 1, mismatched: 2, refused: 0, failed: 1, unchecked: null}" \
    "missing|1|$since|\$k + {exit: 1, since: \$s, $nulls}" \
    "truncated|1|$since|\$k + {exit: 1, since: \$s, $nulls}" \
    "timeout|124|$since|\$k + {exit: 124, since: \$s, $nulls}" \
    "notrun|null||\$k + {exit: null, since: null, $nulls}"; do
    IFS='|' read -r name rc s want <<<"$c"
    stored_order_summary "$dir/$name.txt" "$checker" "$rc" "$s" >"$dir/$name.json" || fail "stored_order_summary failed on the $name output"
    jq -e --argjson k "$checker" --arg s "$since" ". == $want" "$dir/$name.json" >/dev/null ||
      fail "stored-order.json of the $name output is wrong: $(cat "$dir/$name.json")"
  done
  # The step's checker: state.json's, renderer_matches false once Dispatch runs another commit.
  RUN_DIR=$dir/run STATE=$dir/run/state.json
  mkdir -p "$RUN_DIR"
  jq -n --arg d "$commit_old" --argjson k "$checker" '{dispatch_commit: $d, stored_order: $k, browsertype: null}' >"$STATE"
  [ "$(stored_order_checker)" = "$checker" ] || fail "stored_order_checker is not state.json's checker: $(stored_order_checker)"
  jq --arg d "$commit_ok" '.dispatch_commit = $d' "$STATE" >"$STATE.tmp" && mv "$STATE.tmp" "$STATE"
  jq -e '.renderer_matches == false' <<<"$(stored_order_checker)" >/dev/null ||
    fail "stored_order_checker after a redeploy to another commit does not say renderer_matches false: $(stored_order_checker)"
  # STORED-ORDER.md names the overlay, and the escape's renderer skew.
  stored_order_table || fail "stored_order_table failed"
  grep -qF "The checker was built for dispatch commit $commit_old from that commit's tree with INFRA-707's files of source commit $commit_so added (\`a.go\`, \`b.go\`; its stored-order entry registered in \`c.go\`). renderer_matches: true. Binary sha256 feed." "$RUN_DIR/STORED-ORDER.md" ||
    fail "STORED-ORDER.md does not name the overlay: $(cat "$RUN_DIR/STORED-ORDER.md")"
  jq --arg c "$commit_so" '.stored_order += {built_from: $c, overlay_source: null, overlay_files: [], registration: null, renderer_matches: false}' "$STATE" >"$STATE.tmp" &&
    mv "$STATE.tmp" "$STATE"
  stored_order_table || fail "stored_order_table failed"
  grep -qF "The checker was built for dispatch commit $commit_old wholly from checker commit $commit_so (\`--stored-order-checker-commit\`), whose renderer may differ from Dispatch's: a mismatch may be renderer skew. renderer_matches: false." "$RUN_DIR/STORED-ORDER.md" ||
    fail "STORED-ORDER.md does not name the escape's renderer skew: $(cat "$RUN_DIR/STORED-ORDER.md")"
)
pass "stored-order.json holds the checker's fields, a clean and a mismatching run's counts, and nulls, never 0, for a missing or truncated summary line, a timeout and a check not run; a redeploy makes renderer_matches false; STORED-ORDER.md names the overlay and the escape's renderer skew"

# ---- (k) the documents whose stored-order evidence is exported -----------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  dir=$tmp/stored-order-flagged
  mkdir -p "$dir"
  a=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa b=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb c=cccccccc-cccc-4ccc-8ccc-cccccccccccc
  d=dddddddd-dddd-4ddd-8ddd-dddddddddddd e=eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee
  counts='updates_checked=2 updates_skipped=0 versions_checked=2 versions_skipped=0'
  printf '%s\n' "stored-order: $a base=1 $counts mismatches=0" \
    "stored-order: $b base=1 $counts mismatches=3" \
    "stored-order: $b version number=4 doc_update_version=9 stored_md5=0cc175b9c0f1b6a831c399e269772661 stored_length=1 fold_renders=false" \
    "stored-order: $b pending update=9 parked_items=1 parked_delete_ranges=0" \
    "stored-order: $b version number=12 doc_update_version=20 stored_md5=0cc175b9c0f1b6a831c399e269772661 stored_length=1 fold_md5=92eb5ffee6ae2fec3ad71c777531578f fold_length=2" \
    "stored-order: $c base=0 updates_checked=0 updates_skipped=0 versions_checked=0 versions_skipped=0 mismatches=0 refused=\"a prune checkpoint stands at update 4: the history above it is being rewritten\"" \
    "stored-order: $d error (read the document's stored updates: context canceled)" \
    "stored-order: $e base=2 $counts mismatches=1 refused=\"update 5 does not apply to the fold before it: x\"" \
    "stored-order: $e class update=3 stored=true expected=false" \
    'stored-order: documents=5 clean=1 mismatched=1 refused=2 failed=1' >"$dir/out.txt"
  printf '%s\n' "$b 4 12" "$c" "$e" >"$dir/want"
  stored_order_flagged "$dir/out.txt" >"$dir/got" || fail "stored_order_flagged failed"
  diff -u "$dir/want" "$dir/got" >&2 || fail "stored_order_flagged does not name each mismatched or refused document once, with its mismatched versions"
  printf 'a,b\n1,"x\n""y"",\n"\n2,z\n' >"$dir/rows.csv"
  [ "$(csv_rows "$dir/rows.csv")" = 2 ] || fail "csv_rows counts $(csv_rows "$dir/rows.csv") records, not 2, past a quoted line break"
  printf 'a,b\n' >"$dir/header.csv"
  [ "$(csv_rows "$dir/header.csv")" = 0 ] || fail "csv_rows of a header alone is not 0"
)
pass "the stored-order evidence takes each mismatched or refused document once with its mismatched versions, never a clean or failed one, and counts CSV records past quoted line breaks"

# ---- (l) the databases of the evidence export -----------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  dir=$tmp/stored-order-databases
  mkdir -p "$dir/replay" "$dir/none"
  id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa since=2026-10-28T09:30:00.5Z
  printf 'stored-order: %s base=1 updates_checked=2 updates_skipped=0 versions_checked=2 versions_skipped=0 mismatches=1\n' "$id" >"$dir/out.txt"
  ends='{"artifacts.csv.gz": 1, "doc_checkpoints.csv.gz": 0, "artifact_versions.csv.gz": 2, "doc_updates.csv.gz": 3, "doc_snapshots.csv.gz": 1, "issue-events-in-window.csv.gz": 2}'
  # A replay's step: reset from dispatch_replay, so its start-* tables come from there.
  echo '{"template": "dispatch_replay"}' >"$dir/replay/reset.json"
  : >"$fake/kubectl.log"
  stored_order_evidence r "$dir/out.txt" "$dir/replay" "$since" >"$dir/replay.json" 2>"$dir/replay.err" || fail "stored_order_evidence of a replay step failed: $(cat "$dir/replay.err")"
  evidence_calls "$dir/replay-calls" || fail "a replay step's evidence export has a statement_timeout over its 600 s"
  for t in "doc_checkpoints where artifact_id = '$id'" "artifact_versions where artifact_id = '$id' order by number" \
    "doc_updates where artifact_id = '$id' order by version" "doc_snapshots where artifact_id = '$id' order by name"; do
    grep -qxF -- "$(psql_call dispatch_replay) \\copy (select * from $t) to stdout with csv header" "$dir/replay-calls" ||
      fail "a replay step's evidence export did not \\copy $t from its template dispatch_replay: $(cat "$dir/replay-calls")"
  done
  grep -qxF -- "$(psql_call dispatch) \\copy (select e.* from events e where (e.artifact_id = '$id' or e.issue_key = (select issue_key from artifacts where id = '$id')) and e.created_at >= '$since' order by e.seq) to stdout with csv header" "$dir/replay-calls" ||
    fail "a replay step's evidence export did not \\copy the issue events since its window start from database dispatch: $(cat "$dir/replay-calls")"
  [ "$(grep -cF -- ' -d dispatch_replay ' "$dir/replay-calls") $(grep -cF -- ' -d dispatch ' "$dir/replay-calls") $(wc -l <"$dir/replay-calls")" = "4 6 10" ] ||
    fail "a replay step's evidence export is not 4 calls on dispatch_replay and 6 on dispatch: $(cat "$dir/replay-calls")"
  jq -e --argjson ends "$ends" --arg a "$id" '. == [{artifact: $a, versions: [], files: ($ends + {"start-doc_checkpoints.csv.gz": 0,
    "start-artifact_versions.csv.gz": 0, "start-doc_updates.csv.gz": 1, "start-doc_snapshots.csv.gz": 0})}]' "$dir/replay.json" >/dev/null ||
    fail "a replay step's evidence is wrong: $(cat "$dir/replay.json")"
  # A step whose reset.json names no template: no start-* export, each named in error.
  : >"$fake/kubectl.log"
  stored_order_evidence n "$dir/out.txt" "$dir/none" "$since" >"$dir/none.json" 2>"$dir/none.err" || fail "stored_order_evidence without a template failed: $(cat "$dir/none.err")"
  ! grep -qF -- '-d dispatch_' "$fake/kubectl.log" || fail "the evidence export without a template read a template database: $(grep -F -- '-d dispatch_' "$fake/kubectl.log")"
  jq -e --argjson ends "$ends" --arg a "$id" --arg why "not exported: the step's reset.json names no template database" \
    '. == [{artifact: $a, versions: [], files: $ends,
      error: ["doc_checkpoints", "artifact_versions", "doc_updates", "doc_snapshots" | "start-\(.).csv.gz: \($why)"]}]' "$dir/none.json" >/dev/null ||
    fail "the evidence without a template does not name each start-* table as not exported: $(cat "$dir/none.json")"
)
pass "the evidence export reads the start-* tables from the template the step was reset from (dispatch_replay for a replay), the rest and the issue events since the window start from dispatch, and names each start-* table when reset.json names no template"

# ---- (m) browser typing's documents ---------------------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  dir=$tmp/typing-docs
  write_typing_bundle "$dir/bundle" "$dir/ids"
  # 0-5m: doc-1 (3 writes), then doc-2 and doc-3 (2 each, doc-2 first by logged id); doc-4 has no
  # replay_ids row, file-1 is a file, doc-5 is not in the state.
  [ "$(typing_docs_replay "$dir/bundle" 0 300 2 "$dir/ids" | tr '\n' ' ')" = "$typing_l1 $typing_l2 " ] ||
    fail "the replay's 2 typing documents in 0-5m are not doc-1 and doc-2's copy ids: $(typing_docs_replay "$dir/bundle" 0 300 2 "$dir/ids" | tr '\n' ' ')"
  [ "$(typing_docs_replay "$dir/bundle" 0 300 20 "$dir/ids" | tr '\n' ' ')" = "$typing_l1 $typing_l2 $typing_l3 " ] ||
    fail "the replay's typing documents in 0-5m are not doc-1, doc-2 and doc-3's copy ids, most writes first"
  [ "$(typing_docs_replay "$dir/bundle" 300 600 20 "$dir/ids")" = "$typing_l3" ] ||
    fail "the replay's typing documents in 5-10m are not doc-3's copy id alone: $(typing_docs_replay "$dir/bundle" 300 600 20 "$dir/ids")"
  [ -z "$(typing_docs_replay "$dir/bundle" 0 300 20 /dev/null)" ] || fail "a replay with no replay_ids rows still chose typing documents"
  # A load step: loadgen plan-docs' documents, most planned versions first, with their counts and,
  # under 10 planned versions for the Kth, one warning line.
  LOADGEN=$dir/loadgen RUN_DIR=$dir
  cat >"$LOADGEN" <<'PLAN'
#!/usr/bin/env bash
# A plan-docs that prints its flags on the first line of FAKE_DIR/plan-docs.args and the plan
# PLAN_DOCS_JSON holds, cut to -top.
printf '%s\n' "$*" >"$FAKE_DIR/plan-docs.args"
top=20 prev=""
for a in "$@"; do
  [ "$prev" != -top ] || top=$a
  prev=$a
done
jq -c --argjson top "$top" '.[:$top]' <<<"$PLAN_DOCS_JSON"
PLAN
  chmod +x "$LOADGEN"
  plan() { jq -nc --args '[$ARGS.positional[] | split(":") | {artifact: .[0], edits: (.[1] | tonumber), uploads: (.[2] | tonumber), versions: ((.[1] | tonumber) + (.[2] | tonumber))}]' "$@"; }
  export PLAN_DOCS_JSON
  PLAN_DOCS_JSON=$(plan d-a:30:30 d-b:20:20 d-c:8:1)
  TYPING_LIST=() TYPING_PLANNED=null
  typing_plan_step 10x-typing "$dir/seed.json" 400 2 40 30s 10m 1 2>"$dir/warn" || fail "typing_plan_step failed: $(cat "$dir/warn")"
  [ "${TYPING_LIST[*]}" = "d-a d-b" ] || fail "a load step's typing documents are not plan-docs' first 2: ${TYPING_LIST[*]}"
  [ "$TYPING_PLANNED" = '{"d-a":60,"d-b":40}' ] || fail "the planned versions of the typed documents are wrong: $TYPING_PLANNED"
  [ ! -s "$dir/warn" ] || fail "a step whose 2nd typed document has 40 planned versions warned: $(cat "$dir/warn")"
  [ "$(cat "$FAKE_DIR/plan-docs.args")" = "plan-docs -json -seed $dir/seed.json -sessions 400 -top 2 -events 40 -warmup 30s -duration 10m -rng-seed 1" ] ||
    fail "typing_plan_step did not run plan-docs with the step's own flags: $(cat "$FAKE_DIR/plan-docs.args")"
  # The Kth document under 10 planned versions, and fewer documents than K: one warning line each.
  typing_plan_step thin "$dir/seed.json" 400 3 40 30s 10m 1 2>"$dir/warn" || fail "typing_plan_step failed: $(cat "$dir/warn")"
  { [ "$(wc -l <"$dir/warn")" = 1 ] && grep -qF 'WARNING: step thin: typed document number 3 has 9 planned versions, under the 10 versions' "$dir/warn"; } ||
    fail "a step whose third typed document has 9 planned versions did not warn once: $(cat "$dir/warn")"
  [ "${TYPING_LIST[*]}" = "d-a d-b d-c" ] || fail "a warned step's typing documents are not plan-docs' 3: ${TYPING_LIST[*]}"
  typing_plan_step thin "$dir/seed.json" 400 4 40 30s 10m 1 2>"$dir/warn" || fail "typing_plan_step failed: $(cat "$dir/warn")"
  { grep -qF 'typed document number 4 has 0 planned versions' "$dir/warn" && grep -qF 'named 3 documents of --typing-docs 4' "$dir/warn"; } ||
    fail "a step with fewer planned documents than --typing-docs did not warn with 0: $(cat "$dir/warn")"
)
pass "a replay types into the documents with the most agent writes in its sub-window, mapped through replay_ids; a load step into the documents loadgen plan-docs says its own writes plan the most versions for, recording each one's planned count and warning when the Kth has fewer than 10"

# ---- (n) the retry of a read-only kubectl exec ----------------------------------------------------

(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  export FAKE_EXEC_ERRORS=$tmp/exec-errors FAKE_FAST_SLEEP=5
  dial='Error from server: error dialing backend: dial timeout, backstop'
  notfound='Error from server (NotFound): pods "postgres-0" not found'
  # calls_after ERRORS... CMD...: CMD after the exec errors ERRORS (up to --), with its output in
  # $tmp/retry.out and .err, its status in RC, and the kubectl calls and sleeps it made in CALLS
  # and SLEEPS.
  calls_after() {
    : >"$FAKE_EXEC_ERRORS"
    while [ "$1" != -- ]; do
      printf '%s\n' "$1" >>"$FAKE_EXEC_ERRORS"
      shift
    done
    shift
    : >"$fake/kubectl.log"
    rm -f "$fake/sleep.log"
    local mark
    mark=$(own_calls_mark)
    RC=0
    "$@" >"$tmp/retry.out" 2>"$tmp/retry.err" || RC=$?
    CALLS=$(own_calls_since "$mark" | wc -l)
    SLEEPS=""
    [ ! -f "$fake/sleep.log" ] || SLEEPS=$(tr '\n' ' ' <"$fake/sleep.log")
  }
  calls_after "$dial" -- psql_at postgres 'select version()'
  { ((RC == 0)) && [ "$(cat "$tmp/retry.out")" = 'PostgreSQL 16.4 (fake)' ] && ((CALLS == 2)) && [ "$SLEEPS" = "5 " ]; } ||
    fail "a psql read did not retry once 5 s after a dial timeout: rc $RC, $CALLS calls, sleeps '$SLEEPS', output $(cat "$tmp/retry.out")"
  grep -qF "retrying once in 5 s" "$tmp/retry.err" || fail "the retry was not said: $(cat "$tmp/retry.err")"
  for e in 'The connection to the server 10.0.0.1:443 was refused - did you specify the right host or port?' \
    'error: read tcp 10.0.0.2:41000->10.0.0.1:443: read: connection reset by peer' \
    'Error from server: error dialing backend: dial tcp 10.0.0.3:10250: connect: connection refused'; do
    calls_after "$e" -- runner_has "$fake/token" /work/none -f
    { ((CALLS == 2)) && [ "$SLEEPS" = "5 " ]; } || fail "a runner read was not retried after '$e': $CALLS calls, sleeps '$SLEEPS'"
  done
  calls_after "$dial" "$dial" -- psql_at postgres 'select version()'
  { ((RC != 0)) && ((CALLS == 2)) && [ "$SLEEPS" = "5 " ] && grep -qxF "$dial" "$tmp/retry.err"; } ||
    fail "two dial timeouts in a row were not one retry, then the failure: rc $RC, $CALLS calls, sleeps '$SLEEPS'"
  calls_after "$notfound" -- psql_at postgres 'select version()'
  { ((RC != 0)) && ((CALLS == 1)) && [ -z "$SLEEPS" ] && grep -qxF "$notfound" "$tmp/retry.err"; } ||
    fail "a read was retried after an error that is not transient: rc $RC, $CALLS calls, sleeps '$SLEEPS'"
  # The write itself is one call; the lock read before it is postgres_exec's guard.
  calls_after "$dial" -- psql_write postgres 'drop database if exists dispatch with (force)'
  psql_calls=$(grep -c -- 'exec postgres-0 -c postgres -- psql' "$fake/kubectl.log" || true)
  { ((RC != 0)) && [ "$psql_calls" = 1 ] && [ -z "$SLEEPS" ]; } ||
    fail "a psql write was retried after a dial timeout: rc $RC, $psql_calls psql calls of $CALLS"
  NATS_BOX_POD='nats-box-7f6d'
  calls_after "$dial" -- nats_box stream ls --names
  { ((RC == 0)) && ((CALLS == 2)) && [ "$(cat "$tmp/retry.out")" = ENVOY_NOTIFICATIONS ]; } || fail "a nats-box listing was not retried after a dial timeout"
  calls_after "$dial" -- nats_box kv del interests -f
  { ((RC != 0)) && ((CALLS == 1)) && [ -z "$SLEEPS" ]; } || fail "a nats-box removal was retried after a dial timeout"
)
pass "a read-only kubectl exec is retried once 5 s after a dial timeout or a refused or reset API server connection, and never after another error; a write is never retried"

# (o) The heartbeat renews the lock only while a command of its run is alive. The daemons run in a
# session of their own, so a run whose command dies without its EXIT trap (a killed process, a
# worker turn the daemon interrupts, an OOM) leaves the heartbeat patching the lock every 60 s for
# ever: the lock never ages, up never calls it stale and never names the down command, and the
# full-size copy runs on with nothing driving it.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-orphan FAKE_FAST_SLEEP=60
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
orphan_dir=$(find "$LOADTEST_CLUSTER_ROOT" -mindepth 1 -maxdepth 1 -type d)
orphan_hb=$(<"$orphan_dir/heartbeat.pid")
# up has returned, so no cluster.sh command of this run is alive any more.
for ((i = 0; i < 50; i++)); do
  kill -0 "$orphan_hb" 2>/dev/null || break
  sleep 0.1
done
orphan_alive=no
kill -0 "$orphan_hb" 2>/dev/null && orphan_alive=yes
orphan_patches=$(grep -c "patch configmap dispatch-loadtest-lock" "$fake/kubectl.log" || true)
sleep 1
orphan_patches_after=$(grep -c "patch configmap dispatch-loadtest-lock" "$fake/kubectl.log" || true)
run_cluster down --namespace "$ns"
[ "$orphan_alive" = no ] ||
  fail "the heartbeat daemon (pid $orphan_hb) still runs 5 s after the last command of its run ended: it renews the lock for ever, so the lock never goes stale and the copy is never reclaimed"
[ "$orphan_patches_after" = "$orphan_patches" ] ||
  fail "the lock was patched $((orphan_patches_after - orphan_patches)) more times after the last command of the run ended: an abandoned run keeps its lock fresh"
unset FAKE_FAST_SLEEP
pass "the heartbeat stops once no command of its run is alive, so an abandoned run's lock goes stale"

# (p) stored_order_summary reads the summary line of INFRA-707's checker at eb0fe0be and later,
# which ends it with ` unchecked=N` (a document the run compared nothing of) and exits 1. The
# regex anchored at `failed=N$` matches no such line, so every count of stored-order.json and of
# STORED-ORDER.md reads null, as if the checker had never printed a summary.
(
  so_checker=$(jq -nc --arg c "$commit_ok" '{dispatch_commit: $c, built_from: $c, overlay_source: null,
    overlay_files: [], registration: null, renderer_matches: true, sha256: "f00d"}')
  source "$cluster"
  printf 'stored-order: documents=5 clean=2 mismatched=1 refused=0 failed=0 unchecked=2\n' >"$tmp/so-unchecked.txt"
  stored_order_summary "$tmp/so-unchecked.txt" "$so_checker" 1 2026-10-29T00:00:00Z >"$tmp/so-unchecked.json"
  jq -e '.documents == 5 and .clean == 2 and .mismatched == 1 and .refused == 0 and .failed == 0 and .unchecked == 2' \
    "$tmp/so-unchecked.json" >/dev/null ||
    fail "stored-order.json of a summary line ending ' unchecked=2' is $(jq -c '{documents, clean, mismatched, refused, failed, unchecked}' "$tmp/so-unchecked.json"): the checker's counts are lost"
  printf 'stored-order: documents=3 clean=3 mismatched=0 refused=0 failed=0\n' >"$tmp/so-plain.txt"
  stored_order_summary "$tmp/so-plain.txt" "$so_checker" 0 2026-10-29T00:00:00Z >"$tmp/so-plain.json"
  jq -e '.documents == 3 and .clean == 3 and (.unchecked == null)' "$tmp/so-plain.json" >/dev/null ||
    fail "stored-order.json of a summary line without unchecked is $(jq -c '{documents, clean, unchecked}' "$tmp/so-plain.json")"
)
pass "stored_order_summary reads a summary line that ends with unchecked=N, and one without it"

# (q) A copy-back of a step's files from the runner that failed. The step's files are then missing
# here, so the step is a failed one (step-failed.json kind copy_back), not a complete step whose
# directory holds no summary.json; the cut stream leaves no truncated file behind, since a
# truncated summary.json fails loadgen curves and tables for the whole run; and the files stay in
# the runner, so nothing of the step is removed there.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-copyback
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
run=$(jq -r .data.run "$fake/lock.json")
run_dir=$LOADTEST_CLUSTER_ROOT/$run
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC"
FAKE_COPY_BACK_FAIL=1 run_cluster step --namespace "$ns" cut 1 --duration 1m
unset FAKE_COPY_BACK_FAIL
((RC == 0)) || fail "a step whose copy-back failed exited $RC"
cut_dir=$run_dir/steps/cut
jq -e '[.reasons[] | select(.kind == "copy_back")] | length == 1' "$cut_dir/step-failed.json" >/dev/null 2>&1 ||
  fail "a step whose copy-back failed holds no step-failed.json reason of kind copy_back: $(cat "$cut_dir/step-failed.json" 2>/dev/null)"
jq -e '[.reasons[] | select(.kind == "copy_back")][0].detail | test("/work/steps/cut")' "$cut_dir/step-failed.json" >/dev/null ||
  fail "the copy_back reason does not say where the step's files stayed: $(cat "$cut_dir/step-failed.json")"
for f in summary.json meta.json; do
  [ ! -e "$cut_dir/$f" ] ||
    fail "the cut stream left $f ($(stat -c %s "$cut_dir/$f") bytes) in the step directory: a truncated file fails loadgen curves and tables for the whole run"
done
[ -z "$(find "$cut_dir" -maxdepth 1 -name '.copy-back.*')" ] || fail "the copy-back left its scratch directory in $cut_dir"
grep -qF 'step cut failed: copy_back' "$tmp/out" || fail "the step did not say that it failed because the copy-back did"
! grep -q -- "exec runner -c runner -- rm -rf /work/steps/cut " "$fake/kubectl.log" ||
  fail "the step directory was removed from the runner although the copy back failed"
run_cluster down --namespace "$ns"
pass "a step whose copy-back from the runner failed is marked failed with kind copy_back, keeps no truncated file of the cut stream, and leaves its files in the runner"

# (r) The sampler ends with its run's heartbeat. Its eight role loops, each holding a kubectl exec
# of a copy container, outlived every command of the run and the heartbeat alike: a killed run
# kept eight execs open for ever, and a run torn down from elsewhere appended an error line of
# each to cgroup.jsonl every 5 s with nothing left to read.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-sampler-stop FAKE_FAST_SLEEP=60
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
sampler_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")
sampler_pid=$(<"$sampler_dir/sampler.pid")
# up has returned, so no command of this run is alive: its heartbeat stops, and the sampler with it.
sampler_alive=yes
for ((i = 0; i < 200; i++)); do
  kill -0 "$sampler_pid" 2>/dev/null || {
    sampler_alive=no
    break
  }
  sleep 0.1
done
sampler_lines=$(wc -l <"$sampler_dir/cgroup.jsonl" 2>/dev/null || echo 0)
sleep 6
sampler_lines_after=$(wc -l <"$sampler_dir/cgroup.jsonl" 2>/dev/null || echo 0)
run_cluster down --namespace "$ns"
[ "$sampler_alive" = no ] ||
  fail "the sampler daemon (pid $sampler_pid) still runs 20 s after the last command of its run ended and its heartbeat stopped: its eight kubectl execs stay open and it appends an error line of each to cgroup.jsonl every 5 s for ever"
[ ! -f "$sampler_dir/sampler.pid" ] || fail "the sampler that stopped left its pidfile $sampler_dir/sampler.pid"
[ "$sampler_lines_after" = "$sampler_lines" ] ||
  fail "cgroup.jsonl grew by $((sampler_lines_after - sampler_lines)) lines in the 6 s after the sampler was gone: a role loop of it still samples"
unset FAKE_FAST_SLEEP
pass "the sampler stops once no heartbeat of its run is alive, with the kubectl exec of every role loop, and removes its pidfile"

# (s) The hold. all --keep leaves the copy up for the typing steps and replays its caller types by
# hand, but the heartbeat stops 60 s after the last command, so 15 min later any worker's up calls
# that lock stale and prints the down that deletes a copy an hour of seeding made. --keep-for, and
# cluster.sh hold, hold the run: the heartbeat renews the lock while the hold lasts, and only an
# expired hold lets the run age as an abandoned one.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-hold
run_cluster all "${up_args[@]}" --dispatch-commit "$commit_ok" --profile quick --keep-for 1h
((RC != 0)) || fail "all --keep-for without --keep was not refused"
grep -q -- '--keep-for is valid only with --keep' "$tmp/out" ||
  fail "all --keep-for without --keep did not say that the flag needs --keep: $(cat "$tmp/out")"
no_create || fail "all --keep-for without --keep created something before it refused"
export FAKE_FAST_SLEEP=60
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
hold_run=$(jq -r .data.run "$fake/lock.json")
hold_dir=$LOADTEST_CLUSTER_ROOT/$hold_run
# up has returned, so no command of the run is alive and its heartbeat stops, as (o) proves.
for ((i = 0; i < 100; i++)); do
  [ -f "$hold_dir/heartbeat.pid" ] || break
  sleep 0.1
done
run_cluster hold --namespace "$ns" --for 1h
((RC == 0)) || fail "cluster.sh hold exited $RC: $(cat "$tmp/out")"
[ -f "$hold_dir/hold-until" ] || fail "hold wrote no hold-until in $hold_dir: $(cat "$tmp/out")"
hold_hb=$(<"$hold_dir/heartbeat.pid")
hold_patches=$(grep -c "patch configmap dispatch-loadtest-lock" "$fake/kubectl.log" || true)
# Wait on events, not on time: the fake sleep makes each 60 s cycle return at once, so a held
# heartbeat patches again and again; three more patches after hold returned, with no command of
# the run alive, are three cycles the hold kept going. The 300 s is a hang's backstop only.
hold_patches_after=$hold_patches
for ((i = 0; i < 3000; i++)); do
  hold_patches_after=$(grep -c "patch configmap dispatch-loadtest-lock" "$fake/kubectl.log" || true)
  ((hold_patches_after < hold_patches + 3)) || break
  kill -0 "$hold_hb" 2>/dev/null || break
  sleep 0.1
done
hold_alive=no
kill -0 "$hold_hb" 2>/dev/null && hold_alive=yes
[ "$hold_alive" = yes ] ||
  fail "the heartbeat of a held run stopped although no command of it runs: 15 min later another worker's up calls the kept copy stale and prints the down that deletes it"
((hold_patches_after >= hold_patches + 3)) ||
  fail "the heartbeat of a held run patched the lock no more than before ($hold_patches_after, was $hold_patches): the kept run's lock ages and goes stale"
run_cluster status --namespace "$ns"
grep -q 'hold: until ' "$tmp/out" || fail "status does not show the hold's expiry: $(cat "$tmp/out")"
# An expired hold holds nothing, so an abandoned kept run goes stale as any other.
printf '%s\n' "$((EPOCHSECONDS - 1))" >"$hold_dir/hold-until"
hold_expired_alive=yes
for ((i = 0; i < 200; i++)); do
  kill -0 "$hold_hb" 2>/dev/null || {
    hold_expired_alive=no
    break
  }
  sleep 0.1
done
run_cluster down --namespace "$ns"
[ "$hold_expired_alive" = no ] ||
  fail "the heartbeat went on renewing the lock 20 s after the hold expired: a kept run nobody extends never goes stale"
[ ! -f "$hold_dir/hold-until" ] || fail "down left the run's hold in $hold_dir"
unset FAKE_FAST_SLEEP
pass "all refuses --keep-for without --keep; a hold keeps the heartbeat renewing the lock with no command of the run alive, status shows its expiry, down removes it, and an expired hold stops the heartbeat"

# (t) The refusal of a stale lock. The heartbeat's age alone is no proof that the run is
# abandoned: a run held for typing steps may have outlived its hold, and the down command deletes
# a copy that hours of seeding and steps made. So the refusal names the lock's holder and issue
# and says to confirm with that issue's holder, before it gives the command.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-stale
write_lock "$other_run" "$(date -u -d '-20 minutes' +%Y-%m-%dT%H:%M:%SZ)"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 2)) || fail "up against a stale lock exited $RC, not 2"
stale_confirm=$(grep -n -i 'confirm' "$tmp/out" | head -n1 | cut -d: -f1 || true)
[ -n "$stale_confirm" ] ||
  fail "up's stale refusal does not say to confirm with the holder that the run is abandoned before clearing it: $(cat "$tmp/out")"
grep -qi 'confirm with issue OTHER-9' "$tmp/out" ||
  fail "the confirm instruction does not name the holder's issue OTHER-9: $(cat "$tmp/out")"
grep -q 'holder is stale: pod other-worker-3, issue OTHER-9' "$tmp/out" ||
  fail "the stale line does not name the holder pod and issue the lock holds: $(cat "$tmp/out")"
stale_down=$(grep -n -- "cluster.sh down --namespace $ns --run $other_run" "$tmp/out" | head -n1 | cut -d: -f1 || true)
[ -n "$stale_down" ] || fail "up's stale refusal no longer gives the down command: $(cat "$tmp/out")"
((stale_confirm < stale_down)) ||
  fail "up gives the down command (line $stale_down) before it says to confirm with the holder (line $stale_confirm)"
rm -f "$fake/lock.json"
pass "up's refusal of a stale lock names the holder pod and issue and says to confirm with that issue's holder that the run is abandoned, before the down command that clears it"

# (u) One PID namespace drives a run. A Legion pod runs each role in a PID namespace of its own
# over one LOADTEST_CLUSTER_ROOT, so the pids another role recorded (the run's command files, the
# daemons' pidfiles) name nothing here: a command from this one would register itself as alive for
# a heartbeat it cannot see, and ensure_daemons would start a second pair of daemons on the same
# run. Only status and down, which register no command, run from another namespace.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-pidns
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC"
pidns_run=$(jq -r .data.run "$fake/lock.json")
pidns_dir=$LOADTEST_CLUSTER_ROOT/$pidns_run
[ "$(jq -r '.pid_ns // ""' "$pidns_dir/state.json")" = "$(readlink /proc/self/ns/pid)" ] ||
  fail "up did not record its PID namespace in state.json: pid_ns is '$(jq -r '.pid_ns // ""' "$pidns_dir/state.json")', not $(readlink /proc/self/ns/pid)"
# The run as another role of the pod finds it: started in a PID namespace that is not this one.
jq '.pid_ns = "pid:[1]"' "$pidns_dir/state.json" >"$tmp/pidns-state.json"
mv "$tmp/pidns-state.json" "$pidns_dir/state.json"
chmod 600 "$pidns_dir/state.json"
for d in heartbeat sampler; do
  pidns_pid=$(<"$pidns_dir/$d.pid")
  kill -TERM -- "-$pidns_pid" 2>/dev/null || true
  rm -f "$pidns_dir/$d.pid"
done
rm -rf "$pidns_dir/commands"
run_cluster step --namespace "$ns" foreign 1 --duration 1m
((RC != 0)) || fail "a step of a run that another PID namespace started was not refused"
grep -qF 'pid:[1]' "$tmp/out" ||
  fail "the refusal does not name the PID namespace the run recorded: $(cat "$tmp/out")"
grep -qF 'only the role that ran up' "$tmp/out" ||
  fail "the refusal does not say that only the role that ran up can drive the run: $(cat "$tmp/out")"
grep -qF 'test-worker-0' "$tmp/out" || fail "the refusal does not name the holder pod: $(cat "$tmp/out")"
grep -qF 'TEST-1' "$tmp/out" || fail "the refusal does not name the holder's issue: $(cat "$tmp/out")"
[ ! -e "$pidns_dir/commands" ] ||
  fail "a command refused for its PID namespace registered itself in $pidns_dir/commands: $(ls "$pidns_dir/commands")"
[ ! -e "$pidns_dir/steps/foreign" ] || fail "the refused step made the step directory $pidns_dir/steps/foreign"
for d in heartbeat sampler; do
  [ ! -f "$pidns_dir/$d.pid" ] || fail "the refused command started the $d daemon of a run it cannot drive"
done
run_cluster status --namespace "$ns"
((RC == 0)) || fail "status of a run that another PID namespace started exited $RC: $(cat "$tmp/out")"
grep -q "run $pidns_run" "$tmp/out" || fail "status does not name the run: $(cat "$tmp/out")"
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down of a run that another PID namespace started exited $RC: $(cat "$tmp/out")"
[ ! -f "$fake/lock.json" ] || fail "down of a run that another PID namespace started left the lock"
grep -q 'PID namespace pid:\[1\]' "$tmp/out" ||
  fail "down does not say that it cannot stop the daemons of a run another PID namespace started: $(cat "$tmp/out")"
pass "a command of a run another PID namespace started is refused before it registers anything or starts a daemon, while status still reads the run and down still deletes it"

# (v) The Postgres data volume. On a 20Gi volume, a live replay with browser typing on the
# reference seed filled it mid-run: Postgres crash-looped with `FATAL: could not write to file
# "pg_wal/xlogtemp.49": No space left on device`, and the step's stored-order check could not even
# connect, so the step and every later one of that run measured nothing. up now sizes the claim
# (--pg-volume, refused above what the namespace's quota leaves beside NATS's three 20Gi claims),
# and every step reads the volume before its load and every 30 s while the load runs: too little
# free space is a failed step (kind pg_disk) that sends no load, or whose load the watcher stops,
# never a Postgres that fills its volume.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-pgdisk
# A 150Gi volume, as the fake df reports it: a tenth used, a fifth used, and 10% free.
readonly pg_size=161061273600 pg_used_low=16106127360 pg_used_mid=32212254720 pg_used_high=144955146240
readonly pg_free_low=144955146240 pg_free_mid=128849018880 pg_free_high=16106127360
# df_seq LINE...: the readings the fake kubectl answers with, in order, the last repeated.
df_seq() {
  printf '%s\n' "$@" >"$fake/df-seq"
  rm -f -- "$fake/rc-polls" "$fake/runner-stops"
}

run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --pg-volume 300Gi
((RC != 0)) || fail "up accepted --pg-volume 300Gi, which the namespace's quota cannot hold beside NATS"
grep -q -- '--pg-volume 300Gi is more than the 240Gi' "$tmp/out" ||
  fail "up's refusal of --pg-volume 300Gi does not name what the quota leaves: $(cat "$tmp/out")"
no_create || fail "up created something before refusing --pg-volume 300Gi"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --pg-volume 150
{ ((RC != 0)) && grep -q -- "--pg-volume '150' is not a whole number of Gi" "$tmp/out"; } ||
  fail "up accepted a --pg-volume that is not a whole number of Gi: $(cat "$tmp/out")"
no_create || fail "up created something before refusing a --pg-volume that is not Gi"
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --pg-volume 150Gi
((RC == 0)) || fail "up --pg-volume 150Gi exited $RC: $(cat "$tmp/out")"
pg_run=$(jq -r .data.run "$fake/lock.json")
pg_dir=$LOADTEST_CLUSTER_ROOT/$pg_run
[ "$(grep -c '^ *storage: 150Gi$' "$fake/applied.yaml")" = 1 ] ||
  fail "the Postgres claim is not the --pg-volume 150Gi it was given: $(grep -n 'storage:' "$fake/applied.yaml")"
[ "$(jq -r .postgres.volume "$pg_dir/state.json")" = 150Gi ] ||
  fail "state.json does not record the data volume: $(jq -c .postgres "$pg_dir/state.json")"
grep -q 'data volume (claim data-postgres-0 on premium-rwo): 150Gi' "$tmp/out" ||
  fail "up did not print the Postgres data volume with the other sizes: $(cat "$tmp/out")"
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC: $(cat "$tmp/out")"

# A step that starts with less than 15% of the volume free sends no load at all.
df_seq "$pg_size $pg_used_high $pg_free_high"
FAKE_PG_DF_SEQ=$fake/df-seq run_cluster step --namespace "$ns" pgfull 1 --duration 1m
((RC != 0)) || fail "a step that started under the free-space threshold exited 0"
! grep -q '^step-pgfull ' "$fake/runner-starts" ||
  fail "a step that started under the free-space threshold still sent load: $(grep '^step-pgfull ' "$fake/runner-starts")"
jq -e '[.reasons[] | select(.kind == "pg_disk")] | length == 1' "$pg_dir/steps/pgfull/step-failed.json" >/dev/null 2>&1 ||
  fail "a step refused for free space holds no step-failed.json reason of kind pg_disk: $(cat "$pg_dir/steps/pgfull/step-failed.json" 2>/dev/null)"
for want in '/var/lib/postgresql/copy is 150.0 GiB' '135.0 GiB used' '15.0 GiB free (10% of it)' 'under the 15% (22.5 GiB)' '--pg-volume'; do
  grep -qF -- "$want" "$tmp/out" || fail "the refusal does not name '$want': $(cat "$tmp/out")"
done
jq -e --argjson u "$pg_used_high" '.samples | length == 1 and (.[0].used_bytes == $u)' "$pg_dir/steps/pgfull/pg-disk.json" >/dev/null ||
  fail "a refused step's pg-disk.json does not hold its one reading: $(cat "$pg_dir/steps/pgfull/pg-disk.json" 2>/dev/null)"

# A step whose free space falls under the threshold mid-run: the watcher stops its runner job and
# marks the step failed, and pg-disk.json keeps the peak it reached.
df_seq "$pg_size $pg_used_low $pg_free_low" "$pg_size $pg_used_high $pg_free_high"
FAKE_PG_DF_SEQ=$fake/df-seq FAKE_RC_UNTIL_STOP=1 FAKE_FAST_SLEEP="15 30" \
  run_cluster step --namespace "$ns" pgmid 1 --duration 1m
((RC == 0)) || fail "a step whose volume filled mid-run exited $RC: $(cat "$tmp/out")"
grep -q '^step-pgmid ' "$fake/runner-starts" || fail "the step sent no load, so it tests no watcher"
jq -e '[.reasons[] | select(.kind == "pg_disk")] | length == 1' "$pg_dir/steps/pgmid/step-failed.json" >/dev/null 2>&1 ||
  fail "a step whose volume filled mid-run holds no step-failed.json reason of kind pg_disk: $(cat "$pg_dir/steps/pgmid/step-failed.json" 2>/dev/null)"
grep -qx 'step-pgmid' "$fake/runner-stops" ||
  fail "the disk watcher did not stop the step's runner job: $(cat "$fake/runner-stops" 2>/dev/null)"
jq -e --argjson peak "$pg_used_high" --argjson before "$pg_used_low" --argjson free "$pg_free_high" '
  .used_peak == $peak and .used_before == $before and .free_min == $free and .threshold_pct == 15' \
  "$pg_dir/steps/pgmid/pg-disk.json" >/dev/null ||
  fail "pg-disk.json does not hold the peak the step reached: $(cat "$pg_dir/steps/pgmid/pg-disk.json" 2>/dev/null)"
grep -q 'cgroup-merge .*/steps/pgmid ' "$fake/loadgen.log" ||
  fail "the step's after-work did not run after the watcher stopped its load"

# A step that keeps its room: no failure, and pg-disk.json holds the readings around the load.
df_seq "$pg_size $pg_used_low $pg_free_low" "$pg_size $pg_used_mid $pg_free_mid"
FAKE_PG_DF_SEQ=$fake/df-seq run_cluster step --namespace "$ns" pgok 1 --duration 1m
((RC == 0)) || fail "a step with room on the volume exited $RC: $(cat "$tmp/out")"
[ ! -f "$pg_dir/steps/pgok/step-failed.json" ] ||
  fail "a step with room on the volume was marked failed: $(cat "$pg_dir/steps/pgok/step-failed.json")"
jq -e --argjson size "$pg_size" --argjson before "$pg_used_low" --argjson after "$pg_used_mid" --argjson free "$pg_free_mid" '
  .size_bytes == $size and .used_before == $before and .used_after == $after and .used_peak == $after
  and .free_min == $free and .threshold_pct == 15 and (.samples | length) == 2
  and (.samples | map(has("at")) | all)' "$pg_dir/steps/pgok/pg-disk.json" >/dev/null ||
  fail "a normal step's pg-disk.json is wrong: $(cat "$pg_dir/steps/pgok/pg-disk.json" 2>/dev/null)"
[ "$(jq -r .postgres.volume "$pg_dir/steps/pgok/copy-meta.json")" = 150Gi ] ||
  fail "copy-meta.json does not record the data volume: $(jq -c .postgres "$pg_dir/steps/pgok/copy-meta.json")"
! grep -qx 'step-pgok' "$fake/runner-stops" 2>/dev/null ||
  fail "a step with room on the volume had its runner job stopped: $(cat "$fake/runner-stops")"
[ -z "$(find "$pg_dir/steps" -name '.pg-disk.jsonl')" ] || fail "a step left its readings file beside pg-disk.json"
unset FAKE_FAST_SLEEP
run_cluster down --namespace "$ns"
pass "up sizes the Postgres claim with --pg-volume and refuses one the quota cannot hold; a step under the free-space threshold sends no load, one that falls under it mid-run has its load stopped, both failed with kind pg_disk, and each step writes pg-disk.json"

# (w) The liveness of what a step leaves on this pod. A step killed outright (a SIGKILL, an OOM, a
# worker turn cut short) runs neither pg_disk_watch_stop nor its EXIT trap, so its disk watcher
# read the volume every 30 s for ever and its wait on the runner job polled that job's .rc file on
# to its deadline. Step names, job names and pods repeat across runs, so the watcher would later
# record pg_disk in a dead step's directory and stop a new run's job of the same name, and the
# wait would TERM a new run's job of that name at the dead step's deadline.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-watchkill
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --pg-volume 150Gi
((RC == 0)) || fail "up exited $RC: $(cat "$tmp/out")"
kill_run=$(jq -r .data.run "$fake/lock.json")
kill_dir=$LOADTEST_CLUSTER_ROOT/$kill_run/steps/pgkill
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC: $(cat "$tmp/out")"
# The step's load never ends (FAKE_RC_UNTIL_STOP) and the volume has room, so the watcher polls
# on. Only its own 30 s poll is a fast sleep, which it records in sleep.log: the step's shell sits
# in runner_wait's real 15 s sleep when the kill arrives, as a step does while its load runs.
df_seq "$pg_size $pg_used_low $pg_free_low"
: >"$fake/sleep.log"
FAKE_PG_DF_SEQ=$fake/df-seq FAKE_RC_UNTIL_STOP=1 FAKE_FAST_SLEEP=30 \
  bash "$cluster" step --namespace "$ns" pgkill 1 --duration 1m >"$tmp/out" 2>&1 &
kill_step=$!
# The waits in this case are the step's own doing, never a stretch of wall clock: this one ends
# at the watcher's first nap, which the fake sleep records, and at once if the step ends first.
watching=no
while kill -0 "$kill_step" 2>/dev/null; do
  if grep -qx 30 "$fake/sleep.log"; then
    watching=yes
    break
  fi
  sleep 0.05
done
[ "$watching" = yes ] ||
  fail "the step ended before its disk watcher took its first 30 s nap, so it read the volume through no watcher: $(cat "$tmp/out")"
# The two subshells the SIGKILL leaves behind: the disk watcher, and the wait on the runner job
# in its command substitution. Each ends at its own next poll, and the assertions below wait for
# exactly these pids, so no clock decides how long a loaded box may take to reach that poll.
mapfile -t kill_kids < <(pgrep -P "$kill_step")
((${#kill_kids[@]} >= 2)) ||
  fail "the step shell has ${#kill_kids[@]} children, not its disk watcher and its wait on the runner job: $(cat "$tmp/out")"
kill -KILL "$kill_step"
# The zombie reaped, so the pid the watcher and the wait hold names no process any more. A watcher
# that is gone takes no 30 s nap and reads the volume no more; the step's runner_wait, in a
# command substitution the kill leaves behind too, sleeps a real 15 s and reads no volume, so the
# fake kubectl's df is the watcher's read alone, and it ends at the poll after that sleep. The
# wait here is for the watcher's own poll after the kill, which it says it stops at, and it gives
# up as soon as both subshells are gone; it is no assertion, since a watcher that polls on fails
# the counts below.
wait "$kill_step" 2>/dev/null || true
until grep -q 'disk watcher' "$tmp/out" || all_gone "${kill_kids[@]}"; do
  sleep 0.05
done
# From here the volume is under the threshold: a watcher still polling records the dead step
# failed and stops the job step-pgkill, which a later run of a step of that name would be.
df_seq "$pg_size $pg_used_high $pg_free_high"
: >"$fake/sleep.log"
kill_dfs=$(grep -c -- 'df -B1 --output' "$fake/kubectl.log" || true)
# The step's wait on its runner job is a command substitution, a subshell of its own that the
# SIGKILL does not reach either. Left alone it polls /work/logs/step-pgkill.rc on and, at the
# step's deadline, TERMs the pid that job's .pid file names in whatever pod is named runner --
# which a later run's step of that name would be. Given the step shell's pid and start time it
# ends at its next poll instead, as the watcher does, so the kill leaves nothing of the step
# running here and no job is stopped. Nothing but cluster.sh ends them: the cleanup's pkill runs
# long after this.
await_gone "${kill_kids[@]}" ||
  fail "the step's disk watcher and its wait on the runner job ($(pgrep -af "$cluster step --namespace $ns " | tr '\n' ';')) still run after the step shell was killed: they poll on, so the watcher records pg_disk in a dead step's directory and the wait stops the job step-pgkill at the step's deadline, which a later run's step of that name would be"
kill_dfs_after=$(grep -c -- 'df -B1 --output' "$fake/kubectl.log" || true)
! grep -qx 'step-pgkill' "$fake/runner-stops" 2>/dev/null ||
  fail "the disk watcher of a killed step stopped the runner job step-pgkill after the kill: $(cat "$fake/runner-stops")"
[ ! -f "$kill_dir/step-failed.json" ] ||
  fail "the disk watcher of a killed step recorded it failed: $(cat "$kill_dir/step-failed.json")"
[ "$kill_dfs_after" = "$kill_dfs" ] ||
  fail "the killed step's disk watcher made $((kill_dfs_after - kill_dfs)) more reads of the volume before it ended: it reads on under the threshold"
! grep -qx 30 "$fake/sleep.log" ||
  fail "the killed step's disk watcher took $(grep -cx 30 "$fake/sleep.log") more 30 s naps after it was reaped: it polls on for ever"
! pgrep -f "$cluster step --namespace $ns " >/dev/null 2>&1 ||
  fail "the step still runs here although its disk watcher and its wait on the runner job ended: $(pgrep -af "$cluster step --namespace $ns " | tr '\n' ';')"
! grep -qx 'step-pgkill' "$fake/runner-stops" 2>/dev/null ||
  fail "the killed step's wait on its runner job stopped step-pgkill: $(cat "$fake/runner-stops")"
unset FAKE_FAST_SLEEP
run_cluster down --namespace "$ns"
# The same rule with a bound small enough to reach, the step's own being its window, grace and
# drain away: a wait whose step shell is gone ends at its first poll, printing nothing its caller
# would read as an exit status; a wait at its deadline stops nothing once the lock names another
# run, and neither does the step's own way out, which that wait starts (the step shell reads the
# wait's output under errexit, so it ends there and runs its EXIT trap: step_at_exit,
# typing_stop_at_exit, typing_stop, which would TERM typing-<name> and remove the agent token in
# the pod named runner -- another run's by then); and a wait with no pid and start time (seed,
# seed-refs, seed-finish and typing_stop) is what it was, stopping its job at the deadline while
# the lock is its own run's.
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run "$kill_run"
  mkdir -p "$RUN_DIR/logs" "$kill_dir"
  rm -f -- "$fake/runner-stops" "$fake/rc-polls"
  export FAKE_RC_UNTIL_STOP=1
  sleep 60 &
  gone=$!
  gone_start=$(proc_start "$gone")
  kill -KILL "$gone"
  wait "$gone" 2>/dev/null || true
  wait_rc=0
  wait_out=$(runner_wait step-pgkill 2 "$RUN_DIR/logs" "$gone" "$gone_start" 2>/dev/null) || wait_rc=$?
  ! grep -qx 'step-pgkill' "$fake/runner-stops" 2>/dev/null ||
    fail "a wait whose step shell is gone stopped the runner job step-pgkill: $(cat "$fake/runner-stops")"
  [ -z "$wait_out" ] ||
    fail "a wait whose step shell is gone printed '$wait_out', which its caller reads as the job's exit status"
  ((wait_rc != 0)) || fail "a wait whose step shell is gone returned 0, which its caller reads as a job that ended"
  write_lock "$other_run" "$(utc_now)"
  self=$BASHPID
  self_start=$(proc_start "$self")
  wait_rc=0
  wait_out=$(runner_wait step-pgkill 0 "$RUN_DIR/logs" "$self" "$self_start" 2>/dev/null) || wait_rc=$?
  ! grep -qx 'step-pgkill' "$fake/runner-stops" 2>/dev/null ||
    fail "a wait at its deadline stopped step-pgkill although the lock names run $other_run: $(cat "$fake/runner-stops")"
  ((wait_rc != 0)) || fail "a wait that ended because the lock names another run returned 0"
  # What the step shell does next on that path: its EXIT trap, with the typing of a --typing step
  # still running as job typing-pgkill and its token in TYPING_DIR.
  TYPING=1 TYPING_PEERS=2 TYPING_RATE=5 TYPING_DOCS=1
  TYPING_LIST=(doc-1)
  TYPING_STEP=(pgkill "$kill_dir")
  token_rms=$(grep -c -- "rm -f $TYPING_DIR/token" "$fake/kubectl.log" || true)
  typing_stop_at_exit
  ! grep -qx 'typing-pgkill' "$fake/runner-stops" 2>/dev/null ||
    fail "the EXIT trap of a step that ended because the lock names run $other_run stopped the browser typing job typing-pgkill, which that run's typing is: $(cat "$fake/runner-stops")"
  [ "$(grep -c -- "rm -f $TYPING_DIR/token" "$fake/kubectl.log" || true)" = "$token_rms" ] ||
    fail "that EXIT trap removed the agent token from the runner, which is run $other_run's pod now"
  [ ! -e "$kill_dir/browsertype.json" ] && [ ! -e "$kill_dir/browsertype.txt" ] ||
    fail "that EXIT trap wrote a browsertype record of a job it must not touch: $(names "$kill_dir")"
  # The lock is this run's again, so a wait with no pid and start time stops its job as it did.
  write_lock "$kill_run" "$(utc_now)"
  rm -f -- "$fake/rc-polls"
  wait_out=$(runner_wait step-pgkill 0 "$RUN_DIR/logs" 2>/dev/null)
  [ "$wait_out" = timeout ] ||
    fail "a wait with no pid and start time printed '$wait_out' past its deadline, not timeout"
  grep -qx 'step-pgkill' "$fake/runner-stops" ||
    fail "a wait with no pid and start time did not stop its job past its deadline: $(cat "$fake/runner-stops" 2>/dev/null)"
)
rm -f -- "$fake/lock.json"
pass "the disk watcher and the runner-job wait of a step that was killed outright stop at their next poll, record nothing and stop no runner job; a wait stops no job at its deadline once the lock names another run, and the step's EXIT trap then stops no browser typing and removes no token there"

# (x) A copy-back where heap.txt alone did not come back. Every other file of the step is here,
# so the step is a complete one: marking it failed (kind copy_back) would drop its row from the
# curves and the tables for a heap profile they take no number from. It is a note and the marker
# heap-missing.txt instead, and the runner keeps that heap.txt, the step's other files removed
# from it; an existence check that failed says so in its own words and leaves the runner whole,
# since the heap.txt that may be there would go with the rest. The marker is collected with the
# step, inside its tar, so it must say what the removal did and not what it was about to try: a
# removal that failed leaves every file of the step in the runner, and the marker says so.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-heapback
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC: $(cat "$tmp/out")"
heap_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")/steps/heapcut
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC: $(cat "$tmp/out")"
FAKE_HEAP_COPY_FAIL=1 run_cluster step --namespace "$ns" heapcut 1 --duration 1m
unset FAKE_HEAP_COPY_FAIL
((RC == 0)) || fail "a step whose heap.txt alone did not come back exited $RC: $(cat "$tmp/out")"
[ ! -f "$heap_dir/step-failed.json" ] ||
  fail "a step whose heap.txt alone did not come back was marked failed, so the curves and the tables lose its row: $(cat "$heap_dir/step-failed.json")"
[ -f "$heap_dir/summary.json" ] || fail "the step's summary.json did not come back: $(names "$heap_dir")"
[ ! -e "$heap_dir/heap.txt" ] && [ ! -e "$heap_dir/heap.txt.part" ] ||
  fail "the step directory holds a heap.txt the runner never sent whole: $(names "$heap_dir")"
heap_note='heap.txt (the step-end heap profile in text) was not copied back; it stays in the runner at /work/steps/heapcut/heap.txt; every other file of the step is here and was removed from the runner, and codepaths shows - for the MemStats rows.'
grep -qF "step heapcut: $heap_note" "$tmp/out" ||
  fail "the step does not say that heap.txt alone was not copied back: $(cat "$tmp/out")"
[ "$(cat "$heap_dir/heap-missing.txt" 2>/dev/null)" = "$heap_note" ] ||
  fail "the step directory keeps no heap-missing.txt holding the note: $(cat "$heap_dir/heap-missing.txt" 2>/dev/null)"
! grep -q -- "exec runner -c runner -- rm -rf /work/steps/heapcut " "$fake/kubectl.log" ||
  fail "the runner's step directory was removed whole although heap.txt stayed in it"
grep -qx heapcut "$fake/runner-heap-kept" 2>/dev/null ||
  fail "the step's other files were not removed from the runner, heap.txt apart: $(grep -c . "$fake/runner-heap-kept" 2>/dev/null)"
# The existence check itself failing: nothing of the step is removed from the runner, since a
# heap.txt that is there would go with it, and the note says what could not be read.
rm -f -- "$fake/runner-heap-kept"
heap_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")/steps/heapask
FAKE_HEAP_EXISTS_FAIL=1 run_cluster step --namespace "$ns" heapask 1 --duration 1m
unset FAKE_HEAP_EXISTS_FAIL
((RC == 0)) || fail "a step whose heap.txt existence check failed exited $RC: $(cat "$tmp/out")"
[ ! -f "$heap_dir/step-failed.json" ] ||
  fail "a step whose heap.txt existence check failed was marked failed: $(cat "$heap_dir/step-failed.json")"
[ -f "$heap_dir/summary.json" ] || fail "the step's summary.json did not come back: $(names "$heap_dir")"
heap_ask_note='could not read whether /work/steps/heapask/heap.txt exists; the rest of the step is here, and codepaths shows - for the MemStats rows. Nothing of the step was removed from the runner.'
grep -qF "step heapask: $heap_ask_note" "$tmp/out" ||
  fail "the step does not say that the heap.txt existence check failed: $(cat "$tmp/out")"
[ "$(cat "$heap_dir/heap-missing.txt" 2>/dev/null)" = "$heap_ask_note" ] ||
  fail "the step directory keeps no heap-missing.txt holding that note: $(cat "$heap_dir/heap-missing.txt" 2>/dev/null)"
{ ! grep -q -- "exec runner -c runner -- rm -rf /work/steps/heapask " "$fake/kubectl.log" && [ ! -s "$fake/runner-heap-kept" ]; } ||
  fail "something of the step was removed from the runner although it is not known whether heap.txt is there"
# The removal of the step's other files failing too: the note and the marker say that every file
# of the step stays in the runner, never that they were removed.
rm -f -- "$fake/runner-heap-kept"
heap_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")/steps/heapstay
FAKE_HEAP_COPY_FAIL=1 FAKE_HEAP_KEEP_REMOVE_FAIL=1 run_cluster step --namespace "$ns" heapstay 1 --duration 1m
unset FAKE_HEAP_COPY_FAIL FAKE_HEAP_KEEP_REMOVE_FAIL
((RC == 0)) || fail "a step whose heap.txt and whose removal both failed exited $RC: $(cat "$tmp/out")"
[ ! -f "$heap_dir/step-failed.json" ] ||
  fail "a step whose heap.txt alone did not come back was marked failed because the removal failed too: $(cat "$heap_dir/step-failed.json")"
[ -f "$heap_dir/summary.json" ] || fail "the step's summary.json did not come back: $(names "$heap_dir")"
heap_stay_note='heap.txt (the step-end heap profile in text) was not copied back; it stays in the runner at /work/steps/heapstay/heap.txt; every other file of the step is here; removing them from the runner failed, so they stay at /work/steps/heapstay, and codepaths shows - for the MemStats rows.'
[ "$(cat "$heap_dir/heap-missing.txt" 2>/dev/null)" = "$heap_stay_note" ] ||
  fail "heap-missing.txt, which collect packs with the step, does not say that the removal failed and the step's files stay in the runner: $(cat "$heap_dir/heap-missing.txt" 2>/dev/null)"
grep -qF "step heapstay: $heap_stay_note" "$tmp/out" ||
  fail "the step does not say that the removal of its files from the runner failed: $(cat "$tmp/out")"
[ ! -s "$fake/runner-heap-kept" ] ||
  fail "the runner recorded a removal although it failed: $(cat "$fake/runner-heap-kept")"
run_cluster down --namespace "$ns"
pass "a step whose heap.txt alone did not come back is complete, says so in a note and in heap-missing.txt written from what the removal did, and the runner keeps that heap.txt alone, or every file of the step when that removal failed; an existence check that failed removes nothing there"

# (y) The copy answers to fixed names -- the pods runner and postgres-0, the deployments dispatch
# and listener -- so every one of them reaches whatever run holds the namespace now. Once the
# lock names another run this run's files, pids and job names say nothing about who owns what
# they point at, and a stop, a write, a scale or a removal of this run's would hit that run's
# copy. So every write, kill and removal in those pods goes through one of five helpers that
# read the lock first -- runner_exec, postgres_exec, psql_write, runner_put, scale_to -- and each
# leaves the copy alone, saying which pod or deployment; a command whose --run names a run the
# lock does not is refused before it registers itself, naming both runs; and collect, which
# reads this pod's run directory and reaches no pod at all, packs and uploads its own run with
# such a --run, since nothing it does can touch the run that holds the namespace.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-othersrun
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok" --browsertype-commit "$commit_bt"
((RC == 0)) || fail "up exited $RC: $(cat "$tmp/out")"
own_run=$(jq -r .data.run "$fake/lock.json")
own_dir=$LOADTEST_CLUSTER_ROOT/$own_run
# A run with results of its own, for the collect below.
run_cluster seed --namespace "$ns"
((RC == 0)) || fail "seed exited $RC: $(cat "$tmp/out")"
run_cluster step --namespace "$ns" packed 1 --duration 1m
((RC == 0)) || fail "the step packed exited $RC: $(cat "$tmp/out")"
# The namespace is another run's from here: the lock says so, whatever this pod still holds.
write_lock "$other_run" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
guard_out=$tmp/guard.out
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run "$own_run"
  rm -f -- "$fake/runner-stops" "$fake/runner-starts" "$fake/runner-heap-kept"
  guard_rc=0
  runner_stop step-guard 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_stop returned 0 although the lock names run $other_run"
  [ ! -s "$fake/runner-stops" ] ||
    fail "runner_stop TERMed the runner job step-guard, which is run $other_run's now: $(cat "$fake/runner-stops")"
  grep -qF "the lock no longer names run $own_run" "$guard_out" ||
    fail "runner_stop did not say why it stopped nothing: $(cat "$guard_out")"
  grep -qF 'the runner pod' "$guard_out" ||
    fail "runner_stop did not name the pod it left alone: $(cat "$guard_out")"
  guard_rc=0
  runner_step_remove guard 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_step_remove returned 0 although the lock names run $other_run"
  ! grep -q -- 'exec runner -c runner -- rm -rf /work/steps/guard ' "$fake/kubectl.log" ||
    fail "runner_step_remove removed a step directory from run $other_run's runner"
  grep -qF 'they stay at /work/steps/guard' "$guard_out" ||
    fail "runner_step_remove did not say what it left in the runner: $(cat "$guard_out")"
  TYPING=1 TYPING_PEERS=2 TYPING_RATE=5 TYPING_DOCS=1
  TYPING_LIST=(doc-1)
  mkdir -p "$own_dir/steps/guard"
  # The agent token: the runner holds /work/browsertype already (up put it there), so
  # put_executable copies nothing and the token write is typing_start's first call that reaches
  # the pod. Unguarded it would write this run's token over the token of run $other_run, whose
  # runner pod that is.
  guard_rc=0
  tokens=$(grep -cF 'cat >"$1/token"' "$fake/kubectl.log" || true)
  copies=$(grep -c -- ' cp ' "$fake/kubectl.log" || true)
  (typing_start guard "$own_dir/steps/guard" 60) 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "typing_start returned 0 although the lock names run $other_run"
  [ "$(grep -cF 'cat >"$1/token"' "$fake/kubectl.log" || true)" = "$tokens" ] ||
    fail "typing_start wrote this run's agent token into run $other_run's runner pod: $(grep -F 'cat >"$1/token"' "$fake/kubectl.log" | tail -n1)"
  [ ! -e "$fake/runner$TYPING_DIR/token" ] ||
    fail "the agent token lies in the pod named runner, which is run $other_run's now"
  [ "$(grep -c -- ' cp ' "$fake/kubectl.log" || true)" = "$copies" ] ||
    fail "typing_start copied browsertype into run $other_run's runner: the runner holds it already, so put_executable must copy nothing and the token write must guard itself"
  grep -qF 'the runner pod' "$guard_out" ||
    fail "typing_start did not name the pod it left alone: $(cat "$guard_out")"
  typing_stop guard "$own_dir/steps/guard" 2>"$guard_out" ||
    fail "typing_stop returned non-zero while the lock names run $other_run; its callers treat that as a failure"
  [ ! -s "$fake/runner-stops" ] ||
    fail "typing_stop TERMed the browser typing job of run $other_run's copy: $(cat "$fake/runner-stops")"
  [ ! -e "$own_dir/steps/guard/browsertype.json" ] ||
    fail "typing_stop wrote a browsertype record of a job it never stopped"
  grep -qF 'typing-guard' "$guard_out" || fail "typing_stop did not name what it left alone: $(cat "$guard_out")"
  scales=$(grep -c -- 'scale deployment' "$fake/kubectl.log" || true)
  guard_rc=0
  (stop_copy) 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "stop_copy returned 0 although the lock names run $other_run"
  [ "$(grep -c -- 'scale deployment' "$fake/kubectl.log" || true)" = "$scales" ] ||
    fail "stop_copy scaled run $other_run's Dispatch or listener: $(grep -- 'scale deployment' "$fake/kubectl.log" | tail -n2)"
  grep -qF 'refusing to scale deployment dispatch to 0' "$guard_out" ||
    fail "stop_copy did not refuse the scale in its own words: $(cat "$guard_out")"
  grep -qF "the lock no longer names run $own_run" "$guard_out" ||
    fail "stop_copy did not say whose copy it left alone: $(cat "$guard_out")"
  guard_rc=0
  (runner_start step-guard /work/loadgen run) 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_start returned 0 although the lock names run $other_run"
  [ ! -s "$fake/runner-starts" ] ||
    fail "runner_start sent a job into run $other_run's runner pod: $(cat "$fake/runner-starts")"
  grep -qF 'the runner pod' "$guard_out" || fail "runner_start did not say which pod it left alone: $(cat "$guard_out")"
  guard_rc=0
  calls=$(own_calls_mark)
  (runner_put "$cluster" /work/loadgen) 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_put returned 0 although the lock names run $other_run"
  new_calls=$(own_calls_since "$calls")
  ! grep -q -- ' cp ' <<<"$new_calls" || fail "runner_put copied into run $other_run's runner pod: $(grep -- ' cp ' <<<"$new_calls")"
  guard_rc=0
  psqls=$(grep -c -- 'exec postgres-0 -c postgres -- psql' "$fake/kubectl.log" || true)
  psql_write postgres 'drop database if exists dispatch with (force)' 2>"$guard_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "psql_write returned 0 although the lock names run $other_run"
  [ "$(grep -c -- 'exec postgres-0 -c postgres -- psql' "$fake/kubectl.log" || true)" = "$psqls" ] ||
    fail "psql_write ran SQL in run $other_run's Postgres pod: $(grep -- 'exec postgres-0 -c postgres -- psql' "$fake/kubectl.log" | tail -n1)"
  grep -qF 'the Postgres pod postgres-0' "$guard_out" ||
    fail "psql_write did not name the pod it left alone: $(cat "$guard_out")"
)
# collect reads this pod's run directory and runs loadgen here, so it packs and uploads run
# $own_run while run $other_run holds the namespace, without reading the lock at all.
: >"$fake/dispatch.log"
collect_calls=$(own_calls_mark)
run_cluster collect --namespace "$ns" --run "$own_run" --upload-issue TEST-1
((RC == 0)) || fail "collect --run $own_run exited $RC while the lock names run $other_run: $(cat "$tmp/out")"
[ -f "$own_dir/TABLES.md" ] || fail "collect --run $own_run wrote no TABLES.md: $(cat "$tmp/out")"
own_results=$(find "$own_dir/upload" -name 'cluster-load-test-*-raw-results.tar.zst.part00' | head -n1)
[ -n "$own_results" ] || fail "collect --run $own_run packed no part of phase results"
grep -q -- "--name ${own_results##*/}" "$fake/dispatch.log" ||
  fail "collect --run $own_run uploaded no part: $(cat "$fake/dispatch.log")"
collect_own=$(own_calls_since "$collect_calls")
! grep -q 'configmap dispatch-loadtest-lock' <<<"$collect_own" ||
  fail "collect --run $own_run read the lock, which names another run, although it reaches no pod of the copy"
# A command that drives the copy, named by --run, is refused before it drives run $other_run's.
run_cluster step --namespace "$ns" --run "$own_run" guardstep 1 --duration 1m
((RC != 0)) || fail "step --run $own_run ran although the lock names run $other_run: $(cat "$tmp/out")"
{ grep -qF "$own_run" "$tmp/out" && grep -qF "$other_run" "$tmp/out"; } ||
  fail "the refusal of --run $own_run does not name both runs: $(cat "$tmp/out")"
[ ! -e "$own_dir/steps/guardstep" ] || fail "the refused step made the step directory $own_dir/steps/guardstep"
run_cluster down --namespace "$ns" --run "$own_run"
((RC == 0)) || fail "down --run $own_run exited $RC: $(cat "$tmp/out")"
[ -f "$fake/lock.json" ] || fail "down --run $own_run deleted run $other_run's lock"
rm -f -- "$fake/lock.json"
pass "with the lock naming another run, runner_stop, runner_step_remove and typing_stop touch nothing of the copy, runner_start, runner_put, typing_start's token write, psql_write and stop_copy refuse outright, each naming the pod or deployment it left alone, a command whose --run names this run is refused, naming both runs, and collect packs and uploads that run without reading the lock"

# (z) The daemons' lock read. cmd__heartbeat and cmd__sampler come through use_run any-run,
# which reads no lock when --run names their run, as the daemons' own --run does; only after
# that do they write the pidfile that start_daemon waits about 5 s for. So a refused API server
# connection, or a round trip longer than that wait, would kill both daemons and the command
# that started them -- up right after it took the lock, every later command at ensure_daemons --
# although the parent read the lock already and the heartbeat's own loop was built to outlive
# exactly these errors (its patch tests the lock's run id, and a lock that names another run
# ends it).
#
# The guard fails closed ((aa) below), so up itself now stops at the first write into the copy
# it cannot read the lock for -- the scale that starts Dispatch and the listener, which comes
# after the heartbeat and before the sampler. This case takes that refusal and starts the
# sampler the way up starts it, the reads still failing, so that both daemons' pidfiles are
# still proved written with no lock read of their own.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-lockread
FAKE_LOCK_GET_FAIL=1 FAKE_FAST_SLEEP=2 run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC != 0)) ||
  fail "up ran through although no read of the lock answered after it took one: the guard before a write into the copy fails closed: $(cat "$tmp/out")"
grep -qF "could not read the lock dispatch-loadtest-lock in $ns after 3 tries" "$tmp/out" ||
  fail "up did not stop at the guard of a write into the copy, naming the reads that failed: $(cat "$tmp/out")"
lockread_dir=$LOADTEST_CLUSTER_ROOT/$(jq -r .data.run "$fake/lock.json")
# The sampler, which up starts after that scale, started here exactly as up starts it.
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run "${lockread_dir##*/}"
  export FAKE_LOCK_GET_FAIL=1
  start_daemon sampler
) || fail "the sampler did not start while every read of the lock fails: $(cat "$lockread_dir/logs/sampler.log" 2>/dev/null)"
for daemon in heartbeat sampler; do
  [ -f "$lockread_dir/$daemon.pid" ] ||
    fail "no $daemon started: it writes its pidfile only after a lock read that nothing retries ($(cat "$lockread_dir/logs/$daemon.log" 2>/dev/null))"
  kill -0 "$(<"$lockread_dir/$daemon.pid")" 2>/dev/null ||
    fail "the $daemon of run ${lockread_dir##*/} is not running: $(cat "$lockread_dir/logs/$daemon.log" 2>/dev/null)"
  ! grep -q 'could not read ConfigMap' "$lockread_dir/logs/$daemon.log" ||
    fail "the $daemon read the lock before it wrote its pidfile: $(cat "$lockread_dir/logs/$daemon.log")"
done
unset FAKE_LOCK_GET_FAIL
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down exited $RC: $(cat "$tmp/out")"
pass "the daemons write their pidfile without reading the lock, so both start although every read of the lock fails, while the guard stops up itself at the first write into the copy"

# (aa) The guard fails closed. Reading the lock is how a write, a kill or a removal in the copy's
# fixed names -- the pods runner and postgres-0, the deployments dispatch and listener -- tells
# this run's copy from the copy of the run that holds the namespace now. A read that fails says
# nothing about whose that copy is, so it may not pass for this run's: the guard reads again, 3
# tries 2 s apart, and once every try has failed it stops its caller, which then kills, removes
# and writes nothing there and says why, in the words of that last failed read: the guard is the
# only one who saw them, and without them the note says a read failed and not what the API
# server answered. A read that answers after one that failed is an answer, and the caller acts
# on it.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-lockclosed
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC: $(cat "$tmp/out")"
closed_run=$(jq -r .data.run "$fake/lock.json")
closed_out=$tmp/guard-closed.out
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run "$closed_run"
  # The 2 s between two tries is the guard's own nap, which the fake sleep records and returns
  # from at once.
  export FAKE_FAST_SLEEP=2 FAKE_LOCK_GET_FAIL=1
  rm -f -- "$fake/runner-stops" "$fake/lock-get-fails"
  : >"$fake/sleep.log"
  reads=$(grep -c 'get configmap dispatch-loadtest-lock' "$fake/kubectl.log" || true)
  guard_rc=0
  runner_stop step-closed 2>"$closed_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_stop returned 0 although no read of the lock answered"
  [ ! -s "$fake/runner-stops" ] ||
    fail "runner_stop TERMed the runner job step-closed although it could not read the lock: $(cat "$fake/runner-stops")"
  grep -qF "could not read the lock dispatch-loadtest-lock in $ns after 3 tries" "$closed_out" ||
    fail "runner_stop did not say that no read of the lock answered: $(cat "$closed_out")"
  grep -qF 'The connection to the server 10.0.0.1:443 was refused' "$closed_out" ||
    fail "runner_stop's note does not carry what the failed read of the lock said: $(cat "$closed_out")"
  grep -qF 'the runner pod' "$closed_out" ||
    fail "runner_stop did not name the pod it left alone: $(cat "$closed_out")"
  tries=$(($(grep -c 'get configmap dispatch-loadtest-lock' "$fake/kubectl.log" || true) - reads))
  ((tries == 3)) || fail "the guard read the lock $tries times, not the 3 tries it gives a read that fails"
  [ "$(grep -cx 2 "$fake/sleep.log")" = 2 ] ||
    fail "the guard waited $(grep -cx 2 "$fake/sleep.log") times 2 s between its tries, not twice"
  guard_rc=0
  runner_step_remove closed 2>"$closed_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "runner_step_remove returned 0 although no read of the lock answered"
  ! grep -q -- 'exec runner -c runner -- rm -rf /work/steps/closed ' "$fake/kubectl.log" ||
    fail "runner_step_remove removed a step directory from a runner pod it could not read the lock for"
  grep -qF 'they stay at /work/steps/closed' "$closed_out" ||
    fail "runner_step_remove did not say what it left in the runner: $(cat "$closed_out")"
  guard_rc=0
  removals=$(grep -c -- 'exec postgres-0 -c postgres -- rm -f' "$fake/kubectl.log" || true)
  postgres_exec rm -f /var/lib/postgresql/copy/replay-seed.dump 2>"$closed_out" || guard_rc=$?
  ((guard_rc != 0)) || fail "postgres_exec returned 0 although no read of the lock answered"
  [ "$(grep -c -- 'exec postgres-0 -c postgres -- rm -f' "$fake/kubectl.log" || true)" = "$removals" ] ||
    fail "postgres_exec removed the replay dump from a Postgres pod it could not read the lock for: $(grep -- 'exec postgres-0 -c postgres -- rm -f' "$fake/kubectl.log" | tail -n1)"
  grep -qF "could not read the lock dispatch-loadtest-lock in $ns after 3 tries" "$closed_out" ||
    fail "postgres_exec did not say that no read of the lock answered: $(cat "$closed_out")"
  grep -qF 'the Postgres pod postgres-0' "$closed_out" ||
    fail "postgres_exec did not name the pod it left alone: $(cat "$closed_out")"
  # The other side: the first read fails and the next answers the lock, which names this run, so
  # the caller acts.
  export FAKE_LOCK_GET_FAIL_TIMES=1
  rm -f -- "$fake/lock-get-fails" "$fake/runner-stops"
  runner_stop step-closed 2>"$closed_out" ||
    fail "runner_stop returned non-zero although the read after the failed one answered the lock: $(cat "$closed_out")"
  grep -qx step-closed "$fake/runner-stops" 2>/dev/null ||
    fail "runner_stop TERMed no job although the read after the failed one answered the lock: $(cat "$closed_out")"
)
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down exited $RC: $(cat "$tmp/out")"
rm -f -- "$fake/lock-get-fails"
pass "a guarded write, kill or removal whose every read of the lock fails stops after 3 tries 2 s apart, touching nothing of the copy, saying so and carrying what that read said, and one whose read answers after a failed try acts"

# (bb) A lock no read answered is "act on nothing this time", never "stop watching". Failing
# closed is right either way, but the two cases are not the same: a lock that names another run
# says this run's copy is gone, while a lock the API server would not answer for says nothing at
# all -- and the step whose volume is filling, or whose job is past its deadline, is still
# running here. So the two guarded paths that come back, a step's disk watcher and its wait on
# the runner job, record nothing and stop nothing at a poll whose every read of the lock failed,
# and read the lock again at the next poll, where a read that answers decides it. Ending there
# instead would leave the watcher watching nothing while Postgres fills the volume, and would
# end the step at its deadline with the job it never stopped still sending load.
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-lockunknown
run_cluster up "${up_args[@]}" --dispatch-commit "$commit_ok"
((RC == 0)) || fail "up exited $RC: $(cat "$tmp/out")"
unknown_run=$(jq -r .data.run "$fake/lock.json")
unknown_dir=$LOADTEST_CLUSTER_ROOT/$unknown_run/steps/pgunknown
unknown_out=$tmp/lock-unknown.out
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run "$unknown_run"
  mkdir -p "$unknown_dir" "$RUN_DIR/logs"
  self=$BASHPID
  self_start=$(proc_start "$self")
  # The first three reads of the lock fail, which is one whole copy_is_ours (its 3 tries), and
  # every read after them answers the lock, which names this run. The naps the fake sleep
  # returns from at once are the guard's 2 s between tries, the watcher's 30 s poll and the
  # wait's 15 s poll, so no assertion below waits on a clock.
  export FAKE_FAST_SLEEP="2 15 30" FAKE_LOCK_GET_FAIL=1 FAKE_LOCK_GET_FAIL_TIMES=3 FAKE_RC_UNTIL_STOP=1
  export FAKE_PG_DF_SEQ=$fake/df-seq
  # The volume is under the threshold at every reading, so the watcher is about to act at its
  # first poll, where no read of the lock answers.
  df_seq "$pg_size $pg_used_high $pg_free_high"
  rm -f -- "$fake/lock-get-fails"
  : >"$fake/sleep.log"
  pg_disk_watch pgunknown "$unknown_dir" step-pgunknown "$unknown_dir/.pg-disk.jsonl" "$self" "$self_start" 2>"$unknown_out"
  grep -qF "could not read the lock dispatch-loadtest-lock in $ns after 3 tries" "$unknown_out" ||
    fail "the disk watcher did not say that no read of the lock answered: $(cat "$unknown_out")"
  [ "$(grep -cx 30 "$fake/sleep.log")" -ge 2 ] ||
    fail "the disk watcher took $(grep -cx 30 "$fake/sleep.log") poll(s): it stopped watching at the poll whose every read of the lock failed instead of reading again at the next one"
  jq -e '[.reasons[] | select(.kind == "pg_disk")] | length == 1' "$unknown_dir/step-failed.json" >/dev/null 2>&1 ||
    fail "the disk watcher recorded no pg_disk failure at the poll whose read of the lock answered: $(cat "$unknown_dir/step-failed.json" 2>/dev/null)"
  grep -qx 'step-pgunknown' "$fake/runner-stops" ||
    fail "the disk watcher stopped no runner job at the poll whose read of the lock answered: $(cat "$fake/runner-stops" 2>/dev/null)"
  # The wait on a runner job at its deadline: the stop it was refused because no read of the
  # lock answered is tried again at the next poll, and the job's .rc is polled on meanwhile.
  rm -f -- "$fake/lock-get-fails" "$fake/runner-stops" "$fake/rc-polls"
  : >"$fake/sleep.log"
  wait_rc=0
  wait_out=$(runner_wait step-pgunknown 0 "$RUN_DIR/logs" "$self" "$self_start" 2>"$unknown_out") || wait_rc=$?
  ((wait_rc == 0)) ||
    fail "a wait whose deadline fell on a poll where no read of the lock answered ended with status $wait_rc, giving up the step instead of trying the stop again at its next poll: $(cat "$unknown_out")"
  [ "$wait_out" = timeout ] ||
    fail "that wait printed '$wait_out', not the timeout its caller reads past the deadline: $(cat "$unknown_out")"
  grep -qx 'step-pgunknown' "$fake/runner-stops" ||
    fail "that wait stopped no job at the poll whose read of the lock answered: $(cat "$fake/runner-stops" 2>/dev/null)"
  grep -qF "could not read the lock dispatch-loadtest-lock in $ns after 3 tries" "$unknown_out" ||
    fail "that wait did not say why it stopped no job at the deadline whose reads all failed: $(cat "$unknown_out")"
  [ "$(grep -cx 15 "$fake/sleep.log")" -ge 1 ] ||
    fail "that wait polled no more after the deadline it stopped nothing at: it must keep polling the job's .rc"
  [ "$(cat "$fake/rc-polls" 2>/dev/null)" -ge 2 ] ||
    fail "that wait read the job's .rc $(cat "$fake/rc-polls" 2>/dev/null) time(s) after the deadline it stopped nothing at"
)
run_cluster down --namespace "$ns"
((RC == 0)) || fail "down exited $RC: $(cat "$tmp/out")"
rm -f -- "$fake/lock-get-fails"
pass "a poll whose every read of the lock failed stops nothing and records nothing, and the disk watcher and the runner-job wait read the lock again at their next poll, where an answer has the watcher record pg_disk and both stop the job"

# (cc) One option parse for both guarded execs. runner_exec and postgres_exec are one copy_exec,
# which reads the lock for the pod it is given and then runs kubectl: -t SECONDS and -i come in
# either order, the parse ends at the first argument that is neither, and the bound a -t gives
# is kube_for's, which no kubectl call records (it is timeout's own argument).
reset_fake
export LOADTEST_CLUSTER_ROOT=$tmp/root-copyexec
copyexec_calls=$tmp/kube-for.log
(
  # shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
  source "$cluster"
  NS=$ns
  set_run lt-20261030t101010-cafe
  write_lock "$RUN" "$(utc_now)"
  kube_for() { printf '%s\n' "$*" >>"$copyexec_calls"; }
  : >"$copyexec_calls"
  runner_exec -i -t 900 tar -C /work -xf - </dev/null ||
    fail "runner_exec -i -t 900 returned non-zero although the lock names this run"
  runner_exec -t 900 -i tar -C /work -xf - </dev/null ||
    fail "runner_exec -t 900 -i returned non-zero although the lock names this run"
  runner_exec rm -rf /work/steps/x ||
    fail "runner_exec with no option returned non-zero although the lock names this run"
  postgres_exec -t 3600 -i sh -c 'cat >"$1"' sh /tmp/dump </dev/null ||
    fail "postgres_exec -t 3600 -i returned non-zero although the lock names this run"
  mapfile -t copyexec_got <"$copyexec_calls"
  copyexec_want=(
    '900 exec -i runner -c runner -- tar -C /work -xf -'
    '900 exec -i runner -c runner -- tar -C /work -xf -'
    '300 exec runner -c runner -- rm -rf /work/steps/x'
    '3600 exec -i postgres-0 -c postgres -- sh -c cat >"$1" sh /tmp/dump'
  )
  for i in "${!copyexec_want[@]}"; do
    [ "${copyexec_got[i]:-}" = "${copyexec_want[i]}" ] ||
      fail "copy_exec call $((i + 1)) reached kube_for as '${copyexec_got[i]:-}', not '${copyexec_want[i]}'"
  done
)
rm -f -- "$fake/lock.json"
pass "copy_exec takes -t and -i in either order, stops its parse at the first argument that is neither, and gives kube_for the bound, the pod and its container"
