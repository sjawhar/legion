# shellcheck shell=bash disable=SC2154 # session_db_container is the caller's
# A stage proof's reads of its run's session database: the scratch Postgres every pod of the run
# keeps its sessions in (runtime.kubernetes.session_store: postgres), in Oh My Pi's table
# omp_session_files and its parts omp_session_files_parts, keyed by the path the session's file
# would have had (internal/ompsessions). Every read goes through the database's own container, with
# no client on the devbox.
#
# Sourced, never run. The caller sets
#   session_db_container   the container running the session database, whose owner is legion and
#                          whose database is sessions
# and starts that container before it calls a function here.

# session_psql NAME VALUE runs the SQL on stdin in the session database as its owner, legion, with
# VALUE the psql variable NAME, which the SQL quotes as a literal (:'NAME'), so no value is ever
# spliced into it; the SQL comes on stdin because psql interpolates no variable into a -c command.
# The output is unaligned and tuples-only (-A -t): each value exactly as the server sent it, with no
# padding, wrapping, quoting, escaping, header or row count, so a transcript's quotes, backslashes
# and newlines arrive as their writer wrote them, and psql ends the last row with one newline of its
# own. The aligned default pads and wraps a value of many lines, and COPY's text and CSV formats
# escape or quote one, so each would corrupt the JSONL. ON_ERROR_STOP makes an SQL error psql's exit
# status.
session_psql() {
  docker exec -i -e PGCLIENTENCODING=UTF8 "$session_db_container" \
    psql -X -q -A -t -v ON_ERROR_STOP=1 -U legion -d sessions -v "$1=$2"
}
# session_text PATH prints the session the table holds at PATH whole, as Oh My Pi reads it and
# ompsessions.Read does: the row's content, then each of its parts in start_offset order, in one
# statement so both come from one snapshot. The statement answers one row for a session the table
# holds and none otherwise, so psql's closing newline is what tells a held session, however short,
# from none: it is taken off, and none fails naming PATH. The x after psql's output keeps the
# substitution from stripping the session's own trailing newlines.
session_text() {
  local out
  out=$(session_psql path "$1" <<<"SELECT f.content || coalesce((SELECT string_agg(p.content, '' ORDER BY p.start_offset)
    FROM omp_session_files_parts p WHERE p.path = f.path), '') FROM omp_session_files f WHERE f.path = :'path';" && printf x) ||
    { echo "the session database $session_db_container could not be read for the session $1" >&2; return 1; }
  out=${out%x}
  [ -n "$out" ] || { echo "the session database $session_db_container holds no session $1" >&2; return 1; }
  printf '%s' "${out%$'\n'}"
}
# session_byte_len PATH prints the byte_len of the table's row at PATH, the size in bytes of what
# its writers have saved there. A read that fails, or an answer that is no number (the table holds
# no such row), fails saying which.
session_byte_len() {
  local out
  out=$(session_psql path "$1" <<<"SELECT byte_len FROM omp_session_files WHERE path = :'path';") ||
    { echo "the session database $session_db_container could not be read for the session $1" >&2; return 1; }
  case $out in
    '' | *[!0-9]*) echo "the session table holds no byte_len for $1 ('$out')" >&2; return 1 ;;
  esac
  printf '%s\n' "$out"
}
