#!/usr/bin/env bash
# packages/envoy/loadtest/vm/vm.test.sh
#
# Proves vm.sh's behaviour without the deployment it drives: no VM, no Cloud SQL instance and no
# network. cluster/cluster.test.sh is the model -- a $bin directory first on PATH holding one
# small script per external program, each recording its argv and answering from $FAKE_DIR.
#
# The fakes: `ssh` records the call, parses what follows the target the way the machine's login
# shell would -- one word, which is all ssh ever sends -- and runs it here, so the deployment's
# machines are this test's own directories and every program the driver runs on a machine
# (docker, nc, curl, tar, sh, loadgen) is the fake or the real one on this PATH; `docker` keeps
# a container registry per ssh target, computing from each `run` the NanoCpus, Memory, network
# mode, entrypoint and state that docker itself would record, which its `inspect` and `logs`
# then answer, so a reading the test wants to differ is one file to rewrite; `curl` answers the
# metadata server, the token mint, the OS Login key import and login profile, the Cloud SQL
# Admin API, the Compute API, Dispatch's /healthz, the profiling port and, only while the fake
# tunnel of that port runs, a read through the step's profiling tunnel; `psql` keeps a database
# list and makes the database a `create database` names; `nc` refuses every address unless the
# test says otherwise; `loadgen` records its argv, writes the files the next step reads, and as
# the stub embedder holds a port, which a second stub on it is refused; `pprof-tunnel` is the
# deployment file's pprof_tunnel_command, which stands for the `ssh -N -L ...` a step runs on
# the load machine. `jq`, `ssh-keygen` and `sudo`'s own commands are the real ones.
#
# Asserts that: (a) every subcommand refuses a missing --config, and refuses one that is not
# readable JSON, before it reaches a VM; (b) up starts every container of the specification, in
# order, each with exactly the CPU, memory and image arguments the table names, Dispatch with
# host networking and the entrypoint its image needs, each image by digest and none by tag, the
# stub embedder before Dispatch, and waits on /healthz; (c) up refuses an image named by tag;
# (d) check prints expected beside found for every value and exits 0 when they agree, and
# non-zero when a container's memory differs, when a production address answers from a VM, and
# when a VM's maxRunDuration is not 259200 s, naming each; (e) check reads the instance's own
# settings from the Cloud SQL Admin API and each container's endpoint settings, and never prints
# a credential; (f) reset runs decision D6's steps in order -- the Dispatch and listener
# containers stopped, the database dropped and made again from the template, NATS restarted on
# an emptied JetStream store, the four load listeners and the Dispatch listener, then Dispatch,
# then /healthz -- and records its duration; (g) step passes the loop's flags to loadgen run
# (-loop-seed, -loop-factor, -generator-role, -unit-starts, -unit-life, -loop-listeners,
# -driver-topics, -receipt-deadline, -route-mix and -event-mix), with the rates the factor gives,
# fetches the profile from Dispatch's own machine and runs cgroup-merge, cloudsql-cpu and tables;
# (h) a step at factor 0 is the baseline alone, at the baseline's own rates and mixes; (i) series
# runs the baseline, 1x and 2x at the long duration after the long warm-up, stops the grid at the
# first failing step, and then follows refine.txt until it is empty; (j) a step whose VM stopped
# while it ran is discarded -- its directory removed, the VM named, no failure recorded -- and
# so is one whose VM is already stopped, and series stops on it; (k) seed runs the plan, the
# write on the load machine, the check, the export and seed-finish with the embedding gate, and
# records each phase's wall clock and the gate's progress; (l) down stops every container and
# leaves the machines and the instance alone; (m) step opens the profiling tunnel on the load
# machine and proves it with a read of /debug/pprof/goroutine?debug=1 through it before loadgen
# run, gives loadgen that tunnel as -pprof, takes its local port from the deployment file and
# 16060 when it names none, closes it when the step ends -- no process and no pid file left on
# the load machine -- and refuses the step by name, before any load and leaving no directory,
# when the tunnel does not answer; (n) up gives every machine's remote root a bind mount of a
# directory under the stateful partition, made with sudo, remounted exec,nosuid,nodev and given
# to the ssh user, all of it before the loadgen copy and any container, mounts nothing a second
# time over a machine that already carries the mount, takes the backing directory's name from
# remote_root's own last element unless the deployment file names another, and is refused by
# name, with no container started, when that mount cannot be made or is not exec; (o) up and
# check both run on deployment.example.json, with nothing of it changed but the paths that must
# point at this pod;
# (p) that example carries every key vm.sh reads and no other, both lists read out of the
# script's own cfg, cfg_valid and jq filters rather than written here again; (q) the example is
# valid JSON and names no routable address, no real machine and no credential; (r) the
# deployment file, not the script, says which machine each container runs on -- a three-machine
# deployment starts each container where the file puts it and check reads all three machines
# back, a two-machine deployment that keeps NATS on the application machine works the same way,
# and a container the file places nowhere, a container it does not run, a machine no container
# runs on and a load_vm that is no machine of the file are each refused by name with nothing
# started; (s) every ssh is OS Login's -- the token mint, the key import and the login profile
# are read in that order before the first ssh, every ssh runs as the user that profile named
# with the key the run made, a second up of one run makes no second key, a refusal of any of the
# three refuses up by name with nothing started, and no minted token reaches a command line or
# any file the run keeps; (t) each of the eleven defects that driving this script against
# the deployment itself found -- a remote argument holding a space, a quote or a pipe arriving
# as one argument, the run's own known_hosts under accept-new, put_file's write beside the
# target and rename onto it, a second up that stops the stub embedder the first left holding
# the port, the Dispatch container's entrypoint and the --storage-opt this docker refuses,
# ENVOY_MACHINE_ID and ENVOY_ALLOW_REMOTE_NATS in Dispatch's environment, the database up makes
# when the instance lacks it, what wait_healthz takes for ready and when it refuses at once,
# the run-time limit read out of the object the Compute API answers, and every production
# address tried from every machine; and (u) no credential of the deployment -- Dispatch's agent
# token, the listener's token, the embedder's api-key or the instance's password -- reaches the
# command line of an ssh, a docker, a psql or a loadgen of an up, a seed and a step, or a file
# the run keeps, while each still reaches the command that needs it in a file of mode 600 -- on
# the load machine for the commands that run there, under the run's directory on this pod for
# the seed's own phases, which is removed when the seed ends -- and psql in a passfile, and down
# removes the run's credential files from the load machine, leaving none of them there.
set -euo pipefail
# Nothing here is read from this test's own standard input, and the fake ssh reads a pipe it is
# given as ssh does, so the suite runs with /dev/null under it: whatever the caller's standard
# input is, no ssh of a case waits on it.
exec </dev/null

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
# step ends. Both fake loadgens -- this test's $bin one and the copy up put on the load machine
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
# The fake ssh: records the call and runs the command here, so a machine's file system is this
# test's own directories. vm.sh runs `ssh <options> <user@host> <one word>` and the options
# carry the run's own OS Login key and the run's own known_hosts file, so the target is the
# first argument that is not an option or an option's value; the whole argv goes to ssh.log,
# which is where the OS Login cases read the user, the key and the host-key options back from.
#
# What follows the target is one word, because ssh has no argument vector: it joins what it is
# given with spaces and the machine's login shell splits the result again. This fake does both
# -- it joins the rest with spaces and parses that the way that shell would -- so an argument
# vm.sh quoted with remote_argv arrives here as the one argument it quoted, and a case that
# reads a remote command's arguments reads what the machine would have run. The parsed command
# goes to actions.log, and the target reaches it in FAKE_SSH_TARGET, which the fake docker uses
# to keep a container registry per machine.
#
# ssh also reads its own standard input to the end and sends it to the remote command, whether
# or not that command reads it, which is why an ssh inside a `while read` fed by a pipe
# swallows the rest of that pipe. This fake reads a pipe the same way, so a list streamed into
# a loop around a remote command is lost here as it was lost on the deployment. A file -- what
# put_file gives it -- and the /dev/null this test runs under are handed over untouched, as ssh
# hands them over.
set -u
printf '%s\n' "$*" >>"$FAKE_DIR/ssh.log"
target=""
while [ $# -gt 0 ]; do
  case $1 in
    -F | -i | -o | -l | -p | -L) shift 2 ;;
    -*) shift ;;
    *) target=$1; shift; break ;;
  esac
done
# How many words ssh was given after the target, one line per call: it joins them with spaces
# whatever their number, so a caller that means one argument must hand it one word.
printf '%s\n' "$#" >>"$FAKE_DIR/ssh.words"
eval "set -- $*"
printf 'ssh\t%s\t%s\n' "$target" "$*" >>"$FAKE_DIR/actions.log"
export FAKE_SSH_TARGET=$target
if [ -p /dev/stdin ]; then
  cat | "$@"
  exit $?
fi
exec "$@"
EOF

cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
# The fake docker: one registry per ssh target under FAKE_DIR/docker. `run` records what docker
# itself would record of the arguments it was given -- NanoCpus, Memory, the image, the network
# mode and the entrypoint -- and `inspect` answers the five formats vm.sh reads. A container is
# `running` from the moment it is started; a case that wants a Dispatch that died writes
# `exited` into its .state file and its last words into its .log, which `logs` prints, as the
# deployment's own Dispatch did when it refused to start.
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
    name="" cpus=- memory=- network=default entrypoint='<no value>' envfile="" image=""
    while [ $# -gt 0 ]; do
      case $1 in
        --name) name=$2; shift 2 ;;
        --cpus) cpus=$2; shift 2 ;;
        --memory) memory=$2; shift 2 ;;
        --network) network=$2; shift 2 ;;
        --entrypoint) entrypoint=$2; shift 2 ;;
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
    printf '%s\n' "$network" >"$dir/$name.net"
    printf '%s\n' "$entrypoint" >"$dir/$name.entrypoint"
    printf 'running\n' >"$dir/$name.state"
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
      '{{.HostConfig.NetworkMode}}') file=$dir/$name.net ;;
      '{{.State.Status}}') file=$dir/$name.state ;;
      '{{range .Config.Env}}{{println .}}{{end}}') file=$dir/$name.env ;;
      '{{.Id}}') file=$dir/$name.id ;;
      *) echo "fake docker: no answer for format $fmt" >&2; exit 2 ;;
    esac
    [ -f "$file" ] || { echo "Error: No such object: $name" >&2; exit 1; }
    cat -- "$file"
    ;;
  logs)
    name=${!#}
    [ -f "$dir/$name.main" ] || { echo "Error: No such object: $name" >&2; exit 1; }
    [ ! -f "$dir/$name.log" ] || cat -- "$dir/$name.log"
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
# The fake curl: the URL is the last argument of every call vm.sh makes. The whole argv goes to
# curl.argv as well, which is where the OS Login cases prove that no minted token reached a
# command line: vm.sh gives curl the Authorization header over its standard input.
set -u
url=${!#}
printf 'curl\t%s\n' "$url" >>"$FAKE_DIR/actions.log"
printf '%s\n' "$*" >>"$FAKE_DIR/curl.argv"
case $url in
  *metadata*) printf '{"access_token": "fake-token", "expires_in": 3600}\n' ;;
  *generateAccessToken)
    [ ! -f "$FAKE_DIR/mint-refuses" ] ||
      { printf 'curl: (22) The requested URL returned error: 403\n' >&2; exit 22; }
    printf '{"accessToken": "minted-token", "expireTime": "2026-11-05T02:00:00Z"}\n'
    ;;
  *importSshPublicKey*)
    [ ! -f "$FAKE_DIR/import-refuses" ] ||
      { printf 'curl: (22) The requested URL returned error: 400\n' >&2; exit 22; }
    printf '%s\n' "$*" >>"$FAKE_DIR/import.log"
    printf '{"loginProfile": {"posixAccounts": []}}\n'
    ;;
  *loginProfile*)
    [ ! -f "$FAKE_DIR/profile-refuses" ] ||
      { printf 'curl: (22) The requested URL returned error: 404\n' >&2; exit 22; }
    cat "$FAKE_DIR/loginprofile.json"
    ;;
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
    # A read of the profiling port on Dispatch's own machine always answers. A read of any other
    # port is one through the step's tunnel, and answers only while the fake tunnel that forwards
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
# The fake psql: records each statement and keeps a database list under FAKE_DIR/db. The whole
# argv goes to psql.argv as well, which is where the credential case proves that the instance's
# password reached no command line: vm.sh gives psql a URL without it and a passfile.
set -u
sql="" prev=""
for a in "$@"; do
  [ "$prev" = -c ] && sql=$a
  prev=$a
done
printf 'psql\t%s\n' "$sql" >>"$FAKE_DIR/actions.log"
printf '%s\n' "$*" >>"$FAKE_DIR/psql.argv"
# The passfile of each call, its mode beside it: libpq takes the password from PGPASSFILE and
# reads none that anyone but its owner can, so the credential case reads both back.
printf '%s %s\n' "${PGPASSFILE:-none}" "$([ -f "${PGPASSFILE:-}" ] && stat -c %a "$PGPASSFILE" || echo none)" >>"$FAKE_DIR/psql.passfile"
db=$FAKE_DIR/db
mkdir -p "$db"
if [[ $sql =~ ^select\ 1\ from\ pg_database\ where\ datname\ =\ \'([A-Za-z0-9_]+)\'$ ]]; then
  [ -e "$db/${BASH_REMATCH[1]}" ] && printf '1\n'
elif [[ $sql =~ ^drop\ database\ if\ exists\ ([A-Za-z0-9_]+) ]]; then
  rm -f -- "$db/${BASH_REMATCH[1]}"
elif [[ $sql =~ ^create\ database\ ([A-Za-z0-9_]+)\ template\ ([A-Za-z0-9_]+)$ ]]; then
  [ -e "$db/${BASH_REMATCH[2]}" ] || { echo "no template ${BASH_REMATCH[2]}" >&2; exit 1; }
  : >"$db/${BASH_REMATCH[1]}"
elif [[ $sql =~ ^create\ database\ \"([A-Za-z0-9_]+)\"$ ]]; then
  [ ! -e "$db/${BASH_REMATCH[1]}" ] || { echo "database ${BASH_REMATCH[1]} already exists" >&2; exit 1; }
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
# Every credential file this loadgen was given, as it was at the moment loadgen read it: the
# subcommand, the flag, the path, the file's mode and its content. The copy up puts on the load
# machine is this same script, so the line says what that machine held too. It is how the
# credential case proves a command was given the credential it needs while no value of one
# reached any command line.
for credflag in key-file pg-file token-file listener-token-file; do
  credpath=$(flag "$credflag") || continue
  printf '%s\t-%s\t%s\t%s\t%s\n' "$cmd" "$credflag" "$credpath" \
    "$(stat -c %a "$credpath" 2>/dev/null || echo none)" \
    "$(cat "$credpath" 2>/dev/null || echo unreadable)" >>"$FAKE_DIR/credentials.log"
done
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
  stub-embed)
    # The stub embedder holds a port for as long as it serves, and a second one on the same
    # port answers "address already in use" and writes no certificate -- which is what a second
    # up of a deployment whose first stub is still running meets. FAKE_DIR/stub-port is that
    # port: the stub claims it, writes its certificate, and serves until it is killed or until
    # the fakes' own directory goes with the case that started it, so no stub outlives its case.
    # The claim waits a moment for a live holder, because vm.sh's own stop sends that holder a
    # TERM just before this start and a port is freed by the exit that follows, not by the
    # signal.
    lock=$FAKE_DIR/stub-port
    waited=0
    while [ -f "$lock" ] && kill -0 "$(cat "$lock" 2>/dev/null)" 2>/dev/null; do
      if ((waited >= 50)); then
        printf 'fake stub-embed: listen tcp %s: bind: address already in use\n' "$(flag listen)" >&2
        exit 1
      fi
      "$REAL_SLEEP" 0.1
      waited=$((waited + 1))
    done
    printf '%s\n' "$$" >"$lock"
    printf -- '-----FAKE CERT-----\n' >"$(flag cert-out)"
    release() { [ "$(cat "$lock" 2>/dev/null)" != "$$" ] || rm -f -- "$lock"; exit 0; }
    trap release TERM INT
    while [ -d "$FAKE_DIR" ] && [ "$(cat "$lock" 2>/dev/null)" = "$$" ]; do "$REAL_SLEEP" 0.2; done
    release
    ;;
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
# `ssh -N -L <port>:127.0.0.1:<pprof port> <Dispatch's machine>` run on the load machine. It
# records its argv, says in FAKE_DIR/tunnel-up which port now answers and under which pid, and
# ends on TERM as ssh does. FAKE_DIR/tunnel-refuses makes it exit at once, as ssh does when the
# forward cannot be made (ExitOnForwardFailure) or that machine refuses the connection.
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
# The OS Login account the deployment file names, the POSIX user its login profile gives and
# the ssh target each machine is then reached at. Every one of them is this test's own: the
# fake curl answers the mint, the import and the profile, and the fake ssh runs what it is
# given here.
readonly login_account=lt-oslogin@theorem-agent-infra.iam.gserviceaccount.com
readonly oslogin_user=sa_101234567890123456789
readonly app_target=$oslogin_user@10.138.0.21
readonly gen_target=$oslogin_user@10.138.0.22
readonly nats_target=$oslogin_user@10.138.0.23
jq -n --arg remote "$remote" --arg loadgen "$bin/loadgen" --arg tunnel "$bin/pprof-tunnel" \
  --argjson tunnel_port "$tunnel_port" --arg account "$login_account" \
  --arg dispatch "$dispatch_image" --arg listener "$listener_image" \
  --arg proxy "$proxy_image" --arg nats "$nats_image" '{
  project: "theorem-agent-infra", zone: "us-west1-a",
  token_url: "http://metadata.example/token",
  sqladmin: "https://sqladmin.googleapis.com",
  compute: "https://compute.googleapis.com",
  iamcredentials: "https://iamcredentials.googleapis.com",
  oslogin: "https://oslogin.googleapis.com",
  login_service_account: $account,
  vms: {
    app: {host: "10.138.0.21", instance: "lt-app"},
    generator: {host: "10.138.0.22", instance: "lt-gen"},
    nats: {host: "10.138.0.23", instance: "lt-nats"}
  },
  load_vm: "generator",
  containers: {
    dispatch: "app", "auth-proxy": "app", "dispatch-listener": "app",
    nats: "nats", "hub-listener": "nats",
    "load-listener-1": "generator", "load-listener-2": "generator",
    "load-listener-3": "generator", "load-listener-4": "generator"
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
  for name in lt-app lt-gen lt-nats; do
    # The Compute API answers maxRunDuration as an object, never as the duration string it takes
    # on the way in, which is why vm.sh reads its seconds.
    jq -n --arg n "$name" '{name: $n, status: "RUNNING",
      scheduling: {automaticRestart: false, maxRunDuration: {seconds: "259200", nanos: 0}}}' >"$fake/compute-$name.json"
  done
  jq -n --arg u "$oslogin_user" '{posixAccounts: [{username: $u, uid: 1000}]}' >"$fake/loginprofile.json"
  # Dispatch's /healthz body, as stack.sh's dispatch_healthy reads it: it carries no `status`.
  printf '{"ok": true, "nats": true, "schema_version": 1337}\n' >"$fake/healthz"
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

# The run directory of the functions below, which is no run of a subcommand's: ssh_cmd keeps
# the run's known_hosts there and oslogin_ready its keypair.
readonly fn_run=vm-20261103t000000-f00d
readonly fn_run_dir=$tmp/root/$fn_run

# run_fn [NAME=VALUE...] FUNCTION ARGS...: one function of vm.sh, driven here rather than
# through a subcommand, its output in $tmp/out and its status in RC. vm.sh is sourced (its last
# line runs main only when it is the program), the deployment file this test writes is read,
# and the run, its directory, the OS Login user and the run's key are set as a subcommand sets
# them before its first ssh, so a function that reaches a machine reaches the fakes. A leading
# NAME=VALUE sets one of the script's own bounds -- which is how a poll of /healthz is given
# ten seconds here rather than the nine hundred a subcommand gives it.
run_fn() {
  set +e
  bash -c '
    source "$1"
    CONFIG=$2 RUN=$3 RUN_DIR=$4 SSH_USER=$5 SSH_KEY=$4/oslogin
    shift 5
    mkdir -p -- "$RUN_DIR"
    : >"$SSH_KEY"
    while [[ ${1:-} =~ ^[A-Z_]+= ]]; do
      eval "$1"
      shift
    done
    check_placement
    "$@"
  ' _ "$vm" "$config" "$fn_run" "$fn_run_dir" "$oslogin_user" "$@" >"$tmp/out" 2>&1
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
for want in "--cpus 16" "--memory 32g" "--network host" "$dispatch_image"; do
  [[ $line == *"$want"* ]] || fail "up's dispatch container has no '$want': $line"
done
pass "up starts Dispatch at 16 CPU, 32 GiB, host networking and its image digest"

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
[ -f "$remote/app/stub-embed.pem" ] || fail "up left no stub embedder certificate on Dispatch's machine"
grep -q 'curl	.*healthz' "$fake/actions.log" || fail "up did not wait on Dispatch's /healthz"
pass "up starts the stub embedder before Dispatch, copies its certificate to Dispatch's machine and waits on /healthz"

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
grep -q '^| dispatch network mode | host | host | yes |$' "$tmp/out" ||
  fail "check does not read back Dispatch's host networking"
grep -q '^| cloudsql databaseVersion | POSTGRES_18 | POSTGRES_18 | yes |$' "$tmp/out" ||
  fail "check does not read the instance's own settings from the Cloud SQL Admin API"
grep -q '^| cloudsql settings.tier | db-custom-16-61440 | db-custom-16-61440 | yes |$' "$tmp/out" ||
  fail "check does not read the instance's tier"
grep -q '^| app VM scheduling.maxRunDuration seconds | 259200 | 259200 | yes |$' "$tmp/out" ||
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

printf '/dispatch 16000000000 17179869184 %s\n' "$dispatch_image" >"$fake/docker/$app_target/dispatch.main"
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
jq '.scheduling.maxRunDuration.seconds = "86400"' "$fake/compute-lt-app.json" >"$tmp/j" && mv "$tmp/j" "$fake/compute-lt-app.json"
run_vm check --config "$config"
((RC != 0)) || fail "check exited 0 with a run-time limit that is not 72 hours"
grep -q '^| app VM scheduling.maxRunDuration seconds | 259200 | 86400 | no |$' "$tmp/out" ||
  fail "check does not show the run-time limit it expected beside the one it found"
pass "check exits non-zero when a VM's maxRunDuration is not 259200 s"

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
grep -q "ssh	$app_target	curl .*127.0.0.1:6060/debug/pprof/profile?seconds=600" "$fake/actions.log" ||
  fail "step did not fetch the profile from the loopback of Dispatch's own machine"
for file in goroutines.txt heap.pprof heap.txt; do
  [ -f "$step/$file" ] || fail "step fetched no $file from Dispatch's machine"
done
[ -f "$step/cgroup.json" ] || fail "step ran no loadgen cgroup-merge"
[ -f "$step/cloudsql-cpu.json" ] || fail "step ran no loadgen cloudsql-cpu"
[ -f "$step/TABLES.md" ] || fail "step ran no loadgen tables"
[ -f "$step/summary.json" ] || fail "step copied no summary.json back from the load machine"
[ -f "$step/reset.json" ] || fail "step recorded no reset"
grep -q 'cloudsql-cpu.*-instance lt-db' "$fake/loadgen.log" || fail "step's Cloud SQL read does not name the instance"
grep -q -- "-vm $app_target:auth-proxy=.*,dispatch=" "$fake/vm-sample.log" ||
  fail "step's sampler does not read the containers of Dispatch's machine at their cgroups"
grep -q -- "-vm $gen_target:load-listener-1=" "$fake/vm-sample.log" ||
  fail "step's sampler does not read the load machine's containers at their cgroups"
grep -q -- "-vm $nats_target:nats=.*,hub-listener=" "$fake/vm-sample.log" ||
  fail "step's sampler does not read the NATS machine's containers at their cgroups"
pass "step samples every machine, fetches the profile from Dispatch's machine and runs the merge, the Cloud SQL read and the tables"

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
# The step's first reading of all three machines answers RUNNING; every reading after it
# answers TERMINATED, as a machine stopped at its 72-hour run-time limit does.
rm -f "$fake/compute-reads"
printf '3\n' >"$fake/compute-stop-after"
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
[ "$RC" = 3 ] || fail "series over a stopped machine exited $RC, not 3"
grep -q 'because a machine is not running' "$tmp/out" || fail "series does not say that a machine stopped"
pass "a step whose machine is already stopped is discarded before it starts, and series stops on it"

# ---- (k) seed ---------------------------------------------------------------------------------------

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the seed case"
# The database up made on the instance and Dispatch migrated: the seed writes into it and the
# last phase renames it to the template.
[ -e "$fake/db/dispatch" ] || fail "up left no database for the seed to write into"
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
grep -q "ssh	$gen_target	$remote/app/loadgen loop-seed-write" "$fake/actions.log" ||
  fail "seed did not write the loop seed from the load machine"
[ -e "$fake/db/dispatch_seed" ] || fail "seed left no template database"
pass "seed plans, writes on the load machine, checks, exports and finishes with the embedding gate, recording each phase's wall clock and the coverage progress"

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

# The tunnel forwards a port of the load machine, where loadgen run runs, to the profiling port
# on the loopback of Dispatch's own machine, which is the one place that port answers.
grep -q -- "^-L $tunnel_port:127.0.0.1:6060 $app_target$" "$fake/tunnel.log" ||
  fail "the step's tunnel is not '-L $tunnel_port:127.0.0.1:6060 $app_target': $(cat "$fake/tunnel.log")"
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
  fail "step gave loadgen an address of Dispatch's machine, which its sampler cannot reach: $line"
pass "step opens the profiling tunnel on the load machine and proves it with a read of /debug/pprof/goroutine?debug=1 before loadgen run, which reads the profiling port through it"

tunnel_pid=$(awk '{print $1}' "$fake/tunnel-up")
[ -n "$tunnel_pid" ] || fail "the fake tunnel recorded no pid"
kill -0 "$tunnel_pid" 2>/dev/null &&
  fail "the step left its profiling tunnel running on the load machine (pid $tunnel_pid)"
[ ! -e "$remote/work/pprof-tunnel.pid" ] || fail "the step left its tunnel's pid file on the load machine"
pass "the step's profiling tunnel is closed when the step ends, leaving no process and no pid file on the load machine"

# The local port is the deployment file's, and 16060 when it names none.
jq 'del(.ports.pprof_tunnel)' "$config" >"$tmp/default-tunnel.json"
run_vm step --config "$tmp/default-tunnel.json" 3x 3 --duration 10m
((RC == 0)) || fail "a step on a deployment file with no ports.pprof_tunnel failed: see the output above"
grep -q -- "^-L 16060:127.0.0.1:6060 $app_target$" "$fake/tunnel.log" ||
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
for target in "$app_target" "$gen_target" "$nats_target"; do
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
pass "up makes the backing directory under the stateful partition with sudo, bind-mounts it at the remote root, remounts it exec,nosuid,nodev and gives it to the ssh user, on every machine"

mount_at=$(line_at 'chown -- ') copy_at=$(line_at "$remote/app/loadgen") start_at=$(line_at 'run --name ')
[ -n "$mount_at" ] && [ -n "$copy_at" ] && [ -n "$start_at" ] && ((mount_at < copy_at)) && ((copy_at < start_at)) ||
  fail "up did not mount every remote root (through line ${mount_at:-none}) before the loadgen copy (${copy_at:-none}) and the first container (${start_at:-none})"
pass "up mounts every remote root before it copies loadgen onto the load machine and before any container starts"

# A second up over machines that already carry the mount remounts it and stacks no second mount.
: >"$fake/actions.log"
run_vm up --config "$config"
((RC == 0)) || fail "a second up over machines that already carry the mount failed: see the output above"
for target in "$app_target" "$gen_target" "$nats_target"; do
  sudo_log "$target" | grep -q -- '--bind --' &&
    fail "a second up mounted the remote root again on $target, stacking a second mount on it"
  sudo_log "$target" | grep -qxF -- "mount -o remount,bind,exec,nosuid,nodev -- /mnt/stateful_partition/app $remote/app" ||
    fail "a second up did not remount the remote root on $target: $(sudo_log "$target")"
done
[ "$(actions | grep -c '^start ')" = 9 ] ||
  fail "a second up started $(actions | grep -c '^start ') containers, not the 9 of the specification"
pass "a second up over a VM that already carries the mount remounts it, mounts it no second time, and starts the deployment all the same"

# A mount that cannot be made refuses up by name, before a container is started: a load machine
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
for target in "$app_target" "$gen_target" "$nats_target"; do
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
for target in "$app_target" "$gen_target" "$nats_target"; do
  sudo_log "$target" | grep -qxF -- "mount --bind -- /mnt/stateful_partition/loadtest-backing $remote/elsewhere" ||
    fail "paths.stateful_dir is not the backing directory on $target: $(sudo_log "$target")"
done
pass "the deployment file names the backing directory under the stateful partition"

# ---- (r) the deployment file says which machine each container runs on ---------------------------

# The deployment this test's file describes has three machines, as INFRA-1051's has: an
# application machine, a load generator and a machine of their own for NATS and the hub
# listener. Nothing of that is in vm.sh, so these cases prove the script reads it.
reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed on the three-machine deployment: see the output above"
for role in dispatch auth-proxy dispatch-listener; do
  [ -f "$fake/docker/$app_target/$role.main" ] ||
    fail "up did not start $role on the machine the deployment file names for it"
done
for role in nats hub-listener; do
  [ -f "$fake/docker/$nats_target/$role.main" ] ||
    fail "up did not start $role on the NATS machine the deployment file names for it"
  [ ! -f "$fake/docker/$app_target/$role.main" ] ||
    fail "up started $role on the application machine, which the deployment file does not name for it"
done
for n in 1 2 3 4; do
  [ -f "$fake/docker/$gen_target/load-listener-$n.main" ] ||
    fail "up did not start load-listener-$n on the load machine"
done
grep -q "ssh	$nats_target	mkdir -p -- $remote/app/nats" "$fake/actions.log" ||
  fail "up made the JetStream store somewhere other than the machine NATS runs on"
pass "up starts every container on the machine the deployment file names for it, over three machines"

run_vm check --config "$config"
((RC == 0)) || fail "check failed on the three-machine deployment: see the output above"
grep -q '^| nats name cpu memory image (nats VM) | /nats 0 0 .* | /nats 0 0 .* | yes |$' "$tmp/out" ||
  fail "check does not read NATS back from the machine the deployment file runs it on"
grep -q '^| hub-listener name cpu memory image (nats VM) | .* | yes |$' "$tmp/out" ||
  fail "check does not read the hub listener back from the NATS machine"
grep -q '^| nats VM scheduling.maxRunDuration seconds | 259200 | 259200 | yes |$' "$tmp/out" ||
  fail "check does not read the NATS machine's run-time limit"
for address in 10.139.0.2:5432 10.138.0.10:4222; do
  grep -q "^| nats VM to production $address | refused | refused | yes |$" "$tmp/out" ||
    fail "check does not prove $address refused from the NATS machine"
done
grep -q '^| dispatch env NATS_URLS | NATS_URLS=nats://10.138.0.23:4222 | NATS_URLS=nats://10.138.0.23:4222 | yes |$' "$tmp/out" ||
  fail "Dispatch's NATS_URLS is not the address of the machine the deployment file runs NATS on"
pass "check reads all three machines' containers, run-time limits and production-address refusals"

seeded_run
run_vm step --config "$config" 1x 1 --duration 10m
((RC == 0)) || fail "the step of the three-machine case failed: see the output above"
sample=$(tail -n1 "$fake/vm-sample.log")
[ "$(grep -o -- '-vm ' <<<"$sample" | wc -l)" = 3 ] ||
  fail "the sampler read $(grep -o -- '-vm ' <<<"$sample" | wc -l) machines, not the three of this deployment: $sample"
[[ $sample == *"-vm $app_target:auth-proxy="*",dispatch-listener="*",dispatch="* ]] ||
  fail "the sampler does not read the application machine's own containers: $sample"
[[ $sample == *"-vm $gen_target:load-listener-1="*",load-listener-4="* ]] ||
  fail "the sampler does not read the load machine's own containers: $sample"
[[ $sample == *"-vm $nats_target:nats="*",hub-listener="* ]] ||
  fail "the sampler does not read the NATS machine's own containers: $sample"
[[ $sample == *"$nats_target:"*"load-listener"* ]] &&
  fail "the sampler reads a container at a machine the deployment file does not run it on: $sample"
pass "the sampler reads all three machines, each with its own containers' cgroups"

# A container the file places nowhere, a machine no container runs on, a container this
# deployment does not run and a load_vm that is no machine of the file: each refused by name
# before anything is started.
reset_fake
jq 'del(.containers["hub-listener"])' "$config" >"$tmp/unplaced.json"
run_vm up --config "$tmp/unplaced.json"
((RC != 0)) || fail "up accepted a deployment file that places no machine for the hub listener"
grep -q 'does not say which machine the container hub-listener runs on' "$tmp/out" ||
  fail "up's refusal does not name the container the file places nowhere: $(cat "$tmp/out")"
[ ! -s "$fake/actions.log" ] || fail "up reached a machine before refusing an unplaced container"

jq '.containers["hub-listener"] = "elsewhere"' "$config" >"$tmp/nomachine.json"
run_vm up --config "$tmp/nomachine.json"
((RC != 0)) || fail "up accepted a container placed on a machine the file does not name"
grep -q "runs the container hub-listener on the machine 'elsewhere', which is none of its machines" "$tmp/out" ||
  fail "up's refusal does not name the machine the file does not have: $(cat "$tmp/out")"

jq '.containers.nats = "app" | .containers["hub-listener"] = "app"' "$config" >"$tmp/idle.json"
run_vm up --config "$tmp/idle.json"
((RC != 0)) || fail "up accepted a deployment file with a machine no container runs on"
grep -q 'names the machine nats, which no container of this deployment runs on' "$tmp/out" ||
  fail "up's refusal does not name the machine no container runs on: $(cat "$tmp/out")"

jq '.containers["load-listener-5"] = "generator"' "$config" >"$tmp/unknown.json"
run_vm up --config "$tmp/unknown.json"
((RC != 0)) || fail "up accepted a deployment file that places a container this deployment does not run"
grep -q 'places the container load-listener-5, which this deployment does not run' "$tmp/out" ||
  fail "up's refusal does not name the container this deployment does not run: $(cat "$tmp/out")"

jq '.load_vm = "nowhere"' "$config" >"$tmp/noload.json"
run_vm up --config "$tmp/noload.json"
((RC != 0)) || fail "up accepted a load_vm that is no machine of the deployment file"
grep -q "gives load_vm as 'nowhere', which is none of its machines" "$tmp/out" ||
  fail "up's refusal does not name the load machine it could not find: $(cat "$tmp/out")"
[ ! -s "$fake/actions.log" ] || fail "up reached a machine before refusing the deployment file's placement"
[ ! -s "$fake/docker.log" ] || fail "up started a container although the deployment file's placement is wrong"
pass "a container placed nowhere, one placed on a machine the file does not name, a machine no container runs on, a container this deployment does not run and a load_vm that is no machine are each refused by name, with nothing started"

# The same script on a two-machine deployment, with NATS and the hub listener back on the
# application machine: the third machine is the file's to have, never this script's to require.
reset_fake
jq 'del(.vms.nats) | .containers.nats = "app" | .containers["hub-listener"] = "app"' "$config" >"$tmp/two.json"
run_vm up --config "$tmp/two.json"
((RC == 0)) || fail "up failed on a two-machine deployment: see the output above"
[ "$(actions | grep -c '^start ')" = 9 ] ||
  fail "up started $(actions | grep -c '^start ') containers on the two-machine deployment, not the 9 of the specification"
for role in nats hub-listener dispatch; do
  [ -f "$fake/docker/$app_target/$role.main" ] ||
    fail "up did not start $role on the application machine of the two-machine deployment"
done
[ ! -d "$fake/docker/$nats_target" ] ||
  fail "up reached a third machine on a deployment file that names two"
run_vm check --config "$tmp/two.json"
((RC == 0)) || fail "check failed on the two-machine deployment: see the output above"
grep -q '| no |' "$tmp/out" && fail "check read back a value of the two-machine deployment that differs"
grep -q '^| nats name cpu memory image (app VM) | .* | yes |$' "$tmp/out" ||
  fail "check does not read NATS back from the application machine of the two-machine deployment"
grep -q 'nats VM' "$tmp/out" && fail "check read a machine the two-machine deployment file does not name"
pass "a deployment file that keeps NATS on the application machine works, two machines and all"

# ---- (s) every ssh is OS Login's ------------------------------------------------------------

# The machines carry enable-oslogin and block-project-ssh-keys and this pod's identity has no
# POSIX profile, so vm.sh mints a token for the deployment's login service account, imports the
# run's own key for it and reads the POSIX user it was given, before its first ssh.
reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the OS Login cases: see the output above"
mint_at=$(line_at 'generateAccessToken')
import_at=$(line_at 'importSshPublicKey')
profile_at=$(line_at 'loginProfile')
ssh_at=$(line_at 'ssh	')
[ -n "$mint_at" ] || fail "up minted no access token for the login service account"
[ -n "$import_at" ] || fail "up imported no public key for the login service account"
[ -n "$profile_at" ] || fail "up read no login profile"
[ -n "$ssh_at" ] || fail "up reached no machine at all"
((mint_at < import_at)) && ((import_at < profile_at)) && ((profile_at < ssh_at)) ||
  fail "up's mint ($mint_at), import ($import_at) and profile ($profile_at) did not all come before its first ssh ($ssh_at)"
grep -q "importSshPublicKey?projectId=theorem-agent-infra" "$fake/actions.log" ||
  fail "the key import does not name the project"
grep -q "$login_account" "$fake/actions.log" ||
  fail "the OS Login calls do not name the deployment file's login service account"
pass "up mints a token, imports the run's key and reads the login profile, in that order, before its first ssh"

key=$(run_dir)/oslogin
[ -f "$key" ] && [ -f "$key.pub" ] || fail "up made no keypair in the run directory"
[ "$(stat -c %a "$key")" = 600 ] || fail "the run's private key is mode $(stat -c %a "$key"), not 600"
grep -q -- "-F /dev/null -i $key .* $app_target " "$fake/ssh.log" ||
  fail "up's ssh does not run as the OS Login user with the run's own key: $(head -n1 "$fake/ssh.log")"
grep -v -- "-i $key " "$fake/ssh.log" | grep -q . &&
  fail "an ssh of this run ran without the run's own key: $(grep -v -- "-i $key " "$fake/ssh.log" | head -n1)"
# The tunnel's own ssh is the deployment file's command, run on the load machine, so it carries
# no key of this pod's: ssh.log holds this script's own ssh calls alone.
pass "every ssh of the run goes as the user the login profile named, with the key the run made and no ssh config of this pod's"

before=$(cat "$key.pub")
run_vm up --config "$config" --run "$(cat "$LOADTEST_VM_ROOT/current")"
((RC == 0)) || fail "a second up of the same run failed: see the output above"
[ "$(cat "$key.pub")" = "$before" ] ||
  fail "a second up of the same run made a second keypair"
[ "$(grep -c 'importSshPublicKey' "$fake/actions.log")" -ge 2 ] ||
  fail "a second up did not import the run's key again, so its expiry is never refreshed"
pass "a second up of one run imports the key it already has and makes no second one"

! grep -rqF 'minted-token' "$(run_dir)" ||
  fail "the minted access token was written into a file the run keeps: $(grep -rlF 'minted-token' "$(run_dir)" | head -n1)"
! grep -qF 'minted-token' "$fake/curl.argv" ||
  fail "the minted access token reached a curl command line, where any process on this pod can read it"
! grep -qF 'minted-token' "$fake/actions.log" || fail "the minted access token reached the action log"
pass "the minted token reaches no command line and no file the run keeps"

# A refusal of any of the three steps refuses up by name, with nothing started.
for step in mint import profile; do
  reset_fake
  : >"$fake/$step-refuses"
  run_vm up --config "$config"
  ((RC != 0)) || fail "up ran although the OS Login $step was refused"
  case $step in
    mint) want='OS Login step 1 of 4' ;;
    import) want='OS Login step 2 of 4' ;;
    profile) want='OS Login step 3 of 4' ;;
  esac
  grep -q "$want" "$tmp/out" || fail "up's refusal does not say which OS Login step failed: $(cat "$tmp/out")"
  grep -q '403\|400\|404' "$tmp/out" || fail "up's refusal does not say what the endpoint answered: $(cat "$tmp/out")"
  grep -q 'ssh	' "$fake/actions.log" && fail "up reached a machine although the OS Login $step was refused"
  [ ! -s "$fake/docker.log" ] || fail "up started a container although the OS Login $step was refused"
  rm -f "$fake/$step-refuses"
done
pass "a refusal of the mint, the import or the login profile refuses up by name, naming the step and the answer, with nothing started"

reset_fake
jq '.posixAccounts = []' "$fake/loginprofile.json" >"$tmp/j" && mv "$tmp/j" "$fake/loginprofile.json"
run_vm up --config "$config"
((RC != 0)) || fail "up ran although the login profile gave no POSIX account"
grep -q 'no POSIX account, so there is no user to ssh as' "$tmp/out" ||
  fail "up's refusal does not say that the login profile holds no POSIX user: $(cat "$tmp/out")"
[ ! -s "$fake/docker.log" ] || fail "up started a container although there is no user to ssh as"
pass "a login profile with no POSIX account refuses up by name, with nothing started"

# ---- (o) the example deployment file is one vm.sh runs on ---------------------------------------

# deployment.example.json is the file a reader copies. These three cases hold it to vm.sh itself
# and not to a second copy of the schema: the script runs on it, its keys are read out of the
# script's own filters, and nothing of a real deployment is in it.
readonly example=$here/deployment.example.json

# The example with the five remote paths and the loadgen binary pointed at this pod, because
# those alone name a file system the fakes stand in for. Everything else is the example's own --
# the project, the zone, every machine, the instance and the settings it expects, every image,
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

# container_field N: field N of every line of vm.sh's own container table, once each. Field 1
# names the containers its .containers object must place and field 2 the keys of .images.<key>.
container_field() {
  bash -c 'source "$1"; printf "%s\n" "${CONTAINERS[@]}"' _ "$vm" | awk -v n="$1" '{print $n}' | sort -u
}

# script_keys: every key of the deployment file vm.sh reads, one "KEY<tab>required" or
# "KEY<tab>optional" a line, read out of the script's own text and not written here again: each
# cfg and cfg_valid call gives its filter and, by its second argument, whether the key has a
# default; the two objects it reads with jq give theirs by their "// {}"; the two it reads
# element by element -- the ports of ports.load_listeners and the machine of one container --
# give theirs by their "// empty". A filter spelled with a parameter is expanded over the
# arguments the script passes it -- the image keys of its container table, the ports it asks
# port_of for, the values it asks load_value for, and, for .vms.<machine>.*, the machines the
# example itself names, since the script takes that list from the file and holds no list of its
# own -- and a parameter of a shape not known here stays in the key, so the comparison below
# names it rather than dropping it.
script_keys() {
  local body vms images ports loads key kind list word expanded
  body=$(grep -v '^[[:space:]]*#' "$vm")
  vms=$(jq -r '.vms | keys[]' "$example") images=$(container_field 2)
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
# to, an instance of the lt- register, a login service account of that register too, an image
# digest that is one character repeated, and a credential that is the placeholder "...".
while read -r address; do
  [[ $address =~ ^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|127\.) ]] ||
    fail "deployment.example.json names the address $address, which is neither RFC 1918 nor loopback"
done < <(grep -oE '[0-9]+(\.[0-9]+){3}' "$example")

while read -r host; do
  case $host in
    10.* | 127.* | metadata.google.internal | sqladmin.googleapis.com | compute.googleapis.com | iamcredentials.googleapis.com | oslogin.googleapis.com) ;;
    *) fail "deployment.example.json reaches the host $host, which is no placeholder and no public Google endpoint" ;;
  esac
done < <(grep -oE '"[a-z]+://[^/"]+' "$example" | sed -E 's|^.*://||; s|^.*@||; s|:[0-9]+$||')

while read -r instance; do
  [[ $instance =~ ^lt-[a-z0-9-]+$ ]] ||
    fail "deployment.example.json names the instance $instance, which is not of the placeholder lt- register"
done < <(jq -r '(.vms[].instance, .cloudsql.instance)' "$example")

account=$(jq -r .login_service_account "$example")
[[ $account =~ ^lt-[a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com$ ]] ||
  fail "deployment.example.json names the login service account $account, which is not of the placeholder lt- register"

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

# ---- (t) the eleven the deployment itself found --------------------------------------------

# Driving this script against the deployment of INFRA-1051 found eleven defects that every case
# above agreed with, because a fake written from a script agrees with that script. These cases
# hold each fix to what the deployment measured, and each one names the defect it stands for,
# so a change that undoes one fails here by name rather than on the deployment again.

reset_fake

# 1. remote_argv. ssh has no argument vector: it joins what it is given with spaces and the
# machine's shell splits the result again, so an argument holding a space, a quote or a pipe
# arrived as several, and `sh -c '<script>' sh <path>` reached a machine as a dozen words.
run_fn remote_argv 'a b' "it's" 'x|y' plain
((RC == 0)) || fail "remote_argv refused arguments every remote command of this script may hold: $(cat "$tmp/out")"
want="'a b' 'it'\\''s' 'x|y' 'plain'"
[ "$(cat "$tmp/out")" = "$want" ] ||
  fail "remote_argv wrote [$(cat "$tmp/out")], not [$want]"

probe=$remote/argv.txt
# shellcheck disable=SC2016 # expanded by the VM's sh
run_fn on_vm app sh -c 'out=$1; shift; printf "[%s]\n" "$@" >"$out"' sh "$probe" 'a b' "it's" 'x|y'
((RC == 0)) || fail "on_vm failed to run a command whose arguments hold a space, a quote and a pipe: $(cat "$tmp/out")"
[ "$(cat "$probe")" = "$(printf '[a b]\n[%s]\n[x|y]' "it's")" ] ||
  fail "the machine was given $(tr '\n' ' ' <"$probe")rather than the three arguments on_vm was given"
[ "$(sort -u "$fake/ssh.words")" = 1 ] ||
  fail "on_vm handed ssh $(sort -u "$fake/ssh.words" | tr '\n' ' ')words after the target, which ssh joins with spaces: a remote command must reach it as one"
pass "a remote argument holding a space, a single quote or a pipe reaches the machine as exactly one argument"

# The up every case below reads, and the fresh instance it runs against: reset_fake left the
# instance holding no database, which is the state a new one ships in. Its output is kept
# beside $tmp/out, because the cases below drive functions of their own over that file.
[ ! -e "$fake/db/dispatch" ] || fail "the instance holds the database dispatch before any up"
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the cases of the eleven: see the output above"
up_out=$tmp/up.out
cp -- "$tmp/out" "$up_out"

# 1 again, over every remote command of a whole subcommand rather than one call.
[ "$(sort -u "$fake/ssh.words")" = 1 ] ||
  fail "a remote command of up reached ssh as $(sort -u "$fake/ssh.words" | tr '\n' ' ')words, which ssh joins with spaces and the machine's shell splits again"
pass "every remote command of a subcommand reaches ssh as one word"

# 2. ssh_cmd takes each machine's host key into a file of the run's own under accept-new: these
# machines are made by a merge and this pod has never met them, so there is no key to have known
# beforehand and the pod's own known_hosts must not decide what the run trusts.
grep -q -- "-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$(run_dir)/known_hosts $app_target " "$fake/ssh.log" ||
  fail "up's ssh does not take the machines' host keys into the run's own known_hosts: $(head -n1 "$fake/ssh.log")"
grep -v -- "-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$tmp/root/" "$fake/ssh.log" | grep -q . &&
  fail "an ssh ran under this pod's own host keys: $(grep -v -- "UserKnownHostsFile=$tmp/root/" "$fake/ssh.log" | head -n1)"
pass "every ssh accepts a new host key into the run's own known_hosts file and reads no other"

# 3. put_file writes beside its target and renames it on, because a plain write onto a file that
# is running answers ETXTBSY -- which is what a second up of a deployment whose loadgen or stub
# embedder is still running meets -- and because a rename leaves no half-written file where a
# whole one was.
grep -qF 'cat >"$1.new" && mv -f -- "$1.new" "$1"' "$fake/actions.log" ||
  fail "put_file does not write beside its target and rename it on: $(grep -F 'umask 077' "$fake/actions.log" | head -n1)"
# The kernel under this pod refuses no write to a running binary, so this case puts in its place
# a file the kernel does refuse to open for writing. What it proves is what ETXTBSY asks for:
# put_file never opens the target itself, so a target it could not have opened is replaced all
# the same, and nothing of the file it wrote beside it is left.
busy=$remote/app/busy-loadgen
printf 'the binary that is running\n' >"$busy"
chmod 444 "$busy"
! (printf 'a plain write\n' >"$busy") 2>/dev/null ||
  fail "this case proves nothing: a plain write onto $busy went through"
printf 'the binary up installs\n' >"$tmp/new-loadgen"
run_fn put_file app "$tmp/new-loadgen" "$busy"
((RC == 0)) || fail "put_file could not replace a file a plain write cannot open: $(cat "$tmp/out")"
[ "$(cat "$busy")" = "the binary up installs" ] || fail "$busy holds '$(cat "$busy")' after put_file"
[ ! -e "$busy.new" ] || fail "put_file left the file it wrote beside the target behind"
[ "$(stat -c %a "$busy")" = 600 ] || fail "put_file left $busy at mode $(stat -c %a "$busy"), not 600"
pass "put_file writes beside the target and renames it on, which replaces a file a plain write cannot open"

# 5. The Dispatch container takes no --storage-opt: docker on Container-Optimized OS refuses one
# ("supported only for overlay over xfs with 'pquota' mount option"), so production's 8 GiB
# ephemeral limit has no form here and check reads back the network mode alone.
grep -- ' run --name ' "$fake/docker.log" | grep -q -- '--storage-opt' &&
  fail "up gave a container a --storage-opt, which the docker on these machines refuses"
pass "no container is started with a --storage-opt this deployment's docker refuses"

# 6. The Dispatch container names its own entrypoint: the Envoy image carries four binaries and
# its entrypoint is envoy-listener, so without it that container ran a second listener and died
# on the port the real one held.
line=$(docker_run_line dispatch)
[[ $line == *"--entrypoint envoy-dispatch $dispatch_image"* ]] ||
  fail "up's Dispatch container does not name the entrypoint envoy-dispatch before its image: $line"
grep -- ' run --name ' "$fake/docker.log" | grep -v -- ' --name dispatch ' | grep -q -- '--entrypoint' &&
  fail "up gave a container other than Dispatch an entrypoint of its own: $(grep -- ' run --name ' "$fake/docker.log" | grep -v -- ' --name dispatch ' | grep -- '--entrypoint' | head -n1)"
pass "the Dispatch container runs envoy-dispatch, and no other container names an entrypoint"

# 7. Dispatch's environment carries ENVOY_MACHINE_ID and ENVOY_ALLOW_REMOTE_NATS: NATS is on a
# machine of its own here, and without the second every connect refuses a NATS server that is
# not this machine's (internal/bus/reach.go).
env_file=$remote/app/env/dispatch.env
grep -qx 'ENVOY_MACHINE_ID=dispatch' "$env_file" ||
  fail "Dispatch's environment names no ENVOY_MACHINE_ID: $(cat "$env_file")"
grep -qx 'ENVOY_ALLOW_REMOTE_NATS=1' "$env_file" ||
  fail "Dispatch's environment does not allow the NATS of another machine: $(cat "$env_file")"
pass "Dispatch's environment carries ENVOY_MACHINE_ID and ENVOY_ALLOW_REMOTE_NATS"

# 8. up makes the database the deployment file names when the instance does not hold it: a new
# instance ships with the login's own database and no other, and Dispatch exits rather than
# making one.
grep -q 'psql	create database "dispatch"' "$fake/actions.log" ||
  fail "up did not make the database on an instance that holds none"
grep -q 'made the database dispatch on the instance' "$up_out" || fail "up does not say it made the database"
[ -e "$fake/db/dispatch" ] || fail "up left no database dispatch on the instance"

# 5, 7, 10 and 11 read back, in the one check this section runs.
: >"$fake/actions.log"
run_vm check --config "$config"
((RC == 0)) || fail "check failed after the up of these cases: see the output above"
grep -q '^| dispatch network mode | host | host | yes |$' "$tmp/out" ||
  fail "check does not read Dispatch's network mode back on its own"
grep -qi 'ephemeral' "$tmp/out" &&
  fail "check reads back an ephemeral limit, which this deployment's docker cannot set"
grep -q '^| dispatch env ENVOY_MACHINE_ID | ENVOY_MACHINE_ID=dispatch | ENVOY_MACHINE_ID=dispatch | yes |$' "$tmp/out" ||
  fail "check does not read Dispatch's ENVOY_MACHINE_ID back"
grep -q '^| dispatch env ENVOY_ALLOW_REMOTE_NATS | ENVOY_ALLOW_REMOTE_NATS=1 | ENVOY_ALLOW_REMOTE_NATS=1 | yes |$' "$tmp/out" ||
  fail "check does not read Dispatch's ENVOY_ALLOW_REMOTE_NATS back"

# 10. The Compute API answers maxRunDuration as an object -- {"seconds": "259200", "nanos": 0} --
# and never as the duration string it takes on the way in, so check reads its seconds: read
# whole, the value equalled no expectation and every machine's row came back empty.
for name in app generator nats; do
  grep -q "^| $name VM scheduling.maxRunDuration seconds | 259200 | 259200 | yes |$" "$tmp/out" ||
    fail "check does not read the $name machine's run-time limit out of the object the API answers: $(grep maxRunDuration "$tmp/out" | head -n1)"
done
pass "check reads every machine's run-time limit out of the object the Compute API answers"

# 11. check tries every production address on every machine. The addresses were streamed into
# the loop from a pipe, and the ssh of the first address read the rest of that pipe as its own
# standard input, so every address after the first went unchecked.
[ "$(grep -c '^| .* VM to production ' "$tmp/out")" = 6 ] ||
  fail "check printed $(grep -c '^| .* VM to production ' "$tmp/out") production-address rows, not the 2 addresses on each of the 3 machines of this deployment"
for address in 10.139.0.2:5432 10.138.0.10:4222; do
  for name in app generator nats; do
    grep -q "^| $name VM to production $address | refused | refused | yes |$" "$tmp/out" ||
      fail "check did not try $address from the $name machine"
  done
done
[ "$(grep -c '^nc	' "$fake/actions.log")" = 6 ] ||
  fail "check made $(grep -c '^nc	' "$fake/actions.log") connections to production, not the 6 its table claims"
pass "check tries every production address of the deployment file from every machine"

# 4 and 8 again, over a second up of the same run: the stub embedder the first left running
# holds its port, and the instance now holds the database.
first_stub=$(cat "$remote/work/stub-embed.pid")
[ -n "$first_stub" ] || fail "up recorded no pid for the stub embedder"
[ "$(cat "$fake/stub-port")" = "$first_stub" ] ||
  fail "the stub embedder up started does not hold the port it serves on"
: >"$fake/actions.log"
run_vm up --config "$config" --run "$(cat "$LOADTEST_VM_ROOT/current")"
((RC == 0)) || fail "a second up over a running stub embedder failed: see the output above"

# 4. stub_embed_start stops whatever the last start left running: a stub an earlier up left
# holds the port, so the new one answers "address already in use" and writes no certificate.
stop_at=$(line_at 'kill -TERM')
start_at=$(grep -n 'loadgen	stub-embed' "$fake/actions.log" | head -n1 | cut -d: -f1)
[ -n "$stop_at" ] || fail "the second up did not stop the stub embedder the first left running"
[ -n "$start_at" ] && ((stop_at < start_at)) ||
  fail "the second up started its stub embedder (line ${start_at:-none}) before it stopped the one before it (line $stop_at)"
second_stub=$(cat "$remote/work/stub-embed.pid")
[ "$second_stub" != "$first_stub" ] || fail "the second up started no stub embedder of its own (pid $second_stub)"
[ "$(cat "$fake/stub-port")" = "$second_stub" ] ||
  fail "the stub embedder of the second up does not hold the port, so it wrote no certificate of its own"
pass "a second up stops the stub embedder the first left running and starts one that holds the port"

# 8, the other way: the instance holds the database now, so up makes none and says so.
grep -q 'create database' "$fake/actions.log" &&
  fail "a second up made the database again over the one the instance holds"
grep -q 'already holds the database dispatch' "$tmp/out" ||
  fail "a second up does not say the instance already holds the database"
pass "up makes the database when the instance lacks it and makes none when it has it"

# 9. wait_healthz reads `.ok and .nats`, which is what stack.sh's dispatch_healthy reads: the
# body carries no `status` field, and a Dispatch whose database is up but whose NATS is not
# would serve the API while every notification of a step went nowhere. These three run the
# function itself, so a poll's bound is ten seconds here rather than the nine hundred a
# subcommand gives it.
run_fn HEALTHZ_SECONDS=10 HEALTHZ_POLL_SECONDS=5 wait_healthz
((RC == 0)) || fail "wait_healthz did not take an answer of ok and nats for ready: $(cat "$tmp/out")"
grep -q 'answers /healthz ok' "$tmp/out" || fail "wait_healthz said nothing of the answer it took"

printf '{"ok": true, "nats": false, "schema_version": 1337}\n' >"$fake/healthz"
: >"$fake/actions.log"
run_fn HEALTHZ_SECONDS=10 HEALTHZ_POLL_SECONDS=5 wait_healthz
((RC != 0)) || fail "wait_healthz took a Dispatch whose NATS is not up for ready"
grep -q 'did not answer /healthz ok' "$tmp/out" ||
  fail "wait_healthz does not say that Dispatch never answered ok: $(cat "$tmp/out")"
[ "$(grep -c 'healthz' "$fake/actions.log")" -ge 2 ] ||
  fail "wait_healthz read /healthz once and gave up, rather than waiting out its bound"

# A container that is no longer running never will answer, so the wait refuses at once with the
# end of its log, which is where every reason Dispatch refuses to start is written.
printf 'exited\n' >"$fake/docker/$app_target/dispatch.state"
printf 'listen tcp 0.0.0.0:9020: bind: address already in use\n' >"$fake/docker/$app_target/dispatch.log"
: >"$fake/actions.log"
run_fn wait_healthz
((RC != 0)) || fail "wait_healthz waited on a Dispatch container that is no longer running"
grep -q 'the Dispatch container on the app VM is exited' "$tmp/out" ||
  fail "the refusal does not name the container's state: $(cat "$tmp/out")"
grep -q 'address already in use' "$tmp/out" ||
  fail "the refusal does not print the end of the container's log: $(cat "$tmp/out")"
[ "$(grep -c 'healthz' "$fake/actions.log")" = 1 ] ||
  fail "wait_healthz read /healthz $(grep -c 'healthz' "$fake/actions.log") times before it read the container's state, rather than refusing at once"
pass "wait_healthz returns on ok and nats, waits while NATS is not up, and refuses at once, naming the state and the log, when the container has exited"

# ---- (u) no credential of the deployment reaches a command line ----------------------------

# dispatch://INFRA-988. Driving this script against the deployment showed its secrets -- the
# agent token, the listener's token and the Postgres URL with its password -- in the argv of
# `loadgen` on the load machine and of the ssh that carried it there, where `ps -eo args` hands
# them to anything that can list processes on either machine; the embedder's api-key and the
# seed's own phases followed, the key for the whole life of the deployment. Every one of them
# goes in a file of mode 600 now, and loadgen reads it with the -…-file form of its flag
# (guard.go's readCredentials). A command that runs on the load machine reads its file from the
# run's own directory there, written over ssh's standard input (put_file); the seed's phases run
# on this pod and read theirs from the run's directory here, which is removed when the seed
# ends. psql is given a URL with no password in it and takes the password from a passfile.
#
# What this case reads is every argv the fakes record: ssh.log holds each ssh whole, and what
# follows the target is what the machine ran, so that is the machine's own process table too;
# docker.log each docker call, psql.argv each psql call, and loadgen.log every loadgen this pod
# or a machine ran. No credential of the deployment is allowed in any of them: this case runs an
# up, the whole seed and a step, so every command of a deployment's life is in those logs, and
# credentials.log says what each command read its credential file from while it ran.

reset_fake
run_vm up --config "$config"
((RC == 0)) || fail "up failed before the credential case"
run_vm seed --config "$config" --embed-timeout 8h
((RC == 0)) || fail "the seed of the credential case failed: see the output above"
run_vm step --config "$config" 1x 1 --duration 10m
((RC == 0)) || fail "the step of the credential case failed: see the output above"

argv_logs=("$fake/ssh.log" "$fake/docker.log" "$fake/psql.argv" "$fake/loadgen.log")
for log in "${argv_logs[@]}"; do
  [ -s "$log" ] || fail "no fake wrote $log, so this case proves nothing of the commands it records"
done
# The deployment's credentials, each by name, read out of the deployment file itself.
credentials() {
  jq -r '{"agent token": .secrets.dispatch_token, "listener token": .secrets.listener_token,
    "embedder key": .secrets.embed_key, "database password": .cloudsql.password} |
    to_entries[] | "\(.key)\t\(.value)"' "$config"
}
while IFS=$'\t' read -r name value; do
  for log in "${argv_logs[@]}"; do
    ! grep -qF -- "$value" "$log" ||
      fail "the deployment's $name reached a command line (${log##*/}): $(grep -m1 -F -- "$value" "$log")"
  done
  ! grep -rqF -- "$value" "$(run_dir)" ||
    fail "the deployment's $name reached a file the run keeps: $(grep -rlF -- "$value" "$(run_dir)" | head -n1)"
done < <(credentials)
pass "no token, no api-key and no password of the deployment reaches the command line of an ssh, a docker, a psql or a loadgen of an up, a seed and a step, or a file the run keeps"

# The run works all the same: each credential reached the load machine in a file of its own, and
# loadgen was given that file.
run_secrets=$remote/work/runs/$(cat "$LOADTEST_VM_ROOT/current")
line=$(tail -n1 "$fake/loadgen-run.log")
while IFS=$'\t' read -r flag file value; do
  [[ $line == *"$flag $run_secrets/$file"* ]] ||
    fail "step's loadgen run has no '$flag $run_secrets/$file': $line"
  [ -f "$run_secrets/$file" ] || fail "the run wrote no $run_secrets/$file on the load machine"
  [ "$(cat "$run_secrets/$file")" = "$value" ] ||
    fail "$run_secrets/$file holds '$(cat "$run_secrets/$file")', not the deployment file's own value"
  [ "$(stat -c %a "$run_secrets/$file")" = 600 ] ||
    fail "$run_secrets/$file is mode $(stat -c %a "$run_secrets/$file") on the load machine, not 600"
done <<EOF
-token-file	dispatch-token	dispatch-secret
-listener-token-file	listener-token	listener-secret
-pg-file	pg-url	postgres://dispatch:pg-secret@10.139.0.3:5432/dispatch?sslmode=require
EOF
for flag in -token -listener-token -pg; do
  [[ " $line " != *" $flag "* ]] ||
    fail "step's loadgen run still gives $flag as a value: $line"
done
pass "step gives loadgen each credential in a file of mode 600 under the run's own directory on the load machine"

# psql is given the instance's password in a passfile of mode 600, and one for every call it
# made: a call without one would reach the instance as the login's own user and fail there.
calls=$(grep -c . "$fake/psql.argv" || true)
((calls > 0)) || fail "the credential case ran no psql"
[ "$(grep -c . "$fake/psql.passfile")" = "$calls" ] ||
  fail "$calls psql calls, $(grep -c . "$fake/psql.passfile") of them with a PGPASSFILE"
[ "$(awk '$2 != 600' "$fake/psql.passfile" | wc -l)" = 0 ] ||
  fail "a psql call read a passfile that is not mode 600: $(awk '$2 != 600' "$fake/psql.passfile" | head -n1)"
passfile=$(awk 'NR == 1 {print $1}' "$fake/psql.passfile")
[ ! -e "$passfile" ] || fail "the passfile $passfile is still on this pod after the call that read it"
pass "psql takes the instance's password from a passfile of mode 600, written for the call and removed with it"

# credential_read CMD FLAG: the path, the mode and the content of the file the loadgen
# subcommand CMD read FLAG from, as they were at the moment it read them (credentials.log).
credential_read() {
  awk -F'\t' -v c="$1" -v f="$2" '$1 == c && $2 == f { print $3 "\t" $4 "\t" $5; exit }' "$fake/credentials.log"
}

# The embedder's key has a file form now, so the stub embedder takes it like every other
# credential of the load machine: a key given as -key would be in the stub's argv, and in the
# ssh's, for as long as the deployment stands.
embed_key=$(jq -r '.secrets.embed_key' "$config")
IFS=$'\t' read -r path mode content < <(credential_read stub-embed -key-file) || true
[ -n "${path:-}" ] || fail "the stub embedder was given no -key-file: $(grep 'stub-embed' "$fake/loadgen.log" | head -n1)"
[ "$path" = "$run_secrets/embed-key" ] ||
  fail "the stub embedder read its key from $path, not from the run's own directory on the load machine ($run_secrets/embed-key)"
[ "$content" = "$embed_key" ] || fail "$path held '$content', not the deployment file's own embedder key"
[ "$mode" = 600 ] || fail "$path is mode $mode on the load machine, not 600"
grep -q -- 'stub-embed -key ' "$fake/loadgen.log" &&
  fail "the stub embedder still takes its key as a value: $(grep -m1 -- 'stub-embed' "$fake/loadgen.log")"
pass "the stub embedder reads its api-key from a file of mode 600 on the load machine"

# The seed's own phases run on this pod, so each reads its credential from a file of mode 600
# under the run's directory here, and every one of those files is gone once the seed has ended:
# the run directory is what this pod keeps of the run.
pg_url=postgres://dispatch:pg-secret@10.139.0.3:5432/dispatch?sslmode=require
while IFS=$'\t' read -r phase flag want; do
  IFS=$'\t' read -r path mode content < <(credential_read "$phase" "$flag") || true
  [ -n "${path:-}" ] || fail "the seed's $phase phase was given no $flag: $(grep -m1 -- "$phase" "$fake/loadgen.log")"
  [[ $path == "$(run_dir)"/* ]] ||
    fail "$phase read $flag from $path, which is not under the run's own directory on this pod"
  [ "$content" = "$want" ] || fail "$phase's $flag held '$content', not the deployment file's own value"
  [ "$mode" = 600 ] || fail "$phase's $flag ($path) is mode $mode, not 600"
  [ ! -e "$path" ] || fail "$path is still on this pod after the seed that wrote it"
done <<EOF
loop-seed-check	-pg-file	$pg_url
loop-seed-export	-pg-file	$pg_url
seed-finish	-pg-file	$pg_url
seed-finish	-token-file	dispatch-secret
EOF
pass "the seed's own phases read their credentials from files of mode 600 under the run's directory on this pod, each removed when the seed ends"

# down removes the run's credential files from the load machine and says so: a deployment that
# is turned off should leave no token, no key and no database password on a disk that outlives
# it. The containers' environment files are the machines' own and stay with them.
[ -d "$run_secrets" ] || fail "the run wrote no credential directory on the load machine before down"
run_vm down --config "$config"
((RC == 0)) || fail "down failed after the credential case: see the output above"
[ ! -e "$run_secrets" ] ||
  fail "down left the run's credential files on the load machine: $(ls -A "$run_secrets" | tr '\n' ' ')"
grep -q "credential files are removed from the $(jq -r .load_vm "$config") VM" "$tmp/out" ||
  fail "down does not say that it removed the run's credential files: $(cat "$tmp/out")"
while IFS=$'\t' read -r name value; do
  ! grep -rqF -- "$value" "$remote/work" ||
    fail "down left the deployment's $name on the load machine: $(grep -rlF -- "$value" "$remote/work" | head -n1)"
done < <(credentials)
pass "down removes the run's credential files from the load machine, leaving none of its credentials there, and says it did"

printf 'PASS: packages/envoy/loadtest/vm/vm.test.sh\n'
