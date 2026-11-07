#!/usr/bin/env bash
# Tests for `playwright-deps-current.sh`: the pinned apt list of ci.yaml's Playwright install
# step and the list Playwright asks apt for still hold the same names, nothing is left missing
# after the install, and a dry run this script cannot read fails rather than passing as a clean
# one.
#
# NO CASE REACHES THE NETWORK OR APT. Every case puts a stand-in `bunx` and a stand-in `apt-get`
# first on PATH, each reading the environment variables the case sets - PLAYWRIGHT_STDOUT,
# PLAYWRIGHT_STDERR, PLAYWRIGHT_STATUS and PLAYWRIGHT_LIST - to decide what to print and what
# status to exit with. The stand-in apt-get answers one call, the dry run's simulation of
# PLAYWRIGHT_LIST, which the script reaches through the recording `apt-get` it puts first on
# PATH; anything else fails loudly. The script under test is the real file CI runs, called by
# path; nothing here copies it.
#
# Run from anywhere: .github/scripts/playwright-deps-current.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# This harness reads answers out of command substitutions - `$(is …)`, `$(contains …)`,
# `script_dir=$(…)` and `work=$(mktemp -d)` - and without inherit_errexit a failure inside one of
# them would be reported as though it were the answer. `run_check` is the exception that needs no
# guard: it sets `+e` around its own substitution, because the script's non-zero status is what
# most of the cases are about.
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
# handed a missing set it never requested. Before it prints, it makes the apt call the real dry
# run makes, which is where the script reads Playwright's own list from.
cat > "$work/bin/bunx" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
expected="playwright install-deps --dry-run chromium webkit firefox"
if [ "$*" != "$expected" ]; then
  echo "the stand-in bunx was asked for '$*', not '$expected'" >&2
  exit 64
fi
if [ -n "${PLAYWRIGHT_LIST:-}" ]; then
  # shellcheck disable=SC2086 # the list splits into words, as Playwright passes it
  apt-get install -s --no-install-recommends $PLAYWRIGHT_LIST > /dev/null
fi
[ -z "${PLAYWRIGHT_STDOUT:-}" ] || printf '%s\n' "$PLAYWRIGHT_STDOUT"
[ -z "${PLAYWRIGHT_STDERR:-}" ] || printf '%s\n' "$PLAYWRIGHT_STDERR" >&2
exit "${PLAYWRIGHT_STATUS:-0}"
SH

# The stand-in apt-get. It answers the one simulation the run makes and nothing else, in the
# exact shape it is asked for, so a script that dropped `-s` and asked to install for real would
# fail here instead of passing. That call reaches it through the script's recording shim, and
# only its arguments matter: the real Playwright reads apt's output itself and prints the missing
# set the case states.
cat > "$work/bin/apt-get" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ "$*" = "install -s --no-install-recommends ${PLAYWRIGHT_LIST:-}" ]; then
  exit 0
fi
echo "the stand-in apt-get was asked for '$*', which is not the dry run's simulation" >&2
exit 64
SH

chmod +x "$work/bin/bunx" "$work/bin/apt-get"

# The pinned list the cases give the script, standing in for the install step's `packages=`, and
# what the stand-in Playwright asks apt for: the same list, because the step installs Playwright's
# list whole. A case that needs the two to differ sets PLAYWRIGHT_LIST or its arguments itself.
PINNED=(libgtk-3-0t64 libnss3 xvfb)
PLAYWRIGHT_ASKS="${PINNED[*]}"

# run_check: runs the real script with the stubs first on PATH, the case's answers in the
# environment and a pinned list in its arguments, into $out and $status. Everything before a `--`
# is an environment assignment and everything after it is the pinned list; a case that passes no
# `--` is given ${PINNED[*]}, and one that passes `--` and nothing else is given no arguments.
run_check() { # run_check [NAME=VALUE ...] [-- PACKAGE ...]
  local env_args=() pinned_args=() arg after=false
  for arg in "$@"; do
    if [ "$arg" = "--" ]; then
      after=true
    elif [ "$after" = true ]; then
      pinned_args+=("$arg")
    else
      env_args+=("$arg")
    fi
  done
  if [ "$after" = false ]; then
    pinned_args=("${PINNED[@]}")
  fi
  set +e
  out=$(env PATH="$work/bin:$PATH" PLAYWRIGHT_STDOUT= PLAYWRIGHT_STDERR= PLAYWRIGHT_STATUS=0 \
    PLAYWRIGHT_LIST="$PLAYWRIGHT_ASKS" \
    ${env_args[@]+"${env_args[@]}"} "$check_script" ${pinned_args[@]+"${pinned_args[@]}"} 2>&1)
  status=$?
  set -e
}

echo "case: the pinned list is current"
# The run CI makes: the step installs Playwright's list whole, so the two lists hold the same
# names and the dry run after the install finds nothing missing.
run_check PLAYWRIGHT_STATUS=0 PLAYWRIGHT_STDOUT='All system dependencies are installed.'
check "exits 0" "$(is "$status" 0)"
check "says the list is current" "$(contains "$out" 'The pinned list is current')"

echo "case: a package Playwright now asks for that the pinned list does not hold"
# Playwright 1.64.1 does not ask for libsoup-3.0-0 on ubuntu24.04: the name is in that
# platform's lib2package map, for libsoup-3.0.so.0, and not in its tools, chromium, firefox or
# webkit list, which are what install-deps asks apt for. This case is an upgrade that adds it
# to one of those lists. The pinned list does not name it, so nothing but the first direction
# of the comparison decides it: the dry run here reports nothing missing.
run_check PLAYWRIGHT_LIST="$PLAYWRIGHT_ASKS libsoup-3.0-0" PLAYWRIGHT_STATUS=0 \
  PLAYWRIGHT_STDOUT='All system dependencies are installed.'
check "exits 1" "$(is "$status" 1)"
check "names the package and the fix" \
  "$(contains "$out" "install step's list does not pin: libsoup-3.0-0")"
check "names where the list is derived from" "$(contains "$out" "lib/coreBundle.js")"
check "never says the list is current" \
  "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: the pinned list holds a package Playwright does not ask for"
# A name left in `packages=` that Playwright has dropped: apt installs it every run and no
# browser asks for it. Only the second direction of the comparison reads it.
run_check PLAYWRIGHT_STATUS=0 PLAYWRIGHT_STDOUT='All system dependencies are installed.' \
  -- "${PINNED[@]}" libgoneby4
check "exits 1" "$(is "$status" 1)"
check "names the package" \
  "$(contains "$out" "list pins system packages Playwright does not ask for: libgoneby4")"
check "names where the list is derived from" "$(contains "$out" "lib/coreBundle.js")"
check "never says the list is current" \
  "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: both directions fail at once"
run_check PLAYWRIGHT_LIST="$PLAYWRIGHT_ASKS libsoup-3.0-0" PLAYWRIGHT_STATUS=0 \
  PLAYWRIGHT_STDOUT='All system dependencies are installed.' -- "${PINNED[@]}" libgoneby4
check "exits 1" "$(is "$status" 1)"
check "names the package the pinned list does not hold" \
  "$(contains "$out" "does not pin: libsoup-3.0-0")"
check "names the package Playwright does not ask for" \
  "$(contains "$out" "does not ask for: libgoneby4")"

echo "case: a package is still missing after the install"
# Both lists hold the same names, so the step installed this one and apt still reports it
# missing: the step did not deliver it.
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (1):
  libnss3
Please install them with the following command:'
check "exits 1" "$(is "$status" 1)"
check "names the package" \
  "$(contains "$out" 'missing after the install step installed the whole pinned list: libnss3')"
check "never says the list is current" \
  "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: the script is given no pinned list"
run_check --
check "exits 1" "$(is "$status" 1)"
check "says it takes the pinned list as its arguments" \
  "$(contains "$out" 'install step.s pinned apt list as its arguments and was given none')"

echo "case: the dry run reached no apt call"
# No recorded call, so Playwright's list was never read and the two lists cannot be compared.
run_check PLAYWRIGHT_LIST= PLAYWRIGHT_STATUS=0 \
  PLAYWRIGHT_STDOUT='All system dependencies are installed.'
check "exits 1" "$(is "$status" 1)"
check "says it never reached apt" "$(contains "$out" 'it never reached apt')"
check "never says the list is current" \
  "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: the heading without its colon"
# A heading this script does not read, over a dry run that is not a clean one: libnewthing9 is
# missing. The run has to fail, rather than leave an empty parsed set to be taken for a clean
# run and reported as a current list.
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT='Missing system dependencies (1)
  libnewthing9'
check "exits 1" "$(is "$status" 1)"
check "says it failed before it could check" \
  "$(contains "$out" 'failed before it could check the pinned list')"
check "never says the list is current" \
  "$(is "$(contains "$out" 'The pinned list is current')" false)"

echo "case: the count disagrees with the heading"
missing_lines=$(printf '  %s\n' "${PINNED[@]}")
run_check PLAYWRIGHT_STATUS=1 PLAYWRIGHT_STDOUT="Missing system dependencies (5):
$missing_lines"
check "exits 1" "$(is "$status" 1)"
check "names both numbers" \
  "$(contains "$out" \
    "says 5 system dependencies are missing and this script read ${#PINNED[@]} package names")"

echo "case: the dry run broke before it could simulate"
run_check PLAYWRIGHT_LIST= PLAYWRIGHT_STATUS=1 \
  PLAYWRIGHT_STDERR='error: Cannot find module "playwright"'
check "exits 1" "$(is "$status" 1)"
check "says it failed before it could check" \
  "$(contains "$out" 'failed before it could check the pinned list')"
check "carries Playwright's own output" "$(contains "$out" 'Cannot find module')"

summary "playwright-deps-current.sh"
