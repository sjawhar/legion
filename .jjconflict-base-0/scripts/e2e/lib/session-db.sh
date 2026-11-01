# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (listed below)
# A stage proof's session database: the scratch Postgres every pod of the run keeps its
# sessions in (runtime.kubernetes.session_store: postgres), in Oh My Pi's table omp_session_files
# and its parts omp_session_files_parts, keyed by the path the session's file would have had
# (internal/ompsessions). session_db_start starts it; every read goes through the database's own
# container, with no client on the devbox.
#
# Sourced, never run, after lib/rig.sh, whose until_true session_db_start waits with. The caller
# sets
#   session_db_container   the container running the session database, whose owner is legion and
#                          whose database is sessions
# and, for session_db_start (beside fail, which exits),
#   session_db_image       the Postgres image the container runs, pinned by digest
#   port_session_db        the devbox port the container publishes, which the operator named
#   host                   the address every pod dials the database at
#   work                   the run's scratch directory, where the owner-only env file and URL go
# and starts that container with session_db_start before it calls a reader here.

# session_db_start BIND DOCKER_RUN_ARG… starts the run's session database, a throwaway Postgres
# with its own container and its own credential, never the daemon's own database: its URL reaches
# every pod through the providers Secret, and the daemon's state is no pod's to reach. The
# container publishes port_session_db on the devbox's address BIND (the private address the pods
# dial, or the loopback a bridge forwards from), keeps its data directory on tmpfs, and runs with
# DOCKER_RUN_ARG…, the caller's CPU and memory limits. Its password is the run's own and reaches
# docker through the owner-only env file $work/session-db.env, never an argument; the pod-facing
# URL, at host on port_session_db, is written only to the owner-only file $work/session-dsn, which
# the providers Secret reads, and is never printed. It returns once the database accepts
# connections, and fails when docker cannot start the container.
session_db_start() {
  local bind=$1 password
  shift
  password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  (
    umask 077
    printf 'POSTGRES_USER=legion\nPOSTGRES_DB=sessions\nPOSTGRES_PASSWORD=%s\n' "$password" >"$work/session-db.env"
    printf 'postgres://legion:%s@%s:%s/sessions?sslmode=disable\n' "$password" "$host" "$port_session_db" >"$work/session-dsn"
  )
  docker run -d --name "$session_db_container" "$@" --mount type=tmpfs,destination=/var/lib/postgresql \
    --env-file "$work/session-db.env" -p "$bind:$port_session_db:5432" "$session_db_image" >/dev/null ||
    fail "docker could not start the scratch session database $session_db_container on $bind:$port_session_db"
  until_true 60 "the session database to accept TCP connections" docker exec "$session_db_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d sessions
}

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
