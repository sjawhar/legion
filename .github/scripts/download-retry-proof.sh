#!/usr/bin/env bash
# The fault rig of ci.yaml's download-retry and download-retry-images jobs: it starts
# .github/scripts/ci-fault-proxy.py in front of one step, reads the proxy's log after that step,
# and prints and checks what the retry did (dispatch://INFRA-1545, T7). Four verbs:
#
#   start <name> --mode <recover|stall> [--hosts <csv>] [--recover-after <s>] [--bind <address>]
#       Starts a proxy of its own for the step that follows, waits for its port, and writes
#       `proxy`, `port` and `started` to $GITHUB_OUTPUT, so the step names the proxy in its own
#       `env:` and nowhere else. One proxy per step and not one per job, because a recovery
#       window is counted from the proxy's first moment: a second step under the first step's
#       proxy would run after the window closed and prove nothing.
#
#   report <name> --what <phrase> --outcome <the step's outcome> --attempts <n> --floor <s>
#          --budget <s>
#       Reads that proxy's log and says, in lines a person reading the run can count, which site
#       it was, how many attempts reached the proxy, the offset of each from the first, the
#       seconds between the first attempt and the last, and the step's own elapsed seconds
#       against its budget. Then it checks what the mode promises (below) and stops the proxy.
#
#   build <name|none> --file <Dockerfile> --tag <tag> [--build-arg K=V]... [--secret <spec>]...
#       `docker buildx build` of one Dockerfile, with <name>'s proxy passed in as the build's
#       http_proxy and https_proxy build arguments, and `none` for a build that gets no proxy.
#       The build is here, in a script, rather than in the job's `run:` because
#       .github/scripts/check-image-trigger-paths.sh refuses any image build it can read in a
#       run step and cannot be told otherwise, and docker/build-push-action — the one form it
#       reads — would then hold this job's trigger paths to everything the three builds read,
#       which is the wide filter the narrow gating exists to avoid. Nothing here pushes, logs in
#       to a registry or tags for one: every build is `--load`, into the runner's own daemon,
#       and the runner is discarded. worker-image.yaml and ci.yaml's envoy-smoke job are where
#       these images are built for real, through that action, and their triggers are what
#       check-image-trigger-paths.sh proves.
#
#   clear
#       Kills any proxy still running and refuses the job if a proxy variable is in its
#       environment: the rig writes none to $GITHUB_ENV, so the steps after it — the ci-ok
#       report and the upload inside it — must see none. The one verb that names no proxy,
#       because it is about all of them.
#
# What `report` checks, which is what each of the three modes claims:
#   recover  the step succeeded, the proxy refused at least one attempt (so the fault was in the
#            site's path at all) and passed at least one on (so the site recovered through the
#            same proxy), inside the budget.
#   stall    the step failed, which is the retry exhausting its five attempts against a host
#            that answers nothing; the proxy was reached at least once per attempt; the step
#            took at least --floor, the loop's own sleeps, so every attempt really happened
#            rather than the site failing fast on something else; and it ended inside --budget,
#            which is what a per-attempt bound buys — the step ends instead of consuming the
#            job. Each attempt's own duration is printed, against the bound, rather than
#            asserted: a client with a timeout shorter than the bound ends sooner and still
#            retries, and the budget above is what the check is for.
#   fail     the step succeeded and the proxy refused nothing at all, because the site never
#            asked the faulted hosts. That is the shape of a dependency the tree carries rather
#            than fetches: those hosts answer 504 for the whole step and the install does not
#            notice. A request to a host outside --hosts is passed on and counted, since an
#            install reads the npm registry whatever this proves.
#
# Every path it writes goes under $RUNNER_TEMP (or $TMPDIR off a runner), and each name's state
# lives in a file of its own, so two sites in one job never read each other's proxy.
#
# Run from anywhere; the repository root is derived from this script's own path.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/../.." && pwd)
state_dir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/download-retry"

die() {
  echo "::error::download-retry: $*" >&2
  exit 1
}

state_file() {
  printf '%s/%s.state\n' "$state_dir" "$1"
}

# The docker bridge's gateway, which is the runner as a `RUN` of a build sees it: the build
# container's default route. Named rather than written as 172.17.0.1, which is only the usual
# value of a daemon's default pool.
docker_bridge_gateway() {
  local address
  address=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')
  [ -n "$address" ] || die "the docker bridge names no gateway, so a build cannot reach the proxy"
  printf '%s\n' "$address"
}

start() {
  local name=$1 mode="" hosts="" recover_after=60 bind=127.0.0.1
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --mode) mode=$2; shift 2 ;;
      --hosts) hosts=$2; shift 2 ;;
      --recover-after) recover_after=$2; shift 2 ;;
      --bind) bind=$2; shift 2 ;;
      *) die "start: unknown argument '$1'" ;;
    esac
  done
  [ -n "$mode" ] || die "start: --mode is required"
  [ "$bind" != docker-bridge ] || bind=$(docker_bridge_gateway)

  mkdir -p "$state_dir"
  local log="$state_dir/$name.log" port_file="$state_dir/$name.port" port="" pid=""
  rm -f "$log" "$port_file"
  # Each line of the proxy's log is stamped with the second it was written, which is what makes
  # the attempts countable afterwards: the proxy prints one line per request it refuses or
  # stalls, and the stamps turn those lines into a schedule. The stamping is a bash `read` loop
  # rather than an awk one, because mawk — the awk of the runner image and of this repository's
  # pods — holds its input in a block buffer, and a log that appears only once the proxy exits
  # is a log the step after it cannot read. $EPOCHSECONDS is bash's own clock, so no line costs
  # a process.
  (
    python3 "$root/.github/scripts/ci-fault-proxy.py" \
      --mode "$mode" --hosts "$hosts" --recover-after "$recover_after" \
      --bind "$bind" --port 0 --port-file "$port_file" 2>&1 \
      | while IFS= read -r line; do printf '%s %s\n' "$EPOCHSECONDS" "$line"; done > "$log"
  ) &
  local started
  started=$(date +%s)
  # The port file is written whole, by rename, before the proxy serves a request, and the
  # recovery window starts the moment it appears (ci-fault-proxy.py's header).
  local waited=0
  while [ ! -s "$port_file" ]; do
    sleep 0.1
    waited=$((waited + 1))
    [ "$waited" -lt 300 ] || { cat "$log" >&2 || true; die "start: the $name proxy reported no port in 30 s"; }
  done
  port=$(cat "$port_file")
  # The proxy announces its port before it logs the line that carries its pid, which `report`
  # stops it by, so the pid is waited for too rather than read from a log that is still empty.
  while [ -z "$pid" ]; do
    pid=$(awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^pid=/) { sub(/^pid=/, "", $i); print $i; exit } }' "$log")
    [ -n "$pid" ] && break
    sleep 0.1
    waited=$((waited + 1))
    [ "$waited" -lt 300 ] || { cat "$log" >&2 || true; die "start: the $name proxy logged no pid in 30 s"; }
  done

  {
    echo "name=$name"
    echo "mode=$mode"
    echo "log=$log"
    echo "pid=$pid"
    echo "started=$started"
    echo "proxy=http://$bind:$port"
  } > "$(state_file "$name")"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      echo "proxy=http://$bind:$port"
      echo "port=$port"
      echo "started=$started"
    } >> "$GITHUB_OUTPUT"
  fi
  echo "download-retry: the $name fault proxy is http://$bind:$port (--mode $mode, faulting ${hosts:-every host})"
}

report() {
  local name=$1 what="" outcome="" attempts=1 floor=0 budget=0
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --what) what=$2; shift 2 ;;
      --outcome) outcome=$2; shift 2 ;;
      --attempts) attempts=$2; shift 2 ;;
      --floor) floor=$2; shift 2 ;;
      --budget) budget=$2; shift 2 ;;
      *) die "report: unknown argument '$1'" ;;
    esac
  done
  local file
  file=$(state_file "$name")
  [ -f "$file" ] || die "report: no proxy named $name was started"
  local mode log pid started
  # shellcheck disable=SC1090 # the file this verb's own `start` wrote, five name=value lines
  . "$file"
  local now elapsed
  now=$(date +%s)
  elapsed=$((now - started))

  local faults passes fault_count first last
  # One line per request the proxy refused or stalled, as offsets from the step's start, and the
  # same for the ones it passed on. An attempt may open more than one connection — a package
  # manager fetches in parallel — so these are requests, not attempts; the count below is held
  # to at least one per attempt, and the offsets are what a reader counts the attempts by.
  faults=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { n++; if (n <= 40) printf "+%ds ", $1 - t }
                                END { if (n > 40) printf "and %d more", n - 40 }' "$log")
  passes=$(awk '$3 == "pass"' "$log" | wc -l)
  fault_count=$(awk '$3 == "504" || $3 == "stall"' "$log" | wc -l)
  first=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { print $1 - t; exit }' "$log")
  last=$(awk -v t="$started" '$3 == "504" || $3 == "stall" { v = $1 - t } END { if (v != "") print v }' "$log")

  echo "download-retry: $what, under --mode $mode"
  echo "  the step's own outcome: $outcome"
  echo "  the proxy was reached $fault_count time(s) while it faulted, at ${faults:-no offset at all}"
  if [ -n "$first" ] && [ -n "$last" ]; then
    echo "  the first of those was $first s into the step and the last $last s, $((last - first)) s apart"
  fi
  echo "  requests the proxy passed on: $passes"
  echo "  it ran ${elapsed}s from the proxy's start, against a budget of ${budget}s and a floor of ${floor}s for its $attempts attempt(s)"

  kill "$pid" 2> /dev/null || true

  local problems=0
  fail() { echo "::error::download-retry: $what: $*" >&2; problems=$((problems + 1)); }
  case $mode in
    recover)
      [ "$outcome" = success ] || fail "the step did not succeed, so the site did not recover from the fault"
      [ "$fault_count" -ge 1 ] || fail "the proxy refused nothing, so the fault was never in this site's path"
      [ "$passes" -ge 1 ] || fail "the proxy passed nothing on, so the site never fetched through it after the window"
      ;;
    stall)
      [ "$outcome" = failure ] || fail "the step did not fail, so its download never reached the stalling proxy"
      [ "$fault_count" -ge "$attempts" ] || fail "the proxy was reached $fault_count time(s), fewer than the $attempts attempts the retry makes"
      [ "$elapsed" -ge "$floor" ] || fail "it ended after ${elapsed}s, under the ${floor}s its attempts' sleeps alone cost, so they did not all run"
      ;;
    fail)
      [ "$outcome" = success ] || fail "the step did not succeed, though every host it reads but the faulted ones answered"
      [ "$fault_count" -eq 0 ] || fail "the proxy refused $fault_count request(s), so this site still fetches from a host the tree is supposed to carry"
      ;;
    *) fail "unknown mode $mode" ;;
  esac
  [ "$elapsed" -le "$budget" ] || fail "it took ${elapsed}s, over its ${budget}s budget"
  [ "$problems" -eq 0 ] || exit 1
  echo "  as this mode promises"
}

build() {
  local name=$1 file="" tag="" proxy="" args=()
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --file) file=$2; shift 2 ;;
      --tag) tag=$2; shift 2 ;;
      --build-arg) args+=(--build-arg "$2"); shift 2 ;;
      --secret) args+=(--secret "$2"); shift 2 ;;
      *) die "build: unknown argument '$1'" ;;
    esac
  done
  [ -n "$file" ] || die "build: --file is required"
  [ -n "$tag" ] || die "build: --tag is required"
  if [ "$name" != none ]; then
    local state
    state=$(state_file "$name")
    [ -f "$state" ] || die "build: no proxy named $name was started"
    proxy=$(sed -n 's/^proxy=//p' "$state")
    [ -n "$proxy" ] || die "build: the $name state names no proxy"
    # Docker's predefined proxy build arguments: BuildKit sets each as an environment variable
    # of every RUN and keeps it out of the cache key, so the retries inside the Dockerfile meet
    # the fault without the Dockerfile naming a proxy anywhere. The address is the docker bridge
    # gateway (`start --bind docker-bridge`), which is the runner as a RUN's own network
    # namespace can route to it; 127.0.0.1 there is the build container itself.
    args+=(--build-arg "http_proxy=$proxy" --build-arg "https_proxy=$proxy")
  fi
  echo "download-retry: building $file as $tag${proxy:+ through $proxy}"
  # --no-cache: the layers this build must run are the ones that fetch, and a cached layer
  # fetches nothing, so a build that reused one would meet no fault. --load puts the result in
  # the runner's daemon; nothing is pushed and no registry is logged in to.
  docker buildx build --no-cache --load --file "$file" --tag "$tag" "${args[@]}" "$root"
  # The tag exists only once the last stage is exported, so this is the build reaching it.
  docker image inspect "$tag" --format 'download-retry: {{.RepoTags}} reached its last stage, {{.Id}}'
}

# The rig writes no proxy variable to $GITHUB_ENV, so none of them reaches the steps after it —
# the ci-ok report and the upload inside it. This is where that is checked rather than assumed,
# and where a proxy a `report` could not stop is stopped.
clear() {
  [ "$#" -eq 0 ] || die "clear: takes no argument"
  pkill -f ci-fault-proxy.py || true
  local left
  left=$(env | grep -Ei '^(http_proxy|https_proxy|all_proxy|no_proxy)=' || true)
  if [ -n "$left" ]; then
    echo "::error::the job's environment carries a proxy the ci-ok report would use: $left"
    exit 1
  fi
  echo "no proxy variable is in this job's environment"
}

verb=${1:-}
# Every verb but `clear` names the proxy it is about; `clear` is about all of them.
[ "$verb" = clear ] || [ "$#" -ge 2 ] ||
  die "usage: $(basename "$0") <start|report|build> <name> [options], or <clear>"
shift
case $verb in
  start) start "$@" ;;
  report) report "$@" ;;
  build) build "$@" ;;
  clear) clear "$@" ;;
  *) die "unknown verb '$verb'; it takes start, report, build or clear" ;;
esac
