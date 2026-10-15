#!/usr/bin/env bash
# Tests for `check-release-credentials.sh`: a job of the three release workflows that holds a
# credential runs no dependency code, uses only allow-listed actions, restores no cache and holds
# the deploy key only in the env of a step that pushes; a job that runs dependency code holds no
# credential and ends in the no-credential tripwire; every checkout persists no credential; and
# the two release orderings (pi_legion_publish after pi_envoy_publish, release_publish in its
# concurrency group) stay. The check is a tripwire over named patterns (the commands, actions and
# paths its lists hold), not a proof that no other code runs, and these cases exercise those
# patterns, on each side of each line.
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
# setup-bun, a `go build` behind `&&`, upload, tripwire), two credentialed publish jobs (one with
# setup-node and `npm publish`), a plain job that runs no dependency code and a reusable-workflow
# call with no steps. The listener: a docker build job (`packages: write`, a build-push) and
# release_publish in its concurrency group, with `docker buildx imagetools create`. Infra: a
# build job (install-pulumi, `pulumi package get-schema`, tripwire) and a publish job.
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
          file="sjawhar-pi-envoy-$VERSION.tgz"
          tag=$(.github/scripts/release-push.sh handed pi-envoy-v "$VERSION" tarball "$file")
          { echo "tag=$tag"; echo "file=$file"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          FILE: ${{ steps.handed.outputs.file }}
        run: npm publish "tarball/$FILE" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: .github/scripts/release-push.sh push-tag "$TAG"
      - name: Push version bump
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          VERSION: ${{ needs.pi_envoy_build.outputs.version }}
        run: .github/scripts/release-push.sh push-main packages/pi-envoy "$VERSION"
  pi_legion_publish:
    needs: [pi_legion_build, pi_envoy_build, pi_envoy_publish]
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
          file="sjawhar-pi-legion-$VERSION.tgz"
          tag=$(.github/scripts/release-push.sh handed pi-legion-v "$VERSION" tarball "$file")
          { echo "tag=$tag"; echo "file=$file"; } >> "$GITHUB_OUTPUT"
      - name: Publish to npm
        env:
          FILE: ${{ steps.handed.outputs.file }}
        run: npm publish "tarball/$FILE" --access public --provenance
      - name: Push tag
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          TAG: ${{ steps.handed.outputs.tag }}
        run: .github/scripts/release-push.sh push-tag "$TAG"
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

summary "check-release-credentials.sh"
