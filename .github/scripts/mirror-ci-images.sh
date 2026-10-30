#!/usr/bin/env bash
# Copies every image .github/ci-images.yaml names into the package that file's `registry:` names,
# so no CI job fetches an image from a third-party registry (dispatch://INFRA-567). `crane copy`
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
# THE LABEL CHECK, AT THE COPY ALONE. A row this run is about to copy, whose upstream names a
# tag, is read once more: `crane digest <that upstream with its tag>`. The run fails when the
# answer is not the row's digest, because the row's label and the row's bytes then disagree.
# Every other gate reads the label alone — which Bun or Go a row carries is the upstream text
# of that row (.github/scripts/check-ci-images.sh rules 3 and 7,
# .github/scripts/check-bun-version.sh) — so a row that was labelled wrongly passes them all.
# This read belongs to the copy and to nothing else: the row being copied was written now,
# while a floating upstream tag moves, so the same read over a row the package already holds
# would fail on a tag that moved after an honest copy. A row whose upstream names a digest
# alone carries no label to compare, and this run says so and copies it.
#
# It prints one line per row and one summary line. It copies with `crane` from PATH
# (.github/actions/install-crane installs it) under a login that may write the package.
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

# PYTHONDONTWRITEBYTECODE keeps the yaml import from leaving a __pycache__ in the tree.
rows=$(PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import yaml

document = yaml.safe_load(open(".github/ci-images.yaml"))
registry = document["registry"]
for row in document["images"]:
    # The row's upstream without the digest it may carry, and that same string at the row's
    # digest, which is what the copy asks for: crane drops a tag written before the `@`.
    labelled = row["upstream"].partition("@")[0]
    source = f"{labelled}@{row['digest']}"
    # The reference the label check reads, empty where the upstream names no tag. A tag is in
    # the last path element of a reference, after a colon.
    tagged = labelled if ":" in labelled.rpartition("/")[2] else ""
    print("\t".join((registry, row["tag"], row["digest"], source, tagged)))
PY
)
if [ -z "$rows" ]; then
  echo "::error file=.github/ci-images.yaml::this file holds no row, so there is nothing to copy" >&2
  exit 1
fi

held=0
copied=0
while IFS=$'\t' read -r registry tag digest source tagged; do
  have=$(crane digest "$registry:$tag" 2>/dev/null || true)
  if [ "$have" = "$digest" ]; then
    echo "held   $registry:$tag $digest"
    held=$((held + 1))
    continue
  fi
  if [ -n "$tagged" ]; then
    labelled=$(crane digest "$tagged")
    if [ "$labelled" != "$digest" ]; then
      echo "::error file=.github/ci-images.yaml::$tagged is $labelled, and this row's digest is $digest. The row's label and its bytes disagree, and every other gate reads the label: it is the row's upstream text that says which version this image is. Read that upstream again, then write the digest its tag names now, or label this row with the reference its digest belongs to." >&2
      exit 1
    fi
    echo "label  $tagged is $digest, this row's digest"
  else
    echo "label  $source names no tag, so nothing compares this row's label with its bytes"
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
