#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
repo="$root/repo"
compose="$repo/packages/envoy/deploy/compose"
backups="$root/backups"
bin="$root/bin"
log="$root/compose.log"
mkdir -p "$compose" "$backups" "$bin"
cat >"$compose/.env" <<'EOF'
ENVOY_IMAGE_TAG=old-image
DATABASE_URL=postgres://dispatch:secret@db.test:5432/dispatch?sslmode=require
DISPATCH_SERVER_URL=http://browser.test:8766
EOF

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
    if [[ "$*" != *'--network host'* || "$*" != *' -e DATABASE_URL '* || "$*" != *'pg_dump -Fc "$DATABASE_URL"'* ]]; then
      printf '%s\n' 'pg_dump must run with DATABASE_URL passed by name from the environment' >&2
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
    printf 'backup'
    ;;
  compose)
    printf '%s\n' "$*" >>"$AUTODEPLOY_TEST_LOG"
    if [[ "${AUTODEPLOY_TEST_PULL_FAIL:-}" == 1 && "$*" == *" pull "* ]]; then
      exit 1
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

if output=$(PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" REPO="$repo" COMPOSE_DIR="$compose" \
  BACKUPS="$backups" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
  "$(dirname "$0")/autodeploy.sh" --once); then
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

: >"$log"
for _ in 1 2; do
  if PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" AUTODEPLOY_TEST_PULL_FAIL=1 REPO="$repo" COMPOSE_DIR="$compose" \
    BACKUPS="$backups" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
    "$(dirname "$0")/autodeploy.sh" --once; then
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
printf '%s\n' 'autodeploy --once origin rollback and pull retry: PASS'
