#!/usr/bin/env bash
# Tests for `check-download-retry.sh`: every network fetch of this repository's CI retries at
# R1's floor, an `apt-get update` fails rather than warns when an index read fails so that the
# retry has something to retry, a `mise x` carries no retry at all because it exits 0 when it
# cannot resolve the tool, one rule holds that retry, and the four exceptions R2 lists are the
# only sites left bare (dispatch://INFRA-1545, R2).
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs. Every fixture starts from the
# same baseline: the four exceptions, each at the file, job and step name the check's exception
# list holds, plus the three Dockerfiles it reads. A case that proves a refusal adds a workflow,
# an action or a Dockerfile of its own beside that baseline, so a refusal is the case's own and
# not a hole in the baseline, and the baseline's own case proves it passes.
#
# Run from anywhere: .github/scripts/check-download-retry.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-download-retry.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/check-download-retry.XXXXXX")
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

PIN=fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09
DIGEST=sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f

# The canonical inline retry every network RUN of the three Dockerfiles opens with. A fixture
# that means to pass writes this text; a fixture that means to be refused writes another.
read -r -d '' CANONICAL <<'TEXT' || true
RUN retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; \
  [ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; \
  sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; } \
TEXT

# fixture <name>: a tree holding the baseline below, with the script linked in. Echoes its root.
#
# The baseline is the four exceptions of R2, which every fixture carries for two reasons: the
# check refuses an exception that matches no fetch, so a tree without them fails for a reason no
# case is about; and a case that then adds one bad site proves that site alone.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" \
    "$root/.github/actions/pull-test-images" \
    "$root/packages/daemon/docker" "$root/packages/envoy/docker" \
    "$root/packages/envoy/infra/host/heldportwrapper"
  ln -s "$check_script" "$root/.github/scripts/check-download-retry.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  for dockerfile in \
    "$root/packages/daemon/docker/worker.Dockerfile" \
    "$root/packages/envoy/docker/Dockerfile" \
    "$root/packages/envoy/infra/host/heldportwrapper/Dockerfile"; do
    printf 'FROM scratch\nCOPY . /src\n' > "$dockerfile"
  done

  # Exception (a), first site: the ghcr.io pull of a package this repository publishes, read from
  # a Go const and pulled in a loop.
  cat > "$root/.github/actions/pull-test-images/action.yml" <<'YAML'
name: Pull the images tests start
description: Pulls each image a job's tests start, from the package this repository publishes.
runs:
  using: composite
  steps:
    - name: Pull the images tests start
      shell: bash
      run: |
        for image in $IMAGES; do
          docker pull "$image"
        done
YAML

  # Exceptions (a) again, (b) and (c).
  cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: [push]
jobs:
  envoy-client:
    runs-on: ubuntu-24.04
    steps:
      - name: Pre-pull the NATS image the credential tests run
        run: docker pull ghcr.io/example/legion/ci@$DIGEST
  download-retry-images:
    runs-on: ubuntu-24.04
    steps:
      - name: Pull every base image the three builds read
        run: |
          for image in \$images; do docker pull "\$image"; done
  dispatch:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@$PIN
      - name: Install Playwright Chromium, WebKit and Firefox
        run: |
          attempts() {
            local left=\$1 what=\$2 status
            shift 2
            while :; do
              "\$@" && return 0
              status=\$?
              left=\$((left - 1))
              [ "\$left" -gt 0 ] || return 1
            done
          }
          attempts 2 "apt-get update" sudo timeout 90 apt-get update
          attempts 2 "the package download" sudo timeout 300 apt-get install -y -d \$packages
          sudo apt-get install -y \$packages
          . .github/scripts/ci-retry.sh
          retry_fetch "Playwright's Chromium" 300 -- bunx playwright install chromium
YAML

  # Exception (d): a credentialed publish job, whose pinned command set no wrapper may change.
  cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install dependencies
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the workspace's dependencies" 60 -- bun install --frozen-lockfile
  publish:
    runs-on: ubuntu-24.04
    permissions:
      contents: write
    steps:
      - name: Determine latest
        run: gh api "repos/$GITHUB_REPOSITORY/releases/latest"
YAML
  echo "$root"
}

run_check() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && "./.github/scripts/check-download-retry.sh" "$@" 2>&1)
  status=$?
  set -e
}

list_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-download-retry.sh" --list 2>/dev/null)
  status=$?
  set -e
}

echo "case: a tree holding each of R2's four exceptions passes"
root=$(fixture exceptions)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it read" "$(contains "$out" 'every network fetch in CI retries at the floor')"
check "counts the sites the exceptions name" "$(contains "$out" '6 site(s) the exceptions name')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a workflow with a bare curl of a release asset"
root=$(fixture bare-curl)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: |
          curl -fsSL https://github.com/miniscruff/changie/releases/download/v1.21.1/changie.tar.gz \
            | tar -xz -C /usr/local/bin changie
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the step" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=8::.curl. in step .Install changie. fetches without the floor')"
check "names each option it lacks" "$(contains "$out" 'it passes none')"
check "names --retry-all-errors" "$(contains "$out" 'retry-all-errors')"
check "names --max-time" "$(contains "$out" 'max-time')"
check "counts the refusal" "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a workflow whose curl carries a retry below the floor"
root=$(fixture short-retry)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install crane
        run: |
          curl -fsSL --retry 3 --retry-all-errors --max-time 30 -o crane.tar.gz \
            https://github.com/google/go-containerregistry/releases/download/v0.21.1/crane.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the count it passes" "$(contains "$out" 'it passes .--retry 3.')"
check "names the floor" "$(contains "$out" 'only 7 reaches 127 s')"

echo "case: a curl that retries but bounds no attempt"
root=$(fixture unbounded-curl)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -fsSL --retry 7 --retry-all-errors -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing bound" "$(contains "$out" 'which curl resets for each attempt')"

echo "case: a curl that retries and never fails on a persistent 504"
root=$(fixture no-fail)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install uv
        run: curl -sSL --retry 7 --retry-all-errors --max-time 30 -o uv.tar.gz https://example.com/uv.tar.gz
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names --fail and what it costs" "$(contains "$out" 'writes the error body to the output')"

echo "case: a curl that reads the status itself answers for --fail"
root=$(fixture status-read)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the release
        run: |
          code=$(curl -sSL --retry 7 --retry-all-errors --max-time 30 -o out.json -w '%{http_code}' https://example.com/r)
          [ "$code" = 200 ]
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a composite action holding a second copy of the retry helper"
root=$(fixture copied-helper)
mkdir -p "$root/.github/actions/install-thing"
cat > "$root/.github/actions/install-thing/action.yml" <<'YAML'
name: Install the thing
description: Installs the thing, with a retry loop of its own.
runs:
  using: composite
  steps:
    - name: Install the thing
      shell: bash
      run: |
        attempt=1
        while [ "$attempt" -le 5 ]; do
          curl -fsSL --retry 7 --retry-all-errors --max-time 30 -o thing.tar.gz https://example.com/thing && break
          sleep $((10 * attempt))
          attempt=$((attempt + 1))
        done
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the loop as a copy" \
  "$(contains "$out" 'file=.github/actions/install-thing/action.yml,line=9::this loop retries .curl. itself, which makes it a second copy')"
check "names the one rule" "$(contains "$out" 'One rule holds the attempts')"

echo "case: a loop that hands a fetch to a retry function of its own is a copy too"
root=$(fixture copied-helper-indirect)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the packages
        run: |
          attempts() {
            local left=$1 what=$2 status
            shift 2
            while :; do
              "$@" && return 0
              status=$?
              left=$((left - 1))
              [ "$left" -gt 0 ] || return 1
            done
          }
          attempts 2 "apt-get update" sudo timeout 90 apt-get update
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the loop the call reaches" \
  "$(contains "$out" 'file=.github/workflows/case.yaml,line=11::this loop retries .apt-get update. itself')"

echo "case: a loop whose fetch is a retry_fetch call is no copy"
root=$(fixture loop-with-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Copy each row
        run: |
          . .github/scripts/ci-retry.sh
          while IFS= read -r row; do
            retry_fetch "the copy of $row" 300 -- crane copy "$row" "ghcr.io/example/ci:$row"
          done <<<"$rows"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a retry_fetch call written without a per-attempt bound"
root=$(fixture unbounded-helper)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the workspace's dependencies
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the workspace's dependencies" -- bun install --frozen-lockfile
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing bound" \
  "$(contains "$out" 'is retried and no attempt of it is bounded in time. Give .retry_fetch. a per-attempt bound')"

echo "case: a Dockerfile with an unretried apt-get"
root=$(fixture bare-apt)
cat > "$root/packages/envoy/docker/Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/envoy/docker/Dockerfile,line=2::.apt-get update. fetches inside a Dockerfile RUN with no retry')"
check "names the inline loop to use" "$(contains "$out" 'the canonical inline loop')"

echo "case: a Dockerfile RUN that opens with the canonical retry and wraps each fetch passes"
root=$(fixture canonical-retry)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update -o APT::Update::Error-Mode=any \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a Dockerfile's retried apt-get update that keeps apt's warning on a failed index"
root=$(fixture dockerfile-apt-error-mode)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 120 "apt-get update" apt-get update \
    && retry 300 "apt-get install" apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
DOCKER
} > "$root/packages/envoy/docker/Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the option" \
  "$(contains "$out" 'file=packages/envoy/docker/Dockerfile,line=5::.apt-get update. passes no .-o APT::Update::Error-Mode=any.')"
check "says what goes wrong, not only what is missing" \
  "$(contains "$out" 'the retry around it stops after one attempt having read no list')"
check "names the install the unread list breaks" \
  "$(contains "$out" 'has no installation candidate')"

echo "case: a Dockerfile RUN with a retry loop of its own text"
root=$(fixture other-retry)
cat > "$root/packages/envoy/docker/Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN retry() { for i in 1 2 3; do "$@" && return 0; sleep 5; done; return 1; } \
    && retry apt-get update \
    && rm -rf /var/lib/apt/lists/*
DOCKER
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the canonical text as the one definition" \
  "$(contains "$out" 'this RUN defines a retry loop of its own text')"

echo "case: a Dockerfile RUN whose one fetch is a curl carrying curl's own options passes"
root=$(fixture dockerfile-curl)
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM debian:trixie-slim AS build
RUN set -eu; \
    curl -fsSL --retry 7 --retry-all-errors --max-time 120 -o /tmp/mise.tar.gz \
      "https://example.com/mise.tar.gz"; \
    tar -xzf /tmp/mise.tar.gz -C /tmp
DOCKER
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow that wraps a mise x in a retry"
root=$(fixture retried-mise-x)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the worker image's jj
        run: |
          . .github/scripts/ci-retry.sh
          jj_path=$(retry_fetch "the worker image's jj" 60 -- mise x "$jj_tool" -- sh -c 'command -v jj')
          install -m 0755 "$jj_path" "$HOME/.local/bin/jj"
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the step and the verb" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=9::.mise x. in step .Install the worker image.s jj. is retried, and that retry protects nothing")"
check "says what goes wrong, not only what to write instead" \
  "$(contains "$out" 'runs whatever binary of that name is already on PATH')"
check "names the cure" "$(contains "$out" 'The cure is .mise install <tool>. under the retry')"
check "names the resolution that follows it" \
  "$(contains "$out" 'with the resolution after it')"

echo "case: a Dockerfile RUN that wraps a mise x in the canonical retry"
root=$(fixture retried-mise-x-dockerfile)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && install -m 0755 "$(retry 300 "jj download" mise x "$JJ_TOOL" -- sh -c 'command -v jj')" /opt/tools/jj
DOCKER
} > "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile, the line and the verb" \
  "$(contains "$out" 'file=packages/daemon/docker/worker.Dockerfile,line=5::.mise x. is retried, and that retry protects nothing')"
check "names the cure" "$(contains "$out" 'The cure is .mise install <tool>. under the retry')"
check "counts the one refusal" \
  "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a workflow's retried mise install with the resolution after it passes"
root=$(fixture mise-install-cured)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the worker image's jj
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "the worker image's jj" 60 -- mise install "$jj_tool"
          jj_path=$(mise x "$jj_tool" -- sh -c 'command -v jj')
          install -m 0755 "$jj_path" "$HOME/.local/bin/jj"
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is on the list" "$(contains "$out" 'case.yaml	9	mise install	')"
check "the resolution after it is on no list" \
  "$(is "$(contains "$out" 'mise x')" false)"

echo "case: a Dockerfile's retried mise install with the resolution after it passes"
root=$(fixture mise-install-cured-dockerfile)
{
  echo 'FROM debian:trixie-slim AS build'
  printf '%s\n' "$CANONICAL"
  cat <<'DOCKER'
    && retry 300 "jj download" mise install "$JJ_TOOL" \
    && install -m 0755 "$(mise x "$JJ_TOOL" -- sh -c 'command -v jj')" /opt/tools/jj
DOCKER
} > "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the install is on the list" \
  "$(contains "$out" '^packages/daemon/docker/worker.Dockerfile	5	mise install	-$')"
check "the resolution after it is on no list" \
  "$(is "$(contains "$out" 'mise x')" false)"

echo "case: a readiness poll of the loopback is no download"
root=$(fixture loopback)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Wait for the S3-compatible server
        run: |
          for attempt in $(seq 1 60); do
            curl -sf -o /dev/null http://127.0.0.1:18333/ && break
            sleep 1
          done
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
list_check "$root"
check "the poll is on no list" "$(is "$(contains "$out" 'case.yaml')" false)"

echo "case: a probe whose failure is the answer is no download"
root=$(fixture probe)
cat > "$root/.github/scripts/mirror.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
have=$(crane digest "ghcr.io/example/ci:$tag" 2>/dev/null || true)
[ "$have" = "$digest" ] || echo copy
SH
chmod +x "$root/.github/scripts/mirror.sh"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: the same read with its failure a fault is a download"
root=$(fixture probe-not)
cat > "$root/.github/scripts/mirror.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
have=$(crane digest "ghcr.io/example/ci:$tag")
echo "$have"
SH
chmod +x "$root/.github/scripts/mirror.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'file=.github/scripts/mirror.sh,line=3::.crane digest. fetches with no retry')"

echo "case: a Python read that goes straight to urllib"
root=$(fixture python-read)
cat > "$root/.github/scripts/read.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
python3 <<'PY'
import urllib.request

with urllib.request.urlopen("https://ghcr.io/token", timeout=30) as answer:
    print(answer.read())
PY
SH
chmod +x "$root/.github/scripts/read.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the one reader" \
  "$(contains "$out" 'file=.github/scripts/read.sh,line=6::this read goes straight to urllib.request')"
check "names the floor it carries" "$(contains "$out" 'waits of 10, 20, 40 and 80')"

echo "case: the same read inside another file's read_retrying is still refused"
root=$(fixture python-copy)
cat > "$root/.github/scripts/read.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
python3 <<'PY'
import urllib.request


def read_retrying(request):
    for wait in (10, 20, 40, 80, None):
        try:
            return urllib.request.urlopen(request, timeout=30)
        except Exception:
            if wait is None:
                raise
PY
SH
chmod +x "$root/.github/scripts/read.sh"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the copy" "$(contains "$out" 'this read goes straight to urllib.request')"

echo "case: an apt unpack after a retried download takes no network"
root=$(fixture apt-unpack)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install this proof's packages
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "apt's package lists" 95 -- \
            sudo timeout 90 apt-get update -o APT::Update::Error-Mode=any
          retry_fetch "the proof's apt packages" 125 -- \
            sudo timeout 120 apt-get install -y --no-install-recommends -d $PACKAGES
          sudo apt-get install -y --no-install-recommends $PACKAGES
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a workflow's retried apt-get update that keeps apt's warning on a failed index"
root=$(fixture apt-error-mode)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install the packages
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "apt's package lists" 95 -- sudo timeout 90 apt-get update
          retry_fetch "the packages" 125 -- \
            sudo timeout 120 apt-get install -y --no-install-recommends -d $PACKAGES
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the step and the option" \
  "$(contains "$out" "file=.github/workflows/case.yaml,line=9::.apt-get update. in step .Install the packages. passes no .-o APT::Update::Error-Mode=any.")"
check "says what goes wrong, not only what is missing" \
  "$(contains "$out" 'the retry around it stops after one attempt having read no list')"
check "the retried install beside it is no problem of this rule" \
  "$(contains "$out" '1 download(s) outside the one retry rule')"

echo "case: a command that only answers about itself is no fetch"
root=$(fixture versions)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the toolchain
        run: curl --version; wget --version; bunx biome check .
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: --list prints every site with its step name"
root=$(fixture listing)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Download the Go modules this job builds
        run: |
          . .github/scripts/ci-retry.sh
          retry_fetch "packages/envoy's Go modules" 300 -- go -C packages/envoy mod download
YAML
cat > "$root/.github/scripts/report.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
. .github/scripts/ci-retry.sh
retry_fetch "the run's artifact list" 60 -- sh -c 'gh api "$1" > "$2"' gh-api "$route" "$out"
SH
chmod +x "$root/.github/scripts/report.sh"
list_check "$root"
check "exits 0" "$(is "$status" 0)"
check "a workflow site carries its step name" \
  "$(contains "$out" '^.github/workflows/case.yaml	9	go mod download	Download the Go modules this job builds$')"
check "a script site carries a dash for the step" \
  "$(contains "$out" '^.github/scripts/report.sh	4	gh api	-$')"
check "the baseline's own retried site is on it" \
  "$(contains "$out" 'playwright install	Install Playwright Chromium, WebKit and Firefox$')"
check "an excepted site is not on it" "$(is "$(contains "$out" 'docker pull')" false)"
check "the count goes to standard error, not into the rows" \
  "$(is "$(contains "$out" 'site\(s\)')" false)"

echo "case: --list over a tree with a problem refuses instead of listing"
root=$(fixture listing-refused)
cat > "$root/.github/workflows/case.yaml" <<'YAML'
name: Case
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Install changie
        run: curl -fsSL -o changie.tar.gz https://example.com/changie.tar.gz
YAML
run_check "$root" --list
check "exits 1" "$(is "$status" 1)"
check "names the problem" "$(contains "$out" 'fetches without the floor')"

echo "case: an exception that matches no fetch fails"
root=$(fixture stale-exception)
rm "$root/.github/actions/pull-test-images/action.yml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the exception and what it no longer covers" \
  "$(contains "$out" 'the exception for .github/actions/pull-test-images/action.yml matches no fetch')"

echo "case: one of the three Dockerfiles missing fails"
root=$(fixture missing-dockerfile)
rm "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/daemon/docker/worker.Dockerfile is one of the three Dockerfiles')"

echo "case: a workflow that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: Case\non: [push\n' > "$root/.github/workflows/case.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'this file is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: an argument this check does not take"
root=$(fixture bad-argument)
run_check "$root" --everything
check "exits 2" "$(is "$status" 2)"
check "says what it takes" "$(contains "$out" 'this check takes no argument')"

summary "check-download-retry.sh"
