#!/usr/bin/env bash
# Tests for `mirror-ci-images.sh`: it copies the rows the package does not hold, it copies no row
# the package already holds, it asks for the row's digest and never an upstream tag, a run whose
# package holds every row reads nothing outside that package, it refuses a library/golang row —
# held or being copied — whose image carries another Go than the row's tag names, it reads which
# repository a row names as .github/scripts/ci_images.py does, and it fails naming the row when
# a copy lands at another digest.
#
# NO CASE REACHES THE NETWORK. Every case puts a stand-in `crane` first on PATH, which reads and
# writes a stand-in registry under $CRANE_STATE: `tags` is one `<reference>\t<digest>` line per
# tag `crane digest` can read, `upstreams` is one `<reference>\t<digest>` line per reference a
# copy can read, `configs` is one `<reference>\t<GOLANG_VERSION>` line per reference
# `crane config` can read, `package` is one digest per line, `copied` is one reference per line
# the stand-in copied, and `references` is every reference the stand-in was asked for, in the
# order it was asked, which is what a case reads to say where a run went. The cases before the
# last build a miniature repository under a temporary directory and symlink the real script, and
# the reference grammar it reads a row with (ci_images.py), into it, so the script under test is
# the file CI runs: it resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which
# the symlink puts at the fixture root.
#
# The last case is the ordering ci.yaml's ci-images job keeps: the package mode of
# check-ci-images.sh reads which rows the package holds, so the copy for this change must run
# before it. It runs both scripts over the repository's own .github/ci-images.yaml, against a
# stand-in package that holds every row but one, and drives the gate through its documented
# CI_IMAGES_MANIFEST_HEAD seam.
#
# Run from anywhere: .github/scripts/mirror-ci-images.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository=$(cd "$script_dir/../.." && pwd)
mirror_script="$script_dir/mirror-ci-images.sh"
gate_script="$script_dir/check-ci-images.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The digests the fixtures use. Made up: nothing is pulled, and what each case reads is the row
# they match.
NATS=sha256:0a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f60710a1b2c3d4e5f6071
BUN=sha256:1b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f6071821b2c3d4e5f607182
GOLANG=sha256:3d4e5f60718293a43d4e5f60718293a43d4e5f60718293a43d4e5f60718293a4
OTHER=sha256:9f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a3021
REGISTRY=ghcr.io/sjawhar/legion/ci

# The stand-in crane: `digest` prints what the registry's tag points at, and exits 1 where it
# points at nothing, as crane does for a tag the registry does not hold. `copy` reads the
# upstream's digest and writes it under the tag, so a copy lands where `upstreams` says.
# `config` prints an image config of the shape library/golang's own build writes, carrying the
# Go version `configs` gives that reference. Every reference it is asked for, whichever
# subcommand asks, it appends to `references`: that file is what a case reads to say which
# registries a run went to, and a real crane would reach the network for each line of it.
mkdir -p "$work/bin"
cat > "$work/bin/crane" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
state=${CRANE_STATE:?CRANE_STATE names the stand-in registry}
case "${1:-}" in
  digest)
    printf '%s\n' "$2" >> "$state/references"
    awk -F'\t' -v reference="$2" '$1 == reference { digest = $2 } END { if (digest == "") exit 1; print digest }' \
      "$state/tags"
    ;;
  copy)
    printf '%s\n%s\n' "$2" "$3" >> "$state/references"
    digest=$(awk -F'\t' -v upstream="$2" '$1 == upstream { print $2 }' "$state/upstreams")
    if [ -z "$digest" ]; then
      echo "crane: $2 is in no upstream of this test" >&2
      exit 1
    fi
    printf '%s\t%s\n' "$3" "$digest" >> "$state/tags"
    printf '%s\n' "$digest" >> "$state/package"
    printf '%s\n' "$2" >> "$state/copied"
    ;;
  config)
    # `crane config --platform <platform> <reference>`, the call the script makes. The platform
    # is read here so that a call without it fails this stand-in rather than passing unseen.
    shift
    if [ "${1:-}" != "--platform" ]; then
      echo "crane: this test's stand-in reads config with --platform, not ${1:-nothing}" >&2
      exit 1
    fi
    shift 2
    # The reference comes last, after the platform the branch above read off the front.
    printf '%s\n' "${1:-}" >> "$state/references"
    version=$(awk -F'\t' -v reference="${1:-}" '$1 == reference { print $2 }' "$state/configs")
    if [ -z "$version" ]; then
      echo "crane: ${1:-} is in no config of this test" >&2
      exit 1
    fi
    printf '{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/local/go/bin:/usr/local/bin:/bin","GOLANG_VERSION=%s","GOTOOLCHAIN=local","GOPATH=/go"]}}\n' \
      "$version"
    ;;
  *)
    echo "crane: this test's stand-in reads digest, copy and config, not ${1:-nothing}" >&2
    exit 1
    ;;
esac
SH
chmod +x "$work/bin/crane"

# The stand-in for check-ci-images.sh's manifest HEAD: 200 for a digest the stand-in package
# holds, 404 for one it does not.
head_script="$work/head.sh"
cat > "$head_script" <<'SH'
#!/usr/bin/env bash
if grep -qxF -- "$1" "${CRANE_STATE:?}/package"; then
  echo 200
else
  echo 404
fi
SH
chmod +x "$head_script"

# fixture <name>: a tree of two rows, with a stand-in registry that holds the first row's tag and
# not the second's, and that reads each row's digest reference. Echoes the fixture root. Each
# case names its own tree: two cases sharing a name would share a tree, so a repeated name is
# refused here rather than surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/state"
  ln -s "$mirror_script" "$root/.github/scripts/mirror-ci-images.sh"
  ln -s "$script_dir/ci_images.py" "$root/.github/scripts/ci_images.py"
  cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10-0a1b2c3d
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14-1b2c3d4e
    digest: $BUN
YAML
  printf '%s\t%s\n' "docker.io/library/nats:2.10@$NATS" "$NATS" > "$root/state/upstreams"
  printf '%s\t%s\n' "docker.io/oven/bun:1.3.14@$BUN" "$BUN" >> "$root/state/upstreams"
  printf '%s\t%s\n' "$REGISTRY:nats-2.10-0a1b2c3d" "$NATS" > "$root/state/tags"
  printf '%s\n' "$NATS" > "$root/state/package"
  : > "$root/state/configs"
  : > "$root/state/copied"
  : > "$root/state/references"
  echo "$root"
}

# golang_row <root> <version>: appends a library/golang row to that tree, which the package does
# not hold, with a stand-in registry that reads its digest reference and answers `crane config`
# with an image carrying Go <version>. The row's own upstream tag names 1.27.1, so any other
# <version> is an image whose bytes are another Go than its label claims. The config answers
# alike at the upstream and in the package: `crane copy` keeps the manifest digest, so one
# digest is one set of bytes wherever a run reads it, and a case that holds the row reads the
# second line.
golang_row() {
  local root=$1 version=$2
  cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: docker.io/library/golang:1.27.1-alpine
    tag: golang-1.27.1-alpine-3d4e5f60
    digest: $GOLANG
YAML
  printf '%s\t%s\n' "docker.io/library/golang:1.27.1-alpine@$GOLANG" "$GOLANG" \
    >> "$root/state/upstreams"
  printf '%s\t%s\n' "docker.io/library/golang:1.27.1-alpine@$GOLANG" "$version" \
    >> "$root/state/configs"
  printf '%s\t%s\n' "$REGISTRY@$GOLANG" "$version" >> "$root/state/configs"
}

# run_mirror <root>: runs the script under test against that tree, with the stand-in crane first
# on PATH, setting `out` to its combined output and `status` to its exit code. Not a subshell, so
# both survive the call.
run_mirror() {
  local root=$1
  set +e
  out=$(PATH="$work/bin:$PATH" CRANE_STATE="$root/state" \
    "$root/.github/scripts/mirror-ci-images.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a row the package does not hold"
root=$(fixture copies-the-missing-row)
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "names the copy" "$(contains "$out" "copy   docker.io/oven/bun:1.3.14@$BUN -> $REGISTRY:bun-1.3.14-1b2c3d4e")"
check "and the digest it landed at" "$(contains "$out" "copied $REGISTRY:bun-1.3.14-1b2c3d4e $BUN")"
check "counts the row it held" "$(contains "$out" '^1 row(s) already in the package, 1 copied$')"
check "copied that row alone" "$(is "$(cat "$root/state/copied")" "docker.io/oven/bun:1.3.14@$BUN")"
check "and the package now holds its digest" "$(contains "$(cat "$root/state/package")" "$BUN")"
# An upstream tag floats, so a reference without a digest names other bytes on another day.
check "asked crane for no upstream tag" \
  "$(is "$(contains "$(cat "$root/state/references")" '^docker\.io/[a-z/]*:[^@]*$')" false)"

echo "case: a row the package already holds"
root=$(fixture copies-no-held-row)
printf '%s\t%s\n' "$REGISTRY:bun-1.3.14-1b2c3d4e" "$BUN" >> "$root/state/tags"
printf '%s\n' "$BUN" >> "$root/state/package"
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "holds both rows" "$(contains "$out" "held   $REGISTRY:bun-1.3.14-1b2c3d4e $BUN")"
check "copies nothing" "$(is "$(cat "$root/state/copied")" "")"
check "and says so" "$(contains "$out" '^2 row(s) already in the package, 0 copied$')"

echo "case: a run whose package holds every row"
# The common run: the package holds each row, and nothing is copied. Every read it makes is a
# read of the package this repository publishes, the Go check included, which reads the row's
# digest there. A read of Docker Hub in this run would be a third-party fetch in a CI job on a
# run that copies nothing, which is what this package exists to end (sjawhar/legion#2512).
root=$(fixture holds-every-row)
golang_row "$root" 1.27.1
printf '%s\t%s\n' "$REGISTRY:bun-1.3.14-1b2c3d4e" "$BUN" >> "$root/state/tags"
printf '%s\t%s\n' "$REGISTRY:golang-1.27.1-alpine-3d4e5f60" "$GOLANG" >> "$root/state/tags"
printf '%s\n%s\n' "$BUN" "$GOLANG" >> "$root/state/package"
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "holds every row" "$(contains "$out" '^3 row(s) already in the package, 0 copied$')"
check "reads the Go of the package's own copy" \
  "$(contains "$out" "go     $REGISTRY@$GOLANG carries Go 1.27.1, the version its upstream tag names")"
check "copies nothing" "$(is "$(cat "$root/state/copied")" "")"
check "asks crane for no docker.io reference" \
  "$(is "$(contains "$(cat "$root/state/references")" '^docker\.io/')" false)"
check "and for no reference outside the package at all" \
  "$(is "$(grep -v "^$REGISTRY[:@]" "$root/state/references" || true)" "")"

echo "case: a held library/golang row whose image carries another Go than its tag names"
# The row is labelled 1.27.1-alpine and its digest holds a Go 1.26.3 image. Every other gate
# reads that label and never the bytes. library/golang writes its own version into the image
# config, so this run reads it, and the package holds this row already: the read is of the
# package's own copy at the row's digest, which `crane copy` kept, so it is the same bytes the
# upstream holds under that digest.
root=$(fixture golang-config-disagrees)
golang_row "$root" 1.26.3
printf '%s\t%s\n' "$REGISTRY:golang-1.27.1-alpine-3d4e5f60" "$GOLANG" >> "$root/state/tags"
printf '%s\n' "$GOLANG" >> "$root/state/package"
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the reference it read and both versions" \
  "$(contains "$out" "$REGISTRY@$GOLANG carries Go 1.26.3, and this row's upstream tag names Go 1.27.1")"
check "says the label and the bytes disagree" "$(contains "$out" "The row's label and its bytes disagree")"
check "says which copy of those bytes it read" \
  "$(contains "$out" "That read named $REGISTRY@$GOLANG, which holds this row's digest")"
check "names the row's whole upstream tag in the remedy" \
  "$(contains "$out" "Write the digest docker.io/library/golang:1.27.1-alpine names now")"
check "read the package for it, and not Docker Hub" \
  "$(is "$(contains "$(cat "$root/state/references")" '^docker\.io/library/golang')" false)"
check "and copies no golang row" \
  "$(is "$(contains "$(cat "$root/state/copied")" "golang")" false)"

echo "case: a library/golang row being copied whose image carries another Go than its tag names"
# The package does not hold this row, so the check reads the upstream the copy below would read,
# and reads it first: bytes whose label is wrong never land in the package.
root=$(fixture golang-copy-disagrees)
golang_row "$root" 1.26.3
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the reference it read and both versions" \
  "$(contains "$out" "docker.io/library/golang:1.27.1-alpine@$GOLANG carries Go 1.26.3, and this row's upstream tag names Go 1.27.1")"
check "names the row's whole upstream tag in the remedy" \
  "$(contains "$out" "Write the digest docker.io/library/golang:1.27.1-alpine names now")"
check "and copies no golang row" \
  "$(is "$(contains "$(cat "$root/state/copied")" "golang")" false)"

echo "case: a library/golang row whose image carries the Go its tag names"
root=$(fixture golang-config-agrees)
golang_row "$root" 1.27.1
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "names the Go the row's bytes carry" \
  "$(contains "$out" "go     docker.io/library/golang:1.27.1-alpine@$GOLANG carries Go 1.27.1, the version its upstream tag names")"
check "and copies the row" "$(contains "$out" "copied $REGISTRY:golang-1.27.1-alpine-3d4e5f60 $GOLANG")"

echo "case: a library/golang row whose upstream names a host with a port"
# One parser: ci_images.py's upstream_parts reads the repository off the host, as rules 3 and 7
# of check-ci-images.sh do, so this row is a library/golang row to the gate and to this script
# alike. The hand-written split this script carried before read the host as part of the
# repository, so `registry.example.com:5000/library/golang` was not library/golang to it and the
# row went past the Go check and into the package. Its bytes carry Go 1.26.3 and its tag names
# 1.27.1, and the stand-in can copy it, so a run that skipped the check would pass.
root=$(fixture golang-host-with-port)
cat >> "$root/.github/ci-images.yaml" <<YAML
  - upstream: registry.example.com:5000/library/golang:1.27.1-alpine
    tag: golang-1.27.1-alpine-3d4e5f60
    digest: $GOLANG
YAML
printf '%s\t%s\n' "registry.example.com:5000/library/golang:1.27.1-alpine@$GOLANG" "$GOLANG" \
  >> "$root/state/upstreams"
printf '%s\t%s\n' "registry.example.com:5000/library/golang:1.27.1-alpine@$GOLANG" "1.26.3" \
  >> "$root/state/configs"
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the reference it read and both versions" \
  "$(contains "$out" "registry.example.com:5000/library/golang:1.27.1-alpine@$GOLANG carries Go 1.26.3, and this row's upstream tag names Go 1.27.1")"
check "and copies no golang row" \
  "$(is "$(contains "$(cat "$root/state/copied")" "golang")" false)"

echo "case: a copy that lands at another digest"
root=$(fixture copy-lands-elsewhere)
printf '%s\t%s\n' "docker.io/oven/bun:1.3.14@$BUN" "$OTHER" > "$root/state/upstreams"
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the row and both digests" \
  "$(contains "$out" "$REGISTRY:bun-1.3.14-1b2c3d4e landed at $OTHER, and the row says $BUN")"
check "names the reference it copied" "$(contains "$out" "This copy read docker.io/oven/bun:1.3.14@$BUN")"
check "and says what to do" "$(contains "$out" 're-run the ci-images job')"

echo "case: the gate's check 8 before the copy, and after it"
# ci.yaml's ci-images job runs this script and then the gate's package mode, in that order,
# because that mode reads which rows the package holds. Here the stand-in package holds every row
# of .github/ci-images.yaml but the last one.
state="$work/ordering"
mkdir -p "$state"
: > "$state/copied"
: > "$state/references"
held_back=$(PYTHONDONTWRITEBYTECODE=1 python3 - "$repository" "$state" <<'PY'
import sys

import yaml

repository, state = sys.argv[1], sys.argv[2]
document = yaml.safe_load(open(f"{repository}/.github/ci-images.yaml"))
registry, rows = document["registry"], document["images"]
with open(f"{state}/upstreams", "w") as upstreams, open(f"{state}/configs", "w") as configs:
    for row in rows:
        # The reference the script reads for this row: its upstream without the digest it may
        # carry, at the row's digest. The copy asks for that string, and the Go check reads the
        # config of that same string.
        source = f"{row['upstream'].partition('@')[0]}@{row['digest']}"
        upstreams.write(f"{source}\t{row['digest']}\n")
        if "/golang:" in row["upstream"]:
            # The Go the row's upstream tag names, which a library/golang image declares as
            # GOLANG_VERSION. These bytes carry the Go their label claims, so the Go check
            # passes over the repository's own rows. One digest is one set of bytes, so the
            # package's copy answers as the upstream does: a held row is read there.
            carried = row["upstream"].partition("@")[0].rpartition(":")[2]
            go = carried.partition("-")[0]
            configs.write(f"{source}\t{go}\n")
            configs.write(f"{registry}@{row['digest']}\t{go}\n")
with open(f"{state}/tags", "w") as tags, open(f"{state}/package", "w") as package:
    for row in rows[:-1]:
        tags.write(f"{registry}:{row['tag']}\t{row['digest']}\n")
        package.write(f"{row['digest']}\n")
print(rows[-1]["tag"])
PY
)

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" CRANE_STATE="$state" "$gate_script" package 2>&1)
status=$?
set -e
check "the gate fails before the copy" "$(is "$status" 1)"
check "naming the row the package lacks" "$(contains "$out" "($held_back, the copy of")"
check "and the status it answered" "$(contains "$out" 'is not in the package: a manifest HEAD answered 404')"

set +e
out=$(PATH="$work/bin:$PATH" CRANE_STATE="$state" "$mirror_script" 2>&1)
status=$?
set -e
check "the copy runs" "$(is "$status" 0)"
check "and copies that one row" "$(contains "$out" "copied $REGISTRY:$held_back ")"

set +e
out=$(CI_IMAGES_MANIFEST_HEAD="$head_script" CRANE_STATE="$state" "$gate_script" package 2>&1)
status=$?
set -e
check "the gate passes after the copy" "$(is "$status" 0)"
check "with every row in the package" "$(contains "$out" "holds .* row(s), .* of them in the package")"

summary "mirror-ci-images.sh"
