#!/usr/bin/env bash
# Builds one Dispatch build from a commit of this repository into a directory: the server, its web
# app, its `dispatch` command, and the test data the two-build test (cmd/dispatch/twobuild_*_test.go,
# `go test -tags twobuild`) reads from it. It builds from `git archive` of the commit, so it makes
# no worktree, branch or checkout. The server and its web app take the steps the commit's image
# build takes (packages/envoy/docker/Dockerfile): `bun install --frozen-lockfile`, the contracts'
# Go codegen, `bun run build:web`, and `go build` of ./cmd/dispatch with `-X main.buildCommit`.
# The image does not build the `dispatch` command: the script builds the contracts bundle, then
# runs the commit's own build-dispatch-cli.sh. `go build` runs with `GOTOOLCHAIN=auto`, so a
# commit that needs another Go toolchain gets it.
#
#   packages/envoy/scripts/build-dispatch-build.sh --commit <sha|tag> --out <dir>
#   packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>
#   packages/envoy/scripts/build-dispatch-build.sh --commit <sha|tag> --resolve
#   packages/envoy/scripts/build-dispatch-build.sh --previous-release --resolve
#
# --previous-release builds the newest legion-envoy-v* tag that is an ancestor of
# `git merge-base HEAD origin/main`: the release production ran before this branch. --commit HEAD
# builds the current branch the same way. Put <dir> under $TMPDIR. <dir> must be absent or empty,
# or hold a complete build of the same commit, which the script then leaves as it is. --resolve
# builds nothing: it makes the same refusals and prints the full sha it would build, so a caller
# can key a cache of <dir> by it before the build (CI's dispatch-two-builds job).
#
# <dir> then holds:
#   src/                              the commit's archive, where the parts were built
#   envoy-dispatch                    the server, built with -X main.buildCommit=<sha>
#   web/                              the web app (packages/dispatch/web/dist), DISPATCH_WEB_DIST
#   dispatch.js                       the `dispatch` command bundle, run as `bun dispatch.js`
#   pmdoc/testdata/corpus/            the commit's document corpus
#   pmdoc/schema/blocks.json          the commit's block manifest (its schema version)
#   text/testdata/                    the commit's reference-form fixtures
#   COMMIT                            the full sha, written last: a directory with it is complete
#
# The refusals: a commit that is not an ancestor of HEAD, and a commit whose migrations stop below
# 0117. Two builds share one database only from 0117 on (cmd/dispatch/README.md, "Database
# migrations"), so an older build cannot be the previous side of the test.
set -euo pipefail

usage() {
  printf 'usage: %s (--commit <sha|tag> | --previous-release) (--out <dir> | --resolve)\n' "$0" >&2
  exit 2
}

fail() {
  printf 'build-dispatch-build: %s\n' "$*" >&2
  exit 1
}

note() {
  printf 'build-dispatch-build: %s\n' "$*" >&2
}

commit=
previous_release=
resolve=
out=
while (($# > 0)); do
  case "$1" in
  --commit)
    (($# >= 2)) || usage
    commit=$2
    shift 2
    ;;
  --previous-release)
    previous_release=1
    shift
    ;;
  --resolve)
    resolve=1
    shift
    ;;
  --out)
    (($# >= 2)) || usage
    out=$2
    shift 2
    ;;
  *) usage ;;
  esac
done
[[ -n "$out" && -z "$resolve" || -z "$out" && -n "$resolve" ]] || usage
[[ -n "$commit" && -z "$previous_release" || -z "$commit" && -n "$previous_release" ]] || usage

required=(git)
[[ -n "$resolve" ]] || required+=(tar go bun)
for command in "${required[@]}"; do
  command -v "$command" >/dev/null || fail "the command $command is necessary. Install it and run this script again."
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)
readonly root
readonly migrations=packages/envoy/internal/dispatch/store/migrations
readonly floor=117

if [[ -n "$previous_release" ]]; then
  git -C "$root" rev-parse --verify --quiet origin/main >/dev/null ||
    fail "this repository has no origin/main. Fetch origin/main and its tags, then run this script again."
  base=$(git -C "$root" merge-base HEAD origin/main) || fail "HEAD and origin/main have no merge base."
  mapfile -t tags < <(git -C "$root" tag --list 'legion-envoy-v*' --sort=-v:refname --merged "$base")
  ((${#tags[@]} > 0)) ||
    fail "no legion-envoy-v* tag is an ancestor of the merge base $base of HEAD and origin/main. Fetch the tags, then run this script again."
  commit=${tags[0]}
  note "the previous release is $commit (the newest legion-envoy-v* tag in the history of $base)"
fi

sha=$(git -C "$root" rev-parse --verify --quiet "$commit^{commit}") ||
  fail "$commit is not a commit of this repository."
git -C "$root" merge-base --is-ancestor "$sha" HEAD ||
  fail "$commit ($sha) is not an ancestor of HEAD. Give a commit in the history of HEAD."

newest=0
while IFS= read -r file; do
  name=${file##*/}
  [[ "$name" =~ ^([0-9]{4})_.*\.up\.sql$ ]] || continue
  number=$((10#${BASH_REMATCH[1]}))
  ((number > newest)) && newest=$number
done < <(git -C "$root" ls-tree --name-only "$sha" "$migrations/")
((newest > 0)) || fail "$commit ($sha) has no Dispatch migrations in $migrations. The two-build test needs migration $(printf '%04d' "$floor") or later in the earlier build."
((newest >= floor)) ||
  fail "$commit ($sha) has migrations up to $(printf '%04d' "$newest") only. The two-build test needs migration $(printf '%04d' "$floor") or later in the earlier build."

if [[ -n "$resolve" ]]; then
  printf '%s\n' "$sha"
  exit 0
fi

mkdir -p -- "$out"
out=$(cd -- "$out" && pwd -P)
readonly out
if [[ -f "$out/COMMIT" && "$(<"$out/COMMIT")" == "$sha" ]]; then
  note "$out already holds the build of $sha. It stays as it is."
  exit 0
fi
[[ -z "$(ls -A -- "$out")" ]] ||
  fail "$out is not empty and holds no complete build of $sha. Remove it, or give an empty directory."

note "building $commit ($sha) in $out/src"
mkdir -- "$out/src"
git -C "$root" archive "$sha" | tar -x -C "$out/src" || fail "the archive of $sha did not extract into $out/src."
readonly src=$out/src

# The JavaScript parts: the install, the Go contracts the server compiles (regenerated, never the
# copy in the tree) and the contracts bundle, then the web app and the command. The install, the
# codegen and the web app are in the order the commit's image build makes them.
(cd -- "$src" && bun install --frozen-lockfile) || fail "bun install --frozen-lockfile failed in the archive of $sha."
(cd -- "$src/packages/contracts" && bun run gen:go && bun run build) ||
  fail "the contracts build (gen:go, build) of $sha failed."
(cd -- "$src/packages/dispatch" && bun run build:web) || fail "the web build (bun run build:web) of $sha failed."
[[ -f "$src/packages/dispatch/web/dist/index.html" ]] || fail "the web build of $sha wrote no web/dist/index.html."
cp -R -- "$src/packages/dispatch/web/dist" "$out/web"
cli_script=$src/packages/envoy-client/scripts/build-dispatch-cli.sh
[[ -f "$cli_script" ]] ||
  fail "$sha has no packages/envoy-client/scripts/build-dispatch-cli.sh. This script builds the dispatch command only with that script."
bash "$cli_script" "$out" || fail "the dispatch command bundle of $sha failed to build."

(cd -- "$src/packages/envoy" &&
  GOTOOLCHAIN=auto go build -ldflags "-X main.buildCommit=$sha" -o "$out/envoy-dispatch" ./cmd/dispatch) ||
  fail "go build ./cmd/dispatch of $sha failed."

readonly dispatch_dir=$src/packages/envoy/internal/dispatch
mkdir -p -- "$out/pmdoc/testdata" "$out/pmdoc/schema" "$out/text"
cp -R -- "$dispatch_dir/pmdoc/testdata/corpus" "$out/pmdoc/testdata/corpus"
cp -- "$dispatch_dir/pmdoc/schema/blocks.json" "$out/pmdoc/schema/blocks.json"
cp -R -- "$dispatch_dir/text/testdata" "$out/text/testdata"

printf '%s\n' "$sha" >"$out/COMMIT"
note "built $commit ($sha) in $out"
