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
check "says why" "$(contains "$out" 'jobs.image builds or calls an image, and its if: tests needs.\*.outputs in more than one &&-joined clause')"

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

# buildx_step <root> <run body> [working-directory]: replaces the build-push-action step with a
# run step.
buildx_step() {
  local root=$1 body=$2 workdir=${3:-}
  python3 - "$root/.github/workflows/image.yaml" "$body" "$workdir" <<'PY'
import sys
path, body, workdir = sys.argv[1], sys.argv[2], sys.argv[3]
indented = "\n".join("          " + line for line in body.splitlines())
where = f"        working-directory: {workdir}\n" if workdir else ""
text = open(path).read()
head, _, _ = text.partition("      - uses: docker/build-push-action@v6")
open(path, "w").write(f"{head}      - name: Build\n        run: |\n{indented}\n{where}")
PY
}

echo "case: a run step invoking docker buildx build is an image build"
root=$(fixture buildx-run)
buildx_step "$root" 'docker buildx build --load --tag app:ci --file docker/Dockerfile .'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the run step as a build" "$(contains "$out" 'docker/Dockerfile: checked 5 inputs (7 files) against .github/workflows/image.yaml on.push.paths')"
check "reports one build, not zero" "$(contains "$out" '(1 image build(s))')"

echo "case: a buildx run step's inputs are covered like any other build's"
root=$(fixture buildx-run-uncovered)
buildx_step "$root" 'docker buildx build --load --tag app:ci --file docker/Dockerfile .'
drop_path "$root" "packages/app/**"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the source" "$(contains "$out" 'does not cover packages/app (docker/Dockerfile:3)')"

echo "case: a buildx run step across continuations, with an expression and a second command"
root=$(fixture buildx-run-multiline)
buildx_step "$root" 'docker pull debian:trixie-slim
docker buildx build --load --tag "$IMAGE" \
  --build-arg REVISION=${{ github.sha }} \
  --file docker/Dockerfile .'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "finds exactly the one build" "$(contains "$out" '(1 image build(s))')"

echo "case: a run step this check cannot read fails rather than passing vacuously"
root=$(fixture buildx-run-unknown-flag)
buildx_step "$root" 'docker buildx build --load --squash --file docker/Dockerfile .'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the flag" "$(contains "$out" 'Build names --squash, a flag this check cannot read')"

root=$(fixture buildx-run-no-context)
buildx_step "$root" 'docker buildx build --load --file docker/Dockerfile'
run_check "$root"
check "a build with no context fails" "$(is "$status" 1)"
check "says how many it found" "$(contains "$out" 'names 0 build contexts, and this check needs exactly one')"

echo "case: a run step that mentions docker without building is not a build"
root=$(fixture buildx-run-not-a-build)
buildx_step "$root" 'docker pull debian:trixie-slim'
run_check "$root"
check "fails on zero builds rather than passing" "$(is "$status" 1)"
check "says it proves nothing" "$(contains "$out" 'this check covered 0 image builds')"

echo "case: a comment never swallows a build, in any of the four shapes it can take"
root=$(fixture buildx-run-comment-first)
buildx_step "$root" '# build the image the smoke runs against
docker buildx build --load --file docker/Dockerfile .'
run_check "$root"
check "a leading comment leaves the build visible" "$(is "$status" 0)"
check "and it is counted" "$(contains "$out" '(1 image build(s))')"

root=$(fixture buildx-run-comment-between)
buildx_step "$root" 'docker pull debian:trixie-slim
# now build
docker buildx build --load --file docker/Dockerfile .'
run_check "$root"
check "a comment between commands leaves the build visible" "$(is "$status" 0)"
check "and it is counted" "$(contains "$out" '(1 image build(s))')"

root=$(fixture buildx-run-comment-trailing)
cp "$root/docker/Dockerfile" "$root/docker/Other.Dockerfile"
buildx_step "$root" 'docker buildx build --load --file docker/Dockerfile . # the smoke image
docker buildx build --load --file docker/Other.Dockerfile .'
run_check "$root"
check "a trailing comment does not hide the next command's build" "$(is "$status" 0)"
check "both builds are counted" "$(contains "$out" '(2 image build(s))')"

root=$(fixture buildx-run-comment-heredoc)
buildx_step "$root" "cat <<'EOF' > /tmp/note
# a hash inside a heredoc
EOF
docker buildx build --load --file docker/Dockerfile ."
run_check "$root"
check "a hash inside a heredoc does not hide the build" "$(is "$status" 0)"
check "and it is counted" "$(contains "$out" '(1 image build(s))')"

echo "case: working-directory moves the context and the Dockerfile"
root=$(fixture buildx-run-workdir)
mkdir -p "$root/nested/docker"
echo '{}' > "$root/nested/package.json"
printf 'FROM debian:trixie-slim\nCOPY package.json ./\n' > "$root/nested/docker/Dockerfile"
sed -i 's|      - "docker/\*\*"|&\n      - "nested/**"|' "$root/.github/workflows/image.yaml"
buildx_step "$root" 'docker buildx build --load --file docker/Dockerfile .' nested
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the Dockerfile under working-directory, not the root decoy" "$(contains "$out" 'nested/docker/Dockerfile: checked')"
check "and only that one" "$(contains "$out" '(1 image build(s))')"

root=$(fixture buildx-run-workdir-uncovered)
mkdir -p "$root/nested/docker"
echo '{}' > "$root/nested/package.json"
printf 'FROM debian:trixie-slim\nCOPY package.json ./\n' > "$root/nested/docker/Dockerfile"
buildx_step "$root" 'docker buildx build --load --file docker/Dockerfile .' nested
run_check "$root"
check "an uncovered input under working-directory fails" "$(is "$status" 1)"
check "names the nested file" "$(contains "$out" 'nested/package.json')"

root=$(fixture buildx-run-workdir-variable)
buildx_step "$root" 'docker buildx build --load --file docker/Dockerfile .' '${{ github.workspace }}/sub'
run_check "$root"
check "a working-directory this check cannot resolve fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'working-directory')"

echo "case: a leading assignment or sudo is read, not skipped"
root=$(fixture buildx-run-assignment)
buildx_step "$root" 'DOCKER_BUILDKIT=1 IMAGE=app:ci docker buildx build --load --file docker/Dockerfile .'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the build is counted" "$(contains "$out" '(1 image build(s))')"

root=$(fixture buildx-run-sudo)
buildx_step "$root" 'sudo -E docker buildx build --load --file docker/Dockerfile .'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the build is counted" "$(contains "$out" '(1 image build(s))')"

echo "case: a build in a form this check cannot read fails rather than passing unseen"
root=$(fixture buildx-run-subshell)
buildx_step "$root" '( cd . && docker buildx build --load --file docker/Dockerfile . )'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says it cannot read the invocation" "$(contains "$out" 'a docker build this check cannot read')"

root=$(fixture buildx-run-wrapper)
buildx_step "$root" 'retry docker buildx build --load --file docker/Dockerfile .'
run_check "$root"
check "a wrapped build fails" "$(is "$status" 1)"
check "says it cannot read the invocation" "$(contains "$out" 'a docker build this check cannot read')"

root=$(fixture buildx-run-compose)
buildx_step "$root" 'docker compose build listener'
run_check "$root"
check "docker compose build fails rather than being ignored" "$(is "$status" 1)"
check "says it cannot read the invocation" "$(contains "$out" 'a docker build this check cannot read')"

echo "case: the building job's own if: gate narrows what the trigger covers"
# gate_build_job <root> <filter patterns>: adds a changes job to image.yaml and gates the
# building job on one of its filters, the shape envoy-and-contracts.yaml uses.
gate_build_job() {
  local root=$1 patterns=$2
  python3 - "$root/.github/workflows/image.yaml" "$patterns" <<'PY'
import sys
path, patterns = sys.argv[1], sys.argv[2]
text = open(path).read()
head, marker, tail = text.partition("jobs:\n")
changes = (
    "  changes:\n"
    "    runs-on: ubuntu-24.04\n"
    "    outputs:\n"
    "      gated: ${{ steps.filter.outputs.gated }}\n"
    "    steps:\n"
    "      - uses: dorny/paths-filter@v3\n"
    "        id: filter\n"
    "        with:\n"
    "          filters: |\n"
    "            gated:\n"
    f"{patterns}\n"
)
tail = tail.replace(
    "  docker:\n    runs-on: ubuntu-24.04\n",
    "  docker:\n    needs: changes\n    if: needs.changes.outputs.gated == 'true'\n"
    "    runs-on: ubuntu-24.04\n",
)
open(path, "w").write(head + marker + changes + tail)
PY
}

# The negative control: the build job runs only when an unrelated filter matched, so a commit
# touching the image's own inputs builds nothing. The check must say so.
root=$(fixture gated-build-unrelated)
gate_build_job "$root" "              - 'packages/other/**'"
run_check "$root"
check "a build gated on an unrelated filter fails" "$(is "$status" 1)"
check "names the building job's gate" "$(contains "$out" 'jobs.docker.if (jobs.changes filter gated)')"
check "names an input the gate drops" "$(contains "$out" 'does not cover docker/Dockerfile')"

root=$(fixture gated-build-complete)
gate_build_job "$root" "              - 'package.json'
              - 'packages/app/**'
              - 'docker/**'
              - '.dockerignore'
              - '.github/workflows/image.yaml'"
run_check "$root"
check "a gate listing every input passes" "$(is "$status" 0)"
check "names the gate it read" "$(contains "$out" 'jobs.docker.if (jobs.changes filter gated)')"

root=$(fixture gated-build-partial)
gate_build_job "$root" "              - 'package.json'
              - 'packages/app/**'
              - 'docker/**'
              - '.github/workflows/image.yaml'"
run_check "$root"
check "a gate missing one input fails, though the trigger covers it" "$(is "$status" 1)"
check "names that input" "$(contains "$out" 'does not cover .dockerignore')"


summary "check-image-trigger-paths.sh"
