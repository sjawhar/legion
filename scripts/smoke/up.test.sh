#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly project_root
readonly up_script="${project_root}/scripts/smoke/up.sh"
source_file="$(mktemp)"
warning_file="$(mktemp)"
assertion_file="$(mktemp)"
fake_bin="$(mktemp -d)"
headers_file="$(mktemp)"
body_file="$(mktemp)"
response_file="$(mktemp)"
gh_call_file="$(mktemp)"
gh_label_env_file="$(mktemp)"
trap 'rm -f "$source_file" "$warning_file" "$assertion_file" "$headers_file" "$body_file" "$response_file" "$gh_call_file" "$gh_label_env_file"; rm -rf "$fake_bin"' EXIT
export SMOKE_DIR="${fake_bin}/smoke"

sed '$d' "$up_script" >"$source_file"
# shellcheck source=/dev/null
source "$source_file"

GITHUB_WEBHOOK_SECRET=$' \tlegion-smoke-secret\r\n '
normalize_github_webhook_secret >"$warning_file" 2>&1

[[ "$GITHUB_WEBHOOK_SECRET" == "legion-smoke-secret" ]] || {
  printf 'expected normalized secret, got %q\n' "$GITHUB_WEBHOOK_SECRET" >&2
  exit 1
}
[[ "$(<"$warning_file")" == *'WARNING: GITHUB_WEBHOOK_SECRET stored secret contains whitespace'* ]] || {
  printf 'expected whitespace warning\n' >&2
  exit 1
}
[[ "$(env | sed -n 's/^GITHUB_WEBHOOK_SECRET=//p')" == "legion-smoke-secret" ]] || {
  printf 'expected normalized secret to be exported\n' >&2
  exit 1
}
export SMOKE_REPO="sjawhar/legion-smoke"
[[ "$(repo_owner)" == "sjawhar" ]] || {
  printf 'expected repository owner for App-scoped label setup\n' >&2
  exit 1
}


cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

while (($#)); do
  case "$1" in
    --header|-H)
      printf '%s\n' "$2" >>"$SMOKE_CURL_HEADERS_FILE"
      shift 2
      ;;
    --data-binary)
      printf '%s' "$2" >"$SMOKE_CURL_BODY_FILE"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

printf '%s' "$(<"$SMOKE_CURL_RESPONSE_FILE")"
EOF
chmod +x "${fake_bin}/curl"

readonly expected_payload='{"zen":"legion smoke round-trip"}'
GITHUB_WEBHOOK_SECRET='legion-smoke-secret'
export GITHUB_WEBHOOK_SECRET
export SMOKE_CURL_HEADERS_FILE="$headers_file"
export SMOKE_CURL_BODY_FILE="$body_file"
export SMOKE_CURL_RESPONSE_FILE="$response_file"
PATH="${fake_bin}:${PATH}"
cat >"${fake_bin}/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${SMOKE_GH_CALL_FILE:-}" ]]; then
  printf '%s\n' "$*" >>"$SMOKE_GH_CALL_FILE"
fi


case "$1" in
  webhook)
    [[ "$#" == 3 && "$2" == "forward" && "$3" == "--help" ]] ||
      { printf 'unexpected gh invocation: %q\n' "$*" >&2; exit 1; }
    exit "${SMOKE_GH_WEBHOOK_HELP_EXIT:-0}"
    ;;
  label)
    printf '%s|%s\n' "${GH_TOKEN:-}" "${GH_CONFIG_DIR:-}" >>"$SMOKE_GH_LABEL_ENV_FILE"
    ;;
  *)
    printf 'unexpected gh invocation: %q\n' "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/gh"
SMOKE_GH_LABEL_ENV_FILE="$gh_label_env_file" ensure_labels "app-installation-token"
[[ "$(<"$gh_label_env_file")" == $'app-installation-token|'"${SMOKE_DIR}"$'/gh-config\napp-installation-token|'"${SMOKE_DIR}"$'/gh-config\napp-installation-token|'"${SMOKE_DIR}"$'/gh-config\napp-installation-token|'"${SMOKE_DIR}"$'/gh-config' ]] || {
  printf 'expected label setup to isolate gh from user auth state\n' >&2
  exit 1
}


[[ "$(SMOKE_GH_WEBHOOK_HELP_EXIT=0 resolve_webhook_mode)" == "forward" ]] || {
  printf 'expected available webhook forwarding to default to forward mode\n' >&2
  exit 1
}
[[ "$(SMOKE_GH_WEBHOOK_HELP_EXIT=1 resolve_webhook_mode)" == "none" ]] || {
  printf 'expected unavailable webhook forwarding to default to none mode\n' >&2
  exit 1
}
: >"$gh_call_file"
[[ "$(SMOKE_WEBHOOK_MODE=none SMOKE_GH_CALL_FILE="$gh_call_file" resolve_webhook_mode)" == "none" ]] || {
  printf 'expected SMOKE_WEBHOOK_MODE=none to select none mode\n' >&2
  exit 1
}
[[ ! -s "$gh_call_file" ]] || {
  printf 'SMOKE_WEBHOOK_MODE=none must not invoke gh\n' >&2
  exit 1
}
[[ "$(SMOKE_WEBHOOK_MODE=forward SMOKE_GH_WEBHOOK_HELP_EXIT=0 resolve_webhook_mode)" == "forward" ]] || {
  printf 'expected SMOKE_WEBHOOK_MODE=forward to select forward mode\n' >&2
  exit 1
}
if (SMOKE_WEBHOOK_MODE=forward SMOKE_GH_WEBHOOK_HELP_EXIT=1 resolve_webhook_mode) >"$assertion_file" 2>&1; then
  printf 'expected unavailable explicit forward mode to fail\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'SMOKE_WEBHOOK_MODE=forward requires gh webhook forward'* ]] || {
  printf 'expected explicit-forward availability error\n' >&2
  exit 1
}
[[ "$(webhook_ingress_block_reason)" == "SMOKE_WEBHOOK_MODE=none: live GitHub events do not flow to Envoy; checkpoints 1-12 are blocked because they depend on live GitHub events; resync-driven intake still works" ]] || {
  printf 'expected exact no-webhook blocked reason\n' >&2
  exit 1
}

printf 'PASS: selects webhook ingress mode without silently falling back\n'

printf '200' >"$response_file"
if ! (assert_webhook_round_trip) >"$assertion_file" 2>&1; then
  cat "$assertion_file" >&2
  exit 1
fi

expected_signature="sha256=$(SMOKE_WEBHOOK_PAYLOAD="$expected_payload" bun -e '
  import { createHmac } from "node:crypto";
  process.stdout.write(createHmac("sha256", process.env.GITHUB_WEBHOOK_SECRET).update(process.env.SMOKE_WEBHOOK_PAYLOAD).digest("hex"));
')"
[[ "$(<"$body_file")" == "$expected_payload" ]] || {
  printf 'expected signed ping payload\n' >&2
  exit 1
}
[[ "$(<"$headers_file")" == *'X-GitHub-Event: ping'* ]] || {
  printf 'expected GitHub ping event header\n' >&2
  exit 1
}
[[ "$(<"$headers_file")" == *'X-GitHub-Delivery: smoke-round-trip-'* ]] || {
  printf 'expected GitHub delivery header\n' >&2
  exit 1
}
[[ "$(<"$headers_file")" == *"X-Hub-Signature-256: ${expected_signature}"* ]] || {
  printf 'expected standard GitHub SHA-256 signature\n' >&2
  exit 1
}
[[ "$(<"$assertion_file")" == *'GREEN webhook round-trip: listener accepted signed ping'* ]] || {
  printf 'expected signed ping success output\n' >&2
  exit 1
}

printf '401' >"$response_file"
if (assert_webhook_round_trip) >"$assertion_file" 2>&1; then
  printf 'expected 401 signed-ping assertion to fail\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'listener rejected signed webhook round-trip (HTTP 401): secret mismatch'* ]] || {
  printf 'expected precise 401 secret-mismatch error\n' >&2
  exit 1
}

printf 'PASS: asserts a signed local GitHub ping without webhook forwarding\n'

printf 'PASS: normalizes stored GitHub webhook secret before child processes start\n'
