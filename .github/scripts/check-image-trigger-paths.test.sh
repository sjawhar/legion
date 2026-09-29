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
indented = "\n".join("          " + line for line in body.replace("\\n", "\n").splitlines())
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

root=$(fixture tripwire-not-a-build)
run_step "$root" 'docker pull debian:trixie-slim'
run_check "$root"
check "a run step that does not build is not flagged" "$(contains "$out" 'this check covered 0 image builds')"

# extra_step <root> <run body>: appends a run step to the fixture's docker job, leaving the
# readable build-push-action build in place. A literal \n in the body is a newline.
extra_step() {
  local root=$1 body=$2
  python3 - "$root/.github/workflows/image.yaml" "$body" <<'PYEOF'
import sys
path, body = sys.argv[1], sys.argv[2]
indented = "\n".join("          " + line for line in body.replace("\\n", "\n").splitlines())
text = open(path).read()
open(path, "w").write(text + f"      - name: Extra\n        run: |\n{indented}\n")
PYEOF
}

# step_id <root> <step name> <id>: gives a named step an `id:`, which is how an entry addresses
# it - a name is neither required nor unique, so it is not a key.
step_id() {
  python3 - "$1/.github/workflows/image.yaml" "$2" "$3" <<'PYEOF'
import sys
path, name, identifier = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
marker = f"      - name: {name}\n"
assert marker in text
open(path, "w").write(text.replace(marker, marker + f"        id: {identifier}\n", 1))
PYEOF
}

# allowlist <root> <step id> <hits> <reason>: replaces the symlinked checker with a copy whose
# NOT_A_BUILD_STEPS names one step of the fixture's docker job, pinned to <hits> - the reviewed
# edit a refusal asks for. Editing the script is the point: the hatch is data in a reviewed
# file, not something a step can claim for itself.
allowlist() {
  local root=$1
  rm -f "$root/.github/scripts/check-image-trigger-paths.sh"
  python3 - "$check_script" "$root/.github/scripts/check-image-trigger-paths.sh" "$2" "$3" "$4" <<'PYEOF'
import sys
source, destination, identifier, hits, reason = sys.argv[1:6]
text = open(source).read()
anchor = "NOT_A_BUILD_STEPS: list[tuple[str, str, str, tuple[str, ...], str]] = [\n"
entry = (
    f"    ('.github/workflows/image.yaml', 'docker', {identifier!r}, ({hits},), {reason!r}),\n"
)
assert anchor in text
open(destination, "w").write(text.replace(anchor, anchor + entry, 1))
PYEOF
  chmod +x "$root/.github/scripts/check-image-trigger-paths.sh"
}

echo "case: every build shape is refused, and only a listed step is excused"
# A table, not single literals: every row is a real invocation, asserting the refusal, that the
# step is named and that the matched text is printed, so no row can pass on the zero-build
# guard. `\n` in a body is a newline: a build under a wrapper, or after an inspecting command,
# is the shape that hid an escape hatch in every pattern-based veto this replaced.
while IFS='|' read -r label certain body; do
  [ -n "$label" ] || continue
  root=$(fixture "build-$label")
  run_step "$root" "$body"
  run_check "$root"
  check "build shape $label is refused" "$(is "$status" 1)"
  check "  and names the step" "$(contains "$out" "Build builds an image in a run step")"
  check "  and prints the whole command" "$(contains "$out" "builds an image in a run step ('")"
  if [ "$certain" = "yes" ]; then
    check "  and the list is not offered for a certain build" "$(contains "$out" "This shape cannot be listed in NOT_A_BUILD_STEPS")"
  else
    check "  and the last remedy asks for an id and an entry" "$(contains "$out" "give the step an \`id:\` and add")"
  fi
done <<'BUILDS'
docker-build-f-docker|yes|docker build -f docker/Dockerfile .
docker-buildx-build-load|yes|docker buildx build --load -f docker/Dockerfile .
docker-buildx-b-load|yes|docker buildx b --load -f docker/Dockerfile .
docker-buildx-b|yes|docker buildx b .
docker-buildx-b-t|yes|docker buildx b -t x .
docker-buildx-b-2|yes|docker  buildx   b .
docker-buildx-bake-load|yes|docker buildx bake --load listener
docker-image-build-f|yes|docker image build -f docker/Dockerfile .
docker-builder-build-f|yes|docker builder build -f docker/Dockerfile .
docker-context-remote-build|yes|docker --context remote build -f docker/Dockerfile .
docker-h-tcp-x|yes|docker -H tcp://x:2375 build -f docker/Dockerfile .
docker-buildx-builder-mybuilder|yes|docker buildx --builder mybuilder build -f docker/Dockerfile .
sudo-docker-build-f|yes|sudo docker build -f docker/Dockerfile .
docker-buildkit-1-docker|yes|DOCKER_BUILDKIT=1 docker build -f docker/Dockerfile .
img-docker-buildx-build|yes|img=$(docker buildx build --load -q -f docker/Dockerfile .)
img-docker-buildx-build-2|yes|img=`docker buildx build --load -q -f docker/Dockerfile .`
echo-img-docker-buildx|yes|echo "IMG=$(docker buildx build --load -q -f docker/Dockerfile .)" >> $GITHUB_ENV
eval-docker-build-f|yes|eval "docker build -f docker/Dockerfile ."
bash-c-docker-buildx|yes|bash -c "docker buildx build --load -f docker/Dockerfile ."
docker-buildx-build-load-2|yes|(docker buildx build --load -f docker/Dockerfile .)
docker-compose-up-build|yes|docker compose up --build -d
docker-compose-build-listener|yes|docker-compose build listener
podman-compose-up-build|yes|podman compose up --build -d
podman-build-f-docker|yes|podman build -f docker/Dockerfile .
nerdctl-build-f-docker|yes|nerdctl build -f docker/Dockerfile .
depot-build-f-docker|yes|depot build -f docker/Dockerfile .
buildah-bud-f-docker|yes|buildah bud -f docker/Dockerfile .
buildctl-build-frontend-dockerfile|yes|buildctl build --frontend dockerfile.v0
ko-build-cmd-listener|yes|ko build ./cmd/listener
pack-build-app-path|yes|pack build app --path .
skaffold-build-file-output|yes|skaffold build --file-output out.json
earthly-docker|yes|earthly +docker
crane-append-f-layer|yes|crane append -f layer.tar -t app:ci
mvn-compile-jib-dockerbuild|yes|mvn compile jib:dockerBuild
docker-build-f-docker-2|yes|"$DOCKER" build -f docker/Dockerfile .
kaniko-executor-dockerfile-docker|yes|/kaniko/executor --dockerfile docker/Dockerfile --context .
docker-build-f-docker-3|yes|"docker" build -f docker/Dockerfile .
which-docker-build-f|yes|$(which docker) build -f docker/Dockerfile .
docker-docker-build-f|yes|${DOCKER:-docker} build -f docker/Dockerfile .
docker-docker-build-f-2|yes|"${DOCKER:-docker}" build -f docker/Dockerfile .
docker-config-tmp-a|yes|docker --config "/tmp/a b" build -f docker/Dockerfile .
docker-context-env-x|yes|docker --context ${{ env.X }} build -f docker/Dockerfile .
docker-buildx-builder-steps|yes|docker buildx --builder ${{ steps.buildx.outputs.name }} build -f docker/Dockerfile .
nerdctl-compose-build|yes|nerdctl compose build
nerdctl-compose-up-build|yes|nerdctl compose up --build
depot-bake-listener|yes|depot bake listener
buildah-from-scratch-buildah|no|buildah from scratch && buildah commit working-container app:ci
buildctl-addr-tcp-x|yes|buildctl --addr tcp://x:1234 build --frontend dockerfile.v0
buildctl-daemonless-sh-build|yes|buildctl-daemonless.sh build --frontend dockerfile.v0
docker-buildx-build-f|yes|docker-buildx build -f docker/Dockerfile .
buildx-build-f-docker|yes|buildx build -f docker/Dockerfile .
img-build-f-docker|yes|img build -f docker/Dockerfile .
docker-commit-working-app|no|docker commit working app:ci
docker-import-rootfs-tar|no|docker import rootfs.tar app:ci
ko-resolve-f-config|yes|ko resolve -f config/
ko-apply-f-config|yes|ko apply -f config/
skaffold-run|yes|skaffold run
docker-compose-f-compose|yes|docker compose -f compose/listener.compose.yml up -d
docker-compose-f-compose-2|yes|docker compose -f compose/listener.compose.yml run app
docker-compose-f-compose-3|yes|docker compose -f compose/listener.compose.yml create
docker-compose-f-compose-4|yes|docker compose -f compose/listener.compose.yml watch
ko-verbose-build-cmd|yes|ko --verbose build ./cmd/listener
pack-quiet-build-app|yes|pack --quiet build app --path .
skaffold-default-repo-r|yes|skaffold --default-repo=r build
img-debug-build-f|yes|img --debug build -f docker/Dockerfile .
docker-build-f-docker-4|yes|docker "build" -f docker/Dockerfile .
finch-build-f-docker|yes|finch build -f docker/Dockerfile .
gradlew-jibdockerbuild|yes|./gradlew jibDockerBuild
df-h-then-docker|yes|df -h\ndocker buildx build --load -f docker/Dockerfile .
docker-version-then-docker|yes|docker --version\ndocker buildx build --load -f docker/Dockerfile .
sha256sum-check-sums-txt|yes|sha256sum --check sums.txt\ndocker build -f docker/Dockerfile .
kubectl-apply-dry-run|yes|kubectl apply --dry-run=client -f k8s/\ndocker build -f docker/Dockerfile .
helm-template-help-then|yes|helm template --help\ndocker build -f docker/Dockerfile .
docker-buildx-bake-print|yes|docker buildx bake --print\ndocker buildx build --load -f docker/Dockerfile .
docker-build-f-docker-5|yes|docker build -f docker/Dockerfile . && df -h
timeout-900-docker-build|yes|timeout 900 docker build -f docker/Dockerfile .
time-docker-build-f|yes|time docker build -f docker/Dockerfile .
nice-n-10-docker|yes|nice -n 10 docker build -f docker/Dockerfile .
exec-docker-build-f|yes|exec docker build -f docker/Dockerfile .
sudo-e-docker-build|yes|sudo -E docker build -f docker/Dockerfile .
command-docker-build-f|yes|command docker build -f docker/Dockerfile .
env-foo-1-docker|yes|env FOO=1 docker build -f docker/Dockerfile .
usr-bin-docker-build|yes|/usr/bin/docker build -f docker/Dockerfile .
if-docker-build-f|yes|if ! docker build -f docker/Dockerfile .; then exit 1; fi
for-t-in-a|yes|for t in a b; do docker build -f docker/Dockerfile . ; done
until-docker-build-f|yes|until docker build -f docker/Dockerfile .; do sleep 1; done
docker-build-f-docker-6|yes|{ docker build -f docker/Dockerfile . ; }
sh-euc-docker-build|yes|sh -euc "docker build -f docker/Dockerfile ."
sleep-5-then-docker|yes|sleep 5\ndocker build -f docker/Dockerfile .
echo-run-docker-build|yes|echo run docker build now
docker-compose-up-d|yes|docker compose up -d\n# we pass --no-build in CI
docker-compose-f-x|yes|docker compose -f x.yml run --rm --build app
docker-then-buildx-build|yes|docker \\\n  buildx build --load -f docker/Dockerfile .
docker-buildx-build-load-3|yes|docker buildx build --load -f docker/Dockerfile . # --help
podman-compose-up|yes|podman-compose up
docker-debug-build-f|yes|docker --debug build -f docker/Dockerfile .
docker-d-buildx-build|yes|docker -D buildx build --load -f docker/Dockerfile .
buildctl-addr-tcp-h|yes|buildctl --addr tcp://h:1234 --debug build --frontend dockerfile.v0
docker-context-ci-compose|yes|docker --context ci compose build
docker-h-ssh-builder|yes|docker -H ssh://builder compose build
podman-log-level-debug|yes|podman --log-level debug compose build
nerdctl-namespace-k8s-io|yes|nerdctl --namespace k8s.io compose build
docker-context-ci-compose-2|yes|docker --context ci compose run --build app true
docker-context-ci-compose-3|yes|docker --context ci compose up -d --build
docker-buildx-build-call|yes|docker buildx build --call build --load -f docker/Dockerfile .
docker-context-ci-compose-4|yes|docker --context ci compose up -d
docker-config-cat-f|yes|docker --config "$(cat f | head -1)" build -t x .
docker-build-build-arg|yes|docker build --build-arg NODE_VERSION=$(node --version | tr -d v) -f docker/Dockerfile .
docker-build-build-arg-2|yes|docker build --build-arg BUN_VERSION="$(bun --version | cut -d. -f1-2)" -f docker/Dockerfile .
docker-buildx-build-f-2|yes|docker "buildx" build -f docker/Dockerfile .
docker-image-build-f-2|yes|docker "image" build -f docker/Dockerfile .
podman-url-build-example|yes|podman --url build.example:8888 build -f docker/Dockerfile .
docker-compose-f-build|yes|docker compose -f build-compose.yml up -d
docker-compose-p-run|yes|docker compose -p run-tests up -d
docker-compose-profile-up|yes|docker compose --profile up-stack up -d
docker-compose-project-directory|yes|docker compose --project-directory=${{ env.COMPOSE_DIR }} -f x.yml up -d
docker-build-f-docker-7|yes|docker build -f docker/Dockerfile .\ndocker buildx bake --load listener
BUILDS

# The continuation form cannot go in the table: its body spans two lines.
root=$(fixture build-continuation)
run_step "$root" 'docker \
  buildx build --load -f docker/Dockerfile .'
run_check "$root"
check "build shape continuation is refused" "$(is "$status" 1)"
check "  and names the step" "$(contains "$out" "Build builds an image in a run step")"

echo "case: a step that runs a build word without building is refused until it is listed"
# The detector is broad on purpose, so a step that runs a build word without building an image
# is refused too - and the only way out is its own entry in NOT_A_BUILD_STEPS, which is data a
# reviewer reads, not a pattern anyone can spell their way into.
while IFS='|' read -r label hits body; do
  [ -n "$label" ] || continue
  root=$(fixture "fp-$label")
  extra_step "$root" "$body"
  run_check "$root"
  check "non-build $label is refused unlisted" "$(is "$status" 1)"
  check "  and prints the matched text" "$(contains "$out" "builds an image in a run step ($hits)")"
  root=$(fixture "fp-listed-$label")
  extra_step "$root" "$body"
  step_id "$root" "Extra" "not-a-build"
  allowlist "$root" "not-a-build" "$hits" "runs a build word without building an image"
  run_check "$root"
  check "  and goes green once listed" "$(is "$status" 0)"
  check "  and the excusal names the id, hits and reason" "$(contains "$out" "job docker: step not-a-build: $hits - runs a build word without building an image")"
done <<'FALSEPOSITIVES'
docker-h-build-example|'docker -H build.example.com:2375 pull alpine'|docker -H build.example.com:2375 pull alpine
docker-config-build-docker|'docker --config build/.docker pull alpine'|docker --config build/.docker pull alpine
docker-context-build-pull|'docker --context build pull alpine'|docker --context build pull alpine
docker-h-build-corp|'docker -H build.corp pull alpine'|docker -H build.corp pull alpine
docker-run-rm-golang|'docker run --rm golang go build ./...'|docker run --rm golang go build ./...
docker-exec-app-make|'docker exec app make build'|docker exec app make build
docker-run-alpine-echo|'docker run alpine echo a b'|docker run alpine echo a b
docker-run-v-pwd|'docker run -v $PWD:/w -w /w golang go build ./...'|docker run -v "$PWD":/w -w /w golang go build ./...
docker-compose-run-no|'docker compose run --no-build app ./test.sh --build'|docker compose run --no-build app ./test.sh --build
FALSEPOSITIVES

echo "case: a false positive the anchored layer calls certain cannot be listed at all"
while IFS='|' read -r label hits body; do
  [ -n "$label" ] || continue
  root=$(fixture "unlistable-$label")
  extra_step "$root" "$body"
  run_check "$root"
  check "certain shape $label is refused" "$(is "$status" 1)"
  check "  and the list is not offered" "$(contains "$out" "This shape cannot be listed in NOT_A_BUILD_STEPS")"
  root=$(fixture "unlistable-listed-$label")
  extra_step "$root" "$body"
  step_id "$root" "Extra" "not-a-build"
  allowlist "$root" "not-a-build" "$hits" "the reviewer thought it built nothing"
  run_check "$root"
  check "  and an entry for it is refused" "$(is "$status" 1)"
  check "  saying it cannot be listed" "$(contains "$out" "cannot be:")"
done <<'UNLISTABLE'
docker-buildx-bake-print-2|'docker buildx bake --print'|docker buildx bake --print
docker-buildx-build-check|'docker buildx build --check .'|docker buildx build --check .
docker-buildx-build-help|'docker buildx build --help'|docker buildx build --help
docker-buildx-build-call-2|'docker buildx build --call=check .'|docker buildx build --call=check .
docker-buildx-bake-list|'docker buildx bake --list=targets'|docker buildx bake --list=targets
docker-compose-build-dry|'docker compose build --dry-run'|docker compose build --dry-run
earthly-test|'earthly +test'|earthly +test
UNLISTABLE

echo "case: a command the detector does not see needs no entry"
while IFS='|' read -r label body; do
  [ -n "$label" ] || continue
  root=$(fixture "unseen-$label")
  extra_step "$root" "$body"
  run_check "$root"
  check "non-build $label needs no entry" "$(is "$status" 0)"
  check "  and nothing is excused" "$(is "$(printf '%s' "$out" | grep -c 'listed in NOT_A_BUILD_STEPS')" 0)"
done <<'UNSEEN'
earthly-version|earthly --version
docker-pull-gcr-io|docker pull gcr.io/kaniko-project/executor:latest
docker-buildx-builder-buildkit|docker buildx --builder buildkit prune
docker-buildx-builder-buildkit-2|docker buildx --builder buildkit ls
docker-compose-f-compose-5|docker compose -f compose/listener.compose.yml up -d --no-build
docker-buildx-imagetools-inspect|docker buildx imagetools inspect app:ci
docker-buildx-create-use|docker buildx create --use
docker-pull-x-make|docker pull x; make build
echo-run-docker-build-2|echo "run docker build to make the image"
df-h|df -h
docker-build-f-docker-8|# docker build -f docker/Dockerfile .
docker-compose-exec-t|docker compose exec -T app npm run test
docker-compose-logs-run|docker compose logs run-worker
docker-compose-exec-app|docker compose exec app ./bin/up.sh up
UNSEEN

echo "case: an entry names a step by its id, pinned to the command a reviewer read"
root=$(fixture listed-by-id)
extra_step "$root" "docker run --rm golang go build ./..."
step_id "$root" "Extra" "compiles-only"
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "a listed step is excused" "$(is "$status" 0)"
check "  and the excusal names the id, the command and the reason" \
  "$(contains "$out" "listed in NOT_A_BUILD_STEPS: .github/workflows/image.yaml: job docker: step compiles-only: 'docker run --rm golang go build ./...' - compiles a Go binary in a container")"

echo "case: a step with no id cannot be listed, and its refusal says to give it one"
root=$(fixture listed-needs-an-id)
extra_step "$root" "docker run --rm golang go build ./..."
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "would excuse it if ids did not matter"
run_check "$root"
check "a step without an id is still refused" "$(is "$status" 1)"
check "  and the remedy asks for an id" "$(contains "$out" "give the step an \`id:\`")"
check "  and the entry matching nothing fails too" "$(contains "$out" "which matches 0 steps that build")"

echo "case: T1 - two unnamed steps are two steps, not one"
# Both collapse to "an unnamed step", so a name-keyed entry excused whichever came first and
# the other's build with it. Neither has an id, so neither can be listed at all.
root=$(fixture listed-two-unnamed)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(
    text
    + "      - run: docker run --rm golang go build ./...\n"
    + "      - run: docker build -f docker/Dockerfile .\n"
)
PYEOF
run_check "$root"
check "both unnamed steps are refused" "$(is "$status" 1)"
check "  the compiling one" "$(contains "$out" "'docker run --rm golang go build ./...'")"
check "  and the real build" "$(contains "$out" "'docker build -f docker/Dockerfile .'")"

echo "case: T2 - two steps sharing a name are told apart by id"
root=$(fixture listed-duplicate-names)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(
    text
    + "      - name: Extra\n        id: compiles-only\n        run: docker run --rm golang go build ./...\n"
    + "      - name: Extra\n        id: really-builds\n        run: docker build -f docker/Dockerfile .\n"
)
PYEOF
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "the twin that builds is still refused" "$(is "$status" 1)"
check "  and it is the build that is named" "$(contains "$out" "'docker build -f docker/Dockerfile .'")"
check "  while its namesake is excused" "$(contains "$out" "step compiles-only: 'docker run --rm golang go build ./...'")"

echo "case: T3 - a listed step that grows a build is refused, not carried by its entry"
root=$(fixture listed-drifted)
extra_step "$root" 'docker run --rm golang go build ./...\ndocker buildx build --load -f docker/Dockerfile .'
step_id "$root" "Extra" "compiles-only"
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "a drifted step is refused" "$(is "$status" 1)"
check "  and the entry cannot cover a certain build" "$(contains "$out" "cannot be:")"
check "  naming both commands" "$(contains "$out" "'docker run --rm golang go build ./...', 'docker buildx build --load -f docker/Dockerfile .'")"

echo "case: a deleted step's entry fails even when another step takes its name"
root=$(fixture listed-stale-by-deletion)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(
    text + "      - name: Extra\n        id: successor\n        run: docker run --rm golang go build ./...\n"
)
PYEOF
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "the step this described was deleted"
run_check "$root"
check "the stale entry fails" "$(is "$status" 1)"
check "  and names the id it cannot find" "$(contains "$out" "step id 'compiles-only', which matches 0 steps that build")"
check "  and the successor is refused on its own" "$(contains "$out" "Extra builds an image in a run step")"

echo "case: a listed step that stops reading as a build fails its entry"
root=$(fixture listed-no-longer-a-build)
extra_step "$root" "docker pull debian:trixie-slim"
step_id "$root" "Extra" "compiles-only"
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "no longer runs any build word"
run_check "$root"
check "the entry with nothing left to excuse fails" "$(is "$status" 1)"
check "  and says to remove it or correct the id" "$(contains "$out" "remove it or correct the id")"

echo "case: an inner step of a local composite is keyed under the calling step's id"
root=$(fixture listed-composite-inner)
mkdir -p "$root/.github/actions/helper"
cat > "$root/.github/actions/helper/action.yml" <<'YAML'
name: Helper
runs:
  using: composite
  steps:
    - id: compiles
      shell: bash
      run: docker run --rm golang go build ./...
YAML
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text + "      - name: Helper\n        id: helper\n        uses: ./.github/actions/helper\n")
PYEOF
run_check "$root"
check "the inner step is refused before it is listed" "$(is "$status" 1)"
allowlist "$root" "helper/compiles" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "  and excused once listed under caller/inner" "$(is "$status" 0)"
check "  with the joined id in the log" "$(contains "$out" "step helper/compiles:")"

echo "case: every command in a step is reported, not just the first"
root=$(fixture listed-every-hit)
extra_step "$root" 'docker run --rm golang go build ./...\ndocker buildx build --load -f docker/Dockerfile .'
run_check "$root"
check "the refusal lists both commands" "$(contains "$out" "('docker run --rm golang go build ./...', 'docker buildx build --load -f docker/Dockerfile .')")"

echo "case: P1 - an entry pins the whole command, not the span the regex matched"
# `docker buildx build --check .` and `docker buildx build --load -t x .` share the matched
# span `docker buildx build`, so a span-pinned entry for the lint excused the real build.
root=$(fixture pinned-full-text)
extra_step "$root" "docker run --rm golang go build ./..."
step_id "$root" "Extra" "compiles-only"
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "the reviewed command is excused" "$(is "$status" 0)"

root=$(fixture pinned-full-text-edited)
extra_step "$root" "docker run --rm golang go build ./cmd/..."
step_id "$root" "Extra" "compiles-only"
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary in a container"
run_check "$root"
check "an edited command is no longer described by the entry" "$(is "$status" 1)"
check "  and the message names what was reviewed" "$(contains "$out" "reviewed as running 'docker run --rm golang go build ./...'")"
check "  and what it runs now" "$(contains "$out" "now runs 'docker run --rm golang go build ./cmd/...'")"

echo "case: P1 - a repeated command is reported twice, not folded into one"
root=$(fixture pinned-not-deduplicated)
extra_step "$root" 'docker run --rm golang go build ./...\ndocker run --rm golang go build ./...'
run_check "$root"
check "both lines are printed" "$(contains "$out" "('docker run --rm golang go build ./...', 'docker run --rm golang go build ./...')")"

echo "case: P2 - a certain build cannot be listed, whatever its entry says"
# `--check` is a lint, but the shape is the anchored one, and pinning text cannot vouch for it:
# swapping `--check` for `--load` is the same shape. The list is not offered for these.
root=$(fixture unlistable-buildx-check)
extra_step "$root" "docker buildx build --check --file docker/Dockerfile ."
step_id "$root" "Extra" "lint-only"
allowlist "$root" "lint-only" "'docker buildx build --check --file docker/Dockerfile .'" "lints the Dockerfile"
run_check "$root"
check "the entry is refused" "$(is "$status" 1)"
check "  and says why it cannot be listed" "$(contains "$out" "cannot be:")"
check "  and names the shape" "$(contains "$out" "is certainly a build, or a compose verb that builds from a file this step only names")"

root=$(fixture unlistable-not-offered)
extra_step "$root" "docker buildx build --check --file docker/Dockerfile ."
run_check "$root"
check "an unlisted certain build is refused" "$(is "$status" 1)"
check "  and the list is not offered" "$(contains "$out" "This shape cannot be listed in NOT_A_BUILD_STEPS")"

echo "case: P2 - a compose entry cannot outlive the compose file gaining build:"
# The step names a file it does not contain, so its own text cannot say whether it builds. An
# entry for it would stay green after x.yml grows a `build:`, so there is no entry to make.
root=$(fixture unlistable-compose-up)
extra_step "$root" "docker compose -f x.yml up -d"
step_id "$root" "Extra" "starts-services"
allowlist "$root" "starts-services" "'docker compose -f x.yml up -d'" "x.yml has no build: today"
run_check "$root"
check "the compose entry is refused" "$(is "$status" 1)"
check "  and --no-build is the remedy" "$(contains "$out" "Remove the entry and add --no-build if the step should not build")"

echo "case: P2 - an earthly entry is refused: its image comes from the Earthfile"
root=$(fixture unlistable-earthly)
extra_step "$root" "earthly +test"
step_id "$root" "Extra" "runs-tests"
allowlist "$root" "runs-tests" "'earthly +test'" "+test runs tests"
run_check "$root"
check "the earthly entry is refused" "$(is "$status" 1)"
check "  and says it cannot be listed" "$(contains "$out" "cannot be:")"

echo "case: P3 - an inner step under a caller with no id cannot be listed"
# Keyed by its bare id, it answered to an entry written for a job-level step of the same id,
# and inherited that entry the moment the job-level step was removed.
root=$(fixture composite-caller-without-id)
mkdir -p "$root/.github/actions/helper"
cat > "$root/.github/actions/helper/action.yml" <<'YAML'
name: Helper
runs:
  using: composite
  steps:
    - id: compiles-only
      shell: bash
      run: docker run --rm golang go build ./...
YAML
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text + "      - name: Helper\n        uses: ./.github/actions/helper\n")
PYEOF
allowlist "$root" "compiles-only" "'docker run --rm golang go build ./...'" "an entry written for a job-level step"
run_check "$root"
check "the inner step does not answer to the bare id" "$(is "$status" 1)"
check "  and the remedy asks for the calling step's id" "$(contains "$out" "give the CALLING step an \`id:\`")"
check "  and the entry matches nothing" "$(contains "$out" "which matches 0 steps that build")"

echo "case: P3 - one composite used twice is two keys"
root=$(fixture composite-used-twice)
mkdir -p "$root/.github/actions/helper"
cat > "$root/.github/actions/helper/action.yml" <<'YAML'
name: Helper
runs:
  using: composite
  steps:
    - id: compiles-only
      shell: bash
      run: docker run --rm golang go build ./...
YAML
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(
    text
    + "      - name: First\n        id: first\n        uses: ./.github/actions/helper\n"
    + "      - name: Second\n        id: second\n        uses: ./.github/actions/helper\n"
)
PYEOF
allowlist "$root" "first/compiles-only" "'docker run --rm golang go build ./...'" "compiles a Go binary"
run_check "$root"
check "the second use is still refused" "$(is "$status" 1)"
check "  while the first is excused" "$(contains "$out" "step first/compiles-only:")"

echo "case: an entry names one step, and only that step"
root=$(fixture listed-one-step-only)
extra_step "$root" "docker run --rm golang go build ./..."
step_id "$root" "Extra" "listed"
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text + "      - name: Another\n        run: |\n          docker run --rm golang go build ./...\n")
PYEOF
allowlist "$root" "listed" "'docker run --rm golang go build ./...'" "a listed step"
run_check "$root"
check "the same command in an unlisted step is still refused" "$(is "$status" 1)"
check "  and it is the unlisted step that is named" "$(contains "$out" "Another builds an image in a run step")"
check "  and the listed step is excused" "$(contains "$out" "job docker: step listed:")"

echo "case: an implicit compose build names --no-build as its remedy"
root=$(fixture compose-remedy)
extra_step "$root" "docker compose -f compose/listener.compose.yml up -d"
run_check "$root"
check "the compose refusal names --no-build" "$(contains "$out" "add --no-build if the step should not build")"

echo "case: a compose subcommand that is not a build keeps its own remedy out of the way"
root=$(fixture compose-exec-run)
extra_step "$root" "docker compose exec app go run ./cmd/x"
run_check "$root"
check "compose exec running a go program is not a build" "$(is "$status" 0)"

echo "case: a result comparison is compared case-insensitively, as GitHub does"
root=$(fixture gate-result-capital-success)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: always() && needs.gatekeeper.result == 'Success' && needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "result == 'Success' is read like 'success'" "$(is "$status" 1)"

root=$(fixture gate-result-capital-skipped)
gate_probe "$root" "  docker:
    needs: [changes, gatekeeper]
    if: always() && needs.gatekeeper.result != 'FAILURE' && needs.changes.outputs.img == 'true'
    runs-on: ubuntu-24.04
" "$gatekeeper"
run_check "$root"
check "result != 'FAILURE' still admits skipped" "$(is "$status" 0)"

echo "case: a local action is read, not trusted"
# local_action <root> <action.yml body>: replaces the build step with a ./ action that has it.
local_action() {
  local root=$1 body=$2
  mkdir -p "$root/.github/actions/local"
  printf '%s\n' "$body" > "$root/.github/actions/local/action.yml"
  python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
head, _, _ = text.partition("      - uses: docker/build-push-action@v6")
open(path, "w").write(f"{head}      - name: Local\n        uses: ./.github/actions/local\n")
PYEOF
}

root=$(fixture local-action-run-build)
local_action "$root" "name: Local
runs:
  using: composite
  steps:
    - shell: bash
      run: docker build -f docker/Dockerfile ."
run_check "$root"
check "a composite whose run step builds is refused" "$(is "$status" 1)"
check "  and names the action file" "$(contains "$out" '.github/actions/local/action.yml')"

root=$(fixture local-action-build-push)
local_action "$root" "name: Local
runs:
  using: composite
  steps:
    - uses: docker/build-push-action@v6
      with:
        context: .
        file: docker/Dockerfile"
run_check "$root"
check "a build-push-action inside a composite is READ as the build" "$(is "$status" 0)"
check "  and counts as one" "$(contains "$out" '(1 image build(s))')"

root=$(fixture local-action-container)
local_action "$root" "name: Local
runs:
  using: docker
  image: Dockerfile"
run_check "$root"
check "a container action is refused" "$(is "$status" 1)"
check "  and says why" "$(contains "$out" 'using: docker')"

root=$(fixture local-action-node-execsync)
local_action "$root" "name: Local
runs:
  using: node20
  main: index.js"
printf 'require("child_process").execSync("docker buildx build --load -f docker/Dockerfile .");\n' \
  > "$root/.github/actions/local/index.js"
run_check "$root"
check "a local node20 action is refused, since its program is unread" "$(is "$status" 1)"
check "  and says how to admit it" "$(contains "$out" 'add this action to NON_BUILDING_ACTIONS')"

# The three local composites are READ, not named: appending a build to one must be seen.
root=$(fixture local-action-composite-appended)
mkdir -p "$root/.github/actions/setup-bun"
cat > "$root/.github/actions/setup-bun/action.yml" <<'ACTION'
name: Setup Bun
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@v2
    - shell: bash
      run: docker buildx build --load -f docker/Dockerfile .
ACTION
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text.replace(
    "      - uses: docker/build-push-action@v6\n",
    "      - uses: ./.github/actions/setup-bun\n      - uses: docker/build-push-action@v6\n",
))
PYEOF
run_check "$root"
check "a build appended to a local composite is read, not trusted" "$(is "$status" 1)"
check "  and names the action file" "$(contains "$out" '.github/actions/setup-bun/action.yml')"

root=$(fixture local-action-javascript)
local_action "$root" "name: Local
runs:
  using: node20
  main: index.js"
run_check "$root"
check "a JavaScript action is admitted, so zero builds is what fails" "$(contains "$out" 'this check covered 0 image builds')"

echo "case: a job that calls a workflow outside this repository is refused"
root=$(fixture job-uses-foreign)
python3 - "$root/.github/workflows/image.yaml" <<'PYEOF'
import sys
path = sys.argv[1]
text = open(path).read()
open(path, "w").write(text + "  called:\n    uses: acme/ci/.github/workflows/image.yml@v1\n")
PYEOF
run_check "$root"
check "a foreign workflow_call is refused" "$(is "$status" 1)"
check "  and names it" "$(contains "$out" 'acme/ci/.github/workflows/image.yml@v1')"


summary "check-image-trigger-paths.sh"
