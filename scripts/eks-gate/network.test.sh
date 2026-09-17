#!/usr/bin/env bash
# Harness for network.sh: PATH fakes capture the gate's manifests, checks, and cleanup.
set -Eeuo pipefail
trap 'printf "FAIL %s:%s: %s\n" "${BASH_SOURCE[0]}" "$LINENO" "$BASH_COMMAND" >&2' ERR

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
export FAKE_MANIFEST="$tmp/manifest.yaml"
: >"$FAKE_LOG"

fake() {
  {
    # shellcheck disable=SC2016  # The generated fake expands these variables when it runs.
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" %q "$*" >>"$FAKE_LOG"\n' "$1"
    cat
  } >"$fake_bin/$1"
  chmod +x "$fake_bin/$1"
}

fake kubectl <<'EOF'
all="$*"
case "$all" in
  *" apply -f -"*) cat >"$FAKE_MANIFEST" ;;
  *" wait --for=condition=Ready "*) ;;
  *" get pvc legion-gate2 -o json"*) echo '{"status":{"phase":"Bound"}}' ;;
  *" get nodes -l legion.dev/pool=legion -o jsonpath="*) echo 'aws:///us-west-2a/i-gate2' ;;
  *" exec legion-gate2-default "*)
    host="${@: -2:1}"; port="${@: -1}"
    if [ "$host:$port" = 'dispatch.internal.trajectorylabs.com:443' ]; then exit 0; fi
    if [ -n "${FAKE_DEFAULT_REACHES_NATS:-}" ] && [ "$host:$port" = 'nats.internal.trajectorylabs.com:4222' ]; then exit 0; fi
    exit 1
    ;;
  *" exec legion-gate2 "*) exit 0 ;;
  *" delete pod legion-gate2 "*|*" delete pod legion-gate2-default "*|*" delete pvc legion-gate2 "*) ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF
fake aws <<'EOF'
case "$*" in
  "ec2 describe-instances "*) printf 'eks-cluster-sg-production-1753018553\tlegion-nodes\n' ;;
  *) echo "unexpected aws request: $*" >&2; exit 1 ;;
esac
EOF
fake nc <<'EOF'
exit 0
EOF
fake curl <<'EOF'
case "$*" in
  *'/latest/api/token'*) printf 'imds-token-canary\n' ;;
  *'/latest/meta-data/local-ipv4'*) printf '10.1.10.136\n' ;;
  *) echo "unexpected curl request: $*" >&2; exit 1 ;;
esac
EOF
export PATH="$fake_bin:$PATH"

run_network() {
  local status=0
  bash "$here/network.sh" "$@" >"$tmp/out.txt" 2>&1 || status=$?
  return "$status"
}
assert_line() { grep -Fxq -- "$1" "$tmp/out.txt" || { cat "$tmp/out.txt" >&2; exit 1; }; }
assert_cleanup() {
  grep -Fq 'kubectl --context production -n legion delete pod legion-gate2 ' "$FAKE_LOG"
  grep -Fq 'kubectl --context production -n legion delete pod legion-gate2-default ' "$FAKE_LOG"
  grep -Fq 'kubectl --context production -n legion delete pvc legion-gate2 ' "$FAKE_LOG"
}

status=0
run_network || status=$?
[ "$status" = 2 ]
cat "$tmp/out.txt"
assert_line 'GATE network/context FAILED: --context is required'

: >"$FAKE_LOG"
run_network --context production --devbox-ip 10.1.10.136
assert_line 'GATE network/pvc-bound OK: legion-gate2 Bound'
assert_line 'GATE network/legion-pool-reaches nats:4222 OK'
assert_line 'GATE network/legion-pool-reaches envoy-listener:9020 OK'
assert_line 'GATE network/legion-pool-reaches dispatch:443 OK'
assert_line 'GATE network/legion-pool-reaches daemon:13370 OK'
assert_line 'GATE network/legion-pool-reaches worker-stream:13371 OK'
assert_line 'GATE network/default-pool-refused nats:4222 OK'
assert_line 'GATE network/default-pool-refused envoy-listener:9020 OK'
assert_line 'GATE network/default-pool-reaches dispatch:443 OK'
assert_line 'GATE network/default-pool-refused daemon:13370 OK'
assert_line 'GATE network/default-pool-refused worker-stream:13371 OK'
assert_line 'GATE network/node-security-groups OK: cluster and legion-nodes'
grep -Fq 'name: legion-gate2' "$FAKE_MANIFEST"
grep -Fq 'name: legion-gate2-default' "$FAKE_MANIFEST"
grep -Fq 'name: legion-gate2' "$FAKE_MANIFEST"
assert_cleanup

: >"$FAKE_LOG"
status=0
FAKE_DEFAULT_REACHES_NATS=1 run_network --context production --devbox-ip 10.1.10.136 || status=$?
[ "$status" = 1 ]
assert_line 'GATE network/default-pool-refused nats:4222 FAILED: reachable'
assert_cleanup
: >"$FAKE_LOG"
run_network --context production
assert_line 'GATE network/legion-pool-reaches daemon:13370 OK'
if grep -Fq imds-token-canary "$FAKE_LOG"; then
  echo 'IMDSv2 token reached curl argv' >&2
  exit 1
fi

echo 'network.test.sh: OK'
