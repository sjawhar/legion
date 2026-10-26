# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The host-side rig helpers a stage proof shares: bounded waits, ports, the processes the run starts,
# and their teardown. Everything they touch is the run's own — its work directory, its processes,
# and the ports the kernel chose for it.
#
# Sourced, never run. The caller sets
#   root           the checkout
#   work           the run's scratch directory; every process whose command line or working
#                  directory names it belongs to the run
#   evidence       the evidence directory; each service started by start_process logs to
#                  $evidence/logs/<name>.log
#   timeout_hook   empty, or a function a timed-out until_true runs before it fails
# and defines note and fail (which exits). A production guard that finds a violation writes it to
# $evidence/pane-endpoint-violation.txt, and the next bounded wait aborts naming it.
#
# Under the stage scripts' pipefail, an early-exiting consumer — `head -N`, `grep -q`, a
# `while … break` — never sits downstream of a live producer (kubectl, gh, jj): the producer's
# SIGPIPE (141) becomes the pipeline's status the moment the consumer stops reading, and the
# assertion dies on a correct system. Ask the producer for the bounded output (`--limit 1`, `--jq`)
# or capture its whole output first and judge the variable; a `| head -1` is safe only inside a
# `fail` message, guarded by `|| true`, or on a local-file grep whose few lines fit the pipe buffer.
#
# A run has several trees live at once, in one namespace and in clones that fetch every branch, so
# every read of tree state scopes to the tree under test: a jj revset is intersected with that
# tree's own chain (`& ::<its pull request's head>`, `@|@-` of its workspace), a kubectl selector
# carries its issue, tree or role label, and a daemon-log read is keyed by its sandbox and role. A
# query over the whole repository or namespace answers for whichever tree acted last, and "the
# newest" is a sibling's as soon as two trees act within the same minute. A deliberately run-wide
# read — every pod's shape, the admission cap, a negative over the whole log — says so in its comment.

# real_bin_path DEST NAME... symlinks DEST/NAME to the real NAME binary mise resolves for the
# caller's own environment, for every NAME given, and prints DEST. The daemon and every pane it
# launches run with HOME relocated to the run's own isolated home (make_omp_home,
# lib/omp-home.sh), which hides mise's global config from the dotfiles jj shim: it searches PATH
# for mise's own shim first and, under a HOME mise has no pin for, gets handed the call straight
# back — jj is unusable inside a pane until this is worked around. A caller prepends this
# directory to the daemon's own PATH before launching it, so the daemon and every pane it spawns
# use it: `PATH="$(real_bin_path "$work/realbin" gh jj tmux):$PATH"`. PATH reaches every pane
# (paneEnvAllowList, packages/daemon/internal/runtime/tmux/environment.go). gh still needs this
# directory too: resolveTools runs in the daemon's own process, under the same relocated HOME, and
# the dotfiles gh shim hits mise's own shim-recursion guard there the same way jj's shim loops —
# without this, the daemon itself has no real gh to resolve at all. A pane's own gh is the daemon's
# wrapper (legion gh, LEGION_GH_PATH), which resolves "the real gh" by walking its own PATH the
# same way (resolveTools, packages/daemon/internal/daemon/tools.go) — so this directory's gh is
# what LEGION_GH_PATH ends up naming, one step removed from a pane's own PATH lookup, not instead
# of it. jj has no such wrapper and keeps resolving through this directory directly. tmux needs it
# for the daemon itself: the tmux runtime looks tmux up on the pane PATH at boot and runs it with
# the pane environment's allow-list alone, which moves the XDG directories under the state
# directory. Run from a directory under the operator's real HOME, a dotfiles or mise shim's tmux
# then finds that HOME's mise config as a parent directory's config with no trust record and exits
# with status 1. The daemon's boot reads that as a failed scrub of its private server's environment,
# so boot refuses.
real_bin_path() {
  local dest=$1 name target
  shift
  mkdir -p "$dest"
  for name in "$@"; do
    target=$(mise which "$name" 2>/dev/null) || fail "real_bin_path: mise which $name failed; is $name pinned in mise.toml?"
    ln -sfn "$target" "$dest/$name"
  done
  printf '%s\n' "$dest"
}

# until_true SECONDS DESCRIPTION COMMAND... — all synchronization has a bounded named wait. It
# polls COMMAND every half second until SECONDS of wall time have passed, each poll's own run
# included, and says what it waits for on entry and every 60 s while it does (polls and seconds),
# so a healthy wait and a stall read differently to anyone watching a run's output. A poll that
# never returns would hold the wait past its bound, so a caller's remote calls carry their own
# timeouts.
until_true() {
  local limit=$1 what=$2 polls=0 started=$SECONDS beat=$SECONDS
  shift 2
  note "waiting up to ${limit}s for $what"
  while ((SECONDS - started < limit)); do
    [ ! -s "$evidence/pane-endpoint-violation.txt" ] || fail "ABORT: $(cat "$evidence/pane-endpoint-violation.txt")"
    polls=$((polls + 1))
    if "$@" >/dev/null 2>&1; then return 0; fi
    if ((SECONDS - beat >= 60)); then
      beat=$SECONDS
      note "still waiting for $what: poll $polls, $((SECONDS - started))s of ${limit}s"
    fi
    sleep 0.5
  done
  if [ -n "$timeout_hook" ]; then "$timeout_hook" || true; fi
  fail "timed out after ${limit}s ($polls polls) waiting for $what"
}

# A service the run starts serves on a port the kernel chose, held from that choice until the
# service serves on it, so no other process can take it in between (lib/held-port.py). A port
# picked free and passed by number could be taken before the service binds it. Each helper below
# starts its process with start_process (the service's log, <NAME>_pid) and keeps its port file
# and keeper socket in $work.

# await_port_file NAME FILE VAR waits, bounded, for FILE, the port file held-port.py writes before
# it runs the service NAME, while NAME's process (<NAME>_pid) lives, and assigns VAR its port. The
# locals' names are ones no caller passes: printf -v would otherwise assign the local.
await_port_file() {
  local _name=$1 _file=$2 _var=$3 _pid_var _port _i
  _pid_var=${_name}_pid
  for ((_i = 0; _i < 60; _i++)); do
    if [ -s "$_file" ]; then
      read -r _port <"$_file"
      printf -v "$_var" '%s' "$_port"
      return 0
    fi
    kill -0 "${!_pid_var}" 2>/dev/null || fail "$_name exited before it held a port; see $evidence/logs/$_name.log"
    sleep 0.5
  done
  fail "timed out after 30s waiting for $_name to hold a port"
}

# start_held NAME PORT_VAR FD_ENV [--env K=V]... -- COMMAND... starts COMMAND as the service NAME on
# a listening socket held-port.py opens on 127.0.0.1:0 and hands over as the descriptor FD_ENV
# names (DISPATCH_LISTEN_FD, ENVOY_LISTEN_FD, LEGION_LISTEN_FD), and assigns PORT_VAR its port, read
# back before COMMAND runs. {port} in an --env value or in COMMAND is that port. The helper executes
# COMMAND in place, so <NAME>_pid is the service's own pid. A COMMAND that clears its environment
# (`env -i`) names FD_ENV=3 itself: held-port.py always hands the socket over as descriptor 3.
start_held() {
  local _name=$1 _port_var=$2 _fd_env=$3 _options=()
  shift 3
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    _options+=("$1")
    shift
  done
  [ $# -gt 1 ] || fail "start_held $_name: no command after --"
  shift
  rm -f "$work/$_name.port"
  start_process "$_name" python3 "$root/scripts/e2e/lib/held-port.py" exec --port-file "$work/$_name.port" \
    --fd-env "$_fd_env" "${_options[@]}" -- "$@"
  await_port_file "$_name" "$work/$_name.port" "$_port_var"
}

# keep_port NAME PORT_VAR starts the keeper NAME, a held-port.py keep that holds one listening
# socket on 127.0.0.1 for the run, and assigns PORT_VAR its port once the keeper hands out copies of
# it. The caller's cleanup stops it (<NAME>_pid). Every start_kept on it serves that port, so a
# configuration can name the port before its service first starts, and a restart cannot lose the
# port to another process. Between two servers the port still listens and nothing answers: the
# kernel queues a connection until the next server accepts it, so every poll of a kept port carries
# a timeout (curl --max-time) and takes the timeout for down.
keep_port() {
  local _name=$1 _port_var=$2
  rm -f "$work/$_name.port" "$work/$_name.sock"
  start_process "$_name" python3 "$root/scripts/e2e/lib/held-port.py" keep --port-file "$work/$_name.port" \
    --socket "$work/$_name.sock"
  await_port_file "$_name" "$work/$_name.port" "$_port_var"
}

# start_kept NAME KEEPER FD_ENV [--env K=V]... -- COMMAND... starts COMMAND as the service NAME, as
# start_held does, on a copy of the socket the keeper KEEPER (keep_port) holds.
start_kept() {
  local _name=$1 _keeper=$2 _fd_env=$3 _options=()
  shift 3
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    _options+=("$1")
    shift
  done
  [ $# -gt 1 ] || fail "start_kept $_name: no command after --"
  shift
  start_process "$_name" python3 "$root/scripts/e2e/lib/held-port.py" exec --from "$work/$_keeper.sock" \
    --fd-env "$_fd_env" "${_options[@]}" -- "$@"
}

# report_port LOG PATTERN prints the port a process that bound port 0 itself reported: the digits
# right after the first match of the extended regular expression PATTERN in LOG (nats-server -p -1:
# 'Listening for client connections on [^ ]*:'). It fails while LOG holds no such line, so it is
# also the poll that waits for one.
report_port() {
  local _match
  _match=$(grep -m1 -oE -- "$2[0-9]+" "$1" 2>/dev/null) || return 1
  _match=${_match%%$'\n'*}
  printf '%s\n' "${_match##*[!0-9]}"
}

# listening_holders PORT prints, one per line in order, the pid of every process holding the one
# socket that listens on PORT: its inode from the LISTEN row (state 0A) of /proc/net/tcp and
# /proc/net/tcp6, and each process with a descriptor naming socket:[<inode>]. It fails when no
# socket listens on PORT, or more than one does.
listening_holders() {
  local inodes
  inodes=$(awk -v port=":$(printf '%04X' "$1")" \
    '$4 == "0A" && substr($2, length($2) - 4) == port { print $10 }' /proc/net/tcp /proc/net/tcp6 2>/dev/null)
  case "$inodes" in
    '' | *[!0-9]*)
      echo "listening_holders: $(wc -w <<<"$inodes") sockets listen on port $1, not one" >&2
      return 1
      ;;
  esac
  # A process that exits during the walk leaves find an unreadable directory: its status is no answer.
  { find /proc/[0-9]*/fd -maxdepth 1 -lname "socket:\\[$inodes\\]" 2>/dev/null || true; } | cut -d/ -f3 | sort -un
}

# held_only_by PORT PID... succeeds when the processes holding the socket listening on PORT
# (listening_holders) are exactly the PIDs given, and otherwise prints each holder with its command
# and fails: a descriptor that leaked into another process (a tmux server, a pane, a CLI call) keeps
# the port listening after the service stops.
held_only_by() {
  local port=$1 holders want pid
  shift
  holders=$(listening_holders "$port") || return 1
  want=$(printf '%s\n' "$@" | sort -un)
  [ "$holders" != "$want" ] || return 0
  echo "the socket listening on port $port is held by:" >&2
  for pid in $holders; do echo "  $pid $(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" >&2; done
  echo "and should be held by $(tr '\n' ' ' <<<"$want")alone" >&2
  return 1
}

# await_start NAME PID SECONDS DESCRIPTION COMMAND... waits, bounded, for COMMAND to succeed while
# the service NAME started as PID lives, and fails the check naming the service's log the moment it
# exits.
await_start() {
  local name=$1 pid=$2 limit=$3 what=$4 i
  shift 4
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    kill -0 "$pid" 2>/dev/null || fail "$what: the process exited; see $evidence/logs/$name.log"
    sleep 0.5
  done
  fail "timed out after ${limit}s waiting for $what"
}

# start_process NAME COMMAND... starts COMMAND in the background, logging to the service's log, and
# sets <NAME>_pid to its pid.
start_process() {
  local name=$1
  shift
  "$@" >>"$evidence/logs/$name.log" 2>&1 &
  printf -v "${name}_pid" '%s' "$!"
}

# signalable PID is whether PID can name one process the run started: a number without a leading
# zero other than 1, this shell, and this shell's process group leader. 0, however many digits spell
# it, and a negative number name a whole process group, so `kill -STOP 0` would stop the caller with
# everything else in its group and a later `kill -KILL 0` end them all; no pid is written with a
# leading zero. A refusal is printed, since a caller that hands one over has a bug.
signalable() {
  local pid=$1
  case "$pid" in
    '' | *[!0-9]* | 0* | 1) ;;
    *)
      [ "$pid" != "$$" ] && [ "$pid" != "$BASHPID" ] &&
        [ "$pid" != "$(ps -o pgid= -p "$$" 2>/dev/null | tr -d ' ')" ] && return 0
      ;;
  esac
  echo "refused to signal pid '$pid': it is not one process this run started" >&2
  return 1
}

# stop_pid is intentionally best effort: a failed cleanup must never obscure the check that failed.
stop_pid() {
  local pid=${1:-} i
  [ -n "$pid" ] || return 0
  signalable "$pid" || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  return 0
}

# stop_tree PID stops PID and every process under it. A watcher is a loop whose kubectl, jq or sleep
# outlives the loop when only the loop is stopped; the loop is frozen first, so it starts nothing new
# while its children go. PID is signalled only while its parent is PARENT (default this shell), so a
# pid the watcher left and another process reused is never hit. An empty PID is a watcher that
# never started: nothing to stop.
# pgrep exits 1 for a process with no children, which is no error: under a driver's `set -E` it would
# run the ERR trap inside the substitution, and a trap that prints there would hand its words over
# as child pids (the `0` of an `exit 0` among them).
stop_tree() {
  local pid=${1:-} parent=${2:-$$} child
  [ -n "$pid" ] || return 0
  signalable "$pid" || return 0
  [ "$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')" = "$parent" ] || return 0
  kill -STOP "$pid" 2>/dev/null || return 0
  for child in $(pgrep -P "$pid" || true); do stop_tree "$child" "$pid"; done
  kill -KILL "$pid" 2>/dev/null || true
}

run_processes() {
  local p cwd
  for p in /proc/[0-9]*; do
    cwd=$(readlink "$p/cwd" 2>/dev/null) || continue
    case "$cwd/" in
      "$work"/*) printf '%s\n' "${p#/proc/}"; continue ;;
    esac
    if grep -qsF "$work" "$p/cmdline" 2>/dev/null; then printf '%s\n' "${p#/proc/}"; fi
  done
}
