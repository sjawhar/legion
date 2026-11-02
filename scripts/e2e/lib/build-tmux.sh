#!/usr/bin/env bash
# The tmux a Legion pod's tmux-runtime proofs need, built from source under $TMPDIR, because
# `legion start` refuses to boot without tmux on PATH and a worker image carries no tmux, no root
# and no package manager. Run, never sourced; it prints one line on stdout, the bin directory
# holding the tmux it built, so a caller does
#
#   PATH="$(bash scripts/e2e/lib/build-tmux.sh):$PATH"
#
# and every other word it writes goes to stderr. A second run with the build already in place
# prints that directory and does nothing else, so a proof may call it at every start.
#
# The pod has gcc and curl and no make, so GNU make comes first, through its own bootstrap, and
# each later stage is built by the make this one installed:
#   make 4.4.1     ./configure --prefix=$P --disable-dependency-tracking && sh build.sh && ./make install
#   ncurses 6.5    ./configure --prefix=$P --without-shared --enable-widec --with-termlib
#   byacc 20240109 tmux's configure requires a yacc, and the pod has none
#   libevent 2.1.12-stable   ./configure --prefix=$P --disable-shared --disable-openssl
#   tmux 3.5a      ./configure --prefix=$P with LIBEVENT_* and LIBNCURSES_* naming $P, PKG_CONFIG=/bin/false
# ncurses and libevent are built with no shared libraries, so the tmux that comes out loads nothing
# of this prefix at run time: `ldd` on it names libc, libm and libresolv of the image it was built
# in and nothing else. Its terminfo is the database ncurses installs under the prefix, which is the
# one directory compiled into it ($P/bin/ncursesw6-config --terminfo-dirs prints it); the image's
# /usr/share/terminfo is not compiled in, and a caller that wants it names it in TERMINFO_DIRS.
#
# Everything — the tarballs, the unpacked sources, the build logs and the install — goes under one
# prefix, $TMUX_BUILD_PREFIX or $TMPDIR/tmux-build, never /tmp and never the home directory: a
# pod's own disk is small, and filling it evicts the pod with every role in it. $TMPDIR unset and
# no $TMUX_BUILD_PREFIX is refused rather than guessed, since a guess would write to the disk this
# is written to stay off.
#
# Each download is pinned by version to its project's own canonical host (ftp.gnu.org for make and
# ncurses, invisible-island.net for byacc, each project's GitHub release for libevent and tmux) and
# held to the sha256 written below. A tarball whose bytes hash to anything else stops the build
# where it is, named, rather than being compiled.
#
# Each stage writes its compiler output to $P/log/<stage>.log and says on stderr when it starts and
# when it finishes, so a caller watching a cold build of some minutes can see which stage it is in.
# A stage that fails prints its command, its exit status and the last lines of its log, which carry
# the compiler's own error.
#
# The stages are skipped one by one: a stage whose product is installed is not built again, so an
# interrupted build resumes where it stopped, and a prefix is re-usable until it is deleted.
set -Eeuo pipefail

make_version=4.4.1
make_url=https://ftp.gnu.org/gnu/make/make-$make_version.tar.gz
make_sha256=dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3

ncurses_version=6.5
ncurses_url=https://ftp.gnu.org/gnu/ncurses/ncurses-$ncurses_version.tar.gz
ncurses_sha256=136d91bc269a9a5785e5f9e980bc76ab57428f604ce3e5a5a90cebc767971cc6

byacc_version=20240109
byacc_url=https://invisible-island.net/archives/byacc/byacc-$byacc_version.tgz
byacc_sha256=f2897779017189f1a94757705ef6f6e15dc9208ef079eea7f28abec577e08446

libevent_version=2.1.12-stable
libevent_url=https://github.com/libevent/libevent/releases/download/release-$libevent_version/libevent-$libevent_version.tar.gz
libevent_sha256=92e6de1be9ec176428fd2367677e61ceffc2ee1cb119035037a27d346b0403bb

tmux_version=3.5a
tmux_url=https://github.com/tmux/tmux/releases/download/$tmux_version/tmux-$tmux_version.tar.gz
tmux_sha256=16216bd0877170dfcc64157085ba9013610b12b082548c7c9542cc0103198951

# The stage a failure is named for: `setup` until the first build stage begins.
stage=setup
stage_started=0
log_file=/dev/null

fail() {
  echo "build-tmux: $stage: $*" >&2
  exit 1
}

say() { echo "build-tmux: $*" >&2; }

# begin STAGE and finish close a stage, one line of stderr each: the cold build runs for minutes,
# and these two lines are how a caller knows which stage it is holding on.
begin() {
  stage=$1
  stage_started=$SECONDS
  log_file=$prefix/log/$stage.log
  : >"$log_file"
  say "$stage: started"
}

finish() { say "$stage: done in $((SECONDS - stage_started))s"; }

# skip FILE prints what it skipped and returns 0 when FILE, the stage's installed product, is
# already there.
skip() {
  [ -e "$1" ] || return 1
  say "$stage: already built ($1)"
}

# run WHAT COMMAND... runs one build step with its output in the stage's log, and on a failure
# prints the command, its status and the end of that log, where the compiler's own error is.
run() {
  local what=$1 status=0
  shift
  "$@" >>"$log_file" 2>&1 || status=$?
  [ "$status" -eq 0 ] && return 0
  {
    echo "build-tmux: $stage: $what failed with status $status: $*"
    echo "build-tmux: $stage: the last 40 lines of $log_file:"
    tail -n 40 -- "$log_file"
  } >&2
  exit 1
}

# fetch FILE URL SHA256 leaves FILE in the source directory with those bytes. A download already
# there is re-used when it hashes to SHA256 and re-downloaded when it does not; bytes that hash to
# anything else after the download stop the build.
fetch() {
  local file=$1 url=$2 want=$3 path=$src/$1 got
  if [ -f "$path" ]; then
    got=$(sha256sum -- "$path" | cut -d' ' -f1)
    if [ "$got" = "$want" ]; then
      say "$stage: $file is already downloaded (sha256 $got)"
      return 0
    fi
    say "$stage: the $file already here has sha256 $got, not the pinned $want; downloading it again"
    rm -f -- "$path"
  fi
  say "$stage: downloading $url"
  curl -fsSL --retry 3 --retry-delay 2 -o "$path.part" -- "$url" ||
    fail "curl could not download $url"
  got=$(sha256sum -- "$path.part" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    rm -f -- "$path.part"
    fail "$file downloaded from $url has sha256 $got, and this script pins $want: the build stops rather than compile bytes it cannot name"
  fi
  mv -- "$path.part" "$path"
  say "$stage: $file has the pinned sha256 $got"
}

# unpack FILE DIR unpacks FILE into a fresh DIR under the build directory, so a stage that failed
# half way through leaves nothing behind for the next run to build on.
unpack() {
  rm -rf -- "${build:?}/$2"
  run "unpacking $1" tar -xzf "$src/$1" -C "$build"
  [ -d "$build/$2" ] || fail "$1 holds no directory $2"
}

prefix=${TMUX_BUILD_PREFIX:-}
if [ -z "$prefix" ]; then
  [ -n "${TMPDIR:-}" ] ||
    fail "TMPDIR is unset and TMUX_BUILD_PREFIX names no prefix: set one of them. This build goes under \$TMPDIR, never /tmp and never the home directory, so it guesses neither"
  prefix=$TMPDIR/tmux-build
fi
bin=$prefix/bin

# The build already in place, and the tmux in it runs: that is the whole answer, printed.
if [ -x "$bin/tmux" ] && "$bin/tmux" -V >/dev/null 2>&1; then
  printf '%s\n' "$bin"
  exit 0
fi

for tool in gcc curl tar sha256sum; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH, and this build needs it"
done

src=$prefix/src
build=$prefix/build
mkdir -p "$bin" "$src" "$build" "$prefix/log" ||
  fail "cannot make the build's directories under $prefix"
jobs=$(nproc 2>/dev/null || echo 2)
say "building tmux $tmux_version under $prefix with $jobs jobs; the logs are in $prefix/log"

# ---- GNU make, bootstrapped by its own build.sh -------------------------------------------------
begin make
if ! skip "$bin/make"; then
  fetch "make-$make_version.tar.gz" "$make_url" "$make_sha256"
  unpack "make-$make_version.tar.gz" "make-$make_version"
  cd "$build/make-$make_version"
  run "configure" ./configure --prefix="$prefix" --disable-dependency-tracking
  run "build.sh" sh build.sh
  run "install" ./make install
  [ -x "$bin/make" ] || fail "make installed no $bin/make"
fi
finish
# Every stage after this one is built by the make just installed, and configures against the
# headers and libraries this prefix holds.
PATH=$bin:$PATH
export PATH

# ---- ncurses, static and wide, with its terminfo library split out -------------------------------
begin ncurses
if ! skip "$prefix/lib/libncursesw.a"; then
  fetch "ncurses-$ncurses_version.tar.gz" "$ncurses_url" "$ncurses_sha256"
  unpack "ncurses-$ncurses_version.tar.gz" "ncurses-$ncurses_version"
  cd "$build/ncurses-$ncurses_version"
  run "configure" ./configure --prefix="$prefix" --without-shared --enable-widec --with-termlib
  run "make" make -j"$jobs"
  run "install" make install
  [ -f "$prefix/lib/libncursesw.a" ] && [ -f "$prefix/lib/libtinfow.a" ] ||
    fail "ncurses installed no libncursesw.a and libtinfow.a in $prefix/lib"
fi
finish

# ---- byacc, the yacc tmux's configure requires ---------------------------------------------------
begin byacc
if ! skip "$bin/yacc"; then
  fetch "byacc-$byacc_version.tgz" "$byacc_url" "$byacc_sha256"
  unpack "byacc-$byacc_version.tgz" "byacc-$byacc_version"
  cd "$build/byacc-$byacc_version"
  run "configure" ./configure --prefix="$prefix"
  run "make" make -j"$jobs"
  run "install" make install
  [ -x "$bin/yacc" ] || fail "byacc installed no $bin/yacc, which tmux's configure looks for"
fi
finish

# ---- libevent, static, with no TLS of its own ----------------------------------------------------
begin libevent
if ! skip "$prefix/lib/libevent.a"; then
  fetch "libevent-$libevent_version.tar.gz" "$libevent_url" "$libevent_sha256"
  unpack "libevent-$libevent_version.tar.gz" "libevent-$libevent_version"
  cd "$build/libevent-$libevent_version"
  run "configure" ./configure --prefix="$prefix" --disable-shared --disable-openssl
  run "make" make -j"$jobs"
  run "install" make install
  [ -f "$prefix/lib/libevent.a" ] || fail "libevent installed no $prefix/lib/libevent.a"
fi
finish

# ---- tmux ----------------------------------------------------------------------------------------
# PKG_CONFIG=/bin/false keeps configure off the image's .pc files, which name libraries this prefix
# does not hold; the four flag variables are what it falls back to, and they name this prefix only.
begin tmux
if ! skip "$bin/tmux"; then
  fetch "tmux-$tmux_version.tar.gz" "$tmux_url" "$tmux_sha256"
  unpack "tmux-$tmux_version.tar.gz" "tmux-$tmux_version"
  cd "$build/tmux-$tmux_version"
  run "configure" ./configure --prefix="$prefix" \
    PKG_CONFIG=/bin/false \
    LIBEVENT_CFLAGS="-I$prefix/include" \
    LIBEVENT_LIBS="-L$prefix/lib -levent" \
    LIBNCURSES_CFLAGS="-I$prefix/include/ncursesw" \
    LIBNCURSES_LIBS="-L$prefix/lib -lncursesw -ltinfow"
  run "make" make -j"$jobs"
  run "install" make install
  [ -x "$bin/tmux" ] || fail "tmux installed no $bin/tmux"
fi
finish

version=$("$bin/tmux" -V 2>&1) || fail "$bin/tmux does not run: $version"
say "built $version in $bin"
printf '%s\n' "$bin"
