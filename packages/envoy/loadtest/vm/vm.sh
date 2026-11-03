#!/usr/bin/env bash
# vm.sh: the Dispatch load test on the separate deployment of INFRA-988 -- an application VM, a
# load-generator VM and a Cloud SQL instance of its own (dispatch://INFRA-1051) -- driven from a
# Legion worker pod over ssh. cluster/cluster.sh is the model: the same subcommands, the same flag
# parsing and refusals, the same run directory; only the deployment changes, from a Kubernetes
# namespace to two virtual machines and a managed instance.
#
#   vm.sh up --config FILE [--run ID] [--loadgen PATH]
#       Starts every container of the deployment by image digest, with the CPU, memory and
#       ephemeral limits of the specification below and nothing silently defaulted, installs
#       loadgen and the stub embedder on the load-generator VM, and waits until Dispatch answers
#       /healthz. It makes the run directory and names the run in <root>/current.
#   vm.sh check --config FILE [--run ID]
#       The read-back of acceptance criterion A1, as a markdown table of expected beside found:
#       every container's CPU, memory and image, Dispatch's network mode and ephemeral limit,
#       the instance's own settings from the Cloud SQL Admin API, each VM's run-time limit from
#       the Compute API, every endpoint setting of every container, and that each production
#       address the deployment file names is refused from each VM. Exit 1 on any difference.
#   vm.sh seed --config FILE [--run ID] [--embed-timeout 8h] [--workers 8]
#       loop-seed-plan, loop-seed-write (on the generator VM), loop-seed-check, loop-seed-export
#       and seed-finish with the embedding gate (-url), each phase's wall clock and the gate's
#       coverage progress recorded in <run>/seed.json; then the template database.
#   vm.sh reset --config FILE [--run ID]
#       The whole reset of decision D6, in order, its duration recorded.
#   vm.sh step --config FILE [--run ID] NAME FACTOR [--duration 10m] [--warmup 30s]
#       One step: the reset, the VM sampler, loadgen run at FACTOR, the profile fetched from the
#       application VM, cgroup-merge, cloudsql-cpu and tables.
#   vm.sh series --config FILE [--run ID] [--duration 10m] [--long 60m] [--long-warmup 5m]
#       The baseline, 1x and 2x steps at --long after a --long-warmup warm-up, then the grid 3,
#       4, 6, 8, 12, 16 and 20 at --duration, stopping at the first failing step, then the
#       refinement loadgen curves asks for, until refine.txt is empty.
#   vm.sh collect --config FILE [--run ID]
#       loadgen curves -series loop over the run's steps, in the order they ran, into <run>/curves.
#   vm.sh down --config FILE [--run ID]
#       Stops every container and the stub embedder. The VMs and the instance are left alone:
#       they are dispatch://INFRA-1051's to create and to remove.
#
# The container specification (R3 and the plan's departure 7), which up starts and check reads
# back. Every container is started by image digest, never by a tag:
#
#   container           VM          CPU    memory    other
#   dispatch            app         16     32 GiB    8 GiB ephemeral, DISPATCH_PPROF_PORT=6060,
#                                                    host networking
#   auth-proxy          app         200m   256 MiB
#   dispatch-listener   app         500m   512 MiB
#   nats                app         none   none      JetStream, its store on the VM's disk
#   hub-listener        app         none   none
#   load-listener-1..4  generator   500m   512 MiB
#
# Dispatch runs with host networking because its profiling port binds 127.0.0.1 only
# (cmd/dispatch/pprof.go), so the profile is fetched from the application VM itself, over ssh,
# rather than from this pod or from the generator; the step does that fetch, and the -pprof
# endpoint loadgen is given is whatever the deployment file names, since nothing here can make a
# loopback port reachable from another machine.
#
# Dispatch's identity is DISPATCH_IDENTITY=header:X-Dispatch-User with
# DISPATCH_IDENTITY_HEADER_TRUSTED=1 and a shared agent token, as stack.sh's start_dispatch sets
# them: the deployment has no front door, no Tailscale and no OAuth, and the report says so. Its
# embedder is loadgen stub-embed on the generator VM: DISPATCH_EMBEDDER=azure,
# DISPATCH_AZURE_EMBED_ENDPOINT at the stub's https address, DISPATCH_AZURE_EMBED_KEY,
# DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE=1000000 (production's quota) and SSL_CERT_FILE at the
# certificate the stub wrote.
#
# Nothing of the deployment is named here. Every machine, address, port, image, credential and
# production address comes from the deployment file --config names, because the deployment's own
# shape has already changed once under this issue (the application VM is n4d-standard-32 and the
# generator n4d-standard-16 in dispatch://INFRA-1051's later version) and will change again. The
# file is one JSON object:
#
#   {"project": "theorem-agent-infra", "zone": "us-west1-a",
#    "vms": {"app":       {"ssh": "lt@10.138.0.21", "host": "10.138.0.21", "instance": "lt-app"},
#            "generator": {"ssh": "lt@10.138.0.22", "host": "10.138.0.22", "instance": "lt-gen"}},
#    "cloudsql": {"instance": "lt-db", "connection_name": "p:us-west1:lt-db", "host": "10.139.0.3",
#                 "port": 5432, "user": "dispatch", "password": "...", "database": "dispatch",
#                 "template": "dispatch_seed", "sslmode": "require", "vcpus": 16,
#                 "expect": {"databaseVersion": "POSTGRES_18", "settings.edition": "ENTERPRISE"}},
#    "images": {"dispatch": "...@sha256:...", "listener": "...@sha256:...",
#               "proxy": "...@sha256:...", "nats": "...@sha256:..."},
#    "ports": {"dispatch": 8766, "pprof": 6060, "proxy": 5432, "listener": 9020,
#              "hub_listener": 9030, "nats": 4222, "nats_monitor": 8222, "stub_embed": 8443,
#              "load_listeners": [9021, 9022, 9023, 9024]},
#    "secrets": {"dispatch_token": "...", "listener_token": "...", "embed_key": "..."},
#    "paths": {"remote_root": "/var/lib/dispatch-loadtest"},
#    "production_addresses": ["10.139.0.2:5432", "10.138.0.10:4222"]}
#
# cloudsql.expect is read as it is written: each key is a path into the Cloud SQL Admin API's
# answer and each value what that path must read, so a change of production's instance is a
# change of the deployment file and not of this script. production_addresses are the addresses R2
# forbids; check proves each one refuses a connection from each VM. The optional keys and their
# defaults: ssh_command "ssh", paths.loadgen "$LOADTEST_ROOT/bin/loadgen",
# paths.remote_loadgen "<remote_root>/loadgen", paths.nats_store "<remote_root>/nats",
# paths.cert "<remote_root>/stub-embed.pem", paths.cgroup
# "/sys/fs/cgroup/system.slice/docker-%s.scope", token_url, sqladmin and compute (the Google
# endpoints, for a test server), and the whole of load (below).
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
# (mode 600, the deployment file's values that the run was started with), cgroup.jsonl, steps/,
# curves/ and logs/. up names the run in <root>/current, and every later subcommand takes that
# run unless --run names another.
#
# A step whose VM is stopped is discarded, never recorded: each VM stops itself 72 hours after it
# starts (R11), and a VM that is gone is not a Dispatch failure. step reads both VMs' Compute API
# status before the reset and again after the load; either one not RUNNING removes the step's
# directory, says which VM stopped, and ends the step with status 3, which series reports and
# stops on.
#
# Needs ssh, curl, jq, psql, nc (on the VMs), tar, gzip and loadgen. Every subcommand needs
# --config; none creates, resizes or removes a VM or the instance.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/../common.sh"
export LC_ALL=C TZ=UTC

VM_ROOT=${LOADTEST_VM_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest-vm}
# R11's run-time limit: Google stops each VM 72 hours after it starts, and check reads it back
# from the Compute API's .scheduling.maxRunDuration.
MAX_RUN_DURATION=259200s
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
# The Google endpoints and the metadata server's token, each overridable in the deployment file
# so a test server can stand in for them (cloudsqlcpu.go does the same).
TOKEN_URL_DEFAULT=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token
SQLADMIN_DEFAULT=https://sqladmin.googleapis.com
COMPUTE_DEFAULT=https://compute.googleapis.com

# CONTAINERS: every container of the deployment, in start order, one line each:
#
#   ROLE  VM  IMAGE KEY  CPUS  MEMORY
#
# CPUS and MEMORY are docker's own words (--cpus, --memory); "-" is a container with no limit,
# which docker reports as 0 and which check expects as 0. NATS and the hub listener have none, as
# production's hub VM gives its two processes the whole machine (envoy-infra gcp/cloudconfig.go).
CONTAINERS=(
  "nats app nats - -"
  "auth-proxy app proxy 0.2 256m"
  "hub-listener app listener - -"
  "load-listener-1 generator listener 0.5 512m"
  "load-listener-2 generator listener 0.5 512m"
  "load-listener-3 generator listener 0.5 512m"
  "load-listener-4 generator listener 0.5 512m"
  "dispatch-listener app listener 0.5 512m"
  "dispatch app dispatch 16 32g"
)
# The four listeners the loop's sessions register on: the unit sessions round-robin over all but
# the last and the driver session takes the last (run.go -loop-listeners).
LOAD_LISTENERS=(load-listener-1 load-listener-2 load-listener-3 load-listener-4)
# Dispatch's ephemeral storage limit, the setting that exists because Autopilot's 1Gi default
# evicted production's Dispatch (agent-infra dispatch/Pulumi.prod.yaml appEphemeralStorage).
# docker takes it as --storage-opt size=, which check reads back from HostConfig.StorageOpt.
DISPATCH_EPHEMERAL=8G
# Where the stub embedder's certificate is mounted inside the Dispatch container (SSL_CERT_FILE).
CERT_IN_CONTAINER=/etc/ssl/stub-embed.pem
# The environment variables whose values are credentials: check proves each one is set and never
# prints or compares its value.
SECRET_VARS=" DISPATCH_AGENT_TOKEN ENVOY_TOKEN ENVOY_API_TOKEN DISPATCH_AZURE_EMBED_KEY DATABASE_URL "
# The profiles a step fetches from the application VM, as `loadgen run` fetches them when it can
# reach the profiling port itself (run.go:868-878): the pprof path and the file it is written to.
PPROF_FETCHES=(
  "profile?seconds=%s cpu.pprof"
  "goroutine?debug=2 goroutines.txt"
  "heap heap.pprof"
  "heap?debug=1 heap.txt"
)

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
    ssh_target) re='^[A-Za-z0-9._@-]{1,255}$' ;;
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

# vm_ssh VM, vm_host VM, vm_instance VM: the ssh target, the address the other machines reach it
# at, and the Compute instance name of the application or generator VM.
vm_ssh() { cfg_valid ssh_target ".vms.$1.ssh"; }
vm_host() { cfg_valid host ".vms.$1.host"; }
vm_instance() { cfg_valid name ".vms.$1.instance"; }

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

dispatch_url() { printf 'http://%s:%s\n' "$(vm_host app)" "$(port_of dispatch)"; }
listener_url() { printf 'http://%s:%s\n' "$(vm_host app)" "$(port_of listener)"; }
nats_url() { printf 'nats://%s:%s\n' "$(vm_host app)" "$(port_of nats)"; }
nats_mon_url() { printf 'http://%s:%s\n' "$(vm_host app)" "$(port_of nats_monitor)"; }
pprof_url() { cfg .pprof_url "http://$(vm_host app):$(port_of pprof)"; }
stub_embed_url() { printf 'https://%s:%s\n' "$(vm_host generator)" "$(port_of stub_embed)"; }

# ---- ssh --------------------------------------------------------------------------------------

# ssh_cmd: the command that opens a shell on a VM, its arguments split on spaces, into SSH. It is
# also what loadgen vm-sample is given, so the sampler reaches a VM exactly as this script does.
ssh_cmd() {
  local raw
  raw=$(cfg .ssh_command ssh)
  read -r -a SSH <<<"$raw"
  ((${#SSH[@]})) || die "the deployment file $CONFIG gives an empty ssh_command"
}

# on_vm VM ARGS...: ARGS run on VM, bounded. Its standard output and status are the command's.
on_vm() {
  local vm=$1
  shift
  ssh_cmd
  timeout "$SSH_SECONDS" "${SSH[@]}" "$(vm_ssh "$vm")" -- "$@"
}

# on_vm_for SECONDS VM ARGS...: on_vm with a bound of its own, for the load and the seed.
on_vm_for() {
  local seconds=$1 vm=$2
  shift 2
  ssh_cmd
  timeout "$seconds" "${SSH[@]}" "$(vm_ssh "$vm")" -- "$@"
}

# put_file VM FILE DEST: FILE written on VM as DEST, mode 600, its directory made first. The file
# goes over ssh's standard input, so a credential in it never reaches a command line.
put_file() {
  local vm=$1 file=$2 dest=$3
  # shellcheck disable=SC2016 # expanded by the VM's sh
  on_vm "$vm" sh -c 'mkdir -p -- "$(dirname -- "$1")" && umask 077 && cat >"$1"' sh "$dest" <"$file" ||
    die "copying $file to $vm as $dest failed"
}

# get_file VM SOURCE DEST: VM's SOURCE into DEST on this pod.
get_file() {
  local vm=$1 source=$2 dest=$3
  on_vm "$vm" cat -- "$source" >"$dest" || die "reading $source from $vm failed"
}

docker_on() { # VM ARGS...
  local vm=$1
  shift
  on_vm "$vm" docker "$@"
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

# vms_running: both VMs answer RUNNING. VMS_STOPPED names those that do not, for the message a
# discarded step prints.
vms_running() {
  local vm status
  VMS_STOPPED=""
  for vm in app generator; do
    status=$(vm_status "$vm")
    [ "$status" = RUNNING ] || VMS_STOPPED+="${VMS_STOPPED:+, }the $vm VM $(vm_instance "$vm") is ${status:-unreadable}"
  done
  [ -z "$VMS_STOPPED" ]
}

# ---- the containers -----------------------------------------------------------------------------

# container_part ROLE N: field N of ROLE's line of CONTAINERS (1 role, 2 VM, 3 image key,
# 4 cpus, 5 memory).
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

container_vm() { container_part "$1" 2; }
container_image_key() { container_part "$1" 3; }
container_cpus() { container_part "$1" 4; }
container_memory() { container_part "$1" 5; }

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
# and the image by digest. The environment goes in a file on the VM (container_env_file), never
# on this command line.
container_run_args() {
  local role=$1 cpus memory image
  cpus=$(container_cpus "$role") memory=$(container_memory "$role") image=$(container_image "$role")
  printf '%s\n' --name "$role" --detach --restart no
  [ "$cpus" = - ] || printf '%s\n' --cpus "$cpus"
  [ "$memory" = - ] || printf '%s\n' --memory "$memory"
  case $role in
    dispatch)
      # Host networking, because DISPATCH_PPROF_PORT binds 127.0.0.1 only: on the host's loopback
      # the profile is reachable from the application VM itself, which is where step fetches it.
      printf '%s\n' --network host --storage-opt "size=$DISPATCH_EPHEMERAL"
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
  printf '%s\n' "$image"
  case $role in
    nats)
      # JetStream with its store on the VM's disk, which the volume above mounts at /data.
      printf '%s\n' -js -sd /data -p "$(port_of nats)" -m "$(port_of nats_monitor)"
      ;;
    auth-proxy)
      # The Cloud SQL Auth Proxy reaches the instance on its private address, as R2 requires.
      printf '%s\n' --address 0.0.0.0 --port "$(port_of proxy)" --private-ip "$(cfg .cloudsql.connection_name)"
      ;;
  esac
}

# container_env_file ROLE: where ROLE's environment file lives on its VM.
container_env_file() { printf '%s/env/%s.env\n' "$(remote_root)" "$1"; }

# container_write_env ROLE: ROLE's environment into that file, mode 600 on the VM, so no
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

# container_start ROLE: ROLE started on its VM, replacing whatever ran under that name.
container_start() {
  local role=$1 vm args=()
  vm=$(container_vm "$role")
  container_write_env "$role"
  docker_on "$vm" rm -f "$role" >/dev/null 2>&1 || true
  mapfile -t args < <(container_run_args "$role")
  docker_on "$vm" run "${args[@]}" >/dev/null || die "starting the container $role on the $vm VM failed"
  note "$role started on the $vm VM: $(container_image "$role")"
}

# container_stop ROLE: ROLE stopped and removed on its VM. A container that is not there is no
# failure: down and reset both run over containers that may already be gone.
container_stop() {
  local role=$1
  docker_on "$(container_vm "$role")" rm -f "$role" >/dev/null 2>&1 || true
}

# ---- the stub embedder --------------------------------------------------------------------------

# stub_embed_start: loadgen stub-embed on the generator VM, detached, with its certificate
# written where the application VM's Dispatch reads it as SSL_CERT_FILE. The key is given on the
# generator's command line alone; Dispatch takes it from its environment file.
stub_embed_start() {
  local cert=$RUN_DIR/stub-embed.pem remote work
  work=$(remote_work)
  remote=$(remote_loadgen)
  on_vm generator mkdir -p -- "$work" || die "making $work on the generator VM failed"
  # shellcheck disable=SC2016 # expanded by the generator's sh
  on_vm generator sh -c 'rm -f "$3"; nohup setsid "$1" stub-embed -key "$2" -listen "$4" -cert-out "$3" -host "$5" >"$6/stub-embed.log" 2>&1 </dev/null & echo "$!" >"$6/stub-embed.pid"' \
    sh "$remote" "$(cfg .secrets.embed_key)" "$work/stub-embed.pem" "0.0.0.0:$(port_of stub_embed)" "$(vm_host generator)" "$work" ||
    die "starting loadgen stub-embed on the generator VM failed"
  local waited=0
  # shellcheck disable=SC2016 # expanded by the generator's sh
  until on_vm generator sh -c 'test -s "$1"' sh "$work/stub-embed.pem" 2>/dev/null; do
    ((waited < STUB_EMBED_SECONDS)) || die "loadgen stub-embed wrote no certificate at $work/stub-embed.pem on the generator VM within $STUB_EMBED_SECONDS s; its output is $work/stub-embed.log there"
    sleep 2
    waited=$((waited + 2))
  done
  get_file generator "$work/stub-embed.pem" "$cert"
  put_file app "$cert" "$(cert_path)"
  note "the stub embedder serves $(stub_embed_url) on the generator VM; its certificate is $(cert_path) on the application VM"
}

# stub_embed_stop: the stub embedder ended by the pid it recorded.
stub_embed_stop() {
  # shellcheck disable=SC2016 # expanded by the generator's sh
  on_vm generator sh -c 'pid=$(cat "$1/stub-embed.pid" 2>/dev/null); kill -TERM "$pid" 2>/dev/null; exit 0' \
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

# wait_healthz: poll Dispatch's /healthz until it answers ok, within HEALTHZ_SECONDS.
wait_healthz() {
  local waited=0 body
  while :; do
    if body=$(curl -fsS --max-time 10 "$(dispatch_url)/healthz" 2>/dev/null) &&
      [ "$(jq -r '.status // empty' <<<"$body" 2>/dev/null)" = ok ]; then
      note "Dispatch answers /healthz ok at $(dispatch_url)"
      return 0
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
    --arg pod "${HOSTNAME:-unknown}" --argjson deployment "$(jq '{project, zone, vms, images, ports}' "$CONFIG")" \
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
  bin=${OPT[loadgen]:-$(cfg .paths.loadgen "${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}/bin/loadgen")}
  LOADTEST_ROOT=$RUN_DIR "$bin" "$@"
}

# loadgen_bin: the path of this pod's loadgen, for the copy onto the generator VM.
loadgen_bin() {
  printf '%s\n' "${OPT[loadgen]:-$(cfg .paths.loadgen "${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}/bin/loadgen")}"
}

# endpoints: every endpoint flag of loadgen seed and run (guard.go), into ENDPOINTS. Each one is
# the deployment's own, which is what R2 asks and what check reads back.
endpoints() {
  ENDPOINTS=(
    -allow-foreign
    -url "$(dispatch_url)" -token "$(cfg .secrets.dispatch_token)"
    -listener "$(listener_url)" -listener-token "$(cfg .secrets.listener_token)"
    -nats "$(nats_url)" -nats-mon "$(nats_mon_url)"
    -pprof "$(pprof_url)" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")"
  )
}

# ---- up -------------------------------------------------------------------------------------------

cmd_up() {
  parse "config run loadgen" "" "$@"
  positional 0 "no positional arguments"
  if [ -n "${OPT[run]:-}" ]; then use_run; else new_run; fi
  local role bin
  bin=$(loadgen_bin)
  [ -x "$bin" ] || die "up: no loadgen at $bin: build it (packages/envoy/loadtest/stack.sh setup) or name it with --loadgen"
  on_vm app mkdir -p -- "$(remote_root)" "$(nats_store)" || die "making $(remote_root) on the application VM failed"
  on_vm generator mkdir -p -- "$(remote_root)" "$(remote_work)" || die "making $(remote_root) on the generator VM failed"
  put_file generator "$bin" "$(remote_loadgen)"
  on_vm generator chmod 755 "$(remote_loadgen)" || die "chmod of $(remote_loadgen) on the generator VM failed"
  # The stub embedder first: Dispatch reads its certificate at startup (SSL_CERT_FILE), so a
  # Dispatch started before it has nothing to trust and every embed fails.
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
# issue's acceptance criterion A1 names, and Dispatch's network mode and ephemeral limit.
check_containers() {
  local role vm got want
  for role in "${CONTAINERS[@]%% *}"; do
    vm=$(container_vm "$role")
    want="/$role $(nanocpus "$(container_cpus "$role")") $(membytes "$(container_memory "$role")") $(container_image "$role")"
    got=$(docker_on "$vm" inspect -f '{{.Name}} {{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.Config.Image}}' "$role" 2>/dev/null) || got=""
    check_row "$role name cpu memory image ($vm VM)" "$want" "$got"
  done
  got=$(docker_on app inspect -f '{{.HostConfig.NetworkMode}} {{index .HostConfig.StorageOpt "size"}}' dispatch 2>/dev/null) || got=""
  check_row "dispatch network mode and ephemeral storage" "host $DISPATCH_EPHEMERAL" "$got"
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

# check_run_duration: each VM's run-time limit, which R11 sets to 72 hours.
check_run_duration() {
  local vm json
  for vm in app generator; do
    json=$(vm_json "$vm") || json='{}'
    check_row "$vm VM scheduling.maxRunDuration" "$MAX_RUN_DURATION" "$(jq -r '.scheduling.maxRunDuration // empty' <<<"$json" | tr -d '\n')"
  done
}

# check_production_refused: every production address R2 forbids is refused from each VM. nc's
# non-zero exit is the expectation: an address that answers is a path into production.
check_production_refused() {
  local vm address host port rc
  while read -r address; do
    [ -n "$address" ] || continue
    valid address "$address" || die "check: production_addresses holds '$address', which is not <host>:<port>"
    host=${address%:*} port=${address##*:}
    for vm in app generator; do
      rc=0
      on_vm "$vm" nc -z -w5 "$host" "$port" >/dev/null 2>&1 || rc=$?
      check_row "$vm VM to production $address" "refused" "$([ "$rc" -ne 0 ] && echo refused || echo answered)"
    done
  done < <(jq -r '(.production_addresses // [])[]' "$CONFIG")
}

cmd_check() {
  parse "config run" "" "$@"
  positional 0 "no positional arguments"
  use_run
  check_containers
  check_env
  check_instance
  check_run_duration
  check_production_refused
  printf '| check | expected | found | ok |\n| --- | --- | --- | --- |\n%s' "$CHECK_ROWS"
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
  seed_phase push-plan put_file generator "$plan" "$work/loopseed.plan.json"
  seed_phase write on_vm_for 86400 generator "$remote" loop-seed-write \
    -plan "$work/loopseed.plan.json" -out "$work/loopseed.meta.json" -workers "$workers" "${ENDPOINTS[@]}"
  seed_phase pull-meta get_file generator "$work/loopseed.meta.json" "$RUN_DIR/loopseed.meta.json"
  seed_phase check loadgen_local loop-seed-check -plan "$plan" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")"
  seed_phase export loadgen_local loop-seed-export -plan "$plan" -pg "$(pg_url "$(cfg .cloudsql.database dispatch)")" -out "$seed"
  seed_phase push-seed put_file generator "$seed" "$work/loopseed.seed.json"
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
  for role in dispatch dispatch-listener "${LOAD_LISTENERS[@]}" hub-listener; do
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
  for role in dispatch dispatch-listener "${LOAD_LISTENERS[@]}" hub-listener; do
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
  # 4. NATS again on a fresh JetStream store: the store is the VM's disk, so the restart alone
  # would keep every stream, consumer and KV bucket of the step before.
  container_stop nats
  # shellcheck disable=SC2016 # expanded by the application VM's sh
  on_vm app sh -c 'rm -rf -- "$1" && mkdir -p -- "$1"' sh "$(nats_store)" ||
    die "emptying the JetStream store $(nats_store) on the application VM failed"
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
  mkdir -p -- "$RUN_DIR/logs"
  reset_deployment "$RUN_DIR/logs/reset-$(date -u +%Y%m%dt%H%M%S).json"
}

# ---- the sampler --------------------------------------------------------------------------------

# sampler_targets: the -vm arguments of loadgen vm-sample, into SAMPLER_TARGETS: each VM's ssh
# target with the containers on it, each at the cgroup v2 directory its container id gives, so
# the generator VM is a series like any other and cgroup-merge reads them all the same way.
sampler_targets() {
  local vm role id pattern spec roles
  pattern=$(cfg .paths.cgroup '/sys/fs/cgroup/system.slice/docker-%s.scope')
  SAMPLER_TARGETS=()
  for vm in app generator; do
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
  ((${#SAMPLER_TARGETS[@]})) || die "the sampler has no VM to read: no container answered docker inspect"
}

# sampler_start: loadgen vm-sample over both VMs, appending to the run's cgroup.jsonl, its pid in
# SAMPLER_PID.
sampler_start() {
  sampler_targets
  ssh_cmd
  loadgen_local vm-sample -out "$CGROUP_JSONL" -ssh "${SSH[*]}" "${SAMPLER_TARGETS[@]}" \
    >>"$RUN_DIR/logs/vm-sample.log" 2>&1 &
  SAMPLER_PID=$!
  note "the sampler reads both VMs into $CGROUP_JSONL (pid $SAMPLER_PID)"
}

# sampler_stop: the sampler ended, which it does on SIGTERM.
sampler_stop() {
  [ -n "${SAMPLER_PID:-}" ] || return 0
  kill -TERM "$SAMPLER_PID" 2>/dev/null || true
  wait "$SAMPLER_PID" 2>/dev/null || true
  SAMPLER_PID=""
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
  for v in "$be" "$br" "$bs" "$lw" "$lr" "$lu"; do
    valid number "$v" || die "the deployment file $CONFIG gives '$v' as one of the load rates, which is not a number"
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
# and the driver session takes the last.
loop_listener_urls() {
  local role out=""
  for role in "${LOAD_LISTENERS[@]}"; do
    out+="${out:+,}http://$(vm_host generator):$(listener_port "$role")"
  done
  printf '%s\n' "$out"
}

# fetch_profiles DIR SECONDS: Dispatch's profiles, read on the application VM itself, into DIR.
# The profiling port binds 127.0.0.1 only (cmd/dispatch/pprof.go) and Dispatch runs with host
# networking, so the VM's own loopback is the one place they can be had. A fetch that fails is a
# note, never a step failure: the step's measurements stand without its profile.
fetch_profiles() {
  local dir=$1 seconds=$2 entry path file url bound
  for entry in "${PPROF_FETCHES[@]}"; do
    path=${entry%% *} file=${entry##* }
    # shellcheck disable=SC2059 # the path carries at most one %s, the profile's seconds
    path=$(printf "$path" "$seconds")
    url="http://127.0.0.1:$(port_of pprof)/debug/pprof/$path"
    bound=$((seconds + 120))
    if on_vm_for "$bound" app curl -fsS --max-time "$bound" "$url" >"$dir/$file.part" 2>/dev/null &&
      [ -s "$dir/$file.part" ]; then
      mv -- "$dir/$file.part" "$dir/$file"
    else
      rm -f -- "$dir/$file.part"
      note "the application VM gave no $file from $url; the step is whole without it"
    fi
  done
}

# run_step NAME FACTOR DURATION WARMUP: one step into steps/NAME. It returns 3, having removed
# the step's directory, when a VM stopped: the deployment's VMs stop themselves 72 hours after
# they start (R11), and a step the load never reached is no Dispatch failure to record.
run_step() {
  local name=$1 factor=$2 duration=$3 warmup=$4
  valid step "$name" || die "step name '$name' may hold only letters, digits, '.', '_' and '-'"
  valid factor "$factor" || die "factor '$factor' is not a number"
  valid duration "$duration" || die "--duration '$duration' is not a whole number of s, m or h"
  valid duration "$warmup" || die "--warmup '$warmup' is not a whole number of s, m or h"
  local dir=$RUN_DIR/steps/$name work remote seconds rc=0
  [ ! -e "$dir" ] || die "$dir exists: name another step, or remove it"
  [ -f "$RUN_DIR/seed.json" ] || die "no $RUN_DIR/seed.json: vm.sh seed first"
  if ! vms_running; then
    note "step $name is discarded before it started: $VMS_STOPPED. A VM that Google stopped at its 72-hour run-time limit is not a Dispatch failure; start it again and run this step over."
    return 3
  fi
  work=$(remote_work) remote=$(remote_loadgen)
  step_rates "$factor"
  step_mixes "$factor"
  mkdir -p -- "$dir" "$RUN_DIR/logs"
  note "step $name: factor $factor, $EVENTS events/s, $REQUESTS requests/s, $SESSIONS sessions, $UNIT_STARTS unit starts/s, window $duration after $warmup"

  reset_deployment "$dir/reset.json"
  endpoints
  seconds=$(($(duration_seconds "$warmup") + $(duration_seconds "$duration") + 20 + RUN_SLACK_SECONDS))
  sampler_start
  local args=(
    run -name "$name" -out "$work/steps/$name"
    -loop-seed "$work/loopseed.seed.json" -loop-factor "$factor"
    -generator-role "vm-$(vm_host generator)"
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
  on_vm_for "$seconds" generator "$remote" "${args[@]}" >"$dir/loadgen.out" 2>&1 || rc=$?
  sampler_stop
  if ! vms_running; then
    note "step $name is discarded: $VMS_STOPPED. A VM that Google stopped at its 72-hour run-time limit is not a Dispatch failure; start it again and run this step over."
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

# step_copy_back NAME DIR: the generator VM's step directory into DIR, and cleared there.
step_copy_back() {
  local name=$1 dir=$2 src
  src=$(remote_work)/steps/$name
  if on_vm_for 3600 generator tar -C "$src" -czf - . | tar -C "$dir" -xzf -; then
    on_vm generator rm -rf -- "$src" >/dev/null 2>&1 || true
  else
    note "step $name: copying $src back from the generator VM failed; its files stay there"
  fi
}

cmd_step() {
  parse "config run loadgen duration warmup" "" "$@"
  positional 2 "NAME FACTOR"
  use_run
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
        note "series: step ${f}x is void (its own generator was starved), so it ends no bracket and the series stops here; run it again with a larger generator VM"
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
  local duration=${OPT[duration]:-10m} long=${OPT[long]:-60m} long_warmup=${OPT[long-warmup]:-5m} rc=0
  valid duration "$duration" || die "series: --duration '$duration' is not a whole number of s, m or h"
  valid duration "$long" || die "series: --long '$long' is not a whole number of s, m or h"
  valid duration "$long_warmup" || die "series: --long-warmup '$long_warmup' is not a whole number of s, m or h"
  run_series "$long" "$long_warmup" "$duration" || rc=$?
  if ((rc == 3)); then
    note "series: the run stopped because a VM is not running (above); no false step was recorded"
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
  local role
  stub_embed_stop
  # In the reverse of the start order, so Dispatch stops before what it talks to.
  for ((i = ${#CONTAINERS[@]} - 1; i >= 0; i--)); do
    role=${CONTAINERS[i]%% *}
    container_stop "$role"
  done
  note "down done: every container of run $RUN is stopped. The VMs and the Cloud SQL instance are left as they are: dispatch://INFRA-1051's merge removes them."
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
