#!/usr/bin/env bash
# Tests for `check-bun-workspace-manifests.sh`: a frozen Bun install must have every root workspace
# manifest in its own Docker stage, and the check must say what it inspected.
#
# Run from anywhere: .github/scripts/check-bun-workspace-manifests.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-bun-workspace-manifests.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/alpha" "$root/packages/beta" "$root/docker"
  ln -s "$check_script" "$root/.github/scripts/check-bun-workspace-manifests.sh"
  cat > "$root/package.json" <<'JSON'
{
  "workspaces": ["packages/alpha", "packages/beta"]
}
JSON
  printf '{"name":"alpha"}\n' > "$root/packages/alpha/package.json"
  printf '{"name":"beta"}\n' > "$root/packages/beta/package.json"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-bun-workspace-manifests.sh" 2>&1)
  status=$?
  set -e
}

write_passing_dockerfile() {
  local root=$1
  cat > "$root/docker/worker.Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14 AS cli
WORKDIR /repo
COPY package.json bun.lock ./
COPY packages/alpha/package.json packages/alpha/package.json
COPY packages/beta/package.json packages/beta/package.json
RUN bun install --frozen-lockfile
DOCKERFILE
}

echo "case: every workspace manifest is copied before the frozen install"
root=$(fixture green)
write_passing_dockerfile "$root"
cat > "$root/docker/no-install.Dockerfile" <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN echo no-bun-install
DOCKERFILE
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names the checked frozen-install Dockerfile and workspace count" \
  "$(contains "$out" 'docker/worker.Dockerfile: checked 2 root workspaces before bun install --frozen-lockfile')"
check "names the Dockerfile with no frozen install" \
  "$(contains "$out" 'docker/no-install.Dockerfile: checked 2 root workspaces; no bun install --frozen-lockfile')"

echo "case: a missing workspace manifest is rejected"
root=$(fixture missing-manifest)
write_passing_dockerfile "$root"
sed -i '/packages\/beta\/package.json/d' "$root/docker/worker.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile" "$(contains "$out" 'docker/worker.Dockerfile')"
check "names the missing workspace manifest" \
  "$(contains "$out" 'missing COPY for packages/beta/package.json before bun install --frozen-lockfile')"

echo "case: a manifest copied after the frozen install is rejected"
root=$(fixture copied-too-late)
write_passing_dockerfile "$root"
sed -i '/packages\/beta\/package.json/d' "$root/docker/worker.Dockerfile"
cat >> "$root/docker/worker.Dockerfile" <<'DOCKERFILE'
COPY packages/beta/package.json packages/beta/package.json
DOCKERFILE
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "still names the manifest copied too late" \
  "$(contains "$out" 'missing COPY for packages/beta/package.json before bun install --frozen-lockfile')"

echo "case: a COPY whose JSON-array form cannot be decoded fails loudly"
root=$(fixture malformed-json-copy)
cat > "$root/docker/worker.Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14 AS cli
WORKDIR /repo
COPY package.json bun.lock ./
COPY packages/alpha/package.json packages/alpha/package.json
COPY ["packages/beta/package.json", "packages/beta/package.json"
RUN bun install --frozen-lockfile
DOCKERFILE
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile and the line of the COPY it could not read" \
  "$(contains "$out" 'docker/worker.Dockerfile:5: malformed JSON-array COPY')"

echo "case: a COPY whose shell form cannot be split fails loudly"
root=$(fixture unparseable-shell-copy)
cat > "$root/docker/worker.Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14 AS cli
WORKDIR /repo
COPY package.json bun.lock ./
COPY packages/alpha/package.json packages/alpha/package.json
COPY "packages/beta/package.json packages/beta/package.json
RUN bun install --frozen-lockfile
DOCKERFILE
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the Dockerfile and the line of the COPY it could not read" \
  "$(contains "$out" 'docker/worker.Dockerfile:5: unparseable COPY')"

echo "case: a tree where the check covers no frozen install at all is a failure"
root=$(fixture zero-covered)
cat > "$root/docker/worker.Dockerfile" <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN echo no-bun-install
DOCKERFILE
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says nothing was covered" \
  "$(contains "$out" 'no Dockerfile runs bun install --frozen-lockfile')"

summary "check-bun-workspace-manifests.sh"
