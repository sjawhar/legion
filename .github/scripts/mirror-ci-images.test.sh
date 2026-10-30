#!/usr/bin/env bash
# Tests for `mirror-ci-images.sh`: it copies the rows the package does not hold, it copies no row
# the package already holds, it asks for the row's digest and not the row's upstream tag, it
# refuses a row whose upstream tag names another digest, and it fails naming the row when a copy
# lands at another digest.
#
# NO CASE REACHES THE NETWORK. Every case puts a stand-in `crane` first on PATH, which reads and
# writes a stand-in registry under $CRANE_STATE: `tags` is one `<reference>\t<digest>` line per
# tag `crane digest` can read, in the package and upstream both, `upstreams` is one
# `<reference>\t<digest>` line per reference a copy can read, `package` is one digest per line,
# and `copied` is one reference per line the stand-in copied. A later line of `tags` wins, so a
# case moves an upstream tag by appending one. The reference the script copies from is the row's
# upstream at the row's digest, so a fixture line on the upstream tag alone says where a run that
# read the tag would land. The first five cases build a miniature repository under a temporary
# directory and symlink the real script into it, so the script under test is the file CI runs: it
# resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at
# the fixture root.
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
# not the second's, that reads each row's digest reference, and that answers each row's upstream
# tag with that row's digest. Echoes the fixture root. Each case names its own tree: two cases
# sharing a name would share a tree, so a repeated name is refused here rather than surfacing as
# the `ln -s` below failing.
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
    tag: nats-2.10-0a1b2c3d
    digest: $NATS
  - upstream: docker.io/oven/bun:1.3.14
    tag: bun-1.3.14-1b2c3d4e
    digest: $BUN
YAML
  printf '%s\t%s\n' "docker.io/library/nats:2.10@$NATS" "$NATS" > "$root/state/upstreams"
  printf '%s\t%s\n' "docker.io/oven/bun:1.3.14@$BUN" "$BUN" >> "$root/state/upstreams"
  printf '%s\t%s\n' "$REGISTRY:nats-2.10-0a1b2c3d" "$NATS" > "$root/state/tags"
  printf '%s\t%s\n' "docker.io/library/nats:2.10" "$NATS" >> "$root/state/tags"
  printf '%s\t%s\n' "docker.io/oven/bun:1.3.14" "$BUN" >> "$root/state/tags"
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
check "names the copy" "$(contains "$out" "copy   docker.io/oven/bun:1.3.14@$BUN -> $REGISTRY:bun-1.3.14-1b2c3d4e")"
check "read the row's label first" "$(contains "$out" "label  docker.io/oven/bun:1.3.14 is $BUN, this row's digest")"
check "and the digest it landed at" "$(contains "$out" "copied $REGISTRY:bun-1.3.14-1b2c3d4e $BUN")"
check "counts the row it held" "$(contains "$out" '^1 row(s) already in the package, 1 copied$')"
check "copied that row alone" "$(is "$(cat "$root/state/copied")" "docker.io/oven/bun:1.3.14@$BUN")"
check "and the package now holds its digest" "$(contains "$(cat "$root/state/package")" "$BUN")"

echo "case: a row the package already holds"
root=$(fixture copies-no-held-row)
printf '%s\t%s\n' "$REGISTRY:bun-1.3.14-1b2c3d4e" "$BUN" >> "$root/state/tags"
printf '%s\n' "$BUN" >> "$root/state/package"
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "holds both rows" "$(contains "$out" "held   $REGISTRY:bun-1.3.14-1b2c3d4e $BUN")"
check "copies nothing" "$(is "$(cat "$root/state/copied")" "")"
check "and says so" "$(contains "$out" '^2 row(s) already in the package, 0 copied$')"

echo "case: an upstream tag that has moved since a held row was written"
# The nats row is in the package already, and its upstream tag now points at another digest, as a
# floating upstream tag does between two runs. Nothing copies a held row, so nothing reads its
# label: the comparison below belongs to the copy, which writes its row now.
root=$(fixture upstream-tag-moved)
printf '%s\t%s\n' "docker.io/library/nats:2.10" "$OTHER" >> "$root/state/tags"
printf '%s\t%s\n' "docker.io/library/nats:2.10" "$OTHER" >> "$root/state/upstreams"
run_mirror "$root"
check "exits 0" "$(is "$status" 0)"
check "holds the row whose upstream tag moved" "$(contains "$out" "held   $REGISTRY:nats-2.10-0a1b2c3d $NATS")"
check "asked for the other row's digest" "$(is "$(cat "$root/state/copied")" "docker.io/oven/bun:1.3.14@$BUN")"
check "and the package holds no digest a moved tag names" \
  "$(is "$(contains "$(cat "$root/state/package")" "$OTHER")" false)"

echo "case: an upstream tag that names another digest than the row it labels"
# The row says oven/bun:1.3.14 and its digest holds other bytes. Every gate of this repository
# reads that label and never the bytes, so the copy is the one place the two are compared.
root=$(fixture upstream-tag-disagrees)
printf '%s\t%s\n' "docker.io/oven/bun:1.3.14" "$OTHER" >> "$root/state/tags"
run_mirror "$root"
check "fails" "$(is "$status" 1)"
check "names the upstream and both digests" \
  "$(contains "$out" "docker.io/oven/bun:1.3.14 is $OTHER, and this row's digest is $BUN")"
check "says the label and the bytes disagree" "$(contains "$out" "The row's label and its bytes disagree")"
check "and copies nothing" "$(is "$(cat "$root/state/copied")" "")"

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
held_back=$(PYTHONDONTWRITEBYTECODE=1 python3 - "$repository" "$state" <<'PY'
import sys

import yaml

repository, state = sys.argv[1], sys.argv[2]
document = yaml.safe_load(open(f"{repository}/.github/ci-images.yaml"))
registry, rows = document["registry"], document["images"]
with open(f"{state}/upstreams", "w") as upstreams, open(f"{state}/tags", "w") as tags:
    for row in rows:
        # The row's upstream without the digest it may carry. The script copies from that string
        # at the row's digest, and reads the row's label from the string itself, so the stand-in
        # answers both. A row whose upstream names no tag is never read as a label, and the line
        # that holds it here is a line the script asks for nothing.
        labelled = row["upstream"].partition("@")[0]
        upstreams.write(f"{labelled}@{row['digest']}\t{row['digest']}\n")
        tags.write(f"{labelled}\t{row['digest']}\n")
    with open(f"{state}/package", "w") as package:
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
