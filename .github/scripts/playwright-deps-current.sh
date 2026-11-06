#!/usr/bin/env bash
# Has the pinned apt list of ci.yaml's "Install Playwright Chromium, WebKit and Firefox" step
# fallen behind Playwright? That step installs Playwright's whole ubuntu24.04-x64 list less four
# media packages, and this script is what keeps that subtraction honest. It holds names against
# names: every package Playwright asks for must be one the pinned list holds or one of the four.
# It then holds what `playwright install-deps --dry-run` reports missing against the apt closure
# of those four. Either half failing is a list that has fallen behind, and this fails on it.
#
# Both halves are needed. A package Playwright adds can be a dependency of one of the four - on
# noble, gstreamer1.0-plugins-good pulls in libsoup-3.0-0, libgudev-1.0-0 and libxtst6 - so the
# closure accounts for names the step never installs, and only the pinned list decides them.
#
# `dropped_packages` below is the list of four this check uses; the comment above the step's
# `packages=` names the same four for a reader of the workflow.
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
    "would be unexpected, so the check would fail for the wrong reason"
  exit 1
fi

# The four media packages the install step drops from Playwright's list. Their closure is what
# the missing set Playwright reports is expected to be, package for package.
dropped_packages=(gstreamer1.0-libav gstreamer1.0-plugins-bad gstreamer1.0-plugins-good libavcodec60)

# What Playwright is allowed to ask for: the pinned list this was called with, and the four the
# step drops on purpose.
declare -A accounted=()
for name in "$@" "${dropped_packages[@]}"; do
  accounted[$name]=1
done

# Playwright's own list is printed nowhere: `install-deps --dry-run` hands it to `apt-get install
# -s --no-install-recommends` and prints only what that simulation reports missing. Recording
# that call is how the list is read, so the dry run alone gets an `apt-get` of this script's own
# first on PATH: it appends its arguments to a file and hands the call to the real apt-get, whose
# path is resolved here, before PATH is changed.
real_apt_get=$(command -v apt-get) || {
  echo "::error::apt-get is not on PATH, so neither Playwright's dry run nor the simulated" \
    "install of the four media packages the install step drops can be made"
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

# Playwright exits 1 when anything is missing, which is the normal case here, and non-zero for
# its own failures too. This script has errexit, so the status is discarded deliberately and the
# output is what is read; stderr is part of what it reports, so both streams are captured.
dry_run=$(PATH="$shim_dir:$PATH" bunx playwright install-deps --dry-run chromium webkit firefox 2>&1) || true
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

# Playwright's list, out of the recorded call: the words after `install -s
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

unaccounted=()
for name in ${asked[@]+"${asked[@]}"}; do
  if [ -z "${accounted[$name]:-}" ]; then
    unaccounted+=("$name")
  fi
done

# What the missing set is expected to be: the closure of the four dropped packages. A simulation
# needs no root, and it runs with the real PATH, so the shim above records the dry run's call and
# nothing else. The `|| { … }` is there to print apt's own output and an error naming the
# simulation that failed, in place of the empty closure a failed simulation would otherwise leave
# to blame the pinned list for apt's failure.
simulated=$(apt-get install -s --no-install-recommends "${dropped_packages[@]}" 2>&1) || {
  echo "$simulated"
  echo "::error::the simulated install of the four media packages the install step drops" \
    "(${dropped_packages[*]}) failed, so the comparison cannot be made: what Playwright reports" \
    "missing cannot be held against a closure that was never read"
  exit 1
}
declare -A closure=()
while IFS= read -r name; do
  if [ -n "$name" ]; then
    closure[$name]=1
  fi
done < <(awk '$1 == "Inst" { print $2 }' <<<"$simulated")

unexpected=()
for name in ${missing[@]+"${missing[@]}"}; do
  if [ -z "${closure[$name]:-}" ]; then
    unexpected+=("$name")
  fi
done

# Both halves are reported, so one run names everything a re-derivation has to account for.
behind=false
if [ "${#unaccounted[@]}" -gt 0 ]; then
  echo "::error::Playwright asks for system packages the install step neither pins nor drops:" \
    "${unaccounted[*]} - re-derive packages= in .github/workflows/ci.yaml from playwright-core's" \
    "lib/coreBundle.js, as the comment above that step says."
  behind=true
fi
if [ "${#unexpected[@]}" -gt 0 ]; then
  echo "::error::Playwright now needs system packages the install step's list does not hold:" \
    "${unexpected[*]} - re-derive packages= in .github/workflows/ci.yaml from playwright-core's" \
    "lib/coreBundle.js, as the comment above that step says."
  behind=true
fi
if [ "$behind" = true ]; then
  exit 1
fi

echo "The pinned list is current: Playwright asks for no package beyond it and the four media" \
  "packages the install step drops, and every package Playwright reports missing is in the" \
  "closure of those four."
