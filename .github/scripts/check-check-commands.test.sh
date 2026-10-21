#!/usr/bin/env bash
# Tests for `check-check-commands.sh`: every rule it enforces, one case each, and — just as
# important — the shapes that must keep passing. A gate that forbids a word rather than a command
# trips on the Dockerfile line that creates `bunx`, on the guard that lists command names as data,
# on the plans that record what was run in 2026, and on the notes that explain this very defect;
# each of those is a case here, so narrowing the rule to "no `bunx` anywhere" fails the suite.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs, not a copy that can drift: it
# resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which the symlink puts at
# the fixture root.
#
# Run from anywhere: .github/scripts/check-check-commands.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-check-commands.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — two manifests whose check recipes go through the wrapper, a
# workflow and a shell script that do the same, a guide naming `bunx` in prose and in an exempted
# block, a solution note naming it in inline code, a skill, and a Dockerfile. Echoes the fixture
# root. Each case names its own tree: two cases sharing a name would share a tree, so a repeated
# name is refused here rather than surfacing as the `ln -s` below failing.
#
# The line numbers the cases assert on are this fixture's, so a file rewritten by a case writes
# the whole file rather than appending to one of these.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/packages/thing" \
    "$root/scripts" "$root/docs/solutions" "$root/skills/worker"
  ln -s "$check_script" "$root/.github/scripts/check-check-commands.sh"
  cat > "$root/package.json" <<'JSON'
{
  "name": "fixture",
  "scripts": {
    "lint": "scripts/repo-tool.sh biome check",
    "typecheck": "scripts/repo-tool.sh tsc --noEmit"
  }
}
JSON
  cat > "$root/packages/thing/package.json" <<'JSON'
{
  "name": "thing",
  "scripts": {
    "lint": "../../scripts/repo-tool.sh biome check src/",
    "typecheck": "../../scripts/repo-tool.sh tsc --noEmit",
    "build": "vite build",
    "docs:build": "astro build",
    "check": "bun run lint"
  }
}
JSON
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [push]
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - run: bun run lint .
      - name: Typecheck
        run: ../../scripts/repo-tool.sh tsc --noEmit
YAML
  cat > "$root/scripts/release.sh" <<'SH'
#!/bin/sh
# A comment may name bunx biome check . — it is prose, not a command.
set -eu
../../scripts/repo-tool.sh biome check .
SH
  cat > "$root/AGENTS.md" <<'MD'
# Fixture

Prose may say bunx biome check . and inline code may say `bunx biome`.

```bash
bun run lint .
scripts/repo-tool.sh biome check
```

<!-- check-check-commands: allow this block records the defect the gate forbids -->

```bash
bunx biome check .
```
MD
  cat > "$root/docs/solutions/note.md" <<'MD'
# A note

The fix was to stop running `bunx tsc --noEmit` by hand.

```bash
bun run typecheck
```
MD
  cat > "$root/skills/worker/SKILL.md" <<'MD'
# Worker

```bash
bun run lint .
```
MD
  cat > "$root/worker.Dockerfile" <<'DOCKER'
FROM oven/bun:${BUN_VERSION} AS worker
RUN ln -s bun /usr/local/bin/bunx
DOCKER
  echo "$root"
}

# run_check <root>: runs the script under test, setting `out` to its combined output and
# `status` to its exit code. Not a subshell, so both survive the call.
run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-check-commands.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree whose every check command goes through the wrapper"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it checked" \
  "$(contains "$out" 'every prescribed biome, tsc, tsserver and playwright command runs through scripts/repo-tool.sh')"
check "counts the files" "$(contains "$out" '2 manifest(s), 1 workflow(s)')"

echo "case: the shapes that must keep passing"
root=$(fixture dockerfile)
mkdir -p "$root/packages/daemon/docker"
cat > "$root/packages/daemon/docker/worker.Dockerfile" <<'DOCKER'
FROM oven/bun:${BUN_VERSION} AS worker
# The line that creates the bunx every agent then misuses.
RUN ln -s bun /usr/local/bin/bunx
RUN bunx biome check .
DOCKER
run_check "$root"
check "a Dockerfile that creates bunx passes: an image build is where a tool comes from PATH" \
  "$(is "$status" 0)"

root=$(fixture guard-own-text)
cat > "$root/.github/scripts/check-deps.sh" <<'SH'
#!/usr/bin/env bash
python3 <<'PY'
DEPENDENCY_COMMANDS = {
    "bun", "bunx", "go", "node", "npx", "pulumi", "mise", "make",
}
PY
bunx biome check .
SH
run_check "$root"
check "a .github/scripts/ guard quoting the names it forbids passes" "$(is "$status" 0)"

root=$(fixture history)
mkdir -p "$root/docs/plans" "$root/docs/superpowers" "$root/.superpowers"
for recorded in docs/plans/2026-03-03-plan.md docs/superpowers/notes.md .superpowers/notes.md; do
  cat > "$root/$recorded" <<'MD'
# What was run

```bash
bunx biome check .
bunx tsc --noEmit
```
MD
done
run_check "$root"
check "human-authored history keeps its commands" "$(is "$status" 0)"

root=$(fixture prose)
cat > "$root/AGENTS.md" <<'MD'
# Commands

Never run bunx biome check . here; `bunx biome` is a squatted package.
MD
run_check "$root"
check "a prose and an inline-code mention of bunx biome pass" "$(is "$status" 0)"

root=$(fixture exempted-block)
cat > "$root/docs/solutions/exempted.md" <<'MD'
# What the defect looked like

<!-- check-check-commands: allow the defect this note records is the command itself -->

```bash
bunx biome check .
```
MD
run_check "$root"
check "a fenced block exempted with a reason passes" "$(is "$status" 0)"

root=$(fixture exempted-line)
cat >> "$root/scripts/release.sh" <<'SH'
bunx biome check . # check-check-commands: allow this line is the shape the guard forbids
SH
run_check "$root"
check "a shell line exempted with a reason passes" "$(is "$status" 0)"

root=$(fixture unlisted-guide)
cat > "$root/docs/random.md" <<'MD'
# Scratch

```bash
bunx biome check .
```
MD
run_check "$root"
check "a markdown file outside the prescriptive set is not judged" "$(is "$status" 0)"

root=$(fixture nested-wrapper)
cat > "$root/scripts/screenshots.sh" <<'SH'
#!/bin/sh
set -eu
bash -c 'cd "$1/packages/dispatch" && ../../scripts/repo-tool.sh playwright test' shots "$PWD"
SH
run_check "$root"
check "the wrapper nested in a quoted bash -c passes" "$(is "$status" 0)"

root=$(fixture env-prefix)
cat >> "$root/scripts/release.sh" <<'SH'
CI=1 NODE_ENV=test ../../scripts/repo-tool.sh biome check .
SH
run_check "$root"
check "env-var prefixes before the wrapper pass" "$(is "$status" 0)"

echo "case: rule 1 — a package.json recipe"
root=$(fixture recipe-bunx)
sed -i 's|    "lint": .*|    "lint": "bunx biome check src/",|' "$root/packages/thing/package.json"
run_check "$root"
check "a bunx biome recipe fails" "$(is "$status" 1)"
check "names the recipe's own line" \
  "$(contains "$out" 'packages/thing/package.json:4: the "lint" recipe runs `bunx`')"
check "says what to run instead" "$(contains "$out" 'scripts/repo-tool.sh <tool>')"
check "counts it once" "$(contains "$out" '1 place(s)')"

root=$(fixture recipe-bare-tsc)
sed -i 's|    "typecheck": .*|    "typecheck": "tsc --noEmit",|' "$root/packages/thing/package.json"
run_check "$root"
check "a bare tsc recipe fails: the worker image answers the name" "$(is "$status" 1)"
check "names the recipe's own line" \
  "$(contains "$out" 'packages/thing/package.json:5: the "typecheck" recipe runs `tsc`')"

root=$(fixture recipe-bunx-vite)
sed -i 's|    "build": .*|    "build": "bunx vite build",|' "$root/packages/thing/package.json"
run_check "$root"
check "a bunx vite recipe fails, though vite itself is no pinned check tool" "$(is "$status" 1)"
check "names that line" "$(contains "$out" 'packages/thing/package.json:6: the "build" recipe runs `bunx`')"

root=$(fixture recipe-chained)
sed -i 's|    "check": .*|    "check": "bun run build \&\& bunx playwright test"|' \
  "$root/packages/thing/package.json"
run_check "$root"
check "a second segment after && is judged too" "$(is "$status" 1)"
check "names that line" "$(contains "$out" 'packages/thing/package.json:8: the "check" recipe runs `bunx`')"

root=$(fixture manifest-broken)
printf '{ "scripts": {\n' > "$root/packages/thing/package.json"
run_check "$root"
check "a manifest that is not JSON fails" "$(is "$status" 1)"
check "names the file, without a traceback" \
  "$(contains "$out" 'packages/thing/package.json is not valid JSON')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: rule 2 — a workflow step"
root=$(fixture workflow-bunx)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - name: Types
        run: |
          echo checking
          bunx tsc --noEmit
YAML
run_check "$root"
check "a bunx tsc step fails" "$(is "$status" 1)"
check "reports the offending line of the block scalar, not the step's" \
  "$(contains "$out" 'ci.yaml:13: this run: line runs `bunx`')"

root=$(fixture workflow-bare-biome)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: biome check --error-on-warnings .
YAML
run_check "$root"
check "a bare biome step fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'ci.yaml:10: this run: line runs `biome`')"

root=$(fixture workflow-broken)
printf 'jobs: [\n' > "$root/.github/workflows/broken.yaml"
run_check "$root"
check "a workflow that is not YAML fails" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'broken.yaml is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: rule 3 — a shell script and a fenced block"
root=$(fixture shell-playwright)
cat >> "$root/scripts/release.sh" <<'SH'
playwright test --config e2e/playwright.config.ts
SH
run_check "$root"
check "a bare playwright line fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'scripts/release.sh:5: this line runs `playwright`')"

root=$(fixture shell-nested-bunx)
cat > "$root/scripts/screenshots.sh" <<'SH'
#!/bin/sh
set -eu
bash -c 'cd "$1/packages/dispatch" && bunx playwright test' shots "$PWD"
SH
run_check "$root"
check "a bunx nested in a quoted bash -c fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'scripts/screenshots.sh:3: this line runs `bunx`')"

root=$(fixture guide-fenced-bunx)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
bunx biome check .
```
MD
run_check "$root"
check "a fenced bunx biome in AGENTS.md fails" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'AGENTS.md:4: this fenced command runs `bunx`')"

root=$(fixture note-fenced-tsc)
cat > "$root/docs/solutions/note.md" <<'MD'
# A note

```bash
tsc --noEmit
```
MD
run_check "$root"
check "a fenced bare tsc in a solution note fails" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'docs/solutions/note.md:4: this fenced command runs `tsc`')"

root=$(fixture skill-fenced-playwright)
cat > "$root/skills/worker/SKILL.md" <<'MD'
# Worker

```bash
bunx playwright test
```
MD
run_check "$root"
check "a fenced bunx playwright in a skill fails" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'skills/worker/SKILL.md:4: this fenced command runs `bunx`')"

echo "case: rule 4 — an exemption that gives no reason"
root=$(fixture exemption-empty-block)
cat > "$root/AGENTS.md" <<'MD'
# Commands

<!-- check-check-commands: allow -->

```bash
bun run lint .
```
MD
run_check "$root"
check "a block marker with no reason fails" "$(is "$status" 1)"
check "names the marker's line" \
  "$(contains "$out" 'AGENTS.md:3: this exemption gives no reason')"

root=$(fixture exemption-empty-block-hides)
cat > "$root/AGENTS.md" <<'MD'
# Commands

<!-- check-check-commands: allow -->

```bash
bunx biome check .
```
MD
run_check "$root"
check "and it exempts nothing: the command under it is still judged" "$(is "$status" 1)"
check "both are reported" "$(contains "$out" '2 place(s)')"

root=$(fixture exemption-empty-line)
cat >> "$root/scripts/release.sh" <<'SH'
bunx biome check . # check-check-commands: allow
SH
run_check "$root"
check "a line marker with no reason fails" "$(is "$status" 1)"
check "names that line" "$(contains "$out" 'scripts/release.sh:5: this exemption gives no reason')"

echo "case: a file that is not text names itself"
root=$(fixture undecodable)
printf '# A note\n\xff\xfe binary\n' > "$root/docs/solutions/rogue.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, without a traceback" \
  "$(contains "$out" 'docs/solutions/rogue.md is not readable as text')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a file jj has never seen still counts"
root=$(fixture untracked)
cat > "$root/packages/thing/AGENTS.md" <<'MD'
# Thing

```bash
bunx tsc --noEmit
```
MD
run_check "$root"
check "an unsnapshotted guide is read" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'packages/thing/AGENTS.md:4: this fenced command runs `bunx`')"

summary "check-check-commands.sh"
