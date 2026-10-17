#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
repo="$root/repo"
compose="$repo/packages/envoy/deploy/compose"
backups="$root/backups"
bin="$root/bin"
log="$root/compose.log"
schema="$root/schema"
mkdir -p "$compose" "$backups" "$bin"
fresh_env() {
  cat >"$compose/.env" <<'EOF'
ENVOY_IMAGE_TAG=old-image
DATABASE_URL=postgres://dispatch:secret@db.test:5432/dispatch?sslmode=require
DISPATCH_SERVER_URL=http://browser.test:8766
EOF
}
fresh_env
# The database's schema version, as the fake psql answers it. The fake `compose up` of new-image
# moves it to AUTODEPLOY_TEST_MIGRATE_TO when that is set, as a new image's boot migration would.
printf '112\n' >"$schema"

cat >"$bin/jj" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *" git fetch"*) ;;
  *"ancestors(main@origin, 30)"*) printf 'new-image\n' ;;
  *"description.first_line()"*) printf 'test deployment\n' ;;
esac
EOF
cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  manifest)
    exit 0
    ;;
  run)
    if [[ "$*" != *'--network host'* || "$*" != *' -e DATABASE_URL '* ]]; then
      printf '%s\n' 'pg_dump and psql must run with DATABASE_URL passed by name from the environment' >&2
      exit 1
    fi
    if [[ "$*" == *'DATABASE_URL='* ]]; then
      printf '%s\n' 'the database URL (password) must not appear in docker argv' >&2
      exit 1
    fi
    if [[ "${DATABASE_URL:-}" != 'postgres://dispatch:secret@db.test:5432/dispatch?sslmode=require' ]]; then
      printf '%s\n' 'DATABASE_URL must be exported to docker run from compose/.env' >&2
      exit 1
    fi
    if [[ "$*" == *'pg_dump -Fc "$DATABASE_URL"'* ]]; then
      printf 'backup'
    elif [[ "$*" == *'psql '*'from schema_migrations'*'"$DATABASE_URL"'* ]]; then
      if [[ "${AUTODEPLOY_TEST_SCHEMA_UNREADABLE:-}" == 1 ||
        ("${AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP:-}" == 1 && -e "$AUTODEPLOY_TEST_SCHEMA.up") ]]; then
        exit 2
      fi
      cat "$AUTODEPLOY_TEST_SCHEMA"
    else
      printf 'unexpected docker run: %s\n' "$*" >&2
      exit 1
    fi
    ;;
  compose)
    printf '%s\n' "$*" >>"$AUTODEPLOY_TEST_LOG"
    if [[ "${AUTODEPLOY_TEST_PULL_FAIL:-}" == 1 && "$*" == *" pull "* ]]; then
      exit 1
    fi
    if [[ "${ENVOY_IMAGE_TAG:-}" == new-image && "$*" == *" up "* ]]; then
      touch "$AUTODEPLOY_TEST_SCHEMA.up"
      if [[ -n "${AUTODEPLOY_TEST_MIGRATE_TO:-}" ]]; then
        printf '%s\n' "$AUTODEPLOY_TEST_MIGRATE_TO" >"$AUTODEPLOY_TEST_SCHEMA"
      fi
    fi
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
chmod +x "$bin/jj" "$bin/docker" "$bin/curl"

deploy_once() {
  PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" AUTODEPLOY_TEST_SCHEMA="$schema" REPO="$repo" COMPOSE_DIR="$compose" \
    BACKUPS="$backups" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
    "$(dirname "$0")/autodeploy.sh" --once 2>&1
}

# A deploy that fails its origin check with the schema where it was rolls back, as before.
if output=$(deploy_once); then
  printf '%s\n' 'deployer did not fail after the origin check rejected the deployment' >&2
  exit 1
fi

if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  printf '%s\n' 'deployer did not restore the previous image tag after the origin check failed' >&2
  exit 1
fi
if ! compgen -G "$backups/pre-new-image-*.dump" >/dev/null; then
  printf '%s\n' 'deployer did not write the pre-deploy database backup' >&2
  exit 1
fi
if [[ $(wc -l <"$log") -ne 3 ]]; then
  printf '%s\n' 'deployer did not pull, deploy, and roll back after the origin check failed' >&2
  exit 1
fi
if [[ "$output" != *"rolling back to old-image"* ]]; then
  printf '%s\n' 'deployer did not report the redirect-uri rollback' >&2
  exit 1
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:112"* ]]; then
  printf '%s\n' 'deployer did not record the schema version old-image runs against' >&2
  exit 1
fi

# A deploy whose image moved the schema before it failed refuses to start the previous image
# against that schema: it stops the new one, names both versions, and fails.
fresh_env
: >"$log"
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  printf '%s\n' 'deployer did not fail after a deploy that moved the schema failed its origin check' >&2
  exit 1
fi
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113"*"forward recovery is required"* ]]; then
  printf 'deployer did not refuse the rollback naming both schema versions; it said:\n%s\n' "$output" >&2
  exit 1
fi
if grep -qv -e ' pull ' -e ' up ' -e ' stop dispatch' "$log" || [[ $(grep -c ' up ' "$log") -ne 1 ]] ||
  [[ $(grep -c ' stop dispatch' "$log") -ne 1 ]]; then
  printf 'deployer did not pull, deploy, and stop the new image without rolling back; compose ran:\n%s\n' "$(<"$log")" >&2
  exit 1
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  printf '%s\n' 'deployer persisted the failed image tag after refusing its rollback' >&2
  exit 1
fi

# The next pass retries the same image. The schema is already at 113 before it starts, and the
# rollback is still refused: old-image runs against 112, as recorded on the first attempt.
: >"$log"
if output=$(deploy_once); then
  printf '%s\n' 'deployer did not fail on the retry of an image whose rollback it refused' >&2
  exit 1
fi
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  printf 'deployer rolled back to old-image against schema 113 on the retry; it said:\n%s\ncompose ran:\n%s\n' "$output" "$(<"$log")" >&2
  exit 1
fi

# A schema version it cannot read after a failed deploy is no proof the schema stayed: it refuses.
fresh_env
printf '112\n' >"$schema"
rm -f "$schema.up"
: >"$log"
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP=1 deploy_once); then
  printf '%s\n' 'deployer did not fail when it could not read the schema version after a failed deploy' >&2
  exit 1
fi
if [[ "$output" != *"could not read the schema version"*"refusing to roll back to old-image"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  printf 'deployer rolled back without knowing the schema version; it said:\n%s\ncompose ran:\n%s\n' "$output" "$(<"$log")" >&2
  exit 1
fi

# A schema version it cannot read before a deploy stops the deploy before anything changes.
fresh_env
: >"$log"
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE=1 deploy_once); then
  printf '%s\n' 'deployer deployed without reading the schema version first' >&2
  exit 1
fi
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]] || [[ -s "$log" ]]; then
  printf 'deployer did not stop before deploying when the schema version was unreadable; it said:\n%s\n' "$output" >&2
  exit 1
fi

fresh_env
: >"$log"
for _ in 1 2; do
  if AUTODEPLOY_TEST_PULL_FAIL=1 deploy_once >/dev/null; then
    printf '%s\n' 'deployer did not fail when the image pull failed' >&2
    exit 1
  fi
done
if [[ $(wc -l <"$log") -ne 4 ]]; then
  printf '%s\n' 'deployer did not retry the failed image pull on the next --once pass' >&2
  exit 1
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  printf '%s\n' 'deployer persisted the failed image tag after a pull failure' >&2
  exit 1
fi
printf '%s\n' 'autodeploy --once origin rollback, schema-advance refusal and pull retry: PASS'
