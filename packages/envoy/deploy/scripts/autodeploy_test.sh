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
DISPATCH_PG_PORT=55432
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
  exec)
    if [[ "${2:-}" != '-e' || "${3:-}" != 'PORT=55432' || "${7:-}" != 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h 127.0.0.1 -p "$PORT" -U postgres -Fc dispatch' ]]; then
      printf '%s\n' 'pg_dump must use the container POSTGRES_PASSWORD over TCP' >&2
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
