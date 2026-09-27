#!/usr/bin/env bash
# A stage driver's own home for Oh My Pi. Sourced, never run.
#
# make_omp_home DIR — DIR becomes the HOME every Oh My Pi process of the run runs under (the daemon,
# so every pane, the controller, and each `omp` a lib runs), so the run's profile root is
# DIR/.omp/profiles/<name>, inside its work directory, and the operator's own ~/.omp/profiles is
# never written or deleted. Oh My Pi places a profile from the home directory alone:
# os.homedir()/.omp/profiles/<name> (getProfileConfigRoot, @oh-my-pi/pi-utils dirs.ts).
# XDG_DATA_HOME moves only a named profile's data, and only once $XDG_DATA_HOME/omp/profiles/<name>
# exists, and PI_CONFIG_DIR reaches no pane (neither daemon's pane allow-list carries it).
#
# DIR/.omp/natives links to the operator's native module cache (~350 MB per Oh My Pi version,
# resolved as getNativesDir in @oh-my-pi/pi-natives' native/loader-state.js resolves it), which a
# fresh home would download again; removing DIR removes the link, never the cache. MISE_DATA_DIR and
# XDG_CONFIG_HOME are exported first as the operator's own, where each already resolves, so what the
# daemon itself runs under DIR finds what it did under the operator's HOME: `mise` the same pinned
# Oh My Pi, `secrets` its secretsd config (the daemon resolves provider_keys with it), and gh and jj
# their config. Panes are unaffected: the daemon moves their four XDG base directories under
# <state_dir>/home (LEGION-206 P1).
make_omp_home() {
  local natives=$HOME/.omp/natives
  if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
  export MISE_DATA_DIR="${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
  export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
  mkdir -p "$natives" "$1/.omp"
  # -T reads DIR/.omp/natives as the link's own name, never as a directory to put the link in: a
  # second call replaces the link (plain -s would write natives/natives into the operator's cache,
  # through the first link), and a real directory there fails the call instead of taking a link.
  ln -sfT "$natives" "$1/.omp/natives"
}
