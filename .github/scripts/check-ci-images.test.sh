#!/usr/bin/env bash
# Tests for `check-ci-images.sh`: one red case per rule it enforces, one per ground of its
# exception list, and the repository's own tree.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs, not a copy that can drift: it
# resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at
# the fixture root. A fixture holds every file CI's reach names, because the check fails on a
# reach file that is gone.
#
# NO CASE REACHES THE NETWORK. The check reads which rows the package holds through
# CI_IMAGES_MANIFEST_HEAD: when that variable is set, it runs the command it names once per row
# and reads the HTTP status code the command prints. Every case here sets it to `head.sh` below,
# which answers 200 for every digest but the ones MISSING names, and 404 for those. So the rule
# that reads ghcr.io is driven by a case, and nothing is pulled.
#
# Run from anywhere: .github/scripts/check-ci-images.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-ci-images.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The digests the fixtures use. Made up: nothing pulls them, and what each case reads is the row
# they match.
NATS=sha256:0a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f6071
BUN=sha256:1b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f607182
BUN_SLIM=sha256:2c3d4e5f607182932c3d4e5f607182932c3d4e5f607182932c3d4e5f60718293
GOLANG=sha256:3d4e5f60718293a43d4e5f60718293a43d4e5f60718293a43d4e5f60718293a4
UNKNOWN=sha256:9f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a3021
REGISTRY=ghcr.io/sjawhar/legion/ci

# The stand-in for the manifest HEAD: the package holds every digest but the ones MISSING names.
head_script="$work/head.sh"
cat > "$head_script" <<'SH'
#!/usr/bin/env bash
case " ${MISSING:-} " in
  *" $1 "*) echo 404 ;;
  *) echo 200 ;;
esac
SH
chmod +x "$head_script"

# fixture <name>: a tree that passes — the rows, the pins, and every file of CI's reach, each
# naming a digest the rows hold. Echoes the fixture root. Each case names its own tree: two
# cases sharing a name would share a tree, so a repeated name is refused here rather than
# surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" \
    "$root/.github/actions/pull-test-images" \
    "$root/packages/envoy/docker" "$root/packages/envoy/infra/host/heldportwrapper" \
    "$root/packages/daemon/docker" "$root/packages/daemon/internal/testnats" \
    "$root/packages/envoy/internal/testnats" "$root/packages/envoy/internal/testpg" \
    "$root/packages/envoy/internal/tests3" "$root/packages/envoy-client/src/__tests__" \
    "$root/scripts/e2e/lib" "$root/packages/pi-legion/scripts/skill-scenarios"
  ln -s "$check_script" "$root/.github/scripts/check-ci-images.sh"
  echo "1.3.14" > "$root/.bun-version"
  cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14
    digest: $BUN
  - upstream: docker.io/oven/bun:1.3.14-slim
    tag: bun-1.3.14-slim
    digest: $BUN_SLIM
  - upstream: docker.io/library/golang:1.25.0-alpine
    tag: golang-1.25.0-alpine
    digest: $GOLANG
YAML
  cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: [push]
jobs:
  test:
    runs-on: ubuntu-24.04
    services:
      nats:
        image: $REGISTRY@$NATS
    steps:
      - uses: ./.github/actions/pull-test-images
      - run: docker pull $REGISTRY@$NATS
      # The three shapes IMAGE_FROM covers in the real tree, so this fixture holds its entries
      # to a command each, as the check demands.
      - run: |
          image=\$(sed -n 's/^const Image = "\\(.*\\)"\$/\\1/p' packages/envoy/internal/tests3/seaweedfs.go)
          docker pull "\$image"
      - run: docker run --rm "\$ENVOY_SMOKE_IMAGE" ls
YAML
  cat > "$root/.github/actions/pull-test-images/action.yml" <<'YAML'
name: Pull the images tests start
description: Pull the images the tests start.
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        image=$(sed -n "s/^const Image = \"\(.*\)\"$/\1/p" "$file")
        docker pull "$image"
YAML
  cat > "$root/packages/envoy/docker/Dockerfile" <<YAML
FROM $REGISTRY@$GOLANG AS build
RUN go build ./...
FROM build
YAML
  cat > "$root/packages/envoy/infra/host/heldportwrapper/Dockerfile" <<'YAML'
ARG BASE
FROM ${BASE} AS base
YAML
  cat > "$root/packages/daemon/docker/worker.Dockerfile" <<YAML
ARG GO_VERSION=1.25.0
FROM $REGISTRY@$BUN_SLIM AS plugin
RUN bun install
YAML
  local go_file
  for go_file in packages/daemon/internal/testnats/nats.go \
    packages/envoy/internal/testnats/nats.go packages/envoy/internal/testpg/testpg.go \
    packages/envoy/internal/tests3/seaweedfs.go; do
    cat > "$root/$go_file" <<YAML
package testimage

const Image = "$REGISTRY@$NATS"
YAML
  done
  cat > "$root/packages/envoy-client/src/__tests__/nats-server.ts" <<YAML
const NATS_IMAGE = "$REGISTRY@$NATS";
export { NATS_IMAGE };
YAML
  cat > "$root/scripts/e2e/lib/nats-stream.test.ts" <<YAML
docker("run", "--rm", "$REGISTRY@$NATS", "-js");
YAML
  cat > "$root/packages/pi-legion/scripts/skill-scenarios/rig.sh" <<YAML
#!/usr/bin/env bash
judge_image=$REGISTRY@$NATS
docker run --rm --init "\$judge_image" bash -c 'echo judged'
YAML
  cat > "$root/scripts/e2e/stage1-skeleton.sh" <<YAML
#!/usr/bin/env bash
docker run -d --name pg --cpus 2 --memory 2g \\
  -e POSTGRES_DB=legion $REGISTRY@$NATS >/dev/null
YAML
  echo "$root"
}

# run_check <root>: runs the script under test against that tree, setting `out` to its combined
# output and `status` to its exit code. Not a subshell, so both survive the call. MISSING, which
# the caller may set, names the digests the stand-in package does not hold.
run_check() {
  local root=$1
  set +e
  out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" MISSING="${MISSING:-}" \
    "$root/.github/scripts/check-ci-images.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree whose every image is a row of .github/ci-images.yaml"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it read" "$(contains "$out" 'holds 4 row(s), 4 of them in the package')"
check "counts the reach" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"

echo "case: rule 1 — a row this check cannot read"
root=$(fixture row-no-tag)
sed -i '/tag: nats-2.10/d' "$root/.github/ci-images.yaml"
run_check "$root"
check "a row with no tag fails" "$(is "$status" 1)"
check "says why a tag is needed" "$(contains "$out" 'a package version nothing names')"
check "names the row" "$(contains "$out" 'the row for docker.io/library/nats:2.10 has no tag')"

root=$(fixture row-empty-tag)
sed -i 's/tag: nats-2.10/tag: ""/' "$root/.github/ci-images.yaml"
run_check "$root"
check "an empty tag fails too" "$(is "$status" 1)"
check "names the row" "$(contains "$out" 'has no tag')"

root=$(fixture row-bad-digest)
sed -i "s|digest: $NATS|digest: sha256:notadigest|" "$root/.github/ci-images.yaml"
run_check "$root"
check "a digest that is not 64 hex characters fails" "$(is "$status" 1)"
check "says what to run" "$(contains "$out" 'crane digest docker.io/library/nats:2.10')"

echo "case: rule 2 — two rows naming one digest, or one tag"
root=$(fixture duplicate-digest)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.11
    digest: $NATS
YAML
run_check "$root"
check "a second row on one digest fails" "$(is "$status" 1)"
check "says why one digest is one row" "$(contains "$out" 'one digest names one row')"

root=$(fixture duplicate-tag)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.10
    digest: $UNKNOWN
YAML
run_check "$root"
check "a second row on one tag fails" "$(is "$status" 1)"
check "says the copy would move the tag" "$(contains "$out" 'would move the tag off the first')"

echo "case: rule 3 — a row that disagrees with a version pinned elsewhere"
root=$(fixture bun-drift)
sed -i 's|docker.io/oven/bun:1.3.14-slim|docker.io/oven/bun:1.4.2-slim|' \
  "$root/.github/ci-images.yaml"
run_check "$root"
check "a bun row that is not the pinned release fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" '.bun-version locks 1.3.14')"
check "and the row that is now missing" "$(contains "$out" 'no row copies oven/bun:1.3.14-slim')"

root=$(fixture golang-drift)
sed -i 's|ARG GO_VERSION=1.25.0|ARG GO_VERSION=1.26.3|' \
  "$root/packages/daemon/docker/worker.Dockerfile"
run_check "$root"
check "a golang row that is not the pinned release fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" 'pins ARG GO_VERSION=1.26.3')"
check "and what to write" "$(contains "$out" 'golang:1.26.3-alpine')"

echo "case: rule 4 — a reference on another registry"
root=$(fixture other-registry)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<YAML
package testpg

const Image = "docker.io/pgvector/pgvector@$NATS"
YAML
run_check "$root"
check "a docker.io reference fails" "$(is "$status" 1)"
check "names the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and the one package" "$(contains "$out" "a copy in $REGISTRY")"

echo "case: rule 5 — a reference that is not pinned by digest"
root=$(fixture tag-not-digest)
sed -i "s|image: $REGISTRY@$NATS|image: $REGISTRY:nats-2.10|" "$root/.github/workflows/ci.yaml"
run_check "$root"
check "a tagged reference fails" "$(is "$status" 1)"
check "says a tag can move" "$(contains "$out" 'a name the registry can move')"
check "names the service and its line" "$(contains "$out" 'ci.yaml:8: the nats service')"

echo "case: rule 6 — a reference whose digest no row holds"
root=$(fixture digest-unknown)
sed -i "s|$NATS|$UNKNOWN|" "$root/packages/envoy-client/src/__tests__/nats-server.ts"
run_check "$root"
check "a digest outside the file fails" "$(is "$status" 1)"
check "names the file to add it to" "$(contains "$out" 'names a digest no row of .github/ci-images.yaml holds')"
check "and the line to edit" "$(contains "$out" 'nats-server.ts:1:')"

echo "case: rule 7 — a row the package does not hold"
root=$(fixture not-published)
MISSING=$GOLANG run_check "$root"
check "a row ghcr.io answers 404 for fails" "$(is "$status" 1)"
check "names the row and the status" "$(contains "$out" 'is not in the package: a manifest HEAD answered 404')"
check "and says what publishes it" "$(contains "$out" 'mirror-images.yaml publishes a row')"

root=$(fixture published)
MISSING=$UNKNOWN run_check "$root"
check "a 404 on a digest no row holds changes nothing" "$(is "$status" 0)"

echo "case: a docker command whose image this check cannot read"
root=$(fixture opaque-docker)
cat > "$root/scripts/e2e/stage1-skeleton.sh" <<'YAML'
#!/usr/bin/env bash
docker run -d --name pg "$PG_IMAGE" >/dev/null
YAML
run_check "$root"
check "a variable from nowhere fails" "$(is "$status" 1)"
check "says what to do with it" "$(contains "$out" 'add the variable that holds it to IMAGE_FROM')"

echo "case: an IMAGE_FROM entry no docker command uses"
root=$(fixture image-from-stale)
rm -r "$root/.github/actions/pull-test-images"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the variable" "$(contains "$out" 'IMAGE_FROM names \$image here')"
check "says what to delete" "$(contains "$out" 'Delete the entry')"

echo "case: a FROM that is neither a stage nor an ARG"
root=$(fixture from-undeclared)
cat > "$root/packages/envoy/docker/Dockerfile" <<'YAML'
FROM ${NOT_AN_ARG} AS build
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the ARG it wanted" "$(contains "$out" 'does not declare as an ARG')"

echo "case: a FROM on an ARG whose default is a third-party image"
# A Dockerfile may name its base image through an ARG. Docker builds FROM that ARG's default
# value when no --build-arg overrides it, so the default is the image the build fetches and is a
# reference like any other. This is the shape the FROM lines had before this change
# (`ARG BUN_VERSION=1.4.4` with `FROM oven/bun:${BUN_VERSION}`), so it is the shape a return to a
# third-party registry takes.
root=$(fixture from-arg-default)
cat > "$root/packages/envoy/docker/Dockerfile" <<'YAML'
ARG BASE_IMAGE=docker.io/library/debian:trixie-slim
FROM ${BASE_IMAGE} AS build
YAML
run_check "$root"
check "a third-party default fails" "$(is "$status" 1)"
check "names the image the build fetches" "$(contains "$out" 'docker.io/library/debian:trixie-slim')"

echo "case: a file of the \`no CI job runs it\` ground, moved into CI's reach"
# scripts/e2e/stage3-devbox-workflow.sh may name mirror.gcr.io, because no CI job runs it. The
# same line in stage1-skeleton.sh, which the test job runs, is refused.
root=$(fixture mirror-in-reach-script)
cat > "$root/scripts/e2e/stage1-skeleton.sh" <<'YAML'
#!/usr/bin/env bash
docker run -d --name pg mirror.gcr.io/library/postgres:16 >/dev/null
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says a CI job runs it" "$(contains "$out" 'a CI job runs this file and it names mirror.gcr.io')"
check "and names the line" "$(contains "$out" 'stage1-skeleton.sh:2:')"

echo "case: a file of the \`it fetches nothing\` ground, moved into CI's reach"
# packages/envoy/scripts/dev-postgres.test.sh may name mirror.gcr.io, because the job that runs
# it installs a fake docker on PATH. The same shape in a workflow of the reach is refused: there
# the docker is the runner's.
root=$(fixture mirror-in-reach-workflow)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: docker run --rm mirror.gcr.io/library/postgres:16 pg_isready
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says a CI job runs it" "$(contains "$out" 'a CI job runs this file and it names mirror.gcr.io')"

echo "case: a new file naming mirror.gcr.io that is in neither list"
root=$(fixture mirror-new-file)
mkdir -p "$root/docs"
printf 'Start it with mirror.gcr.io/library/redis:7.\n' > "$root/docs/caching.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and the line" "$(contains "$out" 'docs/caching.md:1:')"
check "says it is in neither list" "$(contains "$out" "in neither CI's reach nor the exception list")"
check "and names both grounds" "$(contains "$out" 'no CI job runs it')"

echo "case: an exception the tree outgrew"
# scripts/e2e/README.md is in the exception list. A tree holding it without mirror.gcr.io in it
# is a ground that holds nothing any more.
root=$(fixture exception-stale)
printf 'The live proofs.\n' > "$root/scripts/e2e/README.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'scripts/e2e/README.md:1:')"
check "says what to delete" "$(contains "$out" 'Delete its line from EXCEPTIONS')"

echo "case: a file of CI's reach that is gone"
root=$(fixture reach-missing)
rm "$root/packages/envoy/internal/tests3/seaweedfs.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the list names it" "$(contains "$out" "CI's reach names this file, and it is gone")"

echo "case: a workflow spelled .yml, which neither glob reads"
root=$(fixture workflow-yml)
cp "$root/.github/workflows/ci.yaml" "$root/.github/workflows/extra.yml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says to rename it" "$(contains "$out" 'Rename it, or widen the two globs')"

echo "case: .github/ci-images.yaml itself"
root=$(fixture file-missing)
rm "$root/.github/ci-images.yaml"
run_check "$root"
check "a missing file fails" "$(is "$status" 1)"
check "says what it is for" "$(contains "$out" 'the one place that says which image a digest is')"

root=$(fixture registry-moved)
sed -i "s|^registry: .*|registry: ghcr.io/sjawhar/legion/other|" "$root/.github/ci-images.yaml"
run_check "$root"
check "another registry in the file itself fails" "$(is "$status" 1)"
check "names the one every consumer reads" "$(contains "$out" "every consumer names $REGISTRY")"

root=$(fixture file-unparsable)
printf 'images: [\n' > "$root/.github/ci-images.yaml"
run_check "$root"
check "a file that is not YAML fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the repository's own tree"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" "$check_script" 2>&1)
status=$?
set -e
check "the real tree passes, with every row held" "$(is "$status" 0)"
check "says so" "$(contains "$out" "holds .* row(s), .* of them in the package")"

summary "check-ci-images.sh"
