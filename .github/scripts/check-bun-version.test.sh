#!/usr/bin/env bash
# Tests for `check-bun-version.sh`: every rule it enforces, one case each, plus the shapes six
# review rounds planted by hand and threw away. The gate's rule set changed in four of those
# rounds — twice because a rule was missing rather than wrong — so the probes live here now.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs, not a copy that can drift: it
# resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at
# the fixture root.
#
# Run from anywhere: .github/scripts/check-bun-version.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-bun-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — the pin, the wrapper action, one workflow using it, and
# a Dockerfile whose Bun tag comes from the ARG. Echoes the fixture root. Each case names its
# own tree: two cases sharing a name would share a tree, so a repeated name is refused here
# rather than surfacing as the `ln -s` below failing.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/actions/setup-bun"
  ln -s "$check_script" "$root/.github/scripts/check-bun-version.sh"
  echo "1.3.14" > "$root/.bun-version"
  cat > "$root/.github/actions/setup-bun/action.yml" <<'YAML'
name: Setup Bun
description: Install the Bun version .bun-version locks.
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@v2
      with:
        bun-version-file: .bun-version
YAML
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
      - uses: ./.github/actions/setup-bun
      - run: bun install --frozen-lockfile
YAML
  cat > "$root/Dockerfile" <<'YAML'
ARG BUN_VERSION=1.3.14
FROM oven/bun:${BUN_VERSION} AS web
RUN bun install
YAML
  echo "$root"
}

# run_check <root>: runs the script under test, setting `out` to its combined output and
# `status` to its exit code. Not a subshell, so both survive the call.
run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-bun-version.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree where every Bun install reads the pin"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says so" "$(contains "$out" 'every Bun install reads .bun-version (1.3.14)')"

echo "case: rule 1 — the pin file itself"
root=$(fixture pin-missing)
rm "$root/.bun-version"
run_check "$root"
check "missing pin fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" '.bun-version is missing')"

root=$(fixture pin-empty)
: > "$root/.bun-version"
run_check "$root"
check "empty pin fails" "$(is "$status" 1)"
check "names the file" "$(contains "$out" '.bun-version is empty')"

for range in 1 1.3 "^1.3.14"; do
  root=$(fixture "pin-range-$(echo "$range" | tr -d '^.')")
  echo "$range" > "$root/.bun-version"
  sed -i "s/^ARG BUN_VERSION=.*/ARG BUN_VERSION=$range/" "$root/Dockerfile"
  run_check "$root"
  check "a floating pin ($range) fails even when everything agrees with it" "$(is "$status" 1)"
  check "says it is not a release" "$(contains "$out" 'not a release')"
done

echo "case: rule 2 — a file that is not YAML"
root=$(fixture bad-yaml)
printf 'jobs: [\n' > "$root/.github/workflows/broken.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'broken.yaml is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

root=$(fixture half-written)
for shape in "jobs:" "jobs: []" "jobs: lint"; do
  printf 'name: half\non: [push]\n%s\n' "$shape" > "$root/.github/workflows/half.yaml"
  run_check "$root"
  check "a half-written workflow ($shape) walks to nothing" "$(is "$status" 0)"
done

echo "case: rule 3 — setup-bun outside the wrapper"
root=$(fixture raw-setup-bun)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - uses: oven-sh/setup-bun@v2
        with:
          bun-version-file: .bun-version
YAML
run_check "$root"
check "a correctly-pinned direct call still fails" "$(is "$status" 1)"
check "points at the wrapper" "$(contains "$out" 'is called directly')"

root=$(fixture raw-unpinned)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - uses: oven-sh/setup-bun@v2
YAML
run_check "$root"
check "an unpinned direct call fails" "$(is "$status" 1)"

root=$(fixture nested-action)
mkdir -p "$root/tools/deep/setup"
cat > "$root/tools/deep/setup/action.yml" <<'YAML'
name: deep
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@v2
YAML
run_check "$root"
check "a composite action anywhere in the tree is reached" "$(is "$status" 1)"
check "names that file" "$(contains "$out" 'tools/deep/setup/action.yml')"

root=$(fixture wrapper-literal)
sed -i 's/        bun-version-file: .bun-version/        bun-version: 1.4.2/' \
  "$root/.github/actions/setup-bun/action.yml"
run_check "$root"
check "the wrapper itself must pass the file" "$(is "$status" 1)"
check "says which input it wants" "$(contains "$out" 'must pass bun-version-file')"

root=$(fixture wrapper-download-url)
cat >> "$root/.github/actions/setup-bun/action.yml" <<'YAML'
        bun-download-url: https://example.com/bun-v1.4.2.zip
YAML
run_check "$root"
check "any extra input on the wrapper fails, since setup-bun prefers them" "$(is "$status" 1)"
check "names the input" "$(contains "$out" 'bun-download-url')"

echo "case: rule 4 — a step or a RUN that installs Bun itself"
installers=(
  'curl -fsSL https://bun.sh/install | bash'
  'curl -fsSL https://bun.com/install | bash'
  'npm install -g bun@1.4.2'
  'npm install bun -g'
  'npm i --global bun'
  'mise use -g bun@1.4.2'
  'curl -L https://github.com/oven-sh/bun/releases/download/bun-v1.4.2/bun-linux-x64.zip -o bun.zip'
)
for index in "${!installers[@]}"; do
  installer=${installers[index]}
  root=$(fixture "installer-$index")
  printf '      - run: %s\n' "$installer" >> "$root/.github/workflows/ci.yaml"
  run_check "$root"
  check "a run: step that does '${installer:0:28}…' fails" "$(is "$status" 1)"
done

root=$(fixture dockerfile-run)
printf 'RUN curl -fsSL https://bun.sh/install | bash\n' >> "$root/Dockerfile"
run_check "$root"
check "the same command in a Dockerfile RUN fails" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'this RUN installs Bun itself')"

echo "case: rule 5 — a job runner or service on a Bun image"
root=$(fixture container-image)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  boxed:
    runs-on: ubuntu-24.04
    container:
      image: oven/bun:latest
    steps:
      - run: bun test
YAML
run_check "$root"
check "an unpinned container image fails" "$(is "$status" 1)"
check "names the pin to use" "$(contains "$out" 'runs an unpinned Bun')"

root=$(fixture service-image)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  served:
    runs-on: ubuntu-24.04
    services:
      bun:
        image: oven/bun:1
    steps:
      - run: echo hi
YAML
run_check "$root"
check "an unpinned service image fails" "$(is "$status" 1)"

root=$(fixture container-pinned)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  boxed:
    runs-on: ubuntu-24.04
    container:
      image: oven/bun:1.3.14-slim
    steps:
      - run: bun test
YAML
run_check "$root"
check "the pinned image, suffix and all, passes" "$(is "$status" 0)"

echo "case: rule 6 — Dockerfile bases and ARG defaults"
root=$(fixture from-literal)
printf 'FROM oven/bun:1 AS web\n' > "$root/Dockerfile"
run_check "$root"
check "the pre-pin shape (a literal tag, no ARG) fails" "$(is "$status" 1)"
check "says what to write" "$(contains "$out" 'hardcodes its Bun tag')"

root=$(fixture from-lowercase)
printf 'from oven/bun:latest as web\n' > "$root/Dockerfile"
run_check "$root"
check "a lowercase from is judged the same" "$(is "$status" 1)"

root=$(fixture arg-drift)
sed -i 's/^ARG BUN_VERSION=.*/ARG BUN_VERSION=1.4.2/' "$root/Dockerfile"
run_check "$root"
check "an ARG default that disagrees with the pin fails" "$(is "$status" 1)"
check "names both versions" "$(contains "$out" 'but .bun-version locks 1.3.14')"

root=$(fixture dockerfile-suffix)
printf 'ARG BUN_VERSION=1.3.14\nFROM oven/bun:${BUN_VERSION}-slim AS cli\n' > "$root/worker.Dockerfile"
run_check "$root"
check "a -slim suffix on the ARG tag passes" "$(is "$status" 0)"

echo "case: rule 7 — a tool manager that installs bun"
for manifest in mise.toml .mise.toml mise.local.toml .mise.local.toml .config/mise.toml \
  .config/mise/config.toml .mise/config.toml; do
  root=$(fixture "manifest-$(echo "$manifest" | tr './' '--')")
  mkdir -p "$(dirname "$root/$manifest")"
  printf '[tools]\nbun = "1.4.2"\n' > "$root/$manifest"
  run_check "$root"
  check "$manifest naming bun fails" "$(is "$status" 1)"
done

root=$(fixture manifest-dotted)
printf 'tools.bun = "1.4.2"\n' > "$root/mise.toml"
run_check "$root"
check "the dotted tools.bun spelling fails" "$(is "$status" 1)"

root=$(fixture tool-versions)
printf 'jj 0.45.1\nbun 1.4.2\n' > "$root/.tool-versions"
run_check "$root"
check ".tool-versions naming bun fails" "$(is "$status" 1)"

root=$(fixture manifest-other-tool)
printf '[tools]\njj = "0.45.1"\n' > "$root/mise.toml"
run_check "$root"
check "a manifest that installs something else passes" "$(is "$status" 0)"

echo "case: the log cannot be forged through an interpolated value"
root=$(fixture log-injection)
cat > "$root/.github/actions/setup-bun/action.yml" <<'YAML'
name: Setup Bun
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@v2
      with:
        bun-version-file: |
          .bun-version
          ::stop-commands::hidetoken
YAML
run_check "$root"
check "a block scalar fails the gate" "$(is "$status" 1)"
check "and reaches the log on one line" "$(is "$(contains "$out" '^::stop-commands::')" false)"

echo "case: the wrapper is judged for what it is"
root=$(fixture wrapper-miscased)
cat > "$root/.github/actions/setup-bun/action.yml" <<'YAML'
name: Setup Bun
runs:
  using: composite
  steps:
    - uses: Oven-Sh/setup-bun@v2
      with:
        bun-version: 1.4.2
YAML
run_check "$root"
check "a mis-cased ref with a literal version fails" "$(is "$status" 1)"
check "names the input it must pass" "$(contains "$out" 'must pass bun-version-file')"

root=$(fixture wrapper-missing)
rm "$root/.github/actions/setup-bun/action.yml"
run_check "$root"
check "a deleted wrapper fails" "$(is "$status" 1)"
check "says the wrapper is missing" "$(contains "$out" 'is missing: it is the one place')"

root=$(fixture wrapper-forked)
sed -i 's|oven-sh/setup-bun@v2|acme/setup-bun@v2|' "$root/.github/actions/setup-bun/action.yml"
run_check "$root"
check "a wrapper repointed at a fork fails" "$(is "$status" 1)"
check "says nothing installs Bun" "$(contains "$out" 'not one')"

root=$(fixture wrapper-twice)
cat >> "$root/.github/actions/setup-bun/action.yml" <<'YAML'
    - uses: oven-sh/setup-bun@v2
      with:
        bun-version: 1.4.2
YAML
run_check "$root"
check "a second setup-bun step in the wrapper fails" "$(is "$status" 1)"
check "counts them" "$(contains "$out" 'holds 2 steps, not one')"

root=$(fixture wrapper-second-step)
cat >> "$root/.github/actions/setup-bun/action.yml" <<'YAML'
    - shell: bash
      run: echo "/opt/bun/bin" >> "$GITHUB_PATH"
YAML
run_check "$root"
check "a second step prepending \$GITHUB_PATH in the wrapper fails" "$(is "$status" 1)"
check "says what a second step can do to a later one" "$(contains "$out" 'prepend $GITHUB_PATH')"
check "and the step count is the only rule that catches it" "$(contains "$out" '1 place(s)')"

root=$(fixture wrapper-inert-step)
cat >> "$root/.github/actions/setup-bun/action.yml" <<'YAML'
    - name: neither run nor uses
YAML
run_check "$root"
check "a second step carrying neither run nor uses is still a step" "$(is "$status" 1)"
check "counts it" "$(contains "$out" 'holds 2 steps, not one')"

root=$(fixture wrapper-env)
cat > "$root/.github/actions/setup-bun/action.yml" <<'YAML'
name: Setup Bun
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@v2
      with:
        bun-version-file: .bun-version
      env:
        INPUT_BUN-VERSION: 1.4.2
YAML
run_check "$root"
check "an INPUT_* env on the wrapper's step fails" "$(is "$status" 1)"
check "names the variable" "$(contains "$out" 'INPUT_BUN-VERSION')"
check "and says it once, in one wording" "$(contains "$out" '1 place(s)')"

root=$(fixture wrapper-env-other)
cat >> "$root/.github/actions/setup-bun/action.yml" <<'YAML'
      env:
        BUN_INSTALL: /opt/bun
YAML
run_check "$root"
check "any other env on the wrapper's step fails too" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'BUN_INSTALL')"

root=$(fixture wrapper-vendored)
sed -i 's|uses: oven-sh/setup-bun@v2|uses: ./vendor/oven-sh/setup-bun|' \
  "$root/.github/actions/setup-bun/action.yml"
run_check "$root"
check "a vendored local copy in the wrapper fails" "$(is "$status" 1)"
check "says what it must run" "$(contains "$out" 'not oven-sh/setup-bun@<ref>')"

root=$(fixture wrapper-refless)
sed -i 's|uses: oven-sh/setup-bun@v2|uses: oven-sh/setup-bun|' \
  "$root/.github/actions/setup-bun/action.yml"
run_check "$root"
check "a ref-less uses in the wrapper fails" "$(is "$status" 1)"

echo "case: a RUN split across continuation lines is judged whole"
root=$(fixture continuation)
cat >> "$root/Dockerfile" <<'YAML'
RUN apt-get update \
    && curl -fsSL https://bun.sh/install | bash \
    && rm -rf /var/lib/apt/lists/*
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "reports the line the RUN starts on" "$(contains "$out" 'Dockerfile:4: this RUN installs Bun itself')"

root=$(fixture continuation-comment)
cat >> "$root/Dockerfile" <<'YAML'
# a trailing backslash in a comment does not continue it \
RUN curl -fsSL https://bun.sh/install | bash
YAML
run_check "$root"
check "a comment ending in a backslash does not swallow the RUN under it" "$(is "$status" 1)"
check "the RUN is still judged" "$(contains "$out" 'this RUN installs Bun itself')"

root=$(fixture continuation-interior-comment)
cat >> "$root/Dockerfile" <<'YAML'
RUN apt-get update \
# Docker drops this line and the instruction carries on
    && curl -fsSL https://bun.sh/install | bash
YAML
run_check "$root"
check "a comment inside a continued RUN does not hide the rest of it" "$(is "$status" 1)"
check "the installer is still found" "$(contains "$out" 'this RUN installs Bun itself')"

echo "case: an INPUT_* environment variable written by hand"
root=$(fixture env-step-input)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: bun test
        env:
          INPUT_BUN-VERSION: 1.4.2
YAML
run_check "$root"
check "an INPUT_* on a step fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'INPUT_BUN-VERSION')"

root=$(fixture env-workflow-input)
cat > "$root/.github/workflows/inherited.yaml" <<'YAML'
name: Inherited
on: [push]
env:
  INPUT_BUN-VERSION: 1.4.2
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/setup-bun
YAML
run_check "$root"
check "an INPUT_* at workflow level fails, since every step inherits it" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'INPUT_BUN-VERSION')"

root=$(fixture env-container-input)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  boxed:
    runs-on: ubuntu-24.04
    container:
      image: debian:trixie-slim
      env:
        INPUT_BUN-VERSION: 1.4.2
    steps:
      - uses: ./.github/actions/setup-bun
YAML
run_check "$root"
check "an INPUT_* on a job's container fails" "$(is "$status" 1)"

root=$(fixture env-github-env)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: echo "INPUT_BUN-VERSION=1.4.2" >> "$GITHUB_ENV"
      - uses: ./.github/actions/setup-bun
YAML
run_check "$root"
check "a step exporting INPUT_* into \$GITHUB_ENV before the wrapper fails" "$(is "$status" 1)"
check "says every later step inherits it" "$(contains "$out" 'every step after it inherits')"

root=$(fixture env-github-env-delimited)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: |
          {
            echo "INPUT_BUN-VERSION<<EOF"
            echo 1.4.2
            echo EOF
          } >> "$GITHUB_ENV"
YAML
run_check "$root"
check "the documented {name}<<{delimiter} form, which carries no =, fails too" "$(is "$status" 1)"

root=$(fixture env-github-env-other)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: echo "BUN_INSTALL=/opt/bun" >> "$GITHUB_ENV"
YAML
run_check "$root"
check "a step exporting a variable that is not an action input passes" "$(is "$status" 0)"

root=$(fixture env-job-input)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  boxed:
    runs-on: ubuntu-24.04
    env:
      INPUT_BUN-VERSION: 1.4.2
    steps:
      - uses: ./.github/actions/setup-bun
YAML
run_check "$root"
check "an INPUT_* on a job fails" "$(is "$status" 1)"

echo "case: a job named like the loader's own bookkeeping"
root=$(fixture job-named-lines)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  __lines__:
    runs-on: ubuntu-24.04
    steps:
      - uses: oven-sh/setup-bun@v2
YAML
run_check "$root"
check "a job named __lines__ is judged like any other" "$(is "$status" 1)"
check "names the direct call" "$(contains "$out" 'is called directly')"

echo "case: docker run of an unpinned Bun image"
root=$(fixture docker-run)
printf '      - run: docker run --rm oven/bun:latest bun --version\n' >> "$root/.github/workflows/ci.yaml"
run_check "$root"
check "an unpinned docker run fails" "$(is "$status" 1)"

root=$(fixture docker-run-pinned)
printf '      - run: docker run --rm oven/bun:1.3.14 bun --version\n' >> "$root/.github/workflows/ci.yaml"
run_check "$root"
check "the pinned one passes" "$(is "$status" 0)"

root=$(fixture docker-run-untagged)
printf '      - run: docker run --rm oven/bun bun --version\n' >> "$root/.github/workflows/ci.yaml"
run_check "$root"
check "an untagged docker run fails, like an untagged container image" "$(is "$status" 1)"

echo "case: a [tools.bun] section header"
root=$(fixture manifest-section)
printf '[tools.bun]\nversion = "1.4.2"\n' > "$root/mise.toml"
run_check "$root"
check "fails" "$(is "$status" 1)"

echo "case: a file that is not text names itself"
root=$(fixture undecodable)
printf 'ARG BUN_VERSION=1.3.14\n\xff\xfe binary\n' > "$root/Rogue.Dockerfile"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'Rogue.Dockerfile is not readable as text')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a bare-string container: is reported at its own line"
root=$(fixture container-bare)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  boxed:
    runs-on: ubuntu-24.04
    container: oven/bun:latest
    steps:
      - run: bun test
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the container line, not the job's" "$(contains "$out" 'ci.yaml:12: oven/bun:latest runs an unpinned Bun')"

echo "case: a file git has never seen still counts"
root=$(fixture untracked)
command -v git > /dev/null || { echo "  FAIL: git is required for the untracked case" >&2; exit 1; }
git -C "$root" init -q
cat > "$root/.github/workflows/new.yaml" <<'YAML'
name: new
on: [push]
jobs:
  a:
    runs-on: ubuntu-24.04
    steps:
      - uses: oven-sh/setup-bun@v2
YAML
run_check "$root"
check "an unsnapshotted workflow is read" "$(is "$status" 1)"

summary "check-bun-version.sh"
