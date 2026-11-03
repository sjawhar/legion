#!/usr/bin/env bash
# Tests for `repo-paths.sh`: the seven paths it derives, under today's name and under the name the
# repository moves to, and each value it refuses.
#
# The point of the script is that a repository rename is one environment value, so the two whole
# tables are written out here as literals rather than derived: a case that recomputed the values
# the way the script does would pass whatever either of them spelled. The mixed-case owner is the
# one case where two keys disagree on purpose - npm matches a package's repository.url against the
# publishing repository case-sensitively, while GitHub Pages serves a lowercased owner - and the
# ${VERSION} in plugin_download_url is Pulumi's placeholder, which must survive this script
# uninterpolated, so every expectation naming it is single-quoted here too.
#
# Each case runs the real script as a subprocess with GITHUB_REPOSITORY set (or unset) for that
# case, so what is tested is what a workflow step gets.
#
# Run from anywhere: .github/scripts/repo-paths.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_paths="$script_dir/repo-paths.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# paths <repository> [key]: runs the script under test with GITHUB_REPOSITORY set to <repository>,
# or with it removed from the environment when <repository> is the word `unset`. Leaves its stdout
# in `out`, its stderr in `err` and its exit code in `rc`. Not a subshell, so all three survive.
paths() {
  local repository="$1"
  shift
  set +e
  if [ "$repository" = unset ]; then
    out=$(env -u GITHUB_REPOSITORY "$repo_paths" "$@" 2> "$work/stderr")
  else
    out=$(GITHUB_REPOSITORY="$repository" "$repo_paths" "$@" 2> "$work/stderr")
  fi
  rc=$?
  set -e
  err=$(cat "$work/stderr")
}

# value_of <key>: the value `out`'s `key=value` line carries, empty when there is no such line.
value_of() {
  local line
  line=$(grep -m1 -- "^$1=" <<< "$out") || true
  printf '%s' "${line#*=}"
}

echo "case: today's repository, sjawhar/legion"
paths sjawhar/legion
check "exits 0" "$(is "$rc" 0)"
check "worker_image" "$(is "$(value_of worker_image)" 'ghcr.io/sjawhar/legion-worker')"
check "envoy_image" "$(is "$(value_of envoy_image)" 'ghcr.io/sjawhar/legion/envoy')"
# shellcheck disable=SC2016 # literal ${VERSION}: Pulumi substitutes it at install time
check "plugin_download_url" \
  "$(is "$(value_of plugin_download_url)" 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}')"
check "repository_url" "$(is "$(value_of repository_url)" 'https://github.com/sjawhar/legion')"
check "docs_site" "$(is "$(value_of docs_site)" 'https://sjawhar.github.io')"
check "docs_base" "$(is "$(value_of docs_base)" '/legion')"
check "docs_edit_base" \
  "$(is "$(value_of docs_edit_base)" 'https://github.com/sjawhar/legion/edit/main/docs/site/')"
check "seven keys, no more" "$(is "$(grep -c '' <<< "$out")" 7)"
sjawhar_out="$out"

echo
echo "case: the name the repository moves to, cybermonklabs/legion"
paths cybermonklabs/legion
check "exits 0" "$(is "$rc" 0)"
check "worker_image" "$(is "$(value_of worker_image)" 'ghcr.io/cybermonklabs/legion-worker')"
check "envoy_image" "$(is "$(value_of envoy_image)" 'ghcr.io/cybermonklabs/legion/envoy')"
# shellcheck disable=SC2016 # literal ${VERSION}: Pulumi substitutes it at install time
check "plugin_download_url" \
  "$(is "$(value_of plugin_download_url)" 'https://github.com/cybermonklabs/legion/releases/download/envoy-infra-v${VERSION}')"
check "repository_url" "$(is "$(value_of repository_url)" 'https://github.com/cybermonklabs/legion')"
check "docs_site" "$(is "$(value_of docs_site)" 'https://cybermonklabs.github.io')"
check "docs_base" "$(is "$(value_of docs_base)" '/legion')"
check "docs_edit_base" \
  "$(is "$(value_of docs_edit_base)" 'https://github.com/cybermonklabs/legion/edit/main/docs/site/')"
check "seven keys, no more" "$(is "$(grep -c '' <<< "$out")" 7)"

echo
echo "case: an owner GitHub spells with capitals"
paths CyberMonkLabs/legion
check "exits 0" "$(is "$rc" 0)"
check "repository_url keeps the owner's case, which npm matches case-sensitively" \
  "$(is "$(value_of repository_url)" 'https://github.com/CyberMonkLabs/legion')"
check "docs_site lowercases it, which is what GitHub Pages serves" \
  "$(is "$(value_of docs_site)" 'https://cybermonklabs.github.io')"
check "worker_image lowercases nothing" \
  "$(is "$(value_of worker_image)" 'ghcr.io/CyberMonkLabs/legion-worker')"

echo
echo "case: one key asked for is that value alone"
paths sjawhar/legion docs_base
check "exits 0" "$(is "$rc" 0)"
check "the value, with no key= prefix" "$(is "$out" '/legion')"
check "one line" "$(is "$(grep -c '' <<< "$out")" 1)"

# shellcheck disable=SC2016 # literal ${VERSION}: Pulumi substitutes it at install time
expected_download='https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}'
paths sjawhar/legion plugin_download_url
check "the Pulumi placeholder reaches the caller uninterpolated" "$(is "$out" "$expected_download")"

echo
echo "case: GITHUB_REPOSITORY unset, as it is off a runner"
paths unset
check "exits 0" "$(is "$rc" 0)"
check "gives today's sjawhar/legion table" "$(is "$out" "$sjawhar_out")"

echo
echo "case: a value that is not <owner>/<name> is refused"
for bad in legion a/b/c /legion 'sjawhar legion'; do
  paths "$bad"
  check "'$bad' exits non-zero" "$(is "$rc" 1)"
  check "'$bad' prints nothing on stdout" "$(is "$out" '')"
  check "'$bad' names the value on stderr" "$(contains "$err" "GITHUB_REPOSITORY is '$bad'")"
done

echo
echo "case: an argument that is not a key is refused"
paths sjawhar/legion no_such_key
check "exits non-zero" "$(is "$rc" 1)"
check "prints nothing on stdout" "$(is "$out" '')"
check "names the key" "$(contains "$err" "unknown key 'no_such_key'")"
check "lists the known keys" "$(contains "$err" 'known keys: worker_image envoy_image')"

paths sjawhar/legion docs_base docs_site
check "two keys at once exit non-zero" "$(is "$rc" 1)"
check "printing nothing on stdout" "$(is "$out" '')"
check "and saying how many it takes" "$(contains "$err" 'takes one key or none, got 2')"

summary "repo-paths.sh"
