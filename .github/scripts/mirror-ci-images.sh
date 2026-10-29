#!/usr/bin/env bash
# Copies every image .github/ci-images.yaml names into the package that file's `registry:` names,
# so no CI job fetches an image from a third-party registry (dispatch://INFRA-567). `crane copy`
# keeps the manifest digest, so a copy's digest is its upstream's digest, and every consumer names
# that one string.
#
# Per row: `crane digest <registry>:<tag>` reads the digest the tag points at now. A tag that
# already points at the row's digest is held, and nothing is copied, so the common run copies
# nothing and takes seconds. Any other answer is copied with `crane copy <upstream>
# <registry>:<tag>`, and the same read runs again. A copy that lands at another digest fails,
# naming the row: a consumer pins the digest the row says, and the package now holds other bytes
# under that tag.
#
# It prints one line per row and one summary line. It copies with `crane` from PATH
# (.github/actions/install-crane installs it) under a login that may write the package.
#
# Run from anywhere: .github/scripts/mirror-ci-images.sh
# CI runs it in ci.yaml's ci-images job, before .github/scripts/check-ci-images.sh reads the
# package in that same job, and in mirror-images.yaml on a push to main. Its tests are in
# ci.yaml's test job.
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
    print("\t".join((registry, row["tag"], row["digest"], row["upstream"])))
PY
)
if [ -z "$rows" ]; then
  echo "::error file=.github/ci-images.yaml::this file holds no row, so there is nothing to copy" >&2
  exit 1
fi

held=0
copied=0
while IFS=$'\t' read -r registry tag digest upstream; do
  have=$(crane digest "$registry:$tag" 2>/dev/null || true)
  if [ "$have" = "$digest" ]; then
    echo "held   $registry:$tag $digest"
    held=$((held + 1))
    continue
  fi
  echo "copy   $upstream -> $registry:$tag"
  crane copy "$upstream" "$registry:$tag"
  have=$(crane digest "$registry:$tag")
  if [ "$have" != "$digest" ]; then
    echo "::error file=.github/ci-images.yaml::$registry:$tag landed at $have, and the row says $digest. Correct the row's digest, or the upstream reference it copies from." >&2
    exit 1
  fi
  echo "copied $registry:$tag $digest"
  copied=$((copied + 1))
done <<<"$rows"
echo "$held row(s) already in the package, $copied copied"
