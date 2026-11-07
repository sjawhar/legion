# shellcheck shell=bash
# What packages/envoy/scripts/near-copy-replay.sh and packages/envoy/scripts/related-recall.sh
# both do to read one Dispatch project over the API. The two exports write different records,
# from different reads, and each keeps its own; what they were doing the same way, byte for byte,
# is here, so a fix such as a longer retry reaches both at once instead of one of them.
#
# Sourced, never run. A caller sets `set -euo pipefail`, sources this file, and calls
# dispatch_export_open once before anything else; after that it has `base`, the API root, and
# `workdir`, its own directory, which the open removes on exit.
#
# Only GET requests are made here, one at a time.

# dispatch_export_open checks what both exports need, makes a private work directory named after
# NAME, and writes the token into a header file curl reads, so it never sits on a command line.
# It sets `base` and `workdir`, and takes the EXIT trap that removes the directory before it
# writes the token into it: neither caller sets another EXIT trap.
#
# It exits 2 on an environment the caller cannot be run with and 1 on a missing command, which is
# what both exports did before.
dispatch_export_open() {
  : "${DISPATCH_URL:?DISPATCH_URL must name the Dispatch server to read}"
  : "${DISPATCH_TOKEN_FILE:?DISPATCH_TOKEN_FILE must name a file holding a Dispatch token}"
  [[ -r "$DISPATCH_TOKEN_FILE" ]] || {
    printf 'DISPATCH_TOKEN_FILE is not readable: %s\n' "$DISPATCH_TOKEN_FILE" >&2
    exit 2
  }
  local _open_needed
  for _open_needed in curl jq; do
    command -v "$_open_needed" >/dev/null || {
      printf '%s is required\n' "$_open_needed" >&2
      exit 1
    }
  done
  umask 077
  base="${DISPATCH_URL%/}/api/v1"
  workdir="$(mktemp -d "${TMPDIR:-/tmp}/$1.XXXXXX")"
  trap 'rm -rf "$workdir"' EXIT
  printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$DISPATCH_TOKEN_FILE")" >"$workdir/auth"
}

# get prints the JSON body of GET <path> under /api/v1, or fails naming the status. An export is
# thousands of reads over several minutes, so a Dispatch that restarts in the middle of one must
# not end it: curl retries a refused connection for about two minutes before the read fails.
#
# Every non-2xx is named on stderr. A 404 returns 4 and every other status returns 1, because a
# caller reading an issue to find out whether it exists expects a 404 and counts it itself, while
# a caller that expects none still stops on it: both returns are nonzero under `set -e` and
# `pipefail`.
get() {
  local _get_status
  _get_status="$(curl --silent --show-error --retry 12 --retry-delay 10 --retry-all-errors \
    --retry-connrefused --max-time 120 \
    -H "@$workdir/auth" -o "$workdir/body" -w '%{http_code}' "${base}$1")"
  case "$_get_status" in
    2??) cat "$workdir/body" ;;
    404)
      printf 'GET %s answered %s\n' "$1" "$_get_status" >&2
      return 4
      ;;
    *)
      printf 'GET %s answered %s\n' "$1" "$_get_status" >&2
      return 1
      ;;
  esac
}

# uri escapes one value for a query string or a path segment.
uri() { jq -rn --arg value "$1" '$value | @uri'; }

# times is the jq prelude both exports read every time with. Dispatch writes times with as many
# fraction digits as they need; padded to six, two times compare as text, which every comparison
# in either script relies on.
readonly times='def t: if . == null then . else sub("Z$"; "") | split(".") | .[0] + "." + ((.[1] // "") + "000000")[0:6] + "Z" end;'

# list_issue_keys writes every issue key of PROJECT, whatever its status, to FILE, one key per
# line, reading the listing one page at a time. A project with no issues ends the run: an export
# of nothing is a mistake in the arguments, not a result. The locals are named as they are
# because a caller holds `project` readonly: `local project` on a readonly name fails.
list_issue_keys() {
  local _keys_project="$1" _keys_file="$2" _keys_offset=0 _keys_page _keys_count _keys_total
  : >"$_keys_file"
  while :; do
    _keys_page="$(get "/issues?project=$(uri "$_keys_project")&limit=250&offset=${_keys_offset}")"
    jq -r '.issues[].key' <<<"$_keys_page" >>"$_keys_file"
    _keys_count="$(jq '.issues | length' <<<"$_keys_page")"
    _keys_total="$(jq '.total' <<<"$_keys_page")"
    _keys_offset=$((_keys_offset + _keys_count))
    if ((_keys_count == 0 || _keys_offset >= _keys_total)); then
      break
    fi
  done
  if [[ ! -s "$_keys_file" ]]; then
    printf 'project %s has no issues\n' "$_keys_project" >&2
    exit 1
  fi
}

# list_events writes every event of issue KEY to FILE, one JSON object per line, oldest first.
list_events() {
  local _events_after=0 _events_page
  : >"$2"
  while :; do
    _events_page="$(get "/issues/$(uri "$1")/events?after=${_events_after}&limit=200")"
    jq -c '.[]' <<<"$_events_page" >>"$2"
    (("$(jq length <<<"$_events_page")" == 200)) || break
    _events_after="$(jq '.[-1].seq' <<<"$_events_page")"
  done
}

# markdown writes one version of an issue's primary document to a file, byte for byte. The
# artifact id comes from "$workdir/issues/<key>.json", the per-issue summary each export writes
# there with a `spec_artifact` field.
markdown() {
  get "/artifacts/$(uri "$(jq -r .spec_artifact "$workdir/issues/$1.json")")/versions/$2" | jq -j .markdown >"$3"
}
