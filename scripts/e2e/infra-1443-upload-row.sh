#!/usr/bin/env bash
# dispatch://INFRA-1443, the one open question of the spike: does running inside the candidate
# container image cause `e2e/keyed-writes.e2e.ts:491` -- "a 20 MB upload repeated while its first
# request runs is answered 409 KEY_IN_FLIGHT, never a failed connection, and lands once" -- to
# fail, or does that row fail on this runner anyway?
#
# The row is run three times inside the image and three times on the bare runner, at one commit,
# one lane each time so the row is alone on the machine. Everything but the container is held as
# close as the two shapes allow:
#
#   bare       what ci.yaml does today: DISPATCH_E2E_PG_IMAGE (a `docker run` of the pgvector
#              copy per lane) and the file store as a `docker run` of the seaweedfs copy
#              published on 127.0.0.1:18333.
#   container  what INFRA-1443 proposes: the image's own PostgreSQL 18 binaries through
#              DISPATCH_E2E_PG_BIN, and the image's baked `weed` on the same address.
#
# The two legs therefore differ in the file store's and the database's provider as well as in the
# container, which is the design's own difference; what the pass counts answer is whether the
# proposed shape can run this row at all.
#
# Run it from a checkout on a Linux host with docker, bun, go and this repository's node_modules,
# as INFRA_1443_UPLOAD_EVIDENCE_DIR=/tmp/evidence bash scripts/e2e/infra-1443-upload-row.sh
set -euo pipefail

cd "$(dirname "$0")/../.."
repo="$PWD"

evidence="${INFRA_1443_UPLOAD_EVIDENCE_DIR:-$repo/infra-1443-upload-evidence}"
mkdir -p "$evidence"

row='a 20 MB upload repeated'
image="infra-1443/browser:spike"
buildkit="ghcr.io/cybermonklabs/legion/ci@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea"
# The two images ci.yaml itself names (.github/ci-images.yaml): the pgvector copy its lanes run
# and the seaweedfs copy its file store runs.
pg_image="ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
seaweed="ghcr.io/cybermonklabs/legion/ci@sha256:bb05d66d2963b1cc48073190781c3dd29e2c4c88a2ae2986bf58e38c86d89c6e"

say() { printf '\n=== %s\n' "$*"; }

say "the runner"
uname -a; nproc; free -m | cat
echo "id: $(id)"; echo "HOME=$HOME"
echo "commit: $(git rev-parse HEAD 2>/dev/null || echo '<unknown>')"

say "build the candidate image"
docker buildx create --name infra1443u --driver docker-container \
  --driver-opt "image=$buildkit" --driver-opt provenance-add-gha=false --use > /dev/null
docker buildx build --builder infra1443u \
  --file .github/docker/browser.Dockerfile --platform linux/amd64 --provenance=false \
  --tag "$image" --load .github/docker > "$evidence/build.log" 2>&1
echo "built $image"

# One build of the three things every leg's servers read, done here on the host as the runner's
# own uid, so both legs share them and neither rebuilds as another user.
say "build the contracts, the dashboard and the Dispatch server"
(cd packages/contracts && bun run build) > "$evidence/contracts.log" 2>&1
(cd packages/dispatch && bun run build:web) > "$evidence/dashboard.log" 2>&1
(cd packages/envoy && go build -o ./dispatch-e2e ./cmd/dispatch) > "$evidence/server.log" 2>&1
ls -l packages/dispatch/web/dist/THIRD_PARTY_NOTICES.txt packages/envoy/dispatch-e2e

bare_pass=0
bare_runs=0
container_pass=0
container_runs=0

# The runner's font set before anything is installed on it. Read here, because the
# `--with-deps` below adds Playwright's own `tools` font packages to this machine and the
# difference between the two readings is exactly what `--with-deps` buys a bare job.
say "the GitHub ubuntu-24.04 runner's fonts, before any install"
{
  dpkg-query -W -f '${Package}\n' 'fonts-*' 'ttf-*' 'xfonts-*' 2>/dev/null | sort || true
  echo "families: $(fc-list : family 2>/dev/null | sed 's/,.*//' | sort -u | tr '\n' '|')"
  echo "fc-match sans-serif: $(fc-match sans-serif 2>/dev/null)"
  echo "fc-match serif:      $(fc-match serif 2>/dev/null)"
  echo "fc-match monospace:  $(fc-match monospace 2>/dev/null)"
  echo "font files: $(find /usr/share/fonts /usr/local/share/fonts -type f \( -name '*.ttf' -o -name '*.ttc' -o -name '*.otf' -o -name '*.pfb' \) 2>/dev/null | wc -l)"
} | tee "$evidence/fonts-runner-before.txt"

say "the bare runner: Firefox and its host packages, as ci.yaml installs them"
# The runner image preconfigures Google's chrome-stable apt source, whose index intermittently
# fails `apt-get update`; ci.yaml drops it before installing, and so does this.
grep -rl dl.google.com /etc/apt/sources.list.d 2>/dev/null | xargs -r sudo rm -f || true
# From packages/dispatch, as ci.yaml's own install step runs it (`working-directory:
# packages/dispatch`): the first attempt ran `bunx playwright install` at the repository root,
# where bunx resolved a Playwright of its own rather than the workspace's pinned 1.64.1, so it
# downloaded firefox-1572 while the suite went looking for firefox-1555 and every bare attempt
# failed with `Executable doesn't exist at .../firefox-1555/firefox/firefox` (run 42345557938).
( cd packages/dispatch && timeout 900 bunx playwright install --with-deps firefox ) \
  > "$evidence/playwright-install.log" 2>&1 ||
  { echo "::warning::installing Firefox and its host packages failed; the bare leg cannot run"; tail -20 "$evidence/playwright-install.log"; }
( cd packages/dispatch && bunx playwright --version )
ls /home/runner/.cache/ms-playwright 2>/dev/null || true

say "the bare runner: the file store, as ci.yaml runs it"
docker rm -f dispatch-e2e-files > /dev/null 2>&1 || true
docker run -d --name dispatch-e2e-files -p 127.0.0.1:18333:8333 "$seaweed" \
  server -s3 -dir=/data -ip=0.0.0.0 -s3.port=8333 > /dev/null
for attempt in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:18333/ && break
  [ "$attempt" -lt 60 ] || { docker logs dispatch-e2e-files; exit 1; }
  sleep 1
done
curl -sf -X PUT -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files
echo "the file store answers on 127.0.0.1:18333"

for attempt in 1 2 3; do
  say "bare attempt $attempt of 3"
  bare_runs=$((bare_runs + 1))
  # DATABASE_URL is the proof job's own postgres service, and a run that names it starts no
  # provider; the row must run against a lane of its own, as it does in ci.yaml.
  if (
    cd packages/dispatch
    unset DATABASE_URL
    export TMPDIR="${TMPDIR:-/tmp}"
    export DISPATCH_E2E_PG_IMAGE="$pg_image"
    export DISPATCH_E2E_WORKERS=1
    export DISPATCH_E2E_FILE_STORE_ENDPOINT=http://127.0.0.1:18333
    export DISPATCH_E2E_FILE_STORE_BUCKET=dispatch-e2e-files
    bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts \
      --project firefox -g "$row" --reporter=list
  ) > "$evidence/bare-$attempt.log" 2>&1; then
    bare_pass=$((bare_pass + 1))
    echo "bare attempt $attempt: PASS"
  else
    echo "bare attempt $attempt: FAIL"
  fi
  tail -25 "$evidence/bare-$attempt.log"
done
docker rm -f dispatch-e2e-files > /dev/null 2>&1 || true

say "the container: the candidate image, as the design proposes"
box=infra1443u
home="$HOME/infra-1443-upload-home"
rm -rf "$home"; mkdir -p "$home"; chmod 0777 "$home"
docker rm -f "$box" > /dev/null 2>&1 || true
# The mount set, working directory and HOME a GitHub `container:` job is given, and uid 1001,
# which is the only uid that can run `initdb`. `bash -c`, never `bash -lc`: Debian's
# /etc/profile assigns PATH outright and would throw this one away.
docker create --name "$box" --user 1001 --workdir "$repo" \
  --volume "$HOME:$HOME" \
  --volume /opt/hostedtoolcache:/opt/hostedtoolcache \
  --env "HOME=$home" \
  --env "PATH=$(dirname "$(command -v bun)"):$(dirname "$(command -v go)"):/usr/lib/postgresql/18/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  --entrypoint tail "$image" -f /dev/null > /dev/null
docker start "$box" > /dev/null
in=(docker exec --workdir "$repo" "$box")
"${in[@]}" bash -c '
  set -eu
  store="$HOME/weed"
  rm -rf "$store"; mkdir -p "$store"
  nohup weed server -s3 -dir="$store" -ip=127.0.0.1 -s3.port=18333 > "$HOME/weed.log" 2>&1 &
  for attempt in $(seq 1 60); do
    curl -sf -o /dev/null http://127.0.0.1:18333/ && break
    [ "$attempt" -lt 60 ] || { cat "$HOME/weed.log"; exit 1; }
    sleep 1
  done
  curl -sf -X PUT -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files
  echo "the baked weed answers on 127.0.0.1:18333"
'

for attempt in 1 2 3; do
  say "container attempt $attempt of 3"
  container_runs=$((container_runs + 1))
  if "${in[@]}" bash -c '
    set -u
    cd packages/dispatch
    export TMPDIR=/tmp
    export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
    export DISPATCH_E2E_WORKERS=1
    export DISPATCH_E2E_FILE_STORE_ENDPOINT=http://127.0.0.1:18333
    export DISPATCH_E2E_FILE_STORE_BUCKET=dispatch-e2e-files
    bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts \
      --project firefox -g "'"$row"'" --reporter=list
  ' > "$evidence/container-$attempt.log" 2>&1; then
    container_pass=$((container_pass + 1))
    echo "container attempt $attempt: PASS"
  else
    echo "container attempt $attempt: FAIL"
  fi
  tail -25 "$evidence/container-$attempt.log"
done

say "the font inventory, which the docs screenshot job's --with-deps question needs"
{
  echo "--- the GitHub ubuntu-24.04 runner ---"
  dpkg-query -W -f '${Package}\t${Installed-Size}\n' 'fonts-*' 'ttf-*' 'xfonts-*' 2>/dev/null | sort || true
  echo "families: $(fc-list : family 2>/dev/null | sed 's/,.*//' | sort -u | tr '\n' '|')"
  echo "fc-match sans-serif: $(fc-match sans-serif 2>/dev/null)"
  echo "fc-match serif:      $(fc-match serif 2>/dev/null)"
  echo "fc-match monospace:  $(fc-match monospace 2>/dev/null)"
  echo "font files: $(find /usr/share/fonts /usr/local/share/fonts -type f \( -name '*.ttf' -o -name '*.ttc' -o -name '*.otf' -o -name '*.pfb' \) 2>/dev/null | wc -l)"
} | tee "$evidence/fonts-runner.txt"
"${in[@]}" bash -c '
  echo "--- the candidate image ---"
  dpkg-query -W -f "\${Package}\t\${Installed-Size}\n" "fonts-*" "ttf-*" "xfonts-*" 2>/dev/null | sort || true
  echo "families: $(fc-list : family 2>/dev/null | sed "s/,.*//" | sort -u | tr "\n" "|")"
  echo "fc-match sans-serif: $(fc-match sans-serif 2>/dev/null)"
  echo "fc-match serif:      $(fc-match serif 2>/dev/null)"
  echo "fc-match monospace:  $(fc-match monospace 2>/dev/null)"
  echo "font files: $(find /usr/share/fonts /usr/local/share/fonts -type f \( -name "*.ttf" -o -name "*.ttc" -o -name "*.otf" -o -name "*.pfb" \) 2>/dev/null | wc -l)"
' 2>&1 | tee "$evidence/fonts-image.txt"

docker rm -f "$box" > /dev/null 2>&1 || true
docker buildx rm infra1443u > /dev/null 2>&1 || true

say "the answer"
echo "the 20 MB upload row, one lane, three attempts each, commit $(git rev-parse --short HEAD 2>/dev/null || echo '<unknown>'):"
echo "  bare runner (DISPATCH_E2E_PG_IMAGE, the file store in its own container): $bare_pass of $bare_runs passed"
echo "  the candidate image (DISPATCH_E2E_PG_BIN, the baked weed):                $container_pass of $container_runs passed"
# This script reports; it does not judge. A leg that fails is the reading.
exit 0
