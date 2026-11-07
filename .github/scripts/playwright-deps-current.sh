#!/usr/bin/env bash
# Does the pinned apt list of ci.yaml's "Install Playwright Chromium, WebKit and Firefox" step
# still hold the same names as Playwright's own list? That step installs Playwright's whole
# ubuntu24.04-x64 list, so the two lists hold the same names, and this script holds them against
# each other both ways: every name Playwright asks for is one the pinned list holds, and every
# pinned name is one Playwright asks for. It runs after the step's install, so it also expects
# Playwright to report nothing missing. Either of the first two failing is a list that has
# fallen behind, and the third failing is an install that did not deliver the names it was
# given; this fails on any of them.
#
# The pinned list is the arguments. Playwright's own list is printed nowhere, so it is read from
# the `apt-get install -s --no-install-recommends` call its dry run makes, which the recording
# `apt-get` below captures.
#
# Its one caller is that step, which runs it from packages/dispatch after `bunx playwright
# install chromium webkit firefox`, so both sides are read from the same state. It changes no
# directory, and the one file it writes, the record of Playwright's apt call, is in a temporary
# directory it removes on exit.
#
# It takes the pinned list as its arguments, and `bunx` resolves Playwright from the caller's
# working directory, so it reads the Dispatch suite's Playwright only when it is run from
# packages/dispatch: .github/scripts/playwright-deps-current.sh $packages
# CI runs it in ci.yaml's dispatch job, and its tests in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

if [ "$#" -eq 0 ]; then
  echo "::error::playwright-deps-current.sh takes the install step's pinned apt list as its" \
    "arguments and was given none: with an empty pinned set every package Playwright asks for" \
    "would be reported as unpinned, so the check would fail for the wrong reason"
  exit 1
fi

# One side of the comparison: the pinned list this was called with, and nothing else.
declare -A pinned=()
for name in "$@"; do
  pinned[$name]=1
done

# Playwright's own list is printed nowhere: `install-deps --dry-run` hands it to `apt-get install
# -s --no-install-recommends` and prints only what that simulation reports missing. Recording
# that call is how the list is read, so the dry run alone gets an `apt-get` of this script's own
# first on PATH: it appends its arguments to a file and hands the call to the real apt-get, whose
# path is resolved here, before PATH is changed.
real_apt_get=$(command -v apt-get) || {
  echo "::error::apt-get is not on PATH, so Playwright's dry run cannot be made"
  exit 1
}
shim_dir=$(mktemp -d)
trap 'rm -rf "$shim_dir"' EXIT
recorded="$shim_dir/apt-get-argv"
: >"$recorded"
cat >"$shim_dir/apt-get" <<SH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$recorded"
exec "$real_apt_get" "\$@"
SH
chmod +x "$shim_dir/apt-get"

# Playwright exits 1 when anything is missing, and non-zero for its own failures too. This script
# has errexit, so the status is discarded deliberately and the output is what is read; stderr is
# part of what it reports, so both streams are captured.
dry_run=$(PATH="$shim_dir:$PATH" \
  bunx playwright install-deps --dry-run chromium webkit firefox 2>&1) || true
# The job log carries the whole of it: this is the record of what Playwright wanted here.
echo "$dry_run"

# One reader of Playwright's heading, not two. A heading that is not exactly
# `Missing system dependencies (<count>):` is not read as one, so an upgrade that respells it -
# the colon dropped, a word added - leaves no parsed set to mistake for a clean run. The count
# the heading states is kept, and the package names are the lines after it indented by exactly
# two spaces.
heading='^Missing system dependencies \(([0-9]+)\):$'
stated=
installed=false
reading=false
missing=()
while IFS= read -r line; do
  if [[ $line =~ $heading ]]; then
    stated=${BASH_REMATCH[1]}
    reading=true
    continue
  fi
  if [ "$line" = "All system dependencies are installed." ]; then
    installed=true
    reading=false
    continue
  fi
  if [ "$reading" = true ]; then
    if [[ $line == "  "[^[:space:]]* ]]; then
      name=${line#  }
      missing+=("${name%%[[:space:]]*}")
    else
      reading=false
    fi
  fi
done <<<"$dry_run"

if [ -z "$stated" ] && [ "$installed" = false ]; then
  echo "::error::playwright install-deps --dry-run printed neither a" \
    "'Missing system dependencies (<count>):' heading nor 'All system dependencies are" \
    "installed.', so it failed before it could check the pinned list; its output is above"
  exit 1
fi

# The second guard, independent of the first: a heading this script reads but an indent it does
# not would leave fewer names than the heading counts, and that must fail rather than compare a
# short set.
if [ -n "$stated" ] && [ "${#missing[@]}" -ne "$stated" ]; then
  echo "::error::playwright install-deps --dry-run says $stated system dependencies are missing" \
    "and this script read ${#missing[@]} package names from the lines after that heading, so the" \
    "output's shape is not the one this script reads; its output is above"
  exit 1
fi

# The other side of the comparison, out of the recorded call: the words after `install -s
# --no-install-recommends`. Playwright passes it already deduplicated.
asked=()
recorded_call=false
while IFS= read -r argv; do
  case $argv in
  "install -s --no-install-recommends "*)
    read -r -a asked <<<"${argv#install -s --no-install-recommends }"
    recorded_call=true
    ;;
  esac
done <"$recorded"

if [ "$recorded_call" = false ]; then
  echo "::error::playwright install-deps --dry-run made no 'apt-get install -s" \
    "--no-install-recommends' call, so it never reached apt and the list it asks for cannot be" \
    "read; its output is above"
  exit 1
fi

# One pass over the names Playwright asks for. It records each in `asks`, which the second
# direction below reads whole, and on the way it collects the first direction: a name
# Playwright asks for that the pinned list does not hold.
declare -A asks=()
unpinned=()
for name in ${asked[@]+"${asked[@]}"}; do
  asks[$name]=1
  if [ -z "${pinned[$name]:-}" ]; then
    unpinned+=("$name")
  fi
done

# The second direction: a pinned name Playwright does not ask for. The step installs the pinned
# list whole, so such a name is one the step installs for no reason Playwright states.
unasked=()
for name in "$@"; do
  if [ -z "${asks[$name]:-}" ]; then
    unasked+=("$name")
  fi
done

# All three comparisons are reported, so one run names every name a re-derivation of the list
# has to account for and every name the install did not deliver.
failed=false
if [ "${#unpinned[@]}" -gt 0 ]; then
  echo "::error::Playwright asks for system packages the install step's list does not pin:" \
    "${unpinned[*]} - re-derive packages= in .github/workflows/ci.yaml from playwright-core's" \
    "lib/coreBundle.js, as the comment above that step says."
  failed=true
fi
if [ "${#unasked[@]}" -gt 0 ]; then
  echo "::error::the install step's list pins system packages Playwright does not ask for:" \
    "${unasked[*]} - re-derive packages= in .github/workflows/ci.yaml from playwright-core's" \
    "lib/coreBundle.js, as the comment above that step says."
  failed=true
fi
if [ "${#missing[@]}" -gt 0 ]; then
  echo "::error::Playwright reports system packages missing after the install step installed the" \
    "whole pinned list: ${missing[*]} - the step did not deliver them, so read its apt output" \
    "above this check in the job log."
  failed=true
fi
if [ "$failed" = true ]; then
  exit 1
fi

echo "The pinned list is current: it holds the same names as the list Playwright asks apt for," \
  "and Playwright reports nothing missing after the install."
