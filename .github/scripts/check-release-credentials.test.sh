#!/usr/bin/env bash
# Tests for `check-release-credentials.sh`: a job of the three release workflows that holds a
# credential runs no dependency code, uses only allow-listed actions, restores no cache and holds
# the deploy key only in the env of a step that pushes; a job that runs dependency code holds no
# credential and ends in the no-credential tripwire; every checkout persists no credential; the
# two release orderings (pi_legion_publish after pi_envoy_publish, release_publish in its
# concurrency group) stay; and a job condition downstream of a skippable job calls `always()` and
# tests the result of every job whose outputs it reads. The check is a tripwire over named
# patterns (the commands, actions and paths its lists hold), not a proof that no other code runs,
# and these cases exercise those patterns, on each side of each line.
#
# Each case builds a miniature repository under a temporary directory, with the real script
# symlinked into it and a green trio of release workflows written, and mutates one file.
#
# Run from anywhere: .github/scripts/check-release-credentials.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-release-credentials.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree with the script linked in and the green trio written. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-release-credentials.sh"
  write_green "$root/.github/workflows"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-release-credentials.sh" 2>&1)
  status=$?
  set -e
}

# mutate <file> <old> <new> [nth]: replaces the nth (default: the only) literal occurrence of <old>
# with <new>. A fixture without that occurrence fails the run, so a case cannot pass by mutating
# nothing.
mutate() {
  python3 - "$1" "$2" "$3" "${4:-0}" <<'PY'
import sys

path, old, new, nth = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
text = open(path).read()
count = text.count(old)
if nth == 0 and count != 1:
    sys.exit(f"{path}: expected exactly one occurrence of {old!r}, found {count}")
if nth > count:
    sys.exit(f"{path}: expected at least {nth} occurrence(s) of {old!r}, found {count}")
start = -1
for _ in range(max(nth, 1)):
    start = text.index(old, start + 1)
open(path, "w").write(text[:start] + new + text[start + len(old):])
PY
}

# line_of <file> <regex>: the 1-based line of the first match, for the `<file>:<line>:` checks.
line_of() {
  grep -n -m1 -- "$2" "$1" | cut -d: -f1
}

SHA=3d3c42e5aac5ba805825da76410c181273ba90b1
RELEASE=.github/workflows/release.yaml
LISTENER=.github/workflows/release-envoy-listener.yaml
INFRA=.github/workflows/release-envoy-infra.yaml

# write_green <workflows dir>: the trio in its intended shape. release.yaml: a build job (install,
# setup-bun, a `go build` behind `&&`, upload, tripwire) with an `if:` of its own, two credentialed
# publish jobs (setup-node, `npm publish` of the handed tarball, the tag push, a GitHub release and
# the bump push) whose `if:` calls `always()` and tests the result of the build whose outputs it
# reads, a plain job that runs no dependency code and a reusable-workflow call with no steps and no
# `if:`. The listener: a docker build job (`packages: write`, a build-push) and release_publish in
# its concurrency group, with `docker buildx imagetools create`. Infra: a build job
# (install-pulumi, `pulumi package get-schema`, tripwire) and a publish job.
write_green() {
  local dir=$1
  cat > "$dir/release.yaml" <<'YAML'
name: Release Packages
on:
  push:
    branches: [main]
permissions: {}
jobs:
  pi_envoy_build:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    if: github.ref == 'refs/heads/main'
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: ./.github/actions/setup-bun
      - name: Install dependencies
        run: bun install --frozen-lockfile
      - name: Build the daemon
        run: cd packages/daemon && go build ./cmd/legion
      - uses: actions/upload-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-envoy-tarball
          path: packages/pi-envoy/*.tgz
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  pi_envoy_publish:
    needs: pi_envoy_build
    if: always() && needs.pi_envoy_build.result == 'success' && needs.pi_envoy_build.outputs.skip != 'true'
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-node@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          node-version: "24"
          registry-url: "https://registry.npmjs.org"
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-envoy-tarball
          path: tarball
      - name: Check the hand-over
        id: handed
        env:
          VERSION: ${{ needs.pi_envoy_build.outputs.version }}
        run: |
          stem="sjawhar-pi-envoy-$VERSION"
          tag=$(.github/scripts/release-push.sh handed pi-envoy-v "$VERSION" tarball "$stem.tgz")
          { echo "tag=$tag"; echo "stem=$stem"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          STEM: ${{ steps.handed.outputs.stem }}
        run: npm publish "tarball/$STEM.tgz" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: .github/scripts/release-push.sh push-tag "$TAG"
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          TAG: ${{ steps.handed.outputs.tag }}
          STEM: ${{ steps.handed.outputs.stem }}
        run: .github/scripts/release-push.sh release "$TAG" "Pi Envoy v1" "tarball/$STEM.tgz"
      - name: Push version bump
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          VERSION: ${{ needs.pi_envoy_build.outputs.version }}
        run: .github/scripts/release-push.sh push-main packages/pi-envoy "$VERSION"
  pi_legion_publish:
    needs: [pi_legion_build, pi_envoy_build, pi_envoy_publish]
    # pi_legion_build names no job of this fixture: the checker ignores a dangling needs name on
    # purpose (GitHub refuses such a workflow at parse).
    if: always() && needs.pi_legion_build.result == 'success' && needs.pi_legion_build.outputs.skip != 'true' && (needs.pi_envoy_publish.result == 'success' || needs.pi_envoy_publish.result == 'skipped')
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-node@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          node-version: "24"
          registry-url: "https://registry.npmjs.org"
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: pi-legion-tarball
          path: tarball
      - name: Check the hand-over
        id: handed
        env:
          VERSION: ${{ needs.pi_legion_build.outputs.version }}
        run: |
          stem="sjawhar-pi-legion-$VERSION"
          tag=$(.github/scripts/release-push.sh handed pi-legion-v "$VERSION" tarball "$stem.tgz")
          { echo "tag=$tag"; echo "stem=$stem"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          STEM: ${{ steps.handed.outputs.stem }}
        run: npm publish "tarball/$STEM.tgz" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: .github/scripts/release-push.sh push-tag "$TAG"
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          TAG: ${{ steps.handed.outputs.tag }}
          STEM: ${{ steps.handed.outputs.stem }}
        run: .github/scripts/release-push.sh release "$TAG" "Pi Legion v1" "tarball/$STEM.tgz"
      - name: Push version bump
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          VERSION: ${{ needs.pi_legion_build.outputs.version }}
        run: .github/scripts/release-push.sh push-main packages/pi-legion "$VERSION"
  changes:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - run: echo hello
  worker_image:
    needs: [pi_legion_publish]
    uses: ./.github/workflows/worker-image.yaml
    permissions:
      contents: read
      packages: write
      id-token: write
      attestations: write
YAML
  cat > "$dir/release-envoy-listener.yaml" <<'YAML'
name: Release Envoy Listener
on:
  push:
    branches: [main]
permissions: {}
jobs:
  docker:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          persist-credentials: false
      - uses: docker/setup-qemu-action@3d3c42e5aac5ba805825da76410c181273ba90b1
      - uses: docker/login-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          context: .
          push: true
          tags: ghcr.io/sjawhar/legion/envoy:${{ github.sha }}
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  release_publish:
    needs: docker
    concurrency:
      group: release-envoy-listener
      cancel-in-progress: false
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      packages: write
      id-token: write
      attestations: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: docker/login-action@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Advance latest image
        run: docker buildx imagetools create --tag x y
      - name: Tag release
        env:
          TAG: legion-envoy-v1.0.0
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
        run: |
          git tag "$TAG"
          .github/scripts/release-push.sh push-tag "$TAG"
YAML
  cat > "$dir/release-envoy-infra.yaml" <<'YAML'
name: Release envoy-infra
on:
  push:
    branches: [main]
permissions: {}
jobs:
  build:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: ./.github/actions/install-pulumi
      - name: Smoke test
        run: pulumi package get-schema "$GITHUB_WORKSPACE/out/linux-amd64/pulumi-resource-envoy-infra"
      - uses: actions/upload-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: envoy-infra-assets
          path: assets/
      - name: Check this job holds no credential (tripwire)
        run: .github/scripts/release-push.sh no-credential
  publish:
    needs: build
    runs-on: ubuntu-24.04
    permissions:
      contents: write
      id-token: write
      attestations: write
    cache-mode: none
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/download-artifact@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          name: envoy-infra-assets
          path: assets
      - name: Tag release
        env:
          TAG: envoy-infra-v1.0.0
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
        run: .github/scripts/release-push.sh push-tag "$TAG"
      - uses: actions/attest-build-provenance@3d3c42e5aac5ba805825da76410c181273ba90b1
        with:
          subject-path: assets/*.tar.gz
      - name: Create GitHub release
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: .github/scripts/release-push.sh release "$TAG" "envoy-infra v1" assets/*.tar.gz
YAML
}

# add_publish_step <root> <step yaml lines>: a step before pi_envoy_publish's "Publish to npm".
add_publish_step() {
  mutate "$1/$RELEASE" $'      - name: Publish to npm\n' "$2"$'      - name: Publish to npm\n' 1
}

# add_push_tag_step <root> <name> <run lines>: add_publish_step of a step that holds the deploy key,
# whose `run:` is the given lines (each indented ten spaces, newline-ended) and then the push-tag
# call that allows the key there, so the check's verdict rests on the lines alone.
add_push_tag_step() {
  add_publish_step "$1" "      - name: $2"$'\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n'"$3"$'          .github/scripts/release-push.sh push-tag "$TAG"\n'
}

echo "case: the trio in its intended shape"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" \
  "$(contains "$out" 'check-release-credentials: 4 credentialed job(s) and 3 build job(s) across 3 release workflows keep credentials out of jobs that run dependency code')"
green_release=$(cat "$root/$RELEASE")
green_listener=$(cat "$root/$LISTENER")
check "a credentialed job runs release-push.sh handed" "$(contains "$green_release" 'release-push.sh handed pi-envoy-v')"
check "a credentialed job runs npm publish" "$(contains "$green_release" 'run: npm publish')"
check "a credentialed job runs docker buildx imagetools create" "$(contains "$green_listener" 'run: docker buildx imagetools create')"
check "a build job holds packages: write" "$(contains "$green_listener" 'packages: write')"
check "a build job carries an if: of its own, so the publish jobs downstream have a skippable ancestor" \
  "$(contains "$green_release" "^    if: github.ref == 'refs/heads/main'$")"
check "a publish job's if: calls always() and tests the result of the build whose outputs it reads" \
  "$(contains "$green_release" "^    if: always() && needs.pi_envoy_build.result == 'success' && needs.pi_envoy_build.outputs.skip != 'true'$")"
check "the green fixture needs a job it does not define, pi_legion_build (a dangling needs name is ignored on purpose)" \
  "$(contains "$green_release" '^    needs: \[pi_legion_build, ')"
check "no job pi_legion_build" "$(is "$(contains "$green_release" '^  pi_legion_build:$')" false)"

echo "case: a credentialed job runs bun install"
root=$(fixture bun-install)
add_publish_step "$root" $'      - name: Install the publish dependencies\n        run: bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the step and the command at the step's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - name: Install the publish dependencies$'): job 'pi_envoy_publish' holds a credential, but step 'Install the publish dependencies' runs 'bun'")"

echo "case: go behind && in a credentialed job"
root=$(fixture go-after-and)
add_publish_step "$root" $'      - name: Build the daemon\n        run: cd packages/daemon && go build ./cmd/legion\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names go" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Build the daemon' runs 'go'")"

echo "case: a built binary under out/ in a credentialed job"
root=$(fixture out-path)
add_publish_step "$root" $'      - name: Smoke test\n        run: out/legion version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Smoke test' runs 'out/legion'")"

echo "case: a built binary under \$GITHUB_WORKSPACE/out/ in a credentialed job"
root=$(fixture out-workspace-path)
add_publish_step "$root" $'      - name: Smoke test\n        run: |\n          "$GITHUB_WORKSPACE/out/legion" version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Smoke test' runs '.GITHUB_WORKSPACE/out/legion'")"

echo "case: a scripts/ program in a credentialed job"
root=$(fixture scripts-path)
add_publish_step "$root" $'      - name: Notices\n        run: scripts/foo.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Notices' runs 'scripts/foo.sh'")"

echo "case: a scripts/ program through ../../ in a credentialed job"
root=$(fixture scripts-relative-path)
add_publish_step "$root" $'      - name: Notices\n        run: cd packages/daemon && ../../scripts/foo.sh out\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path" "$(contains "$out" "step 'Notices' runs '../../scripts/foo.sh'")"

echo "case: bun in a comment-only line and in a trailing comment"
root=$(fixture comment-bun)
add_publish_step "$root" $'      - name: Comments\n        run: |\n          # bun install would be dependency code\n          echo ok # bun install\n'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: npm install, npm ci and npm exec in a credentialed job (npm view and npm publish stay allowed)"
root=$(fixture npm-dependency-subcommands)
add_publish_step "$root" $'      - name: Install the publish dependencies\n        run: npm ci\n      - name: Install one more\n        run: npm install --no-save left-pad\n      - name: Run a tool\n        run: npm exec -- some-tool\n      - name: Look up the version\n        run: npm view "@sjawhar/pi-envoy@1.0.0" version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names npm ci" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Install the publish dependencies' runs 'npm ci'")"
check "names npm install" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Install one more' runs 'npm install'")"
check "names npm exec" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Run a tool' runs 'npm exec'")"
check "does not name npm view" "$(is "$(contains "$out" "step 'Look up the version'")" false)"
check "does not name npm publish" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: a scripts/ program run through an interpreter in a credentialed job"
root=$(fixture scripts-via-interpreter)
add_publish_step "$root" $'      - name: Notices\n        run: bash scripts/go-third-party-notices.sh out/NOTICES ./cmd/legion\n      - name: Notices under sh\n        run: sh scripts/foo.sh\n      - name: Source a helper\n        run: source scripts/lib/helpers.sh\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script behind bash" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Notices' runs 'scripts/go-third-party-notices.sh'")"
check "names the script behind sh" "$(contains "$out" "step 'Notices under sh' runs 'scripts/foo.sh'")"
check "names the sourced script" "$(contains "$out" "step 'Source a helper' runs 'scripts/lib/helpers.sh'")"

echo "case: a step whose shell is an interpreter in a credentialed job"
root=$(fixture shell-python)
add_publish_step "$root" $'      - name: Helper\n        shell: python\n        run: |\n          import subprocess\n          subprocess.run(["bun", "install"], check=True)\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step and its shell" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Helper' .*'python'")"

echo "case: docker build and docker buildx build in a credentialed job"
root=$(fixture docker-build)
add_publish_step "$root" $'      - name: Build image\n        run: docker build .\n      - name: Build image with buildx\n        run: docker buildx build .\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker build" "$(contains "$out" "step 'Build image' runs 'docker build'")"
check "names docker buildx build" "$(contains "$out" "step 'Build image with buildx' runs 'docker buildx build'")"

echo "case: a credentialed job sets up Go"
root=$(fixture setup-go)
add_publish_step "$root" "      - uses: actions/setup-go@$SHA"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the action as dependency code" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'actions/setup-go@$SHA' uses 'actions/setup-go@$SHA'; dependency code")"

echo "case: a credentialed job uses an action off the allow-list"
root=$(fixture other-action)
add_publish_step "$root" "      - uses: some/other-action@$SHA"$'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the allow-list" \
  "$(contains "$out" "step 'some/other-action@$SHA' uses 'some/other-action@$SHA', which is not on the checker's allow-list for a job that holds a credential; add it there deliberately")"

echo "case: a credentialed job without cache-mode"
root=$(fixture no-cache-mode)
mutate "$root/$RELEASE" $'    cache-mode: none\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  pi_envoy_publish:$'): job 'pi_envoy_publish' holds a credential, so it must set 'cache-mode: none'")"

echo "case: a credentialed job with cache-mode: read"
root=$(fixture cache-mode-read)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: read\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the value" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, so it must set 'cache-mode: none' to restore no cache (found \"read\")")"

echo "case: a checkout without persist-credentials"
root=$(fixture checkout-no-persist)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: actions/checkout@'): job 'pi_envoy_build' step 'actions/checkout@$SHA' must set 'persist-credentials: false', the boolean, so no token is left in the checkout (it is not set)")"

echo "case: a checkout with the string \"false\""
root=$(fixture checkout-persist-string)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: "false"\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the string at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'persist-credentials: "false"'): job 'pi_envoy_build' step 'actions/checkout@$SHA' must set 'persist-credentials: false', the boolean.*(found \"false\", a string)")"

echo "case: a checkout with persist-credentials: true"
root=$(fixture checkout-persist-true)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: true\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the value" "$(contains "$out" "must set 'persist-credentials: false', the boolean.*(found true)")"

echo "case: the deploy key as a checkout's ssh-key"
root=$(fixture ssh-key)
mutate "$root/$RELEASE" $'          persist-credentials: false\n' $'          persist-credentials: false\n          ssh-key: ${{ secrets.DEPLOY_KEY }}\n' 2
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'ssh-key:'): job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'actions/checkout@$SHA' with.ssh-key; the deploy key may appear only as 'DEPLOY_KEY: \${{ secrets.DEPLOY_KEY }}' in the env of a step whose run calls release-push.sh push-tag or push-main")"

echo "case: the deploy key in a job-level env"
root=$(fixture job-env-key)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: none\n    env:\n      DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at env.DEPLOY_KEY;")"

echo "case: the deploy key in the env of a step that runs release-push.sh release"
root=$(fixture release-step-key)
add_publish_step "$root" $'      - name: Create GitHub release\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: .github/scripts/release-push.sh release "$TAG" "Pi Envoy v1" "tarball/$FILE"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'Create GitHub release' env.DEPLOY_KEY;")"

echo "case: the deploy key under another env key of a push-tag step"
root=$(fixture other-env-key)
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          OTHER_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key" "$(contains "$out" "job 'pi_envoy_publish' names secrets.DEPLOY_KEY at step 'Push tag' env.OTHER_KEY;")"

echo "case: the deploy key named by index, secrets['DEPLOY_KEY']"
root=$(fixture deploy-key-index-syntax)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          KEY: ${{ secrets[\'DEPLOY_KEY\'] }}\n        run: bun install --frozen-lockfile\n' 1
add_publish_step "$root" $'      - name: Create GitHub release\n        env:\n          KEY: ${{ secrets["DEPLOY_KEY"] }}\n        run: .github/scripts/release-push.sh release "$TAG" "Pi Envoy v1" "tarball/$FILE"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the build job's text" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names .*DEPLOY_KEY")"
check "names the step of the credentialed job" "$(contains "$out" "job 'pi_envoy_publish' names .*DEPLOY_KEY at step 'Create GitHub release' env.KEY")"

echo "case: a build job holding id-token: write"
root=$(fixture build-id-token)
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' $'    permissions:\n      contents: read\n      id-token: write\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the scope at the permissions line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    permissions:$'): job 'pi_envoy_build' runs dependency code (step './.github/actions/setup-bun' uses './.github/actions/setup-bun'), but holds 'id-token: write'")"

echo "case: a build job naming the deploy key"
root=$(fixture build-deploy-key)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n        run: bun install --frozen-lockfile\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the text" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.DEPLOY_KEY")"

echo "case: a build job with no permissions block under a workflow granting contents: write"
root=$(fixture build-inherits-write)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions:\n  contents: write\n'
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the inherited scope at the workflow's permissions line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^permissions:$'): job 'pi_envoy_build' runs dependency code (.*), but holds 'contents: write' through the workflow's permissions, having none of its own")"

echo "case: a build job with no permissions block in a workflow with none"
root=$(fixture build-no-permissions-anywhere)
mutate "$root/$RELEASE" $'permissions: {}\n' ''
mutate "$root/$RELEASE" $'    permissions:\n      contents: read\n' '' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, whose token is whatever the repository grants a job that declares no permissions" \
  "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but .*permissions")"

tripwire=$'      - name: Check this job holds no credential (tripwire)\n        run: .github/scripts/release-push.sh no-credential\n'

echo "case: a build job whose tripwire is not its last step"
root=$(fixture tripwire-moved)
mutate "$root/$RELEASE" "$tripwire" ''
mutate "$root/$RELEASE" $'      - uses: actions/upload-artifact@' "$tripwire"$'      - uses: actions/upload-artifact@' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the last step at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: actions/upload-artifact@'): job 'pi_envoy_build' runs dependency code (.*), so its last step must be the tripwire 'run: .github/scripts/release-push.sh no-credential'")"

echo "case: a tripwire step with an if"
root=$(fixture tripwire-if)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        if: always()\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the if at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        if: always()$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must carry no 'if:'")"

echo "case: a build job with no tripwire"
root=$(fixture no-tripwire)
mutate "$root/$RELEASE" "$tripwire" ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "wants the tripwire last" "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), so its last step must be the tripwire")"

echo "case: pi_legion_publish not needing pi_envoy_publish"
root=$(fixture needs-lacks-envoy)
mutate "$root/$RELEASE" $'    needs: [pi_legion_build, pi_envoy_build, pi_envoy_publish]\n' $'    needs: [pi_legion_build, pi_envoy_build]\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the needs at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    needs: \[pi_legion_build, pi_envoy_build\]$'): job 'pi_legion_publish' must need 'pi_envoy_publish'")"

echo "case: no pi_legion_publish"
root=$(fixture no-pi-legion-publish)
mutate "$root/$RELEASE" $'  pi_legion_publish:\n' $'  pi_legion_release:\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing job" "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^jobs:$'): no job 'pi_legion_publish'")"

echo "case: release_publish with cancel-in-progress: true"
root=$(fixture cancel-true)
mutate "$root/$LISTENER" $'      cancel-in-progress: false\n' $'      cancel-in-progress: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the concurrency at its line" \
  "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^    concurrency:$'): job 'release_publish' must run in concurrency group 'release-envoy-listener' with 'cancel-in-progress: false', the boolean (found concurrency: {\"group\": \"release-envoy-listener\", \"cancel-in-progress\": true})")"

echo "case: release_publish without concurrency"
root=$(fixture no-concurrency)
mutate "$root/$LISTENER" $'    concurrency:\n      group: release-envoy-listener\n      cancel-in-progress: false\n' ''
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at its line" \
  "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^  release_publish:$'): job 'release_publish' must run in concurrency group .*(found concurrency: null)")"

echo "case: release_publish in another group"
root=$(fixture other-group)
mutate "$root/$LISTENER" $'      group: release-envoy-listener\n' $'      group: release-${{ github.ref }}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the group" "$(contains "$out" "job 'release_publish' must run in concurrency group 'release-envoy-listener'.*(found concurrency: {\"group\": \"release-\${{ github.ref }}\"")"

echo "case: no release_publish"
root=$(fixture no-release-publish)
mutate "$root/$LISTENER" $'  release_publish:\n' $'  publish:\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the missing job" "$(contains "$out" "^::error::$LISTENER:$(line_of "$root/$LISTENER" '^jobs:$'): no job 'release_publish'")"

# The green job-level conditions of the two publish jobs, for the rule-6 cases to replace.
envoy_if=$'    if: always() && needs.pi_envoy_build.result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
legion_if=$'    if: always() && needs.pi_legion_build.result == \'success\' && needs.pi_legion_build.outputs.skip != \'true\' && (needs.pi_envoy_publish.result == \'success\' || needs.pi_envoy_publish.result == \'skipped\')\n'

echo "case: a publish job's if: reads the build's outputs behind an implicit success() (the bug's shape)"
root=$(fixture if-implicit-success)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job, the missing always() and the skippable ancestor at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.outputs.skip'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own; GitHub's implicit success() is then false whenever any job upstream was skipped, whatever this if: says, so the if: must call always() and test needs.<job>.result for each job it reads")"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a publish job's if: calls success() explicitly"
root=$(fixture if-explicit-success)
mutate "$root/$RELEASE" "$envoy_if" $'    if: success() && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the explicit success() is the implicit one: names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: success()'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: tests the build's result, without always()"
root=$(fixture if-result-no-always)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the result test decides nothing behind the implicit success(): names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.result'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: tests the build's result only, reading no outputs"
root=$(fixture if-result-only)
mutate "$root/$RELEASE" "$envoy_if" $'    if: needs.pi_envoy_build.result == \'success\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_envoy_build.result'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a publish job's if: calls always() and reads the build's outputs without testing its result"
root=$(fixture if-always-no-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the outputs read and the result not tested at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs.pi_envoy_build.outputs'): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result; always() turns GitHub's own gating off, so a failed or skipped job is read through its empty outputs")"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: an if: over two needs that tests one result and reads the other's outputs (the release note's shape)"
root=$(fixture if-two-needs-one-result)
mutate "$root/$RELEASE" "$legion_if" $'    if: always() && needs.pi_envoy_publish.result == \'success\' && needs.pi_legion_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names only the job whose outputs are read untested, not the one whose result is tested" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs.pi_envoy_publish.result'): job 'pi_legion_publish' calls always() in its if: and reads needs.pi_legion_build.outputs, but does not test needs.pi_legion_build.result; always() turns")"

echo "case: a steps-less job's if: reads a skippable job's outputs behind an implicit success()"
root=$(fixture if-steps-less-job)
mutate "$root/$RELEASE" $'    needs: [pi_legion_publish]\n    uses: ./.github/workflows/worker-image.yaml\n' $'    needs: [pi_legion_publish]\n    if: needs.pi_legion_publish.outputs.version != \'\'\n    uses: ./.github/workflows/worker-image.yaml\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job and every skippable ancestor, at any distance, at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.pi_legion_publish.outputs.version'): job 'worker_image' has an if: that calls no always(), but jobs 'pi_envoy_build', 'pi_envoy_publish', 'pi_legion_publish' upstream of it through needs carry an if: of their own")"

echo "case: the build named by index, needs['pi_envoy_build'], behind always()"
root=$(fixture if-index-no-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs[\'pi_envoy_build\'].outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job read by index as by dot" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && needs\['): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result;")"
root=$(fixture if-index-with-result)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && needs[\'pi_envoy_build\'].result == \'success\' && needs[\'pi_envoy_build\'].outputs.skip != \'true\'\n'
run_check "$root"
check "exits 0: the result tested by index is the result read by dot" "$(is "$status" 0)"

echo "case: a boolean if: true"
root=$(fixture if-boolean)
mutate "$root/$RELEASE" "$envoy_if" $'    if: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a boolean calls no always(): names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: true$'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: a folded if: >- carrying the bug (the production shape)"
root=$(fixture if-folded)
mutate "$root/$RELEASE" "$envoy_if" $'    if: >-\n      needs.pi_envoy_build.result == \'success\' &&\n      needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job at the if: >- line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: >-$'): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: Always() and the needs context in another casing"
root=$(fixture if-casing)
mutate "$root/$RELEASE" "$envoy_if" $'    if: Always() && NEEDS.pi_envoy_build.Result == \'success\' && needs.pi_envoy_build.outputs.skip != \'true\'\n'
run_check "$root"
check "exits 0: the function and the context are case-insensitive" "$(is "$status" 0)"

echo "case: 'always()' in a string literal is text (a YAML double-quoted scalar, since a plain one cannot start with a quote)"
root=$(fixture if-quoted-always)
mutate "$root/$RELEASE" "$envoy_if" $'    if: "\'always()\' == needs.pi_envoy_build.outputs.skip"\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the quoted always() calls nothing: names the job at the if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" "^    if: \"'always()' == "): job 'pi_envoy_publish' has an if: that calls no always(), but job 'pi_envoy_build' upstream of it through needs carries an if: of its own")"

echo "case: the build's outputs read inside fromJSON(…), behind always()"
root=$(fixture if-fromjson)
mutate "$root/$RELEASE" "$envoy_if" $'    if: always() && fromJSON(needs.pi_envoy_build.outputs.skip) != true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the outputs read inside the call" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: always() && fromJSON('): job 'pi_envoy_publish' calls always() in its if: and reads needs.pi_envoy_build.outputs, but does not test needs.pi_envoy_build.result;")"

echo "case: two jobs needing each other (one needs: a string, one a list), each with an if: reading the other's outputs"
root=$(fixture if-needs-cycle)
cat >> "$root/$RELEASE" <<'YAML'
  cyc_a:
    needs: cyc_b
    if: needs.cyc_b.outputs.x != ''
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - run: echo a
  cyc_b:
    needs: [cyc_a]
    if: needs.cyc_a.outputs.x != ''
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - run: echo b
YAML
run_check "$root"
check "exits 1, the walk having ended" "$(is "$status" 1)"
check "names cyc_a, whose string needs names cyc_b, at its if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.cyc_b.outputs.x'): job 'cyc_a' has an if: that calls no always(), but job 'cyc_b' upstream of it through needs carries an if: of its own")"
check "names cyc_b, whose list needs names cyc_a, at its if's line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    if: needs.cyc_a.outputs.x'): job 'cyc_b' has an if: that calls no always(), but job 'cyc_a' upstream of it through needs carries an if: of its own")"
check "reports those two problems only: each job once" "$(contains "$out" '^2 release credential problem(s)$')"

echo "case: a step-level if: reading the build's outputs behind an implicit success()"
root=$(fixture step-if)
add_publish_step "$root" $'      - name: Step-level condition\n        if: needs.pi_envoy_build.outputs.skip != \'true\'\n        run: echo ok\n'
run_check "$root"
check "exits 0: a step's implicit success() is over the job's own earlier steps, not over needs" "$(is "$status" 0)"

echo "case: a missing workflow"
root=$(fixture missing-infra)
rm "$root/$INFRA"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" "$(contains "$out" "^::error::$INFRA is missing$")"

echo "case: a workflow that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: CI\non: [push\n' > "$root/$RELEASE"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" "^::error::$RELEASE is not valid YAML")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a build job holding another secret and the whole secrets context"
root=$(fixture build-other-secrets)
mutate "$root/$RELEASE" $'        run: bun install --frozen-lockfile\n' $'        env:\n          NPM_TOKEN: ${{ secrets.NPM_TOKEN }}\n          ALL: ${{ toJSON(secrets) }}\n        run: bun install --frozen-lockfile\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the secret at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'NPM_TOKEN:'): job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.NPM_TOKEN; a build job holds no secret but GITHUB_TOKEN")"
check "names the whole secrets context" \
  "$(contains "$out" "job 'pi_envoy_build' runs dependency code (.*), but its text names \${{ toJSON(secrets) }}; a build job holds no secret but GITHUB_TOKEN")"
check "the listener's docker job, whose login holds secrets.GITHUB_TOKEN, is not named" \
  "$(is "$(contains "$out" "job 'docker'")" false)"

echo "case: docker run in a credentialed job (docker buildx imagetools, docker login and docker buildx version stay allowed)"
root=$(fixture docker-run)
add_publish_step "$root" $'      - name: Smoke the image\n        run: docker run alpine sh\n      - name: Inspect the image\n        run: docker buildx imagetools inspect x && docker login ghcr.io && docker buildx version\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker run" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Smoke the image' runs 'docker run'")"
check "does not name the inspect step" "$(is "$(contains "$out" "step 'Inspect the image'")" false)"

echo "case: a checkout written Actions/Checkout, without persist-credentials"
root=$(fixture checkout-casing)
mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n        with:\n          fetch-depth: 0\n          persist-credentials: false\n' "      - uses: Actions/Checkout@$SHA"$'\n        with:\n          fetch-depth: 0\n' 1
mutate "$root/$RELEASE" "      - uses: actions/checkout@$SHA"$'\n' "      - uses: Actions/Checkout@$SHA"$'\n' 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the build job's checkout at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^      - uses: Actions/Checkout@'): job 'pi_envoy_build' step 'Actions/Checkout@$SHA' must set 'persist-credentials: false', the boolean, so no token is left in the checkout (it is not set)")"
check "the credentialed job's Actions/Checkout, with persist-credentials: false, is on the allow-list" \
  "$(is "$(contains "$out" "not on the checker's allow-list")" false)"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: the deploy key in the workflow-level env"
root=$(fixture workflow-env-key)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\nenv:\n  DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the workflow's env for the build job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  DEPLOY_KEY:'): job 'pi_envoy_build' runs dependency code (.*), but its text names secrets.DEPLOY_KEY; a build job holds no secret but GITHUB_TOKEN")"
check "names the workflow's env for a credentialed job at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^  DEPLOY_KEY:'): job 'pi_envoy_publish' names secrets.DEPLOY_KEY at the workflow's env.DEPLOY_KEY; the deploy key may appear only as 'DEPLOY_KEY: \${{ secrets.DEPLOY_KEY }}' in the env of a step whose run calls release-push.sh push-tag or push-main")"
check "names the workflow's env for the other credentialed job too" \
  "$(contains "$out" "job 'pi_legion_publish' names secrets.DEPLOY_KEY at the workflow's env.DEPLOY_KEY;")"
check "the plain job, credentialed through the workflow's env, must set cache-mode: none" \
  "$(contains "$out" "job 'changes' holds a credential, so it must set 'cache-mode: none'")"

echo "case: a workflow-level defaults.run.shell that is an interpreter"
root=$(fixture workflow-shell-python)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\ndefaults:\n  run:\n    shell: python\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Check the hand-over' 'Publish to npm' 'Push tag' 'Push version bump'; do
  check "names the credentialed job's run step '$step' under the workflow's shell" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs its body under shell 'python', not bash or sh; dependency code runs only in a job that holds none")"
done
check "does not name a step with no run" "$(is "$(contains "$out" "step 'actions/checkout@$SHA' runs its body")" false)"

echo "case: a job-level defaults.run.shell that is an interpreter"
root=$(fixture job-shell-pwsh)
mutate "$root/$RELEASE" $'    cache-mode: none\n' $'    cache-mode: none\n    defaults:\n      run:\n        shell: pwsh\n' 1
add_publish_step "$root" $'      - name: Under bash\n        shell: bash\n        run: echo ok\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Check the hand-over' 'Publish to npm' 'Push tag' 'Push version bump'; do
  check "names pi_envoy_publish's run step '$step' under the job's shell" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs its body under shell 'pwsh', not bash or sh")"
done
check "a step's own shell: bash wins over the job's" "$(is "$(contains "$out" "step 'Under bash'")" false)"
check "does not name pi_legion_publish, whose shell is bash" "$(is "$(contains "$out" "job 'pi_legion_publish'")" false)"

echo "case: the deploy key in lower case, secrets.deploy_key"
root=$(fixture deploy-key-lower-case)
mutate "$root/$RELEASE" $'      - run: echo hello\n' $'      - env:\n          KEY: ${{ secrets.deploy_key }}\n        run: echo hello\n'
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          DEPLOY_KEY: ${{ Secrets.deploy_key }}\n          TAG:' 1
mutate "$root/$RELEASE" $'          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG:' $'          deploy_key: ${{ secrets.DEPLOY_KEY }}\n          TAG:'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "the plain job is credentialed: names the site at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" 'secrets.deploy_key }}$'): job 'changes' names secrets.DEPLOY_KEY at step 1 env.KEY; the deploy key may appear only as")"
check "the plain job is credentialed: wants cache-mode: none" \
  "$(contains "$out" "job 'changes' holds a credential, so it must set 'cache-mode: none' to restore no cache (it is not set)")"
check "the value form in another casing, under DEPLOY_KEY of a push step, is allowed" \
  "$(is "$(contains "$out" "job 'pi_envoy_publish'")" false)"
check "the env key deploy_key is not DEPLOY_KEY, which release-push.sh reads" \
  "$(contains "$out" "job 'pi_legion_publish' names secrets.DEPLOY_KEY at step 'Push tag' env.deploy_key;")"

echo "case: npm publish without a tarball right after it in a credentialed job"
root=$(fixture npm-publish-directory)
add_publish_step "$root" $'      - name: Publish the directory\n        run: npm publish --access public --provenance\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the directory publish and why" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Publish the directory' runs 'npm publish' without a tarball right after it (the tarball must be the word after \`publish\`; a directory publish runs the package's prepack); dependency code runs only in a job that holds none")"
check "does not name the publish of the handed tarball" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: a tripwire step with continue-on-error"
root=$(fixture tripwire-continue-on-error)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        continue-on-error: true\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the key at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        continue-on-error: true$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must carry no 'continue-on-error:'")"

echo "case: a script behind an interpreter's -o or +O option, commands behind eval, and docker's long forms, in a credentialed job"
root=$(fixture option-flags-eval-docker-long-forms)
add_publish_step "$root" $'      - name: Behind -o\n        run: bash -o pipefail scripts/x.sh\n      - name: Behind +O\n        run: bash +O extglob scripts/y.sh\n      - name: Eval quoted\n        run: eval "bun install"\n      - name: Eval bare\n        run: eval bun install\n      - name: Docker long forms\n        run: |\n          docker container run alpine sh\n          docker image build .\n          docker buildx bake\n          docker container exec x sh\n          docker image ls\n          docker container ls\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script behind -o pipefail" "$(contains "$out" "step 'Behind -o' runs 'scripts/x.sh'")"
check "names the script behind +O extglob" "$(contains "$out" "step 'Behind +O' runs 'scripts/y.sh'")"
check "names bun behind a quoted eval" "$(contains "$out" "step 'Eval quoted' runs 'bun'")"
check "names bun behind a bare eval" "$(contains "$out" "step 'Eval bare' runs 'bun'")"
check "names docker's long forms as written, and not its listings" \
  "$(contains "$out" "step 'Docker long forms' runs 'docker container run', 'docker image build', 'docker buildx bake', 'docker container exec'; dependency code")"

echo "case: a build job with continue-on-error"
root=$(fixture build-continue-on-error)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    continue-on-error: true\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job's key at its line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    continue-on-error: true$'): job 'pi_envoy_build' runs dependency code (.*), so it must carry no 'continue-on-error:' (a true or an expression there reports a failed tripwire as success to the jobs that need it)")"
check "does not name the tripwire step, which carries none" "$(is "$(contains "$out" "tripwire step must carry no")" false)"
check "reports that one problem only" "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a build job whose defaults.run.shell is cat {0}, which prints the tripwire instead of running it"
root=$(fixture build-shell-cat)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: \'cat {0}\'\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the job's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" "^        shell: 'cat {0}'$"): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'cat'), which is what runs .github/scripts/release-push.sh no-credential")"
check "reports that one problem only: the build job's other steps may run under anything" \
  "$(contains "$out" '^1 release credential problem(s)$')"

echo "case: a workflow-level defaults.run.shell that is an interpreter reaches the build job's tripwire"
root=$(fixture workflow-shell-python-tripwire)
mutate "$root/$RELEASE" $'permissions: {}\n' $'permissions: {}\ndefaults:\n  run:\n    shell: python\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the workflow's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^    shell: python$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'python'), which is what runs .github/scripts/release-push.sh no-credential")"
check "names a credentialed job's step under the same shell" \
  "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Publish to npm' runs its body under shell 'python', not bash or sh")"

echo "case: a tripwire step with its own shell: python"
root=$(fixture tripwire-shell-python)
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        shell: python\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the tripwire's shell at the step's shell line" \
  "$(contains "$out" "^::error::$RELEASE:$(line_of "$root/$RELEASE" '^        shell: python$'): job 'pi_envoy_build' runs dependency code (.*), so its tripwire step must run under bash or sh (found 'python')")"

echo "case: a tripwire step whose own shell is a bash template under a job default of pwsh"
root=$(fixture tripwire-shell-template)
mutate "$root/$RELEASE" $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n' $'  pi_envoy_build:\n    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: pwsh\n'
mutate "$root/$RELEASE" $'        run: .github/scripts/release-push.sh no-credential\n' $'        shell: bash -e {0}\n        run: .github/scripts/release-push.sh no-credential\n'
run_check "$root"
check "exits 0: the step's bash wins over the job's pwsh" "$(is "$status" 0)"

echo "case: npm publish with a .tgz behind a flag in a credentialed job"
root=$(fixture npm-publish-tgz-behind-flag)
add_publish_step "$root" $'      - name: Publish under a tag\n        run: npm publish --tag x.tgz\n      - name: Publish with an otp\n        run: npm publish --otp 1.tgz\n      - name: Publish under a one-word tag\n        run: npm publish --tag=x.tgz\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
for step in 'Publish under a tag' 'Publish with an otp' 'Publish under a one-word tag'; do
  check "names '$step' as a publish without a tarball right after it" \
    "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step '$step' runs 'npm publish' without a tarball right after it (the tarball must be the word after \`publish\`; a directory publish runs the package's prepack)")"
done
check "does not name the publish of the handed tarball, right after publish" "$(is "$(contains "$out" "step 'Publish to npm'")" false)"

echo "case: docker's global options before its subcommand in a credentialed job"
root=$(fixture docker-global-options)
add_publish_step "$root" $'      - name: Global config\n        run: docker --config /tmp/c run alpine\n      - name: Global host\n        run: docker -H tcp://x:2375 run alpine\n      - name: Global log level\n        run: docker --log-level debug container run alpine\n      - name: Global one-word and boolean\n        run: |\n          docker --config=/tmp/c run alpine\n          docker -D build .\n      - name: Global config before imagetools\n        run: docker --config /tmp/c buildx imagetools create --tag x y\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names docker run behind --config" "$(contains "$out" "job 'pi_envoy_publish' holds a credential, but step 'Global config' runs 'docker run'")"
check "names docker run behind -H" "$(contains "$out" "step 'Global host' runs 'docker run'")"
check "names docker container run behind --log-level" "$(contains "$out" "step 'Global log level' runs 'docker container run'")"
check "names docker run behind --config=value and docker build behind -D" \
  "$(contains "$out" "step 'Global one-word and boolean' runs 'docker run', 'docker build'")"
check "does not name imagetools behind --config" "$(is "$(contains "$out" "step 'Global config before imagetools'")" false)"

echo "case: a backslash-continued command is one command, not one per line"
root=$(fixture backslash-continuation)
add_publish_step "$root" $'      - name: Hand over a long list\n        run: |\n          .github/scripts/release-push.sh handed pi-envoy-v "$VERSION" tarball \\\n            scripts/not-a-command.tgz \\\n            out/not-a-command-either.tgz\n      - name: Install across lines\n        run: |\n          bun \\\n            install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "does not read a continued argument line as a command" "$(is "$(contains "$out" "step 'Hand over a long list'")" false)"
check "still names the command a continuation starts" "$(contains "$out" "step 'Install across lines' runs 'bun'")"

echo "case: a backslash ending a comment, or an escaped backslash, continues nothing"
root=$(fixture comment-backslash)
add_publish_step "$root" $'      - name: Install behind a comment\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n          # keep the toolchain pinned \\\n          bun install --frozen-lockfile\n          .github/scripts/release-push.sh push-tag "$TAG"\n      - name: Install behind an escaped backslash\n        run: |\n          printf \'x\' \\\\\n          bun install\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command under a comment's trailing backslash" "$(contains "$out" "step 'Install behind a comment' runs 'bun'")"
check "names the command under an escaped backslash" "$(contains "$out" "step 'Install behind an escaped backslash' runs 'bun'")"

echo "case: a comment after a separator, ending in a backslash, continues nothing"
root=$(fixture separator-comment)
add_publish_step "$root" $'      - name: Push tag behind a comment\n        env:\n          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}\n          TAG: ${{ steps.handed.outputs.tag }}\n        run: |\n          .github/scripts/release-push.sh push-tag "$TAG" ;# keep the toolchain pinned \\\n          bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command under the comment's trailing backslash" "$(contains "$out" "step 'Push tag behind a comment' runs 'bun'")"

echo "case: a quote carries across newlines, so a # opening its second line is no comment"
root=$(fixture quoted-newline)
add_push_tag_step "$root" 'Install after a quoted note' $'          notes=\'Release notes:\n          # keep the toolchain pinned\' ; bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after the closing quote" "$(contains "$out" "step 'Install after a quoted note' runs 'bun'")"

echo "case: an escaped space before a #, or a continuation gluing a # to a word, makes no comment"
root=$(fixture escaped-hash)
add_push_tag_step "$root" 'Escaped space' $'          echo a\\ #; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Glued hash' $'          echo a\\\n          #;bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after an escaped space and a #" "$(contains "$out" "step 'Escaped space' runs 'bun'")"
check "names the command after a glued #" "$(contains "$out" "step 'Glued hash' runs 'bun'")"

echo "case: an odd run of backslashes before a newline continues the command"
root=$(fixture odd-backslashes)
add_push_tag_step "$root" 'Three backslashes' $'          printf \'x\' \\\\\\\n            bun\n'
run_check "$root"
check "exits 0: the bun on the next line is printf's argument" "$(is "$status" 0)"

echo "case: a # inside a word, in \${#x} or in a string that resumes behind \$(…) is no comment"
root=$(fixture hash-in-word)
add_push_tag_step "$root" 'Resumed string' $'          echo "$(date) # not a comment"; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Length expansion' $'          echo ${#x}; bun install --frozen-lockfile\n'
add_push_tag_step "$root" 'Hash mid-word' $'          echo a#b; bun install --frozen-lockfile\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command after a string that resumes behind \$(…)" "$(contains "$out" "step 'Resumed string' runs 'bun'")"
check "names the command after \${#x}" "$(contains "$out" "step 'Length expansion' runs 'bun'")"
check "names the command after a#b" "$(contains "$out" "step 'Hash mid-word' runs 'bun'")"

echo "case: a command name or a separator inside a string is text"
root=$(fixture quoted-text)
add_push_tag_step "$root" 'Quoted bun' $'          echo "bun install"\n'
add_push_tag_step "$root" 'Quoted separator' $'          echo "a; bun install"\n'
run_check "$root"
check "exits 0: bun inside a string runs nothing" "$(is "$status" 0)"

echo "case: a command substitution, by \$(…) or backticks, is a command of its own"
root=$(fixture substitution)
add_push_tag_step "$root" 'Dollar paren' $'          x=$(bun install)\n'
add_push_tag_step "$root" 'Backticks' $'          y=`bun install`\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the command inside \$(…)" "$(contains "$out" "step 'Dollar paren' runs 'bun'")"
check "names the command inside backticks" "$(contains "$out" "step 'Backticks' runs 'bun'")"

echo "case: a redirection's target is not a command; the same path in command position is"
root=$(fixture redirect-target)
add_push_tag_step "$root" 'Write a file' $'          echo x > out/x.txt\n'
run_check "$root"
check "exits 0: out/x.txt behind > is a target" "$(is "$status" 0)"
root=$(fixture redirect-command)
add_push_tag_step "$root" 'Run the file' $'          out/x.txt\n'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the path in command position" "$(contains "$out" "step 'Run the file' runs 'out/x.txt'")"

summary "check-release-credentials.sh"
