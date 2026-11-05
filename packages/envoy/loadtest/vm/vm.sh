#!/usr/bin/env bash
# vm.sh: the Dispatch load test on the separate deployment of INFRA-988 -- the virtual machines
# its deployment file names and a Cloud SQL instance of its own (dispatch://INFRA-1051) --
# driven from a Legion worker pod over ssh. cluster/cluster.sh is the model: the same
# subcommands, the same flag parsing and refusals, the same run directory; only the deployment
# changes, from a Kubernetes namespace to a set of virtual machines and a managed instance.
#
#   vm.sh up --config FILE [--run ID] [--loadgen PATH]
#       Starts every container of the deployment by image digest, each on the machine the
#       deployment file puts it on, with the CPU and memory limits of the specification below
#       and nothing silently defaulted, installs loadgen and the stub embedder on the load
#       machine, makes the database cloudsql.database names when the instance does not hold it
#       yet, and waits until Dispatch answers /healthz. With no --run it makes the run
#       directory and names the run in <root>/current; with --run it starts the
#       deployment again under a run this pod started, and leaves <root>/current and that run's
#       state.json as they are.
#   vm.sh check --config FILE [--run ID]
#       The read-back of acceptance criterion A1, as a markdown table of expected beside found:
#       every container's CPU, memory and image, Dispatch's network mode, the instance's own
#       settings from the Cloud SQL Admin API, every machine's run-time limit
#       from the Compute API, every endpoint setting of every container, and that each
#       production address the deployment file names is refused from every machine. Exit 1 on
#       any difference.
#   vm.sh seed --config FILE [--run ID] [--embed-timeout 8h] [--workers 8]
#       loop-seed-plan, loop-seed-write (on the load machine), loop-seed-check, loop-seed-export
#       and seed-finish with the embedding gate (-url), each phase's wall clock and the gate's
#       coverage progress recorded in <run>/seed.json; then the template database.
#   vm.sh reset --config FILE [--run ID]
#       The whole reset of decision D6, in order, its duration recorded.
#   vm.sh step --config FILE [--run ID] NAME FACTOR [--duration 10m] [--warmup 30s]
#       One step: the reset, the VM sampler, loadgen run at FACTOR, the profile fetched from the
#       machine Dispatch runs on, cgroup-merge, cloudsql-cpu and tables.
#   vm.sh series --config FILE [--run ID] [--duration 10m] [--long 60m] [--long-warmup 5m]
#       The baseline, 1x and 2x steps at --long after a --long-warmup warm-up, then the grid 3,
#       4, 6, 8, 12, 16 and 20 at --duration, stopping at the first failing step, then the
#       refinement loadgen curves asks for, until refine.txt is empty.
#   vm.sh collect --config FILE [--run ID]
#       loadgen curves -series loop over the run's steps, in the order they ran, into <run>/curves.
#   vm.sh down --config FILE [--run ID]
#       Stops every container and the stub embedder. The machines and the instance are left
#       alone: they are dispatch://INFRA-1051's to create and to remove.
#
# The container specification (R3 and the plan's departure 7), which up starts and check reads
# back. Every container is started by image digest, never by a tag:
#
#   container           CPU    memory    other
#   dispatch            16     32 GiB    DISPATCH_PPROF_PORT=6060, host networking,
#                                        the entrypoint envoy-dispatch
#   auth-proxy          200m   256 MiB
#   dispatch-listener   500m   512 MiB
#   nats                none   none      JetStream, its store on its machine's disk
#   hub-listener        none   none
#   load-listener-1..4  500m   512 MiB
#
# Which machine each container runs on is no part of that table: the deployment file's
# .containers says it, one machine name per container, because the deployment has already moved
# NATS and the hub listener onto a machine of their own once and may move them again. .vms takes
# any number of machines, each with the address the others reach it at and its Compute instance
# name. Two of them have a role this script's own work depends on, and the file names both:
#
#   the load machine, .load_vm: loadgen is installed there, `loop-seed-write` and every
#   `loadgen run` run there, the stub embedder serves from there, <paths.remote_work> is there,
#   and the near end of a step's profiling tunnel listens there;
#   the machine Dispatch runs on, which .containers.dispatch names, so the file says it once and
#   cannot say it twice over: it is the far end of that tunnel, the machine a step fetches its
#   profiles from, and the machine the stub embedder's certificate is put on.
#
# A container .containers does not place, a machine no container runs on, and a .load_vm that is
# not one of the machines are each refused by name before anything is started.
#
# Dispatch runs with host networking because its profiling port binds 127.0.0.1 only
# (cmd/dispatch/pprof.go), so that port answers on its own machine's loopback and nowhere else.
# Two readers need it, and each reaches it its own way. The profiles a step keeps are fetched
# after the load from the machine Dispatch runs on, over ssh. The in-run sampler reads
# Dispatch's goroutines, its pool acquire waiters and its heap while the step runs
# (sample.go:325, :453 and :508), and it runs inside `loadgen run` on the load machine, so step
# opens a tunnel for the life of the load -- `ssh -N -L <ports.pprof_tunnel>:127.0.0.1:<ports.
# pprof> <the machine Dispatch runs on>`, run on the load machine -- and gives loadgen
# `-pprof http://127.0.0.1:<ports.pprof_tunnel>`. The step proves that tunnel answers
# /debug/pprof/goroutine?debug=1 before it sends any load and refuses itself by name when it does
# not: pass criterion (e), "Dispatch's pool acquire waiters p50 = 0", is FAIL for want of a
# measurement (curves.go passCriteria), so a step whose sampler cannot reach that port measures
# nothing worth keeping. The tunnel is closed when the step ends, however it ends. It is the
# harness's own and not the deployment's: no row of check's table reads it. The command that
# opens it is the deployment file's (pprof_tunnel_command), because it runs on the load machine
# and not on this pod, so whatever credential that machine needs to reach the other is the
# file's to give it.
#
# Every machine is Container-Optimized OS, where /var is mounted noexec, so the loadgen copy
# under <remote_root> cannot be executed where a plain mkdir puts it and every step fails on its
# first exec (dispatch://INFRA-49). Every up gives every machine's remote root a bind mount of
# /mnt/stateful_partition/<paths.stateful_dir>, which is COS's own writable partition, remounts
# it exec,nosuid,nodev and gives it to the ssh user; a root that already carries the mount is
# remounted rather than mounted again. up then reads the mount table back and is refused by name,
# before a single container is started, when the root is not mounted exec, because a load machine
# that cannot exec loadgen runs no step. Like the tunnel, the mount is the harness's own and not
# the deployment's: no row of check's table reads it.
#
# Every ssh of this script is OS Login's and never this pod's own key. The machines carry
# enable-oslogin and block-project-ssh-keys, and a Legion worker pod's Google identity is a
# federated Kubernetes principal with no POSIX profile of its own, so no key of this pod's
# authenticates anywhere. Before it reaches a machine, each subcommand mints an access token for
# the deployment's login_service_account (which holds roles/compute.osAdminLogin on the
# machines) with this pod's own metadata token, imports the run's public key for that account
# with an expiry, reads that account's POSIX user name from its login profile, and runs every
# ssh as `ssh -F /dev/null -i <the run's private key> <that user>@<the machine's host>`. The
# keypair is the run's own, made once under the run directory, mode 600, and copied nowhere; the
# minted token lives in one shell variable and reaches no log, no command line another process
# can list and no file the run collects. .vms.<name> therefore names an address and a Compute
# instance alone: the user is OS Login's and no file names it.
#
# Dispatch's identity is DISPATCH_IDENTITY=header:X-Dispatch-User with
# DISPATCH_IDENTITY_HEADER_TRUSTED=1 and a shared agent token, as stack.sh's start_dispatch sets
# them: the deployment has no front door, no Tailscale and no OAuth, and the report says so. Its
# embedder is loadgen stub-embed on the load machine: DISPATCH_EMBEDDER=azure,
# DISPATCH_AZURE_EMBED_ENDPOINT at the stub's https address, DISPATCH_AZURE_EMBED_KEY,
# DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE=1000000 (production's quota) and SSL_CERT_FILE at the
# certificate the stub wrote.
#
# Nothing of the deployment is named here. Every machine, address, port, image, credential and
# production address comes from the deployment file --config names, because the deployment's own
# shape has already changed twice under this issue (a third machine for NATS and the hub
# listener in dispatch://INFRA-1051's later version) and will change again. The file is one JSON
# object, and vm/deployment.example.json beside this script is one: every key read below, each
# optional one at the default named below, with placeholder machines, addresses and credentials
# throughout. Copy that file and edit it; vm.test.sh holds it to this script, so a key added here
# and not to it fails that test.
#
# cloudsql.expect is read as it is written: each key is a path into the Cloud SQL Admin API's
# answer and each value what that path must read, so a change of production's instance is a
# change of the deployment file and not of this script. production_addresses are the addresses R2
# forbids; check proves each one refuses a connection from every machine. The optional keys and
# their defaults: ssh_command "ssh", ports.pprof_tunnel 16060 (the port on the load machine that
# a step's profiling tunnel listens on), pprof_tunnel_command
# "ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes" (the command that opens it, run on the
# load machine; the forward and the machine Dispatch runs on are appended to it), paths.loadgen
# "$LOADTEST_ROOT/bin/loadgen", paths.remote_loadgen "<remote_root>/loadgen", paths.nats_store
# "<remote_root>/nats", paths.cert "<remote_root>/stub-embed.pem", paths.stateful_dir
# "<remote_root's own last element>" (the directory under /mnt/stateful_partition that
# remote_root is a bind mount of), paths.cgroup "/sys/fs/cgroup/system.slice/docker-%s.scope",
# token_url, sqladmin, compute, iamcredentials and oslogin (the Google endpoints, for a test
# server), and the whole of load (below).
#
# The load a step sends, from R5. The baseline is dispatch://INFRA-598's 1x, which every step
# carries as it is; the loop's own rates are multiplied by the step's factor, so a step at factor
# F sends baseline.events + loop.writes x F events a second, baseline.requests + loop.reads x F
# requests a second, baseline.sessions flat sessions and loop.unit_starts x F unit starts a
# second. load's defaults, each from R5: {"baseline": {"events": 4, "requests": 60, "sessions":
# 400, "event_mix": "production-2026-10-26", "route_mix": ""}, "loop": {"writes": 3.5, "reads":
# 38.5, "unit_starts": 1.75, "event_mix": "spec-loop-2026-11-03", "route_mix":
# "routes-spec-loop-2026-11-03.txt"}, "unit_life": "5m", "driver_topics": 614,
# "receipt_deadline": "1s", "session_delivery": "consumer"}. A step at factor 0 is the baseline
# alone and takes the baseline's mixes; every step above it takes the loop's. The sessions take
# their messages through the listener's relay of their consumers, which is the delivery R5 names
# ("each listener uses the consumer delivery the Envoy plugin uses").
#
# The run directory is ${LOADTEST_VM_ROOT:-$TMPDIR/dispatch-loadtest-vm}/<run id>: state.json
# (mode 600, the deployment file's values that the run was started with), the run's OS Login
# keypair, cgroup.jsonl, steps/, curves/ and logs/. up names the run in <root>/current, and every
# later subcommand takes that run unless --run names another.
#
# A step whose machine is stopped is discarded, never recorded: each machine stops itself 72
# hours after it starts (R11), and a machine that is gone is not a Dispatch failure. step reads
# every machine's Compute API status before the reset and again after the load; any one of them
# not RUNNING removes the step's directory, says which machine stopped, and ends the step with
# status 3, which series reports and stops on.
#
# Needs ssh, ssh-keygen, curl, jq, psql, nc (on the machines), tar, gzip and loadgen. Every
# subcommand needs --config; none creates, resizes or removes a machine or the instance.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/../common.sh"
export LC_ALL=C TZ=UTC

VM_ROOT=${LOADTEST_VM_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest-vm}
# R11's run-time limit: Google stops each VM 72 hours after it starts, and check reads it back
# from the Compute API's .scheduling.maxRunDuration, which is an object -- {"seconds": "259200",
# "nanos": 0} -- rather than the duration string the API takes on the way in.
MAX_RUN_DURATION=259200
# How long a wait on Dispatch's /healthz may take, and how long between two polls of it.
HEALTHZ_SECONDS=900
HEALTHZ_POLL_SECONDS=5
# How long one ssh may take: a docker call, a file copy, a read. The load itself is bounded by
# the step's own deadline, not by this.
SSH_SECONDS=300
# The slack a step's wait gives loadgen run over the bound it was started with (its warm-up,
# window, grace and drain), as cluster.sh's RUNNER_RUN_SLACK_SECONDS does.
RUN_SLACK_SECONDS=900
# How long the stub embedder is given to write its certificate before up gives up on it.
STUB_EMBED_SECONDS=60
# How long the step's profiling tunnel is given to answer before the step is refused, and how
# long between two reads through it.
TUNNEL_SECONDS=60
TUNNEL_POLL_SECONDS=2
# The port the tunnel listens on, on the load machine, and the command that opens it there. Both
# are the deployment file's to change (ports.pprof_tunnel, pprof_tunnel_command): that machine's
# own ssh may need a key, a jump host or a port of its own, and a test server stands in for it.
# ExitOnForwardFailure is what makes a port already in use a refusal rather than a tunnel that
# answers nothing.
PPROF_TUNNEL_PORT_DEFAULT=16060
PPROF_TUNNEL_COMMAND_DEFAULT="ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes"
# COS's own writable partition, which is where the remote root's backing directory is made: /var
# is mounted noexec on these machines and this partition is not, so a bind mount of a directory
# under it is what makes loadgen runnable under <remote_root> (dispatch://INFRA-49). The options
# the remount gives that mount: exec because loadgen runs from there, nosuid and nodev because
# nothing under it is a device or needs a set-user-id bit.
STATEFUL_PARTITION=/mnt/stateful_partition
REMOTE_ROOT_MOUNT_OPTIONS=exec,nosuid,nodev
# The Google endpoints and the metadata server's token, each overridable in the deployment file
# so a test server can stand in for them (cloudsqlcpu.go does the same).
TOKEN_URL_DEFAULT=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token
SQLADMIN_DEFAULT=https://sqladmin.googleapis.com
COMPUTE_DEFAULT=https://compute.googleapis.com
IAMCREDENTIALS_DEFAULT=https://iamcredentials.googleapis.com
OSLOGIN_DEFAULT=https://oslogin.googleapis.com
# How long an imported OS Login key is good for. Every subcommand that reaches a machine imports
# the run's key again at its start, so this has only to outlast one subcommand's own work -- the
# longest of them is a series of hour-long steps or a seed with an eight-hour embedding gate --
# and a day is that with room to spare while leaving no key of a finished run behind for long.
OSLOGIN_KEY_SECONDS=86400

# CONTAINERS: every container of the deployment, in start order, one line each:
#
#   ROLE  IMAGE KEY  CPUS  MEMORY
#
# CPUS and MEMORY are docker's own words (--cpus, --memory); "-" is a container with no limit,
# which docker reports as 0 and which check expects as 0. NATS and the hub listener have none, as
# production's hub VM gives its two processes the whole machine (envoy-infra gcp/cloudconfig.go).
# Which machine each one runs on is the deployment file's .containers, not this table's: see
# check_placement.
CONTAINERS=(
  "nats nats - -"
  "auth-proxy proxy 0.2 256m"
  "hub-listener listener - -"
  "load-listener-1 listener 0.5 512m"
  "load-listener-2 listener 0.5 512m"
  "load-listener-3 listener 0.5 512m"
  "load-listener-4 listener 0.5 512m"
  "dispatch-listener listener 0.5 512m"
  "dispatch dispatch 16 32g"
)
# The four listeners the loop's sessions register on: the unit sessions round-robin over all but
# the last and the driver session takes the last (run.go -loop-listeners).
LOAD_LISTENERS=(load-listener-1 load-listener-2 load-listener-3 load-listener-4)
# Everything that holds a connection to the database, in the order it is stopped: the reset and
# the template rename both stop all of it first, so nothing holds a connection or a consumer.
DB_HOLDERS=(dispatch dispatch-listener "${LOAD_LISTENERS[@]}" hub-listener)
# Dispatch's ephemeral storage limit, the setting that exists because Autopilot's 1Gi default
# evicted production's Dispatch (agent-infra dispatch/Pulumi.prod.yaml appEphemeralStorage, 8Gi).
# It has no form on these machines and is not set: docker takes such a limit as
# --storage-opt size=, which its own daemon refuses here -- "--storage-opt is supported only for
# overlay over xfs with 'pquota' mount option", measured on the deployment's Container-Optimized
# OS machines, whose overlay is over ext4. So Dispatch's writable layer is bounded by the
# machine's own disk rather than by 8 GiB, which is a difference from production the report
# names, and check reads back the network mode alone.
# Where the stub embedder's certificate is mounted inside the Dispatch container (SSL_CERT_FILE).
CERT_IN_CONTAINER=/etc/ssl/stub-embed.pem
# The environment variables whose values are credentials: check proves each one is set and never
# prints or compares its value.
SECRET_VARS=" DISPATCH_AGENT_TOKEN ENVOY_TOKEN ENVOY_API_TOKEN DISPATCH_AZURE_EMBED_KEY DATABASE_URL "
# The profiles a step fetches from the machine Dispatch runs on, as `loadgen run` fetches them
# when it can reach the profiling port itself (run.go:868-878): the pprof path and the file it
# is written to.
PPROF_FETCHES=(
  "profile?seconds=%s cpu.pprof"
  "goroutine?debug=2 goroutines.txt"
  "heap heap.pprof"
  "heap?debug=1 heap.txt"
)

# The deployment's own shape, read out of the deployment file by check_placement before any
# command does anything: every machine in the file's order, the machine each container runs on,
# the machine the load runs from and the machine Dispatch runs on.
VM_NAMES=()
declare -A CONTAINER_VM=()
LOAD_VM=""
DISPATCH_VM=""
# The OS Login user every ssh runs as and the run's own private key it runs with, both set by
# oslogin_ready before the first ssh of a subcommand that reaches a machine.
SSH_USER=""
SSH_KEY=""

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
elapsed_since() { awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }'; }

# valid KIND VALUE: VALUE has KIND's shape. Every value that reaches a command line, a URL or a
# docker argument passes one of these first.
valid() {
  local re
  case $1 in
    run) re='^vm-[0-9]{8}t[0-9]{6}-[0-9a-f]{4}$' ;;
    image) re='^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$' ;;
    step) re='^[A-Za-z0-9._-]{1,63}$' ;;
    factor) re='^[0-9]+(\.[0-9]+)?$' ;;
    duration) re='^[1-9][0-9]*(s|m|h)$' ;;
    port) re='^[1-9][0-9]{0,4}$' ;;
    host) re='^[A-Za-z0-9]([A-Za-z0-9.:_-]{0,252}[A-Za-z0-9])?$' ;;
    # A machine's name is a key of .vms and reaches this script's own jq paths, so it is what a
    # jq field name may be and no more.
    machine) re='^[A-Za-z_][A-Za-z0-9_]{0,62}$' ;;
    service_account) re='^[A-Za-z0-9][A-Za-z0-9._-]{0,127}@[A-Za-z0-9][A-Za-z0-9.-]{0,252}$' ;;
    name) re='^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$' ;;
    path) re='^/[A-Za-z0-9._/-]{0,255}$' ;;
    address) re='^[A-Za-z0-9]([A-Za-z0-9.:_-]{0,252}[A-Za-z0-9])?:[1-9][0-9]{0,4}$' ;;
    number) re='^[0-9]+(\.[0-9]+)?$' ;;
    *) die "valid: no kind $1" ;;
  esac
  [[ $2 =~ $re ]]
}

# duration_seconds D: a Go duration of one unit (90s, 10m, 1h) in seconds.
duration_seconds() {
  local n=${1%?}
  case $1 in
    0) echo 0 ;;
    *s) echo "$n" ;;
    *m) echo $((n * 60)) ;;
    *h) echo $((n * 3600)) ;;
    *) die "'$1' is not a whole number of s, m or h" ;;
  esac
}

# ---- arguments ----------------------------------------------------------------------------------

declare -A OPT=()
POS=()

# parse "VALUE FLAGS" "BOOLEAN FLAGS" ARGS...: OPT[flag] and POS, then the required --config into
# CONFIG. Every subcommand calls it first, so each refuses a missing deployment file before it
# reaches a VM.
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
          die "$CMD takes no $1 (vm.sh with no arguments prints the usage)"
        fi
        ;;
      *)
        POS+=("$1")
        shift
        ;;
    esac
  done
  [ -n "${OPT[config]:-}" ] || die "$CMD: --config is required: name the deployment's JSON file (there is no default; the deployment is dispatch://INFRA-1051's, and nothing of it is named in this script)"
  CONFIG=$(abs_file --config "${OPT[config]}")
  jq -e . "$CONFIG" >/dev/null 2>&1 || die "$CMD: --config $CONFIG is not readable JSON"
  check_placement
}

# positional N WHAT: refuse anything but N positional arguments.
positional() {
  [ ${#POS[@]} -eq "$1" ] || die "$CMD takes $2 (got ${#POS[@]} arguments: ${POS[*]:-none})"
}

# ---- the deployment file -------------------------------------------------------------------------

# cfg FILTER [DEFAULT]: one value of the deployment file. Without a DEFAULT, a missing value is
# refused naming the filter, so no command runs half configured.
cfg() {
  local v
  v=$(jq -r "$1 // empty" "$CONFIG") || die "reading $1 from $CONFIG failed"
  if [ -z "$v" ]; then
    [ $# -ge 2 ] || die "the deployment file $CONFIG names no $1"
    v=$2
  fi
  printf '%s\n' "$v"
}

# cfg_valid KIND FILTER [DEFAULT]: cfg, refused when the value is not of KIND's shape.
cfg_valid() {
  local v
  v=$(cfg "$2" "${3-}")
  valid "$1" "$v" || die "the deployment file $CONFIG gives $2 as '$v', which is not a $1"
  printf '%s\n' "$v"
}

# vm_host VM, vm_instance VM: the address the other machines reach a machine at, and its Compute
# instance name. A machine's name is one of .vms's own keys, which check_placement has read and
# proven before any of this is called.
vm_host() { cfg_valid host ".vms.$1.host"; }
vm_instance() { cfg_valid name ".vms.$1.instance"; }

# vm_ssh VM: the ssh target of a machine. The user is OS Login's and the same on every machine of
# the project, so the deployment file names an address alone (oslogin_ready).
vm_ssh() {
  [ -n "$SSH_USER" ] || die "vm_ssh: no OS Login user yet; oslogin_ready runs before the first ssh of a subcommand"
  printf '%s@%s\n' "$SSH_USER" "$(vm_host "$1")"
}

# vm_keys: every machine the deployment file names, in the file's own order.
vm_keys() { jq -r '.vms // {} | keys_unsorted[]' "$CONFIG"; }

# container_vm_configured ROLE: the machine the deployment file starts ROLE on, empty when it
# places ROLE nowhere. check_placement is what reads it; everything else takes CONTAINER_VM.
container_vm_configured() {
  jq -r --arg role "$1" ".containers[\$role] // empty" "$CONFIG" ||
    die "reading .containers from $CONFIG failed"
}

# check_placement: the deployment's machines and the containers on them, out of the deployment
# file and into VM_NAMES, CONTAINER_VM, LOAD_VM and DISPATCH_VM. Every subcommand runs it before
# it does anything, because every one of them walks the machines: a container the file places
# nowhere or places on a machine it does not name, a machine no container runs on, a container
# this script does not run, and a load machine that is not one of the machines are each refused
# here, by name, with nothing started. A machine no container runs on is a refusal and not a
# shrug because this script would then neither mount it, nor sample it, nor stop anything on it,
# and the file's own reading of the deployment would be wrong in a way no later command tells
# anyone about.
check_placement() {
  local role vm name all used=" " unused="" extra=""
  mapfile -t VM_NAMES < <(vm_keys)
  ((${#VM_NAMES[@]})) ||
    die "the deployment file $CONFIG names no machine: .vms holds one object per machine, each with the host the others reach it at and its Compute instance name"
  for name in "${VM_NAMES[@]}"; do
    valid machine "$name" ||
      die "the deployment file $CONFIG names a machine '$name': a machine's name is a key of .vms and reaches this script's jq paths, so it holds letters, digits and '_' alone and does not start with a digit"
  done
  all=" ${VM_NAMES[*]} "
  CONTAINER_VM=()
  for role in "${CONTAINERS[@]%% *}"; do
    vm=$(container_vm_configured "$role")
    [ -n "$vm" ] ||
      die "the deployment file $CONFIG does not say which machine the container $role runs on: give .containers a \"$role\" naming one of its machines (${VM_NAMES[*]})"
    [[ $all == *" $vm "* ]] ||
      die "the deployment file $CONFIG runs the container $role on the machine '$vm', which is none of its machines (${VM_NAMES[*]})"
    CONTAINER_VM[$role]=$vm
    [[ $used == *" $vm "* ]] || used+="$vm "
  done
  for name in "${VM_NAMES[@]}"; do
    [[ $used == *" $name "* ]] || unused+="${unused:+, }$name"
  done
  [ -z "$unused" ] ||
    die "the deployment file $CONFIG names the machine $unused, which no container of this deployment runs on: give it one in .containers, or drop it from .vms"
  while read -r role; do
    [ -n "$role" ] || continue
    [ -z "${CONTAINER_VM[$role]+set}" ] || continue
    extra+="${extra:+, }$role"
  done < <(jq -r '.containers // {} | keys_unsorted[]' "$CONFIG")
  [ -z "$extra" ] ||
    die "the deployment file $CONFIG places the container $extra, which this deployment does not run: its containers are ${CONTAINERS[*]%% *}"
  LOAD_VM=$(cfg_valid machine .load_vm)
  [[ $all == *" $LOAD_VM "* ]] ||
    die "the deployment file $CONFIG gives load_vm as '$LOAD_VM', which is none of its machines (${VM_NAMES[*]}): load_vm names the machine the load runs from, where loadgen, the stub embedder and a step's profiling tunnel live"
  DISPATCH_VM=${CONTAINER_VM[dispatch]}
}

# container_vm ROLE: the machine ROLE runs on, as check_placement read it.
container_vm() {
  [ -n "${CONTAINER_VM[$1]:-}" ] || die "no container $1 in the deployment's container table"
  printf '%s\n' "${CONTAINER_VM[$1]}"
}

port_of() { cfg_valid port ".ports.$1"; }

# load_listener_port N: the port of the Nth load listener (1-based), from ports.load_listeners.
load_listener_port() {
  local v
  v=$(jq -r ".ports.load_listeners[$(($1 - 1))] // empty" "$CONFIG")
  [ -n "$v" ] || die "the deployment file $CONFIG gives no ports.load_listeners[$(($1 - 1))]: name a port for each of the ${#LOAD_LISTENERS[@]} load listeners"
  valid port "$v" || die "the deployment file $CONFIG gives ports.load_listeners[$(($1 - 1))] as '$v', which is not a port"
  printf '%s\n' "$v"
}

remote_root() { cfg_valid path .paths.remote_root; }
nats_store() { cfg_valid path .paths.nats_store "$(remote_root)/nats"; }
cert_path() { cfg_valid path .paths.cert "$(remote_root)/stub-embed.pem"; }
remote_loadgen() { cfg_valid path .paths.remote_loadgen "$(remote_root)/loadgen"; }
remote_work() { cfg_valid path .paths.remote_work "$(remote_root)/work"; }

# pg_url DATABASE: the direct Postgres URL of one database on the instance, which loadgen takes
# as -pg and psql as its connection string.
pg_url() {
  printf 'postgres://%s:%s@%s:%s/%s?sslmode=%s\n' \
    "$(cfg .cloudsql.user)" "$(cfg .cloudsql.password)" "$(cfg_valid host .cloudsql.host)" \
    "$(cfg_valid port .cloudsql.port 5432)" "$1" "$(cfg .cloudsql.sslmode require)"
}

# Each endpoint of the deployment, at the address of the machine the deployment file runs that
# container on: a machine that moves in the file moves every URL of it with it.
dispatch_url() { printf 'http://%s:%s\n' "$(vm_host "$DISPATCH_VM")" "$(port_of dispatch)"; }
listener_url() { printf 'http://%s:%s\n' "$(vm_host "$(container_vm dispatch-listener)")" "$(port_of listener)"; }
nats_url() { printf 'nats://%s:%s\n' "$(vm_host "$(container_vm nats)")" "$(port_of nats)"; }
nats_mon_url() { printf 'http://%s:%s\n' "$(vm_host "$(container_vm nats)")" "$(port_of nats_monitor)"; }
pprof_url() { cfg .pprof_url "http://$(vm_host "$DISPATCH_VM"):$(port_of pprof)"; }
stub_embed_url() { printf 'https://%s:%s\n' "$(vm_host "$LOAD_VM")" "$(port_of stub_embed)"; }

# ---- ssh --------------------------------------------------------------------------------------

# ssh_cmd: the command that opens a shell on a machine, its arguments split on spaces, into SSH,
# with the run's own OS Login key and -F /dev/null after them: no ~/.ssh/config of this pod's
# changes where an ssh of this script goes or who it goes as. It is also what loadgen vm-sample
# is given, so the sampler reaches a machine exactly as this script does.
#
# The host keys go in a file of the run's own, under accept-new: these machines are made by a
# merge and this pod has never met them, so there is no key to have known beforehand, and the
# pod's own known_hosts must not decide what this run trusts. accept-new takes each machine's key
# the first time and refuses it if it changes while the run lasts, which is the part worth
# refusing: a machine replaced mid-series is not the machine the earlier steps measured.
ssh_cmd() {
  local raw
  raw=$(cfg .ssh_command ssh)
  read -r -a SSH <<<"$raw"
  ((${#SSH[@]})) || die "the deployment file $CONFIG gives an empty ssh_command"
  [ -n "$SSH_KEY" ] || die "ssh_cmd: no OS Login key yet; oslogin_ready runs before the first ssh of a subcommand"
  SSH+=(-F /dev/null -i "$SSH_KEY"
    -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$RUN_DIR/known_hosts")
}

# remote_argv ARGS...: ARGS as one word the machine's shell parses back into exactly these
# arguments, each in single quotes with its own quotes escaped. ssh takes no argument vector: it
# joins what it is given with spaces and the machine's shell splits the result again, so an
# argument of this script that holds a space, a quote or a pipe arrives as several. Every remote
# command of this script goes through here, which is why a `sh -c '<script>' sh <path>` reaches
# the machine as the script and the path rather than as a dozen words.
remote_argv() {
  local arg out=
  for arg in "$@"; do
    out+=" '${arg//\'/\'\\\'\'}'"
  done
  printf '%s' "${out# }"
}

# on_vm VM ARGS...: ARGS run on VM, bounded. Its standard output and status are the command's.
on_vm() {
  local vm=$1
  shift
  ssh_cmd
  timeout "$SSH_SECONDS" "${SSH[@]}" "$(vm_ssh "$vm")" "$(remote_argv "$@")"
}

# on_vm_for SECONDS VM ARGS...: on_vm with a bound of its own, for the load and the seed.
on_vm_for() {
  local seconds=$1 vm=$2
  shift 2
  ssh_cmd
  timeout "$seconds" "${SSH[@]}" "$(vm_ssh "$vm")" "$(remote_argv "$@")"
}

# put_file VM FILE DEST: FILE written on VM as DEST, mode 600, its directory made first. The file
# goes over ssh's standard input, so a credential in it never reaches a command line. It is
# written beside DEST and renamed onto it, because a plain write onto a file that is running
# answers ETXTBSY ("Text file busy") -- which is what a second up of a deployment whose loadgen
# or stub embedder is still running would meet -- and because a rename leaves no half-written
# file where a whole one was.
put_file() {
  local vm=$1 file=$2 dest=$3
  # shellcheck disable=SC2016 # expanded by the VM's sh
  on_vm "$vm" sh -c 'mkdir -p -- "$(dirname -- "$1")" && umask 077 && cat >"$1.new" && mv -f -- "$1.new" "$1"' sh "$dest" <"$file" ||
    die "copying $file to $vm as $dest failed"
}

# get_file VM SOURCE DEST: VM's SOURCE into DEST on this pod.
get_file() {
  local vm=$1 source=$2 dest=$3
  on_vm "$vm" cat -- "$source" >"$dest" || die "reading $source from $vm failed"
}

# docker_on VM ARGS...: one docker call on a machine. It goes through sudo because the OS Login
# user is not root and the docker socket on a Container-Optimized OS machine is.
docker_on() {
  local vm=$1
  shift
  on_vm "$vm" sudo docker "$@"
}

# ---- Google's APIs ------------------------------------------------------------------------------

# google_token: an access token for this pod's Google identity, from the metadata server.
google_token() {
  local url token
  url=$(cfg .token_url "$TOKEN_URL_DEFAULT")
  token=$(curl -fsS --max-time 30 -H 'Metadata-Flavor: Google' "$url" | jq -r '.access_token // empty') ||
    die "reading an access token from $url failed"
  [ -n "$token" ] || die "the metadata server at $url answered no access_token"
  printf '%s\n' "$token"
}

# gapi URL: a GET of a Google API with that token, its JSON on standard output.
gapi() {
  curl -fsS --max-time 60 -H "Authorization: Bearer $(google_token)" "$1" ||
    die "GET $1 failed"
}

# gcurl TOKEN URL ARGS...: one call of a Google API with TOKEN, its answer on standard output
# and, when curl fails, curl's own message there too, so a refusal can say what the endpoint
# answered. The token goes in over curl's standard input as a config file and never on a command
# line, because `ps` on this pod lists every argument of every process. What the call is judged
# on is curl's own status and not the pipeline's: a curl that ends before it has read the header
# leaves the write of it unfinished, and that is no answer about the endpoint.
gcurl() {
  local token=$1 url=$2 status
  shift 2
  set +e +o pipefail
  printf 'header = "Authorization: Bearer %s"\n' "$token" |
    curl -fsS --max-time 60 --config - "$@" "$url" 2>&1
  status=${PIPESTATUS[1]}
  set -e -o pipefail
  return "$status"
}

# instance_json: the Cloud SQL instance as the Admin API answers it.
instance_json() {
  gapi "$(cfg .sqladmin "$SQLADMIN_DEFAULT")/v1/projects/$(cfg_valid name .project)/instances/$(cfg_valid name .cloudsql.instance)"
}

# vm_json VM: a VM as the Compute API answers it.
vm_json() {
  gapi "$(cfg .compute "$COMPUTE_DEFAULT")/compute/v1/projects/$(cfg_valid name .project)/zones/$(cfg_valid name .zone)/instances/$(vm_instance "$1")"
}

# vm_status VM: a VM's Compute status (RUNNING, TERMINATED, ...), empty when the read failed.
vm_status() {
  local json
  json=$(vm_json "$1" 2>/dev/null) || return 0
  jq -r '.status // empty' <<<"$json"
}

# vms_running: every machine of the deployment answers RUNNING. VMS_STOPPED names those that do
# not, for the message a discarded step prints.
vms_running() {
  local vm status
  VMS_STOPPED=""
  for vm in "${VM_NAMES[@]}"; do
    status=$(vm_status "$vm")
    [ "$status" = RUNNING ] || VMS_STOPPED+="${VMS_STOPPED:+, }the $vm VM $(vm_instance "$vm") is ${status:-unreadable}"
  done
  [ -z "$VMS_STOPPED" ]
}

# ---- OS Login -----------------------------------------------------------------------------------

# oslogin_ready: the one way this script reaches a machine, made ready and safe to call again.
# The machines carry enable-oslogin and block-project-ssh-keys and this pod's Google identity is
# a federated Kubernetes principal with no POSIX profile, so no key of the pod's authenticates
# anywhere; these four steps are what does:
#
#   1. an access token of the deployment's login service account, which holds
#      roles/compute.osAdminLogin on the machines, minted at iamcredentials with the pod's own
#      metadata token;
#   2. the run's public key imported for that account at oslogin, with an expiry of
#      OSLOGIN_KEY_SECONDS, so a key this run imports does not outlive it by more than a day;
#   3. that account's POSIX user name, read from its login profile;
#   4. every later ssh as that user, with the run's private key and -F /dev/null.
#
# The keypair is made once per run and lives in the run directory alone, mode 600: a second up
# of the same run imports the key it already has rather than making another. The minted token is
# a local of this function, reaches no log, no command line and no file the run collects, and is
# gone when it returns. Each refusal names which of the four steps failed and what it answered,
# because every later command of every subcommand depends on this one.
oslogin_ready() {
  [ -z "$SSH_USER" ] || return 0
  local account project key pub token answer user url body expires
  account=$(cfg_valid service_account .login_service_account)
  project=$(cfg_valid name .project)
  key=$RUN_DIR/oslogin
  pub=$key.pub
  if [ ! -f "$key" ] || [ ! -f "$pub" ]; then
    rm -f -- "$key" "$pub"
    (umask 077 && ssh-keygen -q -t ed25519 -N '' -C "$RUN" -f "$key" </dev/null) ||
      die "OS Login: making the run's own keypair at $key failed; nothing of this deployment can be reached without it"
  fi

  url="$(cfg .iamcredentials "$IAMCREDENTIALS_DEFAULT")/v1/projects/-/serviceAccounts/$account:generateAccessToken"
  body='{"scope":["https://www.googleapis.com/auth/cloud-platform"],"lifetime":"3600s"}'
  answer=$(gcurl "$(google_token)" "$url" -X POST -H 'Content-Type: application/json' -d "$body") ||
    die "OS Login step 1 of 4: minting an access token for $account at $url failed: ${answer:-no answer}"
  token=$(jq -r '.accessToken // empty' <<<"$answer" 2>/dev/null || true)
  [ -n "$token" ] ||
    die "OS Login step 1 of 4: $url answered no accessToken for $account; this pod's own identity must hold roles/iam.serviceAccountTokenCreator on it"

  expires=$(($(date -u +%s) + OSLOGIN_KEY_SECONDS))000000
  url="$(cfg .oslogin "$OSLOGIN_DEFAULT")/v1/users/$account:importSshPublicKey?projectId=$project"
  body=$(jq -n --arg key "$(cat -- "$pub")" --arg expires "$expires" '{key: $key, expirationTimeUsec: $expires}') ||
    die "OS Login step 2 of 4: reading the run's public key $pub failed"
  answer=$(gcurl "$token" "$url" -X POST -H 'Content-Type: application/json' -d "$body") ||
    die "OS Login step 2 of 4: importing $pub for $account at $url failed: ${answer:-no answer}"

  url="$(cfg .oslogin "$OSLOGIN_DEFAULT")/v1/users/$account/loginProfile?projectId=$project"
  answer=$(gcurl "$token" "$url") ||
    die "OS Login step 3 of 4: reading the login profile of $account at $url failed: ${answer:-no answer}"
  user=$(jq -r '.posixAccounts[0].username // empty' <<<"$answer" 2>/dev/null || true)
  [ -n "$user" ] ||
    die "OS Login step 3 of 4: $url gave $account no POSIX account, so there is no user to ssh as"
  valid name "$user" ||
    die "OS Login step 3 of 4: $url gave $account the POSIX user '$user', which is not a name"
  SSH_USER=$user
  SSH_KEY=$key
  note "OS Login: every ssh of run $RUN goes as $SSH_USER with $SSH_KEY, which $account holds for $((OSLOGIN_KEY_SECONDS / 3600)) h"
}

# ---- the containers -----------------------------------------------------------------------------

# container_part ROLE N: field N of ROLE's line of CONTAINERS (1 role, 2 image key, 3 cpus,
# 4 memory). The machine ROLE runs on is no field of it: container_vm reads that from the
# deployment file.
container_part() {
  local line fields
  for line in "${CONTAINERS[@]}"; do
    read -r -a fields <<<"$line"
    [ "${fields[0]}" = "$1" ] || continue
    printf '%s\n' "${fields[$(($2 - 1))]}"
    return 0
  done
  die "no container $1 in the deployment's container table"
}

container_image_key() { container_part "$1" 2; }
container_cpus() { container_part "$1" 3; }
container_memory() { container_part "$1" 4; }

# container_image ROLE: the image ROLE is started from, by digest. A tag is refused: a run that
# cannot name the bytes it measured measures nothing.
container_image() {
  local key image
  key=$(container_image_key "$1")
  image=$(cfg ".images.$key")
  valid image "$image" ||
    die "the deployment file $CONFIG gives images.$key as '$image'; every container is started by digest (registry/repository@sha256:<64 hex>), never by a tag"
  printf '%s\n' "$image"
}

# nanocpus SPEC: what docker records in HostConfig.NanoCpus for --cpus SPEC; 0 for "-".
nanocpus() {
  [ "$1" != - ] || { echo 0; return 0; }
  awk -v c="$1" 'BEGIN { printf "%d\n", c * 1000000000 }'
}

# membytes SPEC: what docker records in HostConfig.Memory for --memory SPEC; 0 for "-". docker's
# g and m are binary, as the specification's GiB and MiB are.
membytes() {
  case $1 in
    -) echo 0 ;;
    *m) echo $((${1%m} * 1024 * 1024)) ;;
    *g) echo $((${1%g} * 1024 * 1024 * 1024)) ;;
    *) die "container memory '$1' is neither a number of m nor of g" ;;
  esac
}

# container_env ROLE: ROLE's environment, one NAME=value a line. Empty for a container that takes
# none.
container_env() {
  local role=$1
  case $role in
    dispatch)
      cat <<EOF
DATABASE_URL=postgres://$(cfg .cloudsql.user):$(cfg .cloudsql.password)@127.0.0.1:$(port_of proxy)/$(cfg .cloudsql.database dispatch)?sslmode=disable
DISPATCH_AGENT_TOKEN=$(cfg .secrets.dispatch_token)
DISPATCH_IDENTITY=header:X-Dispatch-User
DISPATCH_IDENTITY_HEADER_TRUSTED=1
DISPATCH_LISTEN_HOST=0.0.0.0
DISPATCH_PORT=$(port_of dispatch)
DISPATCH_SERVER_URL=$(dispatch_url)
DISPATCH_PPROF_PORT=$(port_of pprof)
NATS_URLS=$(nats_url)
ENVOY_MACHINE_ID=dispatch
ENVOY_ALLOW_REMOTE_NATS=1
ENVOY_URL=$(listener_url)
ENVOY_TOKEN=$(cfg .secrets.listener_token)
DISPATCH_EMBEDDER=azure
DISPATCH_AZURE_EMBED_ENDPOINT=$(stub_embed_url)
DISPATCH_AZURE_EMBED_KEY=$(cfg .secrets.embed_key)
DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE=$(cfg .embed_tokens_per_minute 1000000)
SSL_CERT_FILE=$CERT_IN_CONTAINER
EOF
      ;;
    dispatch-listener | hub-listener | load-listener-*)
      cat <<EOF
PORT=$(listener_port "$role")
ENVOY_LISTEN_HOST=0.0.0.0
ENVOY_MACHINE_ID=$role
NATS_URLS=$(nats_url)
ENVOY_ALLOW_REMOTE_NATS=1
ENVOY_API_TOKEN=$(cfg .secrets.listener_token)
EOF
      ;;
  esac
}

# listener_port ROLE: the port a listener container serves on.
listener_port() {
  case $1 in
    dispatch-listener) port_of listener ;;
    hub-listener) port_of hub_listener ;;
    load-listener-*) load_listener_port "${1##*-}" ;;
    *) die "no listener port for $1" ;;
  esac
}

# container_run_args ROLE: the docker run arguments of ROLE, one a line, image and command last.
# Everything the specification names is here and nothing is left to a default: the CPU and memory
# limits, Dispatch's ephemeral limit and host networking, each published port, each mounted path
# and the image by digest. The environment goes in a file on its machine (container_env_file),
# never on this command line.
container_run_args() {
  local role=$1 cpus memory image
  cpus=$(container_cpus "$role") memory=$(container_memory "$role") image=$(container_image "$role")
  printf '%s\n' --name "$role" --detach --restart no
  [ "$cpus" = - ] || printf '%s\n' --cpus "$cpus"
  [ "$memory" = - ] || printf '%s\n' --memory "$memory"
  case $role in
    dispatch)
      # Host networking, because DISPATCH_PPROF_PORT binds 127.0.0.1 only: on the host's loopback
      # the profile is reachable from its own machine, which is where step fetches it.
      printf '%s\n' --network host
      printf '%s\n' --volume "$(cert_path):$CERT_IN_CONTAINER:ro"
      printf '%s\n' --env-file "$(container_env_file "$role")"
      ;;
    auth-proxy)
      printf '%s\n' --publish "127.0.0.1:$(port_of proxy):$(port_of proxy)"
      ;;
    nats)
      printf '%s\n' --publish "$(port_of nats):$(port_of nats)"
      printf '%s\n' --publish "$(port_of nats_monitor):$(port_of nats_monitor)"
      printf '%s\n' --volume "$(nats_store):/data"
      ;;
    dispatch-listener | hub-listener | load-listener-*)
      printf '%s\n' --publish "$(listener_port "$role"):$(listener_port "$role")"
      printf '%s\n' --env-file "$(container_env_file "$role")"
      ;;
  esac
  # The Envoy image carries four binaries and its entrypoint is envoy-listener, so the container
  # that is Dispatch must name envoy-dispatch itself. Without it that container runs a second
  # listener, which on a machine with host networking binds the port the real listener already
  # holds and exits "address already in use" -- measured on this deployment.
  [ "$role" = dispatch ] && printf '%s\n' --entrypoint envoy-dispatch
  printf '%s\n' "$image"
  case $role in
    nats)
      # JetStream with its store on its machine's disk, which the volume above mounts at /data.
      printf '%s\n' -js -sd /data -p "$(port_of nats)" -m "$(port_of nats_monitor)"
      ;;
    auth-proxy)
      # The Cloud SQL Auth Proxy reaches the instance on its private address, as R2 requires.
      printf '%s\n' --address 0.0.0.0 --port "$(port_of proxy)" --private-ip "$(cfg .cloudsql.connection_name)"
      ;;
  esac
}

# container_env_file ROLE: where ROLE's environment file lives on its machine.
container_env_file() { printf '%s/env/%s.env\n' "$(remote_root)" "$1"; }

# container_write_env ROLE: ROLE's environment into that file, mode 600 on its machine, so no
# credential of the deployment reaches a command line or a process listing.
container_write_env() {
  local role=$1 tmp
  tmp=$(mktemp "$RUN_DIR/.env.XXXXXX")
  container_env "$role" >"$tmp"
  if [ -s "$tmp" ]; then
    put_file "$(container_vm "$role")" "$tmp" "$(container_env_file "$role")"
  fi
  rm -f -- "$tmp"
}

# container_start ROLE: ROLE started on its machine, replacing whatever ran under that name.
container_start() {
  local role=$1 vm args=()
  vm=$(container_vm "$role")
  container_write_env "$role"
  docker_on "$vm" rm -f "$role" >/dev/null 2>&1 || true
  mapfile -t args < <(container_run_args "$role")
  docker_on "$vm" run "${args[@]}" >/dev/null || die "starting the container $role on the $vm VM failed"
  note "$role started on the $vm VM: $(container_image "$role")"
}

# container_stop ROLE: ROLE stopped and removed on its machine. A container that is not there is
# no failure: down and reset both run over containers that may already be gone.
container_stop() {
  local role=$1
  docker_on "$(container_vm "$role")" rm -f "$role" >/dev/null 2>&1 || true
}

# ---- the stub embedder --------------------------------------------------------------------------

# stub_embed_start: loadgen stub-embed on the load machine, detached, with its certificate
# written where Dispatch reads it as SSL_CERT_FILE on its own machine. The key is given on the
# load machine's command line alone; Dispatch takes it from its environment file.
stub_embed_start() {
  local cert=$RUN_DIR/stub-embed.pem remote work
  work=$(remote_work)
  remote=$(remote_loadgen)
  on_vm "$LOAD_VM" mkdir -p -- "$work" || die "making $work on the $LOAD_VM VM failed"
  # A stub embedder an earlier up left running still holds the port, so this one would answer
  # "address already in use" and write no certificate. up is idempotent everywhere else; it is
  # here too: whatever the last start recorded is ended first, and the wait below then sees this
  # start's own certificate rather than the one before it.
  stub_embed_stop
  # shellcheck disable=SC2016 # expanded by the load machine's sh
  on_vm "$LOAD_VM" sh -c 'rm -f "$3"; nohup setsid "$1" stub-embed -key "$2" -listen "$4" -cert-out "$3" -host "$5" >"$6/stub-embed.log" 2>&1 </dev/null & echo "$!" >"$6/stub-embed.pid"' \
    sh "$remote" "$(cfg .secrets.embed_key)" "$work/stub-embed.pem" "0.0.0.0:$(port_of stub_embed)" "$(vm_host "$LOAD_VM")" "$work" ||
    die "starting loadgen stub-embed on the $LOAD_VM VM failed"
  local waited=0
  # shellcheck disable=SC2016 # expanded by the load machine's sh
  until on_vm "$LOAD_VM" sh -c 'test -s "$1"' sh "$work/stub-embed.pem" 2>/dev/null; do
    ((waited < STUB_EMBED_SECONDS)) || die "loadgen stub-embed wrote no certificate at $work/stub-embed.pem on the $LOAD_VM VM within $STUB_EMBED_SECONDS s; its output is $work/stub-embed.log there"
    sleep 2
    waited=$((waited + 2))
  done
  get_file "$LOAD_VM" "$work/stub-embed.pem" "$cert"
  put_file "$DISPATCH_VM" "$cert" "$(cert_path)"
  note "the stub embedder serves $(stub_embed_url) on the $LOAD_VM VM; its certificate is $(cert_path) on the $DISPATCH_VM VM"
}

# stub_embed_stop: the stub embedder ended by the pid it recorded.
stub_embed_stop() {
  # shellcheck disable=SC2016 # expanded by the load machine's sh
  on_vm "$LOAD_VM" sh -c 'pid=$(cat "$1/stub-embed.pid" 2>/dev/null); kill -TERM "$pid" 2>/dev/null; exit 0' \
    sh "$(remote_work)" >/dev/null 2>&1 || true
}

# ---- Postgres -----------------------------------------------------------------------------------

# psql_at DATABASE SQL: one statement on the instance, its result bare on standard output.
psql_at() {
  psql "$(pg_url "$1")" -v ON_ERROR_STOP=1 -Atq -c "$2"
}

# db_exists NAME: the instance holds that database.
db_exists() {
  local got
  got=$(psql_at postgres "select 1 from pg_database where datname = '$1'") || return 1
  [ "$got" = 1 ]
}

# ---- Dispatch -----------------------------------------------------------------------------------

# wait_healthz: poll Dispatch's /healthz until it answers ok, within HEALTHZ_SECONDS. A container
# that is no longer running never will answer, so each poll reads its state first and refuses at
# once with the end of its log, rather than spending the whole bound on a question already
# settled. Its log is where every reason Dispatch refuses to start is written.
#
# What ok means is `.ok and .nats`, which is what stack.sh's dispatch_healthy reads: the body
# carries no `status` field, and a Dispatch whose database is up but whose NATS is not would
# serve the API while every notification of a step went nowhere.
wait_healthz() {
  local waited=0 body state
  while :; do
    if body=$(curl -fsS --max-time 10 "$(dispatch_url)/healthz" 2>/dev/null) &&
      jq -e '.ok and .nats' >/dev/null 2>&1 <<<"$body"; then
      note "Dispatch answers /healthz ok at $(dispatch_url)"
      return 0
    fi
    state=$(docker_on "$DISPATCH_VM" inspect -f '{{.State.Status}}' dispatch 2>/dev/null) || state=""
    if [ -n "$state" ] && [ "$state" != running ]; then
      note "the last of Dispatch's log on the $DISPATCH_VM VM:"
      docker_on "$DISPATCH_VM" logs --tail 20 dispatch 2>&1 | sed 's/^/    /' || true
      die "the Dispatch container on the $DISPATCH_VM VM is $state, so it will not answer /healthz at $(dispatch_url); its log is above"
    fi
    ((waited < HEALTHZ_SECONDS)) ||
      die "Dispatch did not answer /healthz ok at $(dispatch_url) within $HEALTHZ_SECONDS s (its last answer: ${body:-none})"
    sleep "$HEALTHZ_POLL_SECONDS"
    waited=$((waited + HEALTHZ_POLL_SECONDS))
  done
}

# ---- the run ------------------------------------------------------------------------------------

set_run() {
  RUN=$1
  RUN_DIR=$VM_ROOT/$RUN
  STATE=$RUN_DIR/state.json
  CGROUP_JSONL=$RUN_DIR/cgroup.jsonl
}

# new_run: a run id of this moment, its directory and its state.
new_run() {
  local run
  run=vm-$(date -u +%Y%m%dt%H%M%S)-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
  valid run "$run" || die "could not make a run id"
  set_run "$run"
  mkdir -p -- "$RUN_DIR/steps" "$RUN_DIR/logs"
  (umask 077 && jq -n --arg run "$run" --arg at "$(utc_now)" --arg config "$CONFIG" \
    --arg pod "${HOSTNAME:-unknown}" --argjson deployment "$(jq '{project, zone, vms, load_vm, containers, images, ports}' "$CONFIG")" \
    '{run: $run, started: $at, config_file: $config, pod: $pod, deployment: $deployment}' >"$STATE")
  printf '%s\n' "$run" >"$VM_ROOT/current"
  note "run $run: $RUN_DIR"
}

# use_run: RUN from --run, else from <root>/current. The run must have a directory here: a
# command of a run this pod did not start has nothing to write into.
use_run() {
  local run=${OPT[run]:-}
  if [ -z "$run" ]; then
    run=$(cat -- "$VM_ROOT/current" 2>/dev/null || true)
    [ -n "$run" ] || die "$CMD: no run has been started from this pod (no $VM_ROOT/current): vm.sh up first, or name one with --run"
  fi
  valid run "$run" || die "$CMD: run id '$run' is not one vm.sh makes"
  set_run "$run"
  [ -f "$STATE" ] || die "$CMD: no $STATE: run $run was not started from this pod (LOADTEST_VM_ROOT $VM_ROOT)"
}

# loadgen_local ARGS: this pod's loadgen, with its root in the run directory.
loadgen_local() {
  local bin
  bin=$(loadgen_bin)
  LOADTEST_ROOT=$RUN_DIR "$bin" "$@"
}

# loadgen_bin: the path of this pod's loadgen, for the copy onto the load machine.
loadgen_bin() {
  printf '%s\n' "${OPT[loadgen]:-$(cfg .paths.loadgen "${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}/bin/loadgen")}"
}

# endpoints [PPROF]: every endpoint flag of loadgen seed and run (guard.go), into ENDPOINTS. Each
# one is the deployment's own, which is what R2 asks and what check reads back. PPROF is the
# profiling endpoint loadgen is given: the deployment file's own by default, and a step's tunnel
# on the load machine's loopback while a step runs, since that is where loadgen run runs and the
# profiling port answers on the loopback of Dispatch's own machine alone.
endpoints() {
  ENDPOINTS=(
    -allow-foreign
    -url "$(dispatch_url)" -token "$(cfg .secrets.dispatch_token)"
    -listener "$(listener_url)" -listener-token "$(cfg .secrets.listener_token)"
    -nats "$(nats_url)" -nats-mon "$(nats_mon_url)"
    -pprof "${1:-$(pprof_url)}" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")"
  )
}

# ---- the remote root programs are executed from ---------------------------------------------------

# stateful_dir: the directory under the stateful partition that remote_root is a bind mount of.
# Its name is remote_root's own last element unless paths.stateful_dir gives another, so a
# deployment that moves remote_root does not silently go on sharing one backing directory with
# the deployment it moved away from.
stateful_dir() {
  local root
  root=$(remote_root)
  printf '%s/%s\n' "$STATEFUL_PARTITION" "$(cfg_valid name .paths.stateful_dir "${root##*/}")"
}

# mounted_options VM DIR: the options DIR is mounted with on the machine VM, empty when nothing
# is mounted there. It is the kernel's own record, read back out of mount's table rather than
# taken from what a mount was asked for, and in that table an option is named only where it
# differs from the default: exec is the absence of noexec.
mounted_options() {
  local vm=$1 dir=$2 opts
  # shellcheck disable=SC2016 # expanded by the VM's sh
  opts=$(on_vm "$vm" sh -c 'mount | while read -r src keyword at kind fs opts; do if [ "$keyword" = on ] && [ "$at" = "$1" ]; then printf "%s\n" "$opts"; fi; done; exit 0' sh "$dir") ||
    die "up: reading the mount table of the $vm VM failed"
  opts=${opts##*$'\n'}
  opts=${opts#(}
  printf '%s\n' "${opts%)}"
}

# remote_root_mount VM: remote_root on the machine VM made a directory programs can be executed
# from, whatever state an earlier run left it in. These are Container-Optimized OS machines,
# where /var is mounted noexec, so loadgen copied under remote_root cannot be executed where a
# plain mkdir puts it (dispatch://INFRA-49): the root is a bind mount of a directory under the
# stateful partition, remounted exec. That directory and the root are made with sudo, because
# neither partition is the ssh user's to write in, and the root is given to that user afterwards,
# so that nothing but docker and these mounts needs sudo again. A root that already carries a
# mount is remounted and not mounted again, so a second up stacks no second mount on it. The
# mount is then read back and the whole of up refused when it is not exec: a load machine that
# cannot exec loadgen runs no step, and up is a cheaper place to find that out than the first
# step.
remote_root_mount() {
  local vm=$1 root backing opts
  root=$(remote_root)
  backing=$(stateful_dir)
  on_vm "$vm" sudo mkdir -p -- "$backing" ||
    die "up: making $backing on the $vm VM failed: it is the directory $root is a bind mount of, and $root is noexec without it"
  on_vm "$vm" sudo mkdir -p -- "$root" || die "up: making $root on the $vm VM failed"
  if [ -z "$(mounted_options "$vm" "$root")" ]; then
    on_vm "$vm" sudo mount --bind -- "$backing" "$root" ||
      die "up: bind-mounting $backing at $root on the $vm VM failed"
  else
    note "$root already carries a mount on the $vm VM: up remounts it rather than stacking a second one on it"
  fi
  on_vm "$vm" sudo mount -o "remount,bind,$REMOTE_ROOT_MOUNT_OPTIONS" -- "$backing" "$root" ||
    die "up: remounting $root $REMOTE_ROOT_MOUNT_OPTIONS on the $vm VM failed"
  # shellcheck disable=SC2016 # expanded by the VM's sh
  on_vm "$vm" sh -c 'sudo chown -- "$(id -un)" "$1"' sh "$root" ||
    die "up: giving $root on the $vm VM to the ssh user failed: every later command of this script writes there as that user"
  opts=$(mounted_options "$vm" "$root")
  [ -n "$opts" ] ||
    die "up: nothing is mounted at $root on the $vm VM after the bind mount of $backing: loadgen cannot be executed under $root there, so no step of this deployment would run"
  case ",$opts," in
    *,noexec,*)
      die "up: $root on the $vm VM is mounted $opts, which is noexec: loadgen cannot be executed under $root there, so no step of this deployment would run"
      ;;
  esac
  note "$root on the $vm VM is $backing, mounted $opts"
}

# ---- up -------------------------------------------------------------------------------------------

cmd_up() {
  parse "config run loadgen" "" "$@"
  positional 0 "no positional arguments"
  if [ -n "${OPT[run]:-}" ]; then use_run; else new_run; fi
  local role vm bin
  bin=$(loadgen_bin)
  [ -x "$bin" ] || die "up: no loadgen at $bin: build it (packages/envoy/loadtest/stack.sh setup) or name it with --loadgen"
  # The way in, before anything is started: no ssh of this script authenticates until the run's
  # key is imported and the OS Login user read (oslogin_ready).
  oslogin_ready
  # Then every machine's remote root, and executable: /var is noexec on these machines, so the
  # copy below runs from nowhere under $(remote_root) until that mount is there.
  for vm in "${VM_NAMES[@]}"; do
    remote_root_mount "$vm"
  done
  on_vm "$(container_vm nats)" mkdir -p -- "$(nats_store)" ||
    die "up: making $(nats_store) on the $(container_vm nats) VM failed"
  on_vm "$LOAD_VM" mkdir -p -- "$(remote_work)" || die "up: making $(remote_work) on the $LOAD_VM VM failed"
  put_file "$LOAD_VM" "$bin" "$(remote_loadgen)"
  on_vm "$LOAD_VM" chmod 755 "$(remote_loadgen)" || die "chmod of $(remote_loadgen) on the $LOAD_VM VM failed"
  # The database Dispatch opens, if the instance does not hold it yet. A new instance ships with
  # the login's own database and no other, and Dispatch exits "database does not exist" rather
  # than making one; its migrations run themselves once it can open it.
  local database
  database=$(cfg .cloudsql.database dispatch)
  if db_exists "$database"; then
    note "the instance already holds the database $database"
  else
    psql_at postgres "create database \"$database\"" >/dev/null ||
      die "up: making the database $database on the instance failed"
    note "made the database $database on the instance"
  fi
  # The stub embedder before Dispatch: Dispatch reads its certificate at startup (SSL_CERT_FILE),
  # so a Dispatch started before it has nothing to trust and every embed fails.
  stub_embed_start
  for role in "${CONTAINERS[@]%% *}"; do
    container_start "$role"
  done
  wait_healthz
  note "up done: ${CONTAINERS[*]%% *} started; run $RUN"
}

# ---- check ----------------------------------------------------------------------------------------

CHECK_ROWS=""
CHECK_BAD=0

# check_row NAME EXPECTED FOUND: one line of the table, and a difference counted.
check_row() {
  local ok=yes
  [ "$2" = "$3" ] || {
    ok=no
    CHECK_BAD=$((CHECK_BAD + 1))
  }
  CHECK_ROWS+="| $1 | ${2:-(none)} | ${3:-(none)} | $ok |"$'\n'
}

# check_containers: each container's CPU, memory and image, from the one docker inspect the
# issue's acceptance criterion A1 names, and Dispatch's network mode. There is no ephemeral
# limit to read back: docker on these machines refuses one (the container table, above).
check_containers() {
  local role vm got want
  for role in "${CONTAINERS[@]%% *}"; do
    vm=$(container_vm "$role")
    want="/$role $(nanocpus "$(container_cpus "$role")") $(membytes "$(container_memory "$role")") $(container_image "$role")"
    got=$(docker_on "$vm" inspect -f '{{.Name}} {{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.Config.Image}}' "$role" 2>/dev/null) || got=""
    check_row "$role name cpu memory image ($vm VM)" "$want" "$got"
  done
  got=$(docker_on "$DISPATCH_VM" inspect -f '{{.HostConfig.NetworkMode}}' dispatch 2>/dev/null) || got=""
  check_row "dispatch network mode" "host" "$got"
}

# check_env: every endpoint setting of every container, expected beside found. A setting whose
# value is a credential is compared by presence alone, so no token reaches the table.
check_env() {
  local role vm found name want got line
  for role in "${CONTAINERS[@]%% *}"; do
    mapfile -t want < <(container_env "$role")
    ((${#want[@]})) || continue
    vm=$(container_vm "$role")
    found=$(docker_on "$vm" inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$role" 2>/dev/null) || found=""
    for line in "${want[@]}"; do
      name=${line%%=*}
      got=$(grep -m1 -- "^$name=" <<<"$found" || true)
      if [[ $SECRET_VARS == *" $name "* ]]; then
        check_row "$role env $name" "set" "$([ -n "$got" ] && [ "$got" != "$name=" ] && echo set || echo unset)"
      else
        check_row "$role env $name" "$line" "$got"
      fi
    done
  done
}

# check_instance: the Cloud SQL instance's own settings, each path and value exactly as the
# deployment file's cloudsql.expect writes them.
check_instance() {
  local json path want got
  json=$(instance_json) || json='{}'
  while IFS=$'\t' read -r path want; do
    [ -n "$path" ] || continue
    got=$(jq -r ".$path // empty" <<<"$json" 2>/dev/null || true)
    check_row "cloudsql $path" "$want" "$got"
  done < <(jq -r '(.cloudsql.expect // {}) | to_entries[] | "\(.key)\t\(.value)"' "$CONFIG")
}

# check_run_duration: every machine's run-time limit, which R11 sets to 72 hours.
check_run_duration() {
  local vm json
  for vm in "${VM_NAMES[@]}"; do
    json=$(vm_json "$vm") || json='{}'
    check_row "$vm VM scheduling.maxRunDuration seconds" "$MAX_RUN_DURATION" "$(jq -r '.scheduling.maxRunDuration.seconds // empty' <<<"$json")"
  done
}

# check_production_refused: every production address R2 forbids is refused from every machine.
# nc's non-zero exit is the expectation: an address that answers is a path into production.
#
# The addresses are read into an array first, and never streamed into the loop: ssh reads its own
# standard input, so an ssh inside a `while read` fed by a pipe swallows the rest of the list and
# every address after the first goes unchecked -- measured here, where the second address was
# silently never tried.
check_production_refused() {
  local vm address host port rc
  local -a addresses=()
  mapfile -t addresses < <(jq -r '(.production_addresses // [])[]' "$CONFIG")
  for address in "${addresses[@]}"; do
    [ -n "$address" ] || continue
    valid address "$address" || die "check: production_addresses holds '$address', which is not <host>:<port>"
    host=${address%:*} port=${address##*:}
    for vm in "${VM_NAMES[@]}"; do
      rc=0
      on_vm "$vm" nc -z -w5 "$host" "$port" >/dev/null 2>&1 || rc=$?
      check_row "$vm VM to production $address" "refused" "$([ "$rc" -ne 0 ] && echo refused || echo answered)"
    done
  done
}

cmd_check() {
  parse "config run" "" "$@"
  positional 0 "no positional arguments"
  use_run
  oslogin_ready
  check_containers
  check_env
  check_instance
  check_run_duration
  check_production_refused
  printf '| check | expected | found | ok |\n| --- | --- | --- | --- |\n%s' "$CHECK_ROWS"
  # The table is the deployment's, so neither the tunnel nor the remote root's mount is in it.
  # The tunnel is the harness's own, opened on the load machine by each step and closed with it,
  # and between steps there is nothing of it on any machine for check to read. The mount is the
  # harness's own too: up makes it on every machine because /var is noexec there, and what check
  # reads back is the deployment, not the harness's way around the machines it was given.
  note "check: the step's profiling tunnel is no row of this table. It is the harness's, not the deployment's: step opens it on the $LOAD_VM VM (port $(pprof_tunnel_port) to 127.0.0.1:$(port_of pprof) on the $DISPATCH_VM VM), proves it, and closes it when the step ends."
  if ((CHECK_BAD > 0)); then
    note "check: $CHECK_BAD of the readings differ from the specification; the table above names each"
    return 1
  fi
  note "check: every reading equals the specification"
}

# ---- seed -------------------------------------------------------------------------------------------

# seed_phase NAME CMD...: CMD, its wall clock appended to the run's seed record.
seed_phase() {
  local name=$1 t0=$EPOCHREALTIME started rc=0
  shift
  started=$(utc_now)
  note "seed: $name"
  "$@" || rc=$?
  jq -n --arg name "$name" --arg started "$started" --arg ended "$(utc_now)" \
    --argjson seconds "$(elapsed_since "$t0")" --argjson rc "$rc" \
    '{phase: $name, started: $started, ended: $ended, seconds: $seconds, exit: $rc}' >>"$RUN_DIR/seed.phases.jsonl"
  ((rc == 0)) || die "seed: $name ended with exit $rc"
  note "seed: $name done in $(elapsed_since "$t0") s"
}

cmd_seed() {
  parse "config run loadgen embed-timeout workers" "" "$@"
  positional 0 "no positional arguments"
  use_run
  oslogin_ready
  local plan=$RUN_DIR/loopseed.plan.json seed=$RUN_DIR/loopseed.seed.json
  local template work remote embed_timeout workers t0=$EPOCHREALTIME
  template=$(cfg .cloudsql.template dispatch_seed)
  work=$(remote_work) remote=$(remote_loadgen)
  embed_timeout=${OPT[embed-timeout]:-8h}
  valid duration "$embed_timeout" || die "seed: --embed-timeout '$embed_timeout' is not a whole number of s, m or h"
  workers=${OPT[workers]:-8}
  [[ $workers =~ ^[1-9][0-9]*$ ]] || die "seed: --workers '$workers' is not a count"
  [ ! -f "$RUN_DIR/seed.json" ] || die "seed: run $RUN is seeded already ($RUN_DIR/seed.json)"
  ! db_exists "$template" || die "seed: the instance already holds the template database $template"
  : >"$RUN_DIR/seed.phases.jsonl"
  endpoints

  seed_phase plan loadgen_local loop-seed-plan -out "$plan"
  seed_phase push-plan put_file "$LOAD_VM" "$plan" "$work/loopseed.plan.json"
  seed_phase write on_vm_for 86400 "$LOAD_VM" "$remote" loop-seed-write \
    -plan "$work/loopseed.plan.json" -out "$work/loopseed.meta.json" -workers "$workers" "${ENDPOINTS[@]}"
  seed_phase pull-meta get_file "$LOAD_VM" "$work/loopseed.meta.json" "$RUN_DIR/loopseed.meta.json"
  seed_phase check loadgen_local loop-seed-check -plan "$plan" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")"
  seed_phase export loadgen_local loop-seed-export -plan "$plan" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")" -out "$seed"
  seed_phase push-seed put_file "$LOAD_VM" "$seed" "$work/loopseed.seed.json"
  # The embedding gate: seed-finish with -url also waits for meaning search to embed the seed,
  # and -embed-progress records the coverage's progress while it waits (R4's "the seed is
  # complete, its embeddings included").
  seed_phase finish loadgen_local seed-finish \
    -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")" \
    -url "$(dispatch_url)" -token "$(cfg .secrets.dispatch_token)" \
    -embed-timeout "$embed_timeout" -embed-progress "$RUN_DIR/seedfinish.embed.json"
  # The template the reset copies from: Dispatch and the listeners hold connections to the
  # database, so they stop before the rename, as cluster.sh's seed_run does.
  seed_phase template seed_template "$template"

  jq -n --arg at "$(utc_now)" --argjson seconds "$(elapsed_since "$t0")" --arg template "$template" \
    --argjson phases "$(jq -s . "$RUN_DIR/seed.phases.jsonl")" \
    --argjson coverage "$(jq -c . "$RUN_DIR/seedfinish.embed.json" 2>/dev/null || echo null)" \
    '{finished: $at, seconds: $seconds, template: $template, phases: $phases, embed_progress: $coverage}' >"$RUN_DIR/seed.json"
  note "seed done in $(elapsed_since "$t0") s: template database $template; $RUN_DIR/seed.json"
}

# seed_template TEMPLATE: the seeded database renamed to TEMPLATE, with everything that holds a
# connection to it stopped first.
seed_template() {
  local role
  for role in "${DB_HOLDERS[@]}"; do
    container_stop "$role"
  done
  psql_at postgres "alter database $(cfg .cloudsql.database dispatch) rename to $1" >/dev/null ||
    die "renaming the seeded database to $1 failed"
}

# ---- reset ------------------------------------------------------------------------------------------

# reset_deployment RECORD: decision D6's reset, in its order, into RECORD. The NATS and listener
# restart is part of it because the listener keeps interests, sessions and role claims in NATS KV
# and every unit session leaves a durable consumer with a 72-hour inactivity threshold: without
# it, step N+1 starts with step N's consumers.
reset_deployment() {
  local record=$1 template database started t0=$EPOCHREALTIME role steps=()
  template=$(cfg .cloudsql.template dispatch_seed)
  database=$(cfg .cloudsql.database dispatch)
  started=$(utc_now)
  db_exists "$template" || die "the instance has no template database $template; vm.sh seed first"

  # 1. the Dispatch and listener containers stopped, so nothing holds a connection or a consumer.
  for role in "${DB_HOLDERS[@]}"; do
    container_stop "$role"
    steps+=("stop $role")
  done
  # 2. and 3. the database dropped and made again from the template.
  psql_at postgres "drop database if exists $database with (force)" >/dev/null ||
    die "drop database $database failed"
  steps+=("drop database $database")
  psql_at postgres "create database $database template $template" >/dev/null ||
    die "create database $database from $template failed"
  steps+=("create database $database template $template")
  # 4. NATS again on a fresh JetStream store: the store is its machine's disk, so the restart
  # alone would keep every stream, consumer and KV bucket of the step before.
  container_stop nats
  # shellcheck disable=SC2016 # expanded by that machine's sh
  on_vm "$(container_vm nats)" sh -c 'rm -rf -- "$1" && mkdir -p -- "$1"' sh "$(nats_store)" ||
    die "emptying the JetStream store $(nats_store) on the $(container_vm nats) VM failed"
  steps+=("fresh jetstream store $(nats_store)")
  container_start nats
  steps+=("start nats")
  container_start hub-listener
  steps+=("start hub-listener")
  # 5. the four load listeners and the Dispatch listener.
  for role in "${LOAD_LISTENERS[@]}" dispatch-listener; do
    container_start "$role"
    steps+=("start $role")
  done
  # 6. and 7. Dispatch, and the wait on /healthz.
  container_start dispatch
  steps+=("start dispatch")
  wait_healthz
  steps+=("healthz ok")

  jq -n --arg started "$started" --arg ended "$(utc_now)" --argjson seconds "$(elapsed_since "$t0")" \
    --arg template "$template" --arg database "$database" \
    --argjson steps "$(printf '%s\n' "${steps[@]}" | jq -R . | jq -s .)" \
    '{started: $started, ended: $ended, seconds: $seconds, template: $template, database: $database, steps: $steps}' >"$record"
  note "reset done in $(jq -r .seconds "$record") s: database $database from $template, a fresh JetStream store, the listeners and Dispatch ready"
}

cmd_reset() {
  parse "config run" "" "$@"
  positional 0 "no positional arguments"
  use_run
  oslogin_ready
  mkdir -p -- "$RUN_DIR/logs"
  reset_deployment "$RUN_DIR/logs/reset-$(date -u +%Y%m%dt%H%M%S).json"
}

# ---- the sampler --------------------------------------------------------------------------------

# sampler_targets: the -vm arguments of loadgen vm-sample, into SAMPLER_TARGETS: every machine's
# ssh target with the containers the deployment file puts on it, each at the cgroup v2 directory
# its container id gives, so every machine is a series like any other and cgroup-merge reads
# them all the same way. A machine whose containers all answered nothing is left out rather than
# sampled empty.
sampler_targets() {
  local vm role id pattern spec roles
  pattern=$(cfg .paths.cgroup '/sys/fs/cgroup/system.slice/docker-%s.scope')
  SAMPLER_TARGETS=()
  for vm in "${VM_NAMES[@]}"; do
    roles=""
    for role in "${CONTAINERS[@]%% *}"; do
      [ "$(container_vm "$role")" = "$vm" ] || continue
      id=$(docker_on "$vm" inspect -f '{{.Id}}' "$role" 2>/dev/null) || id=""
      [ -n "$id" ] || {
        note "the sampler leaves $role out: the $vm VM gave no container id for it"
        continue
      }
      # shellcheck disable=SC2059 # the pattern is the deployment's, with one %s for the id
      roles+="${roles:+,}$role=$(printf "$pattern" "$id")"
    done
    [ -n "$roles" ] || continue
    spec="$(vm_ssh "$vm"):$roles"
    SAMPLER_TARGETS+=(-vm "$spec")
  done
  ((${#SAMPLER_TARGETS[@]})) || die "the sampler has no machine to read: no container answered docker inspect"
}

# sampler_start: loadgen vm-sample over every machine of the deployment, appending to the run's
# cgroup.jsonl, its pid in SAMPLER_PID.
sampler_start() {
  sampler_targets
  ssh_cmd
  loadgen_local vm-sample -out "$CGROUP_JSONL" -ssh "${SSH[*]}" "${SAMPLER_TARGETS[@]}" \
    >>"$RUN_DIR/logs/vm-sample.log" 2>&1 &
  SAMPLER_PID=$!
  note "the sampler reads $((${#SAMPLER_TARGETS[@]} / 2)) machines of this deployment into $CGROUP_JSONL (pid $SAMPLER_PID)"
}

# sampler_stop: the sampler ended, which it does on SIGTERM.
sampler_stop() {
  [ -n "${SAMPLER_PID:-}" ] || return 0
  kill -TERM "$SAMPLER_PID" 2>/dev/null || true
  wait "$SAMPLER_PID" 2>/dev/null || true
  SAMPLER_PID=""
}

# ---- the profiling tunnel -----------------------------------------------------------------------

# The step's tunnel from the load machine to Dispatch's profiling port on the loopback of the
# machine Dispatch runs on. It exists for the sampler inside `loadgen run`, which reads
# goroutines, the pool's acquire waiters and the heap while the step runs (sample.go) from the
# load machine, where nothing else can reach a port that binds 127.0.0.1 on another machine. It
# is no part of the deployment: check reads no row of it, up starts none and down stops none; it
# lives exactly as long as one step's load. The command that opens it is the deployment file's,
# because it runs on the load machine and not on this pod: the run's own OS Login key stays here.

# pprof_tunnel_port: the port the tunnel listens on, on the load machine.
pprof_tunnel_port() { cfg_valid port .ports.pprof_tunnel "$PPROF_TUNNEL_PORT_DEFAULT"; }

# pprof_tunnel_url: what a step gives loadgen as -pprof. It is a loopback address of the load
# machine, where loadgen run runs, and never of this pod.
pprof_tunnel_url() { printf 'http://127.0.0.1:%s\n' "$(pprof_tunnel_port)"; }

# pprof_tunnel_pid_file, pprof_tunnel_log: where the tunnel's pid and its output are kept on the
# load machine.
pprof_tunnel_pid_file() { printf '%s/pprof-tunnel.pid\n' "$(remote_work)"; }
pprof_tunnel_log() { printf '%s/pprof-tunnel.log\n' "$(remote_work)"; }

# tunnel_kill: whatever the pid file on the load machine names, ended there, and the file
# removed. TERM first, KILL after 10 s, so no step leaves an ssh of its own behind holding the
# port the next step needs. A failure to reach the machine is not raised: a step that cannot ssh
# has already failed on something louder than this.
tunnel_kill() {
  # shellcheck disable=SC2016 # expanded by the load machine's sh
  on_vm "$LOAD_VM" sh -c '
    pid=$(cat "$1" 2>/dev/null)
    if [ -n "$pid" ]; then
      kill -TERM "$pid" 2>/dev/null
      i=0
      while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 10 ]; do
        sleep 1
        i=$((i + 1))
      done
      kill -KILL "$pid" 2>/dev/null
    fi
    rm -f "$1"
    exit 0' sh "$(pprof_tunnel_pid_file)" >/dev/null 2>&1 || true
  return 0
}

# tunnel_start NAME: the tunnel opened on the load machine and proven to answer. It returns 1
# with TUNNEL_REFUSAL saying what went wrong, which run_step turns into the step's refusal: a
# step whose sampler cannot read the profiling port measures no pool acquire waiters, and
# criterion (e) is then FAIL for want of a measurement (curves.go passCriteria), so the step is
# refused before it sends any load rather than run for nothing.
tunnel_start() {
  local name=$1 port remote_port probe raw cmd=() waited=0
  TUNNEL_REFUSAL=""
  port=$(pprof_tunnel_port)
  remote_port=$(port_of pprof)
  raw=$(cfg .pprof_tunnel_command "$PPROF_TUNNEL_COMMAND_DEFAULT")
  read -r -a cmd <<<"$raw"
  ((${#cmd[@]})) || die "the deployment file $CONFIG gives an empty pprof_tunnel_command"
  if ! on_vm "$LOAD_VM" mkdir -p -- "$(remote_work)"; then
    TUNNEL_REFUSAL="making $(remote_work) on the $LOAD_VM VM failed."
    return 1
  fi
  tunnel_kill
  TUNNEL_OPEN=1
  # shellcheck disable=SC2016 # expanded by the load machine's sh
  if ! on_vm "$LOAD_VM" sh -c 'pid_file=$1 log=$2; shift 2; rm -f "$pid_file"; nohup "$@" >"$log" 2>&1 </dev/null & echo "$!" >"$pid_file"' \
    sh "$(pprof_tunnel_pid_file)" "$(pprof_tunnel_log)" \
    "${cmd[@]}" -L "$port:127.0.0.1:$remote_port" "$(vm_ssh "$DISPATCH_VM")"; then
    TUNNEL_REFUSAL="starting the profiling tunnel on the $LOAD_VM VM failed (its output is $(pprof_tunnel_log) there)."
    return 1
  fi
  probe="$(pprof_tunnel_url)/debug/pprof/goroutine?debug=1"
  until on_vm "$LOAD_VM" curl -fsS --max-time 10 "$probe" >/dev/null 2>&1; do
    if ((waited >= TUNNEL_SECONDS)); then
      TUNNEL_REFUSAL="the $LOAD_VM VM could not read $probe through the profiling tunnel to port $remote_port on the $DISPATCH_VM VM within $TUNNEL_SECONDS s (the tunnel's output is $(pprof_tunnel_log) there)."
      return 1
    fi
    sleep "$TUNNEL_POLL_SECONDS"
    waited=$((waited + TUNNEL_POLL_SECONDS))
  done
  note "step $name: the profiling tunnel answers $probe on the $LOAD_VM VM, which reaches Dispatch's port $remote_port on the $DISPATCH_VM VM's loopback"
}

# tunnel_stop: the step's tunnel closed, whatever ended the step. run_step sets it as an EXIT
# trap before it opens one, so a refusal, an interrupt and a failure anywhere below all close it;
# it does nothing when no tunnel is open, and it never changes the exit status it runs under.
tunnel_stop() {
  [ -n "${TUNNEL_OPEN:-}" ] || return 0
  TUNNEL_OPEN=""
  tunnel_kill
  return 0
}

# step_cleanup: the sampler and the tunnel both ended, whatever ended the step. run_step sets it
# as its EXIT trap before it opens either, so a refusal, an interrupt, a failure of the ssh that
# carries the load and a failure of anything after it all leave neither behind: a sampler left
# running holds one ssh per machine open and goes on appending to a finished step's cgroup.jsonl.
step_cleanup() {
  sampler_stop
  tunnel_stop
  return 0
}

# ---- a step -----------------------------------------------------------------------------------------

# load_value FILTER DEFAULT: one number or word of the deployment file's load object.
load_value() { cfg ".load.$1" "$2"; }

# step_rates FACTOR: the step's offered rates, into EVENTS, REQUESTS, SESSIONS, UNIT_STARTS. The
# baseline is carried as it is at every factor and the loop's own rates are multiplied by the
# factor (R5), so factor 0 is the baseline alone.
step_rates() {
  local f=$1 be br bs lw lr lu
  be=$(load_value baseline.events 4) br=$(load_value baseline.requests 60) bs=$(load_value baseline.sessions 400)
  lw=$(load_value loop.writes 3.5) lr=$(load_value loop.reads 38.5) lu=$(load_value loop.unit_starts 1.75)
  local v
  for v in "baseline.events=$be" "baseline.requests=$br" "baseline.sessions=$bs" \
    "loop.writes=$lw" "loop.reads=$lr" "loop.unit_starts=$lu"; do
    valid number "${v#*=}" ||
      die "the deployment file $CONFIG gives .load.${v%%=*} as '${v#*=}', which is not a number"
  done
  EVENTS=$(awk -v b="$be" -v l="$lw" -v f="$f" 'BEGIN { printf "%g", b + l * f }')
  REQUESTS=$(awk -v b="$br" -v l="$lr" -v f="$f" 'BEGIN { printf "%g", b + l * f }')
  SESSIONS=$bs
  UNIT_STARTS=$(awk -v l="$lu" -v f="$f" 'BEGIN { printf "%g", l * f }')
}

# step_mixes FACTOR: the step's -event-mix and -route-mix, into EVENT_MIX and ROUTE_MIX. A step at
# factor 0 is the baseline alone and takes the baseline's mixes; every step above it takes the
# loop's, which carry the loop's three write kinds and seven read routes.
step_mixes() {
  if awk -v f="$1" 'BEGIN { exit !(f > 0) }'; then
    EVENT_MIX=$(load_value loop.event_mix spec-loop-2026-11-03)
    ROUTE_MIX=$(load_value loop.route_mix routes-spec-loop-2026-11-03.txt)
  else
    EVENT_MIX=$(load_value baseline.event_mix production-2026-10-26)
    ROUTE_MIX=$(load_value baseline.route_mix "")
  fi
}

# loop_listener_urls: the four listener endpoints the loop's sessions register on, comma
# separated, in the order run.go reads them: the unit sessions round-robin over all but the last
# and the driver session takes the last. Each one is at the address of the machine the
# deployment file runs that listener on.
loop_listener_urls() {
  local role out=""
  for role in "${LOAD_LISTENERS[@]}"; do
    out+="${out:+,}http://$(vm_host "$(container_vm "$role")"):$(listener_port "$role")"
  done
  printf '%s\n' "$out"
}

# fetch_profiles DIR SECONDS: Dispatch's profiles, read on its own machine, into DIR. The
# profiling port binds 127.0.0.1 only (cmd/dispatch/pprof.go) and Dispatch runs with host
# networking, so that machine's own loopback is the one place they can be had. A fetch that
# fails is a note, never a step failure: the step's measurements stand without its profile.
fetch_profiles() {
  local dir=$1 seconds=$2 entry path file url bound
  for entry in "${PPROF_FETCHES[@]}"; do
    path=${entry%% *} file=${entry##* }
    # shellcheck disable=SC2059 # the path carries at most one %s, the profile's seconds
    path=$(printf "$path" "$seconds")
    url="http://127.0.0.1:$(port_of pprof)/debug/pprof/$path"
    bound=$((seconds + 120))
    if on_vm_for "$bound" "$DISPATCH_VM" curl -fsS --max-time "$bound" "$url" >"$dir/$file.part" 2>/dev/null &&
      [ -s "$dir/$file.part" ]; then
      mv -- "$dir/$file.part" "$dir/$file"
    else
      rm -f -- "$dir/$file.part"
      note "the $DISPATCH_VM VM gave no $file from $url; the step is whole without it"
    fi
  done
}

# run_step NAME FACTOR DURATION WARMUP: one step into steps/NAME. It returns 3, having removed
# the step's directory, when a machine stopped: the deployment's machines stop themselves 72
# hours after they start (R11), and a step the load never reached is no Dispatch failure to
# record.
run_step() {
  local name=$1 factor=$2 duration=$3 warmup=$4
  valid step "$name" || die "step name '$name' may hold only letters, digits, '.', '_' and '-', and at most 63 of them"
  valid factor "$factor" || die "factor '$factor' is not a number"
  valid duration "$duration" || die "--duration '$duration' is not a whole number of s, m or h"
  valid duration "$warmup" || die "--warmup '$warmup' is not a whole number of s, m or h"
  local dir=$RUN_DIR/steps/$name work remote seconds rc=0
  [ ! -e "$dir" ] || die "$dir exists: name another step, or remove it"
  [ -f "$RUN_DIR/seed.json" ] || die "no $RUN_DIR/seed.json: vm.sh seed first"
  if ! vms_running; then
    note "step $name is discarded before it started: $VMS_STOPPED. A machine that Google stopped at its 72-hour run-time limit is not a Dispatch failure; start it again and run this step over."
    return 3
  fi
  work=$(remote_work) remote=$(remote_loadgen)
  step_rates "$factor"
  step_mixes "$factor"
  mkdir -p -- "$dir" "$RUN_DIR/logs"
  note "step $name: factor $factor, $EVENTS events/s, $REQUESTS requests/s, $SESSIONS sessions, $UNIT_STARTS unit starts/s, window $duration after $warmup"

  reset_deployment "$dir/reset.json"
  # The step's -pprof is the tunnel's loopback address on the load machine, and the tunnel is
  # opened and proven before any load: the sampler inside loadgen run reads Dispatch's pool
  # acquire waiters through it, and criterion (e) is judged on nothing without them. The EXIT
  # trap ends both the tunnel and the sampler however the step ends -- a refusal below, an
  # interrupt, a failure of the ssh that carries the load, a failure of the collection after it.
  endpoints "$(pprof_tunnel_url)"
  seconds=$(($(duration_seconds "$warmup") + $(duration_seconds "$duration") + 20 + RUN_SLACK_SECONDS))
  trap step_cleanup EXIT
  if ! tunnel_start "$name"; then
    step_cleanup
    rm -rf -- "$dir"
    die "step $name is refused: $TUNNEL_REFUSAL Without the profiling tunnel loadgen's sampler reads no goroutines, no pool acquire waiters and no heap while the step runs, so pass criterion (e), Dispatch's pool acquire waiters p50 = 0, is FAIL for want of a measurement and the step measures nothing. Its directory is removed, so the step runs again under its own name once the tunnel can be opened."
  fi
  sampler_start
  local args=(
    run -name "$name" -out "$work/steps/$name"
    -loop-seed "$work/loopseed.seed.json" -loop-factor "$factor"
    -generator-role "vm-$(vm_host "$LOAD_VM")"
    -events "$EVENTS" -requests "$REQUESTS" -sessions "$SESSIONS"
    -unit-starts "$UNIT_STARTS" -unit-life "$(load_value unit_life 5m)"
    -loop-listeners "$(loop_listener_urls)" -driver-topics "$(load_value driver_topics 614)"
    -receipt-deadline "$(load_value receipt_deadline 1s)"
    -event-mix "$EVENT_MIX"
    -duration "$duration" -warmup "$warmup" -grace 20s
    -session-delivery "$(load_value session_delivery consumer)"
  )
  [ -z "$ROUTE_MIX" ] || args+=(-route-mix "$ROUTE_MIX")
  args+=("${ENDPOINTS[@]}")
  on_vm_for "$seconds" "$LOAD_VM" "$remote" "${args[@]}" >"$dir/loadgen.out" 2>&1 || rc=$?
  step_cleanup
  trap - EXIT
  if ! vms_running; then
    note "step $name is discarded: $VMS_STOPPED. A machine that Google stopped at its 72-hour run-time limit is not a Dispatch failure; start it again and run this step over."
    rm -rf -- "$dir"
    return 3
  fi
  ((rc == 0)) || note "step $name: loadgen run ended with exit $rc (see $dir/loadgen.out)"
  step_copy_back "$name" "$dir"
  fetch_profiles "$dir" "$(duration_seconds "$duration")"
  loadgen_local cgroup-merge "$dir" "$CGROUP_JSONL" || note "step $name: loadgen cgroup-merge failed"
  loadgen_local cloudsql-cpu -step "$dir" -project "$(cfg_valid name .project)" \
    -instance "$(cfg_valid name .cloudsql.instance)" -vcpus "$(cfg .cloudsql.vcpus 16)" ||
    note "step $name: loadgen cloudsql-cpu failed"
  loadgen_local tables "$dir" || note "step $name: loadgen tables failed"
  note "step $name done: $dir"
}

# step_copy_back NAME DIR: the load machine's step directory into DIR, and cleared there.
step_copy_back() {
  local name=$1 dir=$2 src
  src=$(remote_work)/steps/$name
  if on_vm_for 3600 "$LOAD_VM" tar -C "$src" -czf - . | tar -C "$dir" -xzf -; then
    on_vm "$LOAD_VM" rm -rf -- "$src" >/dev/null 2>&1 || true
  else
    note "step $name: copying $src back from the $LOAD_VM VM failed; its files stay there"
  fi
}

cmd_step() {
  parse "config run loadgen duration warmup" "" "$@"
  positional 2 "NAME FACTOR"
  use_run
  oslogin_ready
  local rc=0
  run_step "${POS[0]}" "${POS[1]}" "${OPT[duration]:-10m}" "${OPT[warmup]:-30s}" || rc=$?
  return "$rc"
}

# ---- the series -------------------------------------------------------------------------------------

# steps_in_run_order: every step directory of the run, in the order the steps ran, into DIRS: by
# meta.json's start, else by reset.json's started; a directory with neither goes last.
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

# curves_run: loadgen curves -series loop over every step of the run, in the order they ran, into
# <run>/curves. It is what collect runs, and what the series reads a step's verdict from.
curves_run() {
  steps_in_run_order
  ((${#DIRS[@]})) || die "no step of run $RUN has a directory, so there is nothing to draw"
  loadgen_local curves -series loop -out "$RUN_DIR/curves" "${DIRS[@]}" || die "loadgen curves failed"
}

# step_verdict NAME: the step's own cell of curves.csv's passes column: true, false or void.
# `passes` is that file's last column (renderCurvesCSV writes it last) and holds one of those
# three words, so it is never a quoted field; the step's name is the first and is never one
# either. Empty when the file has no row of that step.
step_verdict() {
  local csv=$RUN_DIR/curves/curves.csv
  [ -f "$csv" ] || die "loadgen curves wrote no $csv"
  awk -F, -v name="$1" '$1 == name { print $NF }' "$csv" | tail -n1
}

# run_series LONG LONG_WARMUP DURATION: the series of R7. The baseline, 1x and 2x steps run
# whatever they read, since the first three are the question the issue asks; the grid stops at the
# first step that does not pass, and a void step stops it too, since a starved generator measures
# nothing and the next factor would measure less.
run_series() {
  local long=$1 long_warmup=$2 duration=$3 f verdict rc
  local -a head=("baseline 0" "1x 1" "2x 2")
  local entry name factor
  for entry in "${head[@]}"; do
    name=${entry%% *} factor=${entry##* }
    rc=0
    run_step "$name" "$factor" "$long" "$long_warmup" || rc=$?
    ((rc != 3)) || return 3
    ((rc == 0)) || note "series: step $name ended with exit $rc; the series goes on"
  done
  for f in 3 4 6 8 12 16 20; do
    rc=0
    run_step "${f}x" "$f" "$duration" 30s || rc=$?
    ((rc != 3)) || return 3
    ((rc == 0)) || note "series: step ${f}x ended with exit $rc; the series goes on"
    curves_run
    verdict=$(step_verdict "${f}x")
    case $verdict in
      true) note "series: step ${f}x passes" ;;
      void)
        note "series: step ${f}x is void (its own generator was starved), so it ends no bracket and the series stops here; run it again with a larger load machine"
        return 0
        ;;
      *)
        note "series: step ${f}x is the first failing step (curves.csv passes $verdict); the grid stops here and the refinement follows"
        break
        ;;
    esac
  done
  refine_run "$duration"
}

# refine_run DURATION: the refinement. loadgen curves over every step writes refine.txt, one
# factor per knee whose bracket it can still narrow; each factor F runs as step <F>x for
# DURATION, then curves again, until refine.txt is empty.
refine_run() {
  local duration=$1 f factors=() rc
  while :; do
    curves_run
    [ -f "$RUN_DIR/curves/refine.txt" ] || die "refinement: loadgen curves wrote no $RUN_DIR/curves/refine.txt"
    mapfile -t factors < <(grep -v '^[[:space:]]*$' "$RUN_DIR/curves/refine.txt")
    ((${#factors[@]})) || break
    note "refinement: refine.txt names the factors ${factors[*]}"
    for f in "${factors[@]}"; do
      valid factor "$f" || die "refinement: refine.txt line '$f' is not a factor"
      # A factor that comes back, or a step that leaves no directory, would loop for ever.
      [ ! -e "$RUN_DIR/steps/${f}x" ] || die "refinement: refine.txt names factor $f again, but step ${f}x has run; stopping"
      rc=0
      run_step "${f}x" "$f" "$duration" 30s || rc=$?
      ((rc != 3)) || return 3
      [ -d "$RUN_DIR/steps/${f}x" ] || die "refinement: step ${f}x did not start; stopping"
    done
  done
  note "refinement done: refine.txt is empty; $RUN_DIR/curves/KNEES.md names the knees"
}

cmd_series() {
  parse "config run loadgen duration long long-warmup" "" "$@"
  positional 0 "no positional arguments"
  use_run
  oslogin_ready
  local duration=${OPT[duration]:-10m} long=${OPT[long]:-60m} long_warmup=${OPT[long-warmup]:-5m} rc=0
  valid duration "$duration" || die "series: --duration '$duration' is not a whole number of s, m or h"
  valid duration "$long" || die "series: --long '$long' is not a whole number of s, m or h"
  valid duration "$long_warmup" || die "series: --long-warmup '$long_warmup' is not a whole number of s, m or h"
  run_series "$long" "$long_warmup" "$duration" || rc=$?
  if ((rc == 3)); then
    note "series: the run stopped because a machine is not running (above); no false step was recorded"
    return 3
  fi
  return "$rc"
}

# ---- collect ------------------------------------------------------------------------------------

cmd_collect() {
  parse "config run loadgen" "" "$@"
  positional 0 "no positional arguments"
  use_run
  curves_run
  note "collect done: $RUN_DIR/curves"
}

# ---- down ---------------------------------------------------------------------------------------

cmd_down() {
  parse "config run" "" "$@"
  positional 0 "no positional arguments"
  use_run
  oslogin_ready
  local role
  stub_embed_stop
  # In the reverse of the start order, so Dispatch stops before what it talks to.
  for ((i = ${#CONTAINERS[@]} - 1; i >= 0; i--)); do
    role=${CONTAINERS[i]%% *}
    container_stop "$role"
  done
  note "down done: every container of run $RUN is stopped. The machines and the Cloud SQL instance are left as they are: dispatch://INFRA-1051's merge removes them."
}

main() {
  CMD=${1:-}
  [ $# -eq 0 ] || shift
  case $CMD in
    up) cmd_up "$@" ;;
    check) cmd_check "$@" ;;
    seed) cmd_seed "$@" ;;
    reset) cmd_reset "$@" ;;
    step) cmd_step "$@" ;;
    series) cmd_series "$@" ;;
    collect) cmd_collect "$@" ;;
    down) cmd_down "$@" ;;
    *) usage ;;
  esac
}

# vm.test.sh sources this file for its functions.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
