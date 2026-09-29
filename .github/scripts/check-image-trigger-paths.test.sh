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

# The calling job is gated by the jobs it needs as well as by its own if:, the shape
# release.yaml's worker_image has (needs: [changes, cli], cli itself gated). Its always()
# is what breaks the inheritance there; without one the narrow gate binds.
root=$(fixture called-transitive-gate)
callers "$root" "$all_inputs"
python3 - "$root/.github/workflows/release.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
text = text.replace(
    "  image:\n    needs: changes\n",
    "  gatekeeper:\n    needs: changes\n"
    "    if: needs.changes.outputs.other == 'true'\n"
    "    runs-on: ubuntu-24.04\n"
    "    steps:\n      - run: echo gate\n"
    "  image:\n    needs: [changes, gatekeeper]\n",
)
open(path, "w").write(text)
PYEOF
run_check "$root"
check "a gate the calling job inherits through needs: fails" "$(is "$status" 1)"
check "names the job it came from" "$(contains "$out" 'jobs.gatekeeper.if (jobs.changes filter other)')"

sed -i "s|^    if: needs.changes.outputs.other == 'true'$|    if: always() \&\& needs.changes.outputs.other == 'true'|" \
  "$root/.github/workflows/release.yaml"
run_check "$root"
check "and still fails, since always() on the NEEDED job does not break inheritance" "$(is "$status" 1)"

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

# run_step <root> <run body>: replaces the build-push-action step with a run step, leaving the
# workflow with no build this check can read.
run_step() {
  local root=$1 body=$2
  python3 - "$root/.github/workflows/image.yaml" "$body" <<'PYEOF'
import sys
path, body = sys.argv[1], sys.argv[2]
indented = "\n".join("          " + line for line in body.splitlines())
text = open(path).read()
head, _, _ = text.partition("      - uses: docker/build-push-action@v6")
open(path, "w").write(f"{head}      - name: Build\n        run: |\n{indented}\n")
PYEOF
}

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

echo "case: a gate reaches the build however it is written"
# gate_probe <root> <docker job header> [extra job yaml]: a changes job exposing img (covers
# every input) and narrow (covers docker/** only), with the build job's header supplied.
gate_probe() {
  local root=$1 header=$2 extra=${3:-}
  python3 - "$root/.github/workflows/image.yaml" "$header" "$extra" <<'PY'
import sys
path, header, extra = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
head, marker, tail = text.partition("jobs:\n")
changes = (
    "  changes:\n"
    "    runs-on: ubuntu-24.04\n"
    "    outputs:\n"
    "      img: ${{ steps.filter.outputs.img }}\n"
    "      narrow: ${{ steps.filter.outputs.narrow }}\n"
    "    steps:\n"
    "      - uses: dorny/paths-filter@v3\n"
    "        id: filter\n"
    "        with:\n"
    "          filters: |\n"
    "            img:\n"
    "              - 'package.json'\n"
    "              - 'packages/app/**'\n"
    "              - 'docker/**'\n"
    "              - '.dockerignore'\n"
    "              - '.github/workflows/image.yaml'\n"
    "            narrow:\n"
    "              - 'docker/**'\n"
)
tail = tail.replace("  docker:\n    runs-on: ubuntu-24.04\n", header)
open(path, "w").write(head + marker + changes + (extra + "\n" if extra else "") + tail)
PY
}
gatekeeper="  gatekeeper:
    needs: changes
    if: needs.changes.outputs.narrow == 'true'
    runs-on: ubuntu-24.04
    steps:
      - run: echo gate"

# Controls, so a green below means the gate was read rather than the harness misfiring.
root=$(fixture gate-control-complete)
gate_probe "$root" "  docker:
    needs: changes
    if: needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
"
run_check "$root"
check "a complete gate on the builder passes" "$(is "$status" 0)"
check "and is named" "$(contains "$out" 'jobs.docker.if (jobs.changes filter img)')"

root=$(fixture gate-control-narrow)
gate_probe "$root" "  docker:
    needs: changes
    if: needs.changes.outputs.narrow == 'true'
    runs-on: ubuntu-24.04
"
run_check "$root"
check "a narrow gate on the builder fails" "$(is "$status" 1)"
check "names an input it drops" "$(contains "$out" 'does not cover package.json')"

# (a) GitHub skips a job whose needed job was skipped, so a gate reaches the build through
# `needs:` even when the build job carries no `if:` of its own.
root=$(fixture gate-transitive-needs)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "a gate inherited through needs: fails" "$(is "$status" 1)"
check "names the job it was inherited from" "$(contains "$out" 'jobs.gatekeeper.if (jobs.changes filter narrow)')"

# Two jobs' gates must both hold, so they AND; filters inside one if: are ||-joined and widen.
root=$(fixture gate-own-plus-transitive)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "a complete own gate does not excuse a narrow inherited one" "$(is "$status" 1)"

# (b) A top-level && term naming a github. context narrows the gate on something that is not a
# path, so a trigger this check pairs it with may never reach the job.
root=$(fixture gate-and-event-term)
gate_probe "$root" "  docker:
    needs: changes
    if: needs.changes.outputs.img == 'true' && github.event_name != 'merge_group'
    runs-on: ubuntu-24.04
"
run_check "$root"
check "an && term on a github. context is refused" "$(is "$status" 1)"
check "quotes the term" "$(contains "$out" "is narrowed by \`github.event_name != 'merge_group'\`")"

# (c) An if: naming no needs.*.outputs is a gate this check cannot evaluate, not an absent one.
root=$(fixture gate-no-needs-if)
gate_probe "$root" "  docker:
    needs: changes
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
"
run_check "$root"
check "an if: with no paths-filter output is refused" "$(is "$status" 1)"
check "quotes the term" "$(contains "$out" "is narrowed by \`github.event_name == 'push'\`")"

# always() does not narrow by path, so it stays ungated.
root=$(fixture gate-always)
gate_probe "$root" "  docker:
    needs: changes
    if: always()
    runs-on: ubuntu-24.04
"
run_check "$root"
check "always() is read as ungated" "$(is "$status" 0)"

echo "case: an if: this check does not model is refused, never read as ungated"
shape_index=0
for shape in "false" "vars.RUN_IT == 'true'" "needs.changes.outputs.img == 'true' && vars.RUN_IT" "needs.changes.outputs.img == 'true' && false"; do
  shape_index=$((shape_index + 1))
  root=$(fixture "gate-unmodelled-$shape_index")
  gate_probe "$root" "  docker:
    needs: changes
    if: $shape
    runs-on: ubuntu-24.04
"
  run_check "$root"
  check "if: $shape is refused" "$(is "$status" 1)"
  check "  and quotes the term" "$(contains "$out" 'which this check cannot')"
done

echo "case: the terms that do not narrow by path stay accepted"
shape_index=0
# `!cancelled()` needs quoting: bare, YAML reads `!cancelled` as a tag.
for shape in "success()" '"!cancelled()"' "always() && needs.changes.outputs.img == 'true'" "(needs.changes.result == 'success' || needs.changes.result == 'skipped') && needs.changes.outputs.img == 'true'"; do
  shape_index=$((shape_index + 1))
  root=$(fixture "gate-neutral-$shape_index")
  gate_probe "$root" "  docker:
    needs: changes
    if: $shape
    runs-on: ubuntu-24.04
"
  run_check "$root"
  check "if: $shape passes" "$(is "$status" 0)"
done

echo "case: needs.<job>.result is neutral only when it admits 'skipped'"
# The live shape this guards: envoy-smoke tidied up to reuse envoy-go's setup. always() does not
# free the build from envoy-go's gate, because `result == 'success'` re-imposes it — GitHub
# skips the smoke on a change that skips envoy-go, which is #1562's regression exactly.
root=$(fixture gate-result-requires-success)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: always() && needs.gatekeeper.result == 'success' && needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "result == 'success' pulls in the named job's gate, even past always()" "$(is "$status" 1)"
check "names the job it came from" "$(contains "$out" 'jobs.gatekeeper.if (jobs.changes filter narrow)')"

root=$(fixture gate-result-admits-skipped)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: always() && needs.gatekeeper.result != 'failure' && needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "result != 'failure' admits skipped, so it stays neutral" "$(is "$status" 0)"

root=$(fixture gate-result-or-skipped)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: always() && (needs.gatekeeper.result == 'success' || needs.gatekeeper.result == 'skipped') && needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "an || that includes 'skipped' stays neutral" "$(is "$status" 0)"

root=$(fixture gate-result-requires-success-bang-cancelled)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: \"!cancelled() && needs.gatekeeper.result == 'success' && needs.changes.outputs.img == 'true'\"
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "the same, past !cancelled() rather than always()" "$(is "$status" 1)"

echo "case: a build step's own if: is a gate too, and is allowlisted the same way"
# step_if <root> <expression>: names and gates the build-push-action step itself.
step_if() {
  local root=$1 expression=$2
  python3 - "$root/.github/workflows/image.yaml" "$expression" <<'PYEOF'
import sys
path, expression = sys.argv[1], sys.argv[2]
text = open(path).read()
marker = "      - uses: docker/build-push-action@v6\n"
replacement = "      - name: Build image\n        uses: docker/build-push-action@v6\n" + f"        if: {expression}\n"
open(path, "w").write(text.replace(marker, replacement))
PYEOF
}
unchanged="  docker:
    runs-on: ubuntu-24.04
"

root=$(fixture step-gate-complete)
gate_probe "$root" "$unchanged"
step_if "$root" "needs.changes.outputs.img == 'true'"
run_check "$root"
check "a complete gate on the step passes" "$(is "$status" 0)"
check "and is named" "$(contains "$out" "jobs.docker step 'Build image' if (jobs.changes filter img)")"

root=$(fixture step-gate-narrow)
gate_probe "$root" "$unchanged"
step_if "$root" "needs.changes.outputs.narrow == 'true'"
run_check "$root"
check "a narrow gate on the step fails" "$(is "$status" 1)"
check "names an input it drops" "$(contains "$out" 'does not cover package.json')"

root=$(fixture step-gate-unreadable)
gate_probe "$root" "$unchanged"
step_if "$root" "github.event_name == 'push'"
run_check "$root"
check "a step gate this check cannot evaluate is refused" "$(is "$status" 1)"
check "quotes the term" "$(contains "$out" "is narrowed by \`github.event_name == 'push'\`")"

root=$(fixture step-gate-vars)
gate_probe "$root" "$unchanged"
step_if "$root" "vars.RUN_IT"
run_check "$root"
check "a step gate on vars is refused too" "$(is "$status" 1)"

root=$(fixture step-gate-false)
gate_probe "$root" "$unchanged"
step_if "$root" "false"
run_check "$root"
check "a step gate of false is refused too" "$(is "$status" 1)"

echo "case: a run step that builds an image is refused, whatever shape it takes"
while IFS='|' read -r label body; do
  [ -n "$label" ] || continue
  root=$(fixture "tripwire-$label")
  run_step "$root" "$body"
  run_check "$root"
  check "$label is refused" "$(is "$status" 1)"
  check "  and says how to make it readable" "$(contains "$out" 'build images with docker/build-push-action so this check can read the build')"
done <<'SHAPES'
plain|docker build -f docker/Dockerfile .
buildx|docker buildx build --load -f docker/Dockerfile .
V1-abbreviated|docker buildx b --load -f docker/Dockerfile .
V2-bake|docker buildx bake --load listener
V3-compose-up|docker compose up --build -d
V4-compose-build|docker-compose build listener
eval|eval "docker build -f docker/Dockerfile ."
github-env|echo "IMG=$(docker buildx build --load -q -f docker/Dockerfile .)" >> $GITHUB_ENV
buildah|buildah bud -f docker/Dockerfile .
B1-image-build|docker image build -f docker/Dockerfile .
B2-builder-build|docker builder build -f docker/Dockerfile .
B3-context-flag|docker --context remote build -f docker/Dockerfile .
B4-buildx-builder-flag|docker buildx --builder mybuilder build -f docker/Dockerfile .
podman|podman build -f docker/Dockerfile .
B5-host-flag|docker -H tcp://x:2375 build -f docker/Dockerfile .
nerdctl|nerdctl build -f docker/Dockerfile .
depot|depot build -f docker/Dockerfile .
kaniko|/kaniko/executor --dockerfile docker/Dockerfile --context .
SHAPES

# V5: an action that builds an image and is not the one this check reads.
root=$(fixture tripwire-bake-action)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
head, _, _ = text.partition("      - uses: docker/build-push-action@v6")
open(path, "w").write(f"{head}      - name: Bake\n        uses: docker/bake-action@v5\n")
PYEOF
run_check "$root"
check "V5 docker/bake-action is refused" "$(is "$status" 1)"
check "  and names the list to add it to" "$(contains "$out" 'add this action to NON_BUILDING_ACTIONS')"

echo "case: the build action's with: keys are an allowlist too"
# The oracle's repro: build-contexts names another tree the build reads, so dropping that tree
# from the filter would otherwise pass.
root=$(fixture with-build-contexts)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text.replace(
    "          file: docker/Dockerfile\n",
    "          file: docker/Dockerfile\n          build-contexts: other=packages/other\n",
))
PYEOF
run_check "$root"
check "build-contexts is refused" "$(is "$status" 1)"
check "  and says what to do" "$(contains "$out" 'an input this check has not read')"

root=$(fixture with-secret-files)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text.replace(
    "          file: docker/Dockerfile\n",
    "          file: docker/Dockerfile\n          secret-files: tok=./tok.txt\n",
))
PYEOF
run_check "$root"
check "secret-files is refused" "$(is "$status" 1)"

root=$(fixture with-known-inputs)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text.replace(
    "          file: docker/Dockerfile\n",
    "          file: docker/Dockerfile\n          push: true\n          platforms: linux/amd64\n"
    "          cache-from: type=gha\n          secrets: |\n            tok=abc\n",
))
PYEOF
run_check "$root"
check "the inputs this check has read stay green" "$(is "$status" 0)"

root=$(fixture with-expression-context)
sed -i 's|          context: .$|          context: ${{ github.workspace }}|' "$root/.github/workflows/image.yaml"
run_check "$root"
check "a context that is not a plain repository path is refused" "$(is "$status" 1)"
check "  and says so" "$(contains "$out" 'not a plain repository path')"

echo "case: uses: is an allowlist, since a build action cannot be spotted by name"
for other in "depot/build-push-action@v1" "mr-smithers-excellent/docker-build-push@v6"; do
  root=$(fixture "uses-$(printf '%s' "$other" | tr -cd '[:alnum:]' | cut -c1-20)")
  python3 - "$root/.github/workflows/image.yaml" "$other" <<'PYEOF'
import sys
path, other = sys.argv[1], sys.argv[2]
text = open(path).read()
head, _, _ = text.partition("      - uses: docker/build-push-action@v6")
open(path, "w").write(f"{head}      - name: Other\n        uses: {other}\n")
PYEOF
  run_check "$root"
  check "$other is refused" "$(is "$status" 1)"
  check "  and names the list to add it to" "$(contains "$out" 'add this action to NON_BUILDING_ACTIONS')"
done

root=$(fixture uses-known-action)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text.replace(
    "      - uses: docker/build-push-action@v6\n",
    "      - uses: actions/checkout@v5\n      - uses: docker/build-push-action@v6\n",
))
PYEOF
run_check "$root"
check "an action known to build nothing passes" "$(is "$status" 0)"

echo "case: a build split across a backslash continuation is still caught"
root=$(fixture tripwire-continuation)
run_step "$root" 'docker \
  buildx build --load -f docker/Dockerfile .'
run_check "$root"
check "the continuation form is refused" "$(is "$status" 1)"
check "  and says how to make it readable" "$(contains "$out" 'build images with docker/build-push-action so this check can read the build')"

root=$(fixture tripwire-not-a-build)
run_step "$root" 'docker pull debian:trixie-slim'
run_check "$root"
check "a run step that does not build is not flagged" "$(contains "$out" 'this check covered 0 image builds')"


summary "check-image-trigger-paths.sh"
