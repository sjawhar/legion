#!/usr/bin/env bash
# Tests for `check-ci-images.sh`: one red case per rule it enforces, under the mode that owns
# that refusal, one per ground of its exception list, and the repository's own tree in each mode.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script, the path model its sweep walks the tree with (workflow_paths.py) and the reference
# grammar it reads a reference with (ci_images.py), into it, so the code under test is the code
# CI runs, not a copy that can drift: the script resolves its root from its own path
# (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture root.
# A fixture holds every file CI's reach names, because the check fails on a reach file that is
# gone.
#
# NO CASE REACHES THE NETWORK. The check reads which rows the package holds through
# CI_IMAGES_MANIFEST_HEAD: when that variable is set, it runs the command it names once per row
# and reads the HTTP status code the command prints. Every case here sets it to `head.sh` below,
# which answers 200 for every digest but the ones MISSING names, and 404 for those. So the check
# that reads ghcr.io is driven by a case, and nothing is pulled.
#
# EVERY CASE NAMES ITS MODE. The check takes one mode: `tree` runs the checks that read the
# files of the tree, and `package` runs the one that reads ghcr.io. Each case runs under the
# mode that owns its refusal, and one case holds each mode to its own half — the tree mode runs
# the stand-in for the network never, and the package mode passes a tree the tree mode refuses.
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
GOLANG_NEXT=sha256:5f60718293a4b5c65f60718293a4b5c65f60718293a4b5c65f60718293a4b5c6
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

# The spy the mode case drives the check with: it answers 200 for every digest and writes each
# one it was asked about to $SPY_LOG. The check reads ghcr.io through this seam alone, so a run
# that leaves the log empty made no network read.
spy_script="$work/spy.sh"
cat > "$spy_script" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$1" >> "${SPY_LOG:?}"
echo 200
SH
chmod +x "$spy_script"

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
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  ln -s "$script_dir/ci_images.py" "$root/.github/scripts/ci_images.py"
  echo "1.3.14" > "$root/.bun-version"
  cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10-0a1b2c3d
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14-1b2c3d4e
    digest: $BUN
  - upstream: docker.io/oven/bun:1.3.14-slim
    tag: bun-1.3.14-slim-2c3d4e5f
    digest: $BUN_SLIM
  - upstream: docker.io/library/golang:1.25.0-alpine
    tag: golang-1.25.0-alpine-3d4e5f60
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

# run_check <mode> <root>: runs the script under test against that tree, in that mode, setting
# `out` to its combined output and `status` to its exit code. Not a subshell, so both survive the
# call. MISSING, which the caller may set, names the digests the stand-in package does not hold.
run_check() {
  local mode=$1 root=$2
  set +e
  out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" MISSING="${MISSING:-}" \
    "$root/.github/scripts/check-ci-images.sh" "$mode" 2>&1)
  status=$?
  set -e
}

# golang_next_row <root>: appends the row a Go bump's first commit lands, the next release
# beside the one ARG GO_VERSION pins.
golang_next_row() {
  cat >> "$1/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/golang:1.26.3-alpine
    tag: golang-1.26.3-alpine-5f607182
    digest: $GOLANG_NEXT
YAML
}

echo "case: a tree whose every image is a row of .github/ci-images.yaml"
root=$(fixture green)
run_check tree "$root"
check "the tree mode exits 0" "$(is "$status" 0)"
check "says how many rows it read" "$(contains "$out" 'holds 4 row(s);')"
check "counts the reach" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"
run_check package "$root"
check "the package mode exits 0" "$(is "$status" 0)"
check "says what the package holds" "$(contains "$out" 'holds 4 row(s), 4 of them in the package')"

echo "case: each mode runs its own half"
# The tree mode's refusals read files, and the package mode's one refusal reads ghcr.io. This
# tree carries a refusal of the tree half (a Go string on another registry, rule 4) and a package
# that holds every row, so each mode answers for its half alone.
root=$(fixture mode-halves)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<'YAML'
package testpg

const Image = "docker.io/pgvector/pgvector:pg17"
YAML
spy_log="$work/spy.log"
: > "$spy_log"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" tree 2>&1)
status=$?
set -e
check "the tree mode refuses the reference" "$(is "$status" 1)"
check "naming the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and reads the package for no row" "$(is "$(wc -l < "$spy_log")" 0)"

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$spy_script" SPY_LOG="$spy_log" \
  "$root/.github/scripts/check-ci-images.sh" package 2>&1)
status=$?
set -e
check "the package mode passes the same tree" "$(is "$status" 0)"
check "naming that reference nowhere" "$(is "$(contains "$out" 'docker.io/pgvector')" false)"
check "and reads the package once per row" "$(is "$(wc -l < "$spy_log")" 4)"

echo "case: rule 1 — a row this check cannot read"
root=$(fixture row-no-tag)
sed -i '/tag: nats-2.10-0a1b2c3d/d' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a row with no tag fails" "$(is "$status" 1)"
check "says why a tag is needed" "$(contains "$out" 'a package version nothing names')"
check "names the row" "$(contains "$out" 'the row for docker.io/library/nats:2.10 has no tag')"

root=$(fixture row-empty-tag)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: ""/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "an empty tag fails too" "$(is "$status" 1)"
check "names the row" "$(contains "$out" 'has no tag')"

root=$(fixture row-bad-digest)
sed -i "s|digest: $NATS|digest: sha256:notadigest|" "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a digest that is not 64 hex characters fails" "$(is "$status" 1)"
check "says what to run" "$(contains "$out" 'crane digest docker.io/library/nats:2.10')"

echo "case: rule 2 — two rows naming one digest, or one tag"
root=$(fixture duplicate-digest)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.11-0a1b2c3d
    digest: $NATS
YAML
run_check tree "$root"
check "a second row on one digest fails" "$(is "$status" 1)"
check "says why one digest is one row" "$(contains "$out" 'one digest names one row')"

root=$(fixture duplicate-tag)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/nats:2.11
    tag: nats-2.10-0a1b2c3d
    digest: $UNKNOWN
YAML
run_check tree "$root"
check "a second row on one tag fails" "$(is "$status" 1)"
check "says the copy would move the tag" "$(contains "$out" 'would move the tag off the first')"

echo "case: rule 2 — a tag that is not the one its row's upstream and digest give it"
# The whole string is read. The eight characters at its end are what makes a shared tag
# impossible: without them the nats row's tag is the tag every digest that image ever has can
# take, and a later copy moves it off the digest this tree's consumers pin.
root=$(fixture tag-without-digest)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: nats-2.10/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tag without those characters fails" "$(is "$status" 1)"
check "names the tag it read" "$(contains "$out" 'tag nats-2.10 is not nats-2.10-0a1b2c3d')"
check "and the tag the row should carry" "$(contains "$out" 'Name this row nats-2.10-0a1b2c3d')"

root=$(fixture tag-suffix-alone)
sed -i 's/tag: nats-2.10-0a1b2c3d/tag: anything-0a1b2c3d/' "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tag that ends with them and names another image fails" "$(is "$status" 1)"
check "naming the whole tag the rule gives it" \
  "$(contains "$out" 'tag anything-0a1b2c3d is not nats-2.10-0a1b2c3d')"

root=$(fixture tag-without-digest-untagged-upstream)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/ubuntu@$UNKNOWN
    tag: ubuntu
    digest: $UNKNOWN
YAML
run_check tree "$root"
check "a row whose upstream names no tag fails the same way" "$(is "$status" 1)"
check "and is named the digest alone" "$(contains "$out" 'Name this row ubuntu-9f8e7d6c')"

echo "case: rule 3 — a version pinned elsewhere that no row carries"
root=$(fixture bun-drift)
sed -i 's|upstream: docker.io/oven/bun:1.3.14|upstream: docker.io/oven/bun:1.4.2|' \
  "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a tree whose every bun row moved off the pin fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" '.bun-version locks 1.3.14')"
check "and the row it wants" "$(contains "$out" 'no row copies oven/bun:1.3.14')"
check "and the order of a bump" "$(contains "$out" 'Raising Bun is two commits')"

root=$(fixture golang-drift)
sed -i 's|ARG GO_VERSION=1.25.0|ARG GO_VERSION=1.26.3|' \
  "$root/packages/daemon/docker/worker.Dockerfile"
run_check tree "$root"
check "a tree whose golang row is not the pinned release fails" "$(is "$status" 1)"
check "names the pin" "$(contains "$out" 'pins ARG GO_VERSION=1.26.3')"
check "and what to write" "$(contains "$out" 'golang:1.26.3-alpine')"

# The first commit of a bump: the next version's row lands, and is copied, while the pin and
# every FROM still name the version before it. An extra row is admitted for that reason.
root=$(fixture next-version-rows)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/oven/bun:1.4.2-slim
    tag: bun-1.4.2-slim-4e5f6071
    digest: sha256:4e5f60718293a4b54e5f60718293a4b54e5f60718293a4b54e5f60718293a4b5
YAML
golang_next_row "$root"
run_check tree "$root"
check "a row for the next Bun and Go, beside the pinned rows, passes" "$(is "$status" 0)"

echo "case: rule 4 — a reference on another registry"
root=$(fixture other-registry)
cat > "$root/packages/envoy/internal/testpg/testpg.go" <<YAML
package testpg

const Image = "docker.io/pgvector/pgvector@$NATS"
YAML
run_check tree "$root"
check "a docker.io reference fails" "$(is "$status" 1)"
check "names the registry it found" "$(contains "$out" 'this string names docker.io/pgvector/pgvector')"
check "and the one package" "$(contains "$out" "a copy in $REGISTRY")"

echo "case: rule 5 — a reference that is not pinned by digest"
root=$(fixture tag-not-digest)
sed -i "s|image: $REGISTRY@$NATS|image: $REGISTRY:nats-2.10-0a1b2c3d|" "$root/.github/workflows/ci.yaml"
run_check tree "$root"
check "a tagged reference fails" "$(is "$status" 1)"
check "says a tag can move" "$(contains "$out" 'a name the registry can move')"
check "names the service and its line" "$(contains "$out" 'ci.yaml:8: the nats service')"

echo "case: rule 6 — a reference whose digest no row holds"
root=$(fixture digest-unknown)
sed -i "s|$NATS|$UNKNOWN|" "$root/packages/envoy-client/src/__tests__/nats-server.ts"
run_check tree "$root"
check "a digest outside the file fails" "$(is "$status" 1)"
check "names the file to add it to" "$(contains "$out" 'names a digest no row of .github/ci-images.yaml holds')"
check "and the line to edit" "$(contains "$out" 'nats-server.ts:1:')"

echo "case: rule 7 — a reference on a golang row at another version than ARG GO_VERSION"
# A bump's first commit lands the next Go's row beside the pinned one, and rule 3 admits it. A
# FROM moved onto that row before the ARG would build its stage with a Go the worker's toolchain
# stage never downloads, so every reference is read against the ARG.
root=$(fixture golang-from-off-the-arg)
golang_next_row "$root"
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
run_check tree "$root"
check "a FROM on the other golang row fails" "$(is "$status" 1)"
check "names the row and the Go it copies" \
  "$(contains "$out" 'the row for docker.io/library/golang:1.26.3-alpine on line .*, which copies Go 1.26.3-alpine')"
check "and the ARG it disagrees with" "$(contains "$out" 'pins ARG GO_VERSION=1.25.0')"
check "and the line of the FROM" "$(contains "$out" 'packages/envoy/docker/Dockerfile:1:')"

# The second commit of a bump: the ARG and every golang FROM move to the new row together.
root=$(fixture golang-from-moved-with-the-arg)
golang_next_row "$root"
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
sed -i 's|ARG GO_VERSION=1.25.0|ARG GO_VERSION=1.26.3|' \
  "$root/packages/daemon/docker/worker.Dockerfile"
run_check tree "$root"
check "the ARG and the FROM moved together passes" "$(is "$status" 0)"

# The same rows, with Docker Hub's implicit `library/` left out of the golang upstreams:
# docker.io/golang:1.25.0-alpine and docker.io/library/golang:1.25.0-alpine are one image. Rule
# 3 and rule 7 read a repository of one path element under `library/`, so both rules read the
# two spellings alike. Reading the row's text instead would lose the pinned row to rule 3 and
# hide the FROM below from rule 7.
root=$(fixture golang-short-spelling)
sed -i 's|upstream: docker.io/library/golang:|upstream: docker.io/golang:|' \
  "$root/.github/ci-images.yaml"
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/golang:1.26.3-alpine
    tag: golang-1.26.3-alpine-5f607182
    digest: $GOLANG_NEXT
YAML
sed -i "s|$GOLANG|$GOLANG_NEXT|" "$root/packages/envoy/docker/Dockerfile"
run_check tree "$root"
check "a FROM on the short-spelled other golang row fails" "$(is "$status" 1)"
check "names that row and the Go it copies" \
  "$(contains "$out" 'the row for docker.io/golang:1.26.3-alpine on line .*, which copies Go 1.26.3-alpine')"
check "and rule 3 reads the pinned row under its short spelling" \
  "$(is "$(contains "$out" 'no row copies library/golang')" false)"

echo "case: rule 8 — a row the package does not hold"
root=$(fixture not-published)
MISSING=$GOLANG run_check package "$root"
check "a row ghcr.io answers 404 for fails" "$(is "$status" 1)"
check "names the row and the status" "$(contains "$out" 'is not in the package: a manifest HEAD answered 404')"
check "and says what copies it" "$(contains "$out" "ci.yaml's ci-images job copies a row")"

root=$(fixture published)
MISSING=$UNKNOWN run_check package "$root"
check "a 404 on a digest no row holds changes nothing" "$(is "$status" 0)"

echo "case: a docker command whose image this check cannot read"
root=$(fixture opaque-docker)
cat > "$root/scripts/e2e/stage1-skeleton.sh" <<'YAML'
#!/usr/bin/env bash
docker run -d --name pg "$PG_IMAGE" >/dev/null
YAML
run_check tree "$root"
check "a variable from nowhere fails" "$(is "$status" 1)"
check "says what to do with it" "$(contains "$out" 'add the variable that holds it to IMAGE_FROM')"

echo "case: an IMAGE_FROM entry no docker command uses"
root=$(fixture image-from-stale)
rm -r "$root/.github/actions/pull-test-images"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the variable" "$(contains "$out" 'IMAGE_FROM names \$image here')"
check "says what to delete" "$(contains "$out" 'Delete the entry')"

echo "case: a FROM that is neither a stage nor an ARG"
root=$(fixture from-undeclared)
cat > "$root/packages/envoy/docker/Dockerfile" <<'YAML'
FROM ${NOT_AN_ARG} AS build
YAML
run_check tree "$root"
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
run_check tree "$root"
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
run_check tree "$root"
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
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says a CI job runs it" "$(contains "$out" 'a CI job runs this file and it names mirror.gcr.io')"

echo "case: a new file naming mirror.gcr.io that is in neither list"
root=$(fixture mirror-new-file)
mkdir -p "$root/docs"
printf 'Start it with mirror.gcr.io/library/redis:7.\n' > "$root/docs/caching.md"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the file and the line" "$(contains "$out" 'docs/caching.md:1:')"
check "says it is in neither list" "$(contains "$out" "in neither CI's reach nor the exception list")"
check "and names both grounds" "$(contains "$out" 'no CI job runs it')"

echo "case: an exception the tree outgrew"
# scripts/e2e/README.md is in the exception list. A tree holding it without mirror.gcr.io in it
# is a ground that holds nothing any more.
root=$(fixture exception-stale)
printf 'The live proofs.\n' > "$root/scripts/e2e/README.md"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'scripts/e2e/README.md:1:')"
check "says what to delete" "$(contains "$out" 'Delete its line from EXCEPTIONS')"

echo "case: a file of CI's reach that is gone"
root=$(fixture reach-missing)
rm "$root/packages/envoy/internal/tests3/seaweedfs.go"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says the list names it" "$(contains "$out" "CI's reach names this file, and it is gone")"

echo "case: a workflow spelled .yml, which neither glob reads"
root=$(fixture workflow-yml)
cp "$root/.github/workflows/ci.yaml" "$root/.github/workflows/extra.yml"
run_check tree "$root"
check "fails" "$(is "$status" 1)"
check "says to rename it" "$(contains "$out" 'Rename it, or widen the two globs')"

echo "case: .github/ci-images.yaml itself"
root=$(fixture file-missing)
rm "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a missing file fails" "$(is "$status" 1)"
check "says what it is for" "$(contains "$out" 'the one place that says which image a digest is')"

root=$(fixture registry-moved)
sed -i "s|^registry: .*|registry: ghcr.io/sjawhar/legion/other|" "$root/.github/ci-images.yaml"
run_check tree "$root"
check "another registry in the file itself fails" "$(is "$status" 1)"
check "names the one every consumer reads" "$(contains "$out" "every consumer names $REGISTRY")"

root=$(fixture file-unparsable)
printf 'images: [\n' > "$root/.github/ci-images.yaml"
run_check tree "$root"
check "a file that is not YAML fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a call that names no mode, and one that names both"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "no mode fails" "$(is "$status" 1)"
check "naming the tree mode" "$(contains "$out" 'this check takes one mode: .tree. reads the files of this tree')"
check "and the package mode" "$(contains "$out" '.package. reads ghcr.io/sjawhar/legion/ci')"

set +e
out=$("$check_script" tree package 2>&1)
status=$?
set -e
check "both modes in one call fails" "$(is "$status" 1)"
check "naming them" "$(contains "$out" 'this check takes one mode')"

echo "case: the repository's own tree, in each mode"
set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" "$check_script" tree 2>&1)
status=$?
set -e
check "the real tree passes the tree mode" "$(is "$status" 0)"
check "says what it read" "$(contains "$out" "reference(s) in .* file(s) of CI's reach")"

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" "$check_script" package 2>&1)
status=$?
set -e
check "the real tree passes the package mode, with every row held" "$(is "$status" 0)"
check "says so" "$(contains "$out" "holds .* row(s), .* of them in the package")"

summary "check-ci-images.sh"
