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
  "$(contains "$out" 'packages/thing/package.json:5: the "typecheck" recipe names `tsc`')"

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
check "names its line" "$(contains "$out" 'ci.yaml:10: this run: line names `biome`')"

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
check "names its line" "$(contains "$out" 'scripts/release.sh:5: this line names `playwright`')"

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
  "$(contains "$out" 'docs/solutions/note.md:4: this fenced command names `tsc`')"

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

echo "case: \`bun x\` and \`npx\` are the same command spelled another way"
# Measured in a role container of this issue's own pod (worker image, bun 1.3.14, no node_modules):
# `bun x biome check .` and `npx biome check .` each exit 0 having checked nothing, and
# `bun x biome --version` and `npx biome --version` each answer 0.3.3 — the squatted package, not
# the Biome 2.4.11 this repository pins. `bun x` is bunx's own spelling (one binary, one
# resolution), and the worker image ships npm and npx beside bun, so both reach the squat exactly
# as `bunx` does. A gate that keeps the vulnerable form out has to keep these out too.
root=$(fixture guide-fenced-bun-x)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
bun x biome check .
```
MD
run_check "$root"
check "a fenced 'bun x biome' in AGENTS.md fails" "$(is "$status" 1)"
check "names the line inside the block" "$(contains "$out" 'AGENTS.md:4:')"

root=$(fixture recipe-bun-x)
sed -i 's|    "lint": .*|    "lint": "bun x biome check src/",|' "$root/packages/thing/package.json"
run_check "$root"
check "a 'bun x' lint recipe fails" "$(is "$status" 1)"
check "names the recipe's own line" "$(contains "$out" 'packages/thing/package.json:4:')"

root=$(fixture guide-fenced-npx)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
npx biome check .
```
MD
run_check "$root"
check "a fenced 'npx biome' in AGENTS.md fails" "$(is "$status" 1)"
check "names the line inside the block" "$(contains "$out" 'AGENTS.md:4:')"

echo "case: every other spelling that hands a bare name to a resolver"
# One binary or another, the resolution is the same: `pnpx`, `npm exec`, `pnpm dlx` and
# `yarn dlx` each fetch what the registry serves for a name this repository pins. pnpm and yarn
# are judged although no image here installs them — a guide outlives the image that reads it,
# and the rule is about what a reader may be told to run.
root=$(fixture workflow-pnpx)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - run: pnpx tsc --noEmit
YAML
run_check "$root"
check "a pnpx workflow step fails" "$(is "$status" 1)"
check "names its line and the runner" \
  "$(contains "$out" 'ci.yaml:10: this run: line runs `pnpx`, a package runner')"

root=$(fixture shell-npm-exec)
cat >> "$root/scripts/release.sh" <<'SH'
npm exec -- biome check .
SH
run_check "$root"
check "an npm exec shell line fails" "$(is "$status" 1)"
check "names both words of the runner" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `npm exec`, a package runner')"

root=$(fixture guide-pnpm-dlx)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
pnpm dlx @biomejs/biome check .
```
MD
run_check "$root"
check "a fenced pnpm dlx fails" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'AGENTS.md:4: this fenced command runs `pnpm dlx`')"

root=$(fixture recipe-yarn-dlx)
sed -i 's|    "lint": .*|    "lint": "yarn dlx biome check src/",|' "$root/packages/thing/package.json"
run_check "$root"
check "a yarn dlx recipe fails" "$(is "$status" 1)"
check "names the recipe's own line" \
  "$(contains "$out" 'packages/thing/package.json:4: the "lint" recipe runs `yarn dlx`')"

root=$(fixture shell-npx-by-path)
cat >> "$root/scripts/release.sh" <<'SH'
/usr/local/bin/npx biome check .
SH
run_check "$root"
check "a runner named by its path fails: the basename is what runs" "$(is "$status" 1)"
check "names the path the line would run" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `/usr/local/bin/npx`')"

root=$(fixture ordinary-commands)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
bun run lint
bun test
bun build ./src/index.ts
npm view biome version
```
MD
run_check "$root"
check "bun run, bun test, bun build and npm view are no resolvers" "$(is "$status" 0)"

echo "case: a runner behind an ordinary prefix word is still that runner"
# Measured in a role container of this issue's own pod (worker image, bun 1.3.14, no
# node_modules): `exec bunx biome --version`, `env FOO=1 bunx biome --version`,
# `command bunx biome --version`, `nice -n 5 bunx biome --version` and
# `bun --bun x biome --version` each answer 0.3.3 and exit 0 — the squat, exactly as the bare
# spelling does. A prefix word changes nothing about what runs, and the gate already steps over
# the bare `FOO=bar` assignment that `env FOO=bar` spells out, so a prefix must not be the one
# place the silent green comes back.
root=$(fixture shell-exec-bunx)
cat >> "$root/scripts/release.sh" <<'SH'
exec bunx biome check .
SH
run_check "$root"
check "an exec'd bunx shell line fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'scripts/release.sh:5:')"

root=$(fixture shell-env-bunx)
cat >> "$root/scripts/release.sh" <<'SH'
env CI=1 bunx biome check .
SH
run_check "$root"
check "an \`env\` that spells out an assignment the gate already steps over fails" \
  "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'scripts/release.sh:5:')"

root=$(fixture shell-nice-bunx)
cat >> "$root/scripts/release.sh" <<'SH'
nice -n 5 bunx biome check .
SH
run_check "$root"
check "a niced bunx fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'scripts/release.sh:5:')"

root=$(fixture guide-command-bunx)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
command bunx biome check .
```
MD
run_check "$root"
check "a fenced \`command bunx\` fails" "$(is "$status" 1)"
check "names the line inside the block" "$(contains "$out" 'AGENTS.md:4:')"

root=$(fixture recipe-bun-flag-x)
sed -i 's|    "lint": .*|    "lint": "bun --bun x biome check src/",|' \
  "$root/packages/thing/package.json"
run_check "$root"
check "an option between \`bun\` and its \`x\` does not hide the runner" "$(is "$status" 1)"
check "names the recipe's own line" "$(contains "$out" 'packages/thing/package.json:4:')"

root=$(fixture prefixed-ordinary-commands)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
exec ./scripts/repo-tool.sh biome check .
env CI=1 bun run lint .
nice -n 5 bun test
```
MD
run_check "$root"
check "the same prefixes before the wrapper and before ordinary commands still pass" \
  "$(is "$status" 0)"

echo "case: the tool is the command a segment runs, not the text it contains"
# Four lines that live in this repository today and must keep passing, written here as fixtures
# so the suite holds them whatever the tree does next: a trailing comment naming a tool after an
# ordinary command (AGENTS.md:53), a comment naming `tsc` beside `tsconfig.check.json`, a
# capitalised `Biome` and a `biome.json` in the same comment (packages/proof-editor/AGENTS.md:168
# and :169), and `bunx playwright install --with-deps chromium` under a block-level marker
# (docs/solutions/envoy/github-runner-chrome-apt-hash-mismatch.md:45). A rule that read "the
# segment contains a tool's name" would refuse every one of them, the note that records this
# very defect included.
root=$(fixture tool-named-in-a-comment)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
bun run lint <package>/       # Lint (the pinned Biome, through `scripts/repo-tool.sh`)
bun run typecheck             # Type check (the pinned tsc, through the same wrapper)
bun run typecheck              # tsc against tsconfig.check.json (src/, tests/ and scripts/)
bun run lint                   # Biome over the package; the root biome.json turns it off for the copy
../../scripts/repo-tool.sh playwright test --config e2e/playwright.config.ts
```
MD
run_check "$root"
check "a tool named in a trailing comment, or inside an identifier, is no command" \
  "$(is "$status" 0)"

root=$(fixture tool-before-a-comment)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash
tsc --noEmit                  # Type check (the comment is not what hides this)
```
MD
run_check "$root"
check "and stripping the comment leaves the command it follows" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'AGENTS.md:4: this fenced command names `tsc`')"

root=$(fixture marked-block-keeps-the-runner)
cat > "$root/docs/solutions/apt-hash-mismatch.md" <<'MD'
# What the runner did

<!-- check-check-commands: allow the note records the command that reached the registry -->

```bash
bunx playwright install --with-deps chromium
```
MD
run_check "$root"
check "the block-level marker still exempts the block under it" "$(is "$status" 0)"

root=$(fixture marked-block-control)
cat > "$root/docs/solutions/apt-hash-mismatch.md" <<'MD'
# What the runner did

```bash
bunx playwright install --with-deps chromium
```
MD
run_check "$root"
check "the same block without the marker is judged" "$(is "$status" 1)"
check "names its line" \
  "$(contains "$out" 'docs/solutions/apt-hash-mismatch.md:4: this fenced command runs `bunx`')"

echo "case: a fenced YAML block is judged at the tool, not at the segment's first word"
# `docs/solutions/testing/a-package-no-workflow-names-has-no-ci.md` quotes three workflow steps
# this way. The segment starts `- run:`, so a gate that judged the first word could neither
# approve the wrapper nor catch the bare tool.
root=$(fixture yaml-block-wrapper)
cat > "$root/docs/solutions/wiring.md" <<'MD'
# The wiring

```yaml
      - name: Lint workspace
        run: ../../scripts/repo-tool.sh biome check src/
        working-directory: packages/workspace
```
MD
run_check "$root"
check "a run: line that reaches the tool through the wrapper passes" "$(is "$status" 0)"

root=$(fixture yaml-block-bare)
cat > "$root/docs/solutions/wiring.md" <<'MD'
# The wiring

```yaml
      - name: Lint workspace
        run: biome check src/
        working-directory: packages/workspace
```
MD
run_check "$root"
check "the same block with a bare biome fails" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'docs/solutions/wiring.md:5: this fenced command names `biome`')"

echo "case: a fenced block is judged by its info string"
# A guide quotes what a command printed about as often as it prescribes one, and what it quotes
# is the wrong form by construction — that is what a solution note records. So a block labelled
# as data is not read as commands: a `text` block holding `error: Cannot find module 'tsc'` is
# output, while a `console` block holding the same line is a session someone is being shown and
# is judged. A language the gate cannot name is data too, since guessing would report a
# transcript. The first word of the info string decides, because an info string carries
# attributes in this repository's markdown.
root=$(fixture fence-data-info)
cat > "$root/docs/solutions/transcript.md" <<'MD'
# What it printed

```text
error: Cannot find module 'tsc'
bunx biome check .
```

```json
{ "scripts": { "lint": "bunx biome check ." } }
```

```ts
const command = "bunx playwright test";
```

```frobnicate
bunx biome check .
```
MD
run_check "$root"
check "a block labelled as data, or in a language the gate cannot name, is not judged" \
  "$(is "$status" 0)"

root=$(fixture fence-console-info)
cat > "$root/docs/solutions/transcript.md" <<'MD'
# What it printed

```console
error: Cannot find module 'tsc'
```
MD
run_check "$root"
check "a console session holding the very same line is judged" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'docs/solutions/transcript.md:4: this fenced command names `tsc`')"

root=$(fixture fence-no-info)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```
bunx biome check .
```
MD
run_check "$root"
check "a block with no info string at all is judged" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'AGENTS.md:4: this fenced command runs `bunx`')"

root=$(fixture fence-info-attributes)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```bash title="scripts/release.sh"
bunx biome check .
```
MD
run_check "$root"
check "an info string carrying attributes is judged by its first word" "$(is "$status" 1)"
check "names the line inside the block" \
  "$(contains "$out" 'AGENTS.md:4: this fenced command runs `bunx`')"

echo "case: a session block is judged at its prompts, where it writes like a session"
# A command session is the one judged block whose convention interleaves commands with the
# output they printed, and it is how this repository writes one: a `$ …dev-broker.sh` and then
# thirty lines of what it printed (docs/site/src/content/docs/broker/guides/run-locally.md).
# Reading every line of such a block reports a transcript as a command, which is a report that
# is simply untrue — the very thing the narrowing to judged info strings exists to avoid. So a
# session block that carries a prompt is judged at its prompted lines and nowhere else, while
# one that carries none has no convention to read and is judged line by line, as a block with no
# info string is. The labels stay judged rather than being dropped, because `$ bunx biome
# check .` under one is a command a reader copies.
root=$(fixture fence-session-prompted)
cat > "$root/docs/solutions/session.md" <<'MD'
# What it printed

```console
$ bun run typecheck
error: Cannot find module 'tsc'
tsc --noEmit
```

```sh
bunx biome check .
```
MD
run_check "$root"
check "the shell block beside the session is judged" "$(is "$status" 1)"
check "names the line inside it" \
  "$(contains "$out" 'docs/solutions/session.md:10: this fenced command runs `bunx`')"
check "what the session printed is not a command" \
  "$(is "$(contains "$out" 'session.md:5')" false)"
check "nor is the line of output that looks exactly like one" \
  "$(is "$(contains "$out" 'session.md:6')" false)"
check "so only the shell block is reported" "$(contains "$out" '1 place(s)')"

root=$(fixture fence-session-transcript)
cat > "$root/docs/solutions/transcript-session.md" <<'MD'
# What it printed

```console
$ bun run typecheck
error: Cannot find module 'tsc'
bunx biome check .
```
MD
run_check "$root"
check "a session that only quotes what it printed is judged nowhere and passes" \
  "$(is "$status" 0)"

root=$(fixture fence-session-prompted-command)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```console
$ bunx biome check .
error: Cannot find module 'tsc'
```
MD
run_check "$root"
check "the command at a session's prompt is judged" "$(is "$status" 1)"
check "names the prompt's own line" \
  "$(contains "$out" 'AGENTS.md:4: this fenced command runs `bunx`')"
check "and the output under it is reported as nothing" "$(contains "$out" '1 place(s)')"

root=$(fixture fence-session-unprompted-next)
cat > "$root/docs/solutions/two-sessions.md" <<'MD'
# Two sessions

```console
$ bun run lint
checked 1 file
```

```console
bunx biome check .
```
MD
run_check "$root"
check "a second session block of its own, using no prompt, is judged line by line" \
  "$(is "$status" 1)"
check "names the line inside it" \
  "$(contains "$out" 'docs/solutions/two-sessions.md:9: this fenced command runs `bunx`')"
check "the prompt of the block above it does not reach this one" "$(contains "$out" '1 place(s)')"

echo "case: the pinned binary is the other approved spelling"
root=$(fixture pinned-path)
cat >> "$root/scripts/release.sh" <<'SH'
./node_modules/.bin/biome check .
node_modules/.bin/tsc --noEmit
SH
run_check "$root"
check "a path into node_modules/.bin is the file the lockfile installed" "$(is "$status" 0)"

echo "case: whatever stands in front of the tool, the tool is what runs"
# The prefix set only looks closed: a runner, a prefix word, an option, a quote, a `sh -c` and a
# variable holding the runner are the same shape, and each of these reaches the squat or the
# image's own compiler. None of them is judged by its first word any more.
root=$(fixture tool-behind-anything)
cat >> "$root/scripts/release.sh" <<'SH'
exec biome check .
sh -c 'bunx biome check .'
xargs bunx biome check
timeout 60 npx tsc --noEmit
sudo npx playwright test
FOO=1 exec env BAR=2 nice -n 5 bunx biome check .
$RUNNER biome check .
SH
run_check "$root"
check "every one of them fails" "$(is "$status" 1)"
check "exec hides nothing" "$(contains "$out" 'scripts/release.sh:5: this line names `biome`')"
check "a quoted sh -c hides nothing" \
  "$(contains "$out" 'scripts/release.sh:6: this line names `biome`')"
check "xargs hides nothing" "$(contains "$out" 'scripts/release.sh:7: this line names `biome`')"
check "timeout and its option hide nothing" \
  "$(contains "$out" 'scripts/release.sh:8: this line names `tsc`')"
check "sudo hides nothing" \
  "$(contains "$out" 'scripts/release.sh:9: this line names `playwright`')"
check "a pile of prefixes hides nothing" \
  "$(contains "$out" 'scripts/release.sh:10: this line names `biome`')"
check "a variable holding the runner is refused, not read" \
  "$(contains "$out" 'scripts/release.sh:11: this line cannot be read for certain')"
check "each line is reported once" "$(contains "$out" '7 place(s)')"

echo "case: a command the gate cannot read is refused, never waved through"
root=$(fixture shell-expanded-tool)
cat >> "$root/scripts/release.sh" <<'SH'
"$TOOL" biome check .
SH
run_check "$root"
check "an expansion that would run biome fails" "$(is "$status" 1)"
check "names the segment it cannot read" \
  "$(contains "$out" 'scripts/release.sh:5: this line cannot be read for certain: `"\$TOOL" biome check .`')"
check "names the tool the segment carries" "$(contains "$out" 'it names `biome`')"
check "offers the marker as the way to say why it is safe" \
  "$(contains "$out" 'check-check-commands: allow <reason>')"

root=$(fixture shell-expanded-harmless)
cat >> "$root/scripts/release.sh" <<'SH'
"$ROOT/scripts/release.sh" --dry-run
${BUN} run lint
SH
run_check "$root"
check "an expansion naming no pinned tool passes: the rule stays that narrow" "$(is "$status" 0)"

root=$(fixture shell-expanded-exempt)
cat >> "$root/scripts/release.sh" <<'SH'
"$TOOL" biome check . # check-check-commands: allow TOOL is the wrapper, set above
SH
run_check "$root"
check "an unreadable command exempted with a reason passes" "$(is "$status" 0)"

echo "case: a guide nested deeper than a package's own top"
root=$(fixture nested-guide)
mkdir -p "$root/packages/envoy/infra"
cat > "$root/packages/envoy/infra/AGENTS.md" <<'MD'
# Infra

```bash
bunx biome check .
```
MD
run_check "$root"
check "an AGENTS.md three directories down is judged" "$(is "$status" 1)"
check "names the line inside its block" \
  "$(contains "$out" 'packages/envoy/infra/AGENTS.md:4: this fenced command runs `bunx`')"

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

echo "case: the marker is read from the line's comment, not from anywhere on the line"
# The gate used to search the raw line for the marker, before any tokenizing, so a marker
# written inside a quoted string bought a pass for everything else on that line: a guide
# demonstrating how to write an exemption, or a script that greps for the marker, exempted the
# command beside it. The marker is a comment's business, and `#` inside a string is a character.
root=$(fixture marker-quoted-before-the-command)
cat >> "$root/scripts/release.sh" <<'SH'
echo "see # check-check-commands: allow docs" && bunx biome check .
SH
run_check "$root"
check "a marker quoted in a string exempts nothing on its line" "$(is "$status" 1)"
check "names the line and the runner" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `bunx`, a package runner')"

root=$(fixture marker-quoted-after-the-command)
cat >> "$root/scripts/release.sh" <<'SH'
bunx biome check . && echo "# check-check-commands: allow z"
SH
run_check "$root"
check "a marker quoted after the command exempts nothing either" "$(is "$status" 1)"
check "names the line and the runner" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `bunx`, a package runner')"

echo "case: the comment is where the shell ends the command, backslashes and all"
# Both of these are lines a shell reads one way and this gate used to read another. The first is
# the fail-open, where everything else here fails closed: `split_comment` stepped over no escape,
# so it closed the string at the `\"`, read the rest of the line as a comment, and handed `judge`
# the command half alone — `sh -c 'echo "a \" # b" && echo RAN'` prints `a " # b` and then `RAN`,
# while the gate saw a comment and exited 0. The second is the other direction: an apostrophe
# inside an unquoted word opens a quote that never closes, which swallowed the line's whole tail
# into a string, hiding the comment the reader plainly sees — and with it any marker it carries.
root=$(fixture comment-escaped-quote)
cat >> "$root/scripts/release.sh" <<'SH'
echo "a \" # b" && bunx biome check .
SH
run_check "$root"
check "a backslash-escaped quote does not end the string, so the \`&&\` after it is a command" \
  "$(is "$status" 1)"
check "names the line and the runner" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `bunx`, a package runner')"

# A line whose quotes do not balance is judged whole, so the gate reads every word of it rather
# than guessing where its comment begins: this one is reported, and the author quotes the
# apostrophe or writes the marker. Nothing of a line this gate cannot split goes unread.
root=$(fixture comment-unclosed-quote)
cat >> "$root/scripts/release.sh" <<'SH'
echo don't # bunx biome check .
SH
run_check "$root"
check "an apostrophe opening a quote that never closes leaves nothing of the line unjudged" \
  "$(is "$status" 1)"
check "naming the tool the guess would have hidden" \
  "$(contains "$out" 'scripts/release.sh:5: this line names `biome`')"

root=$(fixture comment-unclosed-quote-ordinary)
cat >> "$root/scripts/release.sh" <<'SH'
jq -r '.items[]
awk '
SH
run_check "$root"
check "and an unbalanced quote that is only a multi-line command passes, as 92 scripts here do" \
  "$(is "$status" 0)"

root=$(fixture comment-escaped-quote-exempt)
cat >> "$root/scripts/release.sh" <<'SH'
echo "a \" b" && bunx biome check . # check-check-commands: allow the string is the point here
SH
run_check "$root"
check "and the marker is still read from the comment of such a line" "$(is "$status" 0)"

echo "case: a file that is not text names itself"
root=$(fixture undecodable)
printf '# A note\n\xff\xfe binary\n' > "$root/docs/solutions/rogue.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, without a traceback" \
  "$(contains "$out" 'docs/solutions/rogue.md is not readable as text')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a file the walk finds and no one can open names itself"
# `walk()` keeps symlinked files, so a dangling one reaches `read_text`. It used to end the
# required job with a `FileNotFoundError` traceback whose line numbers are the heredoc's, which
# is both a crash for everyone and a report naming no file.
root=$(fixture dangling-symlink)
ln -s missing-target.sh "$root/scripts/dangling.sh"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file it cannot read, without a traceback" \
  "$(contains "$out" 'scripts/dangling.sh cannot be read: \[Errno 2\] No such file or directory')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a directory the walk cannot list names itself"
# One level up from the dangling symlink above: `read_text` was guarded and `iterdir` was not, so
# a directory no one may list ended the required lint job in a raw `PermissionError` traceback
# naming no file at all — the crash this gate exists not to be.
root=$(fixture unreadable-directory)
mkdir -p "$root/docs/sealed"
chmod 000 "$root/docs/sealed"
run_check "$root"
# Restored before the assertions, so the suite's own `rm -rf` of its temporary directory can
# still take this tree away however the case goes.
chmod 755 "$root/docs/sealed"
check "fails" "$(is "$status" 1)"
check "names the directory it cannot read, without a traceback" \
  "$(contains "$out" 'docs/sealed cannot be read: \[Errno 13\] Permission denied')"
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

echo "case: a package manager's subcommand decides what the words after it are"
# The branch that treats a manager's arguments as non-commands swallowed every segment whose
# subcommand was not the one runner spelling tabled, so `pnpm exec biome check .` and
# `yarn biome check .` — a runner and a tool, as plainly as `bunx biome` is — both passed. Each
# of the four cases is a case here: a runner subcommand, a pinned tool's own basename, the
# ordinary subcommands this repository prescribes, and a subcommand the gate does not know,
# which is refused rather than waved through.
root=$(fixture shell-manager-runners)
cat >> "$root/scripts/release.sh" <<'SH'
pnpm exec biome check .
yarn exec biome check .
yarn biome check .
npm exec biome -- check .
pnpm dlx tsc --noEmit
SH
run_check "$root"
check "every runner subcommand and the tool yarn runs itself fail" "$(is "$status" 1)"
check "pnpm exec is a runner" \
  "$(contains "$out" 'scripts/release.sh:5: this line runs `pnpm exec`, a package runner')"
check "yarn exec is a runner" \
  "$(contains "$out" 'scripts/release.sh:6: this line runs `yarn exec`, a package runner')"
check "yarn <tool> is the tool, judged by the tool rule" \
  "$(contains "$out" 'scripts/release.sh:7: this line names `biome`')"
check "npm exec is a runner" \
  "$(contains "$out" 'scripts/release.sh:8: this line runs `npm exec`, a package runner')"
check "pnpm dlx is a runner" \
  "$(contains "$out" 'scripts/release.sh:9: this line runs `pnpm dlx`, a package runner')"
check "each line is reported once" "$(contains "$out" '5 place(s)')"

root=$(fixture shell-manager-ordinary)
cat >> "$root/scripts/release.sh" <<'SH'
bun run lint
bun test
bun install --frozen-lockfile
npm view biome version
npm ci
SH
run_check "$root"
check "the ordinary subcommands this repository prescribes still pass" "$(is "$status" 0)"

root=$(fixture shell-unknown-subcommand)
cat >> "$root/scripts/release.sh" <<'SH'
bun frobnicate biome check .
SH
run_check "$root"
check "a subcommand the gate does not know, in a segment naming a tool, fails" \
  "$(is "$status" 1)"
check "names the segment it cannot read and the subcommand" \
  "$(contains "$out" 'scripts/release.sh:5: this line cannot be read for certain: `bun frobnicate biome check .` runs `bun frobnicate`, a subcommand this gate does not know')"
check "names the tool the segment carries" "$(contains "$out" 'it names `biome`')"
check "offers the marker as the way to say why it is safe" \
  "$(contains "$out" 'check-check-commands: allow <reason>')"

root=$(fixture shell-unknown-subcommand-harmless)
cat >> "$root/scripts/release.sh" <<'SH'
bun frobnicate --help
SH
run_check "$root"
check "the same unknown subcommand naming no pinned tool passes: the rule stays that narrow" \
  "$(is "$status" 0)"

echo "case: the whole of every sentence a report prints"
# Every case above asserts `file:line` and the first clause and nothing after it, so a sentence
# could say something untrue of the line it was printed for and no case would notice: that is
# how `which biome` came to be reported as a line that runs `biome`, "whatever PATH answers
# with". Each sentence this gate builds is held here word for word.
root=$(fixture sentence-names-a-tool)
cat >> "$root/scripts/release.sh" <<'SH'
which biome
grep -rn biome package.json
rm -rf ~/.cache/biome
SH
run_check "$root"
check "a line that names a tool as an argument is reported" "$(is "$status" 1)"
sentence='this line names `biome` where this gate reads a command, and reaches it neither '
sentence+='through `scripts/repo-tool.sh` nor as the pinned '
sentence+='`node_modules/.bin/biome` — a name reached any other way is answered by whatever PATH holds '
sentence+='(the worker image ships its own tsc, and with no `node_modules` the name reaches a squatted '
sentence+='package) rather than the biome this repository pins, so run `scripts/repo-tool.sh biome` or '
sentence+='the pinned `node_modules/.bin/biome`, or write `check-check-commands: allow <reason>` to say '
sentence+='why this line does not run biome at all, or cannot reach it that way'
check "the whole sentence is true of \`which biome\`, which names the tool and runs none" \
  "$(contains "$out" "scripts/release.sh:5: $sentence")"
check "and of \`grep -rn biome package.json\`" "$(contains "$out" "scripts/release.sh:6: $sentence")"
check "and of \`rm -rf ~/.cache/biome\`, named for the tool rather than the path it deletes" \
  "$(contains "$out" "scripts/release.sh:7: $sentence")"

root=$(fixture sentence-package-runner)
cat >> "$root/scripts/release.sh" <<'SH'
bunx biome check .
SH
run_check "$root"
check "a package runner is reported" "$(is "$status" 1)"
sentence='this line runs `bunx`, a package runner that resolves the name it is given from the npm '
sentence+='registry rather than this repository'\''s lockfile (with no `node_modules`, `bunx biome` '
sentence+='fetches a squatted package that checks nothing and exits 0) — run '
sentence+='`scripts/repo-tool.sh <tool>` instead'
check "the whole of the package-runner sentence" \
  "$(contains "$out" "scripts/release.sh:5: $sentence")"

root=$(fixture sentence-expansion)
cat >> "$root/scripts/release.sh" <<'SH'
"$TOOL" biome check .
SH
run_check "$root"
check "a command that begins with an expansion is refused" "$(is "$status" 1)"
sentence='this line cannot be read for certain: `"\$TOOL" biome check .` starts with a shell '
sentence+='expansion, so what it runs is not knowable here, and it names `biome` — spell the command '
sentence+='out as `scripts/repo-tool.sh biome`, or write `check-check-commands: allow <reason>` to say '
sentence+='why what this expands to is safe'
check "the whole of the cannot-be-read sentence" \
  "$(contains "$out" "scripts/release.sh:5: $sentence")"

root=$(fixture sentence-unknown-subcommand)
cat >> "$root/scripts/release.sh" <<'SH'
bun frobnicate biome check .
SH
run_check "$root"
check "a subcommand the gate does not know is refused" "$(is "$status" 1)"
sentence='this line cannot be read for certain: `bun frobnicate biome check .` runs `bun frobnicate`, '
sentence+='a subcommand this gate does not know, so whether the words after it are commands or that '
sentence+='subcommand'\''s own arguments is not knowable here, and it names `biome` — spell the command '
sentence+='out as `scripts/repo-tool.sh biome`, or write `check-check-commands: allow <reason>` to say '
sentence+='why this line is safe'
check "the whole of the sentence an unknown subcommand gets" \
  "$(contains "$out" "scripts/release.sh:5: $sentence")"

root=$(fixture sentence-exemption-line)
cat >> "$root/scripts/release.sh" <<'SH'
bunx biome check . # check-check-commands: allow
SH
run_check "$root"
check "a line exemption with no reason is reported" "$(is "$status" 1)"
check "the whole of the sentence it gets" \
  "$(contains "$out" 'scripts/release.sh:5: this exemption gives no reason — write `# check-check-commands: allow <reason>`')"

root=$(fixture sentence-exemption-block)
cat > "$root/AGENTS.md" <<'MD'
# Commands

<!-- check-check-commands: allow -->

```bash
bun run lint .
```
MD
run_check "$root"
check "a block exemption with no reason is reported" "$(is "$status" 1)"
check "the whole of the sentence it gets" \
  "$(contains "$out" 'AGENTS.md:3: this exemption gives no reason — write `<!-- check-check-commands: allow <reason> -->`')"

echo "case: a session block that never closes judges every line after it"
# `prompted` concluded from the first prompt it saw, whether or not the block ever closed, so an
# unclosed session block set the prompts-only skip for every line to the end of the file — the
# lines of a later, correctly fenced block among them. A gate that stops reading a file and
# answers green for it is this issue's own defect, in the gate built to end it.
root=$(fixture fence-session-unclosed)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```console
$ bun run lint .
output

bunx biome check .
MD
run_check "$root"
check "a block with no closing fence has no convention to read" "$(is "$status" 1)"
check "so its unprompted line is judged" \
  "$(contains "$out" 'AGENTS.md:7: this fenced command runs `bunx`')"

root=$(fixture fence-session-unclosed-then-block)
cat > "$root/AGENTS.md" <<'MD'
# Commands

```console
$ bun run lint .
output

```sh
bunx biome check .
```
MD
run_check "$root"
check "and a fence a writer opened under it ends the convention too" "$(is "$status" 1)"
check "naming the command in the block they meant to open" \
  "$(contains "$out" 'AGENTS.md:8: this fenced command runs `bunx`')"

summary "check-check-commands.sh"
