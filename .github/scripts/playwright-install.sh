#!/usr/bin/env bash
# The Playwright install of the Dispatch browser suite: the pinned apt packages beside this
# script, then the browser engines it is given, then the check that the pinned list has not
# fallen behind Playwright.
#
# Who runs it, and why it is a script: the "Install the lane's Playwright browsers" step of
# .github/workflows/dispatch-browser.yaml's shard job and the step of that name in its alone
# job. The two jobs repeat their step lists for the reason that file's head gives - a composite
# action carries neither `timeout-minutes` nor `services:` - but that reason is a composite
# action's and not a script's: a step that calls a script keeps its own `timeout-minutes`. So
# this body is written once, both steps call it, and a fix here is a fix for both jobs.
#
# It takes the browser engines to install, one argument each, and installs no others:
#   .github/scripts/playwright-install.sh chromium webkit firefox
# They are named rather than left out because with no argument Playwright resolves its own
# default set rather than the suite's. Which engines those are is the caller's: the shard job
# reads them from the lane, whose projects its browser step names from the same lane, and the
# alone job takes its matrix project's name less the `-alone` suffix. The apt list does not
# narrow with them - it is the three-engine list on every leg, for the reason below.
#
# Run it from packages/dispatch, as both steps do through their `working-directory`: `bunx`
# resolves Playwright from the working directory, so from anywhere else this would install some
# other Playwright's browsers, or none. The pinned list and the list check are read from this
# script's own directory, so neither depends on where it was started.
#
# It has no .test.sh beside it, unlike most scripts here. Every line of it is an apt call, a
# sudo, a download or Playwright's own install, so a test with no apt and no network could
# assert nothing this file does not already say; the one thing it could reach, the refusal of a
# call that names no engine, is the four lines below it.
#
# This script installs the system packages itself instead of through `playwright install
# --with-deps`, which installs Playwright's whole ubuntu24.04-x64 list. The pinned list it reads
# from .github/scripts/playwright-packages.txt is that list less four media packages -
# gstreamer1.0-libav, gstreamer1.0-plugins-bad, gstreamer1.0-plugins-good and libavcodec60 -
# which is what makes this install small enough to bound. Measured on the runner image:
# Playwright's whole list resolves to 181 packages and 114 MB; without those four it is 60
# packages and 51.4 MB. The four carry 121 of the 181 packages and 62.8 of the 114.2 MB, through
# the ffmpeg stack and the GStreamer plugin trees.
#
# Each is safe to drop because they are plugins and codecs loaded at playback, not libraries the
# browsers link: Playwright's own source says of gstreamer1.0-libav that its one library
# libgstlibav.so is the library required for playing h.264. No spec or source file under
# packages/dispatch holds `<video`, `.mp4`, `.webm`, `new Audio` or `playsinline`, so no row
# plays media. The libraries WebKit links stay in the list: libgstreamer-plugins-bad1.0-0,
# libgstreamer-plugins-base1.0-0, libgstreamer1.0-0, libgstreamer-gl1.0-0, libflite1 and
# libgtk-4-1 among them. A row that later needs media reinstalls that package by name. That the
# browsers still launch is what the suite's six WebKit and Firefox projects prove - webkit,
# webkit-iphone, webkit-alone, firefox, firefox-download-navigation and firefox-alone
# (e2e/playwright.config.ts) - which the shard job's browser step and the alone job run.
#
# The list lives in .github/scripts/playwright-packages.txt, one package per line, and this
# script is the one reader of it, so the two machines cannot drift apart. How to re-derive it on
# a Playwright upgrade is in that file's own head comment.
#
# The 22-minute budget each calling step carries is this script's retry arithmetic, not its
# measured maximum, which the bounds make unreachable: two bounded attempts of `apt-get update`
# at 90 s, two of the download at 300 s, then, where both of those time out, one 90 s read of
# the fallback origin's indexes and a third 300 s download attempt, then the unpack, the browser
# download and the two apt simulations of the list check, none of which touch the Ubuntu mirror.
# So the arithmetic is 2 x 90 + 2 x 300 + 90 + 300 s of bounded apt, about 20 s of unpack, about
# 90 s for the browsers and about 30 s for the list check: 1310 s, rounded up to 22 min. The
# budget has to carry the fallback's own update, because the path that needs every one of those
# bounds is the path on which the step still succeeds. Healthy today is a 38 s median and a
# 102 s 95th percentile for the whole step. The apt half of that arithmetic does not move with
# the lane: the pinned list is the three-engine list on every leg, and only the browser download
# at the end narrows.
#
# Each bound is derived from its own phase: each job's two apt rates, as apt logged them, scaled
# onto the head's smaller fetch of 12.9 MB of indexes and 51.4 MB of packages. The population is
# the 205 green dispatch jobs of the tester's window (2026-11-05T08:51:42Z to 23:07:42Z), read
# in full for the 26 whose install step ran over 73 s. No shorter step can reach either bound:
# an update need over 90 s means a merge-base index fetch of 10.5 MB taking over 73 s, and a
# download need over 300 s means a merge-base fetch of 114 MB taking over 665 s.
#
# Two jobs bind the two bounds. Job 127238648049 binds the download: apt logged
# `Fetched 114 MB in 10min 13s (186 kB/s)` for its packages, so the head's 51.4 MB need 276.3 s
# there, and 300 s leaves 8.6 per cent of headroom. Job 127357441704 binds the update: apt
# logged `Fetched 10.5 MB in 45s (233 kB/s)` for its `apt-get update`, so the head's 12.9 MB
# need 55.4 s there, and 90 s leaves 63 per cent. On this model the two bounds fail none of the
# 205.
#
# One blended rate per job is the wrong model, as job 127238648049's own log shows:
# `Fetched 10.5 MB in 1s (9797 kB/s)` for its update and the 186 kB/s above for its packages.
# One rate over both phases mixes a fast update into a slow download and understates the
# download. The model assumes a retry meets the same mirror rate, and azure.archive.ubuntu.com
# is a pool, so a retry may meet a faster mirror or a slower one. Both bounds come from the
# slowest mirror a green job of that window met, and are read again when a green job meets a
# slower one. Under about 143 kB/s the indexes do not arrive inside one bound, and under about
# 171 kB/s the packages do not: job 127274913129 met 62.5 kB/s for its packages, and its job
# budget cancelled it.
#
# What the three attempts complete, which is the threshold this script carries: three bounds of
# 300 s are 900 s of download, so it completes above about 57 kB/s, and under that it fails by
# its calling step's name - in about 900 s where both `apt-get update` phases were fast, and in
# at most 1170 s where each was retried to its bound. Two attempts would have completed only
# above about 86 kB/s, which is why there are three: at 62.5 kB/s the head's 51.4 MB need 822 s,
# which 600 s of download does not reach and 900 s does, and job 127274913129 above met exactly
# that rate.
#
# The attempts accumulate, which is measured and not assumed: `apt-get ... -d` resumes. An
# attempt the bound kills leaves its bytes in apt's partial directory, and the attempt after it
# asks only for the rest, which that attempt logs as `Need to get 42.1 MB/51.4 MB of archives`
# after a first attempt that had asked for 51.4 MB. Measured separately, the partial is resumed
# across the origin switch too, since the two mirrors serve the same file and the server answers
# a byte-range request. Where the fallback's snapshot holds a different version the filename
# differs, the partial is not reused and that attempt carries the full 51.4 MB, which at any
# healthy rate is seconds.
#
# Why the third attempt changes origin rather than the bound being raised: a longer bound buys a
# longer hang and hides the mirror. One step's log holds both of the rates the one host it met
# served, 9977 kB/s for the indexes and 31 kB/s for the pool files, so the slowness was in the
# pool path rather than in the host, and another origin is the lever that answers that. A single
# bad pool member is the case no attempt count on one origin reaches.
#
# The slowest reading this repository holds is that 31 kB/s: 9.3 MB fetched in a 300 s attempt.
# No attempt count worth having completes 51.4 MB at that rate - it needs 1658 s - so the step
# fails by its own name there, which is the intended outcome.
#
# The worst case this script now carries: if both `apt-get update` attempts fail, it stops after
# about 180 s, so the longest the bounded apt phases can run is one update attempt stopped at
# 90 s, a second ending just inside 90 s, two download attempts stopped at 300 s, the fallback
# origin's update stopped at 90 s and a third download attempt stopped at 300 s. The step fails
# by its own name in at most 1170 s, against the 40-minute job cancel it replaces.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

if [ "$#" -eq 0 ]; then
  echo "::error::playwright-install.sh takes the browser engines to install as its arguments" \
    "and was given none: with no engine named, Playwright resolves its own default set rather" \
    "than the suite's, so a lane would install browsers nothing on the machine launches:" \
    ".github/scripts/playwright-install.sh <engine>..."
  exit 1
fi

# The runner image preconfigures Google's chrome-stable apt source, whose index intermittently
# fails `apt-get update` with a hash-sum mismatch and takes the install down with it. Chrome is
# already installed there; the source only serves upgrades, so drop it before installing our
# deps. The `|| true` is this script's `pipefail`: a `grep` that matches nothing exits 1, which
# GitHub's default `bash -e {0}`, with no `pipefail`, read past because the pipeline's status
# was xargs's.
{ grep -rl dl.google.com /etc/apt/sources.list.d || true; } | xargs -r sudo rm -f

# The pinned list, read from the repository rather than written here, so both jobs install the
# same set. It sits beside this script and is read from there, so it is found however the
# calling step's working-directory is set.
#
# The `#` head comment and the blank lines are stripped here rather than left to apt: the shell
# reads this file and $packages is unquoted below, so every word of a comment line would
# otherwise reach apt as a package name (`E: Unable to locate package subtracting`, which failed
# every leg of run 42314019622).
packages=$(grep -vE '^[[:space:]]*(#|$)' "$script_dir/playwright-packages.txt")

# A bounded attempt, retried: the attempt count, then what the log calls the phase, then the
# command. Each failed attempt prints the phase, that attempt's exit status and how many
# attempts are left; `timeout` exits 124 when it killed the command, so the status separates a
# mirror that stalled from one that refused. What an exhausted phase means is the caller's: the
# update below ends the step, and the download moves to another origin and takes its last
# attempt there.
attempts() {
  local left=$1 what=$2 status
  shift 2
  while :; do
    "$@" && return 0
    status=$?
    left=$((left - 1))
    echo "::warning::$what failed with exit $status (124 is the timeout); attempts left: $left"
    [ "$left" -gt 0 ] || return 1
  done
}

# `sudo timeout`, not `timeout sudo`: the signal has to reach apt-get itself, not a sudo holding
# it.
attempts 2 "apt-get update" sudo timeout 90 apt-get update || {
  echo "::error::apt-get update failed twice, so no package list was read;" \
    "the apt mirror is the suspect"
  exit 1
}

# Download only (`-d`): a killed apt releases its fcntl locks either way, but with nothing
# unpacked in this phase no dpkg run is ever stopped part way, so the attempts after it and the
# unbounded install below start from a clean package state. It prints what it fetched, which is
# this step's own byte record. $packages is left unquoted on purpose, to split into words.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
if ! attempts 2 "the package download" sudo timeout 300 apt-get install -y --no-install-recommends -d $packages; then
  # The last attempt fetches from another origin, which is the one thing more attempts on the
  # configured pool cannot do. That pool is a mirrorlist, which `apt-get update` reads as
  # `Get:1 file:/etc/apt/apt-mirrors.txt Mirrorlist`, so the switch is an edit of that file. The
  # copy beside it is what was there; nothing puts it back, and nothing has to, the runner being
  # ephemeral and going with the job.
  mirrorlist=/etc/apt/apt-mirrors.txt
  sudo cp "$mirrorlist" "$mirrorlist.orig"
  sudo sed -i 's,^\(https\?://\)[^/]*,\1archive.ubuntu.com,' "$mirrorlist"
  cat "$mirrorlist"
  # The new origin's indexes are not the old one's, so they are read before the attempt that
  # uses them. Where that read fails the attempt still runs: the lists on disk may still name
  # files this origin serves, and an attempt is worth more here than a tidy exit.
  attempts 1 "apt-get update for the fallback origin" sudo timeout 90 apt-get update ||
    echo "::warning::the fallback origin's lists were not read; the attempt below uses what is on disk"
  # shellcheck disable=SC2086 # the pinned list splits into words, one package each
  attempts 1 "the package download from the fallback origin" sudo timeout 300 apt-get install -y --no-install-recommends -d $packages || {
    echo "::error::the package download failed three times, the last of them on another origin;" \
      "the step ends here rather than leaving the job's budget to cancel the browser step after it"
    exit 1
  }
fi

# Unbounded and unretried: the archive is on disk now, so this takes no network.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
sudo apt-get install -y --no-install-recommends $packages

# The browsers alone, and only the ones this call named: they are this script's arguments, so
# they are passed on as arguments and nothing re-splits them. The apt list above does not narrow
# with them, for the reason this file's head gives.
bunx playwright install "$@"

# Has the pinned list fallen behind Playwright? The check is the repository's
# .github/scripts/playwright-deps-current.sh, beside this file, and it runs here against the
# list this script installed: $packages, unquoted so it splits into words, as apt took it above.
# It holds every package Playwright asks for against that list and the four media packages this
# script drops. It runs after the install, so Playwright's list, its missing set and the apt
# closure they are held against are read from the same state, and it is run from the caller's
# working directory, so the bunx inside it resolves this suite's Playwright.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
"$script_dir/playwright-deps-current.sh" $packages
