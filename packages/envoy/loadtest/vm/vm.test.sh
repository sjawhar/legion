#!/usr/bin/env bash
# packages/envoy/loadtest/vm/vm.test.sh
#
# Proves vm.sh's behaviour without the deployment it drives: no VM, no Cloud SQL instance and no
# network. cluster/cluster.test.sh is the model -- a $bin directory first on PATH holding one
# small script per external program, each recording its argv and answering from $FAKE_DIR.
#
# The fakes: `ssh` records the call and runs the command here, so the two VMs' file systems are
# this test's own directories and every program the driver runs on a VM (docker, nc, curl, tar,
# sh, loadgen) is the fake or the real one on this PATH; `docker` keeps a container registry per
# ssh target, computing from each `run` the NanoCpus, Memory, network mode and storage option
# that docker itself would record, which its `inspect` then answers, so a reading the test wants
# to differ is one file to rewrite; `curl` answers the metadata server, the Cloud SQL Admin API,
# the Compute API, Dispatch's /healthz, the profiling port and, only while the fake tunnel of
# that port runs, a read through the step's profiling tunnel; `psql` keeps a database list;
# `nc` refuses every address unless the test says otherwise; `loadgen` records its argv and
# writes the files the next step reads; `pprof-tunnel` is the deployment file's
# pprof_tunnel_command, which stands for the `ssh -N -L ...` a step runs on the generator VM.
# `jq` is the real one.
#
# Asserts that: (a) every subcommand refuses a missing --config, and refuses one that is not
# readable JSON, before it reaches a VM; (b) up starts every container of the specification, in
# order, each with exactly the CPU, memory and image arguments the table names, Dispatch with
# host networking and an 8 GiB ephemeral limit, each image by digest and none by tag, the stub
# embedder before Dispatch, and waits on /healthz; (c) up refuses an image named by tag;
# (d) check prints expected beside found for every value and exits 0 when they agree, and
# non-zero when a container's memory differs, when a production address answers from a VM, and
# when a VM's maxRunDuration is not 259200s, naming each; (e) check reads the instance's own
# settings from the Cloud SQL Admin API and each container's endpoint settings, and never prints
# a credential; (f) reset runs decision D6's steps in order -- the Dispatch and listener
# containers stopped, the database dropped and made again from the template, NATS restarted on
# an emptied JetStream store, the four load listeners and the Dispatch listener, then Dispatch,
# then /healthz -- and records its duration; (g) step passes the loop's flags to loadgen run
# (-loop-seed, -loop-factor, -generator-role, -unit-starts, -unit-life, -loop-listeners,
# -driver-topics, -receipt-deadline, -route-mix and -event-mix), with the rates the factor gives,
# fetches the profile from the application VM and runs cgroup-merge, cloudsql-cpu and tables;
# (h) a step at factor 0 is the baseline alone, at the baseline's own rates and mixes; (i) series
# runs the baseline, 1x and 2x at the long duration after the long warm-up, stops the grid at the
# first failing step, and then follows refine.txt until it is empty; (j) a step whose VM stopped
# while it ran is discarded -- its directory removed, the VM named, no failure recorded -- and
# so is one whose VM is already stopped, and series stops on it; (k) seed runs the plan, the
# write on the generator VM, the check, the export and seed-finish with the embedding gate, and
# records each phase's wall clock and the gate's progress; (l) down stops every container and
# leaves the VMs and the instance alone; (m) step opens the profiling tunnel on the generator VM
# and proves it with a read of /debug/pprof/goroutine?debug=1 through it before loadgen run,
# gives loadgen that tunnel as -pprof, takes its local port from the deployment file and 16060
# when it names none, closes it when the step ends -- no process and no pid file left on the
# generator VM -- and refuses the step by name, before any load and leaving no directory, when
# the tunnel does not answer; (n) up gives each VM's remote root a bind mount of a directory
# under the stateful partition, made with sudo, remounted exec,nosuid,nodev and given to the ssh
# user, all of it before the loadgen copy and any container, mounts nothing a second time over a
# VM that already carries the mount, takes the backing directory's name from remote_root's own
# last element unless the deployment file names another, and is refused by name, with no
# container started, when that mount cannot be made or is not exec; (o) up and check both run on
# deployment.example.json, with nothing of it changed but the paths that must point at this pod;
# (p) that example carries every key vm.sh reads and no other, both lists read out of the
# script's own cfg, cfg_valid and jq filters rather than written here again; (q) the example is
# valid JSON and names no routable address, no real machine and no credential.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly vm=$here/vm.sh
# The real sleep, caught before the fake one goes on PATH: the fake sampler naps with it, so a
# fake sleep that returns at once never turns its wait into a spin.
REAL_SLEEP=$(command -v sleep)
readonly REAL_SLEEP
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vm-test.XXXXXX")
readonly tmp
readonly fake=$tmp/fake bin=$tmp/bin
export FAKE_DIR=$fake
export LOADTEST_VM_ROOT=$tmp/root

# A case that fails while a step runs leaves the fake sampler, which vm.sh ends itself when the
# step ends. Both fake loadgens -- this test's $bin one and the copy up put on the generator VM
# -- are under $tmp, and this test's own process is not, so the pattern reaches neither it nor
# anything of another run.
cleanup() {
  pkill -f "$tmp/" 2>/dev/null || true
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

mkdir -p "$fake" "$bin" "$tmp/root"

# ---- the fakes -------------------------------------------------------------------------------

cat >"$bin/ssh" <<'EOF'
#!/usr/bin/env bash
# The fake ssh: records the call and runs the command here, so a VM's file system is this test's
# own directories. The target reaches the command in FAKE_SSH_TARGET, which the fake docker uses
# to keep a container registry per VM.
set -u
target=$1
shift
[ "${1:-}" != -- ] || shift
printf 'ssh\t%s\t%s\n' "$target" "$*" >>"$FAKE_DIR/actions.log"
export FAKE_SSH_TARGET=$target
exec "$@"
EOF

cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
# The fake docker: one registry per ssh target under FAKE_DIR/docker. `run` records what docker
# itself would record of the arguments it was given -- NanoCpus, Memory, the image, the network
# mode and the size storage option -- and `inspect` answers the three formats vm.sh reads.
set -u
target=${FAKE_SSH_TARGET:-local}
printf 'docker\t%s\t%s\n' "$target" "$*" >>"$FAKE_DIR/actions.log"
printf '%s %s\n' "$target" "$*" >>"$FAKE_DIR/docker.log"
dir=$FAKE_DIR/docker/$target
mkdir -p "$dir"
verb=${1:-}
shift || true
case $verb in
  rm)
    name=${!#}
    [ -f "$dir/$name.main" ] || exit 1
    rm -f -- "$dir/$name".*
    printf '%s\n' "$name"
    ;;
  run)
    name="" cpus=- memory=- network=default storage='<no value>' envfile="" image=""
    while [ $# -gt 0 ]; do
      case $1 in
        --name) name=$2; shift 2 ;;
        --cpus) cpus=$2; shift 2 ;;
        --memory) memory=$2; shift 2 ;;
        --network) network=$2; shift 2 ;;
        --storage-opt) storage=${2#size=}; shift 2 ;;
        --env-file) envfile=$2; shift 2 ;;
        --publish | --volume | --restart) shift 2 ;;
        --detach) shift ;;
        *) image=$1; shift; break ;;
      esac
    done
    [ -n "$name" ] || { echo 'docker run without --name' >&2; exit 2; }
    [ -n "$image" ] || { echo 'docker run without an image' >&2; exit 2; }
    nano=$(awk -v c="$cpus" 'BEGIN { if (c == "-") print 0; else printf "%d", c * 1000000000 }')
    case $memory in
      -) bytes=0 ;;
      *m) bytes=$(( ${memory%m} * 1024 * 1024 )) ;;
      *g) bytes=$(( ${memory%g} * 1024 * 1024 * 1024 )) ;;
      *) echo "docker run: memory $memory" >&2; exit 2 ;;
    esac
    printf '/%s %s %s %s\n' "$name" "$nano" "$bytes" "$image" >"$dir/$name.main"
    printf '%s %s\n' "$network" "$storage" >"$dir/$name.net"
    : >"$dir/$name.env"
    [ -z "$envfile" ] || cat -- "$envfile" >"$dir/$name.env"
    printf '%s\n' "$(printf '%s%s' "$target" "$name" | sha256sum | cut -c1-64)" >"$dir/$name.id"
    cat "$dir/$name.id"
    ;;
  inspect)
    fmt="" name=""
    while [ $# -gt 0 ]; do
      case $1 in
        -f) fmt=$2; shift 2 ;;
        *) name=$1; shift ;;
      esac
    done
    case $fmt in
      '{{.Name}} {{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.Config.Image}}') file=$dir/$name.main ;;
      '{{.HostConfig.NetworkMode}} {{index .HostConfig.StorageOpt "size"}}') file=$dir/$name.net ;;
      '{{range .Config.Env}}{{println .}}{{end}}') file=$dir/$name.env ;;
      '{{.Id}}') file=$dir/$name.id ;;
      *) echo "fake docker: no answer for format $fmt" >&2; exit 2 ;;
    esac
    [ -f "$file" ] || { echo "Error: No such object: $name" >&2; exit 1; }
    cat -- "$file"
    ;;
  *)
    echo "fake docker: no subcommand $verb" >&2
    exit 2
    ;;
esac
exit 0
EOF

cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
# The fake curl: the URL is the last argument of every call vm.sh makes.
set -u
url=${!#}
printf 'curl\t%s\n' "$url" >>"$FAKE_DIR/actions.log"
case $url in
  *metadata*) printf '{"access_token": "fake-token", "expires_in": 3600}\n' ;;
  *sqladmin*) cat "$FAKE_DIR/cloudsql.json" ;;
  *compute*/instances/*)
    name=${url##*/}
    n=0
    [ ! -f "$FAKE_DIR/compute-reads" ] || n=$(cat "$FAKE_DIR/compute-reads")
    n=$((n + 1))
    printf '%s\n' "$n" >"$FAKE_DIR/compute-reads"
    stop_after=""
    [ ! -f "$FAKE_DIR/compute-stop-after" ] || stop_after=$(cat "$FAKE_DIR/compute-stop-after")
    if [ -n "$stop_after" ] && [ "$n" -gt "$stop_after" ]; then
      jq '.status = "TERMINATED"' "$FAKE_DIR/compute-$name.json"
    else
      cat "$FAKE_DIR/compute-$name.json"
    fi
    ;;
  */healthz) cat "$FAKE_DIR/healthz" ;;
  *debug/pprof/*)
    # A read of the application VM's own profiling port always answers. A read of any other port
    # is one through the step's tunnel, and answers only while the fake tunnel that forwards
    # that port runs: the tunnel records its pid and its port in FAKE_DIR/tunnel-up, and this is
    # how a test proves the tunnel was opened before the step and closed when it ended.
    port=${url#http://127.0.0.1:}
    port=${port%%/*}
    if [ "$port" != "$(cat "$FAKE_DIR/pprof-port")" ]; then
      up=$FAKE_DIR/tunnel-up
      [ -f "$up" ] || { printf 'fake curl: connection refused on %s\n' "$url" >&2; exit 7; }
      kill -0 "$(awk '{print $1}' "$up")" 2>/dev/null && [ "$(awk '{print $2}' "$up")" = "$port" ] ||
        { printf 'fake curl: connection refused on %s\n' "$url" >&2; exit 7; }
    fi
    printf 'fake profile of %s\n' "$url"
    ;;
  *) printf 'fake curl: no answer for %s\n' "$url" >&2; exit 22 ;;
esac
exit 0
EOF

cat >"$bin/psql" <<'EOF'
#!/usr/bin/env bash
# The fake psql: records each statement and keeps a database list under FAKE_DIR/db.
set -u
sql="" prev=""
for a in "$@"; do
  [ "$prev" = -c ] && sql=$a
  prev=$a
done
printf 'psql\t%s\n' "$sql" >>"$FAKE_DIR/actions.log"
db=$FAKE_DIR/db
mkdir -p "$db"
if [[ $sql =~ ^select\ 1\ from\ pg_database\ where\ datname\ =\ \'([A-Za-z0-9_]+)\'$ ]]; then
  [ -e "$db/${BASH_REMATCH[1]}" ] && printf '1\n'
elif [[ $sql =~ ^drop\ database\ if\ exists\ ([A-Za-z0-9_]+) ]]; then
  rm -f -- "$db/${BASH_REMATCH[1]}"
elif [[ $sql =~ ^create\ database\ ([A-Za-z0-9_]+)\ template\ ([A-Za-z0-9_]+)$ ]]; then
  [ -e "$db/${BASH_REMATCH[2]}" ] || { echo "no template ${BASH_REMATCH[2]}" >&2; exit 1; }
  : >"$db/${BASH_REMATCH[1]}"
elif [[ $sql =~ ^alter\ database\ ([A-Za-z0-9_]+)\ rename\ to\ ([A-Za-z0-9_]+)$ ]]; then
  [ -e "$db/${BASH_REMATCH[1]}" ] || { echo "no database ${BASH_REMATCH[1]}" >&2; exit 1; }
  mv -- "$db/${BASH_REMATCH[1]}" "$db/${BASH_REMATCH[2]}"
fi
exit 0
EOF

cat >"$bin/nc" <<'EOF'
#!/usr/bin/env bash
# The fake nc: every address is refused (exit 1) unless FAKE_DIR/nc/<host>-<port> says otherwise.
set -u
port=${!#}
host=${@: -2:1}
printf 'nc\t%s\t%s:%s\n' "${FAKE_SSH_TARGET:-local}" "$host" "$port" >>"$FAKE_DIR/actions.log"
f=$FAKE_DIR/nc/$host-$port
[ -f "$f" ] || exit 1
exit "$(cat "$f")"
EOF

cat >"$bin/loadgen" <<EOF
#!/usr/bin/env bash
# The fake loadgen: records its argv and writes the files the next step reads.
set -u
REAL_SLEEP=$REAL_SLEEP
EOF
cat >>"$bin/loadgen" <<'EOF'
printf 'loadgen\t%s\n' "$*" >>"$FAKE_DIR/actions.log"
printf '%s\n' "$*" >>"$FAKE_DIR/loadgen.log"
cmd=${1:-}
shift || true
args=("$@")
flag() {
  local want=$1 i
  for ((i = 0; i + 1 < ${#args[@]}; i++)); do
    if [ "${args[i]}" = "-$want" ]; then
      printf '%s\n' "${args[i + 1]}"
      return 0
    fi
  done
  return 1
}
# tail_dirs: the trailing arguments that are not a flag or a flag's value.
tail_dirs() {
  local i out=()
  for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
      -*) i=$((i + 1)) ;;
      *) out+=("${args[i]}") ;;
    esac
  done
  printf '%s\n' "${out[@]:-}"
}
case $cmd in
  loop-seed-plan) printf '{"proposals": 891}\n' >"$(flag out)" ;;
  loop-seed-write)
    printf '%s\n' "$*" >>"$FAKE_DIR/loop-seed-write.log"
    printf '{"wrote": 891}\n' >"$(flag out)"
    ;;
  loop-seed-check) : ;;
  loop-seed-export) printf '{"proposals": 891}\n' >"$(flag out)" ;;
  seed-finish)
    printf '%s\n' "$*" >>"$FAKE_DIR/seed-finish.log"
    p=$(flag embed-progress) && printf '{"lacking": 0, "failed": 0}\n' >"$p"
    ;;
  stub-embed) printf -- '-----FAKE CERT-----\n' >"$(flag cert-out)" ;;
  vm-sample)
    printf '%s\n' "$*" >>"$FAKE_DIR/vm-sample.log"
    printf '{"role": "dispatch"}\n' >>"$(flag out)"
    trap 'exit 0' TERM INT
    while :; do "$REAL_SLEEP" 0.2; done
    ;;
  run)
    printf '%s\n' "$*" >>"$FAKE_DIR/loadgen-run.log"
    out=$(flag out)
    mkdir -p "$out"
    printf '{"start": "2026-11-03T00:00:00Z", "name": "%s"}\n' "$(flag name)" >"$out/meta.json"
    printf '{"config": {"loop_factor": %s}}\n' "$(flag loop-factor)" >"$out/summary.json"
    ;;
  cgroup-merge) printf '{"roles": []}\n' >"${args[0]}/cgroup.json" ;;
  cloudsql-cpu) printf '{"cores_mean": 1}\n' >"$(flag step)/cloudsql-cpu.json" ;;
  tables) printf '# TABLES\n' >"${args[${#args[@]} - 1]}/TABLES.md" ;;
  curves)
    out=$(flag out)
    mkdir -p "$out"
    {
      printf 'step,factor,passes\n'
      while read -r d; do
        [ -n "$d" ] || continue
        name=${d##*/}
        verdict=true
        [ ! -f "$FAKE_DIR/pass/$name" ] || verdict=$(cat "$FAKE_DIR/pass/$name")
        printf '%s,0,%s\n' "$name" "$verdict"
      done < <(tail_dirs)
    } >"$out/curves.csv"
    n=0
    [ ! -f "$FAKE_DIR/curves-calls" ] || n=$(cat "$FAKE_DIR/curves-calls")
    n=$((n + 1))
    printf '%s\n' "$n" >"$FAKE_DIR/curves-calls"
    if [ -f "$FAKE_DIR/refine.$n" ]; then
      cp -- "$FAKE_DIR/refine.$n" "$out/refine.txt"
    else
      : >"$out/refine.txt"
    fi
    printf '# KNEES\n' >"$out/KNEES.md"
    ;;
  *) printf 'fake loadgen: no command %s\n' "$cmd" >&2; exit 2 ;;
esac
exit 0
EOF

cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
# The fake sleep: returns at once, so a poll's naps cost the test no wall clock.
set -u
printf 'sleep\t%s\n' "$*" >>"$FAKE_DIR/actions.log"
exit 0
EOF

cat >"$bin/pprof-tunnel" <<EOF
#!/usr/bin/env bash
REAL_SLEEP=$REAL_SLEEP
EOF
cat >>"$bin/pprof-tunnel" <<'EOF'
# The fake tunnel: what the deployment file's pprof_tunnel_command names, standing for
# `ssh -N -L <port>:127.0.0.1:<pprof port> <the application VM>` run on the generator VM. It
# records its argv, says in FAKE_DIR/tunnel-up which port now answers and under which pid, and
# ends on TERM as ssh does. FAKE_DIR/tunnel-refuses makes it exit at once, as ssh does when the
# forward cannot be made (ExitOnForwardFailure) or the application VM refuses the connection.
set -u
printf 'tunnel\t%s\n' "$*" >>"$FAKE_DIR/actions.log"
printf '%s\n' "$*" >>"$FAKE_DIR/tunnel.log"
forward=""
while [ $# -gt 0 ]; do
  case $1 in
    -L) forward=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$forward" ] || { echo 'fake tunnel: no -L forward' >&2; exit 2; }
[ ! -f "$FAKE_DIR/tunnel-refuses" ] || exit 255
printf '%s %s\n' "$$" "${forward%%:*}" >"$FAKE_DIR/tunnel-up"
trap 'exit 0' TERM INT
while :; do "$REAL_SLEEP" 0.2; done
EOF

cat >"$bin/sudo" <<'EOF'
#!/usr/bin/env bash
# The fake sudo: records the call and runs it here. This pod is not root and has no
# /mnt/stateful_partition, so a path under the stateful partition is made under FAKE_DIR/stateful
# instead; the log keeps the path vm.sh asked for, which is what the cases read.
set -u
printf 'sudo\t%s\t%s\n' "${FAKE_SSH_TARGET:-local}" "$*" >>"$FAKE_DIR/actions.log"
args=()
for a in "$@"; do
  case $a in
    /mnt/stateful_partition*) args+=("$FAKE_DIR/stateful${a#/mnt/stateful_partition}") ;;
    *) args+=("$a") ;;
  esac
done
exec "${args[@]}"
EOF

cat >"$bin/mount" <<'EOF'
#!/usr/bin/env bash
# The fake mount: a mount table of its own for each ssh target, under FAKE_DIR/mounts. With no
# argument it prints that target's table in the form mount itself prints it, `SRC on DIR type
# TYPE (OPTS)`; `--bind SRC DIR` adds a line and `-o remount,... SRC DIR` rewrites DIR's. A line
# carries the options the mount was given, less remount, bind and exec, as the kernel's own
# table does: exec is the absence of noexec. FAKE_DIR/mount-refuses makes every mounting call
# fail, as a VM whose stateful partition cannot be bound does, and FAKE_DIR/mount-noexec makes
# every mount noexec whatever it was asked for, which is a VM where the remount does not take.
set -u
printf 'mount\t%s\t%s\n' "${FAKE_SSH_TARGET:-local}" "$*" >>"$FAKE_DIR/actions.log"
mkdir -p "$FAKE_DIR/mounts"
table=$FAKE_DIR/mounts/${FAKE_SSH_TARGET:-local}
[ -f "$table" ] || : >"$table"
[ $# -gt 0 ] || { cat "$table"; exit 0; }
[ ! -f "$FAKE_DIR/mount-refuses" ] || { echo 'fake mount: operation not permitted' >&2; exit 32; }
opts="" args=()
while [ $# -gt 0 ]; do
  case $1 in
    -o) opts=$2; shift 2 ;;
    --) shift ;;
    -*) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
[ ${#args[@]} -eq 2 ] || { echo "fake mount: no source and target in '$*'" >&2; exit 2; }
kept=rw
for o in ${opts//,/ }; do
  case $o in
    rw | remount | bind | exec) ;;
    *) kept="$kept,$o" ;;
  esac
done
[ ! -f "$FAKE_DIR/mount-noexec" ] || kept="$kept,noexec"
left=$(grep -vF -- " on ${args[1]} type " "$table" || true)
{
  [ -z "$left" ] || printf '%s\n' "$left"
  printf '%s on %s type none (%s)\n' "${args[0]}" "${args[1]}" "$kept"
} >"$table.new"
mv -- "$table.new" "$table"
exit 0
EOF

cat >"$bin/chown" <<'EOF'
#!/usr/bin/env bash
# The fake chown: records the call. This pod is not root and the directories a VM's file system
# stands for are its own already, so what a case reads is that up asked, and for which user.
set -u
printf 'chown\t%s\t%s\n' "${FAKE_SSH_TARGET:-local}" "$*" >>"$FAKE_DIR/actions.log"
exit 0
EOF

chmod +x "$bin"/*
export PATH="$bin:$PATH"

# ---- the deployment the fakes stand for ---------------------------------------------------------

readonly remote=$tmp/remote
readonly dispatch_image=europe-docker.pkg.dev/p/r/envoy@sha256:1111111111111111111111111111111111111111111111111111111111111111
readonly listener_image=europe-docker.pkg.dev/p/r/listener@sha256:2222222222222222222222222222222222222222222222222222222222222222
readonly proxy_image=gcr.io/cloud-sql-connectors/cloud-sql-proxy@sha256:3333333333333333333333333333333333333333333333333333333333333333
readonly nats_image=mirror.gcr.io/library/nats@sha256:4444444444444444444444444444444444444444444444444444444444444444

readonly config=$tmp/deployment.json
readonly tunnel_port=16161
jq -n --arg remote "$remote" --arg loadgen "$bin/loadgen" --arg tunnel "$bin/pprof-tunnel" \
  --argjson tunnel_port "$tunnel_port" \
  --arg dispatch "$dispatch_image" --arg listener "$listener_image" \
  --arg proxy "$proxy_image" --arg nats "$nats_image" '{
  project: "theorem-agent-infra", zone: "us-west1-a",
  token_url: "http://metadata.example/token",
  sqladmin: "https://sqladmin.googleapis.com",
  compute: "https://compute.googleapis.com",
  vms: {
    app: {ssh: "lt@10.138.0.21", host: "10.138.0.21", instance: "lt-app"},
    generator: {ssh: "lt@10.138.0.22", host: "10.138.0.22", instance: "lt-gen"}
  },
  cloudsql: {
    instance: "lt-db", connection_name: "theorem-agent-infra:us-west1:lt-db",
    host: "10.139.0.3", port: 5432, user: "dispatch", password: "pg-secret",
    database: "dispatch", template: "dispatch_seed", sslmode: "require", vcpus: 16,
    expect: {databaseVersion: "POSTGRES_18", "settings.edition": "ENTERPRISE",
      "settings.tier": "db-custom-16-61440", "settings.availabilityType": "REGIONAL"}
  },
  images: {dispatch: $dispatch, listener: $listener, proxy: $proxy, nats: $nats},
  pprof_tunnel_command: $tunnel,
  ports: {dispatch: 8766, pprof: 6060, pprof_tunnel: $tunnel_port, proxy: 5432, listener: 9020,
    hub_listener: 9030, nats: 4222, nats_monitor: 8222, stub_embed: 8443,
    load_listeners: [9021, 9022, 9023, 9024]},
  secrets: {dispatch_token: "dispatch-secret", listener_token: "listener-secret",
    embed_key: "embed-secret"},
  paths: {remote_root: ($remote + "/app"), remote_work: ($remote + "/work"),
    nats_store: ($remote + "/app/nats"), cert: ($remote + "/app/stub-embed.pem"),
    remote_loadgen: ($remote + "/app/loadgen"), loadgen: $loadgen},
  production_addresses: ["10.139.0.2:5432", "10.138.0.10:4222"]
}' >"$config"

write_fixtures() {
  jq -n '{databaseVersion: "POSTGRES_18",
    settings: {edition: "ENTERPRISE", tier: "db-custom-16-61440", availabilityType: "REGIONAL"}}' >"$fake/cloudsql.json"
  local name
  for name in lt-app lt-gen; do
    jq -n --arg n "$name" '{name: $n, status: "RUNNING", scheduling: {maxRunDuration: "259200s"}}' >"$fake/compute-$name.json"
  done
  printf '{"status": "ok"}\n' >"$fake/healthz"
  printf '6060\n' >"$fake/pprof-port"
}

reset_fake() {
  rm -rf -- "$fake" "$tmp/root" "$remote"
  mkdir -p "$fake/db" "$fake/nc" "$fake/pass" "$tmp/root" "$remote"
  : >"$fake/actions.log"
  write_fixtures
}

# run_vm ARGS: vm.sh ARGS, its output in $tmp/out and its status in RC.
run_vm() {
  set +e
  bash "$vm" "$@" >"$tmp/out" 2>&1
  RC=$?
  set -e
}

# run_dir: the directory of the run that up started.
run_dir() { printf '%s/%s\n' "$LOADTEST_VM_ROOT" "$(cat "$LOADTEST_VM_ROOT/current")"; }

# docker_run_line ROLE: the `docker run` call that started ROLE, the last one if it was started
# more than once.
docker_run_line() { grep -- " run --name $1 " "$fake/docker.log" | tail -n1; }

# sudo_log TARGET: every command vm.sh ran with sudo on TARGET, one a line.
sudo_log() { awk -F'\t' -v t="$1" '$1 == "sudo" && $2 == t { print $3 }' "$fake/actions.log"; }

# line_at PATTERN: the first line of the action log that matches PATTERN, by number.
line_at() { grep -n -- "$1" "$fake/actions.log" | head -n1 | cut -d: -f1; }

# actions: the ordered actions of the deployment, one word per line, from every fake's log: a
# container stopped or started, the database dropped or made, the JetStream store emptied and
# the wait on /healthz. It is how a test asserts the order of a reset.
actions() {
  awk -F'\t' '
    $1 == "docker" && $3 ~ /^rm -f / { n = split($3, a, " "); print "stop " a[n]; next }
    $1 == "docker" && $3 ~ /^run / {
      if (match($3, /--name [^ ]+/)) print "start " substr($3, RSTART + 7, RLENGTH - 7)
      next
    }
    $1 == "psql" && $2 ~ /^drop database/ { print "drop"; next }
    $1 == "psql" && $2 ~ /^create database/ { print "create"; next }
    $1 == "ssh" && $3 ~ /rm -rf --/ { print "wipe-jetstream"; next }
    $1 == "curl" && $2 ~ /healthz/ { print "healthz"; next }
  ' "$fake/actions.log"
}

seeded_run() {
  printf '{"finished": "2026-11-03T00:00:00Z"}\n' >"$(run_dir)/seed.json"
  : >"$fake/db/dispatch_seed"
}

# ---- (a) every subcommand refuses a missing or unreadable --config --------------------------------

reset_fake
for sub in up check seed reset step series collect down; do
  run_vm "$sub"
  ((RC != 0)) || fail "vm.sh $sub without --config exited 0"
  grep -q -- '--config is required' "$tmp/out" || fail "vm.sh $sub without --config did not name --config"
done
[ ! -s "$fake/actions.log" ] || fail "a subcommand without --config reached a VM"
pass "every subcommand refuses a missing --config before it reaches a VM"

printf 'not json\n' >"$tmp/bad.json"
run_vm up --config "$tmp/bad.json"
((RC != 0)) || fail "up accepted a --config that is not JSON"
grep -q 'is not readable JSON' "$tmp/out" || fail "up's refusal of a bad --config does not say why"
run_vm up --config "$tmp/nowhere.json"
((RC != 0)) || fail "up accepted a --config that is not a file"
grep -q 'which is not a file' "$tmp/out" || fail "up's refusal of a missing --config file does not say why"
[ ! -s "$fake/actions.log" ] || fail "up reached a VM before refusing its --config"
pass "up refuses a --config that is not a readable JSON file, before it reaches a VM"

# ---- (b) up starts every container to the specification -------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed: see the output above"

line=$(docker_run_line dispatch)
for want in "--cpus 16" "--memory 32g" "--network host" "--storage-opt size=8G" "$dispatch_image"; do
  [[ $line == *"$want"* ]] || fail "up's dispatch container has no '$want': $line"
done
pass "up starts Dispatch at 16 CPU, 32 GiB, 8 GiB ephemeral, host networking and its image digest"

line=$(docker_run_line auth-proxy)
[[ $line == *"--cpus 0.2"* && $line == *"--memory 256m"* && $line == *"$proxy_image"* ]] ||
  fail "up's auth-proxy container is not 200m / 256 MiB on its image digest: $line"
[[ $line == *"--network host"* ]] && fail "up gave the auth proxy host networking"
pass "up starts the Auth Proxy at 200m and 256 MiB on its image digest"

for role in dispatch-listener load-listener-1 load-listener-2 load-listener-3 load-listener-4; do
  line=$(docker_run_line "$role")
  [[ $line == *"--cpus 0.5"* && $line == *"--memory 512m"* && $line == *"$listener_image"* ]] ||
    fail "up's $role container is not 500m / 512 MiB on the listener digest: $line"
done
pass "up starts the Dispatch listener and the four load listeners at 500m and 512 MiB"

for role in nats hub-listener; do
  line=$(docker_run_line "$role")
  [[ $line == *"--cpus"* ]] && fail "up gave $role a CPU limit, which the specification does not: $line"
  [[ $line == *"--memory"* ]] && fail "up gave $role a memory limit, which the specification does not: $line"
done
line=$(docker_run_line nats)
[[ $line == *"-js -sd /data"* ]] || fail "up's NATS runs without JetStream on the VM's store: $line"
[[ $line == *"--volume $remote/app/nats:/data"* ]] || fail "up's NATS store is not the VM's disk: $line"
pass "up starts NATS with JetStream on the VM's disk and the hub listener, neither with a limit"

[ "$(grep -c -- ' run --name ' "$fake/docker.log")" = 9 ] ||
  fail "up started $(grep -c -- ' run --name ' "$fake/docker.log") containers, not the 9 of the specification"
grep -q -- '@sha256:' <<<"$(grep -- ' run --name ' "$fake/docker.log")" ||
  fail "up started a container by something other than a digest"
grep -- ' run --name ' "$fake/docker.log" | grep -qv '@sha256:' &&
  fail "up started a container whose image is not a digest"
pass "up starts the nine containers of the specification, each by image digest"

order=$(actions | grep '^start ' | sed 's/^start //')
[ "$order" = "$(printf 'nats\nauth-proxy\nhub-listener\nload-listener-1\nload-listener-2\nload-listener-3\nload-listener-4\ndispatch-listener\ndispatch\n')" ] ||
  fail "up started the containers in the order: $order"
grep -q 'stub-embed' "$fake/loadgen.log" || fail "up did not start the stub embedder"
stub_at=$(grep -n 'loadgen	stub-embed' "$fake/actions.log" | head -n1 | cut -d: -f1)
dispatch_at=$(grep -n 'docker	.*run --name dispatch ' "$fake/actions.log" | head -n1 | cut -d: -f1)
[ -n "$stub_at" ] && [ -n "$dispatch_at" ] && ((stub_at < dispatch_at)) ||
  fail "up started Dispatch before the stub embedder it reads a certificate from"
[ -f "$remote/app/stub-embed.pem" ] || fail "up left no stub embedder certificate on the application VM"
grep -q 'curl	.*healthz' "$fake/actions.log" || fail "up did not wait on Dispatch's /healthz"
pass "up starts the stub embedder before Dispatch, copies its certificate to the application VM and waits on /healthz"

# ---- (c) up refuses an image named by tag ---------------------------------------------------------

reset_fake
jq '.images.dispatch = "europe-docker.pkg.dev/p/r/envoy:latest"' "$config" >"$tmp/tagged.json"
run_vm up --config "$tmp/tagged.json"
((RC != 0)) || fail "up accepted an image named by tag"
grep -q 'never by a tag' "$tmp/out" || fail "up's refusal of a tag does not say why"
pass "up refuses an image named by a tag rather than a digest"

# ---- (d) and (e) check ----------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the check cases"
run_vm check --config "$config"
((RC == 0)) || fail "check failed on a deployment that matches the specification"
grep -q '^| check | expected | found | ok |$' "$tmp/out" || fail "check printed no table header"
grep -q '^| dispatch name cpu memory image (app VM) | /dispatch 16000000000 34359738368 .* | /dispatch 16000000000 34359738368 .* | yes |$' "$tmp/out" ||
  fail "check does not print Dispatch's expected CPU and memory beside what it found"
grep -q '^| dispatch network mode and ephemeral storage | host 8G | host 8G | yes |$' "$tmp/out" ||
  fail "check does not read back Dispatch's host networking and ephemeral limit"
grep -q '^| cloudsql databaseVersion | POSTGRES_18 | POSTGRES_18 | yes |$' "$tmp/out" ||
  fail "check does not read the instance's own settings from the Cloud SQL Admin API"
grep -q '^| cloudsql settings.tier | db-custom-16-61440 | db-custom-16-61440 | yes |$' "$tmp/out" ||
  fail "check does not read the instance's tier"
grep -q '^| app VM scheduling.maxRunDuration | 259200s | 259200s | yes |$' "$tmp/out" ||
  fail "check does not read each VM's run-time limit from the Compute API"
grep -q '^| app VM to production 10.139.0.2:5432 | refused | refused | yes |$' "$tmp/out" ||
  fail "check does not prove each production address refused from each VM"
grep -q '^| generator VM to production 10.138.0.10:4222 | refused | refused | yes |$' "$tmp/out" ||
  fail "check does not try every production address from the generator VM"
grep -q '^| dispatch env DISPATCH_IDENTITY | DISPATCH_IDENTITY=header:X-Dispatch-User | DISPATCH_IDENTITY=header:X-Dispatch-User | yes |$' "$tmp/out" ||
  fail "check does not read back Dispatch's identity setting"
grep -q '^| dispatch env DISPATCH_AZURE_EMBED_ENDPOINT | DISPATCH_AZURE_EMBED_ENDPOINT=https://10.138.0.22:8443 | .* | yes |$' "$tmp/out" ||
  fail "check does not read back Dispatch's embedder endpoint"
grep -q '^| dispatch env DISPATCH_AGENT_TOKEN | set | set | yes |$' "$tmp/out" ||
  fail "check does not prove the agent token set"
grep -q 'dispatch-secret\|listener-secret\|embed-secret\|pg-secret' "$tmp/out" &&
  fail "check printed a credential in its table"
pass "check prints expected beside found for the containers, the instance, the run-time limit, the endpoints and the production addresses, and prints no credential"

printf '/dispatch 16000000000 17179869184 %s\n' "$dispatch_image" >"$fake/docker/lt@10.138.0.21/dispatch.main"
run_vm check --config "$config"
((RC != 0)) || fail "check exited 0 with Dispatch's memory at 16 GiB rather than 32"
grep -q '^| dispatch name cpu memory image (app VM) | /dispatch 16000000000 34359738368 .* | /dispatch 16000000000 17179869184 .* | no |$' "$tmp/out" ||
  fail "check does not show the memory it expected beside the one it found"
grep -q 'of the readings differ' "$tmp/out" || fail "check does not say how many readings differ"
pass "check exits non-zero and names the row when a container's memory differs"

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the production-address case"
printf '0\n' >"$fake/nc/10.139.0.2-5432"
run_vm check --config "$config"
((RC != 0)) || fail "check exited 0 while a production address answered from a VM"
grep -q '^| app VM to production 10.139.0.2:5432 | refused | answered | no |$' "$tmp/out" ||
  fail "check does not show a production address that answered"
pass "check exits non-zero when a production address answers from a VM"

rm -f "$fake/nc/10.139.0.2-5432"
jq '.scheduling.maxRunDuration = "86400s"' "$fake/compute-lt-app.json" >"$tmp/j" && mv "$tmp/j" "$fake/compute-lt-app.json"
run_vm check --config "$config"
((RC != 0)) || fail "check exited 0 with a run-time limit that is not 72 hours"
grep -q '^| app VM scheduling.maxRunDuration | 259200s | 86400s | no |$' "$tmp/out" ||
  fail "check does not show the run-time limit it expected beside the one it found"
pass "check exits non-zero when a VM's maxRunDuration is not 259200s"

# ---- (f) reset ------------------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the reset case"
seeded_run
: >"$fake/actions.log"
run_vm reset --config "$config"
((RC == 0)) || fail "reset failed: see the output above"
want=$(
  printf 'stop %s\n' dispatch dispatch-listener load-listener-1 load-listener-2 load-listener-3 load-listener-4 hub-listener
  printf 'drop\ncreate\n'
  printf 'stop nats\nwipe-jetstream\nstop nats\nstart nats\n'
  printf 'stop hub-listener\nstart hub-listener\n'
  printf 'stop load-listener-%s\nstart load-listener-%s\n' 1 1 2 2 3 3 4 4
  printf 'stop dispatch-listener\nstart dispatch-listener\n'
  printf 'stop dispatch\nstart dispatch\n'
  printf 'healthz\n'
)
got=$(actions)
[ "$got" = "$want" ] || fail "reset's steps are not decision D6's, in order. Got:
$got
Want:
$want"
pass "reset stops Dispatch and the listeners, drops and makes the database from the template, restarts NATS on an emptied JetStream store, then the four load listeners, the Dispatch listener and Dispatch, and waits on /healthz, in that order"

record=$(ls "$(run_dir)"/logs/reset-*.json | tail -n1)
[ "$(jq -r '.steps | length' "$record")" -ge 14 ] || fail "reset's record does not list its steps"
jq -e '.seconds >= 0 and (.started | length) > 0 and (.ended | length) > 0' "$record" >/dev/null ||
  fail "reset's record holds no duration: $(cat "$record")"
[ "$(jq -r .template "$record")" = dispatch_seed ] || fail "reset's record does not name the template"
pass "reset records its duration and its steps"

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the template case"
printf '{"finished": "x"}\n' >"$(run_dir)/seed.json"
run_vm reset --config "$config"
((RC != 0)) || fail "reset ran without the template database"
grep -q 'has no template database dispatch_seed' "$tmp/out" || fail "reset's refusal does not name the template"
pass "reset refuses an instance that holds no template database"

# ---- (g) and (h) step -----------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the step case"
seeded_run
run_vm step --config "$config" 2x 2 --duration 10m
((RC == 0)) || fail "step failed: see the output above"
line=$(tail -n1 "$fake/loadgen-run.log")
for want in \
  "-loop-seed $remote/work/loopseed.seed.json" \
  "-loop-factor 2" \
  "-generator-role vm-10.138.0.22" \
  "-unit-starts 3.5" \
  "-unit-life 5m" \
  "-loop-listeners http://10.138.0.22:9021,http://10.138.0.22:9022,http://10.138.0.22:9023,http://10.138.0.22:9024" \
  "-driver-topics 614" \
  "-receipt-deadline 1s" \
  "-route-mix routes-spec-loop-2026-11-03.txt" \
  "-event-mix spec-loop-2026-11-03" \
  "-events 11" \
  "-requests 137" \
  "-sessions 400" \
  "-duration 10m" \
  "-allow-foreign"; do
  [[ $line == *"$want"* ]] || fail "step's loadgen run has no '$want': $line"
done
pass "step runs loadgen run with the loop's flags and the rates its factor gives"

step=$(run_dir)/steps/2x
[ -f "$step/cpu.pprof" ] || fail "step fetched no CPU profile"
grep -q 'ssh	lt@10.138.0.21	curl .*127.0.0.1:6060/debug/pprof/profile?seconds=600' "$fake/actions.log" ||
  fail "step did not fetch the profile from the application VM's own loopback"
for file in goroutines.txt heap.pprof heap.txt; do
  [ -f "$step/$file" ] || fail "step fetched no $file from the application VM"
done
[ -f "$step/cgroup.json" ] || fail "step ran no loadgen cgroup-merge"
[ -f "$step/cloudsql-cpu.json" ] || fail "step ran no loadgen cloudsql-cpu"
[ -f "$step/TABLES.md" ] || fail "step ran no loadgen tables"
[ -f "$step/summary.json" ] || fail "step copied no summary.json back from the generator VM"
[ -f "$step/reset.json" ] || fail "step recorded no reset"
grep -q 'cloudsql-cpu.*-instance lt-db' "$fake/loadgen.log" || fail "step's Cloud SQL read does not name the instance"
grep -q -- '-vm lt@10.138.0.21:nats=.*,dispatch=' "$fake/vm-sample.log" || fail "step's sampler does not read the application VM's containers at their cgroups"
grep -q -- '-vm lt@10.138.0.22:load-listener-1=' "$fake/vm-sample.log" || fail "step's sampler does not read the generator VM's containers at their cgroups"
pass "step samples both VMs, fetches the profile from the application VM and runs the merge, the Cloud SQL read and the tables"

run_vm step --config "$config" baseline 0 --duration 2m
((RC == 0)) || fail "the baseline step failed: see the output above"
line=$(tail -n1 "$fake/loadgen-run.log")
for want in "-loop-factor 0" "-unit-starts 0" "-events 4" "-requests 60" "-event-mix production-2026-10-26"; do
  [[ $line == *"$want"* ]] || fail "the baseline step's loadgen run has no '$want': $line"
done
[[ $line == *"-route-mix"* ]] && fail "the baseline step named a route mix; it takes loadgen's committed one: $line"
pass "a step at factor 0 is the baseline alone, at the baseline's own rates and mixes"

# ---- (i) series -----------------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the series case"
seeded_run
printf 'false\n' >"$fake/pass/3x"
: >"$fake/refine.1"
printf '3.5\n' >"$fake/refine.2"
: >"$fake/refine.3"
run_vm series --config "$config" --duration 10m --long 60m --long-warmup 5m
((RC == 0)) || fail "series failed: see the output above"
for name in baseline 1x 2x 3x 3.5x; do
  [ -d "$(run_dir)/steps/$name" ] || fail "series ran no step $name"
done
for name in 4x 6x 8x 12x 16x 20x; do
  [ ! -e "$(run_dir)/steps/$name" ] || fail "series ran step $name after the failing step 3x"
done
grep -q 'step 3x is the first failing step' "$tmp/out" || fail "series does not name the first failing step"
grep -q 'refine.txt names the factors 3.5' "$tmp/out" || fail "series does not follow refine.txt"
grep -q 'refinement done: refine.txt is empty' "$tmp/out" || fail "series does not stop when refine.txt is empty"
grep -- '-duration 60m' "$fake/loadgen-run.log" | grep -q -- '-warmup 5m' ||
  fail "series does not run the long steps at 60 minutes after a 5-minute warm-up"
[ "$(grep -c -- '-duration 60m' "$fake/loadgen-run.log")" = 3 ] ||
  fail "series ran $(grep -c -- '-duration 60m' "$fake/loadgen-run.log") long steps, not the baseline, 1x and 2x"
pass "series runs the baseline, 1x and 2x long, stops the grid at the first failing step, and follows refine.txt until it is empty"

# ---- (j) a stopped VM discards the step -------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the stopped-VM case"
seeded_run
# The step's first reading of both VMs answers RUNNING; every reading after it answers
# TERMINATED, as a VM stopped at its 72-hour run-time limit does.
rm -f "$fake/compute-reads"
printf '2\n' >"$fake/compute-stop-after"
run_vm step --config "$config" 1x 1 --duration 10m
[ "$RC" = 3 ] || fail "a step whose VM stopped while it ran exited $RC, not 3"
[ ! -e "$(run_dir)/steps/1x" ] || fail "a step whose VM stopped was recorded rather than discarded"
grep -q 'step 1x is discarded' "$tmp/out" || fail "a discarded step does not say so"
grep -q 'the app VM lt-app is TERMINATED' "$tmp/out" || fail "a discarded step does not name the VM that stopped"
grep -q 'not a Dispatch failure' "$tmp/out" || fail "a discarded step reads as a Dispatch failure"
pass "a step whose VM stopped while it ran is discarded, naming the VM, and is no Dispatch failure"

rm -f "$fake/compute-reads" "$fake/compute-stop-after"
jq '.status = "TERMINATED"' "$fake/compute-lt-gen.json" >"$tmp/j" && mv "$tmp/j" "$fake/compute-lt-gen.json"
run_vm step --config "$config" 1x 1 --duration 10m
[ "$RC" = 3 ] || fail "a step whose VM is already stopped exited $RC, not 3"
[ ! -e "$(run_dir)/steps/1x" ] || fail "a step whose VM is already stopped left a directory"
grep -q 'discarded before it started' "$tmp/out" || fail "a step refused before it started does not say so"
run_vm series --config "$config" --duration 10m --long 60m --long-warmup 5m
[ "$RC" = 3 ] || fail "series over a stopped VM exited $RC, not 3"
grep -q 'because a VM is not running' "$tmp/out" || fail "series does not say that a VM stopped"
pass "a step whose VM is already stopped is discarded before it starts, and series stops on it"

# ---- (k) seed ---------------------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the seed case"
# The database Dispatch made and migrated when up started it: the seed writes into it and the
# last phase renames it to the template.
: >"$fake/db/dispatch"
run_vm seed --config "$config" --embed-timeout 8h
((RC == 0)) || fail "seed failed: see the output above"
record=$(run_dir)/seed.json
[ -f "$record" ] || fail "seed wrote no $record"
phases=$(jq -r '.phases[].phase' "$record" | tr '\n' ' ')
[ "$phases" = "plan push-plan write pull-meta check export push-seed finish template " ] ||
  fail "seed's phases are '$phases'"
jq -e '[.phases[] | select(.seconds >= 0 and (.started | length) > 0)] | length == 9' "$record" >/dev/null ||
  fail "seed did not record each phase's wall clock: $(jq -c .phases "$record")"
jq -e '.embed_progress.lacking == 0' "$record" >/dev/null ||
  fail "seed recorded no coverage progress: $(jq -c . "$record")"
grep -q -- '-url http://10.138.0.21:8766' "$fake/seed-finish.log" ||
  fail "seed-finish ran without the embedding gate's -url: $(cat "$fake/seed-finish.log")"
grep -q -- '-embed-timeout 8h' "$fake/seed-finish.log" || fail "seed-finish ran without the gate's bound"
grep -q -- '-embed-progress' "$fake/seed-finish.log" || fail "seed-finish recorded no coverage progress"
grep -q "ssh	lt@10.138.0.22	$remote/app/loadgen loop-seed-write" "$fake/actions.log" ||
  fail "seed did not write the loop seed from the generator VM"
[ -e "$fake/db/dispatch_seed" ] || fail "seed left no template database"
pass "seed plans, writes on the generator VM, checks, exports and finishes with the embedding gate, recording each phase's wall clock and the coverage progress"

run_vm seed --config "$config"
((RC != 0)) || fail "seed ran twice over one run"
grep -q 'is seeded already' "$tmp/out" || fail "a second seed does not say why it is refused"
pass "seed refuses a run that is seeded already"

# ---- (l) collect and down ---------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the collect case"
seeded_run
run_vm step --config "$config" 1x 1 --duration 10m
((RC == 0)) || fail "the step before collect failed"
run_vm collect --config "$config"
((RC == 0)) || fail "collect failed: see the output above"
[ -f "$(run_dir)/curves/curves.csv" ] || fail "collect wrote no curves.csv"
grep -q 'curves -series loop -out' "$fake/loadgen.log" || fail "collect did not run loadgen curves for the loop series"
pass "collect runs loadgen curves -series loop over the run's steps into the run directory"

: >"$fake/actions.log"
run_vm down --config "$config"
((RC == 0)) || fail "down failed: see the output above"
stopped=$(actions | grep '^stop ' | sed 's/^stop //' | sort -u | tr '\n' ' ')
[ "$stopped" = "auth-proxy dispatch dispatch-listener hub-listener load-listener-1 load-listener-2 load-listener-3 load-listener-4 nats " ] ||
  fail "down stopped '$stopped', not every container"
actions | grep -q '^start ' && fail "down started something"
grep -q 'left as they are' "$tmp/out" || fail "down does not say that the VMs and the instance are left alone"
pass "down stops every container and leaves the VMs and the Cloud SQL instance alone"

# ---- (m) the step's profiling tunnel ---------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the tunnel cases"
seeded_run
run_vm step --config "$config" 1x 1 --duration 10m
((RC == 0)) || fail "the step of the tunnel cases failed: see the output above"

# The tunnel forwards a port of the generator VM, where loadgen run runs, to the profiling port
# on the application VM's loopback, which is the one place that port answers.
grep -q -- "^-L $tunnel_port:127.0.0.1:6060 lt@10.138.0.21$" "$fake/tunnel.log" ||
  fail "the step's tunnel is not '-L $tunnel_port:127.0.0.1:6060 lt@10.138.0.21': $(cat "$fake/tunnel.log")"
tunnel_at=$(grep -n '^tunnel	' "$fake/actions.log" | head -n1 | cut -d: -f1)
probe_at=$(grep -n "^curl	http://127.0.0.1:$tunnel_port/debug/pprof/goroutine?debug=1$" "$fake/actions.log" | head -n1 | cut -d: -f1)
run_at=$(grep -n '^loadgen	run ' "$fake/actions.log" | head -n1 | cut -d: -f1)
[ -n "$tunnel_at" ] || fail "step opened no profiling tunnel"
[ -n "$probe_at" ] || fail "step did not prove the tunnel with a read of /debug/pprof/goroutine?debug=1 through it"
[ -n "$run_at" ] && ((tunnel_at < probe_at)) && ((probe_at < run_at)) ||
  fail "step did not open the tunnel ($tunnel_at) and prove it ($probe_at) before loadgen run (${run_at:-none})"
line=$(tail -n1 "$fake/loadgen-run.log")
[[ $line == *"-pprof http://127.0.0.1:$tunnel_port"* ]] ||
  fail "step's loadgen run does not read the profiling port through the tunnel: $line"
[[ $line == *"-pprof http://10.138.0.21"* ]] &&
  fail "step gave loadgen an address of the application VM, which its sampler cannot reach: $line"
pass "step opens the profiling tunnel on the generator VM and proves it with a read of /debug/pprof/goroutine?debug=1 before loadgen run, which reads the profiling port through it"

tunnel_pid=$(awk '{print $1}' "$fake/tunnel-up")
[ -n "$tunnel_pid" ] || fail "the fake tunnel recorded no pid"
kill -0 "$tunnel_pid" 2>/dev/null &&
  fail "the step left its profiling tunnel running on the generator VM (pid $tunnel_pid)"
[ ! -e "$remote/work/pprof-tunnel.pid" ] || fail "the step left its tunnel's pid file on the generator VM"
pass "the step's profiling tunnel is closed when the step ends, leaving no process and no pid file on the generator VM"

# The local port is the deployment file's, and 16060 when it names none.
jq 'del(.ports.pprof_tunnel)' "$config" >"$tmp/default-tunnel.json"
run_vm step --config "$tmp/default-tunnel.json" 3x 3 --duration 10m
((RC == 0)) || fail "a step on a deployment file with no ports.pprof_tunnel failed: see the output above"
grep -q -- '^-L 16060:127.0.0.1:6060 lt@10.138.0.21$' "$fake/tunnel.log" ||
  fail "a deployment file with no ports.pprof_tunnel did not take the default port: $(cat "$fake/tunnel.log")"
[[ $(tail -n1 "$fake/loadgen-run.log") == *"-pprof http://127.0.0.1:16060"* ]] ||
  fail "a step on the default tunnel port did not give loadgen that port: $(tail -n1 "$fake/loadgen-run.log")"
pass "the tunnel's local port comes from the deployment file, and 16060 is its default"

# A tunnel that cannot be opened refuses the step by name, before any load: a step whose sampler
# cannot read the profiling port can never be judged on pass criterion (e).
: >"$fake/tunnel-refuses"
runs_before=$(wc -l <"$fake/loadgen-run.log")
run_vm step --config "$config" 2x 2 --duration 10m
((RC != 0)) || fail "step ran with a profiling tunnel that does not answer"
grep -q 'step 2x is refused' "$tmp/out" || fail "the refusal does not name the step"
grep -q 'pool acquire waiters p50 = 0' "$tmp/out" ||
  fail "the refusal does not say which pass criterion cannot be measured without the tunnel"
[ "$(wc -l <"$fake/loadgen-run.log")" = "$runs_before" ] ||
  fail "a step whose tunnel does not answer sent its load all the same"
[ ! -e "$(run_dir)/steps/2x" ] ||
  fail "a refused step left its directory, so it cannot run again under its own name"
rm -f "$fake/tunnel-refuses"
pass "a step whose profiling tunnel does not answer is refused by name, before any load, and leaves no directory behind"

# ---- (n) the remote root is a directory loadgen can be executed from -----------------------------

# The VMs are Container-Optimized OS, where /var is mounted noexec: without this mount up copies
# loadgen where it cannot be run and every step fails on its first exec (dispatch://INFRA-49).
reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed: see the output above"
for target in lt@10.138.0.21 lt@10.138.0.22; do
  sudo_log "$target" | grep -qxF -- "mkdir -p -- /mnt/stateful_partition/app" ||
    fail "up did not make the backing directory under the stateful partition with sudo on $target: $(sudo_log "$target")"
  sudo_log "$target" | grep -qxF -- "mount --bind -- /mnt/stateful_partition/app $remote/app" ||
    fail "up did not bind-mount the backing directory at the remote root on $target: $(sudo_log "$target")"
  sudo_log "$target" | grep -qxF -- "mount -o remount,bind,exec,nosuid,nodev -- /mnt/stateful_partition/app $remote/app" ||
    fail "up did not remount the remote root exec,nosuid,nodev on $target: $(sudo_log "$target")"
  sudo_log "$target" | grep -qxF -- "chown -- $(id -un) $remote/app" ||
    fail "up did not give the remote root to the ssh user on $target: $(sudo_log "$target")"
done
[ -d "$fake/stateful/app" ] || fail "up made no backing directory under the stateful partition"
pass "up makes the backing directory under the stateful partition with sudo, bind-mounts it at the remote root, remounts it exec,nosuid,nodev and gives it to the ssh user, on both VMs"

mount_at=$(line_at 'chown -- ') copy_at=$(line_at "$remote/app/loadgen") start_at=$(line_at 'run --name ')
[ -n "$mount_at" ] && [ -n "$copy_at" ] && [ -n "$start_at" ] && ((mount_at < copy_at)) && ((copy_at < start_at)) ||
  fail "up did not mount both remote roots (through line ${mount_at:-none}) before the loadgen copy (${copy_at:-none}) and the first container (${start_at:-none})"
pass "up mounts both remote roots before it copies loadgen onto the generator VM and before any container starts"

# A second up over VMs that already carry the mount remounts it and stacks no second mount.
: >"$fake/actions.log"
run_vm up --config "$config"
((RC == 0)) || fail "a second up over VMs that already carry the mount failed: see the output above"
for target in lt@10.138.0.21 lt@10.138.0.22; do
  sudo_log "$target" | grep -q -- '--bind --' &&
    fail "a second up mounted the remote root again on $target, stacking a second mount on it"
  sudo_log "$target" | grep -qxF -- "mount -o remount,bind,exec,nosuid,nodev -- /mnt/stateful_partition/app $remote/app" ||
    fail "a second up did not remount the remote root on $target: $(sudo_log "$target")"
done
[ "$(actions | grep -c '^start ')" = 9 ] ||
  fail "a second up started $(actions | grep -c '^start ') containers, not the 9 of the specification"
pass "a second up over a VM that already carries the mount remounts it, mounts it no second time, and starts the deployment all the same"

# A mount that cannot be made refuses up by name, before a container is started: a generator VM
# that cannot exec loadgen runs no step at all.
reset_fake
: >"$fake/mount-refuses"
run_vm up --config "$config"
((RC != 0)) || fail "up ran over a VM whose remote root cannot be mounted"
grep -q "up: bind-mounting /mnt/stateful_partition/app at $remote/app on the app VM failed" "$tmp/out" ||
  fail "up's refusal does not name the mount it could not make"
[ ! -s "$fake/docker.log" ] || fail "up started a container although the remote root could not be mounted"
rm -f "$fake/mount-refuses"
pass "up is refused by name, with no container started, when the remote root cannot be mounted"

reset_fake
: >"$fake/mount-noexec"
run_vm up --config "$config"
((RC != 0)) || fail "up ran over a VM whose remote root is mounted noexec"
grep -q "up: $remote/app on the app VM is mounted .*noexec" "$tmp/out" ||
  fail "up's refusal does not name the root it read back noexec"
grep -q 'no step of this deployment would run' "$tmp/out" ||
  fail "up's refusal does not say what a noexec remote root costs"
[ ! -s "$fake/docker.log" ] || fail "up started a container although the remote root is noexec"
rm -f "$fake/mount-noexec"
pass "up is refused by name, with no container started, when the remote root is mounted but not exec"

# The backing directory follows the remote root's own last element, so a deployment that moves
# the root does not go on sharing the directory of the one it moved away from.
reset_fake
jq --arg r "$remote/elsewhere" '.paths.remote_root = $r' "$config" >"$tmp/moved.json"
run_vm up --config "$tmp/moved.json"
((RC == 0)) || fail "up failed on a deployment file whose remote root is elsewhere: see the output above"
for target in lt@10.138.0.21 lt@10.138.0.22; do
  sudo_log "$target" | grep -qxF -- "mkdir -p -- /mnt/stateful_partition/elsewhere" ||
    fail "the backing directory does not follow the remote root's last element on $target: $(sudo_log "$target")"
  sudo_log "$target" | grep -qxF -- "mount --bind -- /mnt/stateful_partition/elsewhere $remote/elsewhere" ||
    fail "the moved remote root is not a bind mount of the directory its last element names on $target: $(sudo_log "$target")"
done
pass "the backing directory's name is the remote root's own last element"

reset_fake
jq --arg r "$remote/elsewhere" '.paths.remote_root = $r | .paths.stateful_dir = "loadtest-backing"' "$config" >"$tmp/named.json"
run_vm up --config "$tmp/named.json"
((RC == 0)) || fail "up failed on a deployment file that names its own stateful directory: see the output above"
for target in lt@10.138.0.21 lt@10.138.0.22; do
  sudo_log "$target" | grep -qxF -- "mount --bind -- /mnt/stateful_partition/loadtest-backing $remote/elsewhere" ||
    fail "paths.stateful_dir is not the backing directory on $target: $(sudo_log "$target")"
done
pass "the deployment file names the backing directory under the stateful partition"

# ---- (o) the example deployment file is one vm.sh runs on ---------------------------------------

# deployment.example.json is the file a reader copies. These three cases hold it to vm.sh itself
# and not to a second copy of the schema: the script runs on it, its keys are read out of the
# script's own filters, and nothing of a real deployment is in it.
readonly example=$here/deployment.example.json

# The example with the five remote paths and the loadgen binary pointed at this pod, because
# those alone name a file system the fakes stand in for. Everything else is the example's own --
# the project, the zone, both VMs, the instance and the settings it expects, every image, port,
# secret, load value and production address -- and up and check read it as a reader's own file.
readonly example_local=$tmp/example-local.json
jq --arg root "$remote/example" --arg loadgen "$bin/loadgen" '
  .paths.remote_root = $root
  | .paths.nats_store = ($root + "/nats")
  | .paths.cert = ($root + "/stub-embed.pem")
  | .paths.remote_loadgen = ($root + "/loadgen")
  | .paths.remote_work = ($root + "/work")
  | .paths.loadgen = $loadgen' "$example" >"$example_local"

reset_fake
run_vm up --config "$example_local"
((RC == 0)) || fail "up refused the example deployment file: see the output above"
[ "$(actions | grep -c '^start ')" = 9 ] ||
  fail "up started $(actions | grep -c '^start ') containers from the example, not the 9 of the specification"
pass "vm.sh up runs on deployment.example.json and starts the whole deployment it describes"

run_vm check --config "$example_local"
((RC == 0)) || fail "check refused the deployment the example describes: see the output above"
grep -q '| no |' "$tmp/out" && fail "check read back a value of the example that differs: see the output above"
grep -q 'the deployment file' "$tmp/out" && fail "check refused a value of the example: see the output above"
pass "vm.sh check reads every value of deployment.example.json back and refuses none of them"

# ---- (p) the example carries the keys vm.sh reads, and only those --------------------------------

# container_field N: field N of every line of vm.sh's own container table, once each. Field 2
# names the VMs its .vms.<vm>.* filters are read for and field 3 the keys of .images.<key>.
container_field() {
  bash -c 'source "$1"; printf "%s\n" "${CONTAINERS[@]}"' _ "$vm" | awk -v n="$1" '{print $n}' | sort -u
}

# script_keys: every key of the deployment file vm.sh reads, one "KEY<tab>required" or
# "KEY<tab>optional" a line, read out of the script's own text and not written here again: each
# cfg and cfg_valid call gives its filter and, by its second argument, whether the key has a
# default; the two objects it reads with jq give theirs by their "// {}"; the one array it reads
# element by element gives its own by "// empty". A filter spelled with a parameter is expanded
# over the arguments the script passes it -- the VMs and image keys of its container table, the
# ports it asks port_of for, the values it asks load_value for -- and a parameter of a shape not
# known here stays in the key, so the comparison below names it rather than dropping it.
script_keys() {
  local body vms images ports loads key kind list word expanded
  body=$(grep -v '^[[:space:]]*#' "$vm")
  vms=$(container_field 2) images=$(container_field 3)
  ports=$(grep -oE '\bport_of [a-z_]+' <<<"$body" | awk '{print $2}' | sort -u)
  loads=$(grep -oE '\bload_value [a-z_.]+' <<<"$body" | awk '{print $2}' | sort -u)
  {
    grep -oE '\bcfg(_valid)? [^)|&;]*' <<<"$body" |
      awk '{ if ($1 == "cfg_valid") { f = $3; n = 4 } else { f = $2; n = 3 }
             gsub(/"/, "", f)
             if (f !~ /^\./) next
             printf "%s\t%s\n", substr(f, 2), (NF >= n ? "optional" : "required") }'
    grep -oE '\(\.[A-Za-z_.]+ // [^)]*\)' <<<"$body" | sed -E 's/^\(\.([A-Za-z_.]+) .*/\1\toptional/'
    grep -oE '"\.[A-Za-z_.]+\[[^"]*\] // empty"' <<<"$body" | sed -E 's/^"\.([A-Za-z_.]+)\[.*/\1\trequired/'
  } | while IFS=$'\t' read -r key kind; do
    case $key in
      vms.*) list=$vms ;;
      images.*) list=$images ;;
      'ports.$1') list=$ports ;;
      'load.$1') list=$loads ;;
      *) printf '%s\t%s\n' "$key" "$kind"; continue ;;
    esac
    for word in $list; do
      expanded=${key//'$1'/$word}
      printf '%s\t%s\n' "${expanded//'$key'/$word}" "$kind"
    done
  done | awk -F'\t' '{ if (($1 in kind) && kind[$1] != $2) kind[$1] = "required"; else kind[$1] = $2 }
                     END { for (k in kind) printf "%s\t%s\n", k, kind[k] }' | sort
}

declare -A KEY_KIND=()
while IFS=$'\t' read -r key kind; do
  KEY_KIND[$key]=$kind
done < <(script_keys)
((${#KEY_KIND[@]} > 40)) ||
  fail "only ${#KEY_KIND[@]} keys were read out of vm.sh's own filters, so this case is reading the script wrong"

# example_key LEAF: the key of the deployment file that LEAF is a value of -- itself, or the
# object or array above it that vm.sh reads whole: cloudsql.expect's Admin API paths, the ports
# of ports.load_listeners, the addresses of production_addresses. Empty when no key vm.sh reads
# holds it.
example_key() {
  local k=$1
  while :; do
    [ -z "${KEY_KIND[$k]+set}" ] || { printf '%s\n' "$k"; return 0; }
    [[ $k == *.* ]] || return 0
    k=${k%.*}
  done
}

declare -A CARRIED=()
while read -r leaf; do
  [ -n "$leaf" ] || continue
  key=$(example_key "$leaf")
  [ -n "$key" ] ||
    fail "deployment.example.json carries $leaf, which vm.sh never reads: drop it, or name it in the script"
  CARRIED[$key]=1
done < <(jq -r 'paths(scalars) | map(tostring) | join(".")' "$example")
pass "every key deployment.example.json carries is one vm.sh reads"

missing=""
for key in "${!KEY_KIND[@]}"; do
  [ -z "${CARRIED[$key]+set}" ] || continue
  missing+=" $key (${KEY_KIND[$key]})"
done
[ -z "$missing" ] ||
  fail "vm.sh reads these keys and deployment.example.json carries none of them:$missing"
pass "deployment.example.json carries every key vm.sh reads, the optional ones at their own defaults"

# ---- (q) the example names nothing of a real deployment ------------------------------------------

jq -e . "$example" >/dev/null || fail "deployment.example.json is not valid JSON"

# This repository is public, so every machine the example names is a placeholder: an address in
# RFC 1918 or on the loopback, a host that is one of the public Google endpoints vm.sh defaults
# to, an instance of the lt- register, an image digest that is one character repeated, and a
# credential that is the placeholder "...".
while read -r address; do
  [[ $address =~ ^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|127\.) ]] ||
    fail "deployment.example.json names the address $address, which is neither RFC 1918 nor loopback"
done < <(grep -oE '[0-9]+(\.[0-9]+){3}' "$example")

while read -r host; do
  case $host in
    10.* | 127.* | metadata.google.internal | sqladmin.googleapis.com | compute.googleapis.com) ;;
    *) fail "deployment.example.json reaches the host $host, which is no placeholder and no public Google endpoint" ;;
  esac
done < <(grep -oE '"[a-z]+://[^/"]+' "$example" | sed -E 's|^.*://||; s|^.*@||; s|:[0-9]+$||')

while read -r instance; do
  [[ $instance =~ ^lt-[a-z0-9-]+$ ]] ||
    fail "deployment.example.json names the instance $instance, which is not of the placeholder lt- register"
done < <(jq -r '(.vms[].instance, .cloudsql.instance)' "$example")

while read -r image; do
  digest=${image##*@sha256:}
  [ "${#digest}" = 64 ] && [ -z "${digest//${digest:0:1}/}" ] ||
    fail "deployment.example.json gives the image $image, whose digest is not one repeated character, so it reads as a real one"
done < <(jq -r '.images[]' "$example")

while read -r secret; do
  [ "$secret" = "..." ] ||
    fail "deployment.example.json holds '$secret' where a credential goes; the placeholder is \"...\""
done < <(jq -r '(.secrets[], .cloudsql.password)' "$example")
pass "deployment.example.json is valid JSON and names no routable address, no real machine and no credential"

printf 'PASS: packages/envoy/loadtest/vm/vm.test.sh\n'
