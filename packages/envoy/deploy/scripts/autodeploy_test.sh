#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
repo="$root/repo"
compose="$repo/packages/envoy/deploy/compose"
backups="$root/backups"
bin="$root/bin"
tools="$root/tools"
log="$root/compose.log"
argv="$root/argv.log"
schema="$root/schema"
mkdir -p "$compose" "$backups" "$bin" "$tools"
# The database password, percent-encoded in DATABASE_URL as s%40cret-canary. Neither spelling may
# appear in the deployer's output or in any process's arguments.
password='s@cret-canary'
url_without_password='postgres://dispatch@db.test:5432/dispatch?sslmode=require'
fresh_env() {
  cat >"$compose/.env" <<EOF
ENVOY_IMAGE_TAG=old-image
DATABASE_URL=${1:-postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?sslmode=require}
DISPATCH_SERVER_URL=http://browser.test:8766
EOF
  printf '112\n' >"$schema"
  rm -f "$schema.up" "$schema.running"
  touch "$schema.running"
  : >"$log"
  : >"$argv"
}

cat >"$bin/jj" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *" git fetch"*) ;;
  *"ancestors(main@origin, 30)"*) printf 'new-image\n' ;;
  *"description.first_line()"*) printf 'test deployment\n' ;;
esac
EOF
# docker runs a container's command for real, as the container would: in an environment holding
# only the variables passed by name with -e, and with the fake psql and pg_dump below on its PATH.
# Every process it starts records its arguments, so the test reads what a host's
# /proc/<pid>/cmdline would show.
cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
held_migration_commits() {
  # A migration the candidate held open commits once Compose stops or replaces the candidate.
  if [[ -n "${AUTODEPLOY_TEST_MIGRATE_ON_STOP:-}" && -e "$AUTODEPLOY_TEST_SCHEMA.up" ]]; then
    printf '%s\n' "$AUTODEPLOY_TEST_MIGRATE_ON_STOP" >"$AUTODEPLOY_TEST_SCHEMA"
  fi
}
case "$1" in
  manifest)
    exit 0
    ;;
  run)
    printf 'docker %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    shift
    names=()
    while [[ "$1" == -* ]]; do
      case "$1" in
        --rm | --init) shift ;;
        --network) shift 2 ;;
        -e) names+=("$2"); shift 2 ;;
        *) printf 'unexpected docker run option: %s\n' "$1" >&2; exit 1 ;;
      esac
    done
    shift
    printf 'container %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    container_env=(PATH="$AUTODEPLOY_TEST_TOOLS:/usr/bin:/bin")
    for name in "${names[@]}" ${!AUTODEPLOY_TEST_@}; do
      if [[ -n "${!name+set}" ]]; then
        container_env+=("$name=${!name}")
      fi
    done
    exec env -i "${container_env[@]}" "$@"
    ;;
  compose)
    printf '%s\n' "$*" >>"$AUTODEPLOY_TEST_LOG"
    case "$*" in
      *" pull "*)
        if [[ "${AUTODEPLOY_TEST_PULL_FAIL:-}" == 1 ]]; then
          exit 1
        fi
        ;;
      *" up "*)
        if [[ "${ENVOY_IMAGE_TAG:-}" == new-image ]]; then
          touch "$AUTODEPLOY_TEST_SCHEMA.up"
          if [[ -n "${AUTODEPLOY_TEST_MIGRATE_TO:-}" ]]; then
            printf '%s\n' "$AUTODEPLOY_TEST_MIGRATE_TO" >"$AUTODEPLOY_TEST_SCHEMA"
          fi
        else
          held_migration_commits
        fi
        touch "$AUTODEPLOY_TEST_SCHEMA.running"
        ;;
      *" stop "*)
        if [[ "${AUTODEPLOY_TEST_STOP_FAIL:-}" == 1 ]]; then
          exit 1
        fi
        held_migration_commits
        if [[ "${AUTODEPLOY_TEST_STOP_LEAVES_RUNNING:-}" != 1 ]]; then
          rm -f "$AUTODEPLOY_TEST_SCHEMA.running"
        fi
        ;;
      *" ps "*)
        if [[ -e "$AUTODEPLOY_TEST_SCHEMA.running" ]]; then
          printf 'candidate-container\n'
        fi
        ;;
    esac
    ;;
  *)
    printf 'unexpected docker invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"/auth/start"*)
    printf 'https://github.com/login/oauth/authorize?redirect_uri=http%%3A%%2F%%2Fwrong.test%%3A8766%%2Fauth%%2Fcallback'
    ;;
esac
EOF
# The fake libpq, which splits a URL as libpq does rather than as the deployer does: the
# credentials run to the first @ before the first /, a raw ? included, and a query key or value is
# percent-decoded, so password= (however its key is spelled) sets the password. The password is
# PGPASSWORD, else the URL's. A malformed percent-encoding is quoted on stderr, as psql 16 does.
cat >"$tools/libpq.sh" <<'EOF'
percent_decode() {
  local valid='^([^%]|%[0-9A-Fa-f]{2})*$'
  [[ "$2" =~ $valid ]] || return 1
  printf -v "$1" '%b' "${2//%/\\x}"
}
connection_password() {
  local url="${!#}"
  local rest="${url#*://}"
  local before_path="${rest%%/*}"
  local after_credentials="$rest"
  local userinfo query param key value
  libpq_password="${PGPASSWORD-}"
  if [[ "$before_path" == *@* ]]; then
    userinfo="${before_path%%@*}"
    after_credentials="${rest#*@}"
    if [[ "$userinfo" == *:* ]] && ! percent_decode libpq_password "${userinfo#*:}"; then
      printf '%s: error: invalid percent-encoded token: "%s"\n' "$1" "${userinfo#*:}" >&2
      return 2
    fi
  fi
  if [[ "$after_credentials" == *\?* ]]; then
    query="${after_credentials#*\?}"
    while [[ -n "$query" ]]; do
      param="${query%%&*}"
      if [[ "$query" == *\&* ]]; then query="${query#*&}"; else query=""; fi
      percent_decode key "${param%%=*}" && percent_decode value "${param#*=}" || return 2
      if [[ "$key" == password ]]; then
        libpq_password="$value"
      fi
    done
  fi
  if [[ "$libpq_password" != "$AUTODEPLOY_TEST_PASSWORD" ]]; then
    printf '%s: error: password authentication failed for user "dispatch"\n' "$1" >&2
    return 2
  fi
}
EOF
cat >"$tools/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
# What a psql diagnostic quoting its own arguments would print. The deployer must not pass it on.
printf 'psql: PSQL-STDERR-CANARY %s\n' "$*" >&2
source "$AUTODEPLOY_TEST_TOOLS/libpq.sh"
connection_password psql "$@" || exit 2
if [[ "${AUTODEPLOY_TEST_SCHEMA_UNREADABLE:-}" == 1 ||
  ("${AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP:-}" == 1 && -e "$AUTODEPLOY_TEST_SCHEMA.up") ]]; then
  exit 2
fi
case "$*" in
  *'pg_advisory_lock(8150001)'*'from schema_migrations'*) printf '\n' ;;
esac
cat "$AUTODEPLOY_TEST_SCHEMA"
EOF
cat >"$tools/pg_dump" <<'EOF'
#!/usr/bin/env bash
printf 'pg_dump %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
source "$AUTODEPLOY_TEST_TOOLS/libpq.sh"
connection_password pg_dump "$@" || exit 1
printf 'backup'
EOF
chmod +x "$bin/jj" "$bin/docker" "$bin/curl" "$tools/psql" "$tools/pg_dump"

deploy_once() {
  PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" AUTODEPLOY_TEST_ARGV="$argv" AUTODEPLOY_TEST_SCHEMA="$schema" \
    AUTODEPLOY_TEST_TOOLS="$tools" AUTODEPLOY_TEST_PASSWORD="${test_password-$password}" REPO="$repo" COMPOSE_DIR="$compose" \
    BACKUPS="$backups" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
    "$(dirname "$0")/autodeploy.sh" --once 2>&1
}
fail() {
  printf '%s\n' "$1" >&2
  printf -- '--- deployer output:\n%s\n--- compose:\n%s\n--- argv:\n%s\n' "$output" "$(<"$log")" "$(<"$argv")" >&2
  exit 1
}
# No secret in the output, and none in any process's arguments, Docker's or the container's.
no_secret() {
  if [[ "$output" == *cret-canary* || "$output" == *review-secret* || "$output" == *PSQL-STDERR-CANARY* ]] ||
    grep -q -e 'cret-canary' -e 'review-secret' "$argv"; then
    fail "$1: a password or psql's stderr reached the output or a process's arguments"
  fi
}

# A deploy that fails its origin check with the schema where it was stops the candidate, confirms
# it stopped, reads the schema, and rolls back. Every database client gets the URL without its
# password and the password as PGPASSWORD.
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
no_secret 'the rollback'
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not restore the previous image tag after the origin check failed'
fi
if ! compgen -G "$backups/pre-new-image-*.dump" >/dev/null; then
  fail 'deployer did not write the pre-deploy database backup'
fi
if [[ $(<"$log") != *" pull "*$'\n'*" up "*$'\n'*" stop dispatch"$'\n'*" ps "*$'\n'*" up "* ]] || [[ $(wc -l <"$log") -ne 5 ]]; then
  fail 'deployer did not pull, deploy, stop, confirm the stop, and roll back after the origin check failed'
fi
if [[ "$output" != *"rolling back to old-image"* ]]; then
  fail 'deployer did not report the redirect-uri rollback'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:112"* ]]; then
  fail 'deployer did not record the schema version old-image runs against'
fi
if [[ $(grep -c "^pg_dump -Fc $url_without_password\$" "$argv") -ne 1 ]] ||
  [[ $(grep -c "^psql .*pg_advisory_lock(8150001).*from schema_migrations.* $url_without_password\$" "$argv") -ne 2 ]]; then
  fail 'deployer did not run pg_dump once and psql twice (before the deploy and after the stop) on the URL without its password, the read after the runner lock'
fi
# Each client container runs under an init that passes on a signal, so `timeout` ends it.
if [[ $(grep -c '^docker run ' "$argv") -ne 3 || $(grep -c '^docker run --init ' "$argv") -ne 3 ]]; then
  fail 'deployer ran a database client container without --init, which a timeout could not stop'
fi

# A deploy whose image moved the schema before it failed refuses to start the previous image
# against that schema: it stops the new one, names both versions, and fails.
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail after a deploy that moved the schema failed its origin check'
fi
no_secret 'the refusal'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113"*"forward recovery is required"* ]]; then
  fail 'deployer did not refuse the rollback naming both schema versions'
fi
if [[ $(grep -c ' up ' "$log") -ne 1 || $(grep -c ' stop dispatch' "$log") -ne 1 ]]; then
  fail 'deployer did not deploy once and stop the new image once, without rolling back'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer persisted the failed image tag after refusing its rollback'
fi

# The next pass retries the same image. The schema is already at 113 before it starts, and the
# rollback is still refused: old-image runs against 112, as recorded on the first attempt.
: >"$log"
if output=$(deploy_once); then
  fail 'deployer did not fail on the retry of an image whose rollback it refused'
fi
no_secret 'the retry'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back to old-image against schema 113 on the retry'
fi

# A migration the failed image held open commits only as the image stops. The deployer reads the
# schema after the stop, so it sees 113 and refuses; a read before the stop would see 112.
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_ON_STOP=113 deploy_once); then
  fail 'deployer did not fail when the failed image migrated as it stopped'
fi
no_secret 'the migration committing as the image stops'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]] ||
  [[ $(<"$schema") != 113 ]]; then
  fail 'deployer rolled back to old-image while the failed image held a migration open'
fi

# A stop that fails, or that leaves the candidate running, proves nothing about the schema.
for stop in AUTODEPLOY_TEST_STOP_FAIL AUTODEPLOY_TEST_STOP_LEAVES_RUNNING; do
  fresh_env
  if output=$(export "$stop=1" && deploy_once); then
    fail "deployer did not fail when $stop"
  fi
  no_secret "the refused stop ($stop)"
  if [[ "$output" != *"refusing to roll back to old-image"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
    fail "deployer rolled back without a confirmed stop ($stop)"
  fi
done

# A schema version it cannot read after the stop is no proof the schema stayed: it refuses.
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP=1 deploy_once); then
  fail 'deployer did not fail when it could not read the schema version after a failed deploy'
fi
no_secret 'the unreadable schema'
if [[ "$output" != *"could not read the schema version"*"status 2"*"refusing to roll back to old-image"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back without knowing the schema version'
fi

# A schema version it cannot read before a deploy stops the deploy before anything changes.
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE=1 deploy_once); then
  fail 'deployer deployed without reading the schema version first'
fi
no_secret 'the unreadable schema before the deploy'
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]] || [[ -s "$log" ]]; then
  fail 'deployer did not stop before deploying when the schema version was unreadable'
fi

# Both URL loops below run under en_US.UTF-8, where bash's [[ =~ ]] ranges such as [a-z] and
# [0-9] also match non-ASCII characters, so a grammar that left the locale in force would accept
# what it should refuse. The control needs that locale installed and widening the ranges.
if ! LC_ALL=en_US.UTF-8 bash -c '[[ "ä" =~ ^[a-z]$ ]]' 2>/dev/null; then
  fail 'en_US.UTF-8 is not installed here (sudo locale-gen en_US.UTF-8), so the URL loops would not test the locale'
fi

# The deployer accepts one URL shape and refuses every other before any client starts, so no
# password in a refused URL can reach a process's arguments, and the refusal never prints the URL.
# The cases cover each way out of the shape:
#   - the password: a malformed escape, %00, an escape that decodes to bytes that are not UTF-8, a
#     raw ? / @ or space, raw non-ASCII, and a double-byte character whose second byte is @;
#   - the query: a password under any spelling (password, sslpassword, an encoded key, a space
#     beside the key), a key outside the list, an @, and a non-ASCII value;
#   - the user, host and port: a non-ASCII user, a bracketed, IPv6, multi-host or socket host, an
#     empty or hyphen-edged host label, a port of 0, above 65535 or not ASCII digits;
#   - an @ in the path, and a keyword/value string.
# Most are URLs pgx or libpq would connect with; the fake is given the password libpq would read,
# so a deployer that let one through would show it in some client's arguments.
for case in \
  'review-secret|postgres://dispatch:review-secret%oops@db.test:5432/dispatch' \
  's|postgres://dispatch:s%00cret-canary@db.test:5432/dispatch' \
  'x|postgres://dispatch:s%FFcret-canary@db.test:5432/dispatch' \
  's?cret-canary|postgres://dispatch:s?cret-canary@db.test:5432/dispatch?sslmode=require' \
  's/cret-canary|postgres://dispatch:s/cret-canary@db.test:5432/dispatch' \
  's|postgres://dispatch:s@cret-canary@db.test:5432/dispatch' \
  's cret-canary|postgres://dispatch:s cret-canary@db.test:5432/dispatch' \
  'pässwörd-cret-canary|postgres://dispatch:pässwörd-cret-canary@db.test:5432/dispatch' \
  $'x|postgres://dispatch:pw\xa9@cret-canary@db.test:5432/dispatch' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require&password=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslpassword=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?pass%77ord=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require& password=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?password =review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require&sslpassword =review-secret' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?target_session_attrs=any' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?application_name=a@b&password=review-secret' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?application_name=ｄｅｐｌｏｙ' \
  's@cret-canary|postgres://dispätch:s%40cret-canary@db.test:5432/dispatch' \
  'review-secret|postgres://dispatch@[::1?x]:5432,db.test:5432/dispatch?password=review-secret&sslmode=require' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@[::1]:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db1.test:5432,db2.test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@%2Fvar%2Frun%2Fpostgresql/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db..test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@-db.test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:0/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:65536/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5²/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dis@patch' \
  'review-secret|host=db.test user=dispatch password=review-secret dbname=dispatch'; do
  fresh_env "${case#*|}"
  if output=$(LC_ALL=en_US.UTF-8 test_password="${case%%|*}" deploy_once); then
    fail "deployer deployed with ${case#*|}"
  fi
  no_secret "${case#*|}"
  if [[ "$output" != *"DATABASE_URL"*"not deploying new-image"* ]] || [[ "$output" == *"${case#*|}"* ]] ||
    [[ -s "$log" || -s "$argv" ]]; then
    fail "deployer did not refuse ${case#*|} before running anything, without printing it"
  fi
done

# The accepted shape splits as intended: each allowed query key, the highest port, a URL with no
# port, escapes that decode to reserved characters or to multi-byte UTF-8, and a URL with no
# password at all. The clients get the URL without its password as their last argument, the
# password reaches the fake libpq only as PGPASSWORD, and the deploy goes on (here to the origin
# check's rollback).
for case in \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?sslmode=require&connect_timeout=10&application_name=dispatch-deploy|postgres://dispatch@db.test:5432/dispatch?sslmode=require&connect_timeout=10&application_name=dispatch-deploy' \
  'Az09._~-:/?#[] @|postgresql://dispatch.ro:Az09._~-%3A%2F%3F%23%5B%5D%20%40@db-1.internal.example/dispatch_prod|postgresql://dispatch.ro@db-1.internal.example/dispatch_prod' \
  'süß-canary|postgres://dispatch:s%C3%BC%C3%9F-canary@10.0.0.7:65535/dispatch|postgres://dispatch@10.0.0.7:65535/dispatch' \
  '|postgres://dispatch@db.test/dispatch|postgres://dispatch@db.test/dispatch'; do
  IFS='|' read -r case_password case_url case_client_url <<<"$case"
  fresh_env "$case_url"
  if output=$(LC_ALL=en_US.UTF-8 test_password="$case_password" deploy_once); then
    fail "deployer did not reach the origin check with $case_url"
  fi
  no_secret "$case_url"
  if [[ "$output" != *"rolling back to old-image"* ]] ||
    [[ $(awk -v url="$case_client_url" '$NF == url' "$argv" | wc -l) -ne 3 ]] ||
    { [[ -n "$case_password" ]] && grep -qF -- "$case_password" "$argv"; }; then
    fail "deployer did not hand the clients $case_client_url, with the password as PGPASSWORD alone"
  fi
done

# A failed pull changes nothing: the previous image still serves, so there is nothing to stop or
# restart, and the next pass tries the pull again.
fresh_env
for _ in 1 2; do
  if output=$(AUTODEPLOY_TEST_PULL_FAIL=1 deploy_once); then
    fail 'deployer did not fail when the image pull failed'
  fi
done
no_secret 'the failed pull'
if [[ $(grep -c ' pull ' "$log") -ne 2 || $(wc -l <"$log") -ne 2 ]]; then
  fail 'deployer did not retry the failed image pull on the next --once pass, or touched the running service'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer persisted the failed image tag after a pull failure'
fi
printf '%s\n' 'autodeploy --once rollback, schema-advance refusal, stop-first, credential and pull-retry cases: PASS'
