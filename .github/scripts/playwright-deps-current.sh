#!/usr/bin/env bash
# Has the pinned apt list of ci.yaml's "Install Playwright Chromium, WebKit and Firefox" step
# fallen behind Playwright? That step installs Playwright's whole ubuntu24.04-x64 list less four
# media packages, and this script is what keeps that subtraction honest: `playwright install-deps
# --dry-run` simulates Playwright's list against what the step has just installed, and every
# package it reports missing must be one the closure of those four accounts for. A package
# outside that closure is a list that has fallen behind, and this fails on it.
#
# The four dropped names live here now, not in the workflow: the step's `packages=` list is
# Playwright's list less them, and this script is the only other reader of them.
#
# Its one caller is that step, which runs it from packages/dispatch after `bunx playwright
# install chromium webkit firefox`, so both sides are read from the same state. It reads no file
# and changes no directory: bunx resolves Playwright from the caller's working directory.
#
# Run from anywhere, with no arguments: .github/scripts/playwright-deps-current.sh
# CI runs it in ci.yaml's dispatch job, and its tests in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

# The four media packages the install step drops from Playwright's list. Their closure is what
# the missing set Playwright reports is expected to be, package for package.
dropped_packages=(gstreamer1.0-libav gstreamer1.0-plugins-bad gstreamer1.0-plugins-good libavcodec60)

# Playwright exits 1 when anything is missing, which is the normal case here, and non-zero for
# its own failures too. This script has errexit, so the status is discarded deliberately and the
# output is what is read; stderr is part of what it reports, so both streams are captured.
dry_run=$(bunx playwright install-deps --dry-run chromium webkit firefox 2>&1) || true
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

# What that missing set is expected to be: the closure of the four dropped packages. A simulation
# needs no root. The status is taken before any pipe, because the caller's shell has errexit and
# no pipefail: a simulation that failed would otherwise leave an empty closure and blame the
# pinned list for apt's failure.
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

declare -A reported=()
unexpected=()
for name in ${missing[@]+"${missing[@]}"}; do
  if [ -z "${closure[$name]:-}" ] && [ -z "${reported[$name]:-}" ]; then
    reported[$name]=1
    unexpected+=("$name")
  fi
done

if [ "${#unexpected[@]}" -gt 0 ]; then
  echo "::error::Playwright now needs system packages the install step's list does not hold:" \
    "${unexpected[*]} - re-derive packages= in .github/workflows/ci.yaml from playwright-core's" \
    "lib/coreBundle.js, as the comment above that step says."
  exit 1
fi

echo "The pinned list is current: every package Playwright reports missing is in the closure of" \
  "the four media packages the install step drops."
