#!/usr/bin/env bash
# Tests for `playwright-deps-current.sh`: the pinned apt list of ci.yaml's Playwright install
# step still accounts for everything Playwright reports missing, and a dry run this script
# cannot read fails rather than passing as a clean one.
#
# NO CASE REACHES THE NETWORK OR APT. Every case puts a stand-in `bunx` and a stand-in `apt-get`
# first on PATH, each reading the environment variables the case sets - PLAYWRIGHT_STDOUT,
# PLAYWRIGHT_STDERR and PLAYWRIGHT_STATUS for the dry run, APT_SIM_STDOUT and APT_SIM_STATUS for
# the simulation - to decide what to print and what status to exit with. The script under test is
# the real file CI runs, called by path; nothing here copies it.
#
# Run from anywhere: .github/scripts/playwright-deps-current.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# The stubs are written inside command substitutions nowhere, but the harness reads the script's
# output into variables: without this, a failure inside one of those substitutions would be
# reported on as though it were the script's answer.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/playwright-deps-current.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir -p "$work/bin"

# The stand-in Playwright. It answers only the dry run the script makes, so a script that asked
# for anything else - another subcommand, another browser set - fails here rather than being
# handed a missing set it never requested.
cat > "$work/bin/bunx" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
expected="playwright install-deps --dry-run chromium webkit firefox"
if [ "$*" != "$expected" ]; then
  echo "the stand-in bunx was asked for '$*', not '$expected'" >&2
  exit 64
fi
[ -z "${PLAYWRIGHT_STDOUT:-}" ] || printf '%s\n' "$PLAYWRIGHT_STDOUT"
[ -z "${PLAYWRIGHT_STDERR:-}" ] || printf '%s\n' "$PLAYWRIGHT_STDERR" >&2
exit "${PLAYWRIGHT_STATUS:-0}"
SH

# The stand-in apt-get. It answers only the simulation of the four dropped packages, in the exact
# shape the script asks for it, so a script that dropped `-s` and asked to install for real would
# fail here instead of passing.
cat > "$work/bin/apt-get" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
expected="install -s --no-install-recommends gstreamer1.0-libav gstreamer1.0-plugins-bad gstreamer1.0-plugins-good libavcodec60"
if [ "$*" != "$expected" ]; then
  echo "the stand-in apt-get was asked for '$*', not '$expected'" >&2
  exit 64
fi
[ -z "${APT_SIM_STDOUT:-}" ] || printf '%s\n' "$APT_SIM_STDOUT"
exit "${APT_SIM_STATUS:-0}"
SH

chmod +x "$work/bin/bunx" "$work/bin/apt-get"

# The closure of the four dropped packages every case simulates: three names, in the shape
# `apt-get install -s` prints them.
CLOSURE='NOTE: This is only a simulation!
Inst libavcodec60 (7:6.1.1-3ubuntu5 Ubuntu:24.04/noble [amd64])
Inst gstreamer1.0-libav (1.24.2-1 Ubuntu:24.04/noble [amd64])
Inst libswscale7 (7:6.1.1-3ubuntu5 Ubuntu:24.04/noble [amd64])
Conf libavcodec60 (7:6.1.1-3ubuntu5 Ubuntu:24.04/noble [amd64])'

# run_check: runs the real script with the stubs first on PATH and the case's answers in the
# environment, into $out and $status.
run_check() { # run_check [NAME=VALUE ...]
  set +e
  out=$(env PATH="$work/bin:$PATH" PLAYWRIGHT_STDOUT= PLAYWRIGHT_STDERR= PLAYWRIGHT_STATUS=0 \
    APT_SIM_STDOUT="$CLOSURE" APT_SIM_STATUS=0 "$@" "$check_script" 2>&1)
  status=$?
  set -e
}

echo "case: the pinned list is current"
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (3):
  libavcodec60
  gstreamer1.0-libav
  libswscale7
Please install them with the following command:'
check "exits 0" "$(is "$status" 0)"
check "says the list is current" "$(contains "$out" 'The pinned list is current')"

echo "case: the heading without its colon"
# The tester's finding, and this case is its lock: the old guard accepted this heading with one
# reader and parsed it with another, so it read an empty missing set, said the list was current
# and exited 0 while libnewthing9 was absent.
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (3)
  libavcodec60
  gstreamer1.0-libav
  libnewthing9'
check "exits 1" "$(is "$status" 1)"
check "says it failed before it could check" "$(contains "$out" 'failed before it could check the pinned list')"
check "never says the list is current" "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: a package Playwright now needs that the list does not hold"
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (4):
  libavcodec60
  gstreamer1.0-libav
  libswscale7
  libnewthing9'
check "exits 1" "$(is "$status" 1)"
check "names the package and the fix" \
  "$(contains "$out" 'Playwright now needs system packages the install step.s list does not hold: libnewthing9')"
check "names where the list is derived from" "$(contains "$out" "lib/coreBundle.js")"

echo "case: the count disagrees with the heading"
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (5):
  libavcodec60
  gstreamer1.0-libav
  libswscale7'
check "exits 1" "$(is "$status" 1)"
check "names both numbers" \
  "$(contains "$out" 'says 5 system dependencies are missing and this script read 3 package names')"

echo "case: the dry run broke before it could simulate"
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDERR='error: Cannot find module "playwright"'
check "exits 1" "$(is "$status" 1)"
check "says it failed before it could check" "$(contains "$out" 'failed before it could check the pinned list')"
check "carries Playwright's own output" "$(contains "$out" 'Cannot find module')"

echo "case: the simulation of the dropped packages failed"
run_check APT_SIM_STATUS=100 APT_SIM_STDOUT='E: Unable to locate package gstreamer1.0-libav' \
  PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (3):
  libavcodec60
  gstreamer1.0-libav
  libswscale7'
check "exits 1" "$(is "$status" 1)"
check "says the comparison cannot be made" "$(contains "$out" 'the comparison cannot be made')"
check "carries apt's own output" "$(contains "$out" 'Unable to locate package')"

echo "case: all system dependencies are installed"
run_check PLAYWRIGHT_STATUS=0 PLAYWRIGHT_STDOUT='All system dependencies are installed.'
check "exits 0" "$(is "$status" 0)"
check "says the list is current" "$(contains "$out" 'The pinned list is current')"

summary "playwright-deps-current.sh"
