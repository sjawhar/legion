#!/usr/bin/env bash
# The measurement spike of dispatch://INFRA-1443, run as a scripts/e2e proof.
#
# Why a proof script and not the `container:` job of .github/workflows/spike-infra-1443.yaml,
# which is written beside it: the Legion role App this spike runs under (legion-reviewer) has no
# `workflows` permission, so no new file under .github/workflows/ can be pushed, and no
# `packages: write`, so no image can be published for a `container:` job to pull. This script is
# the same measurement through the one door that is open: .github/workflows/e2e.yaml's
# `workflow_dispatch` reads .github/e2e-scripts.json and runs scripts/e2e/<name>.sh from the ref
# it is dispatched at, and both of those are ordinary files.
#
# What it does, on a GitHub-hosted ubuntu-24.04 runner:
#
#   1. times a `docker pull` of the Playwright base image (990,220,611 bytes in 7 layers) and of
#      an image of this repository's own ghcr package, so the registry throughput this runner
#      sees is measured rather than predicted;
#   2. builds .github/docker/browser.Dockerfile and reads the built image's compressed size and
#      layer count out of an OCI export, which is what a registry would store and a job pull;
#   3. starts that image as a container with the mount set, working directory and HOME a GitHub
#      `container:` job gets, twice -- once as the image's default user (root) and once as
#      `--user 1001`, the uid the runner owns its work tree with -- and in each one builds the
#      contracts and the Dispatch server, starts the baked `weed` on 127.0.0.1:18333, times the
#      DISPATCH_E2E_PG_BIN provider (one initdb, four lane copies) and runs the real Dispatch
#      WebKit and Firefox browser suite over four lanes.
#
# What it cannot answer, and what the `container:` job still has to: GitHub's own
# `Initialize containers` step, which is a `docker pull` plus a `docker create`, a `docker start`
# and two `docker exec`s -- each of which is timed separately here.
#
# Run it from a checkout on a Linux host with docker, bun, go and this repository's node_modules,
# as INFRA_1443_EVIDENCE_DIR=/tmp/evidence bash scripts/e2e/infra-1443-browser-image.sh
set -euo pipefail

cd "$(dirname "$0")/../.."
repo="$PWD"

evidence="${INFRA_1443_EVIDENCE_DIR:-$repo/infra-1443-evidence}"
mkdir -p "$evidence"

# The base, the ghcr image whose pull measures this runner's throughput to the registry a
# `container:` job would pull from, and the tag the build lands under locally.
base="mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f"
ghcr_probe="ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
buildkit="ghcr.io/cybermonklabs/legion/ci@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea"
image="infra-1443/browser:spike"

say() { printf '\n=== %s\n' "$*"; }

# Seconds, to two places, between two `date +%s.%N` readings.
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }

# One line per measurement, collected for the summary at the end and for the evidence directory.
measurements="$evidence/measurements.txt"
: > "$measurements"
record() {
  printf '%s\n' "$*" | tee -a "$measurements"
}

say "the runner"
uname -a
nproc
free -m | cat
df -h / /mnt 2>/dev/null | cat
docker version --format '{{.Server.Version}}'
echo "id: $(id)"
echo "GITHUB_WORKSPACE=${GITHUB_WORKSPACE:-<unset>} (owned by $(stat -c '%u:%g' "$repo"))"
echo "bun: $(command -v bun || echo MISSING) $(bun --version 2>/dev/null || true)"
echo "go:  $(command -v go || echo MISSING) $(go version 2>/dev/null || true)"
# The runner's own font set, against which the image's is read: a layout row measures text, so
# two different font sets are two different measurements.
echo "runner font files: $(find /usr/share/fonts /usr/local/share/fonts -type f \( -name '*.ttf' -o -name '*.ttc' -o -name '*.otf' -o -name '*.pfb' \) 2>/dev/null | wc -l)"
echo "runner families: $(fc-list : family 2>/dev/null | sed 's/,.*//' | sort -u | tr '\n' '|')"
echo "runner fc-match sans-serif: $(fc-match sans-serif 2>/dev/null || echo 'fc-match MISSING')"

say "pull the Playwright base image (990,220,611 bytes compressed, 7 layers)"
docker image rm -f "$base" > /dev/null 2>&1 || true
start=$(date +%s.%N)
docker pull --quiet "$base"
stop=$(date +%s.%N)
base_pull=$(elapsed "$start" "$stop")
record "base image pull from mcr.microsoft.com: ${base_pull} s for 990220611 bytes ($(awk -v s="$base_pull" 'BEGIN { printf "%.1f", 990220611 / 1048576 / s }') MiB/s)"

say "pull one image of this repository's own ghcr package, for the registry's throughput"
docker image rm -f "$ghcr_probe" > /dev/null 2>&1 || true
# The copy in the ci package is a multi-platform index, so its own manifest holds no `layers`:
# the bytes a pull moves are the amd64 child's. `docker manifest inspect` answers with the
# index, which is what the first attempt at this measurement read as null.
probe_raw=$(docker buildx imagetools inspect "$ghcr_probe" --raw)
if printf '%s' "$probe_raw" | jq -e '.manifests' > /dev/null 2>&1; then
  probe_child=$(printf '%s' "$probe_raw" |
    jq -r '.manifests[] | select(.platform.architecture == "amd64" and .platform.os == "linux") | .digest')
  probe_raw=$(docker buildx imagetools inspect "ghcr.io/cybermonklabs/legion/ci@${probe_child}" --raw)
fi
probe_bytes=$(printf '%s' "$probe_raw" | jq '[.layers[].size] | add')
start=$(date +%s.%N)
docker pull --quiet "$ghcr_probe"
stop=$(date +%s.%N)
ghcr_pull=$(elapsed "$start" "$stop")
record "ghcr.io pull (pgvector copy): ${ghcr_pull} s for ${probe_bytes} bytes ($(awk -v s="$ghcr_pull" -v b="$probe_bytes" 'BEGIN { printf "%.1f", b / 1048576 / s }') MiB/s)"

say "build .github/docker/browser.Dockerfile"
# The docker-container driver, so the build can export an OCI archive below; its BuildKit is this
# repository's own mirror of moby/buildkit (.github/ci-images.yaml), never Docker Hub.
docker buildx create --name infra1443 --driver docker-container \
  --driver-opt "image=$buildkit" --driver-opt provenance-add-gha=false --use > /dev/null
start=$(date +%s.%N)
docker buildx build \
  --builder infra1443 \
  --file .github/docker/browser.Dockerfile \
  --platform linux/amd64 \
  --provenance=false \
  --tag "$image" \
  --load \
  --progress plain \
  .github/docker 2>&1 | tee "$evidence/build.log"
stop=$(date +%s.%N)
record "image build (cold, including the base pull into the builder): $(elapsed "$start" "$stop") s"

say "the built image's compressed size and layer count"
# A second build, which the first one's cache answers in seconds, exporting the OCI archive a
# registry would hold: its manifest's layer sizes are the bytes a `container:` job would pull.
docker buildx build \
  --builder infra1443 \
  --file .github/docker/browser.Dockerfile \
  --platform linux/amd64 \
  --provenance=false \
  --output "type=oci,dest=$evidence/image-oci.tar" \
  .github/docker > /dev/null 2>&1
oci="$evidence/oci"
rm -rf "$oci"
mkdir -p "$oci"
tar -xf "$evidence/image-oci.tar" -C "$oci"
rm -f "$evidence/image-oci.tar"
manifest_digest=$(jq -r '.manifests[0].digest' "$oci/index.json")
manifest="$oci/blobs/sha256/${manifest_digest#sha256:}"
if jq -e '.manifests' "$manifest" > /dev/null 2>&1; then
  child=$(jq -r '.manifests[] | select(.platform.architecture == "amd64") | .digest' "$manifest")
  manifest="$oci/blobs/sha256/${child#sha256:}"
fi
layers=$(jq '.layers | length' "$manifest")
bytes=$(jq '[.layers[].size] | add' "$manifest")
record "built image: ${bytes} bytes compressed ($(awk -v b="$bytes" 'BEGIN { printf "%.1f", b / 1048576 }') MiB) in ${layers} layers"
record "the base alone: 990220611 bytes (944.3 MiB) in 7 layers; this image adds $((bytes - 990220611)) bytes and $((layers - 7)) layers"
jq -r '.layers[] | "  \(.size)\t\(.digest)"' "$manifest" | tee "$evidence/layers.txt"
rm -rf "$oci"

# Where bun and go are on this runner, so the container sees the same two toolchains at the same
# paths a `container:` job's own setup-bun and setup-go would put them at.
bun_dir=$(dirname "$(command -v bun)")
go_dir=$(dirname "$(command -v go)")
runner_home="$HOME"

# One variant: a container of the built image with the mount set, working directory and HOME a
# GitHub `container:` job is given, started as $2, with the whole suite run inside it.
variant() {
  local name=$1 user=$2 status=0
  local box="infra1443-$name"
  local log="$evidence/$name"
  mkdir -p "$log"

  say "variant $name (--user $user)"
  docker rm -f "$box" > /dev/null 2>&1 || true

  # A HOME of this variant's own, as a GitHub `container:` job gets one (it mounts
  # _temp/_github_home at /github/home). Without it the root variant would leave root-owned
  # Go and bun caches in the runner's home for the uid-1001 variant to fail on, which would be
  # an artifact of running both on one machine rather than anything about the image.
  local home="$runner_home/infra-1443-home-$name"
  rm -rf "$home"
  mkdir -p "$home"
  chmod 0777 "$home"

  # `docker create` then `docker start`, as the runner's own `Initialize containers` does, and
  # each timed: that step is a pull, these two, and two `docker exec`s.
  local start stop
  start=$(date +%s.%N)
  # --shm-size: a container's /dev/shm is 64 MB by default, and that is what a GitHub
  # `container:` job gets too, where a job on the bare runner has the host's (about 8 GB). The
  # browsers' content processes pass their buffers through it. With the default, the one row
  # that uploads 20 MB under Firefox failed in runs 42338254048, 42339402235, 42340825955 and
  # 42342307345, the Dispatch server's PutObject to the file store timing out after 35 s, while
  # the same row under WebKit passed every time. INFRA_1443_SHM_SIZE is how this run says what
  # /dev/shm to give the container, so the two readings can be told apart.
  docker create \
    --name "$box" \
    --shm-size "${INFRA_1443_SHM_SIZE:-2g}" \
    --user "$user" \
    --workdir "$repo" \
    --volume "$runner_home:$runner_home" \
    --volume /opt/hostedtoolcache:/opt/hostedtoolcache \
    --env "HOME=$home" \
    --env "PATH=$bun_dir:$go_dir:/usr/lib/postgresql/18/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    --entrypoint tail \
    "$image" -f /dev/null > /dev/null
  stop=$(date +%s.%N)
  record "[$name] docker create: $(elapsed "$start" "$stop") s"
  start=$(date +%s.%N)
  docker start "$box" > /dev/null
  stop=$(date +%s.%N)
  record "[$name] docker start: $(elapsed "$start" "$stop") s"

  # `bash -c`, never `bash -lc`: Debian's /etc/profile assigns PATH outright, so a login shell
  # would throw away the PATH the `docker create` above gave the container and with it bun, go
  # and PostgreSQL 18's binaries. The container's own environment is what every exec reads.
  local in=(docker exec --workdir "$repo" "$box")

  say "[$name] who this container is and what the image holds"
  "${in[@]}" bash -c '
    set -u
    echo "id -u: $(id -u)"
    echo "id -g: $(id -g)"
    echo "id: $(id)"
    echo "whoami: $(whoami 2>&1 || true)"
    echo "HOME=$HOME"
    ls -ld "$HOME"
    ls -ld "$PWD"
    echo "PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-<unset>}"
    ls -ld /ms-playwright
    for tool in psql initdb pg_ctl pg_config python3 flock gh jq yq weed git openssl unzip tar curl node npm bun go nproc; do
      printf "%-10s %s\n" "$tool" "$(command -v "$tool" || echo MISSING)"
    done
    pg_config --version
    python3 --version
    node --version
    bun --version
    go version
    ls -ld /dev/shm; df -h /dev/shm | cat
    touch "$PWD/.infra-1443-write-probe" && echo "the workspace is writable" && rm -f "$PWD/.infra-1443-write-probe" || echo "the workspace is NOT writable"
    echo "--- where a Go cache restore would land in a container: ---"
    echo "the image passwd HOME for uid 0:    $(getent passwd 0 | cut -d: -f6)"
    echo "the image passwd HOME for uid 1001: $(getent passwd 1001 | cut -d: -f6 || echo "<no passwd entry>")"
    echo "a GitHub container: job would set HOME=/github/home whatever those say"
    go env GOPATH GOMODCACHE GOCACHE | tr "\n" " "; echo
    echo "bun install cache: ${BUN_INSTALL_CACHE_DIR:-$HOME/.bun/install/cache}"
    ls -ld "$HOME/go" "$HOME/.cache/go-build" 2>/dev/null || echo "neither Go cache directory exists yet under HOME=$HOME"
    echo "--- the font set, which decides every text measurement a layout row makes: ---"
    echo "font files: $(find /usr/share/fonts /usr/local/share/fonts -type f \( -name "*.ttf" -o -name "*.ttc" -o -name "*.otf" -o -name "*.pfb" \) 2>/dev/null | wc -l)"
    echo "families: $(fc-list : family 2>/dev/null | sed "s/,.*//" | sort -u | tr "\n" "|")"
    echo "fc-match sans-serif: $(fc-match sans-serif 2>/dev/null || echo "fc-match MISSING")"
  ' 2>&1 | tee "$log/facts.log" || true

  # `go build` stamps the binary with the checkout's VCS state, and git refuses to read a
  # repository another uid owns ("dubious ownership"), which ends the build with
  # `error obtaining VCS status: exit status 128`. One checkout serves both variants here, so
  # the uid in the container is not always its owner. Under `container:` the checkout is the
  # container user's own and this line is not needed; it is this machine's, not the design's.
  "${in[@]}" git config --global --add safe.directory "$repo" || true

  say "[$name] the DISPATCH_E2E_PG_BIN provider: one initdb, four lane copies"
  "${in[@]}" bash -c '
    set -eu
    bin=/usr/lib/postgresql/18/bin
    work="${TMPDIR:-/tmp}/infra-1443-pg"
    rm -rf "$work"; mkdir -p "$work/template"
    start=$(date +%s.%N)
    "$bin/initdb" -D "$work/template/pgdata" -U postgres --auth=trust --no-sync -E UTF8
    after_initdb=$(date +%s.%N)
    "$bin/pg_ctl" -D "$work/template/pgdata" -l "$work/template/postgres.log" \
      -o "-p 55432 -h 127.0.0.1 -k $work/template" -w start
    "$bin/psql" "postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable" -v ON_ERROR_STOP=1 -c "create database dispatch_e2e"
    "$bin/psql" "postgres://postgres@127.0.0.1:55432/dispatch_e2e?sslmode=disable" -v ON_ERROR_STOP=1 -c "create extension vector"
    "$bin/pg_ctl" -D "$work/template/pgdata" -m fast -w stop
    after_template=$(date +%s.%N)
    for lane in 0 1 2 3; do mkdir -p "$work/lane-$lane"; cp -a "$work/template/pgdata" "$work/lane-$lane/pgdata"; done
    done_at=$(date +%s.%N)
    awk -v a="$start" -v b="$after_initdb" -v c="$after_template" -v d="$done_at" '"'"'BEGIN {
      printf "initdb:               %.2f s\n", b - a
      printf "template db + vector: %.2f s\n", c - b
      printf "four lane copies:     %.2f s\n", d - c
      printf "whole provider:       %.2f s\n", d - a
    }'"'"'
    du -sh "$work/template/pgdata"
    rm -rf "$work"
  ' 2>&1 | tee "$log/pg-provider.log" || record "[$name] the Postgres provider failed; its last lines are in $name/pg-provider.log"

  # ci.yaml's dispatch job builds three things before its browser step, and all three are
  # needed: contracts' dist/, which Playwright's own Node resolution reads the values the specs
  # import from; the dashboard's web/dist, which run-server.sh serves
  # (`web_dist="${DISPATCH_E2E_SERVER_WEB_DIST:-../dispatch/web/dist}"`); and the Dispatch
  # server itself, which every lane's run-server.sh then finds current. Leaving the dashboard
  # out serves every row an empty page, and all 80 of them fail waiting 15 s for a status
  # element that was never rendered -- which is what the first run of this script measured.
  say "[$name] build the contracts, the dashboard and the Dispatch server"
  "${in[@]}" bash -c 'cd packages/contracts && bun run build' 2>&1 | tail -20 | tee "$log/contracts.log" || status=1
  "${in[@]}" bash -c 'cd packages/dispatch && bun run build:web && ls -l web/dist/THIRD_PARTY_NOTICES.txt' 2>&1 | tail -25 | tee "$log/dashboard.log" || status=1
  "${in[@]}" bash -c 'cd packages/envoy && go build -o ./dispatch-e2e ./cmd/dispatch' 2>&1 | tail -20 | tee "$log/server.log" || status=1

  if [ "$status" -ne 0 ]; then
    record "[$name] the builds failed, so the suite did not run"
    docker rm -f "$box" > /dev/null 2>&1 || true
    return 1
  fi

  # `-dir` on a bind mount, never on the container's own writable layer. ci.yaml's
  # `docker run ... seaweedfs server -dir=/data` writes into the anonymous volume the
  # seaweedfs image declares for /data, so its object writes go straight to the host
  # filesystem; a `-dir` under the container's /tmp goes through overlayfs instead, and the one
  # row of the suite that uploads 20 MB timed out there twice (runs 42338254048 and
  # 42339402235, `PutObject ... context deadline exceeded` after 35 s). $HOME here is a
  # directory of the host's, mounted in, which is what a `container:` job's $RUNNER_TEMP is too.
  say "[$name] start the baked weed on 127.0.0.1:18333 and wait for it"
  "${in[@]}" bash -c '
    set -eu
    store="$HOME/weed"
    rm -rf "$store"; mkdir -p "$store"
    df -h "$store" | cat
    nohup weed server -s3 -dir="$store" -ip=127.0.0.1 -s3.port=18333 > "$HOME/weed.log" 2>&1 &
    echo "weed pid $!"
    for attempt in $(seq 1 60); do
      curl -sf -o /dev/null http://127.0.0.1:18333/ && break
      [ "$attempt" -lt 60 ] || { cat "$HOME/weed.log"; exit 1; }
      sleep 1
    done
    curl -sf -X PUT -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files
    echo "the baked weed answers on 127.0.0.1:18333 with its -dir on a bind mount"
  ' 2>&1 | tee "$log/weed.log" || { record "[$name] the baked weed did not come up"; docker rm -f "$box" > /dev/null 2>&1 || true; return 1; }

  say "[$name] the Dispatch WebKit and Firefox suite, four lanes"
  start=$(date +%s.%N)
  "${in[@]}" bash -c '
    set -u
    cd packages/dispatch
    export TMPDIR="${TMPDIR:-/tmp}"
    export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
    export DISPATCH_E2E_WORKERS=4
    export DISPATCH_E2E_FILE_STORE_ENDPOINT=http://127.0.0.1:18333
    export DISPATCH_E2E_FILE_STORE_BUCKET=dispatch-e2e-files
    export PLAYWRIGHT_JSON_OUTPUT_NAME="$TMPDIR/report.json"
    bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts \
      --project webkit --project webkit-iphone \
      --project firefox --project firefox-download-navigation \
      --reporter=list,json
  ' 2>&1 | tee "$log/suite.log" || status=$?
  stop=$(date +%s.%N)
  record "[$name] the browser suite: $(elapsed "$start" "$stop") s, exit $status"

  say "[$name] what the run left behind"
  "${in[@]}" bash -c '
    tmp="${TMPDIR:-/tmp}"
    if [ -f "$tmp/report.json" ]; then
      jq "{expected: .stats.expected, unexpected: .stats.unexpected, flaky: .stats.flaky, skipped: .stats.skipped, duration: .stats.duration}" "$tmp/report.json"
      jq -r "[.suites[]?|..|objects|select(has(\"projectName\"))|.projectName] | unique | join(\", \")" "$tmp/report.json" 2>/dev/null || true
    else
      echo "no report.json"
    fi
    for f in "$tmp"/dispatch-e2e-run-*/lane-*/log/*.log; do [ -f "$f" ] || continue; echo "--- $f"; tail -15 "$f"; done
  ' 2>&1 | tee "$log/aftermath.log" || true
  docker cp "$box:${TMPDIR:-/tmp}/report.json" "$log/report.json" > /dev/null 2>&1 || true

  docker rm -f "$box" > /dev/null 2>&1 || true
  return "$status"
}

# uid 1001 first, then root. Both share one checkout, and every build here writes into it
# (contracts' dist/, packages/envoy/dispatch-e2e, e2e/test-results); the checkout is uid 1001's,
# so root can overwrite what 1001 wrote and 1001 could not overwrite what root wrote. The order
# is this machine's, not the design's: under `container:` each variant is a job with a checkout
# of its own.
uid_status=0
variant uid-1001 1001 || uid_status=$?
root_status=0
variant root 0 || root_status=$?

say "summary"
record "variant root (--user 0):    exit $root_status"
record "variant uid-1001:           exit $uid_status"
cat "$measurements"

docker buildx rm infra1443 > /dev/null 2>&1 || true

# The spike's own verdict: one of the two variants must run the suite green. The root variant is
# expected to fail at `initdb`, which refuses to run as root, and the uid-1001 variant is the one
# that must pass.
if [ "$uid_status" -ne 0 ]; then
  echo "::error::the uid-1001 variant did not run the Dispatch WebKit and Firefox suite green (exit $uid_status)"
  exit 1
fi
echo "the uid-1001 variant ran the Dispatch WebKit and Firefox suite green"
