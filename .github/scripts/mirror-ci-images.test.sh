#!/usr/bin/env bash
# Tests for `mirror-ci-images.sh`: it copies the rows the package does not hold, it copies no row
# the package already holds, and it fails naming the row when a copy lands at another digest.
#
# NO CASE REACHES THE NETWORK. Every case puts a stand-in `crane` first on PATH, which reads and
# writes a stand-in registry under $CRANE_STATE: `tags` is one `<registry>:<tag>\t<digest>` line
# per tag the registry holds, `upstreams` is one `<upstream>\t<digest>` line per image a copy can
# read, `package` is one digest per line, and `copied` is one upstream per line the stand-in
# copied. The first three cases build a miniature repository under a temporary directory and
# symlink the real script into it, so the script under test is the file CI runs: it resolves its
# root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture
# root.
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
OTHER=sha256:9f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a30219f8e7d6c5b4a3021
REGISTRY=ghcr.io/sjawhar/legion/ci

# The stand-in crane: `digest` prints what the registry's tag points at, and exits 1 where it
# points at nothing, as crane does for a tag the registry does not hold. `copy` reads the
# upstream's digest and writes it under the tag, so a copy lands where `upstreams` says.
mkdir -p "$work/bin"
cat > "$work/bin/crane" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
state=${CRANE_STATE:?CRANE_STATE names the stand-in registry}
case "${1:-}" in
  digest)
    awk -F'\t' -v reference="$2" '$1 == reference { digest = $2 } END { if (digest == "") exit 1; print digest }' \
      "$state/tags"
    ;;
  copy)
    digest=$(awk -F'\t' -v upstream="$2" '$1 == upstream { print $2 }' "$state/upstreams")
    if [ -z "$digest" ]; then
      echo "crane: $2 is in no upstream of this test" >&2
      exit 1
    fi
    printf '%s\t%s\n' "$3" "$digest" >> "$state/tags"
    printf '%s\n' "$digest" >> "$state/package"
    printf '%s\n' "$2" >> "$state/copied"
    ;;
  *)
    echo "crane: this test's stand-in reads digest and copy, not ${1:-nothing}" >&2
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
# not the second's, and can read both upstreams. Echoes the fixture root. Each case names its own
# tree: two cases sharing a name would share a tree, so a repeated name is refused here rather
# than surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/state"
  ln -s "$mirror_script" "$root/.github/scripts/mirror-ci-images.sh"
  cat > "$root/.github/ci-images.yaml" <<YAML
registry: $REGISTRY

images:
  - upstream: docker.io/library/nats:2.10
    tag: nats-2.10
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14
    digest: $BUN
YAML
  printf '%s\t%s\n' "docker.io/library/nats:2.10" "$NATS" > "$root/state/upstreams"
  printf '%s\t%s\n' "docker.io/oven/bun:1.3.14" "$BUN" >> "$root/state/upstreams"
  printf '%s\t%s\n' "$REGISTRY:nats-2.10" "$NATS" > "$root/state/tags"
  printf '%s\n' "$NATS" > "$root/state/package"
  : > "$root/state/copied"
  echo "$root"
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
check "names the copy" "$(contains "$out" "copy   docker.io/oven/bun:1.3.14 -> $REGISTRY:bun-1.3.14")"
check "and the digest it landed at" "$(contains "$out" "copied $REGISTRY:bun-1.3.14 $BUN")"
check "counts the row it held" "$(contains "$out" '^1 row(s) already in the package, 1 copied$')"
check "copied that row alone" "$(is "$(cat "$root/state/copied")" "docker.io/oven/bun:1.3.14")"
check "and the package now holds its digest" "$(contains "$(cat "$root/state/package")" "$BUN")"

echo "case: a row the package already holds"
root=$(fixture copies-no-held-row)
printf '%s\t%s\n' "$REGISTRY:bun-1.3.14" "$BUN" >> "$root/state/tags"
printf '%s\n' "$BUN" >> "$root/state/package"
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "holds both rows" "$(contains "$out" "held   $REGISTRY:bun-1.3.14 $BUN")"
check "copies nothing" "$(is "$(cat "$root/state/copied")" "")"
check "and says so" "$(contains "$out" '^2 row(s) already in the package, 0 copied$')"

echo "case: a copy that lands at another digest"
root=$(fixture copy-lands-elsewhere)
printf '%s\t%s\n' "docker.io/oven/bun:1.3.14" "$OTHER" > "$root/state/upstreams"
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the row and both digests" \
  "$(contains "$out" "$REGISTRY:bun-1.3.14 landed at $OTHER, and the row says $BUN")"
check "says what to correct" "$(contains "$out" "Correct the row's digest, or the upstream reference it copies from")"

echo "case: the gate's check 7 before the copy, and after it"
# ci.yaml's ci-images job runs this script and then the gate's package mode, in that order,
# because that mode reads which rows the package holds. Here the stand-in package holds every row
# of .github/ci-images.yaml but the last one.
state="$work/ordering"
mkdir -p "$state"
: > "$state/copied"
held_back=$(PYTHONDONTWRITEBYTECODE=1 python3 - "$repository" "$state" <<'PY'
import sys

import yaml

repository, state = sys.argv[1], sys.argv[2]
document = yaml.safe_load(open(f"{repository}/.github/ci-images.yaml"))
registry, rows = document["registry"], document["images"]
with open(f"{state}/upstreams", "w") as upstreams:
    for row in rows:
        upstreams.write(f"{row['upstream']}\t{row['digest']}\n")
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
