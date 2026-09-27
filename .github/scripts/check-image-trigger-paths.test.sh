#!/usr/bin/env bash
# Tests for `check-image-trigger-paths.sh`: every trigger that builds an image must cover every
# file the build reads, and the check must fail loud on a build it cannot read.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-image-trigger-paths.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-image-trigger-paths.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — an image whose Dockerfile copies a manifest, a whole
# package, another stage's output and a URL, a .dockerignore that drops the package's dist, and a
# workflow whose push paths cover all of it. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/docker" \
    "$root/packages/app/src/deep" "$root/packages/app/dist" "$root/packages/other"
  ln -s "$check_script" "$root/.github/scripts/check-image-trigger-paths.sh"
  echo '{}' > "$root/package.json"
  echo 'export {};' > "$root/packages/app/src/index.ts"
  echo 'export {};' > "$root/packages/app/src/deep/nested.ts"
  echo '{}' > "$root/packages/app/package.json"
  echo 'built' > "$root/packages/app/dist/out.js"
  echo 'unrelated' > "$root/packages/other/readme.md"
  printf '**/dist\n' > "$root/.dockerignore"
  cat > "$root/docker/Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14 AS web
COPY package.json ./
COPY --chown=1000:1000 packages/app packages/app
ADD https://example.com/tool.tar.gz /tmp/
FROM debian:trixie-slim
COPY --from=web /repo/out /srv
DOCKERFILE
  cat > "$root/.github/workflows/image.yaml" <<'YAML'
name: Image
on:
  push:
    branches: [main]
    paths:
      - "package.json"
      - "packages/app/**"
      - "docker/**"
      - ".dockerignore"
      - ".github/workflows/image.yaml"
  workflow_dispatch:
jobs:
  docker:
    runs-on: ubuntu-24.04
    steps:
      - uses: docker/build-push-action@v6
        with:
          context: .
          file: docker/Dockerfile
YAML
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-image-trigger-paths.sh" 2>&1)
  status=$?
  set -e
}

# drop_path <root> <pattern>: removes one path filter line from the workflow.
drop_path() {
  local root=$1 pattern=$2
  grep -vF -- "- \"$pattern\"" "$root/.github/workflows/image.yaml" > "$root/tmp.yaml"
  mv "$root/tmp.yaml" "$root/.github/workflows/image.yaml"
}

echo "case: every input is covered"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it checked" "$(contains "$out" 'docker/Dockerfile: checked 5 inputs (7 files) against .github/workflows/image.yaml on.push.paths')"

echo "case: a file the .dockerignore drops needs no filter, and one it keeps does"
root=$(fixture dockerignore-applied)
sed -i 's|      - "packages/app/\*\*"|      - "packages/app/package.json"\n      - "packages/app/src/**"|' \
  "$root/.github/workflows/image.yaml"
run_check "$root"
check "passes only because **/dist drops dist/out.js" "$(is "$status" 0)"
: > "$root/.dockerignore"
run_check "$root"
check "fails once the .dockerignore keeps dist" "$(is "$status" 1)"
check "names the file" "$(contains "$out" 'does not cover packages/app (docker/Dockerfile:3): packages/app/dist/out.js is built')"

echo "case: a path filter removed"
root=$(fixture dropped-source)
drop_path "$root" "packages/app/**"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the source, its line and an uncovered file" "$(contains "$out" 'does not cover packages/app (docker/Dockerfile:3): packages/app/[a-z/.]* and 2 more are built')"

echo "case: a new COPY source without a filter"
root=$(fixture new-source)
echo 'COPY packages/other /other' >> "$root/docker/Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the new source" "$(contains "$out" 'does not cover packages/other (docker/Dockerfile:7): packages/other/readme.md is built')"

echo "case: a directory copied whole but filtered in part"
root=$(fixture partial)
sed -i 's|"packages/app/\*\*"|"packages/app/package.json"|' "$root/.github/workflows/image.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names a file outside the partial filter" "$(contains "$out" 'packages/app/src/[a-z/]*\.ts and 1 more are built')"

echo "case: * stays inside one directory"
root=$(fixture single-star)
sed -i 's|      - "packages/app/\*\*"|      - "packages/app/*"\n      - "packages/app/src/*"|' \
  "$root/.github/workflows/image.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names only the file one level further down" "$(contains "$out" 'packages/app/src/deep/nested.ts is built')"

echo "case: the Dockerfile, the .dockerignore and the workflow are inputs too"
for input in docker/** .dockerignore .github/workflows/image.yaml; do
  root=$(fixture "self-$(echo "$input" | tr '/.*' '---')")
  drop_path "$root" "$input"
  run_check "$root"
  check "leaving out $input fails" "$(is "$status" 1)"
done
check "and names it" "$(contains "$out" 'does not cover .github/workflows/image.yaml')"

echo "case: a negated path filter that drops an input"
root=$(fixture negated)
sed -i 's|      - "packages/app/\*\*"|&\n      - "!packages/app/src/**"|' "$root/.github/workflows/image.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names a negated file" "$(contains "$out" 'packages/app/src/')"

echo "case: an event without a path filter covers everything"
root=$(fixture unfiltered)
cat > "$root/.github/workflows/image.yaml" <<'YAML'
name: Image
on:
  push:
    branches: [main]
jobs:
  docker:
    runs-on: ubuntu-24.04
    steps:
      - uses: docker/build-push-action@v6
        with:
          file: docker/Dockerfile
YAML
run_check "$root"
check "passes" "$(is "$status" 0)"

echo "case: paths-ignore naming an input"
root=$(fixture ignored)
sed -i '/^    paths:$/,/^  workflow_dispatch:$/{/^  workflow_dispatch:$/!d}' "$root/.github/workflows/image.yaml"
sed -i 's|^    branches: \[main\]$|&\n    paths-ignore:\n      - "packages/app/src/**"|' \
  "$root/.github/workflows/image.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the trigger" "$(contains "$out" 'on.push.paths-ignore does not cover packages/app')"

echo "case: a GitHub path pattern using ?, + or \\ is refused, not guessed"
# GitHub reads `package.json?` as package.jso or package.json; picomatch as package.json plus one
# character. The check refuses GitHub's form rather than read it either way.
root=$(fixture github-question)
sed -i 's|      - "package.json"|      - "package.json?"|' "$root/.github/workflows/image.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the pattern" "$(contains "$out" "on.push.paths has package.json?: this check does not read GitHub's ?, + or")"

echo "case: a RUN bind mount reads the context"
root=$(fixture bind-mount)
echo 'RUN --mount=type=bind,source=packages/other,target=/o cat /o/readme.md' >> "$root/docker/Dockerfile"
echo 'RUN --mount=type=secret,id=token true' >> "$root/docker/Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the mounted source" "$(contains "$out" 'does not cover packages/other (docker/Dockerfile:7)')"
check "a secret mount is not a source" "$(is "$(contains "$out" 'Dockerfile:8')" false)"

echo "case: a source the check cannot resolve fails instead of reading nothing"
root=$(fixture variable)
# shellcheck disable=SC2016 # a literal Dockerfile variable, no expansion wanted
echo 'COPY ${SRC} /src' >> "$root/docker/Dockerfile"
run_check "$root"
check "a variable fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'names a variable')"

root=$(fixture missing-source)
echo 'COPY packages/gone /gone' >> "$root/docker/Dockerfile"
run_check "$root"
check "a source that matches nothing fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'packages/gone matches no file in the context')"

root=$(fixture no-build)
sed -i 's|docker/build-push-action@v6|actions/checkout@v5|' "$root/.github/workflows/image.yaml"
run_check "$root"
check "no image build at all fails" "$(is "$status" 1)"
check "says it proves nothing" "$(contains "$out" 'proves nothing')"

echo "case: a called workflow is covered by its caller's trigger and the filters gating the call"
# callers <root> <gate filter lines>: turns image.yaml into a called workflow and adds a caller
# whose push paths cover everything the image reads and whose call is gated on one filter.
callers() {
  local root=$1 gate=$2
  sed -i 's|^  workflow_dispatch:$|  workflow_call:|; /^  push:$/,/^  workflow_call:$/{/^  workflow_call:$/!d}' \
    "$root/.github/workflows/image.yaml"
  cat > "$root/.github/workflows/release.yaml" <<YAML
name: Release
on:
  push:
    branches: [main]
    paths:
      - "**"
jobs:
  changes:
    runs-on: ubuntu-24.04
    outputs:
      image: \${{ steps.filter.outputs.image }}
      other: \${{ steps.filter.outputs.other }}
    steps:
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            image:
$gate
            other:
              - 'packages/other/**'
  image:
    needs: changes
    if: needs.changes.outputs.image == 'true' || needs.changes.outputs.other == 'true'
    uses: ./.github/workflows/image.yaml
YAML
}
all_inputs="              - 'package.json'
              - 'packages/app/**'
              - 'docker/**'
              - '.dockerignore'
              - '.github/workflows/image.yaml'"

root=$(fixture called-green)
callers "$root" "$all_inputs"
run_check "$root"
check "a gate covering every input passes" "$(is "$status" 0)"
check "names the caller and its gate" "$(contains "$out" 'release.yaml on.push.paths + jobs.image.if (jobs.changes filter image or jobs.changes filter other)')"

root=$(fixture called-gate-gap)
callers "$root" "$(printf '%s\n' "$all_inputs" | grep -v '.dockerignore')"
run_check "$root"
check "a gate missing an input fails, though the caller's push covers it" "$(is "$status" 1)"
check "names the input" "$(contains "$out" 'jobs.changes filter other) does not cover .dockerignore')"

root=$(fixture called-unreadable-gate)
callers "$root" "$all_inputs"
sed -i "s|needs.changes.outputs.other == 'true'|needs.changes.outputs.manual == 'true'|" \
  "$root/.github/workflows/release.yaml"
run_check "$root"
check "a gate on an output that is no paths filter fails, rather than reading as ungated" "$(is "$status" 1)"
check "names the output" "$(contains "$out" 'gated on needs.changes.outputs.manual, which is not a dorny/paths-filter filter')"

echo "case: the gate shape release.yaml writes is read"
root=$(fixture called-release-shape)
callers "$root" "$all_inputs"
python3 - "$root/.github/workflows/release.yaml" <<'PY'
import sys
path = sys.argv[1]
text = open(path).read().replace(
    "    if: needs.changes.outputs.image == 'true' || needs.changes.outputs.other == 'true'\n",
    "    if: >-\n"
    "      always() && (needs.changes.result == 'success' || needs.changes.result == 'skipped') &&\n"
    "      (needs.changes.outputs.image == 'true' || needs.changes.outputs.other == 'true' ||\n"
    "      github.event_name == 'workflow_dispatch')\n",
)
open(path, "w").write(text)
PY
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names both filters" "$(contains "$out" 'jobs.image.if (jobs.changes filter image or jobs.changes filter other)')"

echo "case: a gate in any other shape fails, rather than being read as ||"
root=$(fixture called-and)
callers "$root" "$all_inputs"
sed -i "s#    if: .*#    if: (needs.changes.outputs.image == 'true') \&\& (needs.changes.outputs.other == 'true')#" \
  "$root/.github/workflows/release.yaml"
run_check "$root"
check "outputs in two &&-joined clauses fail" "$(is "$status" 1)"
check "says why" "$(contains "$out" 'jobs.image calls an image workflow, and its if: tests needs.\*.outputs in more than one &&-joined clause')"

root=$(fixture called-not-true)
callers "$root" "$all_inputs"
sed -i "s#needs.changes.outputs.other == 'true'#needs.changes.outputs.other != 'false'#" \
  "$root/.github/workflows/release.yaml"
run_check "$root"
check "an output compared with anything but == 'true' fails" "$(is "$status" 1)"
check "quotes the term" "$(contains "$out" "tests an output as \`needs.changes.outputs.other != 'false'\`")"

root=$(fixture called-every)
callers "$root" "$all_inputs"
sed -i "s#^        with:\$#&\n          predicate-quantifier: 'every'#" "$root/.github/workflows/release.yaml"
run_check "$root"
check "predicate-quantifier: every fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'predicate-quantifier: every, which this check cannot evaluate')"

echo "case: a paths-filter pattern keeps picomatch's ?"
root=$(fixture called-dorny-question)
callers "$root" "$(printf '%s\n' "$all_inputs" | sed "s|'package.json'|'package.jso?'|")"
run_check "$root"
check "package.jso? covers package.json in a dorny filter" "$(is "$status" 0)"

summary "check-image-trigger-paths.sh"
