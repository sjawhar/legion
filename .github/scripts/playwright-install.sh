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
# --with-deps`, which runs its apt calls with no bound, no retry and no second origin. The
# pinned list it reads from .github/scripts/playwright-packages.txt is Playwright's
# ubuntu24.04-x64 list exactly: the union of the `tools`, `chromium`, `firefox` and `webkit`
# entries of that table in playwright-core's lib/coreBundle.js. Measured in job 128209099649, of
# cybermonklabs/legion#2888 at 5ff42456: Playwright's 79 names and gstreamer1.0-tools resolve,
# with their dependencies, to 182 packages and 114 MB of archives (`0 upgraded, 182 newly
# installed, 0 to remove and 27 not upgraded.` and `Need to get 114 MB of archives.`), and the
# `apt-get update` before them fetched 13.0 MB of indexes (`Fetched 13.0 MB in 1s (10.1 MB/s)`).
# Nothing is left out of the list, so there is no reduced set beside it and no split to measure.
#
# This head used to argue the other way: that the list was Playwright's less four media packages
# - gstreamer1.0-libav, gstreamer1.0-plugins-bad, gstreamer1.0-plugins-good and libavcodec60 -
# for 75 names, 60 packages and 51.4 MB, because no row of the suite plays media. That argument
# was overturned on purpose by dispatch://INFRA-1506 (cybermonklabs/legion#2888), which is a
# correctness fix: WebKit plays video and audio through GStreamer, so without those four it
# loads a page and plays nothing. It was not lost in a merge, and it is not to be restored.
#
# What the four carry: the first three hold the plugins that demux Matroska and decode VP8 and
# h.264, and libavcodec60 is the codec library gstreamer1.0-libav's plugin links for h.264.
# Playwright's own source says of gstreamer1.0-libav that its one library libgstlibav.so is the
# library required for playing h.264. The decoder guard after the install asks the GStreamer
# registry for one element out of each of those three plugin packages and out of the
# already-pinned gstreamer1.0-plugins-base; avdec_h264 loads only where libavcodec60 is
# installed, so it answers for that library too. It runs before the browsers are downloaded, and
# it is the one check that catches a package that installs without its element reaching the
# GStreamer registry. That the browsers launch at all is what the suite's six WebKit and Firefox
# projects prove - webkit, webkit-iphone, webkit-alone, firefox, firefox-download-navigation and
# firefox-alone (e2e/playwright.config.ts) - which the shard job's browser step and the alone job
# run.
#
# `step_tools` below is the one package this script installs that Playwright's list does not
# name: gstreamer1.0-tools, for the `gst-inspect-1.0` the decoder guard runs. It is a variable of
# its own, and is not in .github/scripts/playwright-packages.txt, because that file is what the
# check at the end holds against Playwright's own list, name for name.
#
# The list lives in .github/scripts/playwright-packages.txt, one package per line, and this
# script is the one reader of it, so the two machines cannot drift apart. How to re-derive it on
# a Playwright upgrade is in that file's own head comment: that union again, with nothing taken
# out of it. Deriving it at run time by subtracting one apt closure from another does not work
# and must not be written: the closure of the four that carry playback and the closure of the
# rest share 35 packages Playwright's own list names or needs, and the subtraction leaves WebKit
# unable to launch. This paragraph is a standing refusal, not a record of a design that was taken
# out: nobody is to re-derive a reduced list.
#
# The 22-minute budget each calling step carries is this script's own retry arithmetic, not its
# measured maximum, which the bounds make unreachable: two bounded attempts of `apt-get update`
# at 90 s, two of the download at 300 s, then, where both of those time out, one 90 s read of the
# fallback origin's indexes and a third 300 s download attempt, then the unpack, the keeping of
# the archives for the cache, the decoder guard, the browser download and the one apt simulation
# of the list check, none of which touch the Ubuntu mirror. So the arithmetic is
# 2 x 90 + 2 x 300 + 90 + 300 = 1170 s of bounded apt, 13.6 s of unpack, 1.1 s of keeping, 0.4 s
# of decoder guard, about 90 s for the browsers and 1.6 s for the list check:
# 1170 + 13.6 + 1.1 + 0.4 + 90 + 1.6 = 1276.7 s, rounded up to 22 min. The budget has to carry
# the fallback's own update, because the path that needs every one of those bounds is the path on
# which the script still succeeds. The seeding of restored .deb files is not a term of that sum:
# the run that needs every one of those bounds is the cold one, which seeds nothing, and on the
# run that does seed, the copy is the keeping's 1.1 s in the other direction.
#
# The five readings after the bounded apt are job 128209099649's, of cybermonklabs/legion#2888 at
# 5ff42456, at its 182 packages. The unpack is the 13.6 s from `Download complete and in download
# only mode` at 23:14:13.884 to the last line of the unbounded install at 23:14:27.475. The
# keeping is the 1.1 s to `apt archives: seeded 0 .deb files, keeping 182` at 23:14:28.541, which
# is 114 MB copied out of apt's directory for the cache the calling job restores. The decoder
# guard is the 0.4 s to `the decoders WebKit plays through are in the registry: ...` at
# 23:14:28.984. The browsers and the list check together are the 1.6 s to `The pinned list is
# current: ...` at 23:14:30.540 - but that job restored its browsers from the Actions cache
# (`Cache hit for: playwright-Linux-7f68fc...`, 482 MB, at 23:13:54), so that 1.6 s is no reading
# of the cold browser download and the budget has to carry the cold case. That is what the 90 s
# term is: an estimate for the cold browsers, not a reading, which the package set does not
# reach. The list check is measured inside that 1.6 s, so 1.6 s is the term the reading supports
# for it. The whole step took 30 s in that job, 23:14:00 to 23:14:30: one reading at this package
# set, not a distribution. Each is one reading, so each moves a little from run to run; the
# margin the sum leaves under 22 min is what carries that.
#
# Each bound is derived from its own phase: each job's two apt rates, as apt logged them, scaled
# onto this script's fetch of 13.0 MB of indexes and the list's 114 MB of packages. The
# population is the 205 green dispatch jobs of the tester's window (2026-11-05T08:51:42Z to
# 23:07:42Z), read in full for the 26 whose install step ran over 73 s.
#
# Two jobs bind the two bounds. Job 127238648049 binds the download: apt logged
# `Fetched 114 MB in 10min 13s (186 kB/s)` for its packages, so the list's 114 MB need 613 s
# there, which the two 300 s attempts on the primary do not reach. Job 127357441704 binds the
# update: apt logged `Fetched 10.5 MB in 45s (233 kB/s)` for its `apt-get update`, so the
# script's 13.0 MB of indexes need 55.8 s there, and 90 s leaves 61 per cent over that need.
# Neither the index figure nor its 90 s bound moves with the package set.
#
# So at 186 kB/s, the slowest mirror a green job of that window met, the list's 114 MB need more
# than the 600 s of the two attempts on the primary, and the origin switch fires. That is the
# trade Playwright's whole list costs: a download phase that reaches the fallback origin on that
# mirror. On a run whose apt archive cache hit, nothing of those 114 MB is fetched at all, which
# is what that cache is for.
#
# One blended rate per job is the wrong model, as job 127238648049's own log shows:
# `Fetched 10.5 MB in 1s (9797 kB/s)` for its update and the 186 kB/s above for its packages. One
# rate over both phases mixes a fast update into a slow download and understates the download.
# The model assumes a retry meets the same mirror rate, and azure.archive.ubuntu.com is a pool,
# so a retry may meet a faster mirror or a slower one. Both bounds come from the slowest mirror a
# green job of that window met, and are read again when a green job meets a slower one. Under
# 13.0 MB / 90 s = 144 kB/s the indexes do not arrive inside one bound, and under
# 114 MB / 300 s = 380 kB/s the packages do not: job 127274913129 met 62.5 kB/s for its packages,
# and its job budget cancelled it. The list's 114 MB need 1824 s at that rate, which neither
# 600 s nor 900 s of download reaches, so the step fails by its own name there.
#
# What the attempts complete is the threshold this script carries, and it is three bands rather
# than one number, because the third attempt is on another origin and the partial may not be
# reused across that switch. With B the 114 MB the list fetches:
#   above B / 600 s = 190 kB/s the two attempts on the primary complete the download;
#   between B / 900 s = 127 kB/s and 190 kB/s the origin switch fires on every run and
#     completion rests on the fallback resuming the partial; where it does not, that one
#     attempt carries all 114 MB and needs B / 300 s = 380 kB/s, which nothing in this band
#     meets, so that class fails by its calling step's name;
#   below 127 kB/s it fails by its calling step's name whatever the fallback does - in about
#     900 s where both `apt-get update` phases were fast, and in at most 1170 s where each was
#     retried to its bound.
#
# The attempts accumulate, which is measured and not assumed: `apt-get ... -d` resumes. An
# attempt the bound kills leaves its bytes in apt's partial directory, and the attempt after it
# asks only for the rest. Job 128209099649 measured that at 5ff42456: after the download phase
# had fetched all 114 MB, the install call after it asked for `Need to get 0 B/114 MB of
# archives.`, so what the download put on disk the install does not fetch again. Measured
# separately, the partial is resumed across the origin switch too, since the two mirrors serve
# the same file and the server answers a byte-range request. Where the fallback's snapshot holds
# a different version the filename differs, the partial is not reused and that attempt carries
# the full 114 MB, which that job fetched in 8 s at 14.7 MB/s.
#
# Why the third attempt changes origin rather than the bound being raised: a longer bound buys a
# longer hang and hides the mirror. One step's log holds both of the rates the one host it met
# served, 9977 kB/s for the indexes and 31 kB/s for the pool files, so the slowness was in the
# pool path rather than in the host, and another origin is the lever that answers that. A single
# bad pool member is the case no attempt count on one origin reaches.
#
# The slowest reading this repository holds is that 31 kB/s: the list's 114 MB need 3677 s at it,
# which no attempt count worth having completes, so the step fails by its own name there, which
# is the intended outcome.
#
# The worst case this script carries: if both `apt-get update` attempts fail, it stops after
# about 180 s, so the longest the bounded apt phases can run is one update attempt stopped at
# 90 s, a second ending just inside 90 s, two download attempts stopped at 300 s, the fallback
# origin's update stopped at 90 s and a third download attempt stopped at 300 s. The step fails
# by its own name in at most 1170 s, against the job budget that would otherwise cancel the leg.
#
# The .deb files of that list are cached between runs, and the two halves of that cache sit on
# either side of this file. The calling job's `actions/cache/restore` of ~/apt-archives is a
# workflow step, because a script cannot call an action; the seeding of what it found and the
# keeping of what apt downloaded are here, below. apt takes a file in its archive directory whose
# name and size match the index without fetching it and without hashing it (the pkgAcqArchive
# constructor in apt-pkg/acquire-item.cc, lines 3499-3507 of apt 2.7.14, compares the size
# alone), so a restored file is trusted as the push to main that wrote the entry left it. The key
# names the Playwright version bun.lock pins, because that version decides Playwright's list, and
# the runner image, because the image decides which versions apt resolves, so an image roll
# misses the entry rather than restoring files the new image's indexes no longer name. It names
# neither `step_tools` nor the versions apt resolves from updates published after the image was
# built, and an entry is written once and never replaced, so a package that changes under one key
# is fetched on every run until the key moves. Only a push to main writes an entry, from the
# shard job's first leg alone. A pull-request or merge-queue run restores main's entry where one
# exists under its key and otherwise fetches the whole set, and every bound above is that cold
# case.
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

# gstreamer1.0-tools holds the gst-inspect-1.0 the decoder guard below runs, and is no name of
# Playwright's list. It is out of $packages, and out of playwright-packages.txt, because
# $packages is the list the check at the end of this script holds against Playwright's own.
step_tools="gstreamer1.0-tools"
# What apt is given in every phase: the pinned list and this script's own tool.
apt_list="$packages $step_tools"

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

# What the calling job's cache restore found, handed to apt before it reads any index. A file
# whose version the index no longer names has another file name, and one whose size differs is
# deleted (acquire-item.cc:3508-3511), so apt fetches both again. nullglob makes an empty or
# absent directory an empty list instead of a failed copy.
shopt -s nullglob
seeded=("$HOME"/apt-archives/*.deb)
shopt -u nullglob
[ "${#seeded[@]}" -eq 0 ] || sudo cp "${seeded[@]}" /var/cache/apt/archives/

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
# this step's own byte record. $apt_list is left unquoted on purpose, to split into words.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
if ! attempts 2 "the package download" sudo timeout 300 apt-get install -y --no-install-recommends -d $apt_list; then
  # The last attempt fetches from another origin, which is the one thing more attempts on the
  # configured pool cannot do. That pool is a mirrorlist, which `apt-get update` reads as
  # `Get:1 file:/etc/apt/apt-mirrors.txt Mirrorlist`, so the switch is an edit of that file. The
  # copy beside it is what was there; nothing puts it back, and nothing has to, the runner being
  # ephemeral and going with the job.
  mirrorlist=/etc/apt/apt-mirrors.txt
  # Where the image stops shipping it, the copy below ends on `cp: cannot stat` and the log names
  # no cause, so the file is tested here and the error is this script's own.
  [ -f "$mirrorlist" ] || {
    echo "::error::there is no $mirrorlist, so the fallback origin cannot be set;" \
      "the package download ends here"
    exit 1
  }
  sudo cp "$mirrorlist" "$mirrorlist.orig"
  sudo sed -i 's,^\(https\?://\)[^/]*,\1archive.ubuntu.com,' "$mirrorlist"
  cat "$mirrorlist"
  # The new origin's indexes are not the old one's, so they are read before the attempt that
  # uses them. Where that read fails the attempt still runs: the lists on disk may still name
  # files this origin serves, and an attempt is worth more here than a tidy exit.
  attempts 1 "apt-get update for the fallback origin" sudo timeout 90 apt-get update ||
    echo "::warning::the fallback origin's lists were not read; the attempt below uses what is on disk"
  # shellcheck disable=SC2086 # the pinned list splits into words, one package each
  attempts 1 "the package download from the fallback origin" sudo timeout 300 apt-get install -y --no-install-recommends -d $apt_list || {
    echo "::error::the package download failed three times, the last of them on another origin;" \
      "the step ends here rather than leaving the job's budget to cancel the browser step after it"
    exit 1
  }
fi

# Unbounded and unretried: the archive is on disk now, so this takes no network.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
sudo apt-get install -y --no-install-recommends $apt_list

# What apt has now, kept for the calling job's cache save. That save runs only on a push to main
# whose restore missed, since an entry is never replaced, so on a hit or a branch run this copy
# goes unused. apt's archive directory is root's and the cache action runs as the runner user, so
# the copies change hands.
shopt -s nullglob
kept=(/var/cache/apt/archives/*.deb)
shopt -u nullglob
mkdir -p "$HOME/apt-archives"
[ "${#kept[@]}" -eq 0 ] || {
  sudo cp "${kept[@]}" "$HOME/apt-archives/"
  sudo chown "$(id -u):$(id -g)" "$HOME"/apt-archives/*.deb
}
echo "apt archives: seeded ${#seeded[@]} .deb files, keeping ${#kept[@]}"

# Is each decoder WebKit plays through in the GStreamer registry? One call per element:
# `gst-inspect-1.0 --exists` reads one element argument and discards the rest, so a single call
# with five names would report on the first alone. Each element is written with the package that
# carries it, and this is what fails the calling step by its own name where one of those packages
# has gone missing from the pinned list.
probed=()
for probe in matroskademux:gstreamer1.0-plugins-good vp8dec:gstreamer1.0-plugins-good \
  h264parse:gstreamer1.0-plugins-bad avdec_h264:gstreamer1.0-libav \
  vorbisdec:gstreamer1.0-plugins-base; do
  element=${probe%%:*}
  gst-inspect-1.0 --exists "$element" || {
    echo "::error::GStreamer has no $element, which WebKit plays media through;" \
      "${probe#*:} carries it: .github/scripts/playwright-packages.txt must hold it, and if it" \
      "does, the plugin did not load: gst-inspect-1.0 -b lists the files GStreamer could not load"
    exit 1
  }
  probed+=("$element")
done
echo "the decoders WebKit plays through are in the registry: ${probed[*]}"

# The browsers alone, and only the ones this call named: they are this script's arguments, so
# they are passed on as arguments and nothing re-splits them. The apt list above does not narrow
# with them, for the reason this file's head gives.
bunx playwright install "$@"

# Has the pinned list fallen behind Playwright? The check is the repository's
# .github/scripts/playwright-deps-current.sh, beside this file, and it runs here against the
# list this script installed: $packages, unquoted so it splits into words, as apt took it above.
# $step_tools is not given to it: it is no name of Playwright's list, and the check holds the two
# lists against each other both ways. It runs after the install, so it expects nothing left
# missing, and it is run from the caller's working directory, so the bunx inside it resolves this
# suite's Playwright.
# shellcheck disable=SC2086 # the pinned list splits into words, one package each
"$script_dir/playwright-deps-current.sh" $packages
