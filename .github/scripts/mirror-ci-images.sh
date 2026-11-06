#!/usr/bin/env bash
# Copies every image .github/ci-images.yaml names into the package that file's `registry:` names,
# so no CI job fetches an image from a third-party registry (sjawhar/legion#2512). `crane copy`
# keeps the manifest digest, so a copy's digest is its upstream's digest, and every consumer names
# that one string.
#
# Per row: `crane digest <registry>:<tag>` reads the digest the tag points at now. A tag that
# already points at the row's digest is held, and nothing is copied, so the common run copies
# nothing and takes seconds. Any other answer is copied with
# `crane copy <the row's upstream>@<the row's digest> <registry>:<tag>`, and the same read runs
# again. crane parses that source with go-containerregistry's `name.NewDigest`, which drops a
# tag written before the `@`, so the source names the row's repository at the row's digest
# whatever tag the row's upstream carries. The copy never names the row's upstream tag alone:
# an upstream tag floats, and two runs that read it on different days copy different bytes
# under one mirror tag. A copy that lands at another digest fails, naming the row: a consumer
# pins the digest the row says, and the package now holds other bytes under that tag.
#
# NO READ OF AN UPSTREAM TAG CAN STAND HERE. An upstream tag floats: the bytes
# docker.io/library/nats:2.15.1-alpine names this month are not the bytes it named last month.
# A run that read a row's upstream tag, and refused the row when the answer was not the row's
# digest, could not tell a row written now with a wrong label from a row written long ago with
# a right one. It would fail the second one on every run until a person rewrote the row. The
# two paragraphs below bind a row's label to its bytes, and neither reads a tag.
#
# THE GO CHECK, ON EVERY TAGGED LIBRARY/GOLANG ROW. Everywhere else, which Go a row carries is the
# text of that row's upstream tag (.github/scripts/check-ci-images.sh rules 3 and 7), so a row
# labelled with one Go and holding the digest of another passes every other gate. library/golang
# writes its own version into the image config, so the bytes themselves say which Go they are:
# `crane config --platform linux/amd64 <a reference at the row's digest>` answers a config whose
# `.config.Env` holds GOLANG_VERSION. This run reads it for every library/golang row whose
# upstream names a tag, held or copied, and fails when it is not the version that tag names. A
# library/golang row whose upstream names a digest alone carries no version to compare, so this
# run skips it; rule 7 of check-ci-images.sh refuses any reference on such a row, and rule 3 asks
# for a tagged row at the pin. Where it reads it
# is wherever those bytes are: `crane copy` keeps the manifest digest, so the package's copy and
# the upstream are the same bytes under the same digest, and the answer is the same from either.
# A row the package holds is read in the package, `<registry>@<the row's digest>`, so a run that
# copies nothing — the common one — reads no registry but the package this repository publishes.
# A row this run copies is read at `<the row's upstream>@<the row's digest>` before the copy, so
# bytes whose label is wrong never land in the package; that read is a read of the upstream, as
# the copy beside it is. The read asks for the row's digest and for no tag, so its answer is the
# same on every day. No read of a tag could say as much.
#
# THE BUN SIDE. An oven/bun image declares its version in no part of its config, so no read of
# this kind can hold a Bun row to its label. The builds that run those bytes hold them to the
# pin instead, and both copy .bun-version into the stage: the plugin stage of
# packages/daemon/docker/worker.Dockerfile, whose build script refuses any Bun but the one that
# file pins, and the web stage of packages/envoy/docker/Dockerfile, which refuses a
# `bun --version` that is not that file's content.
#
# It prints one line for each row the package already holds, two for each row it copies (the
# copy it starts, then the digest that copy landed at), one more before either for each
# library/golang row (the reference it read and the Go those bytes carry), and one summary line
# at the end. It copies with `crane` from PATH (.github/actions/install-crane installs it)
# under a login that may write the package.
#
# Run from anywhere: .github/scripts/mirror-ci-images.sh
# CI runs it in ci.yaml's ci-images job, before the package mode of
# .github/scripts/check-ci-images.sh reads the package in that same job. That job is the one
# copy path. Its tests are in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

if ! command -v crane >/dev/null 2>&1; then
  echo "::error file=.github/scripts/mirror-ci-images.sh::crane is not on PATH, and this script copies with it. Run .github/actions/install-crane first." >&2
  exit 1
fi

# The reference grammar is .github/scripts/ci_images.py's, beside this script in the repository
# it copies for, which PYTHONPATH names: the gate reads a row's upstream with the same two
# functions, so a row it counts as the library/golang row is a row this script checks.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
rows=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 - <<'PY'
import yaml

from ci_images import upstream_parts

GOLANG = "library/golang"

document = yaml.safe_load(open(".github/ci-images.yaml"))
registry = document["registry"]
for row in document["images"]:
    # The row's upstream without the digest it may carry, and that same string at the row's
    # digest, which is what the copy asks for: crane drops a tag written before the `@`.
    labelled = row["upstream"].partition("@")[0]
    source = f"{labelled}@{row['digest']}"
    # The Go version this row's label claims, empty where the row copies another image or
    # names no tag. The Go a `1.27.1-alpine` tag names is the part before the first `-`, which
    # is how rule 7 of check-ci-images.sh reads the same tag.
    _, repository, tag, _ = upstream_parts(row["upstream"])
    claimed_go = tag.partition("-")[0] if repository == GOLANG else ""
    print("\t".join((registry, row["tag"], row["digest"], source, claimed_go)))
PY
)
if [ -z "$rows" ]; then
  echo "::error file=.github/ci-images.yaml::this file holds no row, so there is nothing to copy" >&2
  exit 1
fi

# image_go <reference>: the GOLANG_VERSION of that image's config, which library/golang's own
# build writes there. Prints nothing where the config carries no such entry.
image_go() {
  crane config --platform linux/amd64 "$1" | PYTHONDONTWRITEBYTECODE=1 python3 -c '
import json
import sys

for entry in (json.load(sys.stdin).get("config") or {}).get("Env") or []:
    name, separator, value = entry.partition("=")
    if separator and name == "GOLANG_VERSION":
        print(value)
'
}

# check_go <reference> <the Go the row's upstream tag names> <the row's upstream>: holds the
# bytes at <reference> to the row's label, and fails naming the row where they carry another Go.
# <reference> holds the row's digest, in the package or at the upstream, and the message names
# it, so a refusal says which copy of those bytes was read.
check_go() {
  local reference=$1 claimed_go=$2 upstream=$3 carried_go carries
  carried_go=$(image_go "$reference")
  if [ "$carried_go" != "$claimed_go" ]; then
    carries="Go $carried_go"
    [ -n "$carried_go" ] || carries="no GOLANG_VERSION, which every library/golang image declares"
    echo "::error file=.github/ci-images.yaml::$reference carries $carries, and this row's upstream tag names Go $claimed_go. The row's label and its bytes disagree, and every other gate reads the label: it is the row's upstream text that says which Go this image is. That read named $reference, which holds this row's digest. Write the digest $upstream names now, or label this row with the version its digest carries." >&2
    exit 1
  fi
  echo "go     $reference carries Go $carried_go, the version its upstream tag names"
}

held=0
copied=0
while IFS=$'\t' read -r registry tag digest source claimed_go; do
  have=$(crane digest "$registry:$tag" 2>/dev/null || true)
  if [ "$have" = "$digest" ]; then
    # The Go check on the package's own copy: `crane copy` kept the manifest digest, so those
    # are this row's bytes under this row's digest, and a run that copies nothing — the common
    # one — reads no registry but the package.
    if [ -n "$claimed_go" ]; then
      check_go "$registry@$digest" "$claimed_go" "${source%@*}"
    fi
    echo "held   $registry:$tag $digest"
    held=$((held + 1))
    continue
  fi
  # The same check at the upstream the copy below reads, and before it: bytes whose label is
  # wrong never land in the package. The copy reads that registry anyway.
  if [ -n "$claimed_go" ]; then
    check_go "$source" "$claimed_go" "${source%@*}"
  fi
  echo "copy   $source -> $registry:$tag"
  crane copy "$source" "$registry:$tag"
  have=$(crane digest "$registry:$tag")
  if [ "$have" != "$digest" ]; then
    echo "::error file=.github/ci-images.yaml::$registry:$tag landed at $have, and the row says $digest. This copy read $source, so the bytes it copied are the row's: another writer moved this tag after the copy. Read the package for what else writes this tag, then re-run the ci-images job." >&2
    exit 1
  fi
  echo "copied $registry:$tag $digest"
  copied=$((copied + 1))
done <<<"$rows"
echo "$held row(s) already in the package, $copied copied"
