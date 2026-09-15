#!/usr/bin/env bash
# Harness for scripts/kind-smoke/up.sh: every external binary is a PATH fake that logs its argv, so the
# script's decisions, records, and refusals are pinned without docker, kind, kubectl, go, tmux, or a
# network. Needs only bash, coreutils, and jq.
set -euo pipefail
here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
cleanup() {
  local f
  for f in "$tmp"/state*/pids/*.pid; do
    [ -f "$f" ] || continue
    kill -- "-$(<"$f")" 2>/dev/null || kill "$(<"$f")" 2>/dev/null || true
  done
  rm -rf -- "$tmp"
}
trap cleanup EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"

fake() { # fake NAME <<'EOF' body EOF — every fake logs "NAME argv" to $FAKE_LOG, then runs the body
  {
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}
for t in docker kind kubectl go bun tmux ss mise setsid curl; do fake "$t" <<<'exit 0'; done
export PATH="$fake_bin:$PATH"

good_image="ghcr.io/sjawhar/legion-worker@sha256:$(printf 'a%.0s' $(seq 64))"
pem_b64() { printf -- '-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n' "$1" | base64 -w0; }

run_up() { # run_up ENV… — runs up.sh with the harness environment; captures stdout+stderr; returns its exit code
  local out="$tmp/out.txt"
  : >"$out"
  set +e
  env SMOKE_DIR="$tmp/state" SMOKE_INSTANCE=t1 SMOKE_PORT_BASE=41000 SMOKE_POLL_INTERVAL=0 \
    SMOKE_WORKER_IMAGE="$good_image" \
    GH_AGENT_APP_PRIVATE_KEY_B64="$(pem_b64 x)" GH_REVIEW_APP_PRIVATE_KEY_B64="$(pem_b64 y)" \
    ANTHROPIC_API_KEY=anthropic-canary-value GEMINI_API_KEY= OPENAI_API_KEY= \
    "$@" bash "$here/up.sh" >"$out" 2>&1
  local status=$?
  set -e
  cat "$out"
  return $status
}
expect_refusal() { # expect_refusal 'substring' ENV… — up.sh must exit 1 with the text and create nothing
  local want="$1"
  shift
  if run_up "$@" >"$tmp/last.txt"; then
    echo "expected refusal: $want" >&2
    cat "$tmp/last.txt" >&2
    exit 1
  fi
  grep -Fq -- "$want" "$tmp/last.txt" || { echo "missing refusal text: $want" >&2; cat "$tmp/last.txt" >&2; exit 1; }
  ! grep -Eq '^(docker (run|start|rm|exec)|kind (create|delete)|kubectl (apply|delete|port-forward)|go build|tmux new-session|bun run) ' "$FAKE_LOG" || { echo "refusal created something:" >&2; cat "$FAKE_LOG" >&2; exit 1; }
  : >"$FAKE_LOG"
}

# 1. a tag is refused before anything is created
expect_refusal 'SMOKE_WORKER_IMAGE must be pinned by digest (…@sha256:<64 hex>); got ghcr.io/sjawhar/legion-worker:latest' \
  SMOKE_WORKER_IMAGE=ghcr.io/sjawhar/legion-worker:latest
# 2. instance name rules
expect_refusal 'SMOKE_INSTANCE must be 1-9 lowercase letters or digits' SMOKE_INSTANCE=this-is-way-too-long
# 3. every missing tool is named in one line: a PATH with fakes for everything but kind and kubectl
nokind="$tmp/bin-nokind"
mkdir -p "$nokind"
for t in docker go bun tmux ss mise setsid curl jq openssl shred; do
  [ -e "$fake_bin/$t" ] && cp "$fake_bin/$t" "$nokind/$t" || ln -s "$(command -v "$t")" "$nokind/$t"
done
ln -s "$(command -v bash)" "$nokind/bash"
expect_refusal 'missing required tools: kind kubectl' PATH="$nokind"
# 4. an occupied port names the port and SMOKE_PORT_BASE
fake ss <<'EOF'
if [[ "$*" == *"sport = :41002"* ]]; then echo "LISTEN 0 4096 172.30.0.1:41002 0.0.0.0:*"; fi
EOF
expect_refusal "port 41002 (dispatch) is already in use and is not this instance's; choose another SMOKE_PORT_BASE (current 41000)"
fake ss <<<'exit 0'
# 5. no provider key in the environment
expect_refusal 'no provider key in the environment (ANTHROPIC_API_KEY, GEMINI_API_KEY, or OPENAI_API_KEY)' ANTHROPIC_API_KEY=
# 6. no App private key in either form
expect_refusal 'GH_AGENT_APP_PRIVATE_KEY_B64 is unset and SMOKE_IMPLEMENT_APP_KEY_FILE is unset' GH_AGENT_APP_PRIVATE_KEY_B64=
# 6b. a PEM path that is not readable
expect_refusal "SMOKE_REVIEW_APP_KEY_FILE names $tmp/absent.pem, which is not readable" GH_REVIEW_APP_PRIVATE_KEY_B64= SMOKE_REVIEW_APP_KEY_FILE="$tmp/absent.pem"
# 7. postgres session store needs the checkout to carry it (this checkout is judged by grep on config.ts)
if ! grep -q session_dsn_secret "$here/../../packages/daemon/src/daemon/config.ts"; then
  expect_refusal 'SMOKE_SESSION_STORE=postgres needs runtime.kubernetes.session_store in packages/daemon/src/daemon/config.ts (pull request #1108)' SMOKE_SESSION_STORE=postgres
fi
# 8. bad mode values
expect_refusal 'SMOKE_GITHUB_INGRESS must be none or envoy; got webhook' SMOKE_GITHUB_INGRESS=webhook
expect_refusal 'SMOKE_ROOT_ISSUES must be a positive integer; got 0' SMOKE_ROOT_ISSUES=0
echo "up.test.sh: refusals OK"
