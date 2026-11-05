#!/usr/bin/env bash
# with-managed-settings.sh — runs a command with a managed-settings file at
# /etc/claude-code/managed-settings.json on a machine whose /etc this account cannot write.
#
# Claude Code reads a machine's managed settings from /etc/claude-code/managed-settings.json, and
# a guard built into it seats itself only when that file turns its feature on, so a proof of such
# a guard needs the file in place and nothing else will do. A Legion issue pod runs as an
# unprivileged account: /etc there is root's, and no run may write it even if it could, since
# every session in the pod would then read what one proof left behind. So this script makes a user
# namespace and a mount namespace of its own, where the caller is root and the mount table is the
# run's own, bind-mounts a directory it built over /etc, and runs the command there. Nothing
# outside the namespace changes, and both namespaces are gone when the command exits.
#
# The directory it binds is built here, so a caller needs no prepared tree: every entry of /etc
# this account can read is copied into it (a root-only file such as /etc/shadow is not, and the
# run says on stderr how many lines of refusal the copy made), and the file the caller names is
# written under it as claude-code/managed-settings.json.
#
# The command runs as the account that started it: a second, nested user namespace maps the
# namespace's root back to the caller's own uid and gid. Claude Code refuses to run as root with
# permissions bypassed, which is how a proof runs it, and a file the command writes in the
# caller's directories stays the caller's. --as-root leaves the command root inside the namespace.
#
#   bash scripts/e2e/with-managed-settings.sh --settings <file> [--etc <dir>] [--as-root]
#       -- <command> [argument...]
#
#   --settings <file>  required: the JSON this places at /etc/claude-code/managed-settings.json
#                      inside the namespace, such as {"channelsEnabled": true}
#   --etc <dir>        the directory built and bound over /etc; it must hold no entry yet
#                      (default: a fresh directory under TMPDIR, kept, and named on stderr)
#   --as-root          run the command as root inside the namespace, rather than as the caller
#
# Everything it says of its own goes to stderr, so the command's stdout is the command's alone.
#
# Exit status: the command's own; 2 refused (an input is missing or unreadable, unshare is absent,
# or the kernel refuses the namespaces).
set -euo pipefail

readonly me=with-managed-settings
refuse() {
  printf '%s: %s\n' "$me" "$1" >&2
  exit 2
}
note() { printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }

usage() {
  sed -n '/^#   bash scripts/,/^#   --as-root/p' "$0" | sed 's/^# \{0,3\}//'
}

settings=""
etc_dir=""
as_root=""
while (($#)); do
  case $1 in
  --settings) [ $# -ge 2 ] || refuse "--settings needs a file"; settings=$2; shift 2 ;;
  --etc) [ $# -ge 2 ] || refuse "--etc needs a directory"; etc_dir=$2; shift 2 ;;
  --as-root) as_root=1; shift ;;
  -h | --help) usage; exit 0 ;;
  --) shift; break ;;
  *) refuse "unknown argument '$1' (--help lists them, and the command goes after --)" ;;
  esac
done
[ -n "$settings" ] ||
  refuse '--settings is required: the JSON to place at /etc/claude-code/managed-settings.json'
[ -r "$settings" ] || refuse "the managed-settings file $settings is not readable"
(($#)) || refuse 'no command: give it after --, as --settings <file> -- claude --version'
command -v unshare >/dev/null 2>&1 ||
  refuse "unshare is required and is not on PATH: it makes the namespaces this run replaces /etc in (util-linux)"
unshare -rm true 2>/dev/null ||
  refuse "the kernel refuses a user and mount namespace here (unshare -rm failed): without one /etc cannot be replaced for the command, and this account cannot write /etc"

# The directory bound over /etc holds the copy and nothing else, so a directory a caller names must
# be empty: this run fills it, and never writes over what another one left.
if [ -z "$etc_dir" ]; then
  etc_dir=$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")
else
  mkdir -p "$etc_dir" || refuse "could not make the directory $etc_dir"
  [ -z "$(ls -A "$etc_dir")" ] ||
    refuse "$etc_dir already holds entries: name a new or empty directory, which this run fills from /etc"
fi
etc_dir=$(cd -- "$etc_dir" && pwd -P)
# cp copies what this account can read and refuses the rest, which are the root-only files
# (/etc/shadow and its kind) that no session reads; the run says how many refusals it made rather
# than failing on them, and fails only when the copy holds no /etc/passwd, which means none of it
# arrived.
refusals=$(cp -a /etc/. "$etc_dir/" 2>&1 >/dev/null || true)
left_out=$(printf '%s' "$refusals" | grep -c . || true)
[ -r "$etc_dir/passwd" ] || refuse "the copy of /etc in $etc_dir holds no passwd: nothing was copied"
mkdir -p "$etc_dir/claude-code"
cp -- "$settings" "$etc_dir/claude-code/managed-settings.json"
chmod 0644 "$etc_dir/claude-code/managed-settings.json"
note "/etc copied into $etc_dir (${left_out} entries this account cannot read were left out); $settings is its claude-code/managed-settings.json"

# unshare -rm: a user namespace where this account is root (so the mount is allowed) and a mount
# namespace of its own (so the bind is this command's alone). The inner shell is POSIX sh, and
# takes its paths as arguments rather than in its text, so no path is quoted into a script.
if [ -n "$as_root" ]; then
  note "running as root inside the namespace: $*"
  exec unshare -rm sh -c 'etc=$1; shift; mount --bind "$etc" /etc && exec "$@"' sh "$etc_dir" "$@"
fi
uid=$(id -u)
gid=$(id -g)
note "running as uid ${uid} gid ${gid} inside the namespace: $*"
exec unshare -rm sh -c \
  'etc=$1; uid=$2; gid=$3; shift 3; mount --bind "$etc" /etc &&
    exec unshare -U --map-user="$uid" --map-group="$gid" -- "$@"' \
  sh "$etc_dir" "$uid" "$gid" "$@"
