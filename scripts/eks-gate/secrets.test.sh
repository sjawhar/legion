#!/usr/bin/env bash
# Harness for secrets.sh: provider values must reach jq on stdin, never its argv or output.
set -Eeuo pipefail
trap 'printf "FAIL %s:%s: %s\n" "${BASH_SOURCE[0]}" "$LINENO" "$BASH_COMMAND" >&2' ERR

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
fake_bin="$tmp/bin"
mkdir -p "$fake_bin"
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"
real_jq="$(command -v jq)"

cat >"$fake_bin/jq" <<EOF
#!/usr/bin/env bash
printf 'jq %s\n' "\$*" >>"\$FAKE_LOG"
exec "$real_jq" "\$@"
EOF
chmod +x "$fake_bin/jq"
cat >"$fake_bin/kubectl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *" apply -f -") cat >/dev/null ;;
  *) echo "unexpected kubectl request: $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$fake_bin/kubectl"
export PATH="$fake_bin:$PATH"

# shellcheck disable=SC2120,SC2119 # The first invocation intentionally supplies no arguments.
run_secrets() {
  local status=0
  bash "$here/secrets.sh" "$@" >"$tmp/out.txt" 2>&1 || status=$?
  return "$status"
}
assert_line() { grep -Fxq -- "$1" "$tmp/out.txt" || { cat "$tmp/out.txt" >&2; exit 1; }; }

status=0
run_secrets || status=$?
[ "$status" = 2 ]
assert_line 'GATE secrets/context FAILED: --context is required'

: >"$FAKE_LOG"
env ANTHROPIC_API_KEY=anthropic-canary-value GEMINI_API_KEY=gemini-canary-value \
  OPENAI_API_KEY=openai-canary-value DISPATCH_TOKEN=dispatch-canary-value ENVOY_TOKEN=envoy-canary-value \
  bash "$here/secrets.sh" --context production --name legion-sjawharlegion-providers >"$tmp/out.txt"
assert_line 'GATE secrets/providers-secret OK: legion-sjawharlegion-providers keys ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY DISPATCH_TOKEN ENVOY_TOKEN'
printf '%s\n' anthropic-canary-value gemini-canary-value openai-canary-value dispatch-canary-value envoy-canary-value >"$tmp/values"
if grep -Fq -f "$tmp/values" "$FAKE_LOG" "$tmp/out.txt"; then
  echo 'provider value reached jq argv or output' >&2
  exit 1
fi

echo 'secrets.test.sh: OK'
